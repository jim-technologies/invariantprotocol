package invariant

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// serveHTTP starts a blocking HTTP server on the given port.
func (s *Server) serveHTTP(port int) error {
	mux := http.NewServeMux()

	// Build route map: "/greet.v1.GreetService/Greet" -> Tool
	for _, tool := range s.tools {
		route := fmt.Sprintf("/%s/%s", tool.ServiceFullName, tool.MethodName)
		t := tool // capture
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.handleHTTP(w, r, t)
		})
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request, tool *Tool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	var argsJSON json.RawMessage
	if len(body) > 0 {
		if !json.Valid(body) {
			httpError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		argsJSON = body
	} else {
		argsJSON = json.RawMessage("{}")
	}

	result, err := s.invoke(r.Context(), tool, argsJSON)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(result)) //nolint:gosec // response is server-generated JSON, not user taint
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
