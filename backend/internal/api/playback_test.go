package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
	"github.com/ifrunruhin12/flow/backend/internal/playback"
)

type apiPlaybackStore struct {
	mu     sync.Mutex
	states map[int64]playback.Session
}

func (s *apiPlaybackStore) Load(_ context.Context, p auth.Principal) (playback.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[p.User.ID]
	if !ok {
		state = playback.DefaultSession(time.Now())
		s.states[p.User.ID] = state
	}
	return state, nil
}

func (s *apiPlaybackStore) CompareAndSwap(_ context.Context, p auth.Principal, revision int64, state playback.Session) (playback.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.states[p.User.ID]; revision != current.Revision {
		return playback.Session{}, &playback.ConflictError{Current: current}
	}
	state.Revision = revision + 1
	state.UpdatedAt = time.Now().UTC()
	s.states[p.User.ID] = state
	return state, nil
}

type apiPlaybackLibrary struct{ err error }

func (l *apiPlaybackLibrary) Song(_ context.Context, id int64, media string) (library.Song, error) {
	if id != 11 && id != 22 {
		return library.Song{}, navidrome.ErrAuthorization
	}
	return library.Song{ID: media, DurationMS: 180_000}, l.err
}

func newPlaybackServer() (*Server, *fakeAuth, *apiPlaybackLibrary) {
	a := &fakeAuth{principal: auth.Principal{
		User: auth.User{ID: 11, Role: "member", Status: "active", ProvisioningState: "complete"}, TokenHash: make([]byte, 32),
	}}
	l := &apiPlaybackLibrary{}
	service := playback.New(&apiPlaybackStore{states: map[int64]playback.Session{}}, l)
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), a, &fakeMembership{}, "http://127.0.0.1:5173", false, time.Hour, Services{Playback: service})
	return server, a, l
}

func playbackResponse(server *Server, method, target, body string, authenticated bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Origin", "http://127.0.0.1:5173")
	if authenticated {
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestPlaybackRESTOwnershipCASAndClear(t *testing.T) {
	server, authService, _ := newPlaybackServer()
	response := playbackResponse(server, "GET", "/api/playback/session", "", true)
	var state playback.Session
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &state) != nil || state.Revision != 0 || state.Volume != 1 || state.CurrentMediaID != nil || state.Queue == nil {
		t.Fatalf("default response = %d %s", response.Code, response.Body.String())
	}
	response = playbackResponse(server, "PUT", "/api/playback/session", `{"ifRevision":0,"queue":[{"mediaId":"song-a"}],"queueIndex":0,"status":"playing"}`, true)
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &state) != nil || state.Revision != 1 || state.Status != "playing" || *state.CurrentMediaID != "song-a" {
		t.Fatalf("update response = %d %s", response.Code, response.Body.String())
	}
	response = playbackResponse(server, "PUT", "/api/playback/session", `{"ifRevision":0,"volume":0.1}`, true)
	var conflict struct {
		Error           struct{ Code string } `json:"error"`
		CurrentRevision int64                 `json:"currentRevision"`
		CurrentSession  playback.Session      `json:"currentSession"`
	}
	if response.Code != 409 || json.Unmarshal(response.Body.Bytes(), &conflict) != nil || conflict.Error.Code != "conflict" || conflict.CurrentRevision != 1 || *conflict.CurrentSession.CurrentMediaID != "song-a" {
		t.Fatalf("conflict response = %d %s", response.Code, response.Body.String())
	}
	authService.principal.User.ID = 22
	response = playbackResponse(server, "GET", "/api/playback/session", "", true)
	if json.Unmarshal(response.Body.Bytes(), &state) != nil || state.CurrentMediaID != nil || state.Revision != 0 {
		t.Fatal("another user read playback")
	}
	response = playbackResponse(server, "PUT", "/api/playback/session", `{"ifRevision":0,"volume":0.2}`, true)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	authService.principal.User.ID = 11
	response = playbackResponse(server, "DELETE", "/api/playback/session", `{"ifRevision":1}`, true)
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &state) != nil || state.Revision != 2 || state.CurrentMediaID != nil || state.Status != "paused" {
		t.Fatalf("clear response = %d %s", response.Code, response.Body.String())
	}
	authService.principal.User.ID = 22
	response = playbackResponse(server, "GET", "/api/playback/session", "", true)
	if json.Unmarshal(response.Body.Bytes(), &state) != nil || state.Volume != .2 || state.Revision != 1 {
		t.Fatal("one user's clear changed another user's state")
	}
}

func TestPlaybackRESTAuthorizationValidationAndErrors(t *testing.T) {
	server, a, media := newPlaybackServer()
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		if response := playbackResponse(server, method, "/api/playback/session", `{}`, false); response.Code != 401 {
			t.Fatalf("anonymous %s = %d", method, response.Code)
		}
	}
	for _, test := range []struct{ method, target, body string }{
		{"GET", "/api/playback/session?userId=22", ""},
		{"PUT", "/api/playback/session", `{"ifRevision":0,"userId":22,"volume":0.1}`},
		{"PUT", "/api/playback/session", `{"volume":0.1}`},
		{"PUT", "/api/playback/session", `{"ifRevision":null,"volume":0.1}`},
		{"PUT", "/api/playback/session", `{"ifRevision":0,"positionMs":-1}`},
		{"PUT", "/api/playback/session", `{"ifRevision":0,"volume":2}`},
		{"PUT", "/api/playback/session", `{"ifRevision":0,"queue":[{"mediaId":"../secret"}]}`},
		{"DELETE", "/api/playback/session?ifRevision=0", ""},
		{"DELETE", "/api/playback/session", ""},
		{"DELETE", "/api/playback/session", `{"ifRevision":0,"userId":22}`},
	} {
		if response := playbackResponse(server, test.method, test.target, test.body, true); response.Code != 400 {
			t.Fatalf("invalid request = %d %s", response.Code, response.Body.String())
		}
	}
	request := httptest.NewRequest("PUT", "/api/playback/session", strings.NewReader(`{"ifRevision":0,"volume":0.1}`))
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	request.Header.Set("Origin", "https://another-site.invalid")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("cross-origin playback mutation accepted")
	}
	a.authErr = auth.ErrAccountNotActive
	if response := playbackResponse(server, "PUT", "/api/playback/session", `{"ifRevision":0,"volume":0.1}`, true); response.Code != 403 {
		t.Fatal("suspended account mutated playback")
	}
	a.authErr = nil
	a.principal.User.MustChangePassword = true
	if response := playbackResponse(server, "GET", "/api/playback/session", "", true); response.Code != 403 {
		t.Fatal("restricted login read playback")
	}
	a.principal.User.MustChangePassword = false
	for _, test := range []struct {
		err    error
		status int
	}{
		{navidrome.ErrNotFound, 404}, {navidrome.ErrAuthorization, 403},
		{navidrome.ErrUnavailable, 503}, {navidrome.ErrMalformed, 502},
	} {
		media.err = test.err
		response := playbackResponse(server, "PUT", "/api/playback/session", `{"ifRevision":0,"queueEdit":{"add":["song-a"]}}`, true)
		if response.Code != test.status || strings.Contains(response.Body.String(), "navidrome:") || response.Header().Get("X-Correlation-Id") == "" {
			t.Fatalf("media error response = %d %s", response.Code, response.Body.String())
		}
	}
}
