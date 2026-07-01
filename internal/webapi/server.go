// Package webapi is the supervisor's optional control-plane: it exposes the
// discovery/render state that gobackup can't know, plus a "backup now" action
// proxied to gobackup's own /api/perform. Enabled via GOBACKUP_DOCKER_HTTP_ADDR.
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ekho/gobackup-docker/internal/pipeline"
)

// Server serves the control-plane endpoints.
type Server struct {
	Status      func() pipeline.Status // supervisor state provider
	GobackupURL string                 // base URL of gobackup's web API; "" disables the perform proxy
	Client      *http.Client           // optional; defaults to a 10s-timeout client
}

// Serve runs the HTTP server until ctx is cancelled, then shuts it down gracefully.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	log.Printf("[api] control-plane listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Handler builds the route mux (exported so it can be tested with httptest).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/status", s.statusHandler)
	mux.HandleFunc("/api/perform", s.perform)
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, "ok\n")
}

func (s *Server) statusHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Status())
}

// perform triggers a backup by proxying to gobackup's POST /api/perform. It only
// forwards models this supervisor actually rendered, so it can't be used as an
// open proxy for arbitrary model names.
func (s *Server) perform(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	model := r.FormValue("model")
	if model == "" {
		http.Error(w, "missing 'model' parameter", http.StatusBadRequest)
		return
	}
	if s.GobackupURL == "" {
		http.Error(w, "backup proxy not configured (set GOBACKUP_DOCKER_GOBACKUP_URL)", http.StatusNotImplemented)
		return
	}
	if !contains(s.Status().Models, model) {
		http.Error(w, fmt.Sprintf("unknown model %q", model), http.StatusNotFound)
		return
	}

	endpoint := strings.TrimRight(s.GobackupURL, "/") + "/api/perform"
	resp, err := s.client().PostForm(endpoint, url.Values{"model": {model}})
	if err != nil {
		http.Error(w, "gobackup unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func contains(ss []string, x string) bool {
	for _, v := range ss {
		if v == x {
			return true
		}
	}
	return false
}
