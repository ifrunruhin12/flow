package streaming

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
)

type authenticateStreamFunc func(context.Context, string) (auth.Principal, error)

func (f authenticateStreamFunc) AuthenticateStream(ctx context.Context, token string) (auth.Principal, error) {
	return f(ctx, token)
}

type fakeBackend struct {
	song    func(context.Context, int64, string) (navidrome.Song, error)
	stream  func(context.Context, int64, string, navidrome.StreamOptions) (navidrome.BinaryResponse, error)
	artwork func(context.Context, int64, string, navidrome.ArtworkOptions) (navidrome.BinaryResponse, error)
}

func (f *fakeBackend) Song(ctx context.Context, userID int64, id string) (navidrome.Song, error) {
	return f.song(ctx, userID, id)
}

func (f *fakeBackend) OpenStream(ctx context.Context, userID int64, id string, options navidrome.StreamOptions) (navidrome.BinaryResponse, error) {
	return f.stream(ctx, userID, id, options)
}

func (f *fakeBackend) OpenArtwork(ctx context.Context, userID int64, id string, options navidrome.ArtworkOptions) (navidrome.BinaryResponse, error) {
	return f.artwork(ctx, userID, id, options)
}

func TestGatewayRegistersBeforeNavidromeAndUsesMemberIdentity(t *testing.T) {
	registry := NewRegistry(2)
	principal := streamPrincipal(27)
	var authentications atomic.Int32
	var receivedOptions navidrome.StreamOptions
	backend := &fakeBackend{
		song: func(_ context.Context, userID int64, mediaID string) (navidrome.Song, error) {
			if registry.ActiveForUser(userID) != 1 {
				t.Fatal("Navidrome media lookup occurred before stream registration")
			}
			if userID != principal.User.ID || mediaID != "media-1" {
				t.Fatalf("lookup identity=(%d,%q)", userID, mediaID)
			}
			return navidrome.Song{ID: mediaID}, nil
		},
		stream: func(_ context.Context, userID int64, mediaID string, options navidrome.StreamOptions) (navidrome.BinaryResponse, error) {
			if userID != principal.User.ID || mediaID != "media-1" {
				t.Fatalf("stream identity=(%d,%q)", userID, mediaID)
			}
			receivedOptions = options
			return navidrome.BinaryResponse{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Type": {"audio/flac"}}, Body: io.NopCloser(strings.NewReader("audio"))}, nil
		},
	}
	gateway := NewGateway(authenticateStreamFunc(func(context.Context, string) (auth.Principal, error) {
		authentications.Add(1)
		return principal, nil
	}), registry, backend)

	opened, err := gateway.OpenAudio(context.Background(), "session-token", AudioRequest{
		Method: http.MethodGet, MediaID: "media-1", Format: "original", Range: "bytes=0-31",
	})
	if err != nil {
		t.Fatal(err)
	}
	if authentications.Load() != 2 {
		t.Fatalf("authentication calls = %d, want 2", authentications.Load())
	}
	if receivedOptions.Format != "raw" || receivedOptions.Range != "bytes=0-31" {
		t.Fatalf("stream options = %#v", receivedOptions)
	}
	opened.Close()
	if got := registry.ActiveForUser(principal.User.ID); got != 0 {
		t.Fatalf("active streams after close = %d", got)
	}
}

func TestGatewaySuspensionRacingWithMediaLookupCancelsBeforeOpen(t *testing.T) {
	registry := NewRegistry(2)
	principal := streamPrincipal(31)
	lookupStarted := make(chan struct{})
	streamOpened := atomic.Bool{}
	backend := &fakeBackend{
		song: func(ctx context.Context, _ int64, _ string) (navidrome.Song, error) {
			close(lookupStarted)
			<-ctx.Done()
			return navidrome.Song{}, ctx.Err()
		},
		stream: func(context.Context, int64, string, navidrome.StreamOptions) (navidrome.BinaryResponse, error) {
			streamOpened.Store(true)
			return navidrome.BinaryResponse{}, nil
		},
	}
	gateway := NewGateway(authenticateStreamFunc(func(context.Context, string) (auth.Principal, error) {
		return principal, nil
	}), registry, backend)

	result := make(chan error, 1)
	go func() {
		_, err := gateway.OpenAudio(context.Background(), "session-token", AudioRequest{MediaID: "media-1", Format: "mp3"})
		result <- err
	}()
	<-lookupStarted
	registry.BlockAndAbortUser(principal.User.ID)
	if err := <-result; !errors.Is(err, auth.ErrAccountNotActive) {
		t.Fatalf("OpenAudio() error = %v", err)
	}
	if streamOpened.Load() {
		t.Fatal("upstream stream opened after concurrent suspension")
	}
	if got := registry.ActiveForUser(principal.User.ID); got != 0 {
		t.Fatalf("active streams = %d", got)
	}
}

func TestGatewaySuspensionRacingWithPostRegistrationRecheckIsForbidden(t *testing.T) {
	registry := NewRegistry(2)
	principal := streamPrincipal(37)
	recheckStarted := make(chan struct{})
	var calls atomic.Int32
	authenticator := authenticateStreamFunc(func(ctx context.Context, _ string) (auth.Principal, error) {
		if calls.Add(1) == 1 {
			return principal, nil
		}
		close(recheckStarted)
		<-ctx.Done()
		return auth.Principal{}, ctx.Err()
	})
	gateway := NewGateway(authenticator, registry, &fakeBackend{
		song: func(context.Context, int64, string) (navidrome.Song, error) {
			t.Fatal("media lookup ran after canceled account recheck")
			return navidrome.Song{}, nil
		},
		stream: func(context.Context, int64, string, navidrome.StreamOptions) (navidrome.BinaryResponse, error) {
			t.Fatal("stream opened after canceled account recheck")
			return navidrome.BinaryResponse{}, nil
		},
	})
	result := make(chan error, 1)
	go func() {
		_, err := gateway.OpenAudio(context.Background(), "session-token", AudioRequest{MediaID: "media-1", Format: "mp3"})
		result <- err
	}()
	<-recheckStarted
	registry.BlockAndAbortUser(principal.User.ID)
	if err := <-result; !errors.Is(err, auth.ErrAccountNotActive) {
		t.Fatalf("OpenAudio() error = %v", err)
	}
}

func TestGatewaySuspensionTerminatesActiveStream(t *testing.T) {
	registry := NewRegistry(2)
	principal := streamPrincipal(52)
	readStarted := make(chan struct{})
	body := &blockingStreamBody{started: readStarted}
	backend := &fakeBackend{
		song: func(context.Context, int64, string) (navidrome.Song, error) {
			return navidrome.Song{ID: "media-1"}, nil
		},
		stream: func(ctx context.Context, _ int64, _ string, _ navidrome.StreamOptions) (navidrome.BinaryResponse, error) {
			body.ctx = ctx
			return navidrome.BinaryResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: body}, nil
		},
	}
	gateway := NewGateway(authenticateStreamFunc(func(context.Context, string) (auth.Principal, error) {
		return principal, nil
	}), registry, backend)
	opened, err := gateway.OpenAudio(context.Background(), "session-token", AudioRequest{MediaID: "media-1", Format: "mp3"})
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, opened.Response.Body)
		readDone <- err
	}()
	<-readStarted
	registry.BlockAndAbortUser(principal.User.ID)
	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active stream read error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active upstream stream survived suspension")
	}
	opened.Close()
}

