package navidrome

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdapterAlbumsUsesVerifiedSubsonicShapeWithoutInventedHeaders(t *testing.T) {
	adapter := testAdapterTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/auth/login":
			return loginResponse("member", testJWT(time.Now().Add(time.Hour))), nil
		case "/rest/getAlbumList2":
			query := r.URL.Query()
			if query.Get("u") != "member" || query.Get("jwt") == "" || query.Get("type") != "alphabeticalByName" || query.Get("offset") != "10" || query.Get("size") != "3" {
				t.Fatalf("unexpected getAlbumList2 query: %v", query)
			}
			return jsonResponse(http.StatusOK, `{"subsonic-response":{"status":"ok","albumList2":{"album":[{"id":"album-1","name":"","artist":"Artist","duration":12}]}}}`), nil
		default:
			return textResponse(http.StatusNotFound, "not found"), nil
		}
	})

	page, err := adapter.Albums(context.Background(), 7, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "album-1" || page.Items[0].Name != "" {
		t.Fatalf("albums = %#v", page.Items)
	}
}

func TestAdapterArtistUsesArtistNameForTopSongs(t *testing.T) {
	adapter := testAdapterTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/auth/login":
			return loginResponse("member", testJWT(time.Now().Add(time.Hour))), nil
		case "/rest/getArtist":
			return jsonResponse(http.StatusOK, `{"subsonic-response":{"status":"ok","artist":{"id":"artist-1","name":"Björk","album":[]}}}`), nil
		case "/rest/getTopSongs":
			if got := r.URL.Query().Get("artist"); got != "Björk" {
				t.Fatalf("getTopSongs artist = %q", got)
			}
			if got := r.URL.Query().Get("id"); got != "" {
				t.Fatalf("getTopSongs sent non-contract id parameter %q", got)
			}
			return jsonResponse(http.StatusOK, `{"subsonic-response":{"status":"ok","topSongs":{"song":[]}}}`), nil
		default:
			return textResponse(http.StatusNotFound, "not found"), nil
		}
	})

	artist, err := adapter.Artist(context.Background(), 7, "artist-1")
	if err != nil {
		t.Fatal(err)
	}
	if artist.ID != "artist-1" || artist.Name != "Björk" || artist.TopTracks == nil {
		t.Fatalf("artist = %#v", artist)
	}
}

func TestAdapterRetriesAuthenticationOnce(t *testing.T) {
	var logins atomic.Int32
	var requests atomic.Int32
	adapter := testAdapterTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/auth/login":
			call := logins.Add(1)
			return loginResponse("member", testJWT(time.Now().Add(time.Duration(call)*time.Hour))), nil
		case "/rest/getSong":
			if requests.Add(1) == 1 {
				return textResponse(http.StatusUnauthorized, "denied"), nil
			}
			return jsonResponse(http.StatusOK, `{"subsonic-response":{"status":"ok","song":{"id":"song-1","title":"Track"}}}`), nil
		default:
			return textResponse(http.StatusNotFound, "not found"), nil
		}
	})

	item, err := adapter.Song(context.Background(), 12, "song-1")
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "song-1" || logins.Load() != 2 || requests.Load() != 2 {
		t.Fatalf("item=%#v logins=%d requests=%d", item, logins.Load(), requests.Load())
	}
}

func TestAdapterStreamPreservesRepresentationAndSanitizesRangeErrorBody(t *testing.T) {
	adapter := testAdapterTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/auth/login":
			return loginResponse("member", testJWT(time.Now().Add(time.Hour))), nil
		case "/rest/stream":
			if r.URL.Query().Get("format") != "raw" || r.Header.Get("Range") != "bytes=0-31" {
				t.Fatalf("stream query=%v range=%q", r.URL.Query(), r.Header.Get("Range"))
			}
			if r.URL.Query().Get("id") == "missing" {
				response := textResponse(http.StatusRequestedRangeNotSatisfiable, "internal http://navidrome:4533 error")
				response.Header.Set("Content-Range", "bytes */100")
				response.Header.Set("Content-Length", "45")
				return response, nil
			}
			response := textResponse(http.StatusPartialContent, "audio")
			response.Header.Set("Content-Type", "audio/flac")
			response.Header.Set("Accept-Ranges", "bytes")
			response.Header.Set("Content-Range", "bytes 0-31/100")
			return response, nil
		default:
			return textResponse(http.StatusNotFound, "not found"), nil
		}
	})

	response, err := adapter.OpenStream(context.Background(), 3, "song-1", StreamOptions{Format: "raw", Range: "bytes=0-31"})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent || response.Header.Get("Content-Range") != "bytes 0-31/100" || response.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("stream response = %d %#v", response.StatusCode, response.Header)
	}

	unsatisfied, err := adapter.OpenStream(context.Background(), 3, "missing", StreamOptions{Format: "raw", Range: "bytes=0-31"})
	if err != nil {
		t.Fatal(err)
	}
	defer unsatisfied.Body.Close()
	body, _ := io.ReadAll(unsatisfied.Body)
	if unsatisfied.StatusCode != http.StatusRequestedRangeNotSatisfiable || unsatisfied.Header.Get("Content-Range") != "bytes */100" || unsatisfied.Header.Get("Content-Type") != "" || len(body) != 0 {
		t.Fatalf("416 response=%d headers=%v body=%q", unsatisfied.StatusCode, unsatisfied.Header, body)
	}
}

