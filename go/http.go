package invariant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Connect content types.
const (
	jsonContentType        = "application/json"
	protoContentType       = "application/proto"
	connectStreamJSONType  = "application/connect+json"
	connectStreamProtoType = "application/connect+proto"
	connectEndStreamFlag   = byte(0x02)

	// Default body-size safety caps. Per-Server overrides via
	// SetMaxUnaryRequestBytes / SetMaxStreamRequestBytes — useful for apps
	// (e.g. an object store) that legitimately need larger payloads. The
	// defaults stay tight so a misconfigured server doesn't accept
	// arbitrarily large bodies.
	defaultHTTPMaxUnaryRequest      = 16 << 20
	defaultHTTPMaxUnaryResponse     = 16 << 20
	defaultConnectStreamMaxRequest  = 16 << 20
	defaultConnectStreamMaxResponse = 16 << 20
	maxConnectControlEnvelope       = 1 << 20

	// Kept for tests that exercise the default-cap behavior.
	httpMaxUnaryRequest     = defaultHTTPMaxUnaryRequest
	connectStreamMaxRequest = defaultConnectStreamMaxRequest
)

// httpToolEntry caches per-tool state used by the HTTP handler — built once at
// HTTPHandler() time so per-request work stays minimal.
type httpToolEntry struct {
	tool              *Tool
	maxUnaryRequest   int64
	maxUnaryResponse  int64
	maxStreamRequest  int64
	maxStreamResponse int64
}

