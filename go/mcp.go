package invariant

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const mcpProtocolVersion = "2024-11-05"

type mcpSession struct {
	server *Server
	r      io.Reader
	w      io.Writer
	mu     sync.Mutex
	// inflight tracks per-request cancel funcs so notifications/canceled
	// can interrupt long-running tools/call invocations.
	inflightMu sync.Mutex
	inflight   map[string]context.CancelFunc
}

func (s *Server) newMCPSession(r io.Reader, w io.Writer) *mcpSession {
	return &mcpSession{server: s, r: r, w: w, inflight: make(map[string]context.CancelFunc)}
}

// serveMCP runs the MCP server over stdin/stdout (blocking).
func (s *Server) serveMCP(ctx context.Context) error {
	return s.newMCPSession(os.Stdin, os.Stdout).run(ctx)
}

func (m *mcpSession) run(ctx context.Context) error {
	scanner := bufio.NewScanner(m.r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var wg sync.WaitGroup
	defer func() {
		// On stdin EOF, wait for in-flight tools/call to finish so callers see
		// their responses. Cancellation only happens when the parent ctx is done
		// (process shutdown), handled in the loop below.
		wg.Wait()
	}()

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			// Parent shutdown — cancel everything in flight, then wait.
			m.inflightMu.Lock()
			for _, cancel := range m.inflight {
				cancel()
			}
			m.inflightMu.Unlock()
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			m.writeResponse(mcpErr(nil, -32700, "Parse error: "+err.Error()))
			continue
		}

		// Notifications execute synchronously — cancellation must take effect
		// before the next request is read off the wire.
		if req.ID == nil {
			m.handleNotification(&req)
			continue
		}

		// tools/call is the only method that can block (user handler) —
		// dispatch it concurrently so notifications/canceled can interrupt
		// it. Fast metadata methods (initialize, tools/list, ping) run inline
		// to keep response order deterministic.
		if req.Method != "tools/call" {
			if resp := m.dispatch(ctx, &req); resp != nil {
				m.writeResponse(resp)
			}
			continue
		}

		// Register the cancel func synchronously *before* starting the goroutine
		// so a notifications/canceled arriving on the next read always finds it.
		callCtx, cancel := context.WithCancel(ctx) //nolint:gosec // cancel stored in inflight map and invoked via defer below
		idKey := string(req.ID)
		m.inflightMu.Lock()
		m.inflight[idKey] = cancel
		m.inflightMu.Unlock()

		reqCopy := req
		wg.Go(func() {
			defer func() {
				cancel()
				m.inflightMu.Lock()
				delete(m.inflight, idKey)
				m.inflightMu.Unlock()
			}()
			resp := m.dispatch(callCtx, &reqCopy)
			if resp != nil {
				m.writeResponse(resp)
			}
		})
	}
	return scanner.Err()
}

func (m *mcpSession) writeResponse(resp *jsonRPCResponse) {
	out, err := json.Marshal(resp)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, _ = m.w.Write(out)
	_, _ = m.w.Write([]byte("\n"))
	if f, ok := m.w.(flusher); ok {
		_ = f.Flush()
	}
}

func (m *mcpSession) handleNotification(req *jsonRPCRequest) {
	if req.Method != "notifications/canceled" {
		return
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || len(p.RequestID) == 0 {
		return
	}
	m.inflightMu.Lock()
	cancel := m.inflight[string(p.RequestID)]
	m.inflightMu.Unlock()
	if cancel != nil {
		cancel()
	}
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
	return m.server.mcpDispatch(ctx, req)
}

// mcpDispatch routes a single JSON-RPC request through MCP method handling.
// Used by both the stdio session loop and the HTTP /mcp transport.
// Returns nil for notifications (req.ID == nil).
func (s *Server) mcpDispatch(ctx context.Context, req *jsonRPCRequest) *jsonRPCResponse {
	if req.ID == nil {
		return nil
	}
	switch req.Method {
	case "initialize":
		return mcpOK(req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		})
	case "tools/list":
		return mcpOK(req.ID, map[string]any{"tools": s.ToolCatalog()})
	case "tools/call":
		return s.toolsCall(ctx, req.ID, req.Params)
	case "ping":
		return mcpOK(req.ID, map[string]any{})
	default:
		return mcpErr(req.ID, -32601, "Method not found: "+req.Method)
	}
}

