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

func startNativeGRPC(t *testing.T) (addr string, stop func()) {
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

func TestNativeGRPCGreet(t *testing.T) {
	addr, stop := startNativeGRPC(t)
	defer stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	result, err := greetpb.NewGreetServiceClient(conn).Greet(
		t.Context(),
		&greetpb.GreetRequest{Name: "native"},
	)
	require.NoError(t, err)
	assert.Equal(t, "Hello, native", result.GetMessage())
}

func TestNativeGRPCGreetGroup(t *testing.T) {
	addr, stop := startNativeGRPC(t)
	defer stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	result, err := greetpb.NewGreetServiceClient(conn).GreetGroup(
		t.Context(),
		&greetpb.GreetGroupRequest{},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"Group hello"}, result.GetMessages())
	assert.Equal(t, int32(1), result.GetCount())
}

func TestServeRejectsNilListener(t *testing.T) {
	srv, createErr := ServerFromDescriptor(descriptorPath())
	require.NoError(t, createErr)
	err := srv.Serve(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a listener")
}

// TestNativeGRPCViaConnectGRPC proves the remote projection end to end.
func TestNativeGRPCViaConnectGRPC(t *testing.T) {
	addr, stop := startNativeGRPC(t)
	defer stop()

	client := connectServer(t, addr)

	result, err := client.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"E2E"}`})
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, E2E")
}
