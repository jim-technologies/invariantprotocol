package invariant

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	greetpb "github.com/jim-technologies/invariantprotocol/go/tests/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type testGreetServicer struct {
	greetpb.UnimplementedGreetServiceServer
}

func registeredServer(t *testing.T) *Server {
	t.Helper()
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(srv, &testGreetServicer{})
	return srv
}

func TestInvokeUnknownToolReturnsNotFound(t *testing.T) {
	srv := registeredServer(t)
	_, err := srv.Invoke(t.Context(), "Nope.DoesNotExist", nil)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

func TestInvokeRequiresTheRegisteredRequestType(t *testing.T) {
	srv := streamServer(t, &streamServicer{})

	_, err := srv.Invoke(
		t.Context(),
		"greet.v1.GreetService.Greet",
		&greetpb.GreetResponse{Message: "wrong protobuf type"},
	)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "greet.v1.GreetResponse")
	assert.Contains(t, err.Error(), "greet.v1.GreetRequest")

	// The same protobuf identity from an isolated descriptor pool remains valid:
	// protobuf wire conversion bridges it to the generated request type.
	descriptor, err := findMessageDescriptor(srv.protoFiles, "greet.v1.GreetRequest")
	require.NoError(t, err)
	dynamicRequest := dynamicpb.NewMessage(descriptor)
	dynamicRequest.Set(
		descriptor.Fields().ByName(protoreflect.Name("name")),
		protoreflect.ValueOfString("Dynamic"),
	)
	response, err := srv.Invoke(t.Context(), "greet.v1.GreetService.Greet", dynamicRequest)
	require.NoError(t, err)
	assert.Equal(t, "Hello Dynamic", response.(*greetpb.GreetResponse).GetMessage())
}

func TestToolSnapshotsDoNotExposeSchemaSlices(t *testing.T) {
	srv := registeredServer(t)

	tools := srv.Tools()
	schema := tools["greet.v1.GreetService.Greet"].InputSchema
	required := schema["required"].([]string)
	required[0] = "mutated"
	properties := schema["properties"].(map[string]any)
	mood := properties["mood"].(map[string]any)
	mood["enum"].([]string)[0] = "MUTATED"

	freshTool := srv.Tools()["greet.v1.GreetService.Greet"].InputSchema
	assert.NotContains(t, freshTool["required"].([]string), "mutated")
	freshProperties := freshTool["properties"].(map[string]any)
	assert.NotContains(t, freshProperties["mood"].(map[string]any)["enum"].([]string), "MUTATED")

	catalog := srv.ToolCatalog()
	catalogSchema := catalog[0]["inputSchema"].(map[string]any)
	catalogSchema["required"].([]string)[0] = "also-mutated"
	assert.NotContains(t, srv.ToolCatalog()[0]["inputSchema"].(map[string]any)["required"].([]string), "also-mutated")
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
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	srv.Include("greet.v1.GreetService.Greet")
	greetpb.RegisterGreetServiceServer(srv, &testGreetServicer{})
	assert.Len(t, srv.tools, 1)
	assert.Contains(t, srv.tools, "greet.v1.GreetService.Greet")
}

func TestExcludeFilter(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	srv.Exclude("*GreetGroup")
	greetpb.RegisterGreetServiceServer(srv, &testGreetServicer{})
	assert.Len(t, srv.tools, 2)
	assert.Contains(t, srv.tools, "greet.v1.GreetService.Greet")
	assert.Contains(t, srv.tools, "greet.v1.GreetService.StreamGreet")
}

func TestIncludeExcludeCombined(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	srv.Include("greet.v1.GreetService.*")
	srv.Exclude("*GreetGroup")
	greetpb.RegisterGreetServiceServer(srv, &testGreetServicer{})
	assert.Len(t, srv.tools, 2)
	assert.Contains(t, srv.tools, "greet.v1.GreetService.Greet")
	assert.Contains(t, srv.tools, "greet.v1.GreetService.StreamGreet")
}

func TestProjectionFiltersFreezeAtFirstRegistration(t *testing.T) {
	srv := registeredServer(t)

	assert.PanicsWithValue(t,
		"invariant: include filters must be configured before service registration",
		func() { srv.Include("*.Greet") },
	)
	assert.PanicsWithValue(t,
		"invariant: exclude filters must be configured before service registration",
		func() { srv.Exclude("*.GreetGroup") },
	)
}

func TestProjectionLimitsResetWithZeroAndRejectNegativeValues(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)

	srv.SetMaxUnaryRequestBytes(1)
	srv.SetMaxUnaryRequestBytes(0)

	assert.PanicsWithValue(
		t,
		"invariant: HTTP unary response limit must be non-negative",
		func() { srv.SetMaxUnaryResponseBytes(-1) },
	)
	assert.PanicsWithValue(
		t,
		"invariant: method byte limits must be non-negative",
		func() {
			srv.ConfigureMethod("/greet.v1.GreetService/Greet", MethodConfig{
				MaxStreamResponseBytes: -1,
			})
		},
	)

	greetpb.RegisterGreetServiceServer(srv, &testGreetServicer{})
	request := httptest.NewRequest(
		http.MethodPost,
		greetpb.GreetService_Greet_FullMethodName,
		strings.NewReader(`{"name":"reset"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.HTTPHandler().ServeHTTP(response, request)
	assert.Equal(t, http.StatusNotImplemented, response.Code)
}

func TestIncludeEnvVar(t *testing.T) {
	t.Setenv("INVARIANT_INCLUDE", "greet.v1.GreetService.Greet")
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(srv, &testGreetServicer{})
	assert.Len(t, srv.tools, 1)
	assert.Contains(t, srv.tools, "greet.v1.GreetService.Greet")
}

func TestExcludeEnvVar(t *testing.T) {
	t.Setenv("INVARIANT_EXCLUDE", "*GreetGroup")
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	greetpb.RegisterGreetServiceServer(srv, &testGreetServicer{})
	assert.Len(t, srv.tools, 2)
	assert.Contains(t, srv.tools, "greet.v1.GreetService.Greet")
	assert.Contains(t, srv.tools, "greet.v1.GreetService.StreamGreet")
}
