package invariant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// -- HTTP body-size limit: unary requests must reject oversized bodies. --

func TestHTTPUnaryRejectsOversizedBody(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Pad the request well beyond httpMaxUnaryRequest. JSON parses field-by-field
	// but read should fail before parse runs.
	huge := strings.Repeat("a", httpMaxUnaryRequest+1024)
	body := `{"name":"` + huge + `"}`

	req, _ := http.NewRequestWithContext(t.Context(), "POST",
		ts.URL+"/greet.v1.GreetService/Greet", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(responseBody, &envelope))
	assert.Equal(t, "resource_exhausted", envelope["code"])
}

// SetMaxUnaryRequestBytes raises the cap for apps that legitimately accept
// large unary bodies (object stores, file servers). After raising, a body
// that would have been rejected at the default cap must now succeed.
func TestHTTPUnaryRespectsRaisedBodyCap(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	srv.SetMaxUnaryRequestBytes(int64(httpMaxUnaryRequest) * 4) // 64 MiB
	srv.SetMaxUnaryResponseBytes(int64(httpMaxUnaryRequest) * 4)
	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// 1 MiB beyond the default cap — would 400 before the override.
	huge := strings.Repeat("a", httpMaxUnaryRequest+1<<20)
	body := `{"name":"` + huge + `"}`

	req, _ := http.NewRequestWithContext(t.Context(), "POST",
		ts.URL+"/greet.v1.GreetService/Greet", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ConfigureMethod overrides the per-server cap for ONE method only — useful
// when the service has a single oversize-friendly RPC (Upload, BulkImport)
// alongside tightly-capped peers. Other methods keep the small default.
func TestHTTPUnaryConfigureMethodPerMethodCap(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	// Server-level default stays small (the cheap-RPCs cap). Bump only
	// /greet.v1.GreetService/Greet to 4× default.
	srv.ConfigureMethod("/greet.v1.GreetService/Greet", MethodConfig{
		MaxUnaryRequestBytes:  int64(httpMaxUnaryRequest) * 4,
		MaxUnaryResponseBytes: int64(httpMaxUnaryRequest) * 4,
	})
	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// A 1 MiB-over-default body to Greet now succeeds (per-method cap).
	huge := strings.Repeat("a", httpMaxUnaryRequest+1<<20)
	body := `{"name":"` + huge + `"}`

	req, _ := http.NewRequestWithContext(t.Context(), "POST",
		ts.URL+"/greet.v1.GreetService/Greet", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "per-method override should let Greet accept this body")
}

// -- application/connect+proto streaming: binary envelopes for performance. --

func TestStreamingHTTPConnectProto(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Build one request envelope wrapping a binary-encoded proto.
	reqMsg := &greetpb.StreamGreetRequest{Name: "Bin", Count: 3}
	reqBytes, err := proto.Marshal(reqMsg)
	require.NoError(t, err)

	var body bytes.Buffer
	require.NoError(t, writeConnectEnvelope(&body, 0, reqBytes))

	httpReq, _ := http.NewRequestWithContext(t.Context(), "POST",
		ts.URL+"/greet.v1.GreetService/StreamGreet", &body)
	httpReq.Header.Set("Content-Type", connectStreamProtoType)
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, connectStreamProtoType, resp.Header.Get("Content-Type"))

	frames := readAllEnvelopes(t, resp.Body)
	require.Len(t, frames, 4, "3 binary message frames + 1 end-stream JSON frame")

	// Message frames decode as binary proto.
	for i := range 3 {
		assert.Equal(t, byte(0), frames[i].flags)
		var out greetpb.GreetResponse
		require.NoError(t, proto.Unmarshal(frames[i].payload, &out))
		assert.Equal(t, "Hi Bin #"+intStr(i), out.Message)
	}
	// End-of-stream payload is always JSON.
	assert.Equal(t, connectEndStreamFlag, frames[3].flags)
	var end map[string]any
	require.NoError(t, json.Unmarshal(frames[3].payload, &end))
	_, hasErr := end["error"]
	assert.False(t, hasErr)
}

// intStr is a tiny helper to avoid pulling fmt into a hot loop.
func intStr(i int) string {
	switch i {
	case 0:
		return "0"
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	default:
		return "?"
	}
}

// -- Stop / shutdown safety: graceful shutdown must be idempotent. --

func TestServeContextCancelStopsGracefully(t *testing.T) {
	srv := streamServer(t, &streamServicer{})

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		errc <- srv.ServeProjections(ctx, HTTP(0))
	}()

	// Cancel immediately; serve should return cleanly with ctx.Err.
	cancel()
	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.Canceled)
	case <-t.Context().Done():
		t.Fatal("Serve did not return after context cancel")
	}
}

