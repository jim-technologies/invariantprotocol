package invariant

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

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

// These aliases keep the pre-1.0 names source-compatible while making grpc-go's
// middleware types the one interceptor model used by every projection.
type (
	// ServerCallInfo is the compatibility name for grpc.UnaryServerInfo.
	//
	// Deprecated: use grpc.UnaryServerInfo.
	ServerCallInfo = grpc.UnaryServerInfo
	// UnaryHandler is the compatibility name for grpc.UnaryHandler.
	//
	// Deprecated: use grpc.UnaryHandler.
	UnaryHandler = grpc.UnaryHandler
	// UnaryServerInterceptor is the compatibility name for grpc.UnaryServerInterceptor.
	//
	// Deprecated: use grpc.UnaryServerInterceptor.
	UnaryServerInterceptor = grpc.UnaryServerInterceptor
	// StreamHandler is the compatibility name for grpc.StreamHandler.
	//
	// Deprecated: use grpc.StreamHandler.
	StreamHandler = grpc.StreamHandler
	// StreamServerInterceptor is the compatibility name for grpc.StreamServerInterceptor.
	//
	// Deprecated: use grpc.StreamServerInterceptor.
	StreamServerInterceptor = grpc.StreamServerInterceptor
)

// ServerStream is retained only for the reflection-based Register compatibility
// API.
//
// Deprecated: implement the generated grpc.ServerStreamingServer method.
type ServerStream interface {
	Send(msg proto.Message) error
	Context() context.Context
}

type legacyStreamHandler func(req any, stream ServerStream) error

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
	ServerStreaming bool

	// Cached at addTool time so the hot path doesn't reflect on every call.
	invokeHandler UnaryHandler        // non-nil when !ServerStreaming
	streamHandler legacyStreamHandler // compatibility Register stream handler
	streamDesc    *grpc.StreamDesc
	serviceImpl   any
	callInfo      *grpc.UnaryServerInfo
	streamInfo    *grpc.StreamServerInfo
	newRequest    func() proto.Message
}

const (
	serverName    = "invariant-protocol"
	serverVersion = "0.6.1"
)

// MethodConfig overrides per-server defaults for one RPC method. Zero-valued
// fields fall back to the server-level setting. Apply via
// `Server.ConfigureMethod("/pkg.Service/Method", MethodConfig{...})`.
//
// Request and response limits are independent because protobuf and JSON wire
// sizes are not interchangeable.
type MethodConfig struct {
	// MaxUnaryRequestBytes overrides the per-server unary cap. Zero =
	// inherit. Use a positive value for methods that legitimately need
	// large bodies (e.g. an object-store Upload accepting hundreds of
	// MiB) while keeping the rest of the surface tight.
	MaxUnaryRequestBytes int64
	// MaxUnaryResponseBytes overrides the encoded unary HTTP response cap.
	// The selected wire encoding (JSON or protobuf) is measured.
	MaxUnaryResponseBytes int64
	// MaxStreamRequestBytes overrides the per-server Connect-streaming
	// request envelope cap. Zero = inherit.
	MaxStreamRequestBytes int64
	// MaxStreamResponseBytes overrides the encoded Connect response-message
	// cap. It applies independently to each message, like gRPC limits.
	MaxStreamResponseBytes int64
}

// Server holds parsed descriptors and registered tools.
type Server struct {
	parsed             *invpb.ParsedDescriptor
	schemaGen          *schemaGenerator
	tools              map[string]*Tool
	fds                *descriptorpb.FileDescriptorSet // original FDS for dynamic message creation
	interceptors       []grpc.UnaryServerInterceptor
	streamInterceptors []grpc.StreamServerInterceptor
	httpHeaderProvider HTTPHeaderProvider
	httpMetadataMapper HTTPMetadataMapper
	includes           []string // glob patterns for methods to include
	excludes           []string // glob patterns for methods to exclude

	// Body-size safety caps for the HTTP projection. Defaults are tight;
	// raise per-server when the application has a legitimate need (e.g. an
	// object store accepting multi-hundred-megabyte uploads). Or raise
	// only for specific methods via ConfigureMethod — preferred when one
	// RPC is the outlier rather than the whole service.
	httpMaxUnaryRequest      int64
	httpMaxUnaryResponse     int64
	connectStreamMaxRequest  int64
	connectStreamMaxResponse int64

	// methodConfigs is the per-method override table. Keys are
	// `/pkg.Service/Method` paths (matching the Connect URL space and
	// the same identity Register/Connect use). Populated by
	// ConfigureMethod; read by the HTTP handlers before applying the
	// server-level cap.
	methodConfigs map[string]MethodConfig

	mu                 sync.RWMutex
	frozen             bool
	frozenFast         atomic.Bool
	grpcServer         *grpc.Server
	registeredServices map[string]registeredService
}

