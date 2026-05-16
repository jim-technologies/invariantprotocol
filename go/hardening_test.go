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
	"strings"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// -- HTTP body-size limit: unary requests must reject oversized bodies. --

func TestHTTPUnaryRejectsOversizedBody(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	handler, err := srv.HTTPHandler()
	require.NoError(t, err)
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
	require.GreaterOrEqual(t, resp.StatusCode, 400)
}

// -- application/connect+proto streaming: binary envelopes for performance. --

func TestStreamingHTTPConnectProto(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	handler, err := srv.HTTPHandler()
	require.NoError(t, err)
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
		errc <- srv.Serve(ctx, HTTP(0))
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

// -- Stream edge cases. --

type emptyStreamServicer struct{}

func (emptyStreamServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "hi " + req.GetName()}, nil
}

func (emptyStreamServicer) StreamGreet(_ *greetpb.StreamGreetRequest, _ ServerStream) error {
	// No Send calls — clean empty stream.
	return nil
}

func TestEmptyStreamProducesOnlyEndEnvelope(t *testing.T) {
	srv := streamServer(t, emptyStreamServicer{})
	handler, err := srv.HTTPHandler()
	require.NoError(t, err)
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
			"name":      "GreetService.StreamGreet",
			"arguments": map[string]any{"name": "x"},
		},
	})
	result := resp["result"].(map[string]any)
	assert.Nil(t, result["isError"])
	content := result["content"].([]any)
	assert.Empty(t, content, "no messages = empty content array")
}

type immediateErrServicer struct{}

func (immediateErrServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "hi " + req.GetName()}, nil
}

func (immediateErrServicer) StreamGreet(_ *greetpb.StreamGreetRequest, _ ServerStream) error {
	return errors.New("nope")
}

func TestStreamErrorBeforeAnyChunk(t *testing.T) {
	srv := streamServer(t, immediateErrServicer{})
	handler, err := srv.HTTPHandler()
	require.NoError(t, err)
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
	err := srv.InvokeStream(t.Context(), "GreetService.StreamGreet",
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
	_, err := srv.Invoke(t.Context(), "GreetService.StreamGreet",
		&greetpb.StreamGreetRequest{Name: "x"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "InvokeStream")
}

func TestInvokeStreamRejectsUnaryTool(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	err := srv.InvokeStream(t.Context(), "GreetService.Greet",
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

type slowEmittingServicer struct{}

func (slowEmittingServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "hi " + req.GetName()}, nil
}

func (slowEmittingServicer) StreamGreet(_ *greetpb.StreamGreetRequest, stream ServerStream) error {
	if err := stream.Send(&greetpb.GreetResponse{Message: "hi"}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestStreamingHTTPConnectTimeoutDeadlineExceeded(t *testing.T) {
	srv := streamServer(t, slowEmittingServicer{})
	handler, err := srv.HTTPHandler()
	require.NoError(t, err)
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

	_, err = srv.Invoke(t.Context(), "GreetService.Greet", &greetpb.GreetRequest{Name: "x"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.ResourceExhausted, st.Code())
	assert.Contains(t, st.Message(), "exceeds")
}

// -- Connect envelope max-size guard. --

func TestStreamRejectsOversizedRequestEnvelope(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	handler, err := srv.HTTPHandler()
	require.NoError(t, err)
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
	require.GreaterOrEqual(t, resp.StatusCode, 400)

	body, _ := io.ReadAll(resp.Body)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(body, &envelope))
	assert.Equal(t, "invalid_argument", envelope["code"])
}
