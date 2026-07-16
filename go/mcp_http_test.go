package invariant

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postMCP issues one MCP JSON-RPC request via the HTTP transport and returns
// the response status and parsed body (nil body on 202).
func postMCP(t *testing.T, ts *httptest.Server, msg map[string]any) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(msg)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), "POST", ts.URL+"/mcp", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if msg["method"] != "initialize" {
		req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusAccepted {
		return resp.StatusCode, nil
	}

	var parsed map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&parsed))
	return resp.StatusCode, parsed
}

func mcpHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := streamServer(t, &streamServicer{})
	handler := srv.HTTPHandler()
	return httptest.NewServer(handler)
}

func TestMCPHTTPInitialize(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	status, body := postMCP(t, ts, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": mcpInitializeParamsForTest(mcpProtocolVersion),
	})
	require.Equal(t, http.StatusOK, status)

	result := body["result"].(map[string]any)
	assert.Equal(t, mcpProtocolVersion, result["protocolVersion"])
	info := result["serverInfo"].(map[string]any)
	assert.Equal(t, "invariant-protocol", info["name"])

	status, body = postMCP(t, ts, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "initialize",
		"params": mcpInitializeParamsForTest("2099-01-01"),
	})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, mcpProtocolVersion, body["result"].(map[string]any)["protocolVersion"])
}

func TestMCPHTTPToolsList(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	status, body := postMCP(t, ts, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	require.Equal(t, http.StatusOK, status)

	tools := body["result"].(map[string]any)["tools"].([]any)
	require.Len(t, tools, 3) // Greet, GreetGroup, StreamGreet

	var names []string
	for _, raw := range tools {
		names = append(names, raw.(map[string]any)["name"].(string))
	}
	assert.ElementsMatch(t,
		[]string{"GreetService.Greet", "GreetService.GreetGroup", "GreetService.StreamGreet"},
		names,
	)
}

func TestMCPHTTPToolCallUnary(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	status, body := postMCP(t, ts, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "Alice"},
		},
	})
	require.Equal(t, http.StatusOK, status)

	result := body["result"].(map[string]any)
	content := result["content"].([]any)
	require.Len(t, content, 1)
	block := content[0].(map[string]any)
	assert.Contains(t, block["text"], "Hello Alice")
}

type mcpDeadlineServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

func (*mcpDeadlineServicer) Greet(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	switch req.GetName() {
	case "wait":
		<-ctx.Done()
	case "cpu":
		time.Sleep(25 * time.Millisecond)
	}
	return &greetpb.GreetResponse{Message: "late"}, nil
}

func TestMCPHTTPConnectTimeoutReturnsConnectDeadlineError(t *testing.T) {
	server := streamServer(t, &mcpDeadlineServicer{})
	ts := httptest.NewServer(server.HTTPHandler())
	defer ts.Close()

	for _, test := range []struct {
		name      string
		timeoutMS string
	}{
		{name: "wait", timeoutMS: "5"},
		{name: "cpu", timeoutMS: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"GreetService.Greet","arguments":{"name":"` + test.name + `"}}}`)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
			req.Header.Set("Connect-Timeout-Ms", test.timeoutMS)

			response, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			assert.Equal(t, http.StatusGatewayTimeout, response.StatusCode)
			var payload map[string]any
			require.NoError(t, json.NewDecoder(response.Body).Decode(&payload))
			assert.Equal(t, "deadline_exceeded", payload["code"])
		})
	}
}

func TestMCPHTTPToolCallStream(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	status, body := postMCP(t, ts, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.StreamGreet",
			"arguments": map[string]any{"name": "Stream", "count": 3},
		},
	})
	require.Equal(t, http.StatusOK, status)

	result := body["result"].(map[string]any)
	assert.Nil(t, result["isError"])
	content := result["content"].([]any)
	require.Len(t, content, 3, "each chunk becomes one text block")
}

func TestMCPHTTPNotificationReturns202(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	// JSON-RPC notification: no id field.
	status, body := postMCP(t, ts, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized",
	})
	assert.Equal(t, http.StatusAccepted, status)
	assert.Nil(t, body)
}

func TestMCPHTTPAcceptsClientResponse(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	for _, message := range []map[string]any{
		{"jsonrpc": "2.0", "id": 7, "result": map[string]any{}},
		{"jsonrpc": "2.0", "error": map[string]any{"code": -32000, "message": "client failure"}},
	} {
		status, body := postMCP(t, ts, message)
		assert.Equal(t, http.StatusAccepted, status)
		assert.Nil(t, body)
	}
}

func TestMCPHTTPParseError(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	for name, body := range map[string][]byte{
		"malformed JSON": []byte("{not json"),
		"invalid UTF-8":  {'"', 0xff, '"'},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), "POST", ts.URL+"/mcp", bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			var parsed map[string]any
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&parsed))
			errObj := parsed["error"].(map[string]any)
			assert.InEpsilon(t, float64(-32700), errObj["code"], 0)
			assert.Contains(t, errObj["message"], "Parse error")
		})
	}
}

func TestMCPHTTPInvalidRequest(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	for name, body := range map[string]string{
		"non-object":       `[]`,
		"missing method":   `{"jsonrpc":"2.0","id":1}`,
		"missing version":  `{"id":1,"method":"ping"}`,
		"wrong version":    `{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		"invalid method":   `{"jsonrpc":"2.0","id":1,"method":1}`,
		"invalid id":       `{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		"unsafe integer":   `{"jsonrpc":"2.0","id":9007199254740992,"method":"ping"}`,
		"null response id": `{"jsonrpc":"2.0","id":null,"result":{}}`,
		"array result":     `{"jsonrpc":"2.0","id":1,"result":[]}`,
		"malformed error":  `{"jsonrpc":"2.0","error":{"code":1.5,"message":"bad"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(
				t.Context(), http.MethodPost, ts.URL+"/mcp", bytes.NewBufferString(body),
			)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			var parsed map[string]any
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&parsed))
			assert.Nil(t, parsed["id"])
			errorObject := parsed["error"].(map[string]any)
			assert.InEpsilon(t, float64(-32600), errorObject["code"], 0)
			assert.Equal(t, "Invalid Request", errorObject["message"])
		})
	}
}