func (s *Server) updateConfiguration(subject string, update func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen {
		panic(fmt.Sprintf("invariant: %s cannot be changed after serving begins", subject))
	}
	update()
}

// SetMaxUnaryRequestBytes overrides the HTTP unary body-size cap. Pass 0 to
// reset to the default (16 MiB).
func (s *Server) SetMaxUnaryRequestBytes(n int64) {
	if n <= 0 {
		n = defaultHTTPMaxUnaryRequest
	}
	s.updateConfiguration("HTTP unary request limit", func() { s.httpMaxUnaryRequest = n })
}

// SetMaxUnaryResponseBytes overrides the encoded HTTP unary response cap.
// Pass 0 to reset to the default (16 MiB).
func (s *Server) SetMaxUnaryResponseBytes(n int64) {
	if n <= 0 {
		n = defaultHTTPMaxUnaryResponse
	}
	s.updateConfiguration("HTTP unary response limit", func() { s.httpMaxUnaryResponse = n })
}

// SetMaxStreamRequestBytes overrides the Connect streaming request envelope
// cap. Pass 0 to reset to the default (16 MiB).
func (s *Server) SetMaxStreamRequestBytes(n int64) {
	if n <= 0 {
		n = defaultConnectStreamMaxRequest
	}
	s.updateConfiguration("HTTP stream request limit", func() { s.connectStreamMaxRequest = n })
}

// SetMaxStreamResponseBytes overrides the per-message encoded Connect stream
// response cap. Pass 0 to reset to the default (16 MiB).
func (s *Server) SetMaxStreamResponseBytes(n int64) {
	if n <= 0 {
		n = defaultConnectStreamMaxResponse
	}
	s.updateConfiguration("HTTP stream response limit", func() { s.connectStreamMaxResponse = n })
}

// ConfigureMethod registers a per-method override. The method path is the
// Connect/gRPC URL form — `/pkg.Service/Method`. Same identity Register
// and Connect use, so callers can copy-paste from their proto schema.
// Zero-valued fields in `cfg` inherit from the server-level setting;
// non-zero fields override.
//
// Typical use: a service has one big RPC (Upload, BulkImport) plus lots
// of small ones; set the server cap tight and raise just the outlier.
//
//	srv := invariant.ServerFromBytes(desc)
//	srv.SetMaxUnaryRequestBytes(16 * 1024 * 1024)
//	srv.ConfigureMethod("/files.v1.FileService/Upload", invariant.MethodConfig{
//	    MaxUnaryRequestBytes: 1 << 30, // 1 GiB
//	})
//
// Last write wins; re-calling overrides the previous config for that method.
func (s *Server) ConfigureMethod(methodPath string, cfg MethodConfig) {
	s.updateConfiguration("method configuration", func() {
		if s.methodConfigs == nil {
			s.methodConfigs = make(map[string]MethodConfig)
		}
		s.methodConfigs[methodPath] = cfg
	})
}

// methodUnaryCap returns the effective unary cap for one tool. Looks up
// the per-method override table; falls back to the server-level cap.
func (s *Server) methodUnaryCap(t *Tool) int64 {
	if t != nil && s.methodConfigs != nil {
		path := "/" + t.ServiceFullName + "/" + t.MethodName
		if cfg, ok := s.methodConfigs[path]; ok && cfg.MaxUnaryRequestBytes > 0 {
			return cfg.MaxUnaryRequestBytes
		}
	}
	return s.httpMaxUnaryRequest
}