func TestServeProjectionsCancelsAndWaitsForSiblingAfterError(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { require.NoError(t, occupied.Close()) }()
	port := occupied.Addr().(*net.TCPAddr).Port

	srv := streamServer(t, &streamServicer{})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	err = srv.ServeProjections(ctx, HTTP(0), HTTP(port))
	require.Error(t, err)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	var listenErr *net.OpError
	require.ErrorAs(t, err, &listenErr)
	assert.Equal(t, "listen", listenErr.Op)

	// Returning means the ephemeral HTTP sibling observed cancellation and
	// completed graceful shutdown after the occupied-port projection failed.
}

// blockingGreetServicer holds each Greet until release closes or its request
// context ends, so a test controls when an in-flight HTTP call completes.
type blockingGreetServicer struct {
	greetpb.UnimplementedGreetServiceServer
	started chan struct{}
	release chan struct{}
}

func (b *blockingGreetServicer) Greet(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	b.started <- struct{}{}
	select {
	case <-b.release:
		return &greetpb.GreetResponse{Message: "drained " + req.GetName()}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type httpCallResult struct {
	status int
	body   string
	err    error
}

// serveBlockingHTTP runs ServeProjections(HTTP) for srv on a free port, waits
// until it answers /healthz, and starts one Greet call that the servicer holds.
func serveBlockingHTTP(t *testing.T, srv *Server, servicer *blockingGreetServicer) (context.CancelFunc, <-chan error, <-chan httpCallResult) {
	t.Helper()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := reserved.Addr().(*net.TCPAddr).Port
	require.NoError(t, reserved.Close())
	baseURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- srv.ServeProjections(ctx, HTTP(port)) }()
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 10*time.Millisecond)

	called := make(chan httpCallResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			baseURL+"/greet.v1.GreetService/Greet", strings.NewReader(`{"name":"graceful"}`))
		if err != nil {
			called <- httpCallResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			called <- httpCallResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		called <- httpCallResult{status: resp.StatusCode, body: string(body), err: err}
	}()
	select {
	case <-servicer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Greet did not start")
	}
	return cancel, served, called
}

