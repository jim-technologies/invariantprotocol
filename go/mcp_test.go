package invariant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mcpTestServicer implements GreetService RPCs using generated proto types.
type mcpTestServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

type mcpCancellationServicer struct {
	greetpb.UnimplementedGreetServiceServer
	started  chan struct{}
	canceled chan struct{}
}

type mcpHandlerCanceledServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

func (s *mcpCancellationServicer) Greet(ctx context.Context, _ *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	close(s.started)
	<-ctx.Done()
	close(s.canceled)
	return nil, status.FromContextError(ctx.Err()).Err()
}

func (*mcpHandlerCanceledServicer) Greet(context.Context, *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return nil, status.Error(codes.Canceled, "application canceled the operation")
}

func (s *mcpTestServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	resp := &greetpb.GreetResponse{
		Message: "Hello, " + req.Name,
		Tags:    req.Tags,
	}
	if req.Mood != nil {
		resp.Mood = *req.Mood
	}
	return resp, nil
}

func (s *mcpTestServicer) GreetGroup(_ context.Context, req *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	var messages []string
	for _, p := range req.People {
		messages = append(messages, "Hello, "+p.Name)
	}
	return &greetpb.GreetGroupResponse{
		Messages: messages,
		Count:    int32(len(req.People)),
	}, nil
}

func mcpServer(t *testing.T) *Server {
	t.Helper()
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(srv, &mcpTestServicer{})
	return srv
}

func mcpInitializeParamsForTest(protocolVersion string) map[string]any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "invariant-test", "version": "1.0"},
	}
}

// sendMCP writes a single JSON-RPC request and returns the parsed response.
func sendMCP(t *testing.T, srv *Server, req map[string]any) map[string]any {
	t.Helper()
	reqJSON, err := json.Marshal(req)
	require.NoError(t, err)

	r := bytes.NewBuffer(append(reqJSON, '\n'))
	var w bytes.Buffer

	session := srv.newMCPSession(r, &w)
	err = session.run(t.Context())
	require.NoError(t, err)

	var resp map[string]any
	err = json.Unmarshal(bytes.TrimSpace(w.Bytes()), &resp)
	require.NoError(t, err)
	return resp
}

// sendMultiMCP writes multiple JSON-RPC requests and returns all parsed responses.
func sendMultiMCP(t *testing.T, srv *Server, reqs ...map[string]any) []map[string]any {
	t.Helper()
	var input bytes.Buffer
	for _, req := range reqs {
		reqJSON, err := json.Marshal(req)
		require.NoError(t, err)
		input.Write(reqJSON)
		input.WriteByte('\n')
	}

	var output bytes.Buffer
	session := srv.newMCPSession(&input, &output)
	err := session.run(t.Context())
	require.NoError(t, err)

	var resps []map[string]any
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp map[string]any
		err := json.Unmarshal(line, &resp)
		require.NoError(t, err)
		resps = append(resps, resp)
	}
	return resps
}

func TestMCPSessionCancellationInterruptsIdleRead(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	session := mcpServer(t).newMCPSession(reader, io.Discard)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- session.run(ctx) }()

	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("MCP session did not stop while stdin was idle")
	}
}

func TestMCPProtocolCancellationNotificationSuppressesResponse(t *testing.T) {
	tests := []struct {
		name     string
		callID   string
		cancelID string
	}{
		{name: "integer", callID: "7", cancelID: "7"},
		{name: "maximum portable integer", callID: "9007199254740991", cancelID: "9007199254740991"},
		{name: "minimum portable integer", callID: "-9007199254740991", cancelID: "-9007199254740991"},
		{name: "equivalent integer", callID: "-0", cancelID: "0"},
		{name: "escaped string", callID: `"call-\u0031"`, cancelID: `"call-1"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &mcpCancellationServicer{started: make(chan struct{}), canceled: make(chan struct{})}
			srv, err := ServerFromDescriptor(descriptorPath())
			require.NoError(t, err)
			greetpb.RegisterGreetServiceServer(srv, service)

			call := fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"greet.v1.GreetService.Greet","arguments":{"name":"waiting"}}}`,
				test.callID,
			)
			cancel := fmt.Sprintf(
				`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%s}}`, //nolint:misspell // Method name defined by MCP.
				test.cancelID,
			)
			var output bytes.Buffer
			require.NoError(t, srv.newMCPSession(strings.NewReader(call+"\n"+cancel+"\n"), &output).run(t.Context()))

			requireClosed(t, service.started, "MCP handler did not start")
			requireClosed(t, service.canceled, "MCP cancellation did not reach the handler")
			assert.Empty(t, output.String(), "MCP cancellation must suppress the canceled request's response")
		})
	}
}

