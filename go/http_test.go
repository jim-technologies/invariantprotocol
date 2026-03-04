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

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	bindings, err := srv.buildHTTPBindings()
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		binding, pathParams, methodMismatch := findHTTPBinding(bindings, r.Method, r.URL.Path)
		if binding == nil {
			if methodMismatch {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			http.NotFound(w, r)
			return
		}
		srv.handleHTTP(w, r, binding, pathParams)
	})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	server := &http.Server{Handler: mux}
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

func TestHTTPGreetViaAnnotatedRoute(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/v1/greet/Alice", port))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "Hello, Alice")
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

func TestHTTPGreetGroupViaAnnotatedRoute(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	body := postJSON(t, port, "/v1/greet:group", map[string]any{
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

func TestHTTPGreetGroupViaAdditionalBinding(t *testing.T) {
	port, cancel := startHTTPServer(t)
	defer cancel()

	body := postJSON(t, port, "/v1/group:greet", map[string]any{
		"people": []any{
			map[string]any{"name": "Alice"},
		},
	})

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	assert.Equal(t, "Hello, Alice", data["messages"].([]any)[0])
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

	errObj := payload["error"].(map[string]any)
	assert.Equal(t, "INVALID_ARGUMENT", errObj["code"])
	assert.Contains(t, errObj["message"], "unknown field")
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
