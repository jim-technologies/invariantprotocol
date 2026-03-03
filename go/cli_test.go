package invariant

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cliServer(t *testing.T) *Server {
	t.Helper()
	return mcpServer(t)
}

func TestCLIBasic(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", `{"name":"Alice"}`})
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &data))
	assert.Contains(t, data["message"], "Alice")
}

func TestCLIInlineInvalidJSON(t *testing.T) {
	srv := cliServer(t)
	_, err := srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", "not json"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot parse inline value as JSON")
}

func TestCLINoRequest(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{"GreetService", "Greet"})
	require.NoError(t, err)
	assert.NotEmpty(t, result)
}

func TestCLINoArgs(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), nil)
	require.NoError(t, err)
	assert.Contains(t, result, "Usage:")
	assert.Contains(t, result, "GreetService")
	assert.Contains(t, result, "Greet")
}

func TestCLIHelpFlag(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{"--help"})
	require.NoError(t, err)
	assert.Contains(t, result, "Usage:")
	assert.Contains(t, result, "Available methods:")
}

func TestCLIWithEnumAndTags(t *testing.T) {
	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{
		"GreetService", "Greet", "-r",
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
		"GreetService", "GreetGroup", "-r",
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
	_, err := srv.cli(t.Context(), []string{"GreetService"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected method")
}

func TestCLIUnknownServiceMethod(t *testing.T) {
	srv := cliServer(t)
	_, err := srv.cli(t.Context(), []string{"NoSuch", "Tool"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown service/method")
}

func TestCLIRequestYAMLFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "*.yaml")
	require.NoError(t, err)
	defer os.Remove(f.Name())
	_, err = f.WriteString("name: FileTest\n")
	require.NoError(t, err)
	f.Close()

	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", f.Name()})
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &data))
	assert.Contains(t, data["message"], "FileTest")
}

func TestCLIRequestJSONFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "*.json")
	require.NoError(t, err)
	defer os.Remove(f.Name())
	_, err = f.WriteString(`{"name":"JsonFile"}`)
	require.NoError(t, err)
	f.Close()

	srv := cliServer(t)
	result, err := srv.cli(t.Context(), []string{"GreetService", "Greet", "-r", f.Name()})
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &data))
	assert.Contains(t, data["message"], "JsonFile")
}

func TestCLIMissingRValue(t *testing.T) {
	srv := cliServer(t)
	_, err := srv.cli(t.Context(), []string{"GreetService", "Greet", "-r"})
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
