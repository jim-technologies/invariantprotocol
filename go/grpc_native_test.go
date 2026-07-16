package invariant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var _ grpc.ServiceRegistrar = (*Server)(nil)

type nativeTestService struct {
	greetpb.UnimplementedGreetServiceServer
	greetHook  func(context.Context, *greetpb.GreetRequest) (*greetpb.GreetResponse, error)
	streamHook func(*greetpb.StreamGreetRequest, grpc.ServerStreamingServer[greetpb.GreetResponse]) error
}

type nativeTestClientConnWrapper struct {
	grpc.ClientConnInterface
}

type nativeTestCallCredentials struct{}

func (nativeTestCallCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"x-default-call-option": "applied"}, nil
}

func (nativeTestCallCredentials) RequireTransportSecurity() bool { return false }

func (s *nativeTestService) Greet(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	if s.greetHook != nil {
		return s.greetHook(ctx, req)
	}
	return &greetpb.GreetResponse{Message: "Hello, " + req.GetName()}, nil
}

func (*nativeTestService) GreetGroup(_ context.Context, req *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	return &greetpb.GreetGroupResponse{Count: int32(len(req.GetPeople()))}, nil
}

func (s *nativeTestService) StreamGreet(req *greetpb.StreamGreetRequest, stream grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
	if s.streamHook != nil {
		return s.streamHook(req, stream)
	}
	count := int(req.GetCount())
	if count <= 0 {
		count = 1
	}
	for i := range count {
		if err := stream.Send(&greetpb.GreetResponse{Message: fmt.Sprintf("Hello, %s #%d", req.GetName(), i)}); err != nil {
			return err
		}
	}
	return nil
}

func nativeTestServer(t *testing.T, options ...grpc.ServerOption) *Server {
	t.Helper()
	server, err := ServerFromDescriptor(descriptorPath(), options...)
	require.NoError(t, err)
	return server
}

func nativeTestStartConnection(t *testing.T, server *Server) *grpc.ClientConn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
		server.Stop()
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after Stop")
		}
	})
	return conn
}

func nativeTestStart(t *testing.T, server *Server) greetpb.GreetServiceClient {
	t.Helper()
	return greetpb.NewGreetServiceClient(nativeTestStartConnection(t, server))
}

func TestNativeGeneratedRegistrationAndClients(t *testing.T) {
	server := nativeTestServer(t)
	service := &nativeTestService{}
	greetpb.RegisterGreetServiceServer(server, service)

	serviceInfo := server.GetServiceInfo()
	require.Contains(t, serviceInfo, "greet.v1.GreetService")
	assert.Contains(t, server.Tools(), "GreetService.Greet")
	assert.Contains(t, server.Tools(), "GreetService.StreamGreet")

	client := nativeTestStart(t, server)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	response, err := client.Greet(ctx, &greetpb.GreetRequest{Name: "generated"})
	require.NoError(t, err)
	assert.Equal(t, "Hello, generated", response.GetMessage())

	stream, err := client.StreamGreet(ctx, &greetpb.StreamGreetRequest{Name: "stream", Count: 3})
	require.NoError(t, err)
	var messages []string
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		messages = append(messages, message.GetMessage())
	}
	assert.Equal(t, []string{"Hello, stream #0", "Hello, stream #1", "Hello, stream #2"}, messages)
}

