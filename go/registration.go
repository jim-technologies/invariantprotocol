package invariant

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	invpb "github.com/jim-technologies/invariantprotocol/go/gen/invariant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

type registeredService struct {
	desc    *grpc.ServiceDesc
	service any
}

type registeredUnaryHandler struct {
	method  *grpc.MethodDesc
	service any
}

type registeredStreamHandler struct {
	method  *grpc.StreamDesc
	service any
}

type legacyGRPCService any

var _ grpc.ServiceRegistrar = (*Server)(nil)

// RegisterService implements grpc.ServiceRegistrar. Generated
// Register<Service>Server functions can register directly with an Invariant
// server; the same descriptor and implementation are retained for HTTP, MCP,
// and CLI projection dispatch.
//
// grpc.ServiceRegistrar cannot return an error, so invalid or late
// registrations panic with a deterministic invariant-prefixed message, just as
// generated registration is expected to fail during process setup.
func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl any) {
	if err := s.registerService(desc, impl); err != nil {
		panic(err)
	}
}

func (s *Server) registerService(desc *grpc.ServiceDesc, impl any) error {
	subject := "service"
	if desc != nil && desc.ServiceName != "" {
		subject = fmt.Sprintf("%q", desc.ServiceName)
	}
	return s.registerServices([]registeredService{{desc: desc, service: impl}}, subject)
}

func validateService(desc *grpc.ServiceDesc, impl any) error {
	if desc == nil {
		return errors.New("invariant: cannot register a nil grpc.ServiceDesc")
	}
	if desc.ServiceName == "" {
		return errors.New("invariant: cannot register a service with an empty name")
	}
	if desc.HandlerType == nil || reflect.TypeOf(desc.HandlerType).Kind() != reflect.Pointer ||
		reflect.TypeOf(desc.HandlerType).Elem().Kind() != reflect.Interface {
		return fmt.Errorf("invariant: service %q has an invalid HandlerType", desc.ServiceName)
	}
	if impl == nil {
		return fmt.Errorf("invariant: service %q has a nil implementation", desc.ServiceName)
	}
	if !reflect.TypeOf(impl).Implements(reflect.TypeOf(desc.HandlerType).Elem()) {
		return fmt.Errorf("invariant: service %q implementation %T does not satisfy %v", desc.ServiceName, impl, reflect.TypeOf(desc.HandlerType).Elem())
	}
	return nil
}