func (s *Server) methodUnaryResponseCap(t *Tool) int64 {
	if t != nil && s.methodConfigs != nil {
		path := "/" + t.ServiceFullName + "/" + t.MethodName
		if cfg, ok := s.methodConfigs[path]; ok && cfg.MaxUnaryResponseBytes > 0 {
			return cfg.MaxUnaryResponseBytes
		}
	}
	return s.httpMaxUnaryResponse
}

// methodStreamCap is the streaming-request counterpart to methodUnaryCap.
func (s *Server) methodStreamCap(t *Tool) int64 {
	if t != nil && s.methodConfigs != nil {
		path := "/" + t.ServiceFullName + "/" + t.MethodName
		if cfg, ok := s.methodConfigs[path]; ok && cfg.MaxStreamRequestBytes > 0 {
			return cfg.MaxStreamRequestBytes
		}
	}
	return s.connectStreamMaxRequest
}

func (s *Server) methodStreamResponseCap(t *Tool) int64 {
	if t != nil && s.methodConfigs != nil {
		path := "/" + t.ServiceFullName + "/" + t.MethodName
		if cfg, ok := s.methodConfigs[path]; ok && cfg.MaxStreamResponseBytes > 0 {
			return cfg.MaxStreamResponseBytes
		}
	}
	return s.connectStreamMaxResponse
}

// Use registers a unary interceptor. Runs in registration order (first registered
// = outermost) on every unary tool invocation across all projections. Stream RPCs
// are not affected — use UseStream for those.
func (s *Server) Use(interceptor grpc.UnaryServerInterceptor) {
	s.updateConfiguration("shared unary interceptors", func() {
		s.interceptors = append(s.interceptors, interceptor)
	})
}

// UseStream registers a server-streaming interceptor. Mirrors Use but for
// server-streaming RPCs. Same registration-order semantics.
func (s *Server) UseStream(interceptor grpc.StreamServerInterceptor) {
	s.updateConfiguration("shared stream interceptors", func() {
		s.streamInterceptors = append(s.streamInterceptors, interceptor)
	})
}

// UseHTTPHeaderProvider sets an optional outbound header provider for ConnectHTTP.
// The current provider is called for every outbound HTTP request. Like other
// configuration, it may be changed until serving or invocation begins.
func (s *Server) UseHTTPHeaderProvider(provider HTTPHeaderProvider) {
	s.updateConfiguration("HTTP header provider", func() { s.httpHeaderProvider = provider })
}

// Include adds glob patterns for methods to include. Only methods matching at
// least one include pattern are registered. Patterns are matched against the
// fully qualified method path: "service.full.Name.MethodName".
// Use "*" to match any sequence of characters (including dots).
// Examples: "temporal.api.workflowservice.v1.WorkflowService.*", "*.StartWorkflow*".
func (s *Server) Include(patterns ...string) {
	s.updateConfiguration("include filters", func() { s.includes = append(s.includes, patterns...) })
}

// Exclude adds glob patterns for methods to exclude. Methods matching any
// exclude pattern are skipped during projection registration. Exclude is applied after
// include. Patterns use the same syntax as Include.
func (s *Server) Exclude(patterns ...string) {
	s.updateConfiguration("exclude filters", func() { s.excludes = append(s.excludes, patterns...) })
}

func (s *Server) shouldIncludeLocked(serviceFullName, methodName string) bool {
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
	return newServerWithFDS(parsed, nil)
}

func newServerWithFDS(parsed *invpb.ParsedDescriptor, fds *descriptorpb.FileDescriptorSet, grpcOptions ...grpc.ServerOption) *Server {
	s := &Server{
		parsed:                   parsed,
		schemaGen:                newSchemaGenerator(parsed),
		tools:                    make(map[string]*Tool),
		httpMaxUnaryRequest:      defaultHTTPMaxUnaryRequest,
		httpMaxUnaryResponse:     defaultHTTPMaxUnaryResponse,
		connectStreamMaxRequest:  defaultConnectStreamMaxRequest,
		connectStreamMaxResponse: defaultConnectStreamMaxResponse,
		registeredServices:       make(map[string]registeredService),
		httpMetadataMapper:       DefaultHTTPMetadataMapper,
		fds:                      fds,
	}
	s.grpcServer = s.newNativeGRPCServer(grpcOptions...)
	return s
}

