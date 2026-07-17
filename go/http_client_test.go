package invariant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	annotationspb "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func startAnnotatedHTTPBackend(t *testing.T) (baseURL string, stop func()) {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/greet/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/greet/")
		if name == "" {
			http.NotFound(w, r)
			return
		}
		decodedName, err := url.PathUnescape(name)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if decodedName == "bad" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    "invalid_argument",
				"message": "bad name",
			})
			return
		}
		resp := map[string]any{"message": "Hello, " + decodedName}
		if mood := r.URL.Query().Get("mood"); mood != "" {
			resp["mood"] = mood
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/v1/greet:group", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var in struct {
			People []struct {
				Name string `json:"name"`
			} `json:"people"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		messages := make([]string, 0, len(in.People))
		for _, p := range in.People {
			messages = append(messages, "Hello, "+p.Name)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": messages,
			"count":    len(messages),
		})
	})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(lis) }()

	return "http://" + lis.Addr().String(), func() {
		_ = server.Close()
	}
}

func connectHTTPServer(t *testing.T, target string) *Server {
	t.Helper()
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	require.NoError(t, srv.ConnectHTTP(target))
	return srv
}

func TestConnectHTTPRegistersTools(t *testing.T) {
	baseURL, stop := startAnnotatedHTTPBackend(t)
	defer stop()

	srv := connectHTTPServer(t, baseURL)

	assert.Len(t, srv.tools, 2)
	assert.Contains(t, srv.tools, "GreetService.Greet")
	assert.Contains(t, srv.tools, "GreetService.GreetGroup")
}

func TestConnectHTTPMCPToolCall(t *testing.T) {
	baseURL, stop := startAnnotatedHTTPBackend(t)
	defer stop()

	srv := connectHTTPServer(t, baseURL)

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "Remote"},
		},
	})

	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	require.Len(t, content, 1)
	block := content[0].(map[string]any)
	assert.Contains(t, block["text"], "Hello, Remote")
	assert.Nil(t, result["isError"])
}

func TestConnectHTTPMCPToolCallGreetGroup(t *testing.T) {
	baseURL, stop := startAnnotatedHTTPBackend(t)
	defer stop()

	srv := connectHTTPServer(t, baseURL)

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "GreetService.GreetGroup",
			"arguments": map[string]any{
				"people": []any{
					map[string]any{"name": "Alice"},
					map[string]any{"name": "Bob"},
				},
			},
		},
	})

	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	require.Len(t, content, 1)
	block := content[0].(map[string]any)
	assert.Contains(t, block["text"], "Hello, Alice")
	assert.Contains(t, block["text"], "Hello, Bob")
	assert.Nil(t, result["isError"])
}

func TestConnectHTTPMapsRemoteErrors(t *testing.T) {
	baseURL, stop := startAnnotatedHTTPBackend(t)
	defer stop()

	srv := connectHTTPServer(t, baseURL)

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "bad"},
		},
	})

	result := resp["result"].(map[string]any)
	assert.Equal(t, true, result["isError"])

	errObj := result["error"].(map[string]any)
	assert.Equal(t, "invalid_argument", errObj["code"])
	assert.Equal(t, "bad name", errObj["message"])
}

func TestConnectHTTPProgrammaticAndNative(t *testing.T) {
	baseURL, stop := startAnnotatedHTTPBackend(t)
	defer stop()

	srv := connectHTTPServer(t, baseURL)

	result, err := srv.Invoke(t.Context(), "GreetService.Greet", &greetpb.GreetRequest{Name: "Direct"})
	require.NoError(t, err)
	assert.Equal(t, "Hello, Direct", result.(*greetpb.GreetResponse).GetMessage())

	client := nativeTestStart(t, srv)
	nativeResult, err := client.Greet(t.Context(), &greetpb.GreetRequest{Name: "Native"})
	require.NoError(t, err)
	assert.Equal(t, "Hello, Native", nativeResult.GetMessage())
}

func TestConnectHTTPCli(t *testing.T) {
	baseURL, stop := startAnnotatedHTTPBackend(t)
	defer stop()

	srv := connectHTTPServer(t, baseURL)

	result, err := srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"CLI"}`})
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, CLI")
}

func TestConnectHTTPUnknownService(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	err = srv.ConnectHTTP("http://localhost:1", "does.not.ExistService")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestConnectHTTPBasePath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/greet/World", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Hello, World"})
	})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	baseURL := fmt.Sprintf("http://%s/api", lis.Addr().String())
	srv := connectHTTPServer(t, baseURL)

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "World"},
		},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	block := content[0].(map[string]any)
	assert.Contains(t, block["text"], "Hello, World")
}