func TestGatewayAccountStatePrecedesRequestValidation(t *testing.T) {
	for _, expected := range []error{auth.ErrUnauthenticated, auth.ErrAccountNotActive} {
		gateway := NewGateway(authenticateStreamFunc(func(context.Context, string) (auth.Principal, error) {
			return auth.Principal{}, expected
		}), NewRegistry(1), &fakeBackend{})
		_, err := gateway.OpenAudio(context.Background(), "token", AudioRequest{MediaID: "../invalid", InvalidQuery: true})
		if !errors.Is(err, expected) {
			t.Fatalf("authentication error = %v, want %v", err, expected)
		}
	}
}

func TestGatewayValidatesFormatsRatesOffsetsAndRanges(t *testing.T) {
	var authentications atomic.Int32
	gateway := NewGateway(authenticateStreamFunc(func(context.Context, string) (auth.Principal, error) {
		authentications.Add(1)
		return streamPrincipal(61), nil
	}), NewRegistry(1), &fakeBackend{})
	tests := []AudioRequest{
		{MediaID: "../secret", Format: "mp3"},
		{MediaID: "media-1", Format: "flac"},
		{MediaID: "media-1", Format: "original", MaxBitRate: "128"},
		{MediaID: "media-1", Format: "original", OffsetSeconds: "1"},
		{MediaID: "media-1", Format: "mp3", MaxBitRate: "321"},
		{MediaID: "media-1", Format: "mp3", MaxBitRate: "0"},
		{MediaID: "media-1", Format: "opus", OffsetSeconds: "86401"},
		{MediaID: "media-1", Format: "original", OffsetSeconds: "0"},
		{MediaID: "media-1", Format: "mp3", MaxBitRate: "032"},
		{MediaID: "media-1", Format: "mp3", Range: "bytes=0-1,4-5"},
		{MediaID: "media-1", Format: "mp3", Range: "bytes=-0"},
		{MediaID: "media-1", Format: "mp3", Range: "bytes=9-1"},
		{MediaID: "media-1", Format: "mp3", Range: "bytes=" + strings.Repeat("9", 129) + "-"},
	}
	for _, request := range tests {
		if _, err := gateway.OpenAudio(context.Background(), "token", request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("request %#v error = %v", request, err)
		}
	}
	if authentications.Load() != int32(len(tests)) {
		t.Fatalf("authentication calls = %d, want %d", authentications.Load(), len(tests))
	}
}

func TestGatewayAutoUsesSafeMP3Default(t *testing.T) {
	options, err := validateAudioRequest(AudioRequest{MediaID: "media-1", Format: "auto", MaxBitRate: "96", OffsetSeconds: "5", Range: "bytes=0-"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Format != "mp3" || options.MaxBitRate != 96 || options.OffsetSeconds != 5 || options.Range != "bytes=0-" {
		t.Fatalf("options = %#v", options)
	}
}

func streamPrincipal(userID int64) auth.Principal {
	return auth.Principal{
		User:      auth.User{ID: userID, Role: "member", Status: "active", ProvisioningState: "complete"},
		TokenHash: []byte("01234567890123456789012345678901"),
	}
}

type blockingStreamBody struct {
	ctx     context.Context
	started chan struct{}
	once    atomic.Bool
}

func (b *blockingStreamBody) Read([]byte) (int, error) {
	if b.once.CompareAndSwap(false, true) {
		close(b.started)
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*blockingStreamBody) Close() error { return nil }