// ServerFromDescriptor reads a descriptor file and returns a configured Server.
func ServerFromDescriptor(path string, grpcOptions ...grpc.ServerOption) (*Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return serverFromRawBytes(data, grpcOptions...)
}

// ServerFromBytes parses an embedded FileDescriptorSet and returns a configured Server.
func ServerFromBytes(data []byte, grpcOptions ...grpc.ServerOption) (*Server, error) {
	return serverFromRawBytes(data, grpcOptions...)
}

func serverFromRawBytes(data []byte, grpcOptions ...grpc.ServerOption) (*Server, error) {
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &fds); err != nil {
		return nil, fmt.Errorf("unmarshal FileDescriptorSet: %w", err)
	}
	parsed := parseFileDescriptorSet(&fds)
	return newServerWithFDS(parsed, &fds, grpcOptions...), nil
}

// Register discovers methods on servicer that match the service's RPCs and
// creates tools for each unary (non-streaming) method.
// If serviceName is empty, auto-matches by finding services whose RPC method
// names exist on the servicer.
//
// Deprecated: implement the generated server interface and call its
// Register<Service>Server function with this Server.
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

	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	slices.Sort(names)
	registrations := make([]registeredService, 0, len(names))
	for _, svcFullName := range names {
		svcInfo := services[svcFullName]
		desc, err := s.legacyServiceDesc(servicer, svcFullName, svcInfo)
		if err != nil {
			return err
		}
		registrations = append(registrations, registeredService{desc: desc, service: servicer})
	}
	subject := "reflection services"
	if len(registrations) == 1 {
		subject = fmt.Sprintf("%q", registrations[0].desc.ServiceName)
	}
	return s.registerServices(registrations, subject)
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
	if registered, ok := t.Handler.(*registeredStreamHandler); ok {
		t.streamDesc = registered.method
		t.serviceImpl = registered.service
	} else if t.ServerStreaming {
		if handler, err := buildStreamHandler(t.Handler); err == nil {
			t.streamHandler = handler
		} else {
			buildErr := err
			t.streamHandler = func(any, ServerStream) error { return buildErr }
		}
	} else if registered, ok := t.Handler.(*registeredUnaryHandler); ok {
		t.serviceImpl = registered.service
		t.invokeHandler = func(ctx context.Context, req any) (any, error) {
			return registered.method.Handler(
				registered.service,
				ctx,
				func(dst any) error { return copyProtoMessage(dst, req) },
				s.sharedUnaryInterceptor,
			)
		}
	} else {
		if handler, err := buildInvokeHandler(t.Handler); err == nil {
			t.invokeHandler = func(ctx context.Context, req any) (any, error) {
				return s.sharedUnaryInterceptor(ctx, req, t.callInfo, handler)
			}
		} else {
			buildErr := err
			t.invokeHandler = func(context.Context, any) (any, error) { return nil, buildErr }
		}
	}
	if t.newRequest == nil {
		t.newRequest = s.messageFactory(t.InputType)
	}
	if t.newRequest == nil {
		t.newRequest = buildRequestFactory(t.Handler)
	}
	fullMethod := "/" + t.ServiceFullName + "/" + t.MethodName
	server := t.serviceImpl
	if server == nil {
		server = s
	}
	t.callInfo = &grpc.UnaryServerInfo{Server: server, FullMethod: fullMethod}
	t.streamInfo = &grpc.StreamServerInfo{FullMethod: fullMethod, IsServerStream: true}
	s.tools[t.Name] = t
	return nil
}

