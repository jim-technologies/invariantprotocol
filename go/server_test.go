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

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
		want    bool
	}{
		{"*", "anything", true},
		{"*", "a.b.c", true},
		{"*.Greet", "greet.v1.GreetService.Greet", true},
		{"*.Greet", "greet.v1.GreetService.GreetGroup", false},
		{"*.Greet*", "greet.v1.GreetService.GreetGroup", true},
		{"greet.v1.GreetService.*", "greet.v1.GreetService.Greet", true},
		{"greet.v1.GreetService.*", "greet.v1.GreetService.GreetGroup", true},
		{"greet.v1.GreetService.*", "other.v1.OtherService.Greet", false},
		{"*Service.Greet", "greet.v1.GreetService.Greet", true},
		{"exact.match", "exact.match", true},
		{"exact.match", "exact.matchx", false},
		{"exact.match", "xexact.match", false},
		{"*Poll*", "temporal.api.workflowservice.v1.WorkflowService.PollWorkflowTaskQueue", true},
		{"*Respond*", "temporal.api.workflowservice.v1.WorkflowService.RespondWorkflowTaskCompleted", true},
		{"", "", true},
		{"", "nonempty", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, globMatch(tt.pattern, tt.input))
		})
	}
}

func TestIncludeFilter(t *testing.T) {
	srv := newServer(mustParse(t))
	srv.Include("greet.v1.GreetService.Greet")
	require.NoError(t, srv.Register(&testGreetServicer{}))
	assert.Len(t, srv.tools, 1)
	assert.Contains(t, srv.tools, "GreetService.Greet")
}

func TestExcludeFilter(t *testing.T) {
	srv := newServer(mustParse(t))
	srv.Exclude("*GreetGroup")
	require.NoError(t, srv.Register(&testGreetServicer{}))
	assert.Len(t, srv.tools, 1)
	assert.Contains(t, srv.tools, "GreetService.Greet")
}

func TestIncludeExcludeCombined(t *testing.T) {
	srv := newServer(mustParse(t))
	srv.Include("greet.v1.GreetService.*")
	srv.Exclude("*GreetGroup")
	require.NoError(t, srv.Register(&testGreetServicer{}))
	assert.Len(t, srv.tools, 1)
	assert.Contains(t, srv.tools, "GreetService.Greet")
}

func TestIncludeEnvVar(t *testing.T) {
	t.Setenv("INVARIANT_INCLUDE", "greet.v1.GreetService.Greet")
	srv := newServer(mustParse(t))
	require.NoError(t, srv.Register(&testGreetServicer{}))
	assert.Len(t, srv.tools, 1)
	assert.Contains(t, srv.tools, "GreetService.Greet")
}

func TestExcludeEnvVar(t *testing.T) {
	t.Setenv("INVARIANT_EXCLUDE", "*GreetGroup")
	srv := newServer(mustParse(t))
	require.NoError(t, srv.Register(&testGreetServicer{}))
	assert.Len(t, srv.tools, 1)
	assert.Contains(t, srv.tools, "GreetService.Greet")
}

func TestNoFilterRegistersAll(t *testing.T) {
	srv := newServer(mustParse(t))
	require.NoError(t, srv.Register(&testGreetServicer{}))
	assert.Len(t, srv.tools, 2)
}
