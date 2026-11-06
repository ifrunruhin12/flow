package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
	"github.com/ifrunruhin12/flow/backend/internal/playlist"
	"github.com/ifrunruhin12/flow/backend/internal/streaming"
)

type fakeLibrary struct {
	err error
}

func (f *fakeLibrary) Albums(context.Context, int64, string, int) (library.AlbumPage, error) {
	return library.AlbumPage{Items: []library.Album{}}, f.err
}
func (f *fakeLibrary) Album(context.Context, int64, string) (library.AlbumDetail, error) {
	return library.AlbumDetail{Album: library.Album{ID: "album-1", Title: "Album"}, Tracks: []library.Song{}}, f.err
}
func (f *fakeLibrary) Artists(context.Context, int64, string, int, string) (library.ArtistPage, error) {
	return library.ArtistPage{Items: []library.Artist{}}, f.err
}
func (f *fakeLibrary) Artist(context.Context, int64, string) (library.ArtistDetail, error) {
	return library.ArtistDetail{Artist: library.Artist{ID: "artist-1", Name: "Artist"}, Albums: []library.Album{}, TopTracks: []library.Song{}}, f.err
}
func (f *fakeLibrary) Songs(context.Context, int64, string, int, string) (library.SongPage, error) {
	return library.SongPage{Items: []library.Song{}}, f.err
}
func (f *fakeLibrary) Song(context.Context, int64, string) (library.Song, error) {
	return library.Song{ID: "song-1", Title: "Song"}, f.err
}
func (f *fakeLibrary) Search(context.Context, int64, string, int, int, int, int) (library.SearchResult, error) {
	return library.SearchResult{Albums: []library.Album{}, Artists: []library.Artist{}, Songs: []library.Song{}}, f.err
}
func (f *fakeLibrary) SetStar(context.Context, int64, string, string, bool) error { return f.err }
func (f *fakeLibrary) Starred(context.Context, int64, string) (library.SearchResult, error) {
	return library.SearchResult{Albums: []library.Album{}, Artists: []library.Artist{}, Songs: []library.Song{}}, f.err
}
func (f *fakeLibrary) ScanStatus(context.Context, int64) (library.ScanStatus, error) {
	return library.ScanStatus{}, f.err
}

type fakePlaylists struct {
	writes int
}

func (f *fakePlaylists) List(context.Context, int64) ([]playlist.Item, error) {
	return []playlist.Item{}, nil
}
func (f *fakePlaylists) Get(context.Context, int64, string) (playlist.Item, error) {
	return playlist.Item{ID: "playlist-1", Name: "Playlist", Tracks: []library.Song{}}, nil
}
func (f *fakePlaylists) Create(context.Context, int64, string, string, []string) (playlist.Item, error) {
	f.writes++
	return playlist.Item{ID: "playlist-1", Name: "Playlist", Tracks: []library.Song{}}, nil
}
func (f *fakePlaylists) Update(context.Context, int64, string, playlist.Update) (playlist.Item, error) {
	f.writes++
	return playlist.Item{ID: "playlist-1", Name: "Playlist", Tracks: []library.Song{}}, nil
}
func (f *fakePlaylists) Delete(context.Context, int64, string) error {
	f.writes++
	return nil
}

type fakeStreamingGateway struct {
	audio   func(context.Context, string, streaming.AudioRequest) (*streaming.OpenedAudio, error)
	artwork func(context.Context, int64, string, navidrome.ArtworkOptions) (navidrome.BinaryResponse, error)
}

func (f *fakeStreamingGateway) OpenAudio(ctx context.Context, token string, request streaming.AudioRequest) (*streaming.OpenedAudio, error) {
	return f.audio(ctx, token, request)
}
func (f *fakeStreamingGateway) OpenArtwork(ctx context.Context, userID int64, id string, options navidrome.ArtworkOptions) (navidrome.BinaryResponse, error) {
	return f.artwork(ctx, userID, id, options)
}