func TestMCPHTTPInvalidParams(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	for name, message := range map[string]map[string]any{
		"request params array": {
			"jsonrpc": "2.0", "id": 1, "method": "ping", "params": []any{},
		},
		"tool call missing name": {
			"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{},
		},
		"tool call arguments array": {
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "GreetService.Greet", "arguments": []any{}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			statusCode, response := postMCP(t, ts, message)
			assert.Equal(t, http.StatusOK, statusCode)
			assert.InEpsilon(t, float64(1), response["id"], 0)
			errorObject := response["error"].(map[string]any)
			assert.InEpsilon(t, float64(-32602), errorObject["code"], 0)
		})
	}

	statusCode, response := postMCP(t, ts, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/cancelled", "params": []any{}, //nolint:misspell // Method name defined by MCP.
	})
	assert.Equal(t, http.StatusAccepted, statusCode)
	assert.Nil(t, response)
}

func TestMCPHTTPRequiresApplicationJSON(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"invariant-test","version":"1.0"}}}`)

	for _, contentType := range []string{"", "text/plain", "application/connect+json"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Accept", "application/json, text/event-stream")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode, "Content-Type: %q", contentType)
	}
}

func TestMCPHTTPTransportHeaders(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"invariant-test","version":"1.0"}}}`)
	toolsList := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	t.Run("requires both accepted response types", func(t *testing.T) {
		for _, accept := range []string{"", "application/json", "text/event-stream", "application/json, text/event-stream;q=0"} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(initialize))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", accept)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, http.StatusNotAcceptable, resp.StatusCode, "Accept: %q", accept)
		}
	})

	t.Run("rejects every Origin", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+"/mcp", bytes.NewReader(initialize))
			require.NoError(t, err)
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Origin", "https://example.test")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, http.StatusForbidden, resp.StatusCode, "method: %s", method)
		}
	})

	t.Run("requires current protocol after initialize", func(t *testing.T) {
		for _, version := range []string{"", "2099-01-01"} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(toolsList))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", version)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "version: %q", version)
		}
	})

	t.Run("rejects unsupported initialize version", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(initialize))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2099-01-01")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("GET is unavailable without SSE", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/mcp") //nolint:noctx // Test-only one-shot request.
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})
}

func TestMCPHTTPUnknownMethod(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	status, body := postMCP(t, ts, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "does/not/exist",
	})
	require.Equal(t, http.StatusOK, status)
	errObj := body["error"].(map[string]any)
	assert.InEpsilon(t, float64(-32601), errObj["code"], 0)
}

func TestMCPHTTPResponseLimit(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	srv.SetMaxUnaryResponseBytes(160)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	statusCode, body := postMCP(t, ts, map[string]any{
		"jsonrpc": "2.0", "id": 8, "method": "tools/list",
	})
	assert.Equal(t, http.StatusTooManyRequests, statusCode)
	assert.Equal(t, "resource_exhausted", body["code"])
}

func TestMCPHTTPDoesNotShadowToolEndpoints(t *testing.T) {
	// /mcp is its own route — tool POSTs still work alongside it.
	ts := mcpHTTPServer(t)
	defer ts.Close()

	req, _ := http.NewRequestWithContext(t.Context(), "POST",
		ts.URL+"/greet.v1.GreetService/Greet", bytes.NewReader([]byte(`{"name":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