// buildRequestFactory returns a closure that produces an empty proto.Message
// of the handler's request type. Reflection runs once at registration.
//
// Handles three handler shapes:
//   - unary local servicer:   func(ctx, *Req) (*Resp, error)  → req is In(1)
//   - server-streaming:       func(*Req, ServerStream) error  → req is In(0)
//   - proxy handler:          satisfies requestDescriptor()
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
	// Stream signature: func(*Req, ServerStream) error → req is In(0).
	// Unary signature:  func(ctx, *Req) (*Resp, error)  → req is In(1).
	reqIdx := 1
	if isStreamHandlerType(ht) {
		reqIdx = 0
	}
	reqType := ht.In(reqIdx)
	if reqType.Kind() != reflect.Pointer {
		return nil
	}
	elem := reqType.Elem()
	if _, ok := reflect.New(elem).Interface().(proto.Message); !ok {
		return nil
	}
	return func() proto.Message { return reflect.New(elem).Interface().(proto.Message) }
}

var (
	serverStreamType = reflect.TypeFor[ServerStream]()
	errorType        = reflect.TypeFor[error]()
	protoMsgType     = reflect.TypeFor[proto.Message]()
)

// isStreamHandlerType reports whether ht is `func(*Req, ServerStream) error`.
func isStreamHandlerType(ht reflect.Type) bool {
	return ht.NumIn() == 2 &&
		ht.NumOut() == 1 &&
		ht.In(0).Kind() == reflect.Pointer &&
		reflect.New(ht.In(0).Elem()).Type().Implements(protoMsgType) &&
		ht.In(1) == serverStreamType &&
		ht.Out(0) == errorType
}

// buildStreamHandler reflects on a server-streaming handler and returns a
// proto-in/stream-out closure. Reflection runs once at registration.
func buildStreamHandler(handler any) (legacyStreamHandler, error) {
	hv := reflect.ValueOf(handler)
	ht := hv.Type()
	if !isStreamHandlerType(ht) {
		return nil, errors.New("stream handler has unexpected signature (expected func(*Req, ServerStream) error)")
	}
	reqType := ht.In(0)
	return func(req any, stream ServerStream) error {
		reqMsg, ok := req.(proto.Message)
		if !ok {
			return fmt.Errorf("stream request does not implement proto.Message: %T", req)
		}
		// Convert dynamicpb (gRPC proxy ingress) to the handler's typed request.
		if dynMsg, isDyn := reqMsg.(*dynamicpb.Message); isDyn {
			typed := reflect.New(reqType.Elem()).Interface().(proto.Message)
			if dynMsg.ProtoReflect().Descriptor().FullName() == typed.ProtoReflect().Descriptor().FullName() {
				b, err := proto.Marshal(reqMsg)
				if err != nil {
					return fmt.Errorf("marshal dynamic to binary: %w", err)
				}
				if err := proto.Unmarshal(b, typed); err != nil {
					return fmt.Errorf("unmarshal binary to typed: %w", err)
				}
			} else {
				b, err := protojson.Marshal(reqMsg)
				if err != nil {
					return fmt.Errorf("marshal dynamic to JSON: %w", err)
				}
				if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, typed); err != nil {
					return fmt.Errorf("unmarshal JSON to typed: %w", err)
				}
			}
			reqMsg = typed
		}
		results := hv.Call([]reflect.Value{
			reflect.ValueOf(reqMsg),
			reflect.ValueOf(stream),
		})
		if !results[0].IsNil() {
			return results[0].Interface().(error)
		}
		return nil
	}, nil
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
		return nil, errors.New("handler has unexpected signature (expected func(ctx, *Req) (*Resp, error))")
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

