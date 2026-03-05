package invariant

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const mcpProtocolVersion = "2024-11-05"

type mcpSession struct {
	server *Server
	r      io.Reader
	w      io.Writer
	mu     sync.Mutex
}

func (s *Server) newMCPSession(r io.Reader, w io.Writer) *mcpSession {
	return &mcpSession{server: s, r: r, w: w}
}

// serveMCP runs the MCP server over stdin/stdout (blocking).
func (s *Server) serveMCP(ctx context.Context) error {
	return s.newMCPSession(os.Stdin, os.Stdout).run(ctx)
}

func (m *mcpSession) run(ctx context.Context) error {
	scanner := bufio.NewScanner(m.r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		resp := m.dispatch(ctx, &req)
		if resp == nil {
			continue // notification — no response
		}

		out, err := json.Marshal(resp)
		if err != nil {
			continue
		}

		m.mu.Lock()
		_, _ = m.w.Write(out)
		_, _ = m.w.Write([]byte("\n"))
		if f, ok := m.w.(flusher); ok {
			_ = f.Flush()
		}
		m.mu.Unlock()
	}
	return scanner.Err()
}

type flusher interface{ Flush() error }

// --- JSON-RPC types ---

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- Dispatch ---

func (m *mcpSession) dispatch(ctx context.Context, req *jsonRPCRequest) *jsonRPCResponse {
	// Notifications (no id) get no response.
	if req.ID == nil {
		return nil
	}

	switch req.Method {
	case "initialize":
		return mcpOK(req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": m.server.Name, "version": m.server.Version},
		})
	case "tools/list":
		return m.toolsList(req.ID)
	case "tools/call":
		return m.toolsCall(ctx, req.ID, req.Params)
	case "ping":
		return mcpOK(req.ID, map[string]any{})
	default:
		return mcpErr(req.ID, -32601, "Method not found: "+req.Method)
	}
}

func (m *mcpSession) toolsList(id json.RawMessage) *jsonRPCResponse {
	names := make([]string, 0, len(m.server.tools))
	for name := range m.server.tools {
		names = append(names, name)
	}
	slices.Sort(names)

	tools := make([]map[string]any, 0, len(m.server.tools))
	for _, name := range names {
		t := m.server.tools[name]
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return mcpOK(id, map[string]any{"tools": tools})
}

func (m *mcpSession) toolsCall(ctx context.Context, id, rawParams json.RawMessage) *jsonRPCResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return mcpErr(id, -32602, "Invalid params: "+err.Error())
	}

	tool, ok := m.server.tools[p.Name]
	if !ok {
		return mcpErr(id, -32602, "Unknown tool: "+p.Name)
	}

	text, err := m.server.invokeJSON(ctx, tool, p.Arguments)
	if err != nil {
		payload := errorPayload(err)
		return mcpOK(id, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": errorMessage(err)}},
			"isError": true,
			"error":   payload,
		})
	}

	return mcpOK(id, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
	})
}

// invoke is the core proto-in/proto-out dispatch — the equivalent of
// Python's Server._invoke(). Runs the interceptor chain then calls the handler.
//
// Each projection converts at its boundary:
//   - MCP, HTTP: JSON → proto → invoke → proto → JSON  (via invokeJSON())
//   - gRPC:           proto(dynamic) → invoke → proto → proto(dynamic)
//
// For reflected handlers that receive a dynamicpb.Message (e.g. from gRPC),
// the request is converted to the handler's typed proto via binary
// Marshal/Unmarshal (~10x faster than JSON round-trip).
func (s *Server) invoke(ctx context.Context, tool *Tool, req proto.Message) (proto.Message, error) {
	var innerHandler UnaryHandler

	switch h := tool.Handler.(type) {
	case *grpcDynamicHandler:
		innerHandler = func(ctx context.Context, req any) (any, error) {
			return h.callProto(ctx, req.(proto.Message))
		}
	case *httpDynamicHandler:
		innerHandler = func(ctx context.Context, req any) (any, error) {
			return h.callProto(ctx, req.(proto.Message))
		}
	default:
		// Reflected handler path — local servicer.
		handlerVal := reflect.ValueOf(tool.Handler)
		handlerType := handlerVal.Type()

		if handlerType.NumIn() != 2 || handlerType.NumOut() != 2 {
			return nil, fmt.Errorf("handler has unexpected signature (in=%d, out=%d)", handlerType.NumIn(), handlerType.NumOut())
		}

		reqType := handlerType.In(1)

		// Check if the handler's expected type matches the dynamic message type.
		// If they share the same proto full name, binary conversion works (~10x
		// faster). Otherwise fall back to JSON (e.g. when handler uses structpb.Struct).
		handlerReqMsg := reflect.New(reqType.Elem()).Interface().(proto.Message)
		handlerFullName := handlerReqMsg.ProtoReflect().Descriptor().FullName()

		innerHandler = func(ctx context.Context, r any) (any, error) {
			rMsg := r.(proto.Message)

			// If the request is a dynamicpb.Message but the handler expects a
			// typed proto, convert to the handler's type.
			if dynMsg, isDynamic := rMsg.(*dynamicpb.Message); isDynamic {
				typed := reflect.New(reqType.Elem()).Interface().(proto.Message)
				if dynMsg.ProtoReflect().Descriptor().FullName() == handlerFullName {
					// Same proto type — use fast binary conversion.
					b, err := proto.Marshal(rMsg)
					if err != nil {
						return nil, fmt.Errorf("marshal dynamic to binary: %w", err)
					}
					if err := proto.Unmarshal(b, typed); err != nil {
						return nil, fmt.Errorf("unmarshal binary to typed: %w", err)
					}
				} else {
					// Different proto types (e.g. structpb.Struct) — fall back to JSON.
					b, err := protojson.Marshal(rMsg)
					if err != nil {
						return nil, fmt.Errorf("marshal dynamic to JSON: %w", err)
					}
					if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, typed); err != nil {
						return nil, fmt.Errorf("unmarshal JSON to typed: %w", err)
					}
				}
				rMsg = typed
			}

			results := handlerVal.Call([]reflect.Value{
				reflect.ValueOf(ctx),
				reflect.ValueOf(rMsg),
			})
			if !results[1].IsNil() {
				return nil, results[1].Interface().(error)
			}
			return results[0].Interface(), nil
		}
	}

	info := &ServerCallInfo{
		FullMethod: fmt.Sprintf("/%s/%s", tool.ServiceFullName, tool.MethodName),
	}
	resp, err := s.chainedInvoke(ctx, req, info, innerHandler)
	if err != nil {
		return nil, err
	}

	if resp == nil {
		return nil, nil
	}

	respMsg, ok := resp.(proto.Message)
	if !ok {
		return nil, errors.New("response does not implement proto.Message")
	}
	return respMsg, nil
}

