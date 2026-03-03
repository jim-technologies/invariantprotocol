// MCP/gRPC server entry point for the Invariant Protocol test service (Go).
//
// Usage:
//
//	go run . mcp                              # local MCP over stdio
//	go run . mcp --remote localhost:50051     # proxy to gRPC server
//	go run . grpc [--port 50051]              # start gRPC server
//	go run . cli GreetService Greet -r '{"name":"World"}'   # config is app's concern, not the library's
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"

	invariant "github.com/jim-technologies/invariantprotocol/go"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// GreetServicer implements GreetService RPCs using structpb.Struct.
type GreetServicer struct{}

func (s *GreetServicer) Greet(_ context.Context, req *structpb.Struct) (*structpb.Struct, error) {
	name := ""
	if v, ok := req.GetFields()["name"]; ok {
		name = v.GetStringValue()
	}
	result, _ := structpb.NewStruct(map[string]any{"message": "Hi " + name + "!"})
	return result, nil
}

func (s *GreetServicer) GreetGroup(_ context.Context, req *structpb.Struct) (*structpb.Struct, error) {
	var messages []any
	if v, ok := req.GetFields()["people"]; ok {
		for _, p := range v.GetListValue().GetValues() {
			name := p.GetStructValue().GetFields()["name"].GetStringValue()
			messages = append(messages, "Hi "+name)
		}
	}
	result, _ := structpb.NewStruct(map[string]any{
		"messages": messages,
		"count":    float64(len(messages)),
	})
	return result, nil
}

func descriptorPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "python", "tests", "proto", "descriptor.binpb")
}

func flagValue(name string) string {
	for i, arg := range os.Args {
		if arg == name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func runMCP() error {
	server, err := invariant.ServerFromDescriptor(descriptorPath())
	if err != nil {
		return fmt.Errorf("load descriptor: %w", err)
	}
	defer server.Stop()

	if remote := flagValue("--remote"); remote != "" {
		if err := server.Connect(remote); err != nil {
			return fmt.Errorf("connect to %s: %w", remote, err)
		}
	} else {
		if err := server.Register(&GreetServicer{}); err != nil {
			return err
		}
	}

	return server.Serve(invariant.MCP())
}

func runGRPC() error {
	port := 50051
	if p := flagValue("--port"); p != "" {
		var err error
		port, err = strconv.Atoi(p)
		if err != nil {
			return fmt.Errorf("invalid port: %w", err)
		}
	}

	// Build proto file registry for dynamic message creation
	data, err := os.ReadFile(descriptorPath())
	if err != nil {
		return fmt.Errorf("read descriptor: %w", err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &fds); err != nil {
		return fmt.Errorf("unmarshal descriptor: %w", err)
	}
	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		return fmt.Errorf("build file descriptors: %w", err)
	}

	lookup := func(name string) protoreflect.MessageDescriptor {
		d, err := files.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			panic(fmt.Sprintf("message %q not found: %v", name, err))
		}
		return d.(protoreflect.MessageDescriptor)
	}

	reqDesc := lookup("greet.v1.GreetRequest")
	respDesc := lookup("greet.v1.GreetResponse")
	groupReqDesc := lookup("greet.v1.GreetGroupRequest")
	groupRespDesc := lookup("greet.v1.GreetGroupResponse")

	s := grpc.NewServer()
	s.RegisterService(&grpc.ServiceDesc{
		ServiceName: "greet.v1.GreetService",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "Greet",
				Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					req := dynamicpb.NewMessage(reqDesc)
					if err := dec(req); err != nil {
						return nil, err
					}
					name := req.Get(reqDesc.Fields().ByName("name")).String()

					resp := dynamicpb.NewMessage(respDesc)
					resp.Set(respDesc.Fields().ByName("message"), protoreflect.ValueOfString("Hi "+name+"!"))
					if moodField := reqDesc.Fields().ByName("mood"); req.Has(moodField) {
						resp.Set(respDesc.Fields().ByName("mood"), req.Get(moodField))
					}
					tagsField := reqDesc.Fields().ByName("tags")
					reqTags := req.Get(tagsField).Map()
					if reqTags.Len() > 0 {
						respTags := resp.Mutable(respDesc.Fields().ByName("tags")).Map()
						reqTags.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
							respTags.Set(k, v)
							return true
						})
					}
					return resp, nil
				},
			},
			{
				MethodName: "GreetGroup",
				Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					req := dynamicpb.NewMessage(groupReqDesc)
					if err := dec(req); err != nil {
						return nil, err
					}
					people := req.Get(groupReqDesc.Fields().ByName("people")).List()

					resp := dynamicpb.NewMessage(groupRespDesc)
					msgsList := resp.Mutable(groupRespDesc.Fields().ByName("messages")).List()
					for i := 0; i < people.Len(); i++ {
						person := people.Get(i).Message()
						name := person.Get(person.Descriptor().Fields().ByName("name")).String()
						msgsList.Append(protoreflect.ValueOfString("Hi " + name))
					}
					resp.Set(groupRespDesc.Fields().ByName("count"), protoreflect.ValueOfInt32(int32(people.Len())))
					return resp, nil
				},
			},
		},
	}, struct{}{})

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}
	fmt.Fprintf(os.Stderr, "gRPC server listening on port %d\n", lis.Addr().(*net.TCPAddr).Port)

	// Block until Ctrl-C
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go s.Serve(lis)
	<-ctx.Done()
	s.GracefulStop()
	return nil
}

func runCLI() error {
	server, err := invariant.ServerFromDescriptor(descriptorPath())
	if err != nil {
		return fmt.Errorf("load descriptor: %w", err)
	}
	defer server.Stop()

	if remote := flagValue("--remote"); remote != "" {
		if err := server.Connect(remote); err != nil {
			return fmt.Errorf("connect to %s: %w", remote, err)
		}
	} else {
		if err := server.Register(&GreetServicer{}); err != nil {
			return err
		}
	}

	return server.Serve(invariant.CLI())
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  go run . mcp [--remote host:port]    # MCP over stdio")
		fmt.Fprintln(os.Stderr, "  go run . grpc [--port 50051]         # gRPC server")
		fmt.Fprintln(os.Stderr, "  go run . cli ServiceName Method [-r request]   # CLI")
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "mcp":
		err = runMCP()
	case "grpc":
		err = runGRPC()
	case "cli":
		err = runCLI()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