// HTTPHandler returns an http.Handler that serves all registered tools over the
// Connect protocol. Mount on an existing http.ServeMux or framework router
// instead of binding a separate port:
//
//	mux := http.NewServeMux()
//	h := server.HTTPHandler()
//	mux.Handle("/inv/", http.StripPrefix("/inv", h))
//
// Routes:
//
//	POST /{package.Service}/{Method}      — invoke a tool (Connect protocol)
//	GET  /                                — tool catalog (same shape as MCP tools/list)
//	GET  /__invariant/tools               — tool catalog
//	GET  /__invariant/descriptor.binpb    — raw FileDescriptorSet bytes
func (s *Server) HTTPHandler() http.Handler {
	s.freeze()
	entries := make(map[string]*httpToolEntry, len(s.tools))

	for _, t := range s.tools {
		entry := &httpToolEntry{
			tool:              t,
			maxUnaryRequest:   s.methodUnaryCap(t),
			maxUnaryResponse:  s.methodUnaryResponseCap(t),
			maxStreamRequest:  s.methodStreamCap(t),
			maxStreamResponse: s.methodStreamResponseCap(t),
		}
		entries["/"+t.ServiceFullName+"/"+t.MethodName] = entry
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/__invariant/tools"):
			s.handleToolCatalog(w)
		case r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz"):
			s.handleHealth(w)
		case r.Method == http.MethodGet && r.URL.Path == "/__invariant/descriptor.binpb":
			s.handleDescriptor(w)
		case r.Method == http.MethodPost && r.URL.Path == "/mcp":
			// MCP Streamable HTTP transport — one JSON-RPC message per POST.
			s.handleMCPHTTP(w, r)
		case r.URL.Path == "/mcp":
			if httpHeaderPresent(r.Header, "Origin") {
				http.Error(w, "Origin is not accepted", http.StatusForbidden)
				return
			}
			// This projection intentionally does not offer the optional SSE
			// receive stream used by GET in the full Streamable HTTP transport.
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		case r.Method == http.MethodPost:
			entry, ok := entries[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			s.handleHTTP(w, r, entry)
		default:
			if _, ok := entries[r.URL.Path]; !ok {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	return mux
}

// serveHTTP starts a blocking HTTP server on the given port. Honors ctx for
// graceful shutdown.
func (s *Server) serveHTTP(ctx context.Context, port int) error {
	handler := s.HTTPHandler()

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// A fixed WriteTimeout is an absolute deadline on HTTP/1.x responses
		// and would terminate healthy long-lived Connect streams.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := srv.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			closeErr := srv.Close()
			return errors.Join(ctx.Err(), fmt.Errorf("HTTP graceful shutdown: %w", shutdownErr), closeErr)
		}
		return ctx.Err()
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request, entry *httpToolEntry) {
	ctx, cancel, err := applyConnectTimeout(r)
	defer cancel()
	if err != nil {
		httpErrorWithLimit(w, err, entry.maxUnaryResponse)
		return
	}
	trustedMetadata, _ := metadata.FromIncomingContext(ctx)
	ctx = metadata.NewIncomingContext(ctx, metadata.Join(trustedMetadata, s.incomingHTTPMetadata(r)))
	r = r.WithContext(ctx)
	requestBody := r.Body
	stopBodyClose := context.AfterFunc(ctx, func() { _ = requestBody.Close() })
	defer stopBodyClose()

	// Streaming tools speak Connect's streaming protocol (envelope frames).
	// They never accept plain application/json on the wire — Connect splits
	// stream and unary content types intentionally so clients don't accidentally
	// expect a single response.
	if entry.tool.ServerStreaming {
		s.handleHTTPStream(w, r, entry)
		return
	}

	ctx, transport := withProjectionUnaryTransport(r.Context(), entry.tool.callInfo.FullMethod)
	r = r.WithContext(ctx)

	contentType := r.Header.Get("Content-Type")
	jsonRequest := matchContentType(contentType, jsonContentType)
	protoRequest := isProtoContentType(contentType)
	if !jsonRequest && !protoRequest {
		httpUnsupportedMediaTypeWithLimit(w, "unary tools require Content-Type: "+jsonContentType+" or "+protoContentType, entry.maxUnaryResponse)
		return
	}
	if encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); encoding != "" && encoding != "identity" {
		httpErrorWithLimit(w, status.Errorf(codes.Unimplemented, "Content-Encoding %q is not supported", encoding), entry.maxUnaryResponse)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, entry.maxUnaryRequest))
	if err != nil {
		if contextErr := r.Context().Err(); contextErr != nil {
			httpErrorWithLimit(w, status.FromContextError(contextErr).Err(), entry.maxUnaryResponse)
			return
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpErrorWithLimit(w, status.Errorf(codes.ResourceExhausted, "request body exceeds %d byte limit", entry.maxUnaryRequest), entry.maxUnaryResponse)
		} else {
			httpErrorWithLimit(w, invalidArgumentError("read body: "+err.Error()), entry.maxUnaryResponse)
		}
		return
	}

	if protoRequest {
		s.handleHTTPProto(w, r, entry, body, transport)
		return
	}

	resp, err := s.invokeJSON(r.Context(), entry.tool, body)
	if err == nil {
		err = r.Context().Err()
	}
	if err != nil {
		writeUnaryProjectionMetadata(w.Header(), transport)
		httpErrorWithLimit(w, err, entry.maxUnaryResponse)
		return
	}
	writeUnaryProjectionMetadata(w.Header(), transport)

	if int64(len(resp)) > entry.maxUnaryResponse {
		httpErrorWithLimit(w, status.Errorf(codes.ResourceExhausted, "encoded response exceeds %d byte limit", entry.maxUnaryResponse), entry.maxUnaryResponse)
		return
	}

	w.Header().Set("Content-Type", jsonContentType)
	_, _ = w.Write([]byte(resp)) //nolint:gosec // server-generated JSON
}