func TestConnectHTTPUsesCanonicalJSONNamesAndPreservesTrailingSlash(t *testing.T) {
	descriptorBytes, err := os.ReadFile(descriptorPath())
	require.NoError(t, err)
	var files descriptorpb.FileDescriptorSet
	require.NoError(t, proto.Unmarshal(descriptorBytes, &files))

	pathParams := &descriptorpb.DescriptorProto{
		Name: new("PathParams"),
		Field: []*descriptorpb.FieldDescriptorProto{{
			Name:     new("resource_id"),
			JsonName: new("resourceID"),
			Number:   proto.Int32(1),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}},
	}
	payload := &descriptorpb.DescriptorProto{
		Name: new("Payload"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:     new("display_name"),
				JsonName: new("displayName"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
			{
				Name:     new("external_id"),
				JsonName: new("externalID"),
				Number:   proto.Int32(2),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
		},
	}
	queryOptions := &descriptorpb.DescriptorProto{
		Name: new("QueryOptions"),
		Field: []*descriptorpb.FieldDescriptorProto{{
			Name:     new("page_size"),
			JsonName: new("pageSize"),
			Number:   proto.Int32(1),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
		}},
	}
	request := &descriptorpb.DescriptorProto{
		Name: new("TranscodeRequest"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:     new("path_params"),
				JsonName: new("pathParams"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: new(".httpnames.v1.PathParams"),
			},
			{
				Name:     new("payload_info"),
				JsonName: new("payloadInfo"),
				Number:   proto.Int32(2),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: new(".httpnames.v1.Payload"),
			},
			{
				Name:     new("filter_value"),
				JsonName: new("filterValue"),
				Number:   proto.Int32(3),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
			{
				Name:     new("query_options"),
				JsonName: new("queryOptions"),
				Number:   proto.Int32(4),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: new(".httpnames.v1.QueryOptions"),
			},
		},
	}
	response := &descriptorpb.DescriptorProto{
		Name: new("TranscodeResponse"),
		Field: []*descriptorpb.FieldDescriptorProto{{
			Name:     new("result_text"),
			JsonName: new("resultText"),
			Number:   proto.Int32(1),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}},
	}
	methodOptions := &descriptorpb.MethodOptions{}
	proto.SetExtension(methodOptions, annotationspb.E_Http, &annotationspb.HttpRule{
		Pattern:      &annotationspb.HttpRule_Post{Post: "/v1/resources/{path_params.resource_id}/"},
		Body:         "payload_info",
		ResponseBody: "result_text",
	})
	files.File = append(files.File, &descriptorpb.FileDescriptorProto{
		Name:       new("http_json_names.proto"),
		Package:    new("httpnames.v1"),
		Syntax:     new("proto3"),
		Dependency: []string{"google/api/annotations.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			pathParams,
			payload,
			queryOptions,
			request,
			response,
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: new("JSONNameService"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       new("Transcode"),
				InputType:  new(".httpnames.v1.TranscodeRequest"),
				OutputType: new(".httpnames.v1.TranscodeResponse"),
				Options:    methodOptions,
			}},
		}},
	})
	descriptorBytes, err = proto.Marshal(&files)
	require.NoError(t, err)

	type observedRequest struct {
		path  string
		query url.Values
		body  map[string]any
	}
	observed := make(chan observedRequest, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		assert.NoError(t, readErr)
		var decoded map[string]any
		assert.NoError(t, json.Unmarshal(body, &decoded))
		observed <- observedRequest{path: r.URL.Path, query: r.URL.Query(), body: decoded}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer backend.Close()

	srv, err := ServerFromBytes(descriptorBytes)
	require.NoError(t, err)
	require.NoError(t, srv.ConnectHTTP(backend.URL+"/api/"))
	result, err := srv.cli(t.Context(), []string{
		"JSONNameService",
		"Transcode",
		"-r",
		`{
			"pathParams":{"resourceID":"item-1"},
			"payloadInfo":{"displayName":"Visible","externalID":"external-1"},
			"filterValue":"active",
			"queryOptions":{"pageSize":25}
		}`,
	})
	require.NoError(t, err)

	got := <-observed
	assert.Equal(t, "/api/v1/resources/item-1/", got.path)
	assert.Equal(t, url.Values{
		"filterValue":           {"active"},
		"queryOptions.pageSize": {"25"},
	}, got.query)
	assert.Equal(t, map[string]any{
		"displayName": "Visible",
		"externalID":  "external-1",
	}, got.body)

	var responseJSON map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &responseJSON))
	assert.Equal(t, map[string]any{"result_text": "ok"}, responseJSON)
}

