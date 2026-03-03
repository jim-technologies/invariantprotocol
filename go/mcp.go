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
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
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
	tools := make([]map[string]any, 0, len(m.server.tools))
	for _, t := range m.server.tools {
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

	text, err := m.server.invoke(ctx, tool, p.Arguments)
	if err != nil {
		return mcpOK(id, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": err.Error()}},
			"isError": true,
		})
	}

	return mcpOK(id, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
	})
}

// invoke deserializes JSON args into a proto request, runs the interceptor
// chain, calls the handler, and serializes the response back to JSON.
//
// Unlike the Python implementation which is proto-in/proto-out (each projection
// handles its own serialization), this method is JSON-in/JSON-out centralizing
// all serialization logic.
func (s *Server) invoke(ctx context.Context, tool *Tool, argsJSON json.RawMessage) (string, error) {
	// Phase 1: Deserialize JSON → proto message + build inner handler.
	var req proto.Message
	var innerHandler grpc.UnaryHandler

	if dh, ok := tool.Handler.(*grpcDynamicHandler); ok {
		// Dynamic handler path — Connect() proxy.
		dynReq := dynamicpb.NewMessage(dh.reqDesc)
		if len(argsJSON) > 0 && string(argsJSON) != "null" {
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(argsJSON, dynReq); err != nil {
				return "", fmt.Errorf("unmarshal request: %w", err)
			}
		}
		req = dynReq
		innerHandler = func(ctx context.Context, req any) (any, error) {
			resp := dynamicpb.NewMessage(dh.respDesc)
			if err := dh.conn.Invoke(ctx, dh.methodPath, req.(proto.Message), resp); err != nil {
				return nil, fmt.Errorf("grpc call %s: %w", dh.methodPath, err)
			}
			return resp, nil
		}
	} else {
		// Reflected handler path — local servicer.
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
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(argsJSON, reqMsg); err != nil {
				return "", fmt.Errorf("unmarshal request: %w", err)
			}
		}
		req = reqMsg
		innerHandler = func(ctx context.Context, r any) (any, error) {
			results := handlerVal.Call([]reflect.Value{
				reflect.ValueOf(ctx),
				reflect.ValueOf(r),
			})
			if !results[1].IsNil() {
				return nil, results[1].Interface().(error)
			}
			return results[0].Interface(), nil
		}
	}

	// Phase 2: Run interceptor chain.
	info := &grpc.UnaryServerInfo{
		FullMethod: fmt.Sprintf("/%s/%s", tool.ServiceFullName, tool.MethodName),
	}
	resp, err := s.chainedInvoke(ctx, req, info, innerHandler)
	if err != nil {
		return "", err
	}

	// Phase 3: Serialize proto response → JSON.
	if resp == nil {
		return "{}", nil
	}

	respMsg, ok := resp.(proto.Message)
	if !ok {
		return "", errors.New("response does not implement proto.Message")
	}

	out, err := (protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}).Marshal(respMsg)
	if err != nil {
		return "", fmt.Errorf("marshal response: %w", err)
	}
	return string(out), nil
}

// chainedInvoke runs the interceptor chain then calls the handler.
// Chain ordering: first registered = outermost (A(B(C(handler)))).
func (s *Server) chainedInvoke(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
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
