package invariant

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	v1reflectiongrpc "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphareflectiongrpc "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// gracefulStopTimeout caps the time GracefulStop is allowed to wait for in-flight
// RPCs before forcing a hard stop on context cancellation.
const gracefulStopTimeout = 5 * time.Second

type fallbackDescriptorResolver struct {
	primary  protodesc.Resolver
	fallback protodesc.Resolver
}

func (r fallbackDescriptorResolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	file, err := r.primary.FindFileByPath(path)
	if err == nil {
		return file, nil
	}
	return r.fallback.FindFileByPath(path)
}

func (r fallbackDescriptorResolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	descriptor, err := r.primary.FindDescriptorByName(name)
	if err == nil {
		return descriptor, nil
	}
	return r.fallback.FindDescriptorByName(name)
}

func (s *Server) newNativeGRPCServer(opts ...grpc.ServerOption) *grpc.Server {
	serverOptions := slices.Clone(opts)
	serverOptions = append(serverOptions,
		grpc.ChainUnaryInterceptor(s.sharedUnaryInterceptor),
		grpc.ChainStreamInterceptor(s.sharedStreamInterceptor),
	)
	server := grpc.NewServer(serverOptions...)
	reflectionOptions := reflection.ServerOptions{Services: server}
	if s.fds != nil {
		if files, err := s.buildProtoFiles(); err == nil {
			reflectionOptions.DescriptorResolver = fallbackDescriptorResolver{
				primary: files, fallback: protoregistry.GlobalFiles,
			}
		}
	}
	reflectionV1 := reflection.NewServerV1(reflectionOptions)
	v1reflectiongrpc.RegisterServerReflectionServer(server, reflectionV1)
	v1alphareflectiongrpc.RegisterServerReflectionServer(server, reflection.NewServer(reflectionOptions))
	return server
}

// GRPCServer returns the owned native grpc.Server. Access freezes Invariant
// registration so serving it directly cannot race projection metadata capture.
func (s *Server) GRPCServer() *grpc.Server {
	s.freeze()
	return s.grpcServer
}

// Serve is the canonical native gRPC lifecycle entry point.
func (s *Server) Serve(listener net.Listener) error { return s.ServeGRPC(listener) }

// ServeGRPC serves the registered generated services on listener.
func (s *Server) ServeGRPC(listener net.Listener) error {
	if listener == nil {
		return errors.New("invariant: ServeGRPC requires a listener")
	}
	s.freeze()
	return s.grpcServer.Serve(listener)
}

func (s *Server) GracefulStop() { s.grpcServer.GracefulStop() }

func (s *Server) Stop() { s.grpcServer.Stop() }

func (s *Server) GetServiceInfo() map[string]grpc.ServiceInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.grpcServer.GetServiceInfo()
}

// serveGRPC retains the port-owning projection helper for ServeProjections.
// New code should create a listener and call Serve/ServeGRPC directly. Native
// options belong on ServerFromDescriptor/ServerFromBytes so every lifecycle
// method addresses the same owned grpc.Server.
func (s *Server) serveGRPC(ctx context.Context, port int, opts ...grpc.ServerOption) error {
	if len(opts) > 0 {
		return errors.New("invariant: projection-level gRPC options are no longer supported; pass grpc.ServerOption values to ServerFromDescriptor or ServerFromBytes")
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}
	defer lis.Close()

	s.freeze()
	gs := s.grpcServer

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

func (s *Server) grpcMethodHandler(tool *Tool, reqMD, respMD protoreflect.MessageDescriptor) func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		req := dynamicpb.NewMessage(reqMD)
		if err := dec(req); err != nil {
			return nil, err
		}

		terminal := func(ctx context.Context, request any) (any, error) {
			requestMessage, ok := request.(proto.Message)
			if !ok {
				return nil, status.Errorf(codes.Internal, "request for %s does not implement proto.Message: %T", tool.callInfo.FullMethod, request)
			}
			// Proto-first: pass the decoded message directly, no JSON round-trip.
			resp, err := s.invoke(ctx, tool, requestMessage)
			if err != nil {
				return nil, err
			}
			if resp == nil {
				return nil, nil
			}
			if dynResp, ok := resp.(*dynamicpb.Message); ok {
				return dynResp, nil
			}

			dynResp := dynamicpb.NewMessage(respMD)
			if resp.ProtoReflect().Descriptor().FullName() == respMD.FullName() {
				b, err := proto.Marshal(resp)
				if err != nil {
					return nil, fmt.Errorf("marshal response to binary: %w", err)
				}
				if err := proto.Unmarshal(b, dynResp); err != nil {
					return nil, fmt.Errorf("unmarshal binary to dynamic: %w", err)
				}
			} else {
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

		if interceptor == nil {
			return terminal(ctx, req)
		}
		info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + tool.ServiceFullName + "/" + tool.MethodName}
		return interceptor(ctx, req, info, terminal)
	}
}