// matchServicer finds services whose RPC names match methods on the
// servicer. Both unary and server-streaming methods participate; client-
// streaming is intentionally excluded (the framework doesn't project it).
// The reflection check is loose — any method with the matching name on
// the servicer counts as a hit; full handler-signature validation happens
// later in addTool.
func (s *Server) matchServicer(servicer any) map[string]*invpb.ServiceInfo {
	servicerVal := reflect.ValueOf(servicer)
	matched := make(map[string]*invpb.ServiceInfo)
	for svcFullName, svcInfo := range s.parsed.Services {
		for methodName, methodInfo := range svcInfo.Methods {
			if methodInfo.ClientStreaming {
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

// remoteProxyService is the captured implementation behind descriptor-driven
// remote clients. Its grpc.MethodDesc handlers forward directly through the
// caller-owned client connection; there is no hidden in-memory transport hop.
type remoteProxyService struct{}

type protoUnaryCaller interface {
	callProto(context.Context, proto.Message) (proto.Message, error)
}

func proxyUnaryMethodDesc(
	methodName string,
	fullMethod string,
	newRequest func() proto.Message,
	caller protoUnaryCaller,
) grpc.MethodDesc {
	return grpc.MethodDesc{
		MethodName: methodName,
		Handler: func(service any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			if newRequest == nil {
				return nil, status.Errorf(codes.Internal, "no request type registered for %s", fullMethod)
			}
			request := newRequest()
			if err := dec(request); err != nil {
				return nil, err
			}
			terminal := func(ctx context.Context, request any) (any, error) {
				message, ok := request.(proto.Message)
				if !ok {
					return nil, status.Errorf(codes.Internal, "request for %s does not implement proto.Message: %T", fullMethod, request)
				}
				return caller.callProto(ctx, message)
			}
			if interceptor == nil {
				return terminal(ctx, request)
			}
			return interceptor(ctx, request, &grpc.UnaryServerInfo{
				Server: service, FullMethod: fullMethod,
			}, terminal)
		},
	}
}

// ConnectGRPC registers a gRPC client connection's methods as tools.
// The caller creates and owns the grpc.ClientConnInterface. Default call
// options are applied to every projected unary call.
// Use Include/Exclude to filter optional projection tools; the native proxy
// service retains every supported unary method.
func (s *Server) ConnectGRPC(conn grpc.ClientConnInterface, defaultCallOptions ...grpc.CallOption) error {
	if err := s.ensureRegistrationOpen("gRPC connection"); err != nil {
		return err
	}
	if s.fds == nil {
		return errors.New("connect requires a Server created via ServerFromDescriptor or ServerFromBytes")
	}
	if conn == nil {
		return errors.New("connect requires a non-nil grpc.ClientConnInterface")
	}

	files, err := s.buildProtoFiles()
	if err != nil {
		return err
	}

	serviceDescs := make(map[string]*grpc.ServiceDesc)
	serviceImpls := make(map[string]*remoteProxyService)
	for svcFullName, svcInfo := range s.parsed.Services {
		for methodName, methodInfo := range svcInfo.Methods {
			// Streaming RPCs aren't proxied — opinionated. Proxy-streaming would
			// duplicate gRPC's own forwarding story without adding value.
			if methodInfo.ClientStreaming || methodInfo.ServerStreaming {
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
			desc := serviceDescs[svcFullName]
			if desc == nil {
				desc = &grpc.ServiceDesc{
					ServiceName: svcFullName,
					HandlerType: (*legacyGRPCService)(nil),
					Metadata:    reqDesc.ParentFile().Path(),
				}
				serviceDescs[svcFullName] = desc
				serviceImpls[svcFullName] = &remoteProxyService{}
			}
			caller := &grpcDynamicHandler{
				conn:               conn,
				methodPath:         methodPath,
				reqDesc:            reqDesc,
				respDesc:           respDesc,
				newResponse:        s.messageFactory(methodInfo.OutputType),
				defaultCallOptions: slices.Clone(defaultCallOptions),
			}
			desc.Methods = append(desc.Methods, proxyUnaryMethodDesc(
				methodName, methodPath, s.messageFactory(methodInfo.InputType), caller,
			))
		}
	}

	names := make([]string, 0, len(serviceDescs))
	for name := range serviceDescs {
		names = append(names, name)
	}
	slices.Sort(names)
	registrations := make([]registeredService, 0, len(names))
	for _, name := range names {
		registrations = append(registrations, registeredService{
			desc: serviceDescs[name], service: serviceImpls[name],
		})
	}
	return s.registerServices(registrations, "gRPC connection")
}

// Connect is the compatibility spelling for ConnectGRPC.
//
// Deprecated: use ConnectGRPC to supply a grpc.ClientConnInterface and optional
// default grpc.CallOption values.
func (s *Server) Connect(conn grpc.ClientConnInterface) error {
	return s.ConnectGRPC(conn)
}

// Projection specifies a protocol to serve.
type Projection struct {
	kind     string
	port     int
	grpcOpts []grpc.ServerOption
}

// HTTP returns a projection that serves HTTP on the given port.
func HTTP(port int) Projection { return Projection{kind: "http", port: port} }

// GRPC returns the deprecated port-owning gRPC projection. The variadic options
// remain for source compatibility but are rejected at serve time; pass ordinary
// grpc.ServerOption values to ServerFromDescriptor/ServerFromBytes instead.
//
// Deprecated: create a listener and call Server.Serve.
func GRPC(port int, opts ...grpc.ServerOption) Projection {
	return Projection{kind: "grpc", port: port, grpcOpts: opts}
}

// MCP returns a projection that serves MCP over stdio.
func MCP() Projection { return Projection{kind: "mcp"} }

// CLI returns a projection that runs as a CLI from os.Args.
func CLI() Projection { return Projection{kind: "cli"} }

// ServeProjections starts the specified optional projections and blocks until ctx is canceled
// or the first projection returns an error.
//
//	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
//	defer cancel()
//	server.ServeProjections(ctx, invariant.HTTP(8080))
//	server.ServeProjections(ctx, invariant.HTTP(8080), invariant.MCP())
//
// On error or cancellation, all projections receive a graceful shutdown signal.
func (s *Server) ServeProjections(ctx context.Context, projections ...Projection) error {
	if len(projections) == 0 {
		return errors.New("no projections specified")
	}
	s.freeze()
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

// Invoke dispatches a unary request to a registered tool by name. Useful for
// in-process callers (workflow runtimes, tests) that don't need to spin up
// a projection.
//
// Returns NOT_FOUND if the tool is unknown, and FAILED_PRECONDITION if the
// tool is server-streaming — use InvokeStream for those. Both errors project
// to the right code through every projection.
func (s *Server) Invoke(ctx context.Context, toolName string, req proto.Message) (proto.Message, error) {
	s.freeze()
	tool, ok := s.tools[toolName]
	if !ok {
		var available []string
		for name := range s.tools {
			available = append(available, name)
		}
		return nil, status.Errorf(codes.NotFound, "unknown tool %q. Available: %v", toolName, available)
	}
	if tool.ServerStreaming {
		return nil, status.Errorf(codes.FailedPrecondition, "tool %q is server-streaming — use InvokeStream", toolName)
	}
	return s.invoke(ctx, tool, req)
}

// InvokeStream dispatches a server-streaming tool by name. Each emitted
// response message is delivered to send; the call returns once the handler
// returns or send returns an error.
//
// Like Invoke, this is the in-process entry point — no projection required.
func (s *Server) InvokeStream(ctx context.Context, toolName string, req proto.Message, send func(proto.Message) error) error {
	s.freeze()
	tool, ok := s.tools[toolName]
	if !ok {
		var available []string
		for name := range s.tools {
			available = append(available, name)
		}
		return status.Errorf(codes.NotFound, "unknown tool %q. Available: %v", toolName, available)
	}
	if !tool.ServerStreaming {
		return status.Errorf(codes.FailedPrecondition, "tool %q is unary — use Invoke", toolName)
	}
	stream := newCallbackStream(ctx, send)
	defer stream.close()
	return s.invokeStream(tool, req, stream)
}

// Tools returns a snapshot of the registered tool names to their Tool metadata.
func (s *Server) Tools() map[string]*Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*Tool, len(s.tools))
	for name, tool := range s.tools {
		snapshot := *tool
		snapshot.InputSchema = cloneMap(tool.InputSchema)
		out[name] = &snapshot
	}
	return out
}

// ToolCatalog returns the canonical tool catalog (same shape as MCP `tools/list`).
// Used by both the HTTP `GET /` endpoint and MCP's `tools/list`.
//
// Streaming tools carry `_meta.streaming: true` so clients can render and
// consume them differently from unary tools. The MCP spec reserves `_meta`
// for exactly this kind of server-specific annotation.
func (s *Server) ToolCatalog() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	slices.Sort(names)

	out := make([]map[string]any, 0, len(s.tools))
	for _, name := range names {
		t := s.tools[name]
		entry := map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": cloneMap(t.InputSchema),
		}
		if t.ServerStreaming {
			entry["_meta"] = map[string]any{"streaming": true}
		}
		out = append(out, entry)
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
