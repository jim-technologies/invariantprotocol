package invariant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Connect content types.
const (
	protoContentType       = "application/proto"
	connectStreamJSONType  = "application/connect+json"
	connectStreamProtoType = "application/connect+proto"
	connectEndStreamFlag   = byte(0x02)

	// Default body-size safety caps. Per-Server overrides via
	// SetMaxUnaryRequestBytes / SetMaxStreamRequestBytes — useful for apps
	// (e.g. an object store) that legitimately need larger payloads. The
	// defaults stay tight so a misconfigured server doesn't accept
	// arbitrarily large bodies.
	defaultHTTPMaxUnaryRequest     = 16 << 20
	defaultConnectStreamMaxRequest = 16 << 20

	// Kept for tests that exercise the default-cap behavior.
	httpMaxUnaryRequest     = defaultHTTPMaxUnaryRequest
	connectStreamMaxRequest = defaultConnectStreamMaxRequest
)

// httpToolEntry caches per-tool state used by the HTTP handler — built once at
// HTTPHandler() time so per-request work stays minimal.
type httpToolEntry struct {
	tool     *Tool
	reqDesc  protoreflect.MessageDescriptor
	respDesc protoreflect.MessageDescriptor
	reqType  reflect.Type // typed request struct, when handler is a local servicer
}

