package invariant

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// gracefulStopTimeout caps the time GracefulStop is allowed to wait for in-flight
// RPCs before forcing a hard stop on context cancellation.
const gracefulStopTimeout = 5 * time.Second

// serveGRPC starts a blocking gRPC server on the given port. Honors ctx for
// graceful shutdown. Optional grpc.ServerOption values are passed to grpc.NewServer.
func (s *Server) serveGRPC(ctx context.Context, port int, opts ...grpc.ServerOption) error {
	if s.fds == nil {
		return errors.New("serveGRPC requires a Server created via ServerFromDescriptor or ServerFromBytes")
	}

	files, err := protodesc.NewFiles(s.fds)
	if err != nil {
		return fmt.Errorf("build file descriptors: %w", err)
	}

	gs := grpc.NewServer(opts...)

	// Group tools by service for ServiceDesc registration. Unary and stream
	// methods live in different slices on the same desc.
	type svcEntry struct {
		methods []grpc.MethodDesc
		streams []grpc.StreamDesc
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

		if t.ServerStreaming {
			entry.streams = append(entry.streams, grpc.StreamDesc{
				StreamName:    t.MethodName,
				Handler:       s.grpcStreamHandler(t, rmd, rsmd),
				ServerStreams: true,
			})
			continue
		}

		entry.methods = append(entry.methods, grpc.MethodDesc{
			MethodName: t.MethodName,
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
			Streams:     entry.streams,
		}, struct{}{})
	}

	// gRPC reflection — grpcurl, Buf Studio, Connect debug clients work out of the box.
	reflection.Register(gs)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}

	errc := make(chan error, 1)
	go func() { errc <- gs.Serve(lis) }()

	select {
	case <-ctx.Done():
		// Bounded graceful shutdown: give in-flight RPCs up to gracefulStopTimeout
		// to finish, then force-stop. Without the timeout a hung handler would
		// block GracefulStop forever.
		stopped := make(chan struct{})
		go func() {
			gs.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(gracefulStopTimeout):
			gs.Stop()
			<-stopped
		}
		return ctx.Err()
	case err := <-errc:
		return err
	}
}

// grpcStreamHandler bridges grpc-go's stream handler signature to the
// framework's StreamHandler. The first message off the stream is the
// request; each invariant.ServerStream.Send writes one gRPC stream frame.
//
// respMD is unused directly because the user's stream handler emits typed
// proto.Message values — when those are *dynamicpb.Message (proxy mode,
// future), the framework converter at the handler boundary handles it.
func (s *Server) grpcStreamHandler(tool *Tool, reqMD, _ protoreflect.MessageDescriptor) func(srv any, stream grpc.ServerStream) error {
	return func(_ any, stream grpc.ServerStream) error {
		req := dynamicpb.NewMessage(reqMD)
		if err := stream.RecvMsg(req); err != nil {
			return err
		}
		ctx := stream.Context()
		cb := newCallbackStream(ctx, func(msg proto.Message) error {
			return stream.SendMsg(msg)
		})
		defer cb.close()

		return s.invokeStream(tool, req, cb)
	}
}

func (s *Server) grpcMethodHandler(tool *Tool, reqMD, respMD protoreflect.MessageDescriptor) func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	return func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		req := dynamicpb.NewMessage(reqMD)
		if err := dec(req); err != nil {
			return nil, err
		}

		// Proto-first: pass dynamic message directly, no JSON round-trip.
		resp, err := s.invoke(ctx, tool, req)
		if err != nil {
			return nil, err
		}

		// If the response is already a *dynamicpb.Message, return it directly.
		if dynResp, ok := resp.(*dynamicpb.Message); ok {
			return dynResp, nil
		}

		// Convert typed proto response back to dynamicpb for gRPC codec.
		dynResp := dynamicpb.NewMessage(respMD)
		if resp.ProtoReflect().Descriptor().FullName() == respMD.FullName() {
			// Same proto type — fast binary conversion.
			b, err := proto.Marshal(resp)
			if err != nil {
				return nil, fmt.Errorf("marshal response to binary: %w", err)
			}
			if err := proto.Unmarshal(b, dynResp); err != nil {
				return nil, fmt.Errorf("unmarshal binary to dynamic: %w", err)
			}
		} else {
			// Different proto types (e.g. structpb.Struct) — fall back to JSON.
			b, err := protojson.Marshal(resp)
			if err != nil {
				return nil, fmt.Errorf("marshal response to JSON: %w", err)
			}
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, dynResp); err != nil {
				return nil, fmt.Errorf("unmarshal JSON to dynamic: %w", err)
			}
		}
		return dynResp, nil
	}
}
