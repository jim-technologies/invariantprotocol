package invariant

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
)

func TestGRPCReflection(t *testing.T) {
	addr, stop := startServeGRPC(t)
	defer stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	client := reflectionpb.NewServerReflectionClient(conn)
	stream, err := client.ServerReflectionInfo(t.Context())
	require.NoError(t, err)

	require.NoError(t, stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}))
	require.NoError(t, stream.CloseSend())

	services := map[string]bool{}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		list := resp.GetListServicesResponse()
		if list == nil {
			continue
		}
		for _, svc := range list.Service {
			services[svc.Name] = true
		}
	}

	assert.True(t, services["greet.v1.GreetService"], "expected greet.v1.GreetService in reflection list, got: %v", services)
}
