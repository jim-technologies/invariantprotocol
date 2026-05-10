package invariant

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"

	invpb "github.com/jim-technologies/invariantprotocol/go/gen/invariant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// --- Interceptor types (mirrors gRPC pattern, zero coupling to grpc package) ---

// ServerCallInfo holds metadata about the RPC being invoked, passed to interceptors.
type ServerCallInfo struct {
	FullMethod string // e.g. "/greet.v1.GreetService/Greet"
}

// UnaryHandler is the handler function called at the end of the interceptor chain.
type UnaryHandler func(ctx context.Context, req any) (any, error)

// UnaryServerInterceptor intercepts unary RPCs across all projections (MCP, HTTP,
// gRPC, CLI). Same signature as grpc.UnaryServerInterceptor but framework-native.
type UnaryServerInterceptor func(ctx context.Context, req any, info *ServerCallInfo, handler UnaryHandler) (any, error)

// OutboundHTTPRequest describes an HTTP request that will be sent by ConnectHTTP.
// It is passed to HTTPHeaderProvider so callers can compute dynamic auth headers.
type OutboundHTTPRequest struct {
	MethodPath string // e.g. "/greet.v1.GreetService/Greet"
	Method     string // e.g. "GET", "POST"
	URL        string // fully expanded URL with query string
	Body       []byte // JSON body bytes (may be empty)
}

// HTTPHeaderProvider returns extra outbound HTTP headers for ConnectHTTP requests.
// Typical use: API signatures, short-lived tokens, per-request timestamps.
type HTTPHeaderProvider func(ctx context.Context, req *OutboundHTTPRequest) (map[string]string, error)

// Tool represents a single registered RPC method projected as a tool.
type Tool struct {
	Name            string
	Description     string
	InputSchema     map[string]any
	Handler         any
	InputType       string
	OutputType      string
	ServiceFullName string
	MethodName      string

	// Cached at addTool time so the hot path doesn't reflect on every call.
	invokeHandler UnaryHandler
	callInfo      *ServerCallInfo
	newRequest    func() proto.Message
}

const (
	serverName    = "invariant-protocol"
	serverVersion = "0.1.0"
)

// Server holds parsed descriptors and registered tools.
type Server struct {
	parsed             *invpb.ParsedDescriptor
	schemaGen          *schemaGenerator
	tools              map[string]*Tool
	fds                *descriptorpb.FileDescriptorSet // original FDS for dynamic message creation
	interceptors       []UnaryServerInterceptor
	httpHeaderProvider HTTPHeaderProvider
	includes           []string // glob patterns for methods to include
	excludes           []string // glob patterns for methods to exclude
}

// Use registers an interceptor. Interceptors run in registration order
// (first registered = outermost) on every tool invocation across all projections.
func (s *Server) Use(interceptor UnaryServerInterceptor) {
	s.interceptors = append(s.interceptors, interceptor)
}

// UseHTTPHeaderProvider sets an optional outbound header provider for ConnectHTTP.
// The provider is called for every outbound HTTP request.
func (s *Server) UseHTTPHeaderProvider(provider HTTPHeaderProvider) {
	s.httpHeaderProvider = provider
}

// Include adds glob patterns for methods to include. Only methods matching at
// least one include pattern are registered. Patterns are matched against the
// fully qualified method path: "service.full.Name.MethodName".
// Use "*" to match any sequence of characters (including dots).
// Examples: "temporal.api.workflowservice.v1.WorkflowService.*", "*.StartWorkflow*".
func (s *Server) Include(patterns ...string) {
	s.includes = append(s.includes, patterns...)
}

// Exclude adds glob patterns for methods to exclude. Methods matching any
// exclude pattern are skipped during registration. Exclude is applied after
// include. Patterns use the same syntax as Include.
func (s *Server) Exclude(patterns ...string) {
	s.excludes = append(s.excludes, patterns...)
}

// shouldInclude returns true if the method should be registered based on
// the configured include/exclude patterns and INVARIANT_INCLUDE/INVARIANT_EXCLUDE
// environment variables.
func (s *Server) shouldInclude(serviceFullName, methodName string) bool {
	fullPath := serviceFullName + "." + methodName

	includes := s.includes
	if env := os.Getenv("INVARIANT_INCLUDE"); env != "" {
		includes = append(includes, splitPatterns(env)...)
	}

	excludes := s.excludes
	if env := os.Getenv("INVARIANT_EXCLUDE"); env != "" {
		excludes = append(excludes, splitPatterns(env)...)
	}

	if len(includes) > 0 {
		matched := false
		for _, pattern := range includes {
			if globMatch(pattern, fullPath) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	for _, pattern := range excludes {
		if globMatch(pattern, fullPath) {
			return false
		}
	}

	return true
}

// globMatch matches a pattern against a string where "*" matches any sequence
// of characters (including dots). This is simpler than filepath.Match which
// treats "/" specially.
func globMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			pattern = pattern[1:]
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(pattern, s[i:]) {
					return true
				}
			}
			return false
		default:
			if len(s) == 0 || pattern[0] != s[0] {
				return false
			}
			pattern = pattern[1:]
			s = s[1:]
		}
	}
	return len(s) == 0
}