func TestNativeFiltersOnlyOptionalProjections(t *testing.T) {
	server := nativeTestServer(t)
	server.Include("greet.v1.GreetService.Greet")
	greetpb.RegisterGreetServiceServer(server, &nativeTestService{})

	assert.Contains(t, server.Tools(), "GreetService.Greet")
	assert.NotContains(t, server.Tools(), "GreetService.GreetGroup")
	client := nativeTestStart(t, server)
	response, err := client.GreetGroup(t.Context(), &greetpb.GreetGroupRequest{
		People: []*greetpb.Person{{Name: "one"}, {Name: "two"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(2), response.GetCount(), "native generated gRPC retains the full service")
}

func TestNativeConventionalHealthRegistration(t *testing.T) {
	server := nativeTestServer(t)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, healthServer)

	assert.Contains(t, server.GetServiceInfo(), "grpc.health.v1.Health")
	conn := nativeTestStartConnection(t, server)
	response, err := healthpb.NewHealthClient(conn).Check(t.Context(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, response.GetStatus())
	assert.Empty(t, server.Tools(), "infrastructure services outside the descriptor must not become tools")
}

func TestNativeConnectGRPCNativeProxyPreservesSemantics(t *testing.T) {
	incoming := make(chan metadata.MD, 1)
	detail := &errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{
		Field: "name", Description: "remote rejection",
	}}}
	richStatus, err := status.New(codes.FailedPrecondition, "remote status").WithDetails(detail)
	require.NoError(t, err)
	remoteService := &nativeTestService{greetHook: func(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
		if req.GetName() == "status" {
			return nil, richStatus.Err()
		}
		md, _ := metadata.FromIncomingContext(ctx)
		incoming <- md.Copy()
		if err := grpc.SendHeader(ctx, metadata.Pairs("x-remote-header", "leading")); err != nil {
			return nil, err
		}
		if err := grpc.SetTrailer(ctx, metadata.Pairs("x-remote-trailer", "trailing")); err != nil {
			return nil, err
		}
		return &greetpb.GreetResponse{Message: "remote:" + req.GetName()}, nil
	}}
	remoteServer := nativeTestServer(t)
	greetpb.RegisterGreetServiceServer(remoteServer, remoteService)
	remoteConn := nativeTestStartConnection(t, remoteServer)

	proxyServer := nativeTestServer(t)
	var typedRequest, typedResponse atomic.Bool
	proxyServer.Use(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		_, requestOK := req.(*greetpb.GreetRequest)
		typedRequest.Store(requestOK)
		response, err := handler(ctx, req)
		if response != nil {
			_, responseOK := response.(*greetpb.GreetResponse)
			typedResponse.Store(responseOK)
		}
		assert.Equal(t, greetpb.GreetService_Greet_FullMethodName, info.FullMethod)
		return response, err
	})
	require.NoError(t, proxyServer.ConnectGRPC(
		nativeTestClientConnWrapper{ClientConnInterface: remoteConn},
		grpc.PerRPCCredentials(nativeTestCallCredentials{}),
	))
	proxyClient := nativeTestStart(t, proxyServer)

	ctx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-client-metadata", "forwarded"))
	var header, trailer metadata.MD
	response, err := proxyClient.Greet(ctx, &greetpb.GreetRequest{Name: "proxy"}, grpc.Header(&header), grpc.Trailer(&trailer))
	require.NoError(t, err)
	assert.Equal(t, "remote:proxy", response.GetMessage())
	remoteMetadata := <-incoming
	assert.Equal(t, []string{"forwarded"}, remoteMetadata.Get("x-client-metadata"))
	assert.Equal(t, []string{"applied"}, remoteMetadata.Get("x-default-call-option"))
	assert.Equal(t, []string{"leading"}, header.Get("x-remote-header"))
	assert.Equal(t, []string{"trailing"}, trailer.Get("x-remote-trailer"))
	assert.True(t, typedRequest.Load())
	assert.True(t, typedResponse.Load())

	_, err = proxyClient.Greet(t.Context(), &greetpb.GreetRequest{Name: "status"})
	require.Error(t, err)
	gotStatus, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, gotStatus.Code())
	assert.Equal(t, "remote status", gotStatus.Message())
	require.Len(t, gotStatus.Details(), 1)
	gotDetail, ok := gotStatus.Details()[0].(*errdetails.BadRequest)
	require.True(t, ok)
	assert.Equal(t, "remote rejection", gotDetail.GetFieldViolations()[0].GetDescription())
}

