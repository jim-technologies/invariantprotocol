package invariant

import (
	"os"
	"testing"

	invpb "github.com/jim-technologies/invariantprotocol/go/gen/invariant/v1"
	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestGeneratedServiceDescriptorMustMatchRuntimeDescriptor(t *testing.T) {
	server, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	serviceInfo := server.parsed.Services[greetpb.GreetService_ServiceDesc.ServiceName]
	require.NotNil(t, serviceInfo)
	require.NoError(t, server.validateDescriptorAgreement(&greetpb.GreetService_ServiceDesc, serviceInfo))

	t.Run("missing method", func(t *testing.T) {
		desc := cloneServiceDesc(greetpb.GreetService_ServiceDesc)
		desc.Methods = desc.Methods[:1]
		err := server.validateDescriptorAgreement(&desc, serviceInfo)
		require.ErrorContains(t, err, "missing descriptor.binpb methods: GreetGroup")
	})

	t.Run("unknown method", func(t *testing.T) {
		desc := cloneServiceDesc(greetpb.GreetService_ServiceDesc)
		desc.Methods[0].MethodName = "Unknown"
		err := server.validateDescriptorAgreement(&desc, serviceInfo)
		require.ErrorContains(t, err, "method \"Unknown\" is absent from descriptor.binpb")
	})

	t.Run("stream cardinality", func(t *testing.T) {
		desc := cloneServiceDesc(greetpb.GreetService_ServiceDesc)
		desc.Streams[0].ServerStreams = false
		err := server.validateDescriptorAgreement(&desc, serviceInfo)
		require.ErrorContains(t, err, "streaming cardinality disagrees")
	})

	t.Run("message types", func(t *testing.T) {
		mismatched := proto.Clone(serviceInfo).(*invpb.ServiceInfo)
		mismatched.Methods["Greet"].InputType = "greet.v1.GreetGroupRequest"
		err := server.validateDescriptorAgreement(&greetpb.GreetService_ServiceDesc, mismatched)
		require.ErrorContains(t, err, "message types disagree")
	})

	t.Run("linked descriptor required", func(t *testing.T) {
		desc := cloneServiceDesc(greetpb.GreetService_ServiceDesc)
		desc.ServiceName = "greet.v1.NotLinked"
		err := server.validateDescriptorAgreement(&desc, serviceInfo)
		require.ErrorContains(t, err, "has no linked protobuf descriptor")
	})
}

func TestGeneratedRegistrationRejectsSameNameSchemaDriftAtomically(t *testing.T) {
	raw, err := os.ReadFile(descriptorPath())
	require.NoError(t, err)
	var descriptors descriptorpb.FileDescriptorSet
	require.NoError(t, proto.Unmarshal(raw, &descriptors))

	mutated := false
	for _, file := range descriptors.File {
		if file.GetPackage() != "greet.v1" {
			continue
		}
		for _, message := range file.MessageType {
			if message.GetName() != "GreetRequest" {
				continue
			}
			for _, field := range message.Field {
				if field.GetName() == "name" {
					field.Type = descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()
					mutated = true
				}
			}
		}
	}
	require.True(t, mutated)
	staleBytes, err := proto.Marshal(&descriptors)
	require.NoError(t, err)
	server, err := ServerFromBytes(staleBytes)
	require.NoError(t, err)

	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		greetpb.RegisterGreetServiceServer(server, &testGreetServicer{})
	}()
	registrationErr, ok := panicValue.(error)
	require.True(t, ok, "registration panic = %#v", panicValue)
	require.ErrorContains(t, registrationErr, "protobuf file \"greet.proto\" disagrees with descriptor.binpb")
	require.NotContains(t, server.GetServiceInfo(), greetpb.GreetService_ServiceDesc.ServiceName)
	require.Empty(t, server.tools)

	connection, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	err = server.ConnectGRPC(connection)
	require.ErrorContains(t, err, "linked generated message")
	require.ErrorContains(t, err, "protobuf file \"greet.proto\"")
	require.ErrorContains(t, err, "disagrees with descriptor.binpb")
	require.NotContains(t, server.GetServiceInfo(), greetpb.GreetService_ServiceDesc.ServiceName)
	require.Empty(t, server.tools)

	err = server.ConnectHTTP("https://example.invalid")
	require.ErrorContains(t, err, "linked generated message")
	require.ErrorContains(t, err, "protobuf file \"greet.proto\"")
	require.ErrorContains(t, err, "disagrees with descriptor.binpb")
	require.NotContains(t, server.GetServiceInfo(), greetpb.GreetService_ServiceDesc.ServiceName)
	require.Empty(t, server.tools)
}

func TestDescriptorOnlyProxyUsesDynamicMessages(t *testing.T) {
	descriptors := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    new("dynamic.proto"),
		Package: new("dynamic.v1"),
		Syntax:  new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: new("EchoRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:   new("value"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				}},
			},
			{
				Name: new("EchoResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:   new("value"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				}},
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: new("EchoService"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       new("Echo"),
				InputType:  new(".dynamic.v1.EchoRequest"),
				OutputType: new(".dynamic.v1.EchoResponse"),
			}},
		}},
	}}}
	raw, err := proto.Marshal(descriptors)
	require.NoError(t, err)
	server, err := ServerFromBytes(raw)
	require.NoError(t, err)
	connection, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })

	require.NoError(t, server.ConnectGRPC(connection))
	request := server.tools["EchoService.Echo"].newRequest()
	_, ok := request.(*dynamicpb.Message)
	require.True(t, ok, "descriptor-only request type = %T", request)
}

func cloneServiceDesc(desc grpc.ServiceDesc) grpc.ServiceDesc {
	desc.Methods = append([]grpc.MethodDesc(nil), desc.Methods...)
	desc.Streams = append([]grpc.StreamDesc(nil), desc.Streams...)
	return desc
}
