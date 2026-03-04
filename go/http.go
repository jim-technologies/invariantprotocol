package invariant

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// serveHTTP starts a blocking HTTP server on the given port.
// Pattern mirrors Python's start_http: one goroutine per request (via net/http),
// synchronous handler calls invoke() directly.
func (s *Server) serveHTTP(port int) error {
	bindings, err := s.buildHTTPBindings()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		binding, pathParams, methodMismatch := findHTTPBinding(bindings, r.Method, r.URL.Path)
		if binding == nil {
			if methodMismatch {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			http.NotFound(w, r)
			return
		}

		s.handleHTTP(w, r, binding, pathParams)
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request, binding *httpBinding, pathParams map[string]string) {
	argsJSON, err := binding.requestArgs(r, pathParams)
	if err != nil {
		httpError(w, err)
		return
	}

	// Boundary conversion + core dispatch + boundary conversion (JSON → proto → JSON)
	result, err := s.invokeJSON(r.Context(), binding.tool, argsJSON)
	if err != nil {
		httpError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(result)) //nolint:gosec // response is server-generated JSON, not user taint
}

func httpError(w http.ResponseWriter, err error) {
	st := statusFromError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(grpcCodeToHTTPStatus(st.Code()))
	_ = json.NewEncoder(w).Encode(map[string]any{"error": errorPayload(err)})
}