func TestMCPIDKeyUsesPortableIntegerRange(t *testing.T) {
	for _, test := range []struct {
		raw     string
		wantKey string
		wantOK  bool
	}{
		{raw: "9007199254740991", wantKey: "integer:9007199254740991", wantOK: true},
		{raw: "-9007199254740991", wantKey: "integer:-9007199254740991", wantOK: true},
		{raw: "-0", wantKey: "integer:0", wantOK: true},
		{raw: "0", wantKey: "integer:0", wantOK: true},
		{raw: "9007199254740992", wantOK: false},
		{raw: "-9007199254740992", wantOK: false},
		{raw: "1.0", wantOK: false},
	} {
		key, ok := mcpIDKey(json.RawMessage(test.raw))
		assert.Equal(t, test.wantOK, ok, test.raw)
		assert.Equal(t, test.wantKey, key, test.raw)
	}
}

func TestMCPHandlerCanceledStatusStillReturnsResponse(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(srv, &mcpHandlerCanceledServicer{})

	response := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "greet.v1.GreetService.Greet",
			"arguments": map[string]any{"name": "canceled"},
		},
	})
	result := response["result"].(map[string]any)
	assert.Equal(t, true, result["isError"])
	assert.Equal(t, "canceled", result["error"].(map[string]any)["code"])
}

func requireClosed(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func TestMCPInitialize(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": mcpInitializeParamsForTest(mcpProtocolVersion),
	})
	assert.Equal(t, "2.0", resp["jsonrpc"])
	assert.InEpsilon(t, float64(1), resp["id"], 0)

	result := resp["result"].(map[string]any)
	assert.Equal(t, mcpProtocolVersion, result["protocolVersion"])

	caps := result["capabilities"].(map[string]any)
	assert.Contains(t, caps, "tools")

	info := result["serverInfo"].(map[string]any)
	assert.Equal(t, "invariant-protocol", info["name"])
}

func TestMCPInitializeValidatesParamsAndNegotiatesVersion(t *testing.T) {
	srv := mcpServer(t)
	invalidParams := []any{
		nil,
		map[string]any{},
		map[string]any{"protocolVersion": 1, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}},
		map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": []any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}},
		map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{}, "clientInfo": []any{}},
		map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": 1, "version": "1"}},
		map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": 1}},
	}
	for index, params := range invalidParams {
		request := map[string]any{
			"jsonrpc": "2.0",
			"id":      index + 10,
			"method":  "initialize",
		}
		if params != nil {
			request["params"] = params
		}
		response := sendMCP(t, srv, request)
		assert.InEpsilon(t, float64(index+10), response["id"], 0)
		assert.InEpsilon(t, float64(-32602), response["error"].(map[string]any)["code"], 0)
	}

	response := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0",
		"id":      20,
		"method":  "initialize",
		"params":  mcpInitializeParamsForTest("2099-01-01"),
	})
	assert.Equal(t, mcpProtocolVersion, response["result"].(map[string]any)["protocolVersion"])
}

func TestMCPToolsList(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	assert.Len(t, tools, 3)

	var names []string
	for _, raw := range tools {
		tool := raw.(map[string]any)
		names = append(names, tool["name"].(string))
		assert.NotEmpty(t, tool["description"])
		assert.NotNil(t, tool["inputSchema"])
	}
	assert.Equal(t, []string{"greet.v1.GreetService.Greet", "greet.v1.GreetService.GreetGroup", "greet.v1.GreetService.StreamGreet"}, names)
}

func TestMCPToolCall(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "greet.v1.GreetService.Greet",
			"arguments": map[string]any{"name": "Alice"},
		},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	require.Len(t, content, 1)

	block := content[0].(map[string]any)
	assert.Equal(t, "text", block["type"])
	assert.Contains(t, block["text"], "Hello, Alice")
	assert.Nil(t, result["isError"])
}