// HTTPHandler returns an http.Handler that serves all registered tools over the
// Connect protocol. Mount on an existing http.ServeMux or framework router
// instead of binding a separate port:
//
//	mux := http.NewServeMux()
//	h, _ := server.HTTPHandler()
//	mux.Handle("/inv/", http.StripPrefix("/inv", h))
//
// Routes:
//
//	POST /{package.Service}/{Method}      — invoke a tool (Connect protocol)
//	GET  /                                — tool catalog (same shape as MCP tools/list)
//	GET  /__invariant/tools               — tool catalog
//	GET  /__invariant/descriptor.binpb    — raw FileDescriptorSet bytes
func (s *Server) HTTPHandler() (http.Handler, error) {
	entries := make(map[string]*httpToolEntry, len(s.tools))

	// If we have an FDS, pre-resolve descriptors once. For binary proto and
	// optimized JSON paths we want zero per-request descriptor lookups.
	var files *protoregistry.Files
	if s.fds != nil {
		var err error
		files, err = protodesc.NewFiles(s.fds)
		if err != nil {
			return nil, fmt.Errorf("build file descriptors: %w", err)
		}
	}

	for _, t := range s.tools {
		entry := &httpToolEntry{tool: t}
		if files != nil {
			reqDesc, err := findMessageDescriptor(files, t.InputType)
			if err == nil {
				entry.reqDesc = reqDesc
			}
			respDesc, err := findMessageDescriptor(files, t.OutputType)
			if err == nil {
				entry.respDesc = respDesc
			}
		}
		// For local servicer handlers, cache the typed request reflect.Type so
		// binary-proto requests can decode directly into the handler's type
		// without a dynamicpb intermediate.
		if _, dyn := t.Handler.(*grpcDynamicHandler); !dyn {
			if _, dyn2 := t.Handler.(*httpDynamicHandler); !dyn2 {
				if hv := reflect.ValueOf(t.Handler); hv.Kind() == reflect.Func {
					ht := hv.Type()
					if ht.NumIn() == 2 {
						entry.reqType = ht.In(1)
					}
				}
			}
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
			// MCP Streamable HTTP transport — accepts a single JSON-RPC request
			// per POST and returns one JSON-RPC response. Lets remote agent
			// platforms speak MCP without a stdio process.
			s.handleMCPHTTP(w, r)
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

	return mux, nil
}

// serveHTTP starts a blocking HTTP server on the given port. Honors ctx for
// graceful shutdown.
func (s *Server) serveHTTP(ctx context.Context, port int) error {
	handler, err := s.HTTPHandler()
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request, entry *httpToolEntry) {
	ctx, cancel := applyConnectTimeout(r)
	defer cancel()
	r = r.WithContext(ctx)

	// Streaming tools speak Connect's streaming protocol (envelope frames).
	// They never accept plain application/json on the wire — Connect splits
	// stream and unary content types intentionally so clients don't accidentally
	// expect a single response.
	if entry.tool.ServerStreaming {
		s.handleHTTPStream(w, r, entry)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.methodUnaryCap(entry.tool)))
	if err != nil {
		httpError(w, invalidArgumentError("read body: "+err.Error()))
		return
	}

	if isProtoContentType(r.Header.Get("Content-Type")) {
		s.handleHTTPProto(w, r, entry, body)
		return
	}

	resp, err := s.invokeJSON(r.Context(), entry.tool, body)
	if err != nil {
		httpError(w, err)
		return
	}

	if wantsProto(r.Header.Get("Accept")) {
		s.writeProtoResponse(w, entry, []byte(resp))
		return
	}

	w.Header().Set("Content-Type", "application/json")
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
		httpError(w, invalidArgumentError("streaming tools require Content-Type: "+connectStreamJSONType+" or "+connectStreamProtoType))
		return
	}

	reqBytes, err := readConnectEnvelope(r.Body, int(s.methodStreamCap(entry.tool)))
	if err != nil {
		httpError(w, invalidArgumentError("read request envelope: "+err.Error()))
		return
	}

	req, err := s.newRequest(entry.tool)
	if err != nil {
		httpError(w, err)
		return
	}
	if len(reqBytes) > 0 {
		if binary {
			if err := proto.Unmarshal(reqBytes, req); err != nil {
				httpError(w, invalidArgumentError("decode binary proto: "+err.Error()))
				return
			}
		} else if err := protojson.Unmarshal(reqBytes, req); err != nil {
			httpError(w, invalidArgumentFromJSONError(err))
			return
		}
	}

	respCT := connectStreamJSONType
	if binary {
		respCT = connectStreamProtoType
	}
	w.Header().Set("Content-Type", respCT)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	jsonOpts := protojson.MarshalOptions{UseProtoNames: true}
	stream := newCallbackStream(r.Context(), func(msg proto.Message) error {
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
		if err := writeConnectEnvelope(w, 0, payload); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	defer stream.close()

	streamErr := s.invokeStream(entry.tool, req, stream)
	// End-of-stream envelope: empty JSON on success, error envelope on failure.
	endPayload := []byte("{}")
	if streamErr != nil {
		buf, encErr := json.Marshal(map[string]any{"error": errorPayload(streamErr)})
		if encErr == nil {
			endPayload = buf
		}
	}
	_ = writeConnectEnvelope(w, connectEndStreamFlag, endPayload)
	if flusher != nil {
		flusher.Flush()
	}
}

// handleMCPHTTP serves a single JSON-RPC request over HTTP as the MCP
// Streamable HTTP transport. Request body is one JSON-RPC envelope; response
// body is one JSON-RPC envelope (or 204 for notifications).
//
// We don't accept multiple JSON-RPC messages per POST or open an SSE stream
// here — keep the transport minimal and shaped like the rest of the framework.
func (s *Server) handleMCPHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := applyConnectTimeout(r)
	defer cancel()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.httpMaxUnaryRequest))
	if err != nil {
		httpError(w, invalidArgumentError("read body: "+err.Error()))
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcpErr(nil, -32700, "Parse error: "+err.Error()))
		return
	}

	resp := s.mcpDispatch(ctx, &req)
	if resp == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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
	if ct == "" {
		return false
	}
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct) == want
}