// registerServices validates and commits a group of registrations atomically
// with respect to Invariant's freeze boundary. This is used by remote proxy
// registration, where one connection may expose several services.
func (s *Server) registerServices(services []registeredService, subject string) error {
	if len(services) == 0 {
		return nil
	}
	for _, service := range services {
		if err := validateService(service.desc, service.service); err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen {
		return fmt.Errorf("invariant: service registration is frozen; cannot register %s", subject)
	}

	existingNative := s.grpcServer.GetServiceInfo()
	serviceNames := make(map[string]struct{}, len(services))
	stagedTools := make([][]*Tool, len(services))
	toolOwners := make(map[string]string, len(s.tools))
	for name, tool := range s.tools {
		toolOwners[name] = tool.ServiceFullName
	}
	for i, service := range services {
		name := service.desc.ServiceName
		if _, exists := serviceNames[name]; exists {
			return fmt.Errorf("invariant: duplicate service registration for %q", name)
		}
		serviceNames[name] = struct{}{}
		if _, exists := s.registeredServices[name]; exists {
			return fmt.Errorf("invariant: duplicate service registration for %q", name)
		}
		if _, exists := existingNative[name]; exists {
			return fmt.Errorf("invariant: duplicate native gRPC service registration for %q", name)
		}

		stagedTools[i] = s.toolsForService(service.desc, service.service)
		for _, tool := range stagedTools[i] {
			if owner, exists := toolOwners[tool.Name]; exists {
				return fmt.Errorf(
					"tool name collision: %q is registered by both %q and %q; use Server.Include() to scope to one",
					tool.Name, owner, tool.ServiceFullName,
				)
			}
			toolOwners[tool.Name] = tool.ServiceFullName
		}
	}

	// Pre-validation above avoids grpc-go's process-terminating duplicate/type
	// checks and ensures projection metadata cannot be committed partially.
	for i, service := range services {
		s.grpcServer.RegisterService(service.desc, service.service)
		s.registeredServices[service.desc.ServiceName] = service
		for _, tool := range stagedTools[i] {
			if err := s.addTool(tool); err != nil {
				return err // collision checks above make this unreachable in practice
			}
		}
	}
	return nil
}

func (s *Server) toolsForService(desc *grpc.ServiceDesc, impl any) []*Tool {
	if s.parsed == nil {
		return nil
	}
	svcInfo, ok := s.parsed.Services[desc.ServiceName]
	if !ok {
		// Infrastructure services (for example grpc.health.v1.Health) can be
		// registered normally even when they are not part of the projected FDS.
		return nil
	}

	var tools []*Tool
	for i := range desc.Methods {
		method := &desc.Methods[i]
		methodInfo, ok := svcInfo.Methods[method.MethodName]
		if !ok || methodInfo.ClientStreaming || methodInfo.ServerStreaming ||
			!s.shouldIncludeLocked(desc.ServiceName, method.MethodName) {
			continue
		}
		tools = append(tools, s.registeredTool(svcInfo, methodInfo, method.MethodName,
			&registeredUnaryHandler{method: method, service: impl}))
	}
	for i := range desc.Streams {
		method := &desc.Streams[i]
		methodInfo, ok := svcInfo.Methods[method.StreamName]
		if !ok || methodInfo.ClientStreaming || !methodInfo.ServerStreaming ||
			!s.shouldIncludeLocked(desc.ServiceName, method.StreamName) {
			continue
		}
		tools = append(tools, s.registeredTool(svcInfo, methodInfo, method.StreamName,
			&registeredStreamHandler{method: method, service: impl}))
	}
	return tools
}

func (s *Server) registeredTool(svcInfo *invpb.ServiceInfo, methodInfo *invpb.MethodInfo, methodName string, handler any) *Tool {
	toolName := svcInfo.Name + "." + methodName
	description := methodInfo.Comment
	if description == "" {
		description = toolName
	}
	return &Tool{
		Name:            toolName,
		Description:     description,
		InputSchema:     s.schemaGen.MessageToSchema(methodInfo.InputType),
		Handler:         handler,
		InputType:       methodInfo.InputType,
		OutputType:      methodInfo.OutputType,
		ServiceFullName: svcInfo.FullName,
		MethodName:      methodName,
		ServerStreaming: methodInfo.ServerStreaming,
		newRequest:      s.messageFactory(methodInfo.InputType),
	}
}

func (s *Server) messageFactory(fullName string) func() proto.Message {
	if mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(fullName)); err == nil {
		return func() proto.Message { return mt.New().Interface() }
	}
	if s.fds == nil {
		return nil
	}
	files, err := s.buildProtoFiles()
	if err != nil {
		return nil
	}
	md, err := findMessageDescriptor(files, fullName)
	if err != nil {
		return nil
	}
	return func() proto.Message { return dynamicpb.NewMessage(md) }
}

