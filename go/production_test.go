package invariant

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// -- Panic recovery: a panicking handler must NOT crash the server. --

type panicServicer struct{}

func (panicServicer) Greet(_ context.Context, _ *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	panic("kaboom")
}

func (panicServicer) GreetGroup(_ context.Context, _ *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	return &greetpb.GreetGroupResponse{}, nil
}

func (panicServicer) StreamGreet(_ *greetpb.StreamGreetRequest, _ ServerStream) error {
	panic("stream-kaboom")
}

func TestUnaryPanicBecomesInternalError(t *testing.T) {
	srv := streamServer(t, panicServicer{})
	tool := srv.tools["GreetService.Greet"]
	_, err := srv.invoke(t.Context(), tool, &greetpb.GreetRequest{Name: "x"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Internal, st.Code())
	assert.Contains(t, st.Message(), "kaboom")
	assert.Contains(t, st.Message(), "/greet.v1.GreetService/Greet")
}

func TestStreamPanicBecomesInternalError(t *testing.T) {
	srv := streamServer(t, panicServicer{})
	tool := srv.tools["GreetService.StreamGreet"]
	cb := newCallbackStream(t.Context(), func(proto.Message) error { return nil })
	defer cb.close()
	err := srv.invokeStream(tool, &greetpb.StreamGreetRequest{Name: "x"}, cb)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Internal, st.Code())
	assert.Contains(t, st.Message(), "stream-kaboom")
}

// -- /healthz: serves a simple OK response for any HTTP deployment. --

func TestHTTPHealthz(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	handler, err := srv.HTTPHandler()
	require.NoError(t, err)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	for _, path := range []string{"/healthz", "/readyz"} {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "path=%s", path)
		assert.JSONEq(t, `{"status":"ok"}`, string(body), "path=%s", path)
	}
}

// -- Multi-interceptor stream ordering: first registered = outermost. --

func TestStreamInterceptorOrdering(t *testing.T) {
	srv := streamServer(t, &streamServicer{})

	var order []string
	srv.UseStream(func(req any, stream ServerStream, _ *ServerCallInfo, handler StreamHandler) error {
		order = append(order, "outer-before")
		err := handler(req, stream)
		order = append(order, "outer-after")
		return err
	})
	srv.UseStream(func(req any, stream ServerStream, _ *ServerCallInfo, handler StreamHandler) error {
		order = append(order, "inner-before")
		err := handler(req, stream)
		order = append(order, "inner-after")
		return err
	})

	tool := srv.tools["GreetService.StreamGreet"]
	var sent atomic.Int32
	cb := newCallbackStream(t.Context(), func(proto.Message) error {
		sent.Add(1)
		return nil
	})
	defer cb.close()

	require.NoError(t, srv.invokeStream(tool, &greetpb.StreamGreetRequest{Name: "A", Count: 1}, cb))
	assert.Equal(t, int32(1), sent.Load())
	assert.Equal(t, []string{"outer-before", "inner-before", "inner-after", "outer-after"}, order)
}

// -- Stream cancellation: canceled context propagates to handler's Send. --

type slowStreamServicer struct {
	started chan struct{}
}

func (s *slowStreamServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "hi " + req.Name}, nil
}

func (s *slowStreamServicer) StreamGreet(_ *greetpb.StreamGreetRequest, stream ServerStream) error {
	if err := stream.Send(&greetpb.GreetResponse{Message: "alive"}); err != nil {
		return err
	}
	close(s.started)
	// Block until ctx is canceled — then try one more Send which must fail.
	<-stream.Context().Done()
	if err := stream.Send(&greetpb.GreetResponse{Message: "should-not-arrive"}); err == nil {
		return errors.New("send must fail after ctx cancel but returned nil")
	}
	return stream.Context().Err()
}

func TestStreamCancellationStopsSend(t *testing.T) {
	started := make(chan struct{})
	srv := streamServer(t, &slowStreamServicer{started: started})
	tool := srv.tools["GreetService.StreamGreet"]

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var sent atomic.Int32
	cb := newCallbackStream(ctx, func(proto.Message) error {
		sent.Add(1)
		return nil
	})
	defer cb.close()

	errc := make(chan error, 1)
	go func() {
		errc <- srv.invokeStream(tool, &greetpb.StreamGreetRequest{Name: "X"}, cb)
	}()

	<-started
	cancel()
	err := <-errc
	require.ErrorIs(t, err, context.Canceled)
	// Only the first chunk should have been emitted.
	assert.Equal(t, int32(1), sent.Load())
}
