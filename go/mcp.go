package invariant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"unicode/utf8"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	mcpProtocolVersion = "2025-11-25"
	mcpMaxSafeInteger  = int64(1<<53 - 1)
)

type mcpSession struct {
	server *Server
	r      io.Reader
	w      io.Writer
	mu     sync.Mutex
	// inflight tracks per-request cancel funcs so MCP cancellation notifications
	// can interrupt long-running tools/call invocations.
	inflightMu sync.Mutex
	inflight   map[string]*mcpInflightCall
}

type mcpInflightCall struct {
	cancel   context.CancelFunc
	canceled bool
}

func (s *Server) newMCPSession(r io.Reader, w io.Writer) *mcpSession {
	return &mcpSession{server: s, r: r, w: w, inflight: make(map[string]*mcpInflightCall)}
}

// serveMCP runs the MCP server over stdin/stdout (blocking).
func (s *Server) serveMCP(ctx context.Context) error {
	return s.newMCPSession(os.Stdin, os.Stdout).run(ctx)
}

func (m *mcpSession) run(ctx context.Context) error {
	scanner := bufio.NewScanner(m.r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	type scanResult struct {
		line []byte
		err  error
		done bool
	}
	scanned := make(chan scanResult, 1)
	go func() {
		for scanner.Scan() {
			result := scanResult{line: append([]byte(nil), scanner.Bytes()...)}
			select {
			case scanned <- result:
			case <-ctx.Done():
				return
			}
		}
		result := scanResult{err: scanner.Err(), done: true}
		select {
		case scanned <- result:
		case <-ctx.Done():
		}
	}()

	// stdin and pipe readers are closable. Closing them on cancellation also
	// releases the scanner goroutine; the select below still lets sessions over
	// an arbitrary blocking io.Reader return promptly even when it is not.
	stopReaderClose := func() bool { return false }
	if closer, ok := m.r.(io.Closer); ok {
		stopReaderClose = context.AfterFunc(ctx, func() { _ = closer.Close() })
	}
	defer stopReaderClose()

	var wg sync.WaitGroup
	defer func() {
		// On stdin EOF, wait for in-flight tools/call to finish so callers see
		// their responses. Cancellation only happens when the parent ctx is done
		// (process shutdown), handled in the loop below.
		wg.Wait()
	}()

	for {
		var line []byte
		select {
		case <-ctx.Done():
			// Parent shutdown — cancel everything in flight, then wait.
			m.inflightMu.Lock()
			for _, call := range m.inflight {
				call.cancel()
			}
			m.inflightMu.Unlock()
			return ctx.Err()
		case result := <-scanned:
			if result.done {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return result.err
			}
			line = result.line
		}

		if len(line) == 0 {
			continue
		}

		req, clientResponse, protocolError := parseMCPJSONRPC(line)
		if protocolError != nil {
			m.writeResponse(protocolError)
			continue
		}
		if clientResponse {
			continue
		}

		// Notifications execute synchronously — cancellation must take effect
		// before the next request is read off the wire.
		if req.ID == nil {
			m.handleNotification(req)
			continue
		}

		// tools/call is the only method that can block (user handler) —
		// dispatch it concurrently so an MCP cancellation notification can interrupt
		// it. Fast metadata methods (initialize, tools/list, ping) run inline
		// to keep response order deterministic.
		if req.Method != "tools/call" {
			if resp := m.dispatch(ctx, req); resp != nil {
				m.writeResponse(resp)
			}
			continue
		}

		// Register the cancel func synchronously *before* starting the goroutine
		// so a cancellation notification on the next read always finds it.
		callCtx, cancel := context.WithCancel(ctx) //nolint:gosec // cancel stored in inflight map and invoked via defer below
		idKey, ok := mcpIDKey(req.ID)
		if !ok {
			cancel()
			m.writeResponse(mcpErr(nil, -32600, "Invalid Request"))
			continue
		}
		call := &mcpInflightCall{cancel: cancel}
		m.inflightMu.Lock()
		m.inflight[idKey] = call
		m.inflightMu.Unlock()

		reqCopy := *req
		wg.Go(func() {
			defer func() {
				cancel()
			}()
			resp := m.dispatch(callCtx, &reqCopy)
			m.inflightMu.Lock()
			canceled := call.canceled
			delete(m.inflight, idKey)
			m.inflightMu.Unlock()
			if !canceled && resp != nil {
				m.writeResponse(resp)
			}
		})
	}
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
	if req.Method != "notifications/cancelled" { //nolint:misspell // Method name defined by MCP.
		return
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || len(p.RequestID) == 0 {
		return
	}
	idKey, ok := mcpIDKey(p.RequestID)
	if !ok {
		return
	}
	m.inflightMu.Lock()
	call := m.inflight[idKey]
	if call != nil {
		call.canceled = true
	}
	m.inflightMu.Unlock()
	if call != nil {
		call.cancel()
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

// parseMCPJSONRPC distinguishes malformed JSON from a syntactically valid
// value that is not a JSON-RPC 2.0 request. It also accepts client responses,
// which MCP transports may carry in either direction even though Invariant
// does not currently initiate server-to-client requests.
func parseMCPJSONRPC(data []byte) (*jsonRPCRequest, bool, *jsonRPCResponse) {
	if !utf8.Valid(data) || !json.Valid(data) {
		var value any
		err := json.Unmarshal(data, &value)
		message := "Parse error"
		if err != nil {
			message += ": " + err.Error()
		}
		return nil, false, mcpErr(nil, -32700, message)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, false, mcpErr(nil, -32600, "Invalid Request")
	}

	var version string
	rawVersion, hasVersion := fields["jsonrpc"]
	if !hasVersion || json.Unmarshal(rawVersion, &version) != nil || version != "2.0" {
		return nil, false, mcpErr(nil, -32600, "Invalid Request")
	}

	_, hasMethod := fields["method"]
	_, hasResult := fields["result"]
	_, hasError := fields["error"]
	rawID, hasID := fields["id"]
	if !hasMethod {
		switch {
		case hasResult && !hasError:
			if !hasID || !validMCPID(rawID) || !mcpJSONObject(fields["result"]) {
				return nil, false, mcpErr(nil, -32600, "Invalid Request")
			}
		case hasError && !hasResult:
			if (hasID && !validMCPID(rawID)) || !validMCPError(fields["error"]) {
				return nil, false, mcpErr(nil, -32600, "Invalid Request")
			}
		default:
			return nil, false, mcpErr(nil, -32600, "Invalid Request")
		}
		return nil, true, nil
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, false, mcpErr(nil, -32600, "Invalid Request")
	}
	if hasID && !validMCPID(rawID) {
		return nil, false, mcpErr(nil, -32600, "Invalid Request")
	}
	if normalizedID := bytes.TrimSpace(req.ID); hasID && len(normalizedID) > 0 && normalizedID[0] != '"' {
		value, _ := mcpIntegerValue(req.ID)
		req.ID = json.RawMessage(strconv.FormatInt(value, 10))
	}
	if rawParams, hasParams := fields["params"]; hasParams && !mcpJSONObject(rawParams) {
		if !hasID {
			return &req, false, nil
		}
		return nil, false, mcpErr(req.ID, -32602, "Invalid params")
	}
	if req.Method == "tools/call" && !validMCPToolCallParams(fields["params"]) {
		if !hasID {
			return &req, false, nil
		}
		return nil, false, mcpErr(req.ID, -32602, "Invalid params")
	}
	return &req, false, nil
}

func validMCPID(raw json.RawMessage) bool {
	text := string(bytes.TrimSpace(raw))
	if len(text) == 0 || text == "null" || text == "true" || text == "false" {
		return false
	}
	if text[0] == '"' {
		var value string
		return json.Unmarshal(raw, &value) == nil
	}
	return validMCPInteger(raw)
}

func validMCPInteger(raw json.RawMessage) bool {
	_, ok := mcpIntegerValue(raw)
	return ok
}

func mcpIntegerValue(raw json.RawMessage) (int64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if !json.Valid(trimmed) {
		return 0, false
	}
	text := string(trimmed)
	if len(text) == 0 || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < -mcpMaxSafeInteger || value > mcpMaxSafeInteger {
		return 0, false
	}
	return value, true
}

func mcpIDKey(raw json.RawMessage) (string, bool) {
	text := bytes.TrimSpace(raw)
	if !validMCPID(text) {
		return "", false
	}
	if text[0] == '"' {
		var value string
		if err := json.Unmarshal(text, &value); err != nil {
			return "", false
		}
		return "string:" + value, true
	}
	value, ok := mcpIntegerValue(text)
	if !ok {
		return "", false
	}
	return "integer:" + strconv.FormatInt(value, 10), true
}

func mcpJSONObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

func validMCPError(raw json.RawMessage) bool {
	if !mcpJSONObject(raw) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	code, hasCode := fields["code"]
	message, hasMessage := fields["message"]
	if !hasCode || !validMCPInteger(code) || !hasMessage {
		return false
	}
	var text string
	return json.Unmarshal(message, &text) == nil
}

func validMCPToolCallParams(raw json.RawMessage) bool {
	if !mcpJSONObject(raw) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	name, hasName := fields["name"]
	if !hasName {
		return false
	}
	var toolName string
	if json.Unmarshal(name, &toolName) != nil {
		return false
	}
	arguments, hasArguments := fields["arguments"]
	return !hasArguments || mcpJSONObject(arguments)
}

func validMCPInitializeParams(raw json.RawMessage) bool {
	if !mcpJSONObject(raw) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	var protocolVersion string
	if json.Unmarshal(fields["protocolVersion"], &protocolVersion) != nil {
		return false
	}
	if !mcpJSONObject(fields["capabilities"]) || !mcpJSONObject(fields["clientInfo"]) {
		return false
	}
	var clientInfo map[string]json.RawMessage
	if json.Unmarshal(fields["clientInfo"], &clientInfo) != nil {
		return false
	}
	var name, version string
	return json.Unmarshal(clientInfo["name"], &name) == nil &&
		json.Unmarshal(clientInfo["version"], &version) == nil
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
		if !validMCPInitializeParams(req.Params) {
			return mcpErr(req.ID, -32602, "Invalid params")
		}
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
	if len(p.Arguments) == 0 {
		p.Arguments = json.RawMessage("{}")
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
	err = s.invokeStream(ctx, tool, req, func(msg proto.Message) error {
		raw, err := marshalOpts.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal stream chunk: %w", err)
		}
		content = append(content, map[string]any{"type": "text", "text": string(raw)})
		return nil
	})
	if err != nil {
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
// The captured generated gRPC method handler decodes directly into its typed
// request before invoking the registered service implementation.
func (s *Server) invoke(ctx context.Context, tool *Tool, req proto.Message) (proto.Message, error) {
	s.freeze()
	ctx, _ = withProjectionUnaryTransport(ctx, tool.callInfo.FullMethod)
	resp, err := tool.invokeHandler(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
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

// --- Helpers ---

func mcpOK(id json.RawMessage, result any) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func mcpErr(id json.RawMessage, code int, message string) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: message}}
}
