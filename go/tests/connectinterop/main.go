// Command connectinterop serves the generated Greet service over Invariant's
// HTTP/Connect projection for the repository interoperability test.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	invariant "github.com/jim-technologies/invariantprotocol/go"
	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type greetService struct {
	greetpb.UnimplementedGreetServiceServer
}

func (greetService) Greet(_ context.Context, request *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	if request.GetName() == "error" {
		return nil, status.Error(codes.FailedPrecondition, "interop status")
	}
	return &greetpb.GreetResponse{Message: "Hi " + request.GetName()}, nil
}

func (greetService) GreetGroup(
	_ context.Context,
	request *greetpb.GreetGroupRequest,
) (*greetpb.GreetGroupResponse, error) {
	messages := make([]string, 0, len(request.GetPeople()))
	for _, person := range request.GetPeople() {
		messages = append(messages, "Hi "+person.GetName())
	}
	return &greetpb.GreetGroupResponse{
		Messages: messages,
		Count:    int32(len(messages)),
	}, nil
}

func (greetService) StreamGreet(
	request *greetpb.StreamGreetRequest,
	stream greetpb.GreetService_StreamGreetServer,
) error {
	count := request.GetCount()
	if count <= 0 {
		count = 1
	}
	for index := range count {
		if err := stream.Send(&greetpb.GreetResponse{
			Message: fmt.Sprintf("Hi %s #%d", request.GetName(), index),
		}); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	server, err := invariant.ServerFromDescriptor("python/tests/proto/descriptor.binpb")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	greetpb.RegisterGreetServiceServer(server, greetService{})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	httpServer := &http.Server{
		Handler:           server.HTTPHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()

	fmt.Printf("http://%s\n", listener.Addr())
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