func splitPatterns(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func newServer(parsed *invpb.ParsedDescriptor) *Server {
	return &Server{
		parsed:    parsed,
		schemaGen: newSchemaGenerator(parsed),
		tools:     make(map[string]*Tool),
	}
}

// ServerFromDescriptor reads a descriptor file and returns a configured Server.
func ServerFromDescriptor(path string) (*Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return serverFromRawBytes(data)
}

// ServerFromBytes parses an embedded FileDescriptorSet and returns a configured Server.
func ServerFromBytes(data []byte) (*Server, error) {
	return serverFromRawBytes(data)
}

func serverFromRawBytes(data []byte) (*Server, error) {
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &fds); err != nil {
		return nil, fmt.Errorf("unmarshal FileDescriptorSet: %w", err)
	}
	parsed := parseFileDescriptorSet(&fds)
	srv := newServer(parsed)
	srv.fds = &fds
	return srv, nil
}

// Register discovers methods on servicer that match the service's RPCs and
// creates tools for each unary (non-streaming) method.
// If serviceName is empty, auto-matches by finding services whose RPC method
// names exist on the servicer.
func (s *Server) Register(servicer any, serviceName ...string) error {
	var services map[string]*invpb.ServiceInfo

	if len(serviceName) > 0 && serviceName[0] != "" {
		name := serviceName[0]
		svcInfo, ok := s.parsed.Services[name]
		if !ok {
			var available []string
			for k := range s.parsed.Services {
				available = append(available, k)
			}
			return fmt.Errorf("service %q not found in descriptor. Available: %v", name, available)
		}
		services = map[string]*invpb.ServiceInfo{name: svcInfo}
	} else {
		services = s.matchServicer(servicer)
		if len(services) == 0 {
			var available []string
			for k := range s.parsed.Services {
				available = append(available, k)
			}
			return fmt.Errorf("no matching service found for servicer. Available: %v", available)
		}
	}

	servicerVal := reflect.ValueOf(servicer)

	for svcFullName, svcInfo := range services {
		for methodName, methodInfo := range svcInfo.Methods {
			if methodInfo.ClientStreaming || methodInfo.ServerStreaming {
				continue
			}

			if !s.shouldInclude(svcFullName, methodName) {
				continue
			}

			method := servicerVal.MethodByName(methodName)
			if !method.IsValid() {
				continue
			}

			toolName := svcInfo.Name + "." + methodName
			description := methodInfo.Comment
			if description == "" {
				description = toolName
			}

			if err := s.addTool(&Tool{
				Name:            toolName,
				Description:     description,
				InputSchema:     s.schemaGen.MessageToSchema(methodInfo.InputType),
				Handler:         method.Interface(),
				InputType:       methodInfo.InputType,
				OutputType:      methodInfo.OutputType,
				ServiceFullName: svcFullName,
				MethodName:      methodName,
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

// addTool registers a Tool, rejecting collisions and pre-building the per-Tool
// invocation closure so invoke() doesn't reflect on every call.
//
// Build errors are deferred to invoke time — registration accepts the tool so
// metadata-only tests don't have to use real handlers.
func (s *Server) addTool(t *Tool) error {
	if existing, ok := s.tools[t.Name]; ok && existing.ServiceFullName != t.ServiceFullName {
		return fmt.Errorf(
			"tool name collision: %q is registered by both %q and %q. "+
				"Two services in different packages share the same simple name; "+
				"use Server.Include() to scope to one",
			t.Name, existing.ServiceFullName, t.ServiceFullName,
		)
	}
	if handler, err := buildInvokeHandler(t.Handler); err == nil {
		t.invokeHandler = handler
	} else {
		buildErr := err
		t.invokeHandler = func(context.Context, any) (any, error) { return nil, buildErr }
	}
	t.newRequest = buildRequestFactory(t.Handler)
	t.callInfo = &ServerCallInfo{FullMethod: "/" + t.ServiceFullName + "/" + t.MethodName}
	s.tools[t.Name] = t
	return nil
}

// buildRequestFactory returns a closure that produces an empty proto.Message
// of the handler's request type. Reflection runs once at registration.
func buildRequestFactory(handler any) func() proto.Message {
	if provider, ok := handler.(interface {
		requestDescriptor() protoreflect.MessageDescriptor
	}); ok {
		desc := provider.requestDescriptor()
		return func() proto.Message { return dynamicpb.NewMessage(desc) }
	}
	hv := reflect.ValueOf(handler)
	if hv.Kind() != reflect.Func {
		return nil
	}
	ht := hv.Type()
	if ht.NumIn() != 2 {
		return nil
	}
	reqType := ht.In(1)
	if reqType.Kind() != reflect.Ptr {
		return nil
	}
	elem := reqType.Elem()
	// Verify the pointer-to type implements proto.Message.
	if _, ok := reflect.New(elem).Interface().(proto.Message); !ok {
		return nil
	}
	return func() proto.Message { return reflect.New(elem).Interface().(proto.Message) }
}

// buildInvokeHandler returns the proto-in/proto-out closure for a tool's handler.
// Reflection happens once at registration time, not on each request.
func buildInvokeHandler(handler any) (UnaryHandler, error) {
	switch h := handler.(type) {
	case *grpcDynamicHandler:
		return func(ctx context.Context, req any) (any, error) {
			return h.callProto(ctx, req.(proto.Message))
		}, nil
	case *httpDynamicHandler:
		return func(ctx context.Context, req any) (any, error) {
			return h.callProto(ctx, req.(proto.Message))
		}, nil
	}

	// Local servicer — bind via reflection once.
	handlerVal := reflect.ValueOf(handler)
	handlerType := handlerVal.Type()
	if handlerType.Kind() != reflect.Func || handlerType.NumIn() != 2 || handlerType.NumOut() != 2 {
		return nil, fmt.Errorf("handler has unexpected signature (expected func(ctx, *Req) (*Resp, error))")
	}

	reqType := handlerType.In(1)
	// Snapshot the typed request's proto FullName so the binary fast-path on
	// dynamicpb inputs can decide between binary roundtrip and JSON fallback.
	handlerReqMsg, ok := reflect.New(reqType.Elem()).Interface().(proto.Message)
	if !ok {
		return nil, fmt.Errorf("handler request type %s does not implement proto.Message", reqType)
	}
	handlerFullName := handlerReqMsg.ProtoReflect().Descriptor().FullName()

	return func(ctx context.Context, r any) (any, error) {
		rMsg := r.(proto.Message)

		// dynamicpb inputs (gRPC, binary HTTP proxy) need conversion to the
		// handler's typed proto. Same-name → fast binary roundtrip; otherwise
		// fall through to JSON for cross-type conversion (e.g. structpb.Struct).
		if dynMsg, isDynamic := rMsg.(*dynamicpb.Message); isDynamic {
			typed := reflect.New(reqType.Elem()).Interface().(proto.Message)
			if dynMsg.ProtoReflect().Descriptor().FullName() == handlerFullName {
				b, err := proto.Marshal(rMsg)
				if err != nil {
					return nil, fmt.Errorf("marshal dynamic to binary: %w", err)
				}
				if err := proto.Unmarshal(b, typed); err != nil {
					return nil, fmt.Errorf("unmarshal binary to typed: %w", err)
				}
			} else {
				b, err := protojson.Marshal(rMsg)
				if err != nil {
					return nil, fmt.Errorf("marshal dynamic to JSON: %w", err)
				}
				if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, typed); err != nil {
					return nil, fmt.Errorf("unmarshal JSON to typed: %w", err)
				}
			}
			rMsg = typed
		}

		results := handlerVal.Call([]reflect.Value{
			reflect.ValueOf(ctx),
			reflect.ValueOf(rMsg),
		})
		if !results[1].IsNil() {
			return nil, results[1].Interface().(error)
		}
		return results[0].Interface(), nil
	}, nil
}

// matchServicer finds services whose RPC names match methods on the servicer.
func (s *Server) matchServicer(servicer any) map[string]*invpb.ServiceInfo {
	servicerVal := reflect.ValueOf(servicer)
	matched := make(map[string]*invpb.ServiceInfo)
	for svcFullName, svcInfo := range s.parsed.Services {
		for methodName, methodInfo := range svcInfo.Methods {
			if methodInfo.ClientStreaming || methodInfo.ServerStreaming {
				continue
			}
			if servicerVal.MethodByName(methodName).IsValid() {
				matched[svcFullName] = svcInfo
				break
			}
		}
	}
	return matched
}

// Connect registers a gRPC client connection's methods as tools.
// The caller creates and manages the *grpc.ClientConn; Connect only reads from it.
// Use Include/Exclude to filter which services and methods are registered.
func (s *Server) Connect(conn *grpc.ClientConn) error {
	if s.fds == nil {
		return errors.New("connect requires a Server created via ServerFromDescriptor or ServerFromBytes")
	}

	files, err := s.buildProtoFiles()
	if err != nil {
		return err
	}

	services := s.parsed.Services

	for svcFullName, svcInfo := range services {
		for methodName, methodInfo := range svcInfo.Methods {
			if methodInfo.ClientStreaming || methodInfo.ServerStreaming {
				continue
			}

			if !s.shouldInclude(svcFullName, methodName) {
				continue
			}

			reqDesc, err := findMessageDescriptor(files, methodInfo.InputType)
			if err != nil {
				return err
			}
			respDesc, err := findMessageDescriptor(files, methodInfo.OutputType)
			if err != nil {
				return err
			}

			methodPath := fmt.Sprintf("/%s/%s", svcFullName, methodName)
			toolName := svcInfo.Name + "." + methodName
			description := methodInfo.Comment
			if description == "" {
				description = toolName
			}

			if err := s.addTool(&Tool{
				Name:            toolName,
				Description:     description,
				InputSchema:     s.schemaGen.MessageToSchema(methodInfo.InputType),
				Handler:         &grpcDynamicHandler{conn: conn, methodPath: methodPath, reqDesc: reqDesc, respDesc: respDesc},
				InputType:       methodInfo.InputType,
				OutputType:      methodInfo.OutputType,
				ServiceFullName: svcFullName,
				MethodName:      methodName,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Projection specifies a protocol to serve.
type Projection struct {
	kind     string
	port     int
	grpcOpts []grpc.ServerOption
}

// HTTP returns a projection that serves HTTP on the given port.
func HTTP(port int) Projection { return Projection{kind: "http", port: port} }

// GRPC returns a projection that serves gRPC on the given port.
// Optional grpc.ServerOption values (TLS, keepalive, etc.) are passed to grpc.NewServer.
func GRPC(port int, opts ...grpc.ServerOption) Projection {
	return Projection{kind: "grpc", port: port, grpcOpts: opts}
}

// MCP returns a projection that serves MCP over stdio.
func MCP() Projection { return Projection{kind: "mcp"} }

// CLI returns a projection that runs as a CLI from os.Args.
func CLI() Projection { return Projection{kind: "cli"} }

// Serve starts the specified projections and blocks until ctx is cancelled
// or the first projection returns an error.
//
//	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
//	defer cancel()
//	server.Serve(ctx, invariant.HTTP(8080))
//	server.Serve(ctx, invariant.HTTP(8080), invariant.GRPC(50051))
//
// On error or cancellation, all projections receive a graceful shutdown signal.
func (s *Server) Serve(ctx context.Context, projections ...Projection) error {
	if len(projections) == 0 {
		return errors.New("no projections specified")
	}
	if len(projections) == 1 {
		return s.serveOne(ctx, projections[0])
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(projections))
	for _, p := range projections {
		go func() { errc <- s.serveOne(ctx, p) }()
	}
	first := <-errc
	cancel()
	for i := 1; i < len(projections); i++ {
		<-errc
	}
	return first
}

func (s *Server) serveOne(ctx context.Context, p Projection) error {
	switch p.kind {
	case "mcp":
		return s.serveMCP(ctx)
	case "cli":
		return s.serveCLI(ctx)
	case "http":
		return s.serveHTTP(ctx, p.port)
	case "grpc":
		return s.serveGRPC(ctx, p.port, p.grpcOpts...)
	default:
		return fmt.Errorf("unknown projection: %s", p.kind)
	}
}

// Invoke dispatches a request to a registered tool by name. Useful for
// in-process callers (workflow runtimes, tests) that don't need to spin up
// a projection.
//
// Returns a NOT_FOUND status error if the tool is not registered, so the
// error projects to the right code through every projection.
func (s *Server) Invoke(ctx context.Context, toolName string, req proto.Message) (proto.Message, error) {
	tool, ok := s.tools[toolName]
	if !ok {
		var available []string
		for name := range s.tools {
			available = append(available, name)
		}
		return nil, status.Errorf(codes.NotFound, "unknown tool %q. Available: %v", toolName, available)
	}
	return s.invoke(ctx, tool, req)
}

// Tools returns a snapshot of the registered tool names to their Tool metadata.
func (s *Server) Tools() map[string]*Tool {
	out := make(map[string]*Tool, len(s.tools))
	maps.Copy(out, s.tools)
	return out
}

// ToolCatalog returns the canonical tool catalog (same shape as MCP `tools/list`).
// Used by both the HTTP `GET /` endpoint and MCP's `tools/list`.
func (s *Server) ToolCatalog() []map[string]any {
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	slices.Sort(names)

	out := make([]map[string]any, 0, len(s.tools))
	for _, name := range names {
		t := s.tools[name]
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return out
}

func (s *Server) buildProtoFiles() (*protoregistry.Files, error) {
	files, err := protodesc.NewFiles(s.fds)
	if err != nil {
		return nil, fmt.Errorf("build file descriptors: %w", err)
	}
	return files, nil
}
