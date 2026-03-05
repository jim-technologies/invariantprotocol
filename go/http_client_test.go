package invariant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
				"error": map[string]any{
					"code":    "INVALID_ARGUMENT",
					"message": "bad name",
				},
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
	defer srv.Stop()

	assert.Len(t, srv.tools, 2)
	assert.Contains(t, srv.tools, "GreetService.Greet")
	assert.Contains(t, srv.tools, "GreetService.GreetGroup")
}

func TestConnectHTTPMCPToolCall(t *testing.T) {
	baseURL, stop := startAnnotatedHTTPBackend(t)
	defer stop()

	srv := connectHTTPServer(t, baseURL)
	defer srv.Stop()

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
	defer srv.Stop()

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
	defer srv.Stop()

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
	assert.Equal(t, "INVALID_ARGUMENT", errObj["code"])
	assert.Equal(t, "bad name", errObj["message"])
}

func TestConnectHTTPHandlerDirect(t *testing.T) {
	baseURL, stop := startAnnotatedHTTPBackend(t)
	defer stop()

	srv := connectHTTPServer(t, baseURL)
	defer srv.Stop()

	tool := srv.tools["GreetService.Greet"]
	dh, ok := tool.Handler.(*httpDynamicHandler)
	require.True(t, ok, "handler should be *httpDynamicHandler")

	result, err := dh.CallJSON(t.Context(), []byte(`{"name":"Direct"}`))
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, Direct")
}

func TestConnectHTTPCli(t *testing.T) {
	baseURL, stop := startAnnotatedHTTPBackend(t)
	defer stop()

	srv := connectHTTPServer(t, baseURL)
	defer srv.Stop()

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
	defer srv.Stop()

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

func TestConnectHTTPInjectsHeadersFromEnv(t *testing.T) {
	t.Setenv("INVARIANT_HTTP_HEADER_AUTHORIZATION", "Bearer test-token")

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/greet/World", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "UNAUTHENTICATED",
					"message": "missing auth",
				},
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
	defer srv.Stop()

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
				"error": map[string]any{
					"code":    "UNAVAILABLE",
					"message": "temporary outage",
				},
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
	defer srv.Stop()

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
			"error": map[string]any{
				"code":    "UNAVAILABLE",
				"message": "temporary outage",
			},
		})
	})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	srv := connectHTTPServer(t, "http://"+lis.Addr().String())
	defer srv.Stop()

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
				"error": map[string]any{
					"code":    "UNAUTHENTICATED",
					"message": "missing signature",
				},
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
	defer srv.Stop()

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
	srv.UseHTTPHeaderProvider(func(_ context.Context, _ *OutboundHTTPRequest) (map[string]string, error) {
		return nil, errors.New("missing signing key")
	})
	require.NoError(t, srv.ConnectHTTP("http://localhost:1"))
	defer srv.Stop()

	_, err = srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"World"}`})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.Contains(t, err.Error(), "missing signing key")
}
