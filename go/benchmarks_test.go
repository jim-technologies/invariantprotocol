package invariant

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// benchServicer is a no-allocation servicer for benchmarks.
type benchServicer struct{}

func (s *benchServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "Hi " + req.Name}, nil
}

func (s *benchServicer) GreetGroup(_ context.Context, req *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	msgs := make([]string, len(req.People))
	for i, p := range req.People {
		msgs[i] = "Hi " + p.Name
	}
	return &greetpb.GreetGroupResponse{Messages: msgs, Count: int32(len(req.People))}, nil
}

func benchServer(b *testing.B) *Server {
	b.Helper()
	srv, err := ServerFromDescriptor(descriptorPath())
	if err != nil {
		b.Fatal(err)
	}
	if err := srv.Register(&benchServicer{}); err != nil {
		b.Fatal(err)
	}
	return srv
}

// BenchmarkInvokeDirect measures the in-process dispatch path
// (Server.Invoke → interceptor chain → handler → typed response).
func BenchmarkInvokeDirect(b *testing.B) {
	srv := benchServer(b)
	ctx := context.Background()
	req := &greetpb.GreetRequest{Name: "World"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := srv.Invoke(ctx, "GreetService.Greet", req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInvokeWithInterceptor measures the path with one no-op interceptor.
func BenchmarkInvokeWithInterceptor(b *testing.B) {
	srv := benchServer(b)
	srv.Use(func(ctx context.Context, req any, info *ServerCallInfo, handler UnaryHandler) (any, error) {
		return handler(ctx, req)
	})
	ctx := context.Background()
	req := &greetpb.GreetRequest{Name: "World"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := srv.Invoke(ctx, "GreetService.Greet", req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHTTPJSON measures the full HTTP/JSON path (parse → invoke → serialize).
func BenchmarkHTTPJSON(b *testing.B) {
	srv := benchServer(b)
	handler, err := srv.HTTPHandler()
	if err != nil {
		b.Fatal(err)
	}

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		b.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	port := lis.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://localhost:%d/greet.v1.GreetService/Greet", port)
	body := []byte(`{"name":"World"}`)

	client := &http.Client{}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkHTTPProto measures the full HTTP/binary-proto path.
func BenchmarkHTTPProto(b *testing.B) {
	srv := benchServer(b)
	handler, err := srv.HTTPHandler()
	if err != nil {
		b.Fatal(err)
	}

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		b.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(lis) }()
	defer server.Close()

	port := lis.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://localhost:%d/greet.v1.GreetService/Greet", port)
	body, _ := proto.Marshal(&greetpb.GreetRequest{Name: "World"})

	client := &http.Client{}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/proto")
		req.Header.Set("Accept", "application/proto")
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkGRPCUnary measures the full gRPC unary path through the framework.
func BenchmarkGRPCUnary(b *testing.B) {
	addr, stop := startServeGRPC_b(b)
	defer stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()

	ctx := context.Background()
	req := &greetpb.GreetRequest{Name: "World"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var resp greetpb.GreetResponse
		if err := conn.Invoke(ctx, "/greet.v1.GreetService/Greet", req, &resp); err != nil {
			b.Fatal(err)
		}
	}
}

// startServeGRPC_b is the benchmark-friendly equivalent of startServeGRPC (uses *testing.B).
func startServeGRPC_b(b *testing.B) (string, func()) {
	b.Helper()
	srv, err := ServerFromDescriptor(descriptorPath())
	if err != nil {
		b.Fatal(err)
	}
	if err := srv.Register(&benchServicer{}); err != nil {
		b.Fatal(err)
	}

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		b.Fatal(err)
	}

	files, err := srv.buildProtoFiles()
	if err != nil {
		b.Fatal(err)
	}

	gs := grpc.NewServer()
	type svcEntry struct {
		methods []grpc.MethodDesc
	}
	svcMap := make(map[string]*svcEntry)
	for _, tool := range srv.tools {
		entry, ok := svcMap[tool.ServiceFullName]
		if !ok {
			entry = &svcEntry{}
			svcMap[tool.ServiceFullName] = entry
		}
		reqMD, err := findMessageDescriptor(files, tool.InputType)
		if err != nil {
			b.Fatal(err)
		}
		respMD, err := findMessageDescriptor(files, tool.OutputType)
		if err != nil {
			b.Fatal(err)
		}
		entry.methods = append(entry.methods, grpc.MethodDesc{
			MethodName: tool.MethodName,
			Handler:    srv.grpcMethodHandler(tool, reqMD, respMD),
		})
	}
	type grpcServicer any
	for svcName, entry := range svcMap {
		gs.RegisterService(&grpc.ServiceDesc{
			ServiceName: svcName,
			HandlerType: (*grpcServicer)(nil),
			Methods:     entry.methods,
		}, struct{}{})
	}

	go func() { _ = gs.Serve(lis) }()
	return lis.Addr().String(), gs.Stop
}
