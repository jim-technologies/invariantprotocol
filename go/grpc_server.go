package invariant

import (
	"errors"
	"net"
	"slices"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	v1reflectiongrpc "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphareflectiongrpc "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

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

// Serve runs the native gRPC server on listener.
func (s *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("invariant: Serve requires a listener")
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
