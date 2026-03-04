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

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpcpkg "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// interceptorTestServicer implements GreetService RPCs using generated proto types.
type interceptorTestServicer struct{}

func (s *interceptorTestServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "Hello, " + req.Name}, nil
}

func (s *interceptorTestServicer) GreetGroup(_ context.Context, _ *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	return &greetpb.GreetGroupResponse{Messages: []string{"hello"}, Count: 1}, nil
}

// trackingInterceptor appends a label to the log before and after calling handler.
func trackingInterceptor(label string, log *[]string) UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *ServerCallInfo, handler UnaryHandler) (any, error) {
		*log = append(*log, label+"-before")
		resp, err := handler(ctx, req)
		*log = append(*log, label+"-after")
		return resp, err
	}
}

func interceptorServer(t *testing.T, interceptors ...UnaryServerInterceptor) *Server {
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

	bindings, err := srv.buildHTTPBindings()
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		binding, pathParams, methodMismatch := findHTTPBinding(bindings, r.Method, r.URL.Path)
		if binding == nil {
			if methodMismatch {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			http.NotFound(w, r)
			return
		}
		srv.handleHTTP(w, r, binding, pathParams)
	})

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
	gs := grpcpkg.NewServer()
	type svcEntry struct{ methods []grpcpkg.MethodDesc }
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
		entry.methods = append(entry.methods, grpcpkg.MethodDesc{
			MethodName: tool.MethodName,
			Handler:    srv.grpcMethodHandler(tool, reqMD, respMD),
		})
	}
	type grpcServicer any
	for svcName, entry := range svcMap {
		gs.RegisterService(&grpcpkg.ServiceDesc{
			ServiceName: svcName,
			HandlerType: (*grpcServicer)(nil),
			Methods:     entry.methods,
		}, struct{}{})
	}
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	// Call via dynamic handler
	conn, err := grpcpkg.NewClient(addr, grpcpkg.WithTransportCredentials(insecure.NewCredentials()))
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

	srv.Use(func(_ context.Context, _ any, _ *ServerCallInfo, _ UnaryHandler) (any, error) {
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
	srv := interceptorServer(t, func(ctx context.Context, req any, info *ServerCallInfo, handler UnaryHandler) (any, error) {
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
