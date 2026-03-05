package invariant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mcpTestServicer implements GreetService RPCs using generated proto types.
type mcpTestServicer struct{}

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
	srv := newServer(mustParse(t))
	require.NoError(t, srv.Register(&mcpTestServicer{}))
	return srv
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

func TestMCPInitialize(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	})
	assert.Equal(t, "2.0", resp["jsonrpc"])
	assert.InEpsilon(t, float64(1), resp["id"], 0)

	result := resp["result"].(map[string]any)
	assert.Equal(t, "2024-11-05", result["protocolVersion"])

	caps := result["capabilities"].(map[string]any)
	assert.Contains(t, caps, "tools")

	info := result["serverInfo"].(map[string]any)
	assert.Equal(t, "invariant-protocol", info["name"])
}

func TestMCPToolsList(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	assert.Len(t, tools, 2)

	var names []string
	for _, raw := range tools {
		tool := raw.(map[string]any)
		names = append(names, tool["name"].(string))
		assert.NotEmpty(t, tool["description"])
		assert.NotNil(t, tool["inputSchema"])
	}
	assert.Equal(t, []string{"GreetService.Greet", "GreetService.GreetGroup"}, names)
}

func TestMCPToolCall(t *testing.T) {
	resp := sendMCP(t, mcpServer(t), map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
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
			"name":      "GreetService.Greet",
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
	assert.Equal(t, "INVALID_ARGUMENT", errObj["code"])
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
			"name": "GreetService.Greet",
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
			"name": "GreetService.GreetGroup",
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

func TestMCPMultipleRequests(t *testing.T) {
	resps := sendMultiMCP(t, mcpServer(t),
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, // notification
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
		map[string]any{
			"jsonrpc": "2.0", "id": 3, "method": "tools/call",
			"params": map[string]any{
				"name":      "GreetService.Greet",
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
