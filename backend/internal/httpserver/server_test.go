package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type pingerFunc func(context.Context) error

func (f pingerFunc) Ping(ctx context.Context) error { return f(ctx) }

func TestHealthEndpoints(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	integration := &IntegrationStatus{}
	integration.Set("verified", "security gates closed")
	server := New(logger, pingerFunc(func(context.Context) error { return nil }), pingerFunc(func(context.Context) error { return nil }), integration)
	server.SetReady(true)

	for _, path := range []string{"/livez", "/readyz", "/healthz"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.Code)
		}
	}
}

func TestHealthReportsDependencyFailure(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	integration := &IntegrationStatus{}
	server := New(logger, pingerFunc(func(context.Context) error { return errors.New("db down") }), pingerFunc(func(context.Context) error { return nil }), integration)
	server.SetReady(true)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestApplicationHandlerOwnsAPIAndAudioPrefixes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	integration := &IntegrationStatus{}
	application := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	server := New(logger, pingerFunc(func(context.Context) error { return nil }), pingerFunc(func(context.Context) error { return nil }), integration, application)
	for _, path := range []string{"/api/library/albums", "/audio/song-1", "/audio/coverart/al-cover"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusTeapot {
			t.Fatalf("%s status=%d", path, response.Code)
		}
	}
}