func (s *Server) toolsCall(ctx context.Context, id, rawParams json.RawMessage) *jsonRPCResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return mcpErr(id, -32602, "Invalid params: "+err.Error())
	}

	tool, ok := s.tools[p.Name]
	if !ok {
		return mcpErr(id, -32602, "Unknown tool: "+p.Name)
	}

	// Cancellation is wired up by run() before this goroutine started, so ctx
	// already carries the per-request cancel.
	if tool.ServerStreaming {
		return s.toolsCallStream(ctx, id, tool, p.Arguments)
	}

	text, err := s.invokeJSON(ctx, tool, p.Arguments)
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

// toolsCallStream runs a server-streaming tool and collects each chunk into
// the content array — one text block per emitted message. Errors short-circuit
// and return an isError result with whatever chunks were already produced.
func (s *Server) toolsCallStream(ctx context.Context, id json.RawMessage, tool *Tool, argsJSON json.RawMessage) *jsonRPCResponse {
	req, err := s.newRequest(tool)
	if err != nil {
		return mcpOK(id, errorContent(err))
	}
	if len(argsJSON) > 0 && string(argsJSON) != "null" {
		if err := protojson.Unmarshal(argsJSON, req); err != nil {
			return mcpOK(id, errorContent(invalidArgumentFromJSONError(err)))
		}
	}

	marshalOpts := protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}
	var content []any
	stream := newCallbackStream(ctx, func(msg proto.Message) error {
		raw, err := marshalOpts.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal stream chunk: %w", err)
		}
		content = append(content, map[string]any{"type": "text", "text": string(raw)})
		return nil
	})
	defer stream.close()

	if err := s.invokeStream(tool, req, stream); err != nil {
		// Include any chunks that were already emitted before the error.
		payload := errorPayload(err)
		content = append(content, map[string]any{"type": "text", "text": errorMessage(err)})
		return mcpOK(id, map[string]any{
			"content": content,
			"isError": true,
			"error":   payload,
		})
	}

	if len(content) == 0 {
		content = []any{}
	}
	return mcpOK(id, map[string]any{"content": content})
}

// errorContent builds the standard MCP error content envelope from an error.
func errorContent(err error) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": errorMessage(err)}},
		"isError": true,
		"error":   errorPayload(err),
	}
}

// invoke is the core proto-in/proto-out dispatch.
//
// Each projection converts at its boundary:
//   - MCP, HTTP: JSON → proto → invoke → proto → JSON  (via invokeJSON())
//   - gRPC:           proto(dynamic) → invoke → proto → proto(dynamic)
//
// The per-tool invoke handler (cached at addTool time) handles the
// dynamicpb→typed conversion for reflected handlers, using a binary roundtrip
// when proto names match (~10x faster than JSON) and JSON fallback otherwise.
func (s *Server) invoke(ctx context.Context, tool *Tool, req proto.Message) (proto.Message, error) {
	resp, err := s.chainedInvoke(ctx, req, tool.callInfo, tool.invokeHandler)
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
	req, err := s.newRequest(tool)
	if err != nil {
		return "", err
	}
	if len(argsJSON) > 0 && string(argsJSON) != "null" {
		if err := protojson.Unmarshal(argsJSON, req); err != nil {
			return "", invalidArgumentFromJSONError(err)
		}
	}

	resp, err := s.invoke(ctx, tool, req)
	if err != nil {
		return "", err
	}
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
//
// Panics inside any interceptor or the handler are recovered and converted
// to a codes.Internal status error — a single goroutine bug must not be
// allowed to crash the whole server.
func (s *Server) chainedInvoke(ctx context.Context, req any, info *ServerCallInfo, handler UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = status.Errorf(codes.Internal, "panic in %s: %v", info.FullMethod, r)
		}
	}()

	if len(s.interceptors) == 0 {
		return handler(ctx, req)
	}

	// Build chain from inside out: wrap handler with interceptors in reverse order.
	current := handler
	for _, interceptor := range slices.Backward(s.interceptors) {
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
