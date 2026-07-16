package invariant

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
	})
	require.Equal(t, http.StatusOK, status)

	result := body["result"].(map[string]any)
	assert.Equal(t, mcpProtocolVersion, result["protocolVersion"])
	info := result["serverInfo"].(map[string]any)
	assert.Equal(t, "invariant-protocol", info["name"])
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

	status, body := postMCP(t, ts, map[string]any{
		"jsonrpc": "2.0", "id": 7, "result": map[string]any{},
	})
	assert.Equal(t, http.StatusAccepted, status)
	assert.Nil(t, body)
}

func TestMCPHTTPParseError(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()

	req, err := http.NewRequestWithContext(t.Context(), "POST", ts.URL+"/mcp",
		bytes.NewReader([]byte("{not json")))
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
}

func TestMCPHTTPTransportHeaders(t *testing.T) {
	ts := mcpHTTPServer(t)
	defer ts.Close()
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
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
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(initialize))
		require.NoError(t, err)
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Origin", "https://example.test")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("requires current protocol after initialize", func(t *testing.T) {
		for _, version := range []string{"", "2099-01-01"} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", bytes.NewReader(toolsList))
			require.NoError(t, err)
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