// legacyServiceDesc turns the reflection-based compatibility API into the
// same grpc.ServiceDesc registration model used by generated code.
func (s *Server) legacyServiceDesc(servicer any, serviceName string, svcInfo *invpb.ServiceInfo) (*grpc.ServiceDesc, error) {
	servicerValue := reflect.ValueOf(servicer)
	desc := &grpc.ServiceDesc{
		ServiceName: serviceName,
		HandlerType: (*legacyGRPCService)(nil),
	}

	for methodName, methodInfo := range svcInfo.Methods {
		if methodInfo.ClientStreaming {
			continue
		}
		method := servicerValue.MethodByName(methodName)
		if !method.IsValid() {
			continue
		}
		factory := buildRequestFactory(method.Interface())
		if factory == nil {
			factory = s.messageFactory(methodInfo.InputType)
		}
		if factory == nil {
			return nil, fmt.Errorf("method %s has no usable protobuf request type", methodName)
		}
		fullMethod := "/" + serviceName + "/" + methodName

		if methodInfo.ServerStreaming {
			raw, err := buildStreamHandler(method.Interface())
			if err != nil {
				buildErr := err
				raw = func(any, ServerStream) error { return buildErr }
			}
			desc.Streams = append(desc.Streams, grpc.StreamDesc{
				StreamName:    methodName,
				ServerStreams: true,
				Handler: func(_ any, stream grpc.ServerStream) error {
					req := factory()
					if err := stream.RecvMsg(req); err != nil {
						return err
					}
					return raw(req, legacyServerStream{ServerStream: stream})
				},
			})
			continue
		}

		raw, err := buildInvokeHandler(method.Interface())
		if err != nil {
			buildErr := err
			raw = func(context.Context, any) (any, error) { return nil, buildErr }
		}
		desc.Methods = append(desc.Methods, grpc.MethodDesc{
			MethodName: methodName,
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				req := factory()
				if err := dec(req); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return raw(ctx, req)
				}
				info := &grpc.UnaryServerInfo{Server: srv, FullMethod: fullMethod}
				return interceptor(ctx, req, info, raw)
			},
		})
	}
	if len(desc.Methods) == 0 && len(desc.Streams) == 0 {
		return nil, fmt.Errorf("no supported methods found for service %q", serviceName)
	}
	return desc, nil
}

type legacyServerStream struct {
	grpc.ServerStream
}

func (s legacyServerStream) Send(msg proto.Message) error { return s.SendMsg(msg) }

func copyProtoMessage(dst, src any) error {
	dstMsg, ok := dst.(proto.Message)
	if !ok {
		return fmt.Errorf("decode target does not implement proto.Message: %T", dst)
	}
	srcMsg, ok := src.(proto.Message)
	if !ok {
		return fmt.Errorf("request does not implement proto.Message: %T", src)
	}
	proto.Reset(dstMsg)
	if reflect.TypeOf(dstMsg) == reflect.TypeOf(srcMsg) {
		proto.Merge(dstMsg, srcMsg)
		return nil
	}
	if dstMsg.ProtoReflect().Descriptor().FullName() == srcMsg.ProtoReflect().Descriptor().FullName() {
		data, err := proto.Marshal(srcMsg)
		if err != nil {
			return err
		}
		return proto.Unmarshal(data, dstMsg)
	}
	data, err := protojson.Marshal(srcMsg)
	if err != nil {
		return err
	}
	return (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, dstMsg)
}

func (s *Server) freeze() {
	if s.frozenFast.Load() {
		return
	}
	s.mu.Lock()
	s.frozen = true
	s.frozenFast.Store(true)
	s.mu.Unlock()
}

func (s *Server) ensureRegistrationOpen(subject string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.frozen {
		return fmt.Errorf("invariant: service registration is frozen; cannot register %s", subject)
	}
	return nil
}

func (s *Server) sharedUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = status.Errorf(codes.Internal, "panic in %s: %v", info.FullMethod, recovered)
		}
	}()
	if len(s.interceptors) == 0 {
		return handler(ctx, req)
	}
	current := handler
	for _, interceptor := range slices.Backward(s.interceptors) {
		next := current
		current = func(ctx context.Context, req any) (any, error) {
			return interceptor(ctx, req, info, next)
		}
	}
	return current(ctx, req)
}

func (s *Server) sharedStreamInterceptor(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = status.Errorf(codes.Internal, "panic in %s: %v", info.FullMethod, recovered)
		}
	}()
	if len(s.streamInterceptors) == 0 {
		return handler(srv, stream)
	}
	current := handler
	for _, interceptor := range slices.Backward(s.streamInterceptors) {
		next := current
		current = func(srv any, stream grpc.ServerStream) error {
			return interceptor(srv, stream, info, next)
		}
	}
	return current(srv, stream)
}
