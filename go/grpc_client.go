package invariant

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// grpcDynamicHandler proxies tool calls to a remote gRPC server using dynamic
// proto messages (no generated Go stubs required).
type grpcDynamicHandler struct {
	conn       *grpc.ClientConn
	methodPath string
	reqDesc    protoreflect.MessageDescriptor
	respDesc   protoreflect.MessageDescriptor
}

func (h *grpcDynamicHandler) CallJSON(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	req := dynamicpb.NewMessage(h.reqDesc)
	if len(argsJSON) > 0 && string(argsJSON) != "null" {
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(argsJSON, req); err != nil {
			return "", fmt.Errorf("unmarshal request: %w", err)
		}
	}

	resp := dynamicpb.NewMessage(h.respDesc)
	if err := h.conn.Invoke(ctx, h.methodPath, req, resp); err != nil {
		return "", fmt.Errorf("grpc call %s: %w", h.methodPath, err)
	}

	out, err := (protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}).Marshal(resp)
	if err != nil {
		return "", fmt.Errorf("marshal response: %w", err)
	}
	return string(out), nil
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
