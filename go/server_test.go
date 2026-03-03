package invariant

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testGreetServicer struct{}

func (s *testGreetServicer) Greet(_, _ any) any      { return nil }
func (s *testGreetServicer) GreetGroup(_, _ any) any { return nil }

func registeredServer(t *testing.T) *Server {
	t.Helper()
	srv := newServer(mustParse(t))
	require.NoError(t, srv.Register(&testGreetServicer{}))
	return srv
}

func TestRegisterCreatesTools(t *testing.T) {
	assert.Len(t, registeredServer(t).tools, 2)
}

func TestToolNames(t *testing.T) {
	srv := registeredServer(t)
	assert.Contains(t, srv.tools, "GreetService.Greet")
	assert.Contains(t, srv.tools, "GreetService.GreetGroup")
}

func TestToolDescription(t *testing.T) {
	tool := registeredServer(t).tools["GreetService.Greet"]
	assert.Contains(t, strings.ToLower(tool.Description), "greet a person")
}

func TestToolInputSchema(t *testing.T) {
	tool := registeredServer(t).tools["GreetService.Greet"]
	assert.Equal(t, "object", tool.InputSchema["type"])
}

func TestRegisterExplicitServiceName(t *testing.T) {
	srv := newServer(mustParse(t))
	require.NoError(t, srv.Register(&testGreetServicer{}, "greet.v1.GreetService"))
	assert.Len(t, srv.tools, 2)
}

func TestRegisterUnknownService(t *testing.T) {
	srv := newServer(mustParse(t))
	assert.Error(t, srv.Register(&testGreetServicer{}, "does.not.ExistService"))
}

type noMethodServicer struct{}

func TestRegisterNoMatchingService(t *testing.T) {
	srv := newServer(mustParse(t))
	assert.Error(t, srv.Register(&noMethodServicer{}))
}
