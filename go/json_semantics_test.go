package invariant

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const protoJSONLargeInteger = int64(9007199254740993)

type protoJSONSemanticsServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

func (protoJSONSemanticsServicer) Greet(
	_ context.Context,
	request *greetpb.GreetRequest,
) (*greetpb.GreetResponse, error) {
	count := protoJSONLargeInteger
	if request.AccountSequence != nil {
		count = request.GetAccountSequence()
	}
	return &greetpb.GreetResponse{
		ResponseLabel: "canonical",
		ResponseCount: count,
	}, nil
}

func (protoJSONSemanticsServicer) StreamGreet(
	_ *greetpb.StreamGreetRequest,
	stream grpc.ServerStreamingServer[greetpb.GreetResponse],
) error {
	return stream.Send(&greetpb.GreetResponse{
		ResponseLabel: "canonical",
		ResponseCount: protoJSONLargeInteger,
	})
}

func newProtoJSONSemanticsServer(t *testing.T) *Server {
	t.Helper()
	server, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(server, protoJSONSemanticsServicer{})
	return server
}

func assertCanonicalProtoJSONResponse(t *testing.T, raw []byte) {
	t.Helper()
	var response map[string]any
	require.NoError(t, json.Unmarshal(raw, &response))
	assert.Equal(t, "canonical", response["wireDisplayLabel"])
	assert.Equal(t, "9007199254740993", response["wireResponseCount"])
	assert.NotContains(t, response, "response_label")
	assert.NotContains(t, response, "response_count")
}

func TestJSONProjectionsUseCanonicalProtoJSONNamesAnd64BitStrings(t *testing.T) {
	t.Run("HTTP unary", func(t *testing.T) {
		server := newProtoJSONSemanticsServer(t)
		request := httptest.NewRequest(
			http.MethodPost,
			greetpb.GreetService_Greet_FullMethodName,
			strings.NewReader(`{"name":"json","wireSequenceId":"9007199254740993"}`),
		)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.HTTPHandler().ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		assertCanonicalProtoJSONResponse(t, response.Body.Bytes())
	})

	t.Run("HTTP stream", func(t *testing.T) {
		handler := newProtoJSONSemanticsServer(t).HTTPHandler()
		response := serveHTTPProjectionStream(t, handler)
		require.Equal(t, http.StatusOK, response.Code)
		frames := readAllEnvelopes(t, bytes.NewReader(response.Body.Bytes()))
		require.Len(t, frames, 2)
		assertCanonicalProtoJSONResponse(t, frames[0].payload)
		assert.Equal(t, connectEndStreamFlag, frames[1].flags)
	})

	t.Run("MCP unary", func(t *testing.T) {
		response := sendMCP(t, newProtoJSONSemanticsServer(t), map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name": "greet.v1.GreetService.Greet",
				"arguments": map[string]any{
					"name":           "json",
					"wireSequenceId": "9007199254740993",
				},
			},
		})
		block := response["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
		assertCanonicalProtoJSONResponse(t, []byte(block["text"].(string)))
	})

	t.Run("MCP stream", func(t *testing.T) {
		response := sendMCP(t, newProtoJSONSemanticsServer(t), map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      "greet.v1.GreetService.StreamGreet",
				"arguments": map[string]any{"name": "json", "count": 1},
			},
		})
		block := response["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
		assertCanonicalProtoJSONResponse(t, []byte(block["text"].(string)))
	})

	t.Run("CLI unary", func(t *testing.T) {
		response, err := newProtoJSONSemanticsServer(t).cli(t.Context(), []string{
			"greet.v1.GreetService",
			"Greet",
			"-r",
			`{"name":"json","wireSequenceId":"9007199254740993"}`,
		})
		require.NoError(t, err)
		assertCanonicalProtoJSONResponse(t, []byte(response))
	})

	t.Run("CLI stream", func(t *testing.T) {
		response, err := newProtoJSONSemanticsServer(t).cli(t.Context(), []string{
			"greet.v1.GreetService",
			"StreamGreet",
			"-r",
			`{"name":"json","count":1}`,
		})
		require.NoError(t, err)
		assertCanonicalProtoJSONResponse(t, []byte(strings.TrimSpace(response)))
	})
}
