package invariant

import (
	"bufio"
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

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// streamServicer covers Greet, GreetGroup, and StreamGreet so streaming tests
// can drive the same Server instance used by other projections.
type streamServicer struct {
	greetpb.UnimplementedGreetServiceServer
	preSendErr error // optional: emit n chunks then fail
}

func (s *streamServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "Hello " + req.GetName()}, nil
}

func (s *streamServicer) GreetGroup(_ context.Context, req *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	var msgs []string
	for _, p := range req.GetPeople() {
		msgs = append(msgs, "Hi "+p.GetName())
	}
	return &greetpb.GreetGroupResponse{Messages: msgs, Count: int32(len(msgs))}, nil
}

func (s *streamServicer) StreamGreet(req *greetpb.StreamGreetRequest, stream grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
	n := int(req.GetCount())
	if n <= 0 {
		n = 1
	}
	for i := range n {
		if s.preSendErr != nil && i == n/2 {
			return s.preSendErr
		}
		if err := stream.Send(&greetpb.GreetResponse{
			Message: fmt.Sprintf("Hi %s #%d", req.GetName(), i),
		}); err != nil {
			return err
		}
	}
	return nil
}

func streamServer(t *testing.T, service greetpb.GreetServiceServer) *Server {
	t.Helper()
	full, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(full, service)
	return full
}

// -- Direct dispatch --

func TestStreamRegistrationFlagsTool(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	tool, ok := srv.tools["greet.v1.GreetService.StreamGreet"]
	require.True(t, ok, "StreamGreet should register as a tool")
	assert.True(t, tool.ServerStreaming)
	assert.NotNil(t, tool.streamDesc)
	assert.Nil(t, tool.invokeHandler)
}

func TestToolCatalogMarksStreamingTools(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	catalog := srv.ToolCatalog()

	byName := make(map[string]map[string]any, len(catalog))
	for _, entry := range catalog {
		byName[entry["name"].(string)] = entry
	}

	stream := byName["greet.v1.GreetService.StreamGreet"]
	require.NotNil(t, stream)
	meta := stream["_meta"].(map[string]any)
	assert.Equal(t, true, meta["streaming"])

	// Unary tools intentionally have no _meta so the wire shape stays compact.
	unary := byName["greet.v1.GreetService.Greet"]
	require.NotNil(t, unary)
	_, hasMeta := unary["_meta"]
	assert.False(t, hasMeta, "unary tools should not carry _meta")
}

func TestStreamInvocationCollectsAllChunks(t *testing.T) {
	srv := streamServer(t, &streamServicer{})

	var got []string
	err := srv.InvokeStream(t.Context(), "greet.v1.GreetService.StreamGreet", &greetpb.StreamGreetRequest{Name: "Alice", Count: 3}, func(msg proto.Message) error {
		resp := msg.(*greetpb.GreetResponse)
		got = append(got, resp.Message)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"Hi Alice #0", "Hi Alice #1", "Hi Alice #2"}, got)
}

func TestStreamInvocationRequiresTheRegisteredRequestType(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	sent := false
	err := srv.InvokeStream(
		t.Context(),
		"greet.v1.GreetService.StreamGreet",
		&greetpb.GreetRequest{Name: "wrong protobuf type"},
		func(proto.Message) error {
			sent = true
			return nil
		},
	)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "greet.v1.GreetRequest")
	assert.Contains(t, err.Error(), "greet.v1.StreamGreetRequest")
	assert.False(t, sent)
}

func TestStreamInterceptorWraps(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	var saw atomic.Int32
	srv.UseStream(func(service any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		saw.Add(1)
		assert.Equal(t, "/greet.v1.GreetService/StreamGreet", info.FullMethod)
		return handler(service, stream)
	})

	var got []string
	err := srv.InvokeStream(t.Context(), "greet.v1.GreetService.StreamGreet", &greetpb.StreamGreetRequest{Name: "B", Count: 2}, func(msg proto.Message) error {
		got = append(got, msg.(*greetpb.GreetResponse).Message)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), saw.Load())
	assert.Len(t, got, 2)
}

func TestStreamHandlerErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	srv := streamServer(t, &streamServicer{preSendErr: boom})

	var got []string
	err := srv.InvokeStream(t.Context(), "greet.v1.GreetService.StreamGreet", &greetpb.StreamGreetRequest{Name: "x", Count: 4}, func(msg proto.Message) error {
		got = append(got, msg.(*greetpb.GreetResponse).Message)
		return nil
	})
	require.ErrorIs(t, err, boom)
	// preSendErr fires at i == n/2 (= 2 when n=4), so chunks 0..1 land before the error.
	assert.Len(t, got, 2)
}

// -- MCP projection --

