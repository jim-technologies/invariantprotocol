package invariant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func cliServer(t *testing.T) *Server {
	t.Helper()
	return mcpServer(t)
}

func TestCLIBasic(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{"greet.v1.GreetService", "Greet", "-r", `{"name":"Alice"}`})
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &data))
	assert.Contains(t, data["message"], "Alice")
}

func TestCLIInlineInvalidJSON(t *testing.T) {
	srv := cliServer(t)
	_, err := srv.cli(t.Context(), []string{"greet.v1.GreetService", "Greet", "-r", "not json"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot parse inline value as JSON")
}

func TestCLIUnknownFieldRejected(t *testing.T) {
	srv := cliServer(t)
	_, err := srv.cli(t.Context(), []string{"greet.v1.GreetService", "Greet", "-r", `{"name":"Alice","extra":"x"}`})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

func TestCLINoRequest(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{"greet.v1.GreetService", "Greet"})
	require.NoError(t, err)
	assert.NotEmpty(t, result)
}

func TestCLINoArgs(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), nil)
	require.NoError(t, err)
	assert.Contains(t, result, "Usage:")
	assert.Contains(t, result, "greet.v1.GreetService")
	assert.Contains(t, result, "Greet")
}

func TestCLIHelpFlag(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{"--help"})
	require.NoError(t, err)
	assert.Contains(t, result, "Usage:")
	assert.Contains(t, result, "Available methods:")
	assert.Contains(t, result, "name                 string     (required)")
	assert.Contains(t, result, "MOOD_UNSPECIFIED|MOOD_HAPPY|MOOD_SAD")
}

func TestCLIWithEnumAndTags(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{
		"greet.v1.GreetService", "Greet", "-r",
		`{"name":"Alice","mood":"MOOD_HAPPY","tags":{"lang":"en"}}`,
	})
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &data))
	assert.Contains(t, data["message"], "Alice")
	assert.Equal(t, "MOOD_HAPPY", data["mood"])
	tags := data["tags"].(map[string]any)
	assert.Equal(t, "en", tags["lang"])
}

func TestCLIGreetGroup(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{
		"greet.v1.GreetService", "GreetGroup", "-r",
		`{"people":[{"name":"Alice"},{"name":"Bob"}]}`,
	})
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &data))
	messages := data["messages"].([]any)
	assert.Equal(t, "Hello, Alice", messages[0])
	assert.Equal(t, "Hello, Bob", messages[1])
	assert.InEpsilon(t, float64(2), data["count"], 0)
}

func TestCLIMissingMethod(t *testing.T) {
	srv := cliServer(t)
	_, err := srv.cli(t.Context(), []string{"greet.v1.GreetService"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected method")
}

func TestCLIUnknownServiceMethod(t *testing.T) {
	srv := cliServer(t)
	_, err := srv.cli(t.Context(), []string{"NoSuch", "Tool"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown service/method")
}

func TestCLIServiceResolutionIsFullyQualifiedAndCollisionFree(t *testing.T) {
	server := &Server{tools: map[string]*Tool{
		"one.v1.EchoService.Echo": {
			Name:            "one.v1.EchoService.Echo",
			ServiceFullName: "one.v1.EchoService",
			MethodName:      "Echo",
		},
		"two.v1.EchoService.Echo": {
			Name:            "two.v1.EchoService.Echo",
			ServiceFullName: "two.v1.EchoService",
			MethodName:      "Echo",
		},
	}}

	assert.Equal(t, "one.v1.EchoService.Echo", server.resolveServiceMethod("one.v1.EchoService", "Echo"))
	assert.Equal(t, "two.v1.EchoService.Echo", server.resolveServiceMethod("two.v1.EchoService", "Echo"))
	assert.Empty(t, server.resolveServiceMethod("EchoService", "Echo"))
}

func TestCLIRequestUnsupportedExtension(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "*.yaml")
	require.NoError(t, err)
	defer os.Remove(f.Name())
	_, err = f.WriteString("name: FileTest\n")
	require.NoError(t, err)
	f.Close()

	srv := cliServer(t)
	_, err = srv.cli(t.Context(), []string{"greet.v1.GreetService", "Greet", "-r", f.Name()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported request file extension")
}

func TestCLIRequestJSONFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "*.json")
	require.NoError(t, err)
	defer os.Remove(f.Name())
	_, err = f.WriteString(`{"name":"JsonFile"}`)
	require.NoError(t, err)
	f.Close()

	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{"greet.v1.GreetService", "Greet", "-r", f.Name()})
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &data))
	assert.Contains(t, data["message"], "JsonFile")
}

