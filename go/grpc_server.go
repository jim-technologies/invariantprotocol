package invariant

import (
	"context"
	"errors"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// serveGRPC starts a blocking gRPC server on the given port.
func (s *Server) serveGRPC(port int) error {
	if s.fds == nil {
		return errors.New("serveGRPC requires a Server created via ServerFromDescriptor or ServerFromBytes")
	}

	files, err := protodesc.NewFiles(s.fds)
	if err != nil {
		return fmt.Errorf("build file descriptors: %w", err)
	}

	gs := grpc.NewServer()

	// Group tools by service for ServiceDesc registration.
	type svcEntry struct {
		methods []grpc.MethodDesc
	}
	svcMap := make(map[string]*svcEntry)

	for _, tool := range s.tools {
		entry, ok := svcMap[tool.ServiceFullName]
		if !ok {
			entry = &svcEntry{}
			svcMap[tool.ServiceFullName] = entry
		}

		reqMD, err := findMessageDescriptor(files, tool.InputType)
		if err != nil {
			return err
		}
		respMD, err := findMessageDescriptor(files, tool.OutputType)
		if err != nil {
			return err
		}

		t := tool // capture for closure
		rmd := reqMD
		rsmd := respMD
		entry.methods = append(entry.methods, grpc.MethodDesc{
			MethodName: tool.MethodName,
			Handler:    s.grpcMethodHandler(t, rmd, rsmd),
		})
	}

	// Register each service.
	type grpcServicer any
	for svcName, entry := range svcMap {
		gs.RegisterService(&grpc.ServiceDesc{
			ServiceName: svcName,
			HandlerType: (*grpcServicer)(nil),
			Methods:     entry.methods,
		}, struct{}{})
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}
	return gs.Serve(lis)
}

func (s *Server) grpcMethodHandler(tool *Tool, reqMD, respMD protoreflect.MessageDescriptor) func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	return func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		req := dynamicpb.NewMessage(reqMD)
		if err := dec(req); err != nil {
			return nil, err
		}

		// Marshal request to JSON, invoke handler, unmarshal response from JSON.
		reqJSON, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("marshal request to JSON: %w", err)
		}

		resultJSON, err := s.invoke(ctx, tool, reqJSON)
		if err != nil {
			return nil, err
		}

		resp := dynamicpb.NewMessage(respMD)
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(resultJSON), resp); err != nil {
			return nil, fmt.Errorf("unmarshal response JSON: %w", err)
		}
		return resp, nil
	}
}