func TestStreamingMCPCollectsToContent(t *testing.T) {
	srv := streamServer(t, &streamServicer{})

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "greet.v1.GreetService.StreamGreet",
			"arguments": map[string]any{"name": "Alice", "count": 3},
		},
	})

	result := resp["result"].(map[string]any)
	assert.Nil(t, result["isError"])
	content := result["content"].([]any)
	require.Len(t, content, 3)
	for i, raw := range content {
		block := raw.(map[string]any)
		assert.Equal(t, "text", block["type"])
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(block["text"].(string)), &parsed))
		assert.Equal(t, fmt.Sprintf("Hi Alice #%d", i), parsed["message"])
	}
}

func TestStreamingMCPSurfacesMidStreamError(t *testing.T) {
	srv := streamServer(t, &streamServicer{preSendErr: errors.New("mid-stream failure")})

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "greet.v1.GreetService.StreamGreet",
			"arguments": map[string]any{"name": "x", "count": 4},
		},
	})

	result := resp["result"].(map[string]any)
	assert.Equal(t, true, result["isError"])
	content := result["content"].([]any)
	require.GreaterOrEqual(t, len(content), 1)
	// Last item is the error text block.
	last := content[len(content)-1].(map[string]any)
	assert.Contains(t, last["text"], "mid-stream failure")
}

// -- CLI projection --

func TestStreamingCLIWritesNDJSON(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	var interceptorCalls atomic.Int32
	srv.UseStream(func(service any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		interceptorCalls.Add(1)
		assert.Equal(t, "/greet.v1.GreetService/StreamGreet", info.FullMethod)
		return handler(service, stream)
	})

	out, err := srv.cli(t.Context(), []string{"greet.v1.GreetService", "StreamGreet", "-r", `{"name":"Z","count":2}`})
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 2)
	for i, line := range lines {
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &parsed), "line %d not JSON: %q", i, line)
		assert.Equal(t, fmt.Sprintf("Hi Z #%d", i), parsed["message"])
	}
	assert.Equal(t, int32(1), interceptorCalls.Load())
}

// TestStreamingCLIFlushesPerChunk verifies that cliWrite emits each chunk to
// the writer immediately rather than buffering everything until the stream
// ends — piping a long-running stream through CLI must not feel frozen.
//
// The proof: chunk 1 reaches the reader, the test releases `gate`, then chunk
// 2 reaches the reader. If cliWrite buffered, chunk 1 would never arrive
// because gate would never be released.
func TestStreamingCLIFlushesPerChunk(t *testing.T) {
	gate := make(chan struct{})
	srv := streamServer(t, &gatedServicer{gate: gate})

	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()

	done := make(chan error, 1)
	go func() {
		done <- srv.cliWrite(t.Context(), []string{"greet.v1.GreetService", "StreamGreet", "-r", `{"name":"X","count":2}`}, pw)
		_ = pw.Close()
	}()

	scanner := bufio.NewScanner(pr)
	require.True(t, scanner.Scan(), "first chunk did not flush")
	var chunk1 map[string]any
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &chunk1))
	assert.Equal(t, "Hi X #0", chunk1["message"])

	// Releasing the gate is what allows chunk 2 to be produced — proving the
	// handler was actively waiting (i.e. chunk 1 had already been flushed and
	// read) when we got here.
	close(gate)
	require.True(t, scanner.Scan())
	var chunk2 map[string]any
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &chunk2))
	assert.Equal(t, "Hi X #1", chunk2["message"])

	require.NoError(t, <-done)
}

// gatedServicer emits one chunk, then blocks on `gate` before emitting the
// second. Used by the per-chunk flush test to prove the consumer sees chunk
// 1 before chunk 2 is even produced.
type gatedServicer struct {
	greetpb.UnimplementedGreetServiceServer
	gate chan struct{}
}

func (g *gatedServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	return &greetpb.GreetResponse{Message: "hi " + req.Name}, nil
}

