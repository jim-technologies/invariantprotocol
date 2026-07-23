package invariant

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	invpb "github.com/jim-technologies/invariantprotocol/go/gen/invariant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

type registeredService struct {
	desc    *grpc.ServiceDesc
	service any
}

type projectedGRPCService any

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

		var err error
		stagedTools[i], err = s.toolsForService(service.desc, service.service)
		if err != nil {
			return err
		}
		for _, tool := range stagedTools[i] {
			if owner, exists := toolOwners[tool.Name]; exists {
				return fmt.Errorf(
					"tool name collision: %q is registered by both %q and %q; configure Server.Include() before registration to scope to one",
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

func (s *Server) toolsForService(desc *grpc.ServiceDesc, impl any) ([]*Tool, error) {
	if s.parsed == nil {
		return nil, nil
	}
	svcInfo, ok := s.parsed.Services[desc.ServiceName]
	if !ok {
		// Infrastructure services (for example grpc.health.v1.Health) can be
		// registered normally even when they are not part of the projected FDS.
		return nil, nil
	}
	if reflect.TypeOf(desc.HandlerType) != reflect.TypeFor[*projectedGRPCService]() {
		if err := s.validateDescriptorAgreement(desc, svcInfo); err != nil {
			return nil, err
		}
	}

	var tools []*Tool
	for i := range desc.Methods {
		method := &desc.Methods[i]
		methodInfo, ok := svcInfo.Methods[method.MethodName]
		if !ok || methodInfo.ClientStreaming || methodInfo.ServerStreaming ||
			!s.shouldIncludeLocked(desc.ServiceName, method.MethodName) {
			continue
		}
		tools = append(tools, s.registeredTool(svcInfo, methodInfo, method.MethodName, impl, method, nil))
	}
	for i := range desc.Streams {
		method := &desc.Streams[i]
		methodInfo, ok := svcInfo.Methods[method.StreamName]
		if !ok || methodInfo.ClientStreaming || !methodInfo.ServerStreaming ||
			!s.shouldIncludeLocked(desc.ServiceName, method.StreamName) {
			continue
		}
		tools = append(tools, s.registeredTool(svcInfo, methodInfo, method.StreamName, impl, nil, method))
	}
	return tools, nil
}

// validateDescriptorAgreement prevents generated code and descriptor.binpb
// from describing different revisions of the same service. Projection
// metadata comes from the descriptor image while native dispatch comes from
// grpc.ServiceDesc, so silently accepting disagreement would split the
// canonical contract by transport.
func (s *Server) validateDescriptorAgreement(desc *grpc.ServiceDesc, svcInfo *invpb.ServiceInfo) error {
	seen := make(map[string]struct{}, len(desc.Methods)+len(desc.Streams))
	for i := range desc.Methods {
		method := &desc.Methods[i]
		if method.MethodName == "" || method.Handler == nil {
			return fmt.Errorf("invariant: service %q has an invalid unary method descriptor", desc.ServiceName)
		}
		if _, duplicate := seen[method.MethodName]; duplicate {
			return fmt.Errorf("invariant: service %q has duplicate method %q", desc.ServiceName, method.MethodName)
		}
		seen[method.MethodName] = struct{}{}
		methodInfo, ok := svcInfo.Methods[method.MethodName]
		if !ok {
			return fmt.Errorf(
				"invariant: generated service %q method %q is absent from descriptor.binpb",
				desc.ServiceName, method.MethodName,
			)
		}
		if methodInfo.ClientStreaming || methodInfo.ServerStreaming {
			return fmt.Errorf(
				"invariant: generated service %q method %q is unary but descriptor.binpb declares streaming",
				desc.ServiceName, method.MethodName,
			)
		}
	}
	for i := range desc.Streams {
		method := &desc.Streams[i]
		if method.StreamName == "" || method.Handler == nil {
			return fmt.Errorf("invariant: service %q has an invalid stream method descriptor", desc.ServiceName)
		}
		if _, duplicate := seen[method.StreamName]; duplicate {
			return fmt.Errorf("invariant: service %q has duplicate method %q", desc.ServiceName, method.StreamName)
		}
		seen[method.StreamName] = struct{}{}
		methodInfo, ok := svcInfo.Methods[method.StreamName]
		if !ok {
			return fmt.Errorf(
				"invariant: generated service %q method %q is absent from descriptor.binpb",
				desc.ServiceName, method.StreamName,
			)
		}
		if methodInfo.ClientStreaming != method.ClientStreams || methodInfo.ServerStreaming != method.ServerStreams {
			return fmt.Errorf(
				"invariant: generated service %q method %q streaming cardinality disagrees with descriptor.binpb",
				desc.ServiceName, method.StreamName,
			)
		}
	}

	missing := make([]string, 0)
	for name := range svcInfo.Methods {
		if _, ok := seen[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		slices.Sort(missing)
		return fmt.Errorf(
			"invariant: generated service %q is missing descriptor.binpb methods: %s",
			desc.ServiceName, strings.Join(missing, ", "),
		)
	}

	// Generated registration links the protobuf service descriptor into the
	// binary. Require it so input/output identities can be checked against the
	// runtime image instead of trusting only method names and cardinalities.
	linked, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(desc.ServiceName))
	if err != nil {
		return fmt.Errorf(
			"invariant: generated service %q has no linked protobuf descriptor: %w",
			desc.ServiceName, err,
		)
	}
	linkedService, ok := linked.(protoreflect.ServiceDescriptor)
	if !ok {
		return fmt.Errorf("invariant: linked descriptor %q is not a protobuf service", desc.ServiceName)
	}
	for name, methodInfo := range svcInfo.Methods {
		method := linkedService.Methods().ByName(protoreflect.Name(name))
		if method == nil {
			return fmt.Errorf(
				"invariant: linked generated service %q is missing descriptor.binpb method %q",
				desc.ServiceName, name,
			)
		}
		if string(method.Input().FullName()) != methodInfo.InputType ||
			string(method.Output().FullName()) != methodInfo.OutputType {
			return fmt.Errorf(
				"invariant: generated service %q method %q message types disagree with descriptor.binpb",
				desc.ServiceName, name,
			)
		}
	}

	runtimeFiles, err := s.buildProtoFiles()
	if err != nil {
		return err
	}
	runtimeDescriptor, err := runtimeFiles.FindDescriptorByName(protoreflect.FullName(desc.ServiceName))
	if err != nil {
		return fmt.Errorf("invariant: service %q is absent from the runtime descriptor graph: %w", desc.ServiceName, err)
	}
	runtimeService, ok := runtimeDescriptor.(protoreflect.ServiceDescriptor)
	if !ok {
		return fmt.Errorf("invariant: runtime descriptor %q is not a protobuf service", desc.ServiceName)
	}
	linkedFiles := reachableServiceFiles(linkedService)
	runtimeGraph := reachableServiceFiles(runtimeService)
	if mismatch := descriptorGraphMismatch(linkedFiles, runtimeGraph); mismatch != "" {
		return fmt.Errorf(
			"invariant: generated service %q protobuf file %q disagrees with descriptor.binpb",
			desc.ServiceName, mismatch,
		)
	}
	return nil
}

// reachableServiceFiles returns the defining file and every file reached by a
// request/response message or enum field. Compiler-support imports that are not
// part of the service's value graph are intentionally excluded: their bundled
// descriptors can vary across protobuf runtimes without changing the service
// contract.
func reachableServiceFiles(service protoreflect.ServiceDescriptor) map[string]protoreflect.FileDescriptor {
	files := map[string]protoreflect.FileDescriptor{
		service.ParentFile().Path(): service.ParentFile(),
	}
	messages := make(map[protoreflect.FullName]struct{})
	methods := service.Methods()
	for i := range methods.Len() {
		method := methods.Get(i)
		addReachableMessageFiles(files, messages, method.Input())
		addReachableMessageFiles(files, messages, method.Output())
	}
	return files
}

func reachableMessageFiles(message protoreflect.MessageDescriptor) map[string]protoreflect.FileDescriptor {
	files := make(map[string]protoreflect.FileDescriptor)
	addReachableMessageFiles(files, make(map[protoreflect.FullName]struct{}), message)
	return files
}

func addReachableMessageFiles(
	files map[string]protoreflect.FileDescriptor,
	seen map[protoreflect.FullName]struct{},
	message protoreflect.MessageDescriptor,
) {
	if _, exists := seen[message.FullName()]; exists {
		return
	}
	seen[message.FullName()] = struct{}{}
	files[message.ParentFile().Path()] = message.ParentFile()
	fields := message.Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		switch field.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			addReachableMessageFiles(files, seen, field.Message())
		case protoreflect.EnumKind:
			files[field.Enum().ParentFile().Path()] = field.Enum().ParentFile()
		}
	}
}

// descriptorGraphMismatch returns the first mismatched file path, or an empty
// string when the reachable descriptor graphs are equal. SourceCodeInfo is
// intentionally ignored because generated language descriptors omit comments.
func descriptorGraphMismatch(
	generated map[string]protoreflect.FileDescriptor,
	runtime map[string]protoreflect.FileDescriptor,
) string {
	if len(generated) != len(runtime) {
		return "<reachable graph>"
	}
	paths := make([]string, 0, len(generated))
	for path := range generated {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		runtimeFile, exists := runtime[path]
		if !exists {
			return path
		}
		generatedProto := protodesc.ToFileDescriptorProto(generated[path])
		runtimeProto := protodesc.ToFileDescriptorProto(runtimeFile)
		generatedProto.SourceCodeInfo = nil
		runtimeProto.SourceCodeInfo = nil
		if !proto.Equal(generatedProto, runtimeProto) {
			return path
		}
	}
	return ""
}

func (s *Server) registeredTool(
	svcInfo *invpb.ServiceInfo,
	methodInfo *invpb.MethodInfo,
	methodName string,
	impl any,
	unaryDesc *grpc.MethodDesc,
	streamDesc *grpc.StreamDesc,
) *Tool {
	toolName := svcInfo.FullName + "." + methodName
	description := methodInfo.Comment
	if description == "" {
		description = toolName
	}
	return &Tool{
		Name:            toolName,
		Description:     description,
		InputSchema:     s.schemaGen.MessageToSchema(methodInfo.InputType),
		InputType:       methodInfo.InputType,
		OutputType:      methodInfo.OutputType,
		ServiceFullName: svcInfo.FullName,
		MethodName:      methodName,
		ServerStreaming: methodInfo.ServerStreaming,
		serviceImpl:     impl,
		unaryDesc:       unaryDesc,
		streamDesc:      streamDesc,
		newRequest:      s.messageFactory(methodInfo.InputType),
	}
}

func (s *Server) messageFactory(fullName string) func() proto.Message {
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
	factory, err := s.checkedMessageFactory(md)
	if err != nil {
		return nil
	}
	return factory
}

// checkedMessageFactory keeps descriptor-only proxies dynamic, but reuses a
// linked generated type when its complete reachable schema matches the runtime
// image. A same-name stale generated type is rejected instead of silently
// splitting JSON/schema interpretation from protobuf dispatch.
func (s *Server) checkedMessageFactory(descriptor protoreflect.MessageDescriptor) (func() proto.Message, error) {
	messageType, err := protoregistry.GlobalTypes.FindMessageByName(descriptor.FullName())
	if errors.Is(err, protoregistry.NotFound) {
		return dynamicMessageFactory(descriptor), nil
	}
	if err != nil {
		return nil, fmt.Errorf("invariant: resolve linked generated message %q: %w", descriptor.FullName(), err)
	}
	if mismatch := descriptorGraphMismatch(
		reachableMessageFiles(messageType.Descriptor()),
		reachableMessageFiles(descriptor),
	); mismatch != "" {
		return nil, fmt.Errorf(
			"invariant: linked generated message %q protobuf file %q disagrees with descriptor.binpb",
			descriptor.FullName(), mismatch,
		)
	}
	return func() proto.Message { return messageType.New().Interface() }, nil
}

func copyProtoMessage(dst, src any) error {
	dstMsg, ok := dst.(proto.Message)
	if !ok {
		return fmt.Errorf("decode target does not implement proto.Message: %T", dst)
	}
	srcMsg, ok := src.(proto.Message)
	if !ok {
		return fmt.Errorf("request does not implement proto.Message: %T", src)
	}
	dstDescriptor := dstMsg.ProtoReflect().Descriptor()
	srcDescriptor := srcMsg.ProtoReflect().Descriptor()
	if dstDescriptor.FullName() != srcDescriptor.FullName() {
		return status.Errorf(
			codes.InvalidArgument,
			"request message type %q does not match expected %q",
			srcDescriptor.FullName(),
			dstDescriptor.FullName(),
		)
	}
	proto.Reset(dstMsg)
	if reflect.TypeOf(dstMsg) == reflect.TypeOf(srcMsg) &&
		dstDescriptor == srcDescriptor {
		proto.Merge(dstMsg, srcMsg)
		return nil
	}
	data, err := proto.Marshal(srcMsg)
	if err != nil {
		return err
	}
	return proto.Unmarshal(data, dstMsg)
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
