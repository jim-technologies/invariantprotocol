package invariant

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	v1reflectiongrpc "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphareflectiongrpc "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

type registeredDescriptorResolver struct {
	server *Server
	once   sync.Once
	files  *protoregistry.Files
	err    error
}

func (r *registeredDescriptorResolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	r.init()
	if r.err != nil {
		return nil, r.err
	}
	return r.files.FindFileByPath(path)
}

func (r *registeredDescriptorResolver) FindDescriptorByName(
	name protoreflect.FullName,
) (protoreflect.Descriptor, error) {
	r.init()
	if r.err != nil {
		return nil, r.err
	}
	return r.files.FindDescriptorByName(name)
}

func (r *registeredDescriptorResolver) init() {
	r.once.Do(func() {
		r.files, r.err = r.server.buildReflectionFiles()
	})
}

func (s *Server) newNativeGRPCServer(opts ...grpc.ServerOption) *grpc.Server {
	serverOptions := slices.Clone(opts)
	serverOptions = append(serverOptions,
		grpc.ChainUnaryInterceptor(s.sharedUnaryInterceptor),
		grpc.ChainStreamInterceptor(s.sharedStreamInterceptor),
	)
	server := grpc.NewServer(serverOptions...)
	reflectionOptions := reflection.ServerOptions{
		Services:           server,
		DescriptorResolver: &registeredDescriptorResolver{server: s},
	}
	reflectionV1 := reflection.NewServerV1(reflectionOptions)
	v1reflectiongrpc.RegisterServerReflectionServer(server, reflectionV1)
	v1alphareflectiongrpc.RegisterServerReflectionServer(server, reflection.NewServer(reflectionOptions))
	return server
}

func (s *Server) buildReflectionFiles() (*protoregistry.Files, error) {
	serviceInfo := s.grpcServer.GetServiceInfo()
	serviceMethods := make(map[string]map[string]struct{}, len(serviceInfo))
	appServices := make([]protoreflect.ServiceDescriptor, 0, len(serviceInfo))
	globalServices := make([]protoreflect.ServiceDescriptor, 0, len(serviceInfo))

	names := make([]string, 0, len(serviceInfo))
	for name, info := range serviceInfo {
		names = append(names, name)
		methods := make(map[string]struct{}, len(info.Methods))
		for _, method := range info.Methods {
			methods[method.Name] = struct{}{}
		}
		serviceMethods[name] = methods
	}
	slices.Sort(names)

	for _, name := range names {
		descriptor, err := s.protoFiles.FindDescriptorByName(protoreflect.FullName(name))
		if err == nil {
			if service, ok := descriptor.(protoreflect.ServiceDescriptor); ok {
				appServices = append(appServices, service)
				continue
			}
		}
		descriptor, err = protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
		if err == nil {
			if service, ok := descriptor.(protoreflect.ServiceDescriptor); ok {
				globalServices = append(globalServices, service)
			}
		}
	}

	descriptors := make(map[string]protoreflect.FileDescriptor)
	var addFile func(protoreflect.FileDescriptor)
	addFile = func(file protoreflect.FileDescriptor) {
		if file == nil {
			return
		}
		if _, exists := descriptors[file.Path()]; exists {
			return
		}
		descriptors[file.Path()] = file
		imports := file.Imports()
		for i := range imports.Len() {
			addFile(imports.Get(i).FileDescriptor)
		}
	}
	// Prefer descriptors from the supplied image whenever an application and
	// the process-global registry contain the same dependency path.
	for _, service := range appServices {
		addFile(service.ParentFile())
	}
	for _, service := range globalServices {
		addFile(service.ParentFile())
	}

	paths := make([]string, 0, len(descriptors))
	for path := range descriptors {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	fds := &descriptorpb.FileDescriptorSet{File: make([]*descriptorpb.FileDescriptorProto, 0, len(paths))}
	for _, path := range paths {
		file := protodesc.ToFileDescriptorProto(descriptors[path])
		serviceIndexes := make(map[int]int, len(file.Service))
		methodIndexes := make(map[int]map[int]int, len(file.Service))
		services := make([]*descriptorpb.ServiceDescriptorProto, 0, len(file.Service))
		for oldServiceIndex, service := range file.Service {
			fullName := service.GetName()
			if file.GetPackage() != "" {
				fullName = file.GetPackage() + "." + fullName
			}
			methods, registered := serviceMethods[fullName]
			if !registered {
				continue
			}
			filteredMethods := make([]*descriptorpb.MethodDescriptorProto, 0, len(service.Method))
			filteredMethodIndexes := make(map[int]int, len(service.Method))
			for oldMethodIndex, method := range service.Method {
				if _, ok := methods[method.GetName()]; ok {
					filteredMethodIndexes[oldMethodIndex] = len(filteredMethods)
					filteredMethods = append(filteredMethods, method)
				}
			}
			service.Method = filteredMethods
			serviceIndexes[oldServiceIndex] = len(services)
			methodIndexes[oldServiceIndex] = filteredMethodIndexes
			services = append(services, service)
		}
		file.Service = services
		if sourceInfo := file.GetSourceCodeInfo(); sourceInfo != nil {
			locations := make([]*descriptorpb.SourceCodeInfo_Location, 0, len(sourceInfo.Location))
			for _, location := range sourceInfo.Location {
				path := location.GetPath()
				if len(path) >= 2 && path[0] == 6 {
					oldServiceIndex := int(path[1])
					newServiceIndex, ok := serviceIndexes[oldServiceIndex]
					if !ok {
						continue
					}
					location.Path[1] = int32(newServiceIndex)
					if len(path) >= 4 && path[2] == 2 {
						newMethodIndex, ok := methodIndexes[oldServiceIndex][int(path[3])]
						if !ok {
							continue
						}
						location.Path[3] = int32(newMethodIndex)
					}
				}
				locations = append(locations, location)
			}
			sourceInfo.Location = locations
		}
		fds.File = append(fds.File, file)
	}

	files, err := protodesc.NewFiles(fds)
	if err != nil {
		return nil, fmt.Errorf("build reflection descriptors: %w", err)
	}
	return files, nil
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