func TestMCPToolCallRejectsUnknownField(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "greet.v1.GreetService.Greet",
			"arguments": map[string]any{"name": "Alice", "extra": "x"},
		},
	})
	result := resp["result"].(map[string]any)
	assert.Equal(t, true, result["isError"])

	content := result["content"].([]any)
	require.Len(t, content, 1)
	block := content[0].(map[string]any)
	assert.Contains(t, block["text"], "unknown field")

	errObj := result["error"].(map[string]any)
	assert.Equal(t, "invalid_argument", errObj["code"])
	assert.Contains(t, errObj["message"], "unknown field")

	details := errObj["details"].([]any)
	require.NotEmpty(t, details)
	first := details[0].(map[string]any)
	violations := first["fieldViolations"].([]any)
	require.NotEmpty(t, violations)
	v := violations[0].(map[string]any)
	assert.Equal(t, "extra", v["field"])
}

func TestMCPToolCallWithEnumAndTags(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name": "greet.v1.GreetService.Greet",
			"arguments": map[string]any{
				"name": "Alice",
				"mood": "MOOD_HAPPY",
				"tags": map[string]any{"lang": "en"},
			},
		},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	block := content[0].(map[string]any)

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(block["text"].(string)), &data))
	assert.Contains(t, data["message"], "Alice")
	assert.Equal(t, "MOOD_HAPPY", data["mood"])
	tags := data["tags"].(map[string]any)
	assert.Equal(t, "en", tags["lang"])
}

func TestMCPToolCallGreetGroup(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name": "greet.v1.GreetService.GreetGroup",
			"arguments": map[string]any{
				"people": []any{
					map[string]any{"name": "Alice", "mood": "MOOD_HAPPY"},
					map[string]any{"name": "Bob"},
				},
			},
		},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	block := content[0].(map[string]any)

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(block["text"].(string)), &data))
	messages := data["messages"].([]any)
	assert.Equal(t, "Hello, Alice", messages[0])
	assert.Equal(t, "Hello, Bob", messages[1])
	assert.InEpsilon(t, float64(2), data["count"], 0)
}

func TestMCPToolCallUnknown(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{
			"name":      "no_such_tool",
			"arguments": map[string]any{},
		},
	})
	errObj := resp["error"].(map[string]any)
	assert.InEpsilon(t, float64(-32602), errObj["code"], 0)
	assert.Contains(t, errObj["message"], "Unknown tool")
}

func TestMCPPing(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "ping",
	})
	result := resp["result"].(map[string]any)
	assert.Empty(t, result)
}

func TestMCPUnknownMethod(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "nonexistent/method",
	})
	errObj := resp["error"].(map[string]any)
	assert.InEpsilon(t, float64(-32601), errObj["code"], 0)
	assert.Contains(t, errObj["message"], "Method not found")
}

func TestMCPNotificationNoResponse(t *testing.T) {
	// Notification = no "id" field → should produce no response.
	srv := mcpServer(t)

	reqJSON, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized",
	})
	require.NoError(t, err)

	r := bytes.NewBuffer(append(reqJSON, '\n'))
	var w bytes.Buffer

	session := srv.newMCPSession(r, &w)
	err = session.run(t.Context())
	require.NoError(t, err)
	assert.Empty(t, w.String())
}

func TestMCPInvalidRequest(t *testing.T) {
	for name, input := range map[string]string{
		"non-object":                 `[]`,
		"missing method":             `{"jsonrpc":"2.0","id":1}`,
		"missing version":            `{"id":1,"method":"ping"}`,
		"wrong version":              `{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		"invalid method":             `{"jsonrpc":"2.0","id":1,"method":1}`,
		"null id":                    `{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		"fractional id":              `{"jsonrpc":"2.0","id":1.0,"method":"ping"}`,
		"above portable integer max": `{"jsonrpc":"2.0","id":9007199254740992,"method":"ping"}`,
		"below portable integer min": `{"jsonrpc":"2.0","id":-9007199254740992,"method":"ping"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			session := mcpServer(t).newMCPSession(strings.NewReader(input+"\n"), &output)
			require.NoError(t, session.run(t.Context()))

			var response map[string]any
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response))
			assert.Nil(t, response["id"])
			errorObject := response["error"].(map[string]any)
			assert.InEpsilon(t, float64(-32600), errorObject["code"], 0)
			assert.Equal(t, "Invalid Request", errorObject["message"])
		})
	}
}

func TestMCPPortableNumericIDsAreAcceptedAndNegativeZeroIsCanonical(t *testing.T) {
	for _, test := range []struct {
		id   string
		want string
	}{
		{id: "9007199254740991", want: "9007199254740991"},
		{id: "-9007199254740991", want: "-9007199254740991"},
		{id: "-0", want: "0"},
	} {
		t.Run(test.id, func(t *testing.T) {
			input := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"ping"}`, test.id)
			var output bytes.Buffer
			require.NoError(
				t,
				mcpServer(t).newMCPSession(strings.NewReader(input+"\n"), &output).run(t.Context()),
			)
			var response struct {
				ID json.RawMessage `json:"id"`
			}
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response))
			assert.Equal(t, test.want, string(response.ID))
		})
	}
}

