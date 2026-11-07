package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	flowsync "github.com/ifrunruhin12/flow/backend/internal/sync"
)

type fakeSynchronization struct {
	snapshot    flowsync.Snapshot
	result      flowsync.Result
	err         error
	snapshotErr error
	started     *flowsync.Seed
	mutated     *flowsync.Mutation
	ended       bool
}

func syncPointer[T any](value T) *T { return &value }

func (f *fakeSynchronization) Snapshot(context.Context) (flowsync.Snapshot, error) {
	return f.snapshot, f.snapshotErr
}

func (f *fakeSynchronization) Start(_ context.Context, _ auth.Principal, seed flowsync.Seed) (flowsync.Result, error) {
	f.started = &seed
	return f.result, f.err
}

func (f *fakeSynchronization) Mutate(_ context.Context, _ auth.Principal, mutation flowsync.Mutation) (flowsync.Result, error) {
	f.mutated = &mutation
	return f.result, f.err
}

func (f *fakeSynchronization) End(_ context.Context, _ auth.Principal, _ *int64, _ flowsync.LeaseCredentials) (flowsync.Result, error) {
	f.ended = true
	return f.result, f.err
}

func syncAPIServer(role string, service *fakeSynchronization) *Server {
	principal := activePrincipal(role, false)
	principal.User.ID = 7
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(logger, &fakeAuth{principal: principal}, &fakeMembership{}, "http://127.0.0.1:5173", false, time.Hour, Services{Synchronization: service})
}

func syncRequest(server *Server, method, path, body string, headers bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	if method != http.MethodGet {
		request.Header.Set("Origin", "http://127.0.0.1:5173")
	}
	if headers {
		request.Header.Set("X-Flow-DJ-Generation", "3")
		request.Header.Set("X-Flow-DJ-Connection", "11111111-1111-4111-8111-111111111111")
		request.Header.Set("X-Flow-DJ-Lease", strings.Repeat("ab", 32))
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestSynchronizationReadAndCanonicalModeRoutes(t *testing.T) {
	session := flowsync.Session{ID: 9, Version: 2, EventSeq: 5, State: flowsync.StatePaused, CurrentMediaID: "song", Queue: []string{"song"}}
	service := &fakeSynchronization{snapshot: flowsync.Snapshot{Mode: flowsync.ModeSynchronized, ActiveSharedSessionID: syncPointer(int64(9)), Session: &session}}
	service.result.Snapshot = service.snapshot
	server := syncAPIServer("member", service)
	response := syncRequest(server, http.MethodGet, "/api/platform/mode", "", false)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"activeSharedSessionId":9`) {
		t.Fatalf("mode=%d %s", response.Code, response.Body.String())
	}
	response = syncRequest(server, http.MethodGet, "/api/sync/state", "", false)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"eventSeq":5`) {
		t.Fatalf("state=%d %s", response.Code, response.Body.String())
	}
	if response = syncRequest(server, http.MethodPost, "/api/platform/mode", `{"mode":"synchronized"}`, false); response.Code != http.StatusForbidden {
		t.Fatalf("member mode write=%d %s", response.Code, response.Body.String())
	}
	admin := syncAPIServer("admin", service)
	if response = syncRequest(admin, http.MethodPost, "/api/platform/mode", `{"mode":"synchronized","seed":{"mediaId":"song","positionMs":10}}`, false); response.Code != http.StatusOK || service.started == nil || *service.started.MediaID != "song" {
		t.Fatalf("admin start=%d %s", response.Code, response.Body.String())
	}
	if response = syncRequest(admin, http.MethodPost, "/api/sync/session", `{}`, false); response.Code != http.StatusNotFound {
		t.Fatalf("competing start route exists: %d", response.Code)
	}
	if response = syncRequest(admin, http.MethodPost, "/api/sync/session/lease", `{}`, false); response.Code != http.StatusNotFound {
		t.Fatalf("public lease route exists: %d", response.Code)
	}
}

func TestSynchronizationCommandsValidateLeaseBodyOriginAndErrors(t *testing.T) {
	session := flowsync.Session{ID: 9, Version: 2, EventSeq: 5, State: flowsync.StatePaused, CurrentMediaID: "song", Queue: []string{"song"}}
	service := &fakeSynchronization{result: flowsync.Result{Snapshot: flowsync.Snapshot{Mode: flowsync.ModeSynchronized, Session: &session}}}
	server := syncAPIServer("admin", service)
	if response := syncRequest(server, http.MethodPost, "/api/sync/session/play", `{"ifVersion":2}`, false); response.Code != http.StatusBadRequest {
		t.Fatalf("missing lease=%d %s", response.Code, response.Body.String())
	}
	if response := syncRequest(server, http.MethodPost, "/api/sync/session/play", `{"ifVersion":2,"positionMs":3}`, true); response.Code != http.StatusBadRequest {
		t.Fatalf("extra play position=%d %s", response.Code, response.Body.String())
	}
	response := syncRequest(server, http.MethodPost, "/api/sync/session/seek", `{"ifVersion":2,"positionMs":4000}`, true)
	if response.Code != http.StatusOK || service.mutated == nil || service.mutated.Action != "seek" || *service.mutated.PositionMS != 4000 || service.mutated.Lease.Generation != 3 {
		t.Fatalf("seek=%d %s mutation=%+v", response.Code, response.Body.String(), service.mutated)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/sync/session/pause", strings.NewReader(`{"ifVersion":2}`))
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	request.Header.Set("Origin", "https://evil.example")
	blocked := httptest.NewRecorder()
	server.Handler().ServeHTTP(blocked, request)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("foreign origin=%d", blocked.Code)
	}
	for err, expected := range map[error]int{
		flowsync.ErrStaleLease: http.StatusConflict, flowsync.ErrVersionConflict: http.StatusConflict,
		flowsync.ErrSessionEnded: http.StatusGone, flowsync.ErrNoActiveSession: http.StatusServiceUnavailable,
	} {
		service.err = err
		response = syncRequest(server, http.MethodPost, "/api/sync/session/pause", `{"ifVersion":2}`, true)
		if response.Code != expected || expected == http.StatusConflict && !strings.Contains(response.Body.String(), `"currentState"`) {
			t.Fatalf("error %v status=%d body=%s", err, response.Code, response.Body.String())
		}
	}
}

func TestSynchronizationStateUnavailableAndAnonymousRejected(t *testing.T) {
	service := &fakeSynchronization{snapshot: flowsync.Snapshot{Mode: flowsync.ModeIndividual}}
	server := syncAPIServer("member", service)
	if response := syncRequest(server, http.MethodGet, "/api/sync/state", "", false); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("individual sync state=%d", response.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/platform/mode", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous mode read=%d", response.Code)
	}
	service.snapshotErr = errors.New("database unavailable")
	response = syncRequest(server, http.MethodGet, "/api/platform/mode", "", false)
	var payload map[string]any
	if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &payload) != nil {
		t.Fatalf("store error=%d %s", response.Code, response.Body.String())
	}
}
