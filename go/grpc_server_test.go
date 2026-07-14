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
type grpcServerServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

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
	greetpb.RegisterGreetServiceServer(srv, &grpcServerServicer{})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	addr = lis.Addr().String()

	go func() { _ = srv.Serve(lis) }()
	return addr, srv.Stop
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

	result, err := callDynamicJSON(t.Context(), handler, []byte(`{"name":"ServeGRPC"}`))
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

	result, err := callDynamicJSON(t.Context(), handler, []byte(`{}`))
	require.NoError(t, err)
	assert.Contains(t, result, "Group hello")
	assert.Contains(t, result, "count")
}

func TestServeGRPCRejectsNilListener(t *testing.T) {
	srv := newServer(mustParse(t))
	err := srv.ServeGRPC(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a listener")
}

// TestServeGRPCViaConnect uses Connect() to proxy through our served gRPC server,
// proving end-to-end: client -> Connect() -> ServeGRPC -> local handler.
func TestServeGRPCViaConnect(t *testing.T) {
	addr, stop := startServeGRPC(t)
	defer stop()

	client := connectServer(t, addr)

	result, err := client.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"E2E"}`})
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, E2E")
}
