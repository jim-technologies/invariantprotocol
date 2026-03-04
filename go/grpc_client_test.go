package invariant

import (
	"context"
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// greetServicer is a dummy type to satisfy RegisterService's HandlerType check.
type greetServicer any

// startTestGRPCServer starts a gRPC server implementing greet.v1.GreetService
// using dynamic proto messages (no generated Go stubs needed).
func startTestGRPCServer(t *testing.T) (addr string, stop func()) {
	t.Helper()

	data, err := os.ReadFile(descriptorPath())
	require.NoError(t, err)
	var fds descriptorpb.FileDescriptorSet
	require.NoError(t, proto.Unmarshal(data, &fds))
	files, err := protodesc.NewFiles(&fds)
	require.NoError(t, err)

	lookup := func(name string) protoreflect.MessageDescriptor {
		t.Helper()
		d, err := files.FindDescriptorByName(protoreflect.FullName(name))
		require.NoError(t, err)
		return d.(protoreflect.MessageDescriptor)
	}

	reqDesc := lookup("greet.v1.GreetRequest")
	respDesc := lookup("greet.v1.GreetResponse")
	groupReqDesc := lookup("greet.v1.GreetGroupRequest")
	groupRespDesc := lookup("greet.v1.GreetGroupResponse")

	s := grpc.NewServer()
	s.RegisterService(&grpc.ServiceDesc{
		ServiceName: "greet.v1.GreetService",
		HandlerType: (*greetServicer)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "Greet",
				Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					req := dynamicpb.NewMessage(reqDesc)
					if err := dec(req); err != nil {
						return nil, err
					}
					name := req.Get(reqDesc.Fields().ByName("name")).String()

					resp := dynamicpb.NewMessage(respDesc)
					resp.Set(respDesc.Fields().ByName("message"), protoreflect.ValueOfString("Hello, "+name))
					// Echo mood if set
					moodField := reqDesc.Fields().ByName("mood")
					if req.Has(moodField) {
						resp.Set(respDesc.Fields().ByName("mood"), req.Get(moodField))
					}
					// Echo tags if present
					tagsField := reqDesc.Fields().ByName("tags")
					reqTags := req.Get(tagsField).Map()
					if reqTags.Len() > 0 {
						respTags := resp.Mutable(respDesc.Fields().ByName("tags")).Map()
						reqTags.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
							respTags.Set(k, v)
							return true
						})
					}
					return resp, nil
				},
			},
			{
				MethodName: "GreetGroup",
				Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					req := dynamicpb.NewMessage(groupReqDesc)
					if err := dec(req); err != nil {
						return nil, err
					}
					people := req.Get(groupReqDesc.Fields().ByName("people")).List()

					resp := dynamicpb.NewMessage(groupRespDesc)
					msgsList := resp.Mutable(groupRespDesc.Fields().ByName("messages")).List()
					for i := range people.Len() {
						person := people.Get(i).Message()
						name := person.Get(person.Descriptor().Fields().ByName("name")).String()
						msgsList.Append(protoreflect.ValueOfString("Hello, " + name))
					}
					resp.Set(groupRespDesc.Fields().ByName("count"), protoreflect.ValueOfInt32(int32(people.Len())))
					return resp, nil
				},
			},
		},
	}, struct{}{})

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	go func() { _ = s.Serve(lis) }()

	return lis.Addr().String(), s.Stop
}

func connectServer(t *testing.T, target string) *Server {
	t.Helper()
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	require.NoError(t, srv.Connect(target))
	return srv
}

// --- Tests ---

func TestConnectRegistersTools(t *testing.T) {
	addr, stop := startTestGRPCServer(t)
	defer stop()

	srv := connectServer(t, addr)
	defer srv.Stop()

	assert.Len(t, srv.tools, 2)
	assert.Contains(t, srv.tools, "GreetService.Greet")
	assert.Contains(t, srv.tools, "GreetService.GreetGroup")
}

func TestConnectToolSchemaMatches(t *testing.T) {
	addr, stop := startTestGRPCServer(t)
	defer stop()

	remote := connectServer(t, addr)
	defer remote.Stop()

	local := registeredServer(t)

	assert.Equal(t,
		local.tools["GreetService.Greet"].InputSchema,
		remote.tools["GreetService.Greet"].InputSchema,
	)
}

func TestConnectMCPToolsList(t *testing.T) {
	addr, stop := startTestGRPCServer(t)
	defer stop()

	srv := connectServer(t, addr)
	defer srv.Stop()

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	assert.Len(t, tools, 2)
}

func TestConnectMCPToolCall(t *testing.T) {
	addr, stop := startTestGRPCServer(t)
	defer stop()

	srv := connectServer(t, addr)
	defer srv.Stop()

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{"name": "Remote"},
		},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	require.Len(t, content, 1)

	block := content[0].(map[string]any)
	assert.Equal(t, "text", block["type"])
	assert.Contains(t, block["text"], "Hello, Remote")
	assert.Nil(t, result["isError"])
}