// readConnectEnvelope reads a single Connect envelope frame from r. The
// envelope header (flags+length) must be present; data is read up to size.
// maxSize is enforced to avoid unbounded allocations from a hostile client.
func readConnectEnvelope(r io.Reader, maxSize int) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	size := uint32(hdr[1])<<24 | uint32(hdr[2])<<16 | uint32(hdr[3])<<8 | uint32(hdr[4])
	if maxSize > 0 && size > uint32(maxSize) {
		return nil, fmt.Errorf("envelope size %d exceeds max %d", size, maxSize)
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// writeConnectEnvelope writes one Connect envelope frame to w.
func writeConnectEnvelope(w io.Writer, flags byte, payload []byte) error {
	var hdr [5]byte
	hdr[0] = flags
	size := uint32(len(payload))
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

// applyConnectTimeout honors the Connect-Timeout-Ms request header. Returns
// the request's existing context unchanged if the header is missing or invalid.
// Caller must always defer the returned cancel.
func applyConnectTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	raw := r.Header.Get("Connect-Timeout-Ms")
	if raw == "" {
		return r.Context(), func() {}
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		return r.Context(), func() {}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(ms)*time.Millisecond) //nolint:gosec // cancel returned for caller defer
	return ctx, cancel
}

// handleHTTPProto handles requests with Content-Type: application/proto.
// When the handler is a local servicer with a typed request struct, the body
// is decoded directly into that struct (no dynamicpb intermediate). For proxy
// handlers (gRPC/HTTP), falls back to dynamicpb.
func (s *Server) handleHTTPProto(w http.ResponseWriter, r *http.Request, entry *httpToolEntry, body []byte) {
	var req proto.Message

	switch {
	case entry.reqType != nil:
		// Fast path: decode directly into the handler's typed request.
		req = reflect.New(entry.reqType.Elem()).Interface().(proto.Message)
		if len(body) > 0 {
			if err := proto.Unmarshal(body, req); err != nil {
				httpError(w, invalidArgumentError("decode binary proto: "+err.Error()))
				return
			}
		}
	case entry.reqDesc != nil:
		// Proxy path: dynamicpb (descriptor known but no typed handler).
		dyn := dynamicpb.NewMessage(entry.reqDesc)
		if len(body) > 0 {
			if err := proto.Unmarshal(body, dyn); err != nil {
				httpError(w, invalidArgumentError("decode binary proto: "+err.Error()))
				return
			}
		}
		req = dyn
	default:
		httpError(w, invalidArgumentError("binary proto requires a Server created via ServerFromDescriptor or ServerFromBytes"))
		return
	}

	resp, err := s.invoke(r.Context(), entry.tool, req)
	if err != nil {
		httpError(w, err)
		return
	}

	respBytes, err := proto.Marshal(resp)
	if err != nil {
		httpError(w, fmt.Errorf("encode binary proto: %w", err))
		return
	}

	w.Header().Set("Content-Type", protoContentType)
	_, _ = w.Write(respBytes)
}

// writeProtoResponse re-encodes a JSON response string as binary proto using
// the cached response descriptor.
func (s *Server) writeProtoResponse(w http.ResponseWriter, entry *httpToolEntry, jsonBytes []byte) {
	if entry.respDesc == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonBytes) //nolint:gosec
		return
	}
	resp := dynamicpb.NewMessage(entry.respDesc)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(jsonBytes, resp); err != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonBytes) //nolint:gosec
		return
	}
	respBytes, err := proto.Marshal(resp)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonBytes) //nolint:gosec
		return
	}
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

// httpError writes a Connect-style error envelope:
//
//	{"code": "invalid_argument", "message": "...", "details": [...]}
func httpError(w http.ResponseWriter, err error) {
	st := statusFromError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(grpcCodeToHTTPStatus(st.Code()))
	_ = json.NewEncoder(w).Encode(errorPayload(err))
}

func isProtoContentType(ct string) bool {
	if ct == "" {
		return false
	}
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct) == protoContentType
}

func wantsProto(accept string) bool {
	for part := range strings.SplitSeq(accept, ",") {
		mt := strings.TrimSpace(part)
		if i := strings.Index(mt, ";"); i >= 0 {
			mt = mt[:i]
		}
		if strings.TrimSpace(mt) == protoContentType {
			return true
		}
	}
	return false
}