// invokeJSON deserializes JSON args into a proto request, calls invoke(),
// and serializes the response back to JSON. Used by MCP and HTTP projections
// (JSON wire boundaries). CLI and gRPC call invoke() directly.
func (s *Server) invokeJSON(ctx context.Context, tool *Tool, argsJSON json.RawMessage) (string, error) {
	// Deserialize JSON → proto message.
	var req proto.Message

	if provider, ok := tool.Handler.(interface {
		requestDescriptor() protoreflect.MessageDescriptor
	}); ok {
		dynReq := dynamicpb.NewMessage(provider.requestDescriptor())
		if len(argsJSON) > 0 && string(argsJSON) != "null" {
			if err := protojson.Unmarshal(argsJSON, dynReq); err != nil {
				return "", invalidArgumentFromJSONError(err)
			}
		}
		req = dynReq
	} else {
		handlerVal := reflect.ValueOf(tool.Handler)
		handlerType := handlerVal.Type()

		if handlerType.NumIn() != 2 || handlerType.NumOut() != 2 {
			return "", fmt.Errorf("handler has unexpected signature (in=%d, out=%d)", handlerType.NumIn(), handlerType.NumOut())
		}

		reqType := handlerType.In(1)
		reqPtr := reflect.New(reqType.Elem())

		reqMsg, ok := reqPtr.Interface().(proto.Message)
		if !ok {
			return "", fmt.Errorf("request type %s does not implement proto.Message", reqType)
		}

		if len(argsJSON) > 0 && string(argsJSON) != "null" {
			if err := protojson.Unmarshal(argsJSON, reqMsg); err != nil {
				return "", invalidArgumentFromJSONError(err)
			}
		}
		req = reqMsg
	}

	// Call proto-first dispatch.
	resp, err := s.invoke(ctx, tool, req)
	if err != nil {
		return "", err
	}

	// Serialize proto response → JSON.
	if resp == nil {
		return "{}", nil
	}

	out, err := (protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}).Marshal(resp)
	if err != nil {
		return "", fmt.Errorf("marshal response: %w", err)
	}
	return string(out), nil
}

// chainedInvoke runs the interceptor chain then calls the handler.
// Chain ordering: first registered = outermost (A(B(C(handler)))).
func (s *Server) chainedInvoke(ctx context.Context, req any, info *ServerCallInfo, handler UnaryHandler) (any, error) {
	if len(s.interceptors) == 0 {
		return handler(ctx, req)
	}

	// Build chain from inside out: wrap handler with interceptors in reverse order.
	current := handler
	for i := len(s.interceptors) - 1; i >= 0; i-- {
		interceptor := s.interceptors[i]
		next := current
		current = func(ctx context.Context, req any) (any, error) {
			return interceptor(ctx, req, info, next)
		}
	}
	return current(ctx, req)
}

// --- Helpers ---

func mcpOK(id json.RawMessage, result any) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func mcpErr(id json.RawMessage, code int, message string) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: message}}
}