func TestNativeConnectGRPCPropagatesDeadlineAndCancellation(t *testing.T) {
	deadlineSeen := make(chan bool, 1)
	cancelStarted := make(chan struct{})
	cancelSeen := make(chan error, 1)
	remoteService := &nativeTestService{greetHook: func(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
		switch req.GetName() {
		case "deadline":
			_, ok := ctx.Deadline()
			deadlineSeen <- ok
			<-ctx.Done()
			return nil, status.FromContextError(ctx.Err()).Err()
		case "cancel":
			close(cancelStarted)
			<-ctx.Done()
			cancelSeen <- ctx.Err()
			return nil, status.FromContextError(ctx.Err()).Err()
		default:
			return &greetpb.GreetResponse{Message: req.GetName()}, nil
		}
	}}
	remoteServer := nativeTestServer(t)
	greetpb.RegisterGreetServiceServer(remoteServer, remoteService)
	remoteConn := nativeTestStartConnection(t, remoteServer)

	proxyServer := nativeTestServer(t)
	require.NoError(t, proxyServer.ConnectGRPC(remoteConn))
	client := nativeTestStart(t, proxyServer)

	deadlineCtx, deadlineCancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer deadlineCancel()
	_, err := client.Greet(deadlineCtx, &greetpb.GreetRequest{Name: "deadline"})
	require.Error(t, err)
	assert.Equal(t, codes.DeadlineExceeded, status.Code(err))
	select {
	case seen := <-deadlineSeen:
		assert.True(t, seen)
	case <-time.After(2 * time.Second):
		t.Fatal("proxied deadline did not reach the remote handler")
	}

	cancelCtx, cancel := context.WithCancel(t.Context())
	callErr := make(chan error, 1)
	go func() {
		_, err := client.Greet(cancelCtx, &greetpb.GreetRequest{Name: "cancel"})
		callErr <- err
	}()
	select {
	case <-cancelStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("proxied cancellation request did not reach the remote handler")
	}
	cancel()
	select {
	case err := <-callErr:
		require.Error(t, err)
		assert.Equal(t, codes.Canceled, status.Code(err))
	case <-time.After(2 * time.Second):
		t.Fatal("canceled proxied RPC did not return")
	}
	select {
	case err := <-cancelSeen:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("remote handler did not observe proxied cancellation")
	}
}