func TestServeProjectionsDrainsInFlightHTTPOnCancellation(t *testing.T) {
	servicer := &blockingGreetServicer{started: make(chan struct{}, 1), release: make(chan struct{})}
	srv := streamServer(t, servicer)
	cancel, served, called := serveBlockingHTTP(t, srv, servicer)

	cancel()
	select {
	case err := <-served:
		t.Fatalf("ServeProjections returned before the in-flight call finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(servicer.release)

	result := <-called
	require.NoError(t, result.err)
	assert.Equal(t, http.StatusOK, result.status)
	assert.Contains(t, result.body, "drained graceful")
	err := <-served
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
}

func TestServeProjectionsClosesHTTPCallsThatOutliveShutdownTimeout(t *testing.T) {
	const timeout = 100 * time.Millisecond
	servicer := &blockingGreetServicer{started: make(chan struct{}, 1), release: make(chan struct{})}
	srv := streamServer(t, servicer)
	srv.SetHTTPShutdownTimeout(timeout)
	cancel, served, called := serveBlockingHTTP(t, srv, servicer)

	stopped := time.Now()
	cancel()
	var err error
	select {
	case err = <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeProjections did not return after the shutdown timeout")
	}
	assert.GreaterOrEqual(t, time.Since(stopped), timeout)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// The held call's connection is closed rather than answered.
	result := <-called
	require.Error(t, result.err)
}

func TestSetHTTPShutdownTimeout(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	assert.Equal(t, defaultHTTPShutdownTimeout, srv.httpShutdownTimeout)

	srv.SetHTTPShutdownTimeout(25 * time.Second)
	assert.Equal(t, 25*time.Second, srv.httpShutdownTimeout)
	srv.SetHTTPShutdownTimeout(0)
	assert.Equal(t, defaultHTTPShutdownTimeout, srv.httpShutdownTimeout)
	assert.PanicsWithValue(t, "invariant: HTTP shutdown timeout must be non-negative", func() {
		srv.SetHTTPShutdownTimeout(-time.Second)
	})

	_ = srv.HTTPHandler()
	assert.PanicsWithValue(t, "invariant: HTTP shutdown timeout cannot be changed after serving begins", func() {
		srv.SetHTTPShutdownTimeout(time.Second)
	})
}

// -- Stream edge cases. --

type emptyStreamServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

func (emptyStreamServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "hi " + req.GetName()}, nil
}

func (emptyStreamServicer) StreamGreet(_ *greetpb.StreamGreetRequest, _ grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
	// No Send calls — clean empty stream.
	return nil
}

func TestEmptyStreamProducesOnlyEndEnvelope(t *testing.T) {
	srv := streamServer(t, emptyStreamServicer{})
	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	var body bytes.Buffer
	require.NoError(t, writeConnectEnvelope(&body, 0, []byte(`{"name":"x"}`)))

	httpReq, _ := http.NewRequestWithContext(t.Context(), "POST",
		ts.URL+"/greet.v1.GreetService/StreamGreet", &body)
	httpReq.Header.Set("Content-Type", connectStreamJSONType)
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	frames := readAllEnvelopes(t, resp.Body)
	require.Len(t, frames, 1, "only the end-stream envelope")
	assert.Equal(t, connectEndStreamFlag, frames[0].flags)
}

func TestEmptyStreamOverMCP(t *testing.T) {
	srv := streamServer(t, emptyStreamServicer{})
	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "greet.v1.GreetService.StreamGreet",
			"arguments": map[string]any{"name": "x"},
		},
	})
	result := resp["result"].(map[string]any)
	assert.Nil(t, result["isError"])
	content := result["content"].([]any)
	assert.Empty(t, content, "no messages = empty content array")
}

type immediateErrServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

func (immediateErrServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "hi " + req.GetName()}, nil
}

func (immediateErrServicer) StreamGreet(_ *greetpb.StreamGreetRequest, _ grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
	return errors.New("nope")
}

func TestStreamErrorBeforeAnyChunk(t *testing.T) {
	srv := streamServer(t, immediateErrServicer{})
	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	var body bytes.Buffer
	require.NoError(t, writeConnectEnvelope(&body, 0, []byte(`{"name":"x"}`)))

	httpReq, _ := http.NewRequestWithContext(t.Context(), "POST",
		ts.URL+"/greet.v1.GreetService/StreamGreet", &body)
	httpReq.Header.Set("Content-Type", connectStreamJSONType)
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	frames := readAllEnvelopes(t, resp.Body)
	require.Len(t, frames, 1, "no message frames, just the end-stream w/ error")
	assert.Equal(t, connectEndStreamFlag, frames[0].flags)
	var end map[string]any
	require.NoError(t, json.Unmarshal(frames[0].payload, &end))
	errObj := end["error"].(map[string]any)
	assert.Equal(t, "unknown", errObj["code"])
	assert.Contains(t, errObj["message"], "nope")
}

// -- Public InvokeStream / Invoke type-mismatch errors. --

func TestInvokeStreamDeliversChunks(t *testing.T) {
	srv := streamServer(t, &streamServicer{})

	var got []string
	err := srv.InvokeStream(t.Context(), "greet.v1.GreetService.StreamGreet",
		&greetpb.StreamGreetRequest{Name: "API", Count: 3},
		func(msg proto.Message) error {
			got = append(got, msg.(*greetpb.GreetResponse).Message)
			return nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"Hi API #0", "Hi API #1", "Hi API #2"}, got)
}