func TestAdapterStreamCancellationReachesTransportBody(t *testing.T) {
	requestStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	adapter := testAdapterTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/auth/login":
			return loginResponse("member", testJWT(time.Now().Add(time.Hour))), nil
		case "/rest/stream":
			close(requestStarted)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"audio/mpeg"}},
				Body:       &cancelAwareBody{ctx: r.Context(), canceled: upstreamCanceled},
			}, nil
		default:
			return textResponse(http.StatusNotFound, "not found"), nil
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	response, err := adapter.OpenStream(ctx, 3, "song-1", StreamOptions{Format: "mp3"})
	if err != nil {
		t.Fatal(err)
	}
	<-requestStarted
	cancel()
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("request cancellation did not reach upstream body")
	}
}

func TestAdapterMalformedAndRateLimitedResponses(t *testing.T) {
	adapter := testAdapterTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/auth/login":
			return loginResponse("member", testJWT(time.Now().Add(time.Hour))), nil
		case "/rest/getSong":
			return textResponse(http.StatusOK, "not-json"), nil
		case "/rest/search3":
			response := textResponse(http.StatusTooManyRequests, "slow down")
			response.Header.Set("Retry-After", "7")
			return response, nil
		default:
			return textResponse(http.StatusNotFound, "not found"), nil
		}
	})
	if _, err := adapter.Song(context.Background(), 1, "song-1"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed Song() error = %v", err)
	}
	_, err := adapter.Search(context.Background(), 1, "query", 1, 1, 1, 0, 0, 0)
	var rateLimit *RateLimitError
	if !errors.As(err, &rateLimit) || rateLimit.RetryAfter != "7" {
		t.Fatalf("rate-limit error = %#v", err)
	}
}

func TestAdapterRejectsMediaObjectsWithoutStableIDs(t *testing.T) {
	adapter := testAdapterTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/auth/login":
			return loginResponse("member", testJWT(time.Now().Add(time.Hour))), nil
		case "/rest/getAlbumList2":
			return jsonResponse(http.StatusOK, `{"subsonic-response":{"status":"ok","albumList2":{"album":[{"name":"Missing ID"}]}}}`), nil
		default:
			return textResponse(http.StatusNotFound, "not found"), nil
		}
	})
	if _, err := adapter.Albums(context.Background(), 1, 0, 10); !errors.Is(err, ErrMalformed) {
		t.Fatalf("ID-less album error = %v", err)
	}
}

func TestAdapterTransportErrorsDoNotLeakInternalAddress(t *testing.T) {
	client, err := NewWithHTTPClient("http://navidrome.internal:4533", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp navidrome.internal:4533: connection refused")
	})})
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCredentialCache(credentialSourceFunc(func(context.Context, int64) (PasswordCredential, error) {
		return PasswordCredential{Username: "member", Password: "secret"}, nil
	}), client)
	_, err = cache.Get(context.Background(), 1)
	if err == nil || strings.Contains(err.Error(), "navidrome.internal") || strings.Contains(err.Error(), "4533") {
		t.Fatalf("transport error leaked internal address: %v", err)
	}
}

func TestAdapterMapsUpstreamTimeout(t *testing.T) {
	adapter := testAdapterTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/auth/login" {
			return loginResponse("member", testJWT(time.Now().Add(time.Hour))), nil
		}
		return nil, context.DeadlineExceeded
	})
	if _, err := adapter.Song(context.Background(), 1, "song-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
}

func testAdapterTransport(t *testing.T, transport roundTripFunc) *Adapter {
	t.Helper()
	client, err := NewWithHTTPClient("http://navidrome.internal:4533", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCredentialCache(credentialSourceFunc(func(_ context.Context, userID int64) (PasswordCredential, error) {
		if userID < 1 {
			return PasswordCredential{}, ErrCredentialUnavailable
		}
		return PasswordCredential{Username: "member", Password: "member-password"}, nil
	}), client)
	return NewAdapter(client, cache)
}

func loginResponse(username, token string) *http.Response {
	return jsonResponse(http.StatusOK, `{"id":"user-1","username":"`+username+`","token":"`+token+`"}`)
}

func jsonResponse(status int, body string) *http.Response {
	response := textResponse(status, body)
	response.Header.Set("Content-Type", "application/json")
	return response
}

func textResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

type cancelAwareBody struct {
	ctx      context.Context
	canceled chan struct{}
	once     sync.Once
}

func (b *cancelAwareBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	b.once.Do(func() { close(b.canceled) })
	return 0, b.ctx.Err()
}

func (b *cancelAwareBody) Close() error {
	b.once.Do(func() { close(b.canceled) })
	return nil
}
