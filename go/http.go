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

// protoContentType is the Connect-standard binary proto content type.
const protoContentType = "application/proto"

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
		case r.Method == http.MethodGet && r.URL.Path == "/__invariant/descriptor.binpb":
			s.handleDescriptor(w)
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, invalidArgumentError("read body: "+err.Error()))
		return
	}

	ctx, cancel := applyConnectTimeout(r)
	defer cancel()
	r = r.WithContext(ctx)

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
