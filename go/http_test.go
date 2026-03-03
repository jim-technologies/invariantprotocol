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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

// httpTestServicer mirrors mcpTestServicer but for HTTP tests.
type httpTestServicer struct{}

func (s *httpTestServicer) Greet(_ context.Context, req *structpb.Struct) (*structpb.Struct, error) {
	fields := req.GetFields()
	out := map[string]any{"message": "Hello, " + fields["name"].GetStringValue()}
	if v, ok := fields["mood"]; ok {
		out["mood"] = v.GetStringValue()
	}
	if v, ok := fields["tags"]; ok && v.GetStructValue() != nil {
		tags := make(map[string]any)
		for k, val := range v.GetStructValue().GetFields() {
			tags[k] = val.GetStringValue()
		}
		out["tags"] = tags
	}
	result, _ := structpb.NewStruct(out)
	return result, nil
}

func (s *httpTestServicer) GreetGroup(_ context.Context, req *structpb.Struct) (*structpb.Struct, error) {
	people := req.GetFields()["people"].GetListValue().GetValues()
	var messages []any
	for _, p := range people {
		name := p.GetStructValue().GetFields()["name"].GetStringValue()
		messages = append(messages, "Hello, "+name)
	}
	result, _ := structpb.NewStruct(map[string]any{
		"messages": messages,
		"count":    float64(len(people)),
	})
	return result, nil
}

func startHTTPServer(t *testing.T) (port int, cancel context.CancelFunc) {
	t.Helper()
	srv := newServer(mustParse(t))
	require.NoError(t, srv.Register(&httpTestServicer{}))

	mux := http.NewServeMux()
	for _, tool := range srv.tools {
		route := "/" + tool.ServiceFullName + "/" + tool.MethodName
		t := tool
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			srv.handleHTTP(w, r, t)
		})
	}

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