func TestM2LibraryRoutesRequireAnActiveSessionForMembersAndAdmins(t *testing.T) {
	routes := []string{
		"/api/library/albums", "/api/library/albums/album-1", "/api/library/artists",
		"/api/library/artists/artist-1", "/api/library/songs", "/api/library/songs/song-1",
		"/api/library/search?q=track", "/api/library/stars", "/api/library/scanner/status",
	}
	for _, role := range []string{"member", "admin"} {
		server := m2Server(&fakeAuth{principal: activePrincipal(role, false)}, &fakeLibrary{}, &fakePlaylists{}, nil)
		for _, path := range routes {
			response := performAuthenticated(server, http.MethodGet, path, "")
			if response.Code != http.StatusOK {
				t.Fatalf("%s %s status=%d body=%s", role, path, response.Code, response.Body.String())
			}
		}
	}

	missing := httptest.NewRecorder()
	m2Server(&fakeAuth{}, &fakeLibrary{}, &fakePlaylists{}, nil).Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/library/albums", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing session status=%d", missing.Code)
	}
	for _, authErr := range []error{auth.ErrAccountNotActive} {
		response := performAuthenticated(m2Server(&fakeAuth{authErr: authErr}, &fakeLibrary{}, &fakePlaylists{}, nil), http.MethodGet, "/api/library/albums", "")
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "account_not_active") {
			t.Fatalf("inactive status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestM2LibraryValidatesInputAndMapsUpstreamFailures(t *testing.T) {
	authService := &fakeAuth{principal: activePrincipal("member", false)}
	server := m2Server(authService, &fakeLibrary{}, &fakePlaylists{}, nil)
	for _, path := range []string{
		"/api/library/albums?limit=1000",
		"/api/library/albums?limit=10&limit=20",
		"/api/library/search?q=track&upstream=http://navidrome.internal",
	} {
		invalid := performAuthenticated(server, http.MethodGet, path, "")
		if invalid.Code != http.StatusBadRequest {
			t.Fatalf("invalid query %q status=%d body=%s", path, invalid.Code, invalid.Body.String())
		}
	}
	for err, expected := range map[error]int{
		navidrome.ErrUnavailable:    http.StatusServiceUnavailable,
		navidrome.ErrAuthentication: http.StatusServiceUnavailable,
		context.DeadlineExceeded:    http.StatusServiceUnavailable,
		navidrome.ErrMalformed:      http.StatusBadGateway,
		navidrome.ErrNotFound:       http.StatusNotFound,
	} {
		response := performAuthenticated(m2Server(authService, &fakeLibrary{err: err}, &fakePlaylists{}, nil), http.MethodGet, "/api/library/albums", "")
		if response.Code != expected {
			t.Fatalf("error %v status=%d body=%s", err, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "navidrome.internal") {
			t.Fatal("internal Navidrome address leaked")
		}
	}
}

func TestMemberCannotMutatePlaylists(t *testing.T) {
	playlists := &fakePlaylists{}
	server := m2Server(&fakeAuth{principal: activePrincipal("member", false)}, &fakeLibrary{}, playlists, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/playlists", strings.NewReader(`{"name":"Nope"}`))
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	request.Header.Set("Origin", "http://127.0.0.1:5173")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || playlists.writes != 0 {
		t.Fatalf("member mutation status=%d writes=%d", response.Code, playlists.writes)
	}
}

func TestAdminCanCreatePlaylist(t *testing.T) {
	playlists := &fakePlaylists{}
	server := m2Server(&fakeAuth{principal: activePrincipal("admin", false)}, &fakeLibrary{}, playlists, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/playlists", strings.NewReader(`{"name":"Flow list","songIds":["song-1"]}`))
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	request.Header.Set("Origin", "http://127.0.0.1:5173")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || playlists.writes != 1 {
		t.Fatalf("admin mutation status=%d writes=%d body=%s", response.Code, playlists.writes, response.Body.String())
	}
}

func TestAudioGatewayPreservesSafeRangeHeadersAndStripsSensitiveHeaders(t *testing.T) {
	gateway := &fakeStreamingGateway{}
	gateway.audio = func(_ context.Context, token string, request streaming.AudioRequest) (*streaming.OpenedAudio, error) {
		if token == "" {
			return nil, auth.ErrUnauthenticated
		}
		if request.MediaID != "song-1" || request.Range != "bytes=0-4" || request.Format != "original" {
			t.Fatalf("audio request = %#v", request)
		}
		return &streaming.OpenedAudio{Response: navidrome.BinaryResponse{
			StatusCode: http.StatusPartialContent,
			Header: http.Header{
				"Content-Type":        {"audio/flac"},
				"Content-Length":      {"5"},
				"Content-Range":       {"bytes 0-4/100"},
				"Accept-Ranges":       {"bytes"},
				"Location":            {"http://navidrome.internal/rest/stream"},
				"Set-Cookie":          {"nd=secret"},
				"X-Nd-Authorization":  {"Bearer secret"},
				"Content-Disposition": {"attachment; filename=track.flac"},
			},
			Body: io.NopCloser(strings.NewReader("audio")),
		}}, nil
	}
	server := m2Server(&fakeAuth{}, &fakeLibrary{}, &fakePlaylists{}, gateway)
	request := httptest.NewRequest(http.MethodGet, "/audio/song-1?format=original", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	request.Header.Set("Range", "bytes=0-4")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "audio" || response.Header().Get("Content-Range") != "bytes 0-4/100" || response.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("audio response status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	for _, name := range []string{"Location", "Set-Cookie", "X-ND-Authorization", "Content-Disposition"} {
		if value := response.Header().Get(name); value != "" {
			t.Fatalf("sensitive header %s leaked as %q", name, value)
		}
	}
	if strings.Contains(response.Body.String(), "navidrome.internal") {
		t.Fatal("internal URL leaked in audio body")
	}
}

func TestAudioGatewayAuthenticationErrorsAndUnsupportedParameters(t *testing.T) {
	gateway := &fakeStreamingGateway{}
	gateway.audio = func(_ context.Context, token string, request streaming.AudioRequest) (*streaming.OpenedAudio, error) {
		if token == "" {
			return nil, auth.ErrUnauthenticated
		}
		if request.InvalidQuery {
			return nil, streaming.ErrInvalidRequest
		}
		return nil, auth.ErrAccountNotActive
	}
	server := m2Server(&fakeAuth{}, &fakeLibrary{}, &fakePlaylists{}, gateway)
	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/audio/song-1", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing audio session status=%d", missing.Code)
	}
	missingInvalid := httptest.NewRecorder()
	server.Handler().ServeHTTP(missingInvalid, httptest.NewRequest(http.MethodGet, "/audio/song-1?download=true", nil))
	if missingInvalid.Code != http.StatusUnauthorized {
		t.Fatalf("missing session with invalid request status=%d", missingInvalid.Code)
	}
	inactiveRequest := httptest.NewRequest(http.MethodGet, "/audio/song-1", nil)
	inactiveRequest.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	inactive := httptest.NewRecorder()
	server.Handler().ServeHTTP(inactive, inactiveRequest)
	if inactive.Code != http.StatusForbidden {
		t.Fatalf("inactive audio status=%d", inactive.Code)
	}
	unknown := httptest.NewRequest(http.MethodGet, "/audio/song-1?download=true", nil)
	unknown.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	unknownResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusBadRequest {
		t.Fatalf("unsupported audio parameter status=%d", unknownResponse.Code)
	}
	empty := httptest.NewRequest(http.MethodGet, "/audio/song-1?format=", nil)
	empty.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	emptyResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(emptyResponse, empty)
	if emptyResponse.Code != http.StatusBadRequest {
		t.Fatalf("empty audio parameter status=%d", emptyResponse.Code)
	}
}

func TestAudioGatewayDoesNotInventRangeSupportForTranscodes(t *testing.T) {
	gateway := &fakeStreamingGateway{
		audio: func(context.Context, string, streaming.AudioRequest) (*streaming.OpenedAudio, error) {
			return &streaming.OpenedAudio{Response: navidrome.BinaryResponse{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"audio/ogg"}, "Accept-Ranges": {"none"}},
				Body:       io.NopCloser(strings.NewReader("opus")),
			}}, nil
		},
	}
	server := m2Server(&fakeAuth{}, &fakeLibrary{}, &fakePlaylists{}, gateway)
	request := httptest.NewRequest(http.MethodGet, "/audio/song-1?format=opus&offsetSeconds=5", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	request.Header.Set("Range", "bytes=0-")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Accept-Ranges") != "none" {
		t.Fatalf("transcode status=%d Accept-Ranges=%q", response.Code, response.Header().Get("Accept-Ranges"))
	}
}

func TestArtworkIsAuthenticatedPrivateAndDoesNotLeakUpstreamHeaders(t *testing.T) {
	gateway := &fakeStreamingGateway{}
	gateway.audio = func(context.Context, string, streaming.AudioRequest) (*streaming.OpenedAudio, error) {
		return nil, errors.New("unexpected audio call")
	}
	gateway.artwork = func(_ context.Context, userID int64, id string, _ navidrome.ArtworkOptions) (navidrome.BinaryResponse, error) {
		if userID != 1 || id != "al-cover" {
			t.Fatalf("artwork identity=(%d,%q)", userID, id)
		}
		return navidrome.BinaryResponse{StatusCode: http.StatusOK, Header: http.Header{
			"Content-Type": {"image/jpeg"}, "ETag": {`"art"`}, "Location": {"http://navidrome.internal/art"},
		}, Body: io.NopCloser(strings.NewReader("jpeg"))}, nil
	}
	server := m2Server(&fakeAuth{principal: activePrincipal("member", false)}, &fakeLibrary{}, &fakePlaylists{}, gateway)
	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/audio/coverart/al-cover", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing artwork auth status=%d", missing.Code)
	}
	response := performAuthenticated(server, http.MethodGet, "/audio/coverart/al-cover", "")
	if response.Code != http.StatusOK || response.Body.String() != "jpeg" || response.Header().Get("Cache-Control") != "private, max-age=3600" || response.Header().Get("Location") != "" {
		t.Fatalf("artwork status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}

func TestAudioProxyReadsLargeBodiesIncrementally(t *testing.T) {
	const size = int64(32 << 20)
	reader := &generatedReadCloser{remaining: size}
	gateway := &fakeStreamingGateway{
		audio: func(context.Context, string, streaming.AudioRequest) (*streaming.OpenedAudio, error) {
			return &streaming.OpenedAudio{Response: navidrome.BinaryResponse{StatusCode: http.StatusOK, Header: http.Header{
				"Content-Type": {"audio/flac"}, "Content-Length": {strconv.FormatInt(size, 10)},
			}, Body: reader}}, nil
		},
	}
	server := m2Server(&fakeAuth{}, &fakeLibrary{}, &fakePlaylists{}, gateway)
	request := httptest.NewRequest(http.MethodGet, "/audio/song-1?format=original", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
	writer := &discardResponseWriter{header: make(http.Header)}
	server.Handler().ServeHTTP(writer, request)
	if writer.status != http.StatusOK || writer.written != size {
		t.Fatalf("large stream status=%d bytes=%d", writer.status, writer.written)
	}
	if reader.maximumRead > 64<<10 || writer.maximumWrite > 64<<10 {
		t.Fatalf("stream was buffered in large chunks: read=%d write=%d", reader.maximumRead, writer.maximumWrite)
	}
}

func TestAudioProxyConcurrentStreamsRemainIncremental(t *testing.T) {
	const (
		streamCount = 8
		streamSize  = int64(4 << 20)
	)
	readers := make(chan *generatedReadCloser, streamCount)
	gateway := &fakeStreamingGateway{
		audio: func(context.Context, string, streaming.AudioRequest) (*streaming.OpenedAudio, error) {
			reader := &generatedReadCloser{remaining: streamSize}
			readers <- reader
			return &streaming.OpenedAudio{Response: navidrome.BinaryResponse{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"audio/flac"}, "Content-Length": {strconv.FormatInt(streamSize, 10)}},
				Body:       reader,
			}}, nil
		},
	}
	server := m2Server(&fakeAuth{}, &fakeLibrary{}, &fakePlaylists{}, gateway)
	results := make(chan *discardResponseWriter, streamCount)
	var wait sync.WaitGroup
	wait.Add(streamCount)
	for range streamCount {
		go func() {
			defer wait.Done()
			request := httptest.NewRequest(http.MethodGet, "/audio/song-1?format=original", nil)
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)})
			writer := &discardResponseWriter{header: make(http.Header)}
			server.Handler().ServeHTTP(writer, request)
			results <- writer
		}()
	}
	wait.Wait()
	close(results)
	for writer := range results {
		if writer.status != http.StatusOK || writer.written != streamSize || writer.maximumWrite > 64<<10 {
			t.Fatalf("concurrent stream status=%d bytes=%d maxWrite=%d", writer.status, writer.written, writer.maximumWrite)
		}
	}
	close(readers)
	for reader := range readers {
		if reader.maximumRead > 64<<10 {
			t.Fatalf("concurrent stream maxRead=%d", reader.maximumRead)
		}
	}
}