func TestNativeRejectsSplitLocalAndProxyRegistration(t *testing.T) {
	remoteServer := nativeTestServer(t)
	greetpb.RegisterGreetServiceServer(remoteServer, &nativeTestService{})
	remoteConn := nativeTestStartConnection(t, remoteServer)

	server := nativeTestServer(t)
	greetpb.RegisterGreetServiceServer(server, &nativeTestService{})
	err := server.ConnectGRPC(remoteConn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate service registration for "greet.v1.GreetService"`)
	assert.Len(t, server.Tools(), 3, "the failed proxy registration must not replace local projections")
}

type nativeTestObservingStream struct {
	grpc.ServerStream
	received *atomic.Int32
}

func (s *nativeTestObservingStream) RecvMsg(message any) error {
	if _, ok := message.(*greetpb.StreamGreetRequest); !ok {
		return status.Errorf(codes.Internal, "stream interceptor received %T, want *greetpb.StreamGreetRequest", message)
	}
	s.received.Add(1)
	return s.ServerStream.RecvMsg(message)
}

func TestNativeStandardAndSharedInterceptorsRunOnce(t *testing.T) {
	service := &nativeTestService{}
	var nativeUnaryOption, nativeUnaryChain, nativeStream atomic.Int32
	var nativeStreamRecv atomic.Int32

	server := nativeTestServer(t,
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			nativeUnaryOption.Add(1)
			if _, ok := req.(*greetpb.GreetRequest); !ok {
				return nil, status.Errorf(codes.Internal, "native unary interceptor received %T", req)
			}
			if info.Server != service || info.FullMethod != greetpb.GreetService_Greet_FullMethodName {
				return nil, status.Errorf(codes.Internal, "bad native unary info: server=%T method=%q", info.Server, info.FullMethod)
			}
			return handler(ctx, req)
		}),
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			nativeUnaryChain.Add(1)
			if _, ok := req.(*greetpb.GreetRequest); !ok {
				return nil, status.Errorf(codes.Internal, "native unary interceptor received %T", req)
			}
			if info.Server != service || info.FullMethod != greetpb.GreetService_Greet_FullMethodName {
				return nil, status.Errorf(codes.Internal, "bad native unary info: server=%T method=%q", info.Server, info.FullMethod)
			}
			return handler(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			nativeStream.Add(1)
			if srv != service || info.FullMethod != greetpb.GreetService_StreamGreet_FullMethodName {
				return status.Errorf(codes.Internal, "bad native stream info: server=%T method=%q", srv, info.FullMethod)
			}
			return handler(srv, &nativeTestObservingStream{ServerStream: stream, received: &nativeStreamRecv})
		}),
	)

	var sharedUnary, sharedStream atomic.Int32
	var sharedStreamRecv atomic.Int32
	server.Use(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		sharedUnary.Add(1)
		if _, ok := req.(*greetpb.GreetRequest); !ok {
			return nil, status.Errorf(codes.Internal, "shared unary interceptor received %T", req)
		}
		if info.Server != service || info.FullMethod != greetpb.GreetService_Greet_FullMethodName {
			return nil, status.Errorf(codes.Internal, "bad shared unary info: server=%T method=%q", info.Server, info.FullMethod)
		}
		return handler(ctx, req)
	})
	server.UseStream(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		sharedStream.Add(1)
		if srv != service || info.FullMethod != greetpb.GreetService_StreamGreet_FullMethodName {
			return status.Errorf(codes.Internal, "bad shared stream info: server=%T method=%q", srv, info.FullMethod)
		}
		return handler(srv, &nativeTestObservingStream{ServerStream: stream, received: &sharedStreamRecv})
	})
	greetpb.RegisterGreetServiceServer(server, service)

	client := nativeTestStart(t, server)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	_, err := client.Greet(ctx, &greetpb.GreetRequest{Name: "intercepted"})
	require.NoError(t, err)
	stream, err := client.StreamGreet(ctx, &greetpb.StreamGreetRequest{Name: "intercepted", Count: 1})
	require.NoError(t, err)
	_, err = stream.Recv()
	require.NoError(t, err)
	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF)

	assert.Equal(t, int32(1), nativeUnaryOption.Load())
	assert.Equal(t, int32(1), nativeUnaryChain.Load())
	assert.Equal(t, int32(1), sharedUnary.Load())
	assert.Equal(t, int32(1), nativeStream.Load())
	assert.Equal(t, int32(1), sharedStream.Load())
	assert.Equal(t, int32(1), nativeStreamRecv.Load())
	assert.Equal(t, int32(1), sharedStreamRecv.Load())
}

func TestNativeMetadataAndRichStatus(t *testing.T) {
	incoming := make(chan metadata.MD, 1)
	detail := &errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{
		Field: "name", Description: "reserved",
	}}}
	richStatus, err := status.New(codes.FailedPrecondition, "cannot greet").WithDetails(detail)
	require.NoError(t, err)
	service := &nativeTestService{greetHook: func(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
		if req.GetName() == "status" {
			return nil, richStatus.Err()
		}
		md, _ := metadata.FromIncomingContext(ctx)
		incoming <- md.Copy()
		if err := grpc.SendHeader(ctx, metadata.Pairs("x-response-header", "header-value")); err != nil {
			return nil, err
		}
		if err := grpc.SetTrailer(ctx, metadata.Pairs("x-response-trailer", "trailer-value")); err != nil {
			return nil, err
		}
		return &greetpb.GreetResponse{Message: req.GetName()}, nil
	}}
	server := nativeTestServer(t)
	greetpb.RegisterGreetServiceServer(server, service)
	client := nativeTestStart(t, server)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-request-metadata", "request-value"))
	var header, trailer metadata.MD
	_, err = client.Greet(ctx, &greetpb.GreetRequest{Name: "metadata"}, grpc.Header(&header), grpc.Trailer(&trailer))
	require.NoError(t, err)
	assert.Equal(t, []string{"request-value"}, (<-incoming).Get("x-request-metadata"))
	assert.Equal(t, []string{"header-value"}, header.Get("x-response-header"))
	assert.Equal(t, []string{"trailer-value"}, trailer.Get("x-response-trailer"))

	_, err = client.Greet(ctx, &greetpb.GreetRequest{Name: "status"})
	require.Error(t, err)
	gotStatus, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, gotStatus.Code())
	assert.Equal(t, "cannot greet", gotStatus.Message())
	require.Len(t, gotStatus.Details(), 1)
	gotDetail, ok := gotStatus.Details()[0].(*errdetails.BadRequest)
	require.True(t, ok)
	require.Len(t, gotDetail.GetFieldViolations(), 1)
	assert.Equal(t, "name", gotDetail.GetFieldViolations()[0].GetField())
	assert.Equal(t, "reserved", gotDetail.GetFieldViolations()[0].GetDescription())
}

func TestNativeDeadlineAndCancellationPropagate(t *testing.T) {
	deadlineSeen := make(chan bool, 1)
	cancelStarted := make(chan struct{})
	cancelObserved := make(chan error, 1)
	service := &nativeTestService{greetHook: func(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
		switch req.GetName() {
		case "deadline":
			_, ok := ctx.Deadline()
			deadlineSeen <- ok
			<-ctx.Done()
			return nil, status.FromContextError(ctx.Err()).Err()
		case "cancel":
			close(cancelStarted)
			<-ctx.Done()
			cancelObserved <- ctx.Err()
			return nil, status.FromContextError(ctx.Err()).Err()
		default:
			return &greetpb.GreetResponse{Message: req.GetName()}, nil
		}
	}}
	server := nativeTestServer(t)
	greetpb.RegisterGreetServiceServer(server, service)
	client := nativeTestStart(t, server)

	deadlineCtx, deadlineCancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer deadlineCancel()
	_, err := client.Greet(deadlineCtx, &greetpb.GreetRequest{Name: "deadline"})
	require.Error(t, err)
	assert.Equal(t, codes.DeadlineExceeded, status.Code(err))
	select {
	case seen := <-deadlineSeen:
		assert.True(t, seen)
	case <-time.After(2 * time.Second):
		t.Fatal("deadline request did not reach handler")
	}

	cancelCtx, cancel := context.WithCancel(t.Context())
	callErr := make(chan error, 1)
	go func() {
		_, err := client.Greet(cancelCtx, &greetpb.GreetRequest{Name: "cancel"})
		callErr <- err
	}()
	select {
	case <-cancelStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation request did not reach handler")
	}
	cancel()
	select {
	case err := <-callErr:
		require.Error(t, err)
		assert.Equal(t, codes.Canceled, status.Code(err))
	case <-time.After(2 * time.Second):
		t.Fatal("canceled RPC did not return")
	}
	select {
	case observed := <-cancelObserved:
		require.ErrorIs(t, observed, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not observe cancellation")
	}
}

func TestNativeGRPCMessageLimits(t *testing.T) {
	t.Run("receive", func(t *testing.T) {
		var calls atomic.Int32
		service := &nativeTestService{greetHook: func(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
			calls.Add(1)
			return &greetpb.GreetResponse{Message: req.GetName()}, nil
		}}
		server := nativeTestServer(t, grpc.MaxRecvMsgSize(64))
		greetpb.RegisterGreetServiceServer(server, service)
		client := nativeTestStart(t, server)
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()

		_, err := client.Greet(ctx, &greetpb.GreetRequest{Name: strings.Repeat("r", 1024)})
		require.Error(t, err)
		assert.Equal(t, codes.ResourceExhausted, status.Code(err))
		assert.Zero(t, calls.Load())
	})

	t.Run("send", func(t *testing.T) {
		service := &nativeTestService{greetHook: func(_ context.Context, _ *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
			return &greetpb.GreetResponse{Message: strings.Repeat("s", 1024)}, nil
		}}
		server := nativeTestServer(t, grpc.MaxSendMsgSize(64))
		greetpb.RegisterGreetServiceServer(server, service)
		client := nativeTestStart(t, server)
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()

		_, err := client.Greet(ctx, &greetpb.GreetRequest{Name: "small"})
		require.Error(t, err)
		assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	})
}

func TestNativeLateRegistrationPanicsDeterministically(t *testing.T) {
	server := nativeTestServer(t)
	greetpb.RegisterGreetServiceServer(server, &nativeTestService{})
	_ = nativeTestStart(t, server)
	require.Eventually(t, server.frozenFast.Load, 2*time.Second, time.Millisecond,
		"Serve did not cross the registration freeze boundary")

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		greetpb.RegisterGreetServiceServer(server, &nativeTestService{})
	}()
	require.NotNil(t, recovered)
	assert.Equal(t,
		`invariant: service registration is frozen; cannot register "greet.v1.GreetService"`,
		fmt.Sprint(recovered),
	)
}

func TestNativeHTTPHandlerFreezesDistinctServiceRegistration(t *testing.T) {
	server := nativeTestServer(t)
	greetpb.RegisterGreetServiceServer(server, &nativeTestService{})
	server.HTTPHandler()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		healthpb.RegisterHealthServer(server, health.NewServer())
	}()
	require.NotNil(t, recovered)
	assert.Equal(t,
		`invariant: service registration is frozen; cannot register "grpc.health.v1.Health"`,
		fmt.Sprint(recovered),
	)
}

func TestNativeHTTPUsesGeneratedImplementation(t *testing.T) {
	var calls atomic.Int32
	var sharedCalls atomic.Int32
	service := &nativeTestService{greetHook: func(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
		calls.Add(1)
		return &greetpb.GreetResponse{Message: "shared:" + req.GetName()}, nil
	}}
	server := nativeTestServer(t)
	server.Use(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		sharedCalls.Add(1)
		if _, ok := req.(*greetpb.GreetRequest); !ok {
			return nil, status.Errorf(codes.Internal, "HTTP/native shared interceptor received %T", req)
		}
		if info.Server != service || info.FullMethod != greetpb.GreetService_Greet_FullMethodName {
			return nil, status.Errorf(codes.Internal, "bad HTTP/native shared unary info: server=%T method=%q", info.Server, info.FullMethod)
		}
		return handler(ctx, req)
	})
	greetpb.RegisterGreetServiceServer(server, service)

	handler := server.HTTPHandler()
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		httpServer.URL+greetpb.GreetService_Greet_FullMethodName,
		bytes.NewBufferString(`{"name":"http"}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var httpResponse greetpb.GreetResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&httpResponse))
	assert.Equal(t, "shared:http", httpResponse.GetMessage())

	client := nativeTestStart(t, server)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	nativeResponse, err := client.Greet(ctx, &greetpb.GreetRequest{Name: "grpc"})
	require.NoError(t, err)
	assert.Equal(t, "shared:grpc", nativeResponse.GetMessage())
	assert.Equal(t, int32(2), calls.Load())
	assert.Equal(t, int32(2), sharedCalls.Load())
}

func TestNativeGracefulStopDrainsInFlightUnary(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	service := &nativeTestService{greetHook: func(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
		close(started)
		<-release
		return &greetpb.GreetResponse{Message: "drained:" + req.GetName()}, nil
	}}
	server := nativeTestServer(t)
	greetpb.RegisterGreetServiceServer(server, service)
	client := nativeTestStart(t, server)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	type result struct {
		response *greetpb.GreetResponse
		err      error
	}
	rpcDone := make(chan result, 1)
	go func() {
		response, err := client.Greet(ctx, &greetpb.GreetRequest{Name: "request"})
		rpcDone <- result{response: response, err: err}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("gated RPC did not start")
	}

	stopDone := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("GracefulStop returned before the in-flight RPC drained")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case got := <-rpcDone:
		require.NoError(t, got.err)
		require.NotNil(t, got.response)
		assert.Equal(t, "drained:request", got.response.GetMessage())
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight RPC did not complete")
	}
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("GracefulStop did not return after the RPC drained")
	}
}