// handleHTTPStream serves a server-streaming RPC over Connect's streaming
// wire format. Request body is a single Connect envelope wrapping the
// request (JSON or binary proto). Response body is a series of message
// envelopes followed by one end-of-stream envelope.
//
// Envelope: [flags:1byte] [size:uint32 BE] [data]
// Flags bit 0x02 marks the end-of-stream envelope; its payload is ALWAYS
// JSON regardless of the message content type — per the Connect spec.
func (s *Server) handleHTTPStream(w http.ResponseWriter, r *http.Request, entry *httpToolEntry) {
	ct := r.Header.Get("Content-Type")
	binary := isConnectStreamProto(ct)
	if !binary && !isConnectStreamJSON(ct) {
		httpUnsupportedMediaTypeWithLimit(w, "streaming tools require Content-Type: "+connectStreamJSONType+" or "+connectStreamProtoType, maxConnectControlEnvelope)
		return
	}
	respCT := connectStreamJSONType
	if binary {
		respCT = connectStreamProtoType
	}

	reqBytes, err := readConnectEnvelope(r.Body, entry.maxStreamRequest)
	if err != nil {
		if contextErr := r.Context().Err(); contextErr != nil {
			err = status.FromContextError(contextErr).Err()
		}
		if _, ok := status.FromError(err); !ok {
			err = invalidArgumentError("read request envelope: " + err.Error())
		}
		writeConnectStreamError(w, respCT, err)
		return
	}

	req, err := s.newRequest(entry.tool)
	if err != nil {
		writeConnectStreamError(w, respCT, err)
		return
	}
	if len(reqBytes) > 0 {
		if binary {
			if err := proto.Unmarshal(reqBytes, req); err != nil {
				writeConnectStreamError(w, respCT, invalidArgumentError("decode binary proto: "+err.Error()))
				return
			}
		} else if err := protojson.Unmarshal(reqBytes, req); err != nil {
			writeConnectStreamError(w, respCT, invalidArgumentFromJSONError(err))
			return
		}
	}

	flusher, _ := w.(http.Flusher)

	jsonOpts := protojson.MarshalOptions{UseProtoNames: true}
	committed := false
	var stream *projectedServerStream
	commit := func() {
		if committed {
			return
		}
		header, _ := stream.metadata()
		writeConnectMetadataHeaders(w.Header(), header, false)
		w.Header().Set("Content-Type", respCT)
		w.WriteHeader(http.StatusOK)
		committed = true
	}
	stream = newProjectedServerStream(r.Context(), entry.tool.streamInfo.FullMethod, req, func(msg proto.Message) error {
		var payload []byte
		var marshalErr error
		if binary {
			payload, marshalErr = proto.Marshal(msg)
		} else {
			payload, marshalErr = jsonOpts.Marshal(msg)
		}
		if marshalErr != nil {
			return fmt.Errorf("marshal stream chunk: %w", marshalErr)
		}
		if int64(len(payload)) > entry.maxStreamResponse {
			return status.Errorf(codes.ResourceExhausted, "encoded stream response message exceeds %d byte limit", entry.maxStreamResponse)
		}
		commit()
		if err := writeConnectEnvelope(w, 0, payload); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	stream.setHeaderSender(func() error {
		commit()
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})

	streamErr := s.invokeGRPCStream(entry.tool, stream)
	if streamErr == nil {
		streamErr = r.Context().Err()
	}
	commit()
	_, trailer := stream.metadata()
	endPayload := connectEndStreamPayload(streamErr, trailer)
	_ = writeConnectEnvelope(w, connectEndStreamFlag, endPayload)
	if flusher != nil {
		flusher.Flush()
	}
}

// handleMCPHTTP serves a single JSON-RPC request over HTTP as the MCP
// Streamable HTTP transport. Request body is one JSON-RPC envelope; response
// body is one JSON-RPC envelope. Accepted notifications and client responses
// return 202 with an empty body.
//
// We don't accept multiple JSON-RPC messages per POST or open an SSE stream
// here — keep the transport minimal and shaped like the rest of the framework.
func (s *Server) handleMCPHTTP(w http.ResponseWriter, r *http.Request) {
	if httpHeaderPresent(r.Header, "Origin") {
		http.Error(w, "Origin is not accepted", http.StatusForbidden)
		return
	}
	if !acceptsMCPResponseTypes(strings.Join(r.Header.Values("Accept"), ",")) {
		http.Error(w, "Accept must include application/json and text/event-stream", http.StatusNotAcceptable)
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(contentType, jsonContentType) {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	ctx, cancel, err := applyConnectTimeout(r)
	defer cancel()
	if err != nil {
		httpErrorWithLimit(w, err, s.httpMaxUnaryResponse)
		return
	}
	trustedMetadata, _ := metadata.FromIncomingContext(ctx)
	ctx = metadata.NewIncomingContext(ctx, metadata.Join(trustedMetadata, s.incomingHTTPMetadata(r)))
	requestBody := r.Body
	stopBodyClose := context.AfterFunc(ctx, func() { _ = requestBody.Close() })
	defer stopBodyClose()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.httpMaxUnaryRequest))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			httpErrorWithLimit(w, status.FromContextError(contextErr).Err(), s.httpMaxUnaryResponse)
			return
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpErrorWithLimit(w, status.Errorf(codes.ResourceExhausted, "request body exceeds %d byte limit", s.httpMaxUnaryRequest), s.httpMaxUnaryResponse)
		} else {
			httpErrorWithLimit(w, invalidArgumentError("read body: "+err.Error()), s.httpMaxUnaryResponse)
		}
		return
	}

	req, clientResponse, protocolError := parseMCPJSONRPC(body)
	if protocolError != nil {
		writeMCPJSON(w, protocolError, s.httpMaxUnaryResponse)
		return
	}
	method := ""
	if req != nil {
		method = req.Method
	}
	protocolVersion := r.Header.Get("MCP-Protocol-Version")
	if (method == "initialize" && protocolVersion != "" && protocolVersion != mcpProtocolVersion) ||
		(method != "initialize" && protocolVersion != mcpProtocolVersion) {
		http.Error(w, "unsupported or missing MCP-Protocol-Version", http.StatusBadRequest)
		return
	}

	if clientResponse {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := s.mcpDispatch(ctx, req)
	if contextErr := ctx.Err(); contextErr != nil {
		httpErrorWithLimit(w, status.FromContextError(contextErr).Err(), s.httpMaxUnaryResponse)
		return
	}
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	writeMCPJSON(w, resp, s.httpMaxUnaryResponse)
}

func acceptsMCPResponseTypes(accept string) bool {
	var acceptsJSON, acceptsSSE bool
	for value := range strings.SplitSeq(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		if quality, ok := params["q"]; ok {
			weight, parseErr := strconv.ParseFloat(quality, 64)
			if parseErr != nil || weight <= 0 {
				continue
			}
		}
		switch strings.ToLower(mediaType) {
		case jsonContentType:
			acceptsJSON = true
		case "text/event-stream":
			acceptsSSE = true
		}
	}
	return acceptsJSON && acceptsSSE
}

func httpHeaderPresent(headers http.Header, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

func writeMCPJSON(w http.ResponseWriter, response *jsonRPCResponse, maxBytes int64) {
	payload, err := json.Marshal(response)
	if err != nil {
		httpErrorWithLimit(w, status.Error(codes.Internal, "encode MCP response"), maxBytes)
		return
	}
	if maxBytes > 0 && int64(len(payload)) > maxBytes {
		httpErrorWithLimit(w, status.Error(codes.ResourceExhausted, "encoded MCP response exceeds configured byte limit"), maxBytes)
		return
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	_, _ = w.Write(payload)
}

// isConnectStreamJSON reports whether ct is application/connect+json (with
// optional parameters).
func isConnectStreamJSON(ct string) bool {
	return matchContentType(ct, connectStreamJSONType)
}

// isConnectStreamProto reports whether ct is application/connect+proto (with
// optional parameters).
func isConnectStreamProto(ct string) bool {
	return matchContentType(ct, connectStreamProtoType)
}

func matchContentType(ct, want string) bool {
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mediaType == want
}

// readConnectEnvelope reads a single Connect envelope frame from r. The
// envelope header (flags+length) must be present; data is read up to size.
// maxSize is enforced to avoid unbounded allocations from a hostile client.
func readConnectEnvelope(r io.Reader, maxSize int64) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if reserved := hdr[0] &^ byte(0x03); reserved != 0 {
		return nil, status.Errorf(codes.InvalidArgument, "request envelope has unsupported reserved flags 0x%02x", reserved)
	}
	if hdr[0]&byte(0x01) != 0 {
		return nil, status.Error(codes.Unimplemented, "compressed request envelopes are not supported")
	}
	if hdr[0]&connectEndStreamFlag != 0 {
		return nil, status.Error(codes.InvalidArgument, "request envelope must not use the end-stream flag")
	}
	size := uint32(hdr[1])<<24 | uint32(hdr[2])<<16 | uint32(hdr[3])<<8 | uint32(hdr[4])
	if maxSize > 0 && int64(size) > maxSize {
		return nil, status.Errorf(codes.ResourceExhausted, "request envelope size %d exceeds %d byte limit", size, maxSize)
	}
	buf := make([]byte, size)
	if size > 0 {
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
	}
	extra, err := io.ReadAll(io.LimitReader(r, 1))
	if err != nil {
		return nil, err
	}
	if len(extra) != 0 {
		return nil, status.Error(codes.InvalidArgument, "stream request body must contain exactly one envelope")
	}
	return buf, nil
}

// writeConnectEnvelope writes one Connect envelope frame to w.
func writeConnectEnvelope(w io.Writer, flags byte, payload []byte) error {
	var hdr [5]byte
	hdr[0] = flags
	size, err := connectEnvelopeSize(uint64(len(payload)))
	if err != nil {
		return err
	}
	hdr[1] = byte(size >> 24)
	hdr[2] = byte(size >> 16)
	hdr[3] = byte(size >> 8)
	hdr[4] = byte(size)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func connectEnvelopeSize(size uint64) (uint32, error) {
	if size > uint64(^uint32(0)) {
		return 0, status.Errorf(codes.ResourceExhausted, "Connect envelope payload size %d exceeds uint32 framing limit", size)
	}
	return uint32(size), nil
}

// applyConnectTimeout honors the Connect-Timeout-Ms request header. The
// Connect grammar requires one to ten ASCII digits representing a positive
// integer. Caller must always defer the returned cancel.
func applyConnectTimeout(r *http.Request) (context.Context, context.CancelFunc, error) {
	if !httpHeaderPresent(r.Header, "Connect-Timeout-Ms") {
		return r.Context(), func() {}, nil
	}
	raw := r.Header.Get("Connect-Timeout-Ms")
	if len(raw) == 0 || len(raw) > 10 {
		return r.Context(), func() {}, invalidArgumentError(
			"Connect-Timeout-Ms must be a positive integer of at most 10 ASCII digits",
		)
	}
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return r.Context(), func() {}, invalidArgumentError(
				"Connect-Timeout-Ms must be a positive integer of at most 10 ASCII digits",
			)
		}
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		return r.Context(), func() {}, invalidArgumentError(
			"Connect-Timeout-Ms must be a positive integer of at most 10 ASCII digits",
		)
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(ms)*time.Millisecond) //nolint:gosec // cancel returned for caller defer
	return ctx, cancel, nil
}

// handleHTTPProto handles requests with Content-Type: application/proto.
// Registration precomputes a typed request factory, so decoding does not need
// a descriptor lookup or a dynamic-message conversion on the request path.
func (s *Server) handleHTTPProto(w http.ResponseWriter, r *http.Request, entry *httpToolEntry, body []byte, transport *projectionUnaryTransport) {
	req, err := s.newRequest(entry.tool)
	if err != nil {
		httpErrorWithLimit(w, invalidArgumentError("binary proto requires a Server created via ServerFromDescriptor or ServerFromBytes"), entry.maxUnaryResponse)
		return
	}
	if len(body) > 0 {
		if err := proto.Unmarshal(body, req); err != nil {
			httpErrorWithLimit(w, invalidArgumentError("decode binary proto: "+err.Error()), entry.maxUnaryResponse)
			return
		}
	}

	resp, err := s.invoke(r.Context(), entry.tool, req)
	if err == nil {
		err = r.Context().Err()
	}
	if err != nil {
		writeUnaryProjectionMetadata(w.Header(), transport)
		httpErrorWithLimit(w, err, entry.maxUnaryResponse)
		return
	}

	respBytes, err := proto.Marshal(resp)
	if err != nil {
		writeUnaryProjectionMetadata(w.Header(), transport)
		httpErrorWithLimit(w, fmt.Errorf("encode binary proto: %w", err), entry.maxUnaryResponse)
		return
	}
	if int64(len(respBytes)) > entry.maxUnaryResponse {
		writeUnaryProjectionMetadata(w.Header(), transport)
		httpErrorWithLimit(w, status.Errorf(codes.ResourceExhausted, "encoded response exceeds %d byte limit", entry.maxUnaryResponse), entry.maxUnaryResponse)
		return
	}

	writeUnaryProjectionMetadata(w.Header(), transport)
	w.Header().Set("Content-Type", protoContentType)
	_, _ = w.Write(respBytes)
}

func (s *Server) handleToolCatalog(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tools": s.ToolCatalog()})
}

// handleHealth answers k8s-style liveness/readiness probes. Always 200 once
// the server is serving — registration is synchronous and complete before
// HTTPHandler returns, so by the time a request lands we are ready.
func (s *Server) handleHealth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleDescriptor(w http.ResponseWriter) {
	if s.fds == nil {
		http.Error(w, "no descriptor available", http.StatusNotFound)
		return
	}
	bytes, err := proto.Marshal(s.fds)
	if err != nil {
		http.Error(w, "marshal descriptor: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", protoContentType)
	_, _ = w.Write(bytes)
}

func httpErrorWithLimit(w http.ResponseWriter, err error, maxBytes int64) {
	st := statusFromError(err)
	payload, marshalErr := json.Marshal(connectErrorPayload(err))
	if marshalErr != nil {
		st = status.New(codes.Internal, "encode Connect error")
		payload = []byte(`{"code":"internal","message":"encode Connect error"}`)
	}
	if maxBytes > 0 && int64(len(payload)) > maxBytes {
		st = status.New(codes.ResourceExhausted, "encoded error response exceeds configured byte limit")
		payload, _ = json.Marshal(connectErrorPayload(st.Err()))
		if int64(len(payload)) > maxBytes {
			payload = nil
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(grpcCodeToHTTPStatus(st.Code()))
	_, _ = w.Write(payload)
}

func httpUnsupportedMediaTypeWithLimit(w http.ResponseWriter, message string, maxBytes int64) {
	err := status.Error(codes.InvalidArgument, message)
	payload, _ := json.Marshal(connectErrorPayload(err))
	if maxBytes > 0 && int64(len(payload)) > maxBytes {
		payload = nil
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusUnsupportedMediaType)
	_, _ = w.Write(payload)
}

func writeConnectStreamError(w http.ResponseWriter, contentType string, err error) {
	payload := connectEndStreamPayload(err, nil)
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_ = writeConnectEnvelope(w, connectEndStreamFlag, payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func connectEndStreamPayload(streamErr error, trailer metadata.MD) []byte {
	end := map[string]any{}
	if streamErr != nil {
		end["error"] = connectErrorPayload(streamErr)
	}
	if encoded := connectEndStreamMetadata(trailer); len(encoded) > 0 {
		end["metadata"] = encoded
	}
	payload, err := json.Marshal(end)
	if err == nil && len(payload) <= maxConnectControlEnvelope {
		return payload
	}
	fallback, _ := json.Marshal(map[string]any{"error": connectErrorPayload(status.Error(
		codes.ResourceExhausted, "Connect control envelope exceeds configured byte limit",
	))})
	return fallback
}

func isProtoContentType(ct string) bool {
	return matchContentType(ct, protoContentType)
}
