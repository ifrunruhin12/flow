package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type Pinger interface {
	Ping(context.Context) error
}

type IntegrationStatus struct {
	mu      sync.RWMutex
	state   string
	detail  string
	checked time.Time
}

type Server struct {
	logger      *slog.Logger
	database    Pinger
	navidrome   Pinger
	integration *IntegrationStatus
	ready       atomic.Bool
	handler     http.Handler
}

func New(logger *slog.Logger, database, navidrome Pinger, integration *IntegrationStatus, applicationHandlers ...http.Handler) *Server {
	server := &Server{logger: logger, database: database, navidrome: navidrome, integration: integration}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", server.livez)
	mux.HandleFunc("GET /readyz", server.readyz)
	mux.HandleFunc("GET /healthz", server.healthz)
	if len(applicationHandlers) > 0 && applicationHandlers[0] != nil {
		mux.Handle("/api/", applicationHandlers[0])
		mux.Handle("/audio/", applicationHandlers[0])
	}
	server.handler = requestLog(logger, mux)
	return server
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

func (s *IntegrationStatus) Set(state, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	s.detail = detail
	s.checked = time.Now().UTC()
}

func (s *IntegrationStatus) snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]any{
		"state":     s.state,
		"detail":    s.detail,
		"checkedAt": s.checked,
	}
}

func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.database.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "database": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	databaseState := "ok"
	navidromeState := "ok"
	status := http.StatusOK
	if err := s.database.Ping(ctx); err != nil {
		databaseState = "unavailable"
		status = http.StatusServiceUnavailable
	}
	if err := s.navidrome.Ping(ctx); err != nil {
		navidromeState = "unavailable"
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"status":      map[bool]string{true: "ok", false: "degraded"}[status == http.StatusOK],
		"database":    databaseState,
		"navidrome":   navidromeState,
		"integration": s.integration.snapshot(),
	})
}

func requestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		logger.InfoContext(r.Context(), "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"correlation_id", recorder.Header().Get("X-Correlation-Id"),
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(body)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
