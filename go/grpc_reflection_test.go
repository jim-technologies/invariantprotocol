package invariant

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
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
		if errors.Is(err, io.EOF) {
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

	descriptorStream, err := client.ServerReflectionInfo(t.Context())
	require.NoError(t, err)
	require.NoError(t, descriptorStream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: "greet.v1.GreetService",
		},
	}))
	response, err := descriptorStream.Recv()
	require.NoError(t, err)
	require.Nil(t, response.GetErrorResponse())
	files := response.GetFileDescriptorResponse().GetFileDescriptorProto()
	require.NotEmpty(t, files)
	var greetFile descriptorpb.FileDescriptorProto
	require.NoError(t, proto.Unmarshal(files[0], &greetFile))
	assert.Equal(t, "greet.proto", greetFile.GetName())
	require.NoError(t, descriptorStream.CloseSend())
}
