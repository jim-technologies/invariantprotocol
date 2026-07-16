package invariant

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// grpcDynamicHandler proxies tool calls to a remote gRPC server using dynamic
// proto messages (no generated Go stubs required).
type grpcDynamicHandler struct {
	conn               grpc.ClientConnInterface
	methodPath         string
	respDesc           protoreflect.MessageDescriptor
	newResponse        func() proto.Message
	defaultCallOptions []grpc.CallOption
}

func dynamicMessageFactory(descriptor protoreflect.MessageDescriptor) func() proto.Message {
	return func() proto.Message { return dynamicpb.NewMessage(descriptor) }
}

func (h *grpcDynamicHandler) callProto(ctx context.Context, req proto.Message) (proto.Message, error) {
	var resp proto.Message
	if h.newResponse != nil {
		resp = h.newResponse()
	} else {
		resp = dynamicpb.NewMessage(h.respDesc)
	}
	if incoming, ok := metadata.FromIncomingContext(ctx); ok {
		outgoing, _ := metadata.FromOutgoingContext(ctx)
		ctx = metadata.NewOutgoingContext(ctx, metadata.Join(outgoing, incoming))
	}
	var header, trailer metadata.MD
	callOptions := append([]grpc.CallOption{}, h.defaultCallOptions...)
	callOptions = append(callOptions, grpc.Header(&header), grpc.Trailer(&trailer))
	err := h.conn.Invoke(ctx, h.methodPath, req, resp, callOptions...)
	_ = grpc.SetHeader(ctx, header)
	_ = grpc.SetTrailer(ctx, trailer)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// findMessageDescriptor looks up a message descriptor by full name from a Files registry.
func findMessageDescriptor(files *protoregistry.Files, fullName string) (protoreflect.MessageDescriptor, error) {
	desc, err := files.FindDescriptorByName(protoreflect.FullName(fullName))
	if err != nil {
		return nil, fmt.Errorf("message %q not found in descriptor: %w", fullName, err)
	}
	md, ok := desc.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is not a message descriptor", fullName)
	}
	return md, nil
}
