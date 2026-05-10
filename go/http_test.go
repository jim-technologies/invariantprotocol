package invariant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// slowGreetServicer is used to exercise Connect-Timeout-Ms handling.
type slowGreetServicer struct{}

func (s *slowGreetServicer) Greet(ctx context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	select {
	case <-time.After(2 * time.Second):
		return &greetpb.GreetResponse{Message: "Hello, " + req.Name}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *slowGreetServicer) GreetGroup(_ context.Context, _ *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	return &greetpb.GreetGroupResponse{}, nil
}

// httpTestServicer implements GreetService RPCs using generated proto types.
type httpTestServicer struct{}

func (s *httpTestServicer) Greet(_ context.Context, req *greetpb.GreetRequest) (*greetpb.GreetResponse, error) {
	resp := &greetpb.GreetResponse{
		Message: "Hello, " + req.Name,
		Tags:    req.Tags,
	}
	if req.Mood != nil {
		resp.Mood = *req.Mood
	}
	return resp, nil
}

func (s *httpTestServicer) GreetGroup(_ context.Context, req *greetpb.GreetGroupRequest) (*greetpb.GreetGroupResponse, error) {
	var messages []string
	for _, p := range req.People {
		messages = append(messages, "Hello, "+p.Name)
	}
	return &greetpb.GreetGroupResponse{
		Messages: messages,
		Count:    int32(len(req.People)),
	}, nil
}

func startHTTPServer(t *testing.T) (port int, cancel context.CancelFunc) {
	t.Helper()
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	require.NoError(t, srv.Register(&httpTestServicer{}))

	handler, err := srv.HTTPHandler()
	require.NoError(t, err)

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(lis) }()

	ctx, cancelFn := context.WithCancel(t.Context())
	go func() {
		<-ctx.Done()
		server.Close()
	}()

	return lis.Addr().(*net.TCPAddr).Port, cancelFn
}

func TestHTTPGreet(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	body := postJSON(t, port, "/greet.v1.GreetService/Greet", map[string]any{"name": "Alice"})
	assert.Contains(t, body, "Hello, Alice")
}

func TestHTTPGreetWithEnumAndTags(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	body := postJSON(t, port, "/greet.v1.GreetService/Greet", map[string]any{
		"name": "Alice",
		"mood": "MOOD_HAPPY",
		"tags": map[string]any{"lang": "en"},
	})

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	assert.Contains(t, data["message"], "Alice")
	assert.Equal(t, "MOOD_HAPPY", data["mood"])
	tags := data["tags"].(map[string]any)
	assert.Equal(t, "en", tags["lang"])
}

func TestHTTPGreetGroup(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	body := postJSON(t, port, "/greet.v1.GreetService/GreetGroup", map[string]any{
		"people": []any{
			map[string]any{"name": "Alice"},
			map[string]any{"name": "Bob"},
		},
	})

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	messages := data["messages"].([]any)
	assert.Equal(t, "Hello, Alice", messages[0])
	assert.Equal(t, "Hello, Bob", messages[1])
}

func TestHTTPMethodNotAllowed(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/greet.v1.GreetService/Greet", port))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 405, resp.StatusCode)
}

func TestHTTPNotFound(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/greet.v1.GreetService/DoesNotExist", port),
		"application/json",
		bytes.NewReader([]byte("{}")),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode)
}

func TestHTTPInvalidJSON(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/greet.v1.GreetService/Greet", port),
		"application/json",
		bytes.NewReader([]byte("not valid json")),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 400, resp.StatusCode)
}

func TestHTTPUnknownFieldRejected(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/greet.v1.GreetService/Greet", port),
		"application/json",
		bytes.NewReader([]byte(`{"name":"Alice","extra":"x"}`)),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 400, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))

	// Connect-style: unwrapped, lowercase code
	assert.Equal(t, "invalid_argument", payload["code"])
	assert.Contains(t, payload["message"], "unknown field")
}

func TestHTTPEmptyBody(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/greet.v1.GreetService/Greet", port),
		"application/json",
		bytes.NewReader(nil),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
}

// postJSON sends a POST with JSON body and returns the response body string.
func postJSON(t *testing.T, port int, path string, body map[string]any) string {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)

	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d%s", port, path),
		"application/json",
		bytes.NewReader(data),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(out)
}

func TestHTTPGreetBinaryProto(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	reqBytes, err := proto.Marshal(&greetpb.GreetRequest{Name: "Binary"})
	require.NoError(t, err)

	httpReq, err := http.NewRequest(
		http.MethodPost,
		fmt.Sprintf("http://localhost:%d/greet.v1.GreetService/Greet", port),
		bytes.NewReader(reqBytes),
	)
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/proto")
	httpReq.Header.Set("Accept", "application/proto")

	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "application/proto", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var out greetpb.GreetResponse
	require.NoError(t, proto.Unmarshal(body, &out))
	assert.Equal(t, "Hello, Binary", out.Message)
}

func TestHTTPConnectTimeoutMsHonored(t *testing.T) {
	// Servicer that sleeps longer than the requested timeout.
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	require.NoError(t, srv.Register(&slowGreetServicer{}))

	handler, err := srv.HTTPHandler()
	require.NoError(t, err)
	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	httpServer := &http.Server{Handler: handler}
	go func() { _ = httpServer.Serve(lis) }()
	defer httpServer.Close()
	port := lis.Addr().(*net.TCPAddr).Port

	body := []byte(`{"name":"World"}`)
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://localhost:%d/greet.v1.GreetService/Greet", port),
		bytes.NewReader(body),
	)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Timeout-Ms", "50")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 504, resp.StatusCode) // DEADLINE_EXCEEDED → HTTP 504

	var payload map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	assert.Equal(t, "deadline_exceeded", payload["code"])
}

func TestHTTPToolCatalog(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/", port))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	var body struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	names := map[string]bool{}
	for _, tool := range body.Tools {
		names[tool.Name] = true
	}
	assert.True(t, names["GreetService.Greet"])
	assert.True(t, names["GreetService.GreetGroup"])
}