func TestCLIRequestBinaryFiles(t *testing.T) {
	request, err := proto.Marshal(&greetpb.GreetRequest{Name: "BinaryFile"})
	require.NoError(t, err)
	// Preserve an unknown field to prove normal protobuf forward compatibility.
	request = append(request, 0x9a, 0x06, 0x03, 'n', 'e', 'w')

	for _, extension := range []string{".binpb", ".pb"} {
		t.Run(extension, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "request"+extension)
			require.NoError(t, os.WriteFile(path, request, 0o600))

			result, err := cliServer(t).cli(
				t.Context(),
				[]string{"greet.v1.GreetService", "Greet", "-r", path},
			)
			require.NoError(t, err)
			assert.Contains(t, result, "BinaryFile")
		})
	}
}

func TestCLIMalformedBinaryIsInvalidArgument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.binpb")
	require.NoError(t, os.WriteFile(path, []byte{0xff}, 0o600))
	directory := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.Mkdir(directory, 0o700))

	for _, request := range []string{path, directory} {
		_, err := cliServer(t).cli(
			t.Context(),
			[]string{"greet.v1.GreetService", "Greet", "-r", request},
		)
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	}
}

type cliStatusServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

func (*cliStatusServicer) Greet(
	context.Context,
	*greetpb.GreetRequest,
) (*greetpb.GreetResponse, error) {
	return nil, status.Error(codes.FailedPrecondition, "cli status")
}

func TestCLIStatusIsPreserved(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(srv, &cliStatusServicer{})

	_, err = srv.cli(t.Context(), []string{"greet.v1.GreetService", "Greet", "-r", `{"name":"status"}`})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Equal(t, "cli status", status.Convert(err).Message())
}

type cliCancellationServicer struct {
	greetpb.UnimplementedGreetServiceServer
	started  chan struct{}
	canceled chan struct{}
}

func (s *cliCancellationServicer) Greet(
	ctx context.Context,
	_ *greetpb.GreetRequest,
) (*greetpb.GreetResponse, error) {
	close(s.started)
	<-ctx.Done()
	close(s.canceled)
	return nil, status.FromContextError(ctx.Err()).Err()
}

func TestCLICancellationReachesHandler(t *testing.T) {
	service := &cliCancellationServicer{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(srv, service)

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := srv.cli(ctx, []string{"greet.v1.GreetService", "Greet", "-r", `{"name":"cancel"}`})
		result <- err
	}()

	select {
	case <-service.started:
	case <-time.After(time.Second):
		t.Fatal("CLI handler did not start")
	}
	cancel()

	select {
	case <-service.canceled:
	case <-time.After(time.Second):
		t.Fatal("CLI handler did not observe cancellation")
	}
	select {
	case err := <-result:
		require.Error(t, err)
		assert.Equal(t, codes.Canceled, status.Code(err))
	case <-time.After(time.Second):
		t.Fatal("CLI call did not return after cancellation")
	}
}

func TestCLIMissingRValue(t *testing.T) {
	srv := cliServer(t)
	_, err := srv.cli(t.Context(), []string{"greet.v1.GreetService", "Greet", "-r"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing value after -r")
}

func TestSplitCLIArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		service     string
		method      string
		request     string
		expectError bool
	}{
		{
			name:    "basic",
			args:    []string{"Svc", "Method"},
			service: "Svc", method: "Method",
		},
		{
			name:    "with request",
			args:    []string{"Svc", "Method", "-r", `{"a":1}`},
			service: "Svc", method: "Method", request: `{"a":1}`,
		},
		{
			name:        "empty",
			args:        []string{},
			expectError: true,
		},
		{
			name:        "missing method",
			args:        []string{"Svc"},
			expectError: true,
		},
		{
			name:        "missing r value",
			args:        []string{"Svc", "Method", "-r"},
			expectError: true,
		},
		{
			name:        "unexpected argument",
			args:        []string{"Svc", "Method", "extra"},
			expectError: true,
		},
		{
			name:        "trailing argument after request",
			args:        []string{"Svc", "Method", "-r", `{}`, "extra"},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, method, req, err := splitCLIArgs(tt.args)
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.service, svc)
			assert.Equal(t, tt.method, method)
			assert.Equal(t, tt.request, req)
		})
	}
}