func TestInvokeRejectsStreamingTool(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	_, err := srv.Invoke(t.Context(), "greet.v1.GreetService.StreamGreet",
		&greetpb.StreamGreetRequest{Name: "x"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "InvokeStream")
}

func TestInvokeStreamRejectsUnaryTool(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	err := srv.InvokeStream(t.Context(), "greet.v1.GreetService.Greet",
		&greetpb.GreetRequest{Name: "x"},
		func(proto.Message) error { return nil })
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "Invoke")
}

func TestInvokeStreamUnknownTool(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	err := srv.InvokeStream(t.Context(), "Nope.Nope", nil, func(proto.Message) error { return nil })
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

// -- Connect-Timeout-Ms on streaming endpoints. --

type slowEmittingServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

func (slowEmittingServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "hi " + req.GetName()}, nil
}

func (slowEmittingServicer) StreamGreet(_ *greetpb.StreamGreetRequest, stream grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
	if err := stream.Send(&greetpb.GreetResponse{Message: "hi"}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestStreamingHTTPConnectTimeoutDeadlineExceeded(t *testing.T) {
	srv := streamServer(t, slowEmittingServicer{})
	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	var body bytes.Buffer
	require.NoError(t, writeConnectEnvelope(&body, 0, []byte(`{"name":"X"}`)))

	httpReq, _ := http.NewRequestWithContext(t.Context(), "POST",
		ts.URL+"/greet.v1.GreetService/StreamGreet", &body)
	httpReq.Header.Set("Content-Type", connectStreamJSONType)
	httpReq.Header.Set("Connect-Timeout-Ms", "100")
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	frames := readAllEnvelopes(t, resp.Body)
	require.GreaterOrEqual(t, len(frames), 1)
	last := frames[len(frames)-1]
	assert.Equal(t, connectEndStreamFlag, last.flags)

	var end map[string]any
	require.NoError(t, json.Unmarshal(last.payload, &end))
	errObj := end["error"].(map[string]any)
	assert.Equal(t, "deadline_exceeded", errObj["code"])
}

// -- Outbound HTTP response size cap (connect_http proxy mode). --

func TestConnectHTTPRejectsOversizedUpstreamResponse(t *testing.T) {
	// Mock an upstream that returns way more bytes than our cap permits.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/greet/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		// Build a JSON payload that, even before parsing, exceeds the cap.
		filler := strings.Repeat("x", httpClientMaxResponseBytes+1024)
		_, _ = w.Write([]byte(`{"message":"` + filler + `"}`))
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	mockSrv := &http.Server{Handler: mux}
	go func() { _ = mockSrv.Serve(lis) }()
	defer func() { _ = mockSrv.Close() }()

	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	require.NoError(t, srv.ConnectHTTP("http://"+lis.Addr().String()))

	_, err = srv.Invoke(t.Context(), "greet.v1.GreetService.Greet", &greetpb.GreetRequest{Name: "x"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.ResourceExhausted, st.Code())
	assert.Contains(t, st.Message(), "exceeds")
}

// -- Connect envelope max-size guard. --

func TestStreamRejectsOversizedRequestEnvelope(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Forge a header that claims the data is bigger than the cap.
	var header [5]byte
	header[0] = 0
	size := uint32(connectStreamMaxRequest + 1)
	header[1] = byte(size >> 24)
	header[2] = byte(size >> 16)
	header[3] = byte(size >> 8)
	header[4] = byte(size)

	httpReq, _ := http.NewRequestWithContext(t.Context(), "POST",
		ts.URL+"/greet.v1.GreetService/StreamGreet", bytes.NewReader(header[:]))
	httpReq.Header.Set("Content-Type", connectStreamJSONType)
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	if got := resp.Header.Get("Content-Type"); got != connectStreamJSONType {
		t.Errorf("Content-Type = %q, want %q", got, connectStreamJSONType)
	}

	frames := readAllEnvelopes(t, resp.Body)
	require.Len(t, frames, 1)
	assert.Equal(t, connectEndStreamFlag, frames[0].flags)
	var end struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(frames[0].payload, &end))
	assert.Equal(t, "resource_exhausted", end.Error.Code)
}