func TestMCPInvalidUTF8IsParseError(t *testing.T) {
	var output bytes.Buffer
	input := append([]byte{'"', 0xff, '"'}, '\n')
	require.NoError(t, mcpServer(t).newMCPSession(bytes.NewReader(input), &output).run(t.Context()))

	var response map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response))
	errorObject := response["error"].(map[string]any)
	assert.InEpsilon(t, float64(-32700), errorObject["code"], 0)
	assert.Nil(t, response["id"])
}

func TestMCPClientResponseValidation(t *testing.T) {
	srv := mcpServer(t)
	responses := sendMultiMCP(t, srv,
		map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{}},
		map[string]any{
			"jsonrpc": "2.0",
			"error":   map[string]any{"code": -32000, "message": "client failure"},
		},
		map[string]any{"jsonrpc": "2.0", "id": nil, "result": map[string]any{}},
		map[string]any{"jsonrpc": "2.0", "id": 2, "result": []any{}},
		map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"error":   map[string]any{"code": 1.5, "message": "not an integer"},
		},
		map[string]any{"jsonrpc": "2.0", "id": 4, "method": "ping"},
	)

	require.Len(t, responses, 4, "two valid client responses must be ignored")
	for _, response := range responses[:3] {
		errorObject := response["error"].(map[string]any)
		assert.InEpsilon(t, float64(-32600), errorObject["code"], 0)
		assert.Nil(t, response["id"])
	}
	assert.InEpsilon(t, float64(4), responses[3]["id"], 0)
	assert.Empty(t, responses[3]["result"])
}

func TestMCPInvalidParams(t *testing.T) {
	for name, request := range map[string]map[string]any{
		"ping params array": {
			"jsonrpc": "2.0", "id": 1, "method": "ping", "params": []any{},
		},
		"tools list params scalar": {
			"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": "bad",
		},
		"tool call params array": {
			"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": []any{},
		},
		"tool call missing name": {
			"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{},
		},
		"tool call non-string name": {
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": 7},
		},
		"tool call arguments array": {
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "greet.v1.GreetService.Greet", "arguments": []any{}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := sendMCP(t, mcpServer(t), request)
			assert.InEpsilon(t, float64(1), response["id"], 0)
			errorObject := response["error"].(map[string]any)
			assert.InEpsilon(t, float64(-32602), errorObject["code"], 0)
		})
	}
}

func TestMCPInvalidNotificationParamsAreIgnored(t *testing.T) {
	responses := sendMultiMCP(t, mcpServer(t),
		map[string]any{
			"jsonrpc": "2.0", "method": "notifications/cancelled", "params": []any{}, //nolint:misspell // Method name defined by MCP.
		},
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"},
	)
	require.Len(t, responses, 1)
	assert.InEpsilon(t, float64(1), responses[0]["id"], 0)
}

func TestMCPToolCallArgumentsAreOptional(t *testing.T) {
	response := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": "greet.v1.GreetService.Greet"},
	})
	result := response["result"].(map[string]any)
	assert.Nil(t, result["isError"])
}

func TestMCPMultipleRequests(t *testing.T) {
	resps := sendMultiMCP(t, mcpServer(t),
		map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": mcpInitializeParamsForTest(mcpProtocolVersion),
		},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, // notification
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
		map[string]any{
			"jsonrpc": "2.0", "id": 3, "method": "tools/call",
			"params": map[string]any{
				"name":      "greet.v1.GreetService.Greet",
				"arguments": map[string]any{"name": "Bob"},
			},
		},
	)
	// 3 responses (notification produces none)
	require.Len(t, resps, 3)
	assert.InEpsilon(t, float64(1), resps[0]["id"], 0)
	assert.InEpsilon(t, float64(2), resps[1]["id"], 0)
	assert.InEpsilon(t, float64(3), resps[2]["id"], 0)
}