func m2Server(authService *fakeAuth, libraryService LibraryService, playlistService PlaylistService, gateway StreamingGateway) *Server {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(logger, authService, &fakeMembership{}, "http://127.0.0.1:5173", false, time.Hour, Services{
		Library: libraryService, Playlists: playlistService, Streaming: gateway,
	})
}

type generatedReadCloser struct {
	remaining   int64
	maximumRead int
}

func (r *generatedReadCloser) Read(buffer []byte) (int, error) {
	if len(buffer) > r.maximumRead {
		r.maximumRead = len(buffer)
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	count := len(buffer)
	if int64(count) > r.remaining {
		count = int(r.remaining)
	}
	r.remaining -= int64(count)
	return count, nil
}
func (*generatedReadCloser) Close() error { return nil }

type discardResponseWriter struct {
	header       http.Header
	status       int
	written      int64
	maximumWrite int
	mu           sync.Mutex
}

func (w *discardResponseWriter) Header() http.Header { return w.header }
func (w *discardResponseWriter) WriteHeader(status int) {
	w.status = status
}
func (w *discardResponseWriter) Write(buffer []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.written += int64(len(buffer))
	if len(buffer) > w.maximumWrite {
		w.maximumWrite = len(buffer)
	}
	return len(buffer), nil
}