func TestConnectMCPToolCallWithEnum(t *testing.T) {
	addr, stop := startTestGRPCServer(t)
	defer stop()

	srv := connectServer(t, addr)
	defer srv.Stop()

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "GreetService.Greet",
			"arguments": map[string]any{
				"name": "World",
				"mood": "MOOD_HAPPY",
				"tags": map[string]any{"lang": "en"},
			},
		},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	require.Len(t, content, 1)

	block := content[0].(map[string]any)
	assert.Contains(t, block["text"], "Hello, World")
	assert.Contains(t, block["text"], "MOOD_HAPPY")
	assert.Contains(t, block["text"], "lang")
}

func TestConnectMCPToolCallGreetGroup(t *testing.T) {
	addr, stop := startTestGRPCServer(t)
	defer stop()

	srv := connectServer(t, addr)
	defer srv.Stop()

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "GreetService.GreetGroup",
			"arguments": map[string]any{
				"people": []any{
					map[string]any{"name": "Alice"},
					map[string]any{"name": "Bob"},
				},
			},
		},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	require.Len(t, content, 1)

	block := content[0].(map[string]any)
	assert.Contains(t, block["text"], "Hello, Alice")
	assert.Contains(t, block["text"], "Hello, Bob")
}

func TestConnectdynamicHandlerDirect(t *testing.T) {
	addr, stop := startTestGRPCServer(t)
	defer stop()

	srv := connectServer(t, addr)
	defer srv.Stop()

	tool := srv.tools["GreetService.Greet"]
	dh, ok := tool.Handler.(*grpcDynamicHandler)
	require.True(t, ok, "handler should be *grpcDynamicHandler")

	result, err := dh.CallJSON(t.Context(), []byte(`{"name": "Direct"}`))
	require.NoError(t, err)
	assert.Contains(t, result, "Hello, Direct")
}

func TestConnectdynamicHandlerDirectRejectsUnknownField(t *testing.T) {
	addr, stop := startTestGRPCServer(t)
	defer stop()

	srv := connectServer(t, addr)
	defer srv.Stop()

	tool := srv.tools["GreetService.Greet"]
	dh, ok := tool.Handler.(*grpcDynamicHandler)
	require.True(t, ok, "handler should be *grpcDynamicHandler")

	_, err := dh.CallJSON(t.Context(), []byte(`{"name":"Direct","extra":"x"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

func TestConnectUnknownService(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	err = srv.Connect("localhost:1", "does.not.ExistService")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestConnectMultipleRequests(t *testing.T) {
	addr, stop := startTestGRPCServer(t)
	defer stop()

	srv := connectServer(t, addr)
	defer srv.Stop()

	resps := sendMultiMCP(t, srv,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
		map[string]any{
			"jsonrpc": "2.0", "id": 3, "method": "tools/call",
			"params": map[string]any{
				"name":      "GreetService.Greet",
				"arguments": map[string]any{"name": "Multi"},
			},
		},
	)
	require.Len(t, resps, 3)
	assert.InEpsilon(t, float64(1), resps[0]["id"], 0)
	assert.InEpsilon(t, float64(2), resps[1]["id"], 0)
	assert.InEpsilon(t, float64(3), resps[2]["id"], 0)

	// Check tool call result
	result := resps[2]["result"].(map[string]any)
	content := result["content"].([]any)
	block := content[0].(map[string]any)
	assert.Contains(t, block["text"], "Hello, Multi")
}

func TestConnectEmptyArgs(t *testing.T) {
	addr, stop := startTestGRPCServer(t)
	defer stop()

	srv := connectServer(t, addr)
	defer srv.Stop()

	resp := sendMCP(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "GreetService.Greet",
			"arguments": map[string]any{},
		},
	})
	result := resp["result"].(map[string]any)
	assert.Nil(t, result["isError"])
}

func TestServerFromDescriptorStoresFDS(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	assert.NotNil(t, srv.fds, "ServerFromDescriptor should store the FileDescriptorSet")
}

func TestServerFromBytesStoresFDS(t *testing.T) {
	data, err := os.ReadFile(descriptorPath())
	require.NoError(t, err)
	srv, err := ServerFromBytes(data)
	require.NoError(t, err)
	assert.NotNil(t, srv.fds, "ServerFromBytes should store the FileDescriptorSet")
}

func TestNewServerNoFDS(t *testing.T) {
	srv := newServer(mustParse(t))
	err := srv.Connect("localhost:1", "greet.v1.GreetService")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ServerFromDescriptor or ServerFromBytes")
}

func TestConnectBuildProtoFiles(t *testing.T) {
	srv, err := ServerFromDescriptor(descriptorPath())
	require.NoError(t, err)
	files, err := srv.buildProtoFiles()
	require.NoError(t, err)

	desc, err := findMessageDescriptor(files, "greet.v1.GreetRequest")
	require.NoError(t, err)
	assert.Equal(t, "GreetRequest", string(desc.Name()))
}