func (g *gatedServicer) StreamGreet(req *greetpb.StreamGreetRequest, stream grpc.ServerStreamingServer[greetpb.GreetResponse]) error {
	if err := stream.Send(&greetpb.GreetResponse{Message: "Hi " + req.GetName() + " #0"}); err != nil {
		return err
	}
	select {
	case <-g.gate:
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	return stream.Send(&greetpb.GreetResponse{Message: "Hi " + req.GetName() + " #1"})
}

// -- HTTP / Connect projection --

func TestStreamingHTTPConnectEnvelopes(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	handler := srv.HTTPHandler()

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Build one request envelope wrapping the JSON request.
	reqJSON := []byte(`{"name":"K","count":3}`)
	var body bytes.Buffer
	require.NoError(t, writeConnectEnvelope(&body, 0, reqJSON))

	httpReq, _ := http.NewRequestWithContext(t.Context(), "POST", ts.URL+"/greet.v1.GreetService/StreamGreet", &body)
	httpReq.Header.Set("Content-Type", connectStreamJSONType)
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	// Compare against the constant — testifylint mistakes "+json" for an encoded payload.
	assert.Equal(t, connectStreamJSONType, resp.Header.Get("Content-Type")) //nolint:testifylint

	frames := readAllEnvelopes(t, resp.Body)
	require.Len(t, frames, 4, "3 message frames + 1 end-stream")

	// First three are message frames.
	for i := range 3 {
		assert.Equal(t, byte(0), frames[i].flags, "frame %d should be a message", i)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal(frames[i].payload, &parsed))
		assert.Equal(t, fmt.Sprintf("Hi K #%d", i), parsed["message"])
	}
	// Last is end-of-stream marker.
	assert.Equal(t, connectEndStreamFlag, frames[3].flags)
	var endPayload map[string]any
	require.NoError(t, json.Unmarshal(frames[3].payload, &endPayload))
	_, hasErr := endPayload["error"]
	assert.False(t, hasErr, "end-stream should not carry error on success")
}

func TestStreamingHTTPRejectsNonStreamContentType(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	httpReq, _ := http.NewRequestWithContext(t.Context(), "POST", ts.URL+"/greet.v1.GreetService/StreamGreet",
		strings.NewReader(`{"name":"K","count":1}`))
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
	var envelope map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&envelope))
	assert.Equal(t, "invalid_argument", envelope["code"])
	assert.Contains(t, envelope["message"], connectStreamJSONType)
}

func TestStreamingHTTPSurfacesErrorAsEndStream(t *testing.T) {
	srv := streamServer(t, &streamServicer{preSendErr: errors.New("kapow")})
	handler := srv.HTTPHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	var body bytes.Buffer
	require.NoError(t, writeConnectEnvelope(&body, 0, []byte(`{"name":"K","count":4}`)))

	httpReq, _ := http.NewRequestWithContext(t.Context(), "POST", ts.URL+"/greet.v1.GreetService/StreamGreet", &body)
	httpReq.Header.Set("Content-Type", connectStreamJSONType)
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	frames := readAllEnvelopes(t, resp.Body)
	require.GreaterOrEqual(t, len(frames), 1)
	last := frames[len(frames)-1]
	assert.Equal(t, connectEndStreamFlag, last.flags)
	var endPayload map[string]any
	require.NoError(t, json.Unmarshal(last.payload, &endPayload))
	errObj := endPayload["error"].(map[string]any)
	assert.Equal(t, "unknown", errObj["code"]) // boom is a plain error → Unknown
	assert.Contains(t, errObj["message"], "kapow")
}

// -- gRPC projection --

func TestStreamingGRPCNative(t *testing.T) {
	srv := streamServer(t, &streamServicer{})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	stream, err := greetpb.NewGreetServiceClient(conn).StreamGreet(
		t.Context(),
		&greetpb.StreamGreetRequest{Name: "Gee", Count: 3},
	)
	require.NoError(t, err)

	var msgs []string
	for {
		out, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("recv: %v", err)
		}
		msgs = append(msgs, out.GetMessage())
	}
	assert.Equal(t, []string{"Hi Gee #0", "Hi Gee #1", "Hi Gee #2"}, msgs)
}

func TestStreamingGRPCErrorBecomesStatusError(t *testing.T) {
	srv := streamServer(t, &streamServicer{preSendErr: status.Error(codes.FailedPrecondition, "nope")})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	stream, err := greetpb.NewGreetServiceClient(conn).StreamGreet(
		t.Context(),
		&greetpb.StreamGreetRequest{Name: "X", Count: 4},
	)
	require.NoError(t, err)

	var lastErr error
	for {
		if _, err := stream.Recv(); err != nil {
			lastErr = err
			break
		}
	}
	st, ok := status.FromError(lastErr)
	require.True(t, ok, "expected gRPC status, got %T", lastErr)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "nope")
}

// -- helpers --

type connectFrame struct {
	flags   byte
	payload []byte
}

func readAllEnvelopes(t *testing.T, r io.Reader) []connectFrame {
	t.Helper()
	var out []connectFrame
	for {
		var hdr [5]byte
		_, err := io.ReadFull(r, hdr[:])
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		size := uint32(hdr[1])<<24 | uint32(hdr[2])<<16 | uint32(hdr[3])<<8 | uint32(hdr[4])
		buf := make([]byte, size)
		if size > 0 {
			_, err = io.ReadFull(r, buf)
			require.NoError(t, err)
		}
		out = append(out, connectFrame{flags: hdr[0], payload: buf})
		if hdr[0]&connectEndStreamFlag != 0 {
			break
		}
	}
	return out
}