func TestPathTemplatePreservesRootAndTrailingSlash(t *testing.T) {
	for _, test := range []struct {
		name     string
		pattern  string
		expected string
	}{
		{name: "root", pattern: "/", expected: "/"},
		{name: "without trailing slash", pattern: "/questions", expected: "/questions"},
		{name: "with trailing slash", pattern: "/questions/", expected: "/questions/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			template, err := parsePathTemplate(test.pattern)
			require.NoError(t, err)
			path, _, err := (&httpClientBinding{template: template}).expandPath(nil)
			require.NoError(t, err)
			assert.Equal(t, test.expected, path)
		})
	}
}

func TestDecodeHTTPResponseWithResponseBody(t *testing.T) {
	resp := &greetpb.GreetResponse{}
	err := decodeHTTPResponse([]byte(`"Hello, World"`), resp, "message")
	require.NoError(t, err)
	assert.Equal(t, "Hello, World", resp.GetMessage())
}

func TestConnectHTTPInjectsHeadersFromEnv(t *testing.T) {
	t.Setenv("INVARIANT_HTTP_HEADER_AUTHORIZATION", "Bearer test-token")

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/greet/World", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    "unauthenticated",
				"message": "missing auth",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Hello, World"})
	})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	srv := connectHTTPServer(t, "http://"+lis.Addr().String())

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "World"},
		},
	})
	result := resp["result"].(map[string]any)
	assert.Nil(t, result["isError"])
}

func TestConnectHTTPSetsDefaultUserAgent(t *testing.T) {
	t.Setenv("INVARIANT_HTTP_HEADER_USER_AGENT", "")
	var seenUserAgent string

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/greet/World", func(w http.ResponseWriter, r *http.Request) {
		seenUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Hello, World"})
	})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	srv := connectHTTPServer(t, "http://"+lis.Addr().String())

	result, err := srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"World"}`})
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, World")
	require.NotEmpty(t, seenUserAgent)
	assert.True(t, strings.HasPrefix(seenUserAgent, "invariant-protocol/"))
}

func TestConnectHTTPUserAgentOverrideFromEnv(t *testing.T) {
	t.Setenv("INVARIANT_HTTP_HEADER_USER_AGENT", "custom-agent/9.9")
	var seenUserAgent string

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/greet/World", func(w http.ResponseWriter, r *http.Request) {
		seenUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Hello, World"})
	})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	srv := connectHTTPServer(t, "http://"+lis.Addr().String())

	result, err := srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"World"}`})
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, World")
	assert.Equal(t, "custom-agent/9.9", seenUserAgent)
}

func TestConnectHTTPRetriesTransientGET(t *testing.T) {
	var attempts atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/greet/World", func(w http.ResponseWriter, _ *http.Request) {
		current := attempts.Add(1)
		if current <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    "unavailable",
				"message": "temporary outage",
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Hello, World"})
	})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	srv := connectHTTPServer(t, "http://"+lis.Addr().String())

	result, err := srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"World"}`})
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, World")
	assert.Equal(t, int32(3), attempts.Load())
}

func TestConnectHTTPDoesNotRetryPOST(t *testing.T) {
	var attempts atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/greet:group", func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    "unavailable",
			"message": "temporary outage",
		})
	})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	srv := connectHTTPServer(t, "http://"+lis.Addr().String())

	_, err = srv.cli(t.Context(), []string{"GreetService", "GreetGroup", "-r", `{"people":[{"name":"Alice"}]}`})
	require.Error(t, err)
	assert.Equal(t, int32(1), attempts.Load())
}

func TestConnectHTTPUsesDynamicHeaderProvider(t *testing.T) {
	var gotMethodPath string
	var gotMethod string
	var gotBody string

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/greet/World", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Signature") != "sig-value" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    "unauthenticated",
				"message": "missing signature",
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Hello, World"})
	})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	srv.UseHTTPHeaderProvider(func(_ context.Context, req *OutboundHTTPRequest) (map[string]string, error) {
		gotMethodPath = req.MethodPath
		gotMethod = req.Method
		gotBody = string(req.Body)
		return map[string]string{"X-Signature": "sig-value"}, nil
	})
	require.NoError(t, srv.ConnectHTTP("http://"+lis.Addr().String()))

	result, err := srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"World"}`})
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, World")
	assert.Equal(t, "/greet.v1.GreetService/Greet", gotMethodPath)
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Empty(t, gotBody)
}

func TestConnectHTTPDynamicHeaderProviderError(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	require.NoError(t, srv.ConnectHTTP("http://localhost:1"))
	srv.UseHTTPHeaderProvider(func(_ context.Context, _ *OutboundHTTPRequest) (map[string]string, error) {
		return nil, errors.New("missing signing key")
	})

	_, err = srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"World"}`})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.Contains(t, err.Error(), "missing signing key")
}
