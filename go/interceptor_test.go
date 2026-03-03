package invariant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// interceptorTestServicer is a simple servicer for interceptor tests.
type interceptorTestServicer struct{}

func (s *interceptorTestServicer) Greet(_ context.Context, req *structpb.Struct) (*structpb.Struct, error) {
	name := ""
	if v, ok := req.GetFields()["name"]; ok {
		name = v.GetStringValue()
	}
	result, _ := structpb.NewStruct(map[string]any{"message": "Hello, " + name})
	return result, nil
}

func (s *interceptorTestServicer) GreetGroup(_ context.Context, _ *structpb.Struct) (*structpb.Struct, error) {
	result, _ := structpb.NewStruct(map[string]any{"messages": []any{"hello"}, "count": float64(1)})
	return result, nil
}

// trackingInterceptor appends a label to the log before and after calling handler.
func trackingInterceptor(label string, log *[]string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		*log = append(*log, label+"-before")
		resp, err := handler(ctx, req)
		*log = append(*log, label+"-after")
		return resp, err
	}
}

func interceptorServer(t *testing.T, interceptors ...grpc.UnaryServerInterceptor) *Server {
	t.Helper()
	srv := newServer(mustParse(t))
	require.NoError(t, srv.Register(&interceptorTestServicer{}))
	for _, i := range interceptors {
		srv.Use(i)
	}
	return srv
}

// --- MCP ---

func TestInterceptorFiresOnMCP(t *testing.T) {
	var log []string
	srv := interceptorServer(t, trackingInterceptor("A", &log))

	sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "Test"},
		},
	})

	assert.Equal(t, []string{"A-before", "A-after"}, log)
}

// --- HTTP ---

func TestInterceptorFiresOnHTTP(t *testing.T) {
	var log []string
	srv := interceptorServer(t, trackingInterceptor("A", &log))

	mux := http.NewServeMux()
	for _, tool := range srv.tools {
		route := "/" + tool.ServiceFullName + "/" + tool.MethodName
		tl := tool
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			srv.handleHTTP(w, r, tl)
		})
	}

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	port := lis.Addr().(*net.TCPAddr).Port
	data, _ := json.Marshal(map[string]any{"name": "Test"})
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/greet.v1.GreetService/Greet", port),
		"application/json",
		bytes.NewReader(data),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	assert.Equal(t, []string{"A-before", "A-after"}, log)
}

// --- CLI ---

func TestInterceptorFiresOnCLI(t *testing.T) {
	var log []string
	srv := interceptorServer(t, trackingInterceptor("A", &log))

	result, err := srv.cli(t.Context(), []string{
		"GreetService", "Greet", "-r", `{"name":"CLI"}`,
	})
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, CLI")
	assert.Equal(t, []string{"A-before", "A-after"}, log)
}

// --- gRPC ---

func TestInterceptorFiresOnGRPC(t *testing.T) {
	var log []string
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	require.NoError(t, srv.Register(&interceptorTestServicer{}))
	srv.Use(trackingInterceptor("A", &log))

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	addr := lis.Addr().String()

	files, err := srv.buildProtoFiles()
	require.NoError(t, err)
	gs := grpc.NewServer()
	type svcEntry struct{ methods []grpc.MethodDesc }
	svcMap := make(map[string]*svcEntry)
	for _, tool := range srv.tools {
		entry, ok := svcMap[tool.ServiceFullName]
		if !ok {
			entry = &svcEntry{}
			svcMap[tool.ServiceFullName] = entry
		}
		reqMD, err := findMessageDescriptor(files, tool.InputType)
		require.NoError(t, err)
		respMD, err := findMessageDescriptor(files, tool.OutputType)
		require.NoError(t, err)
		entry.methods = append(entry.methods, grpc.MethodDesc{
			MethodName: tool.MethodName,
			Handler:    srv.grpcMethodHandler(tool, reqMD, respMD),
		})
	}
	type grpcServicer any
	for svcName, entry := range svcMap {
		gs.RegisterService(&grpc.ServiceDesc{
			ServiceName: svcName,
			HandlerType: (*grpcServicer)(nil),
			Methods:     entry.methods,
		}, struct{}{})
	}
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	// Call via dynamic handler
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	reqDesc, err := findMessageDescriptor(files, "greet.v1.GreetRequest")
	require.NoError(t, err)
	respDesc, err := findMessageDescriptor(files, "greet.v1.GreetResponse")
	require.NoError(t, err)

	dh := &grpcDynamicHandler{conn: conn, methodPath: "/greet.v1.GreetService/Greet", reqDesc: reqDesc, respDesc: respDesc}
	result, err := dh.CallJSON(t.Context(), []byte(`{"name":"gRPC"}`))
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, gRPC")
	assert.Equal(t, []string{"A-before", "A-after"}, log)
}

// --- Chain order ---

func TestInterceptorChainOrder(t *testing.T) {
	var log []string
	srv := interceptorServer(t,
		trackingInterceptor("A", &log),
		trackingInterceptor("B", &log),
	)

	sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "Test"},
		},
	})

	assert.Equal(t, []string{"A-before", "B-before", "B-after", "A-after"}, log)
}

// --- Short-circuit ---

func TestInterceptorShortCircuit(t *testing.T) {
	srv := newServer(mustParse(t))
	require.NoError(t, srv.Register(&interceptorTestServicer{}))

	srv.Use(func(_ context.Context, _ any, _ *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (any, error) {
		return nil, errors.New("blocked by interceptor")
	})

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "Test"},
		},
	})

	result := resp["result"].(map[string]any)
	assert.True(t, result["isError"].(bool))
	content := result["content"].([]any)
	block := content[0].(map[string]any)
	assert.Contains(t, block["text"], "blocked by interceptor")
}

// --- FullMethod ---

func TestInterceptorFullMethod(t *testing.T) {
	var capturedMethod string
	srv := interceptorServer(t, func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		capturedMethod = info.FullMethod
		return handler(ctx, req)
	})

	sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "Test"},
		},
	})

	assert.Equal(t, "/greet.v1.GreetService/Greet", capturedMethod)
}

// --- No interceptors backward compat ---

func TestNoInterceptorsBackwardCompat(t *testing.T) {
	srv := interceptorServer(t) // no interceptors

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "Compat"},
		},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	block := content[0].(map[string]any)
	assert.Contains(t, block["text"], "Hello, Compat")

	assert.Nil(t, result["isError"])
}
