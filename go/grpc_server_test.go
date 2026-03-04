package invariant

import (
	"context"
	"net"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// grpcServerServicer implements GreetService RPCs using generated proto types.
type grpcServerServicer struct{}

func (s *grpcServerServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "Hello, " + req.Name}, nil
}

func (s *grpcServerServicer) GreetGroup(_ context.Context, _ *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	return &greetpb.GreetGroupResponse{Messages: []string{"Group hello"}, Count: 1}, nil
}

func startServeGRPC(t *testing.T) (addr string, stop func()) {
	t.Helper()
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	require.NoError(t, srv.Register(&grpcServerServicer{}))

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	addr = lis.Addr().String()

	// Build the gRPC server manually (reusing the internal logic)
	// to avoid the blocking ServeGRPC call.
	files, err := srv.buildProtoFiles()
	require.NoError(t, err)

	gs := grpc.NewServer()

	type svcEntry struct {
		methods []grpc.MethodDesc
	}
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
	return addr, gs.Stop
}

func TestServeGRPCGreet(t *testing.T) {
	addr, stop := startServeGRPC(t)
	defer stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	// Use the same dynamic approach as grpc_client_test.go
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	files, err := srv.buildProtoFiles()
	require.NoError(t, err)

	reqDesc, err := findMessageDescriptor(files, "greet.v1.GreetRequest")
	require.NoError(t, err)
	respDesc, err := findMessageDescriptor(files, "greet.v1.GreetResponse")
	require.NoError(t, err)

	handler := &grpcDynamicHandler{
		conn:       conn,
		methodPath: "/greet.v1.GreetService/Greet",
		reqDesc:    reqDesc,
		respDesc:   respDesc,
	}

	result, err := handler.CallJSON(t.Context(), []byte(`{"name":"ServeGRPC"}`))
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, ServeGRPC")
}

func TestServeGRPCGreetGroup(t *testing.T) {
	addr, stop := startServeGRPC(t)
	defer stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	files, err := srv.buildProtoFiles()
	require.NoError(t, err)

	reqDesc, err := findMessageDescriptor(files, "greet.v1.GreetGroupRequest")
	require.NoError(t, err)
	respDesc, err := findMessageDescriptor(files, "greet.v1.GreetGroupResponse")
	require.NoError(t, err)

	handler := &grpcDynamicHandler{
		conn:       conn,
		methodPath: "/greet.v1.GreetService/GreetGroup",
		reqDesc:    reqDesc,
		respDesc:   respDesc,
	}

	result, err := handler.CallJSON(t.Context(), []byte(`{}`))
	require.NoError(t, err)
	assert.Contains(t, result, "Group hello")
	assert.Contains(t, result, "count")
}

func TestServeGRPCRequiresFDS(t *testing.T) {
	srv := newServer(mustParse(t))
	err := srv.serveGRPC(0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ServerFromDescriptor or ServerFromBytes")
}

// TestServeGRPCViaConnect uses Connect() to proxy through our served gRPC server,
// proving end-to-end: client -> Connect() -> ServeGRPC -> local handler.
func TestServeGRPCViaConnect(t *testing.T) {
	addr, stop := startServeGRPC(t)
	defer stop()

	client := connectServer(t, addr)
	defer client.Stop()

	result, err := client.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"E2E"}`})
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, E2E")
}
