package invariant

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	invpb "github.com/jim-technologies/invariantprotocol/go/gen/invariant/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// --- Interceptor types (mirrors gRPC pattern, zero coupling to grpc package) ---

// ServerCallInfo holds metadata about the RPC being invoked, passed to interceptors.
// Equivalent to Python's ServerCallInfo.
type ServerCallInfo struct {
	FullMethod string // e.g. "/greet.v1.GreetService/Greet"
}

// UnaryHandler is the handler function called at the end of the interceptor chain.
// Equivalent to Python's Handler type.
type UnaryHandler func(ctx context.Context, req any) (any, error)

// UnaryServerInterceptor intercepts unary RPCs across all projections (MCP, HTTP,
// gRPC, CLI). Same signature as grpc.UnaryServerInterceptor but framework-native.
// Equivalent to Python's Interceptor type.
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
}

// Server holds parsed descriptors and registered tools.
type Server struct {
	Name    string // server name (used in MCP initialize)
	Version string // server version

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
// Examples: "temporal.api.workflowservice.v1.WorkflowService.*", "*.StartWorkflow*"
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
	for _, p := range strings.Split(s, ",") {
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
		Name:      "invariant-protocol",
		Version:   "0.1.0",
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

			s.tools[toolName] = &Tool{
				Name:            toolName,
				Description:     description,
				InputSchema:     s.schemaGen.MessageToSchema(methodInfo.InputType),
				Handler:         method.Interface(),
				InputType:       methodInfo.InputType,
				OutputType:      methodInfo.OutputType,
				ServiceFullName: svcFullName,
				MethodName:      methodName,
			}
		}
	}

	return nil
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

			s.tools[toolName] = &Tool{
				Name:            toolName,
				Description:     description,
				InputSchema:     s.schemaGen.MessageToSchema(methodInfo.InputType),
				Handler:         &grpcDynamicHandler{conn: conn, methodPath: methodPath, reqDesc: reqDesc, respDesc: respDesc},
				InputType:       methodInfo.InputType,
				OutputType:      methodInfo.OutputType,
				ServiceFullName: svcFullName,
				MethodName:      methodName,
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

// Serve starts the specified projections and blocks.
//
//	server.Serve(invariant.HTTP(8080))
//	server.Serve(invariant.HTTP(8080), invariant.GRPC(50051))
//	server.Serve(invariant.MCP())
//	server.Serve(invariant.CLI())
func (s *Server) Serve(projections ...Projection) error {
	if len(projections) == 0 {
		return errors.New("no projections specified")
	}
	if len(projections) == 1 {
		return s.serveOne(projections[0])
	}
	errc := make(chan error, len(projections))
	for _, p := range projections {
		go func() { errc <- s.serveOne(p) }()
	}
	return <-errc
}

func (s *Server) serveOne(p Projection) error {
	switch p.kind {
	case "mcp":
		return s.serveMCP(context.Background())
	case "cli":
		return s.serveCLI(context.Background())
	case "http":
		return s.serveHTTP(p.port)
	case "grpc":
		return s.serveGRPC(p.port, p.grpcOpts...)
	default:
		return fmt.Errorf("unknown projection: %s", p.kind)
	}
}

// Tools returns a snapshot of the registered tool names to their Tool metadata.
func (s *Server) Tools() map[string]*Tool {
	out := make(map[string]*Tool, len(s.tools))
	for k, v := range s.tools {
		out[k] = v
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
