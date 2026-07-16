package invariant

import (
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestGRPCReflection(t *testing.T) {
	addr, stop := startNativeGRPC(t)
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

func TestGRPCReflectionMatchesRegisteredSurface(t *testing.T) {
	t.Run("generated registration hides descriptor-only services", func(t *testing.T) {
		raw, err := os.ReadFile(descriptorPath())
		require.NoError(t, err)
		var descriptors descriptorpb.FileDescriptorSet
		require.NoError(t, proto.Unmarshal(raw, &descriptors))
		descriptors.File = append(descriptors.File, &descriptorpb.FileDescriptorProto{
			Name:       new("hidden.proto"),
			Package:    new("hidden.v1"),
			Syntax:     new("proto3"),
			Dependency: []string{"greet.proto"},
			Service: []*descriptorpb.ServiceDescriptorProto{{
				Name: new("HiddenService"),
				Method: []*descriptorpb.MethodDescriptorProto{{
					Name:       new("Hidden"),
					InputType:  new(".greet.v1.GreetRequest"),
					OutputType: new(".greet.v1.GreetResponse"),
				}},
			}},
		})
		descriptorBytes, err := proto.Marshal(&descriptors)
		require.NoError(t, err)

		server, err := ServerFromBytes(descriptorBytes)
		require.NoError(t, err)
		greetpb.RegisterGreetServiceServer(server, &nativeTestService{})
		healthpb.RegisterHealthServer(server, health.NewServer())
		client := reflectionpb.NewServerReflectionClient(nativeTestStartConnection(t, server))
		services := reflectionServices(t, client)
		assert.Contains(t, services, greetpb.GreetService_ServiceDesc.ServiceName)
		assert.Contains(t, services, healthpb.Health_ServiceDesc.ServiceName)
		assert.NotContains(t, services, "hidden.v1.HiddenService")

		response := reflectionSymbol(t, client, "hidden.v1.HiddenService")
		require.NotNil(t, response.GetErrorResponse())
		assert.Equal(t, int32(codes.NotFound), response.GetErrorResponse().GetErrorCode())

		response = reflectionSymbol(t, client, reflectionpb.ServerReflection_ServiceDesc.ServiceName)
		require.Nil(t, response.GetErrorResponse())
		require.NotEmpty(t, response.GetFileDescriptorResponse().GetFileDescriptorProto())

		response = reflectionSymbol(t, client, healthpb.Health_ServiceDesc.ServiceName)
		require.Nil(t, response.GetErrorResponse())
		require.NotEmpty(t, response.GetFileDescriptorResponse().GetFileDescriptorProto())
	})

	t.Run("remote proxy advertises only registered unary methods", func(t *testing.T) {
		server, err := ServerFromDescriptor(descriptorPath())
		require.NoError(t, err)
		remote, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, remote.Close()) })
		require.NoError(t, server.ConnectGRPC(remote))

		client := reflectionpb.NewServerReflectionClient(nativeTestStartConnection(t, server))
		assert.Contains(t, reflectionServices(t, client), greetpb.GreetService_ServiceDesc.ServiceName)
		response := reflectionSymbol(t, client, greetpb.GreetService_ServiceDesc.ServiceName)
		require.Nil(t, response.GetErrorResponse())
		files := response.GetFileDescriptorResponse().GetFileDescriptorProto()
		require.NotEmpty(t, files)
		var greetFile descriptorpb.FileDescriptorProto
		require.NoError(t, proto.Unmarshal(files[0], &greetFile))
		require.Len(t, greetFile.Service, 1)
		methods := make([]string, 0, len(greetFile.Service[0].Method))
		for _, method := range greetFile.Service[0].Method {
			methods = append(methods, method.GetName())
		}
		assert.ElementsMatch(t, []string{"Greet", "GreetGroup"}, methods)

		response = reflectionSymbol(t, client, "greet.v1.GreetService.StreamGreet")
		require.NotNil(t, response.GetErrorResponse())
		assert.Equal(t, int32(codes.NotFound), response.GetErrorResponse().GetErrorCode())
	})

	t.Run("filtered descriptors retain remapped source comments", func(t *testing.T) {
		descriptors := &descriptorpb.FileDescriptorSet{
			File: []*descriptorpb.FileDescriptorProto{{
				Name:    new("reflection_filter.proto"),
				Package: new("reflect.v1"),
				Syntax:  new("proto3"),
				MessageType: []*descriptorpb.DescriptorProto{
					{Name: new("Request")},
					{Name: new("Response")},
				},
				Service: []*descriptorpb.ServiceDescriptorProto{
					{
						Name: new("HiddenService"),
						Method: []*descriptorpb.MethodDescriptorProto{{
							Name:            new("HiddenStream"),
							InputType:       new(".reflect.v1.Request"),
							OutputType:      new(".reflect.v1.Response"),
							ServerStreaming: new(true),
						}},
					},
					{
						Name: new("ProxyService"),
						Method: []*descriptorpb.MethodDescriptorProto{
							{
								Name:            new("RemovedStream"),
								InputType:       new(".reflect.v1.Request"),
								OutputType:      new(".reflect.v1.Response"),
								ServerStreaming: new(true),
							},
							{
								Name:       new("Unary"),
								InputType:  new(".reflect.v1.Request"),
								OutputType: new(".reflect.v1.Response"),
							},
						},
					},
				},
				SourceCodeInfo: &descriptorpb.SourceCodeInfo{
					Location: []*descriptorpb.SourceCodeInfo_Location{
						{
							Path: []int32{6, 0}, Span: []int32{1, 0, 1},
							LeadingComments: new("hidden service"),
						},
						{
							Path: []int32{6, 1}, Span: []int32{2, 0, 2},
							LeadingComments: new("proxy service"),
						},
						{
							Path: []int32{6, 1, 2, 0}, Span: []int32{3, 0, 3},
							LeadingComments: new("removed stream"),
						},
						{
							Path: []int32{6, 1, 2, 1}, Span: []int32{4, 0, 4},
							LeadingComments: new("retained unary"),
						},
					},
				},
			}},
		}
		raw, err := proto.Marshal(descriptors)
		require.NoError(t, err)
		server, err := ServerFromBytes(raw)
		require.NoError(t, err)
		remote, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, remote.Close()) })
		require.NoError(t, server.ConnectGRPC(remote))

		client := reflectionpb.NewServerReflectionClient(nativeTestStartConnection(t, server))
		services := reflectionServices(t, client)
		assert.Contains(t, services, "reflect.v1.ProxyService")
		assert.NotContains(t, services, "reflect.v1.HiddenService")

		response := reflectionSymbol(t, client, "reflect.v1.ProxyService.Unary")
		require.Nil(t, response.GetErrorResponse())
		files := response.GetFileDescriptorResponse().GetFileDescriptorProto()
		require.NotEmpty(t, files)
		var reflected descriptorpb.FileDescriptorProto
		require.NoError(t, proto.Unmarshal(files[0], &reflected))
		require.Len(t, reflected.Service, 1)
		assert.Equal(t, "ProxyService", reflected.Service[0].GetName())
		require.Len(t, reflected.Service[0].Method, 1)
		assert.Equal(t, "Unary", reflected.Service[0].Method[0].GetName())

		comments := make(map[string]string)
		for _, location := range reflected.GetSourceCodeInfo().GetLocation() {
			switch {
			case slices.Equal(location.GetPath(), []int32{6, 0}):
				comments["service"] = location.GetLeadingComments()
			case slices.Equal(location.GetPath(), []int32{6, 0, 2, 0}):
				comments["method"] = location.GetLeadingComments()
			}
			if len(location.GetPath()) >= 2 && location.GetPath()[0] == 6 {
				assert.Less(t, int(location.GetPath()[1]), len(reflected.Service))
			}
		}
		assert.Equal(t, map[string]string{
			"service": "proxy service",
			"method":  "retained unary",
		}, comments)
	})
}

func reflectionSymbol(
	t *testing.T,
	client reflectionpb.ServerReflectionClient,
	symbol string,
) *reflectionpb.ServerReflectionResponse {
	t.Helper()
	stream, err := client.ServerReflectionInfo(t.Context())
	require.NoError(t, err)
	require.NoError(t, stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: symbol,
		},
	}))
	require.NoError(t, stream.CloseSend())
	response, err := stream.Recv()
	require.NoError(t, err)
	return response
}

func reflectionServices(
	t *testing.T,
	client reflectionpb.ServerReflectionClient,
) map[string]struct{} {
	t.Helper()
	stream, err := client.ServerReflectionInfo(t.Context())
	require.NoError(t, err)
	require.NoError(t, stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}))
	require.NoError(t, stream.CloseSend())
	response, err := stream.Recv()
	require.NoError(t, err)
	services := make(map[string]struct{})
	for _, service := range response.GetListServicesResponse().GetService() {
		services[service.GetName()] = struct{}{}
	}
	return services
}
