// MCP/gRPC server entry point for the Invariant Protocol test service (Go).
//
// Usage:
//
//	go run . mcp                              # local MCP over stdio
//	go run . mcp --remote localhost:50051     # proxy to gRPC server
//	go run . grpc [--port 50051]              # start gRPC server
//	go run . cli greet.v1.GreetService Greet -r '{"name":"World"}'   # config is app's concern, not the library's
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
	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// GreetServicer implements GreetService RPCs using generated proto types.
type GreetServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

func (s *GreetServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "Hi " + req.Name + "!"}, nil
}

func (s *GreetServicer) GreetGroup(_ context.Context, req *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	var messages []string
	for _, p := range req.People {
		messages = append(messages, "Hi "+p.Name)
	}
	return &greetpb.GreetGroupResponse{
		Messages: messages,
		Count:    int32(len(messages)),
	}, nil
}

func (s *GreetServicer) StreamGreet(req *greetpb.StreamGreetRequest, stream greetpb.GreetService_StreamGreetServer) error {
	count := int(req.GetCount())
	if count <= 0 {
		count = 1
	}
	for i := range count {
		if err := stream.Send(&greetpb.GreetResponse{
			Message: fmt.Sprintf("Hi %s #%d", req.GetName(), i+1),
		}); err != nil {
			return err
		}
	}
	return nil
}

func descriptorPath() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("resolve source path")
	}
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

	if remote := flagValue("--remote"); remote != "" {
		conn, err := grpc.NewClient(remote, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return fmt.Errorf("connect to %s: %w", remote, err)
		}
		defer conn.Close()
		if err := server.ConnectGRPC(conn); err != nil {
			return fmt.Errorf("register from %s: %w", remote, err)
		}
	} else {
		greetpb.RegisterGreetServiceServer(server, &GreetServicer{})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return server.ServeProjections(ctx, invariant.MCP())
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

	server, err := invariant.ServerFromDescriptor(descriptorPath())
	if err != nil {
		return fmt.Errorf("load descriptor: %w", err)
	}
	greetpb.RegisterGreetServiceServer(server, &GreetServicer{})

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}
	fmt.Fprintf(os.Stderr, "gRPC server listening on port %d\n", lis.Addr().(*net.TCPAddr).Port)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()
	return server.Serve(lis)
}

func runCLI() error {
	server, err := invariant.ServerFromDescriptor(descriptorPath())
	if err != nil {
		return fmt.Errorf("load descriptor: %w", err)
	}

	if remote := flagValue("--remote"); remote != "" {
		conn, err := grpc.NewClient(remote, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return fmt.Errorf("connect to %s: %w", remote, err)
		}
		defer conn.Close()
		if err := server.ConnectGRPC(conn); err != nil {
			return fmt.Errorf("register from %s: %w", remote, err)
		}
	} else {
		greetpb.RegisterGreetServiceServer(server, &GreetServicer{})
	}

	return server.ServeProjections(context.Background(), invariant.CLI())
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  go run . mcp [--remote host:port]    # MCP over stdio")
		fmt.Fprintln(os.Stderr, "  go run . grpc [--port 50051]         # gRPC server")
		fmt.Fprintln(os.Stderr, "  go run . cli package.ServiceName Method [-r request]   # CLI")
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
