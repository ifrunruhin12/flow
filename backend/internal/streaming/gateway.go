package streaming

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
)

var (
	ErrInvalidRequest         = errors.New("invalid streaming request")
	ErrPasswordChangeRequired = errors.New("password change required")
)

type Authenticator interface {
	AuthenticateStream(context.Context, string) (auth.Principal, error)
}

type Backend interface {
	Song(context.Context, int64, string) (navidrome.Song, error)
	OpenStream(context.Context, int64, string, navidrome.StreamOptions) (navidrome.BinaryResponse, error)
	OpenArtwork(context.Context, int64, string, navidrome.ArtworkOptions) (navidrome.BinaryResponse, error)
}

type Gateway struct {
	auth     Authenticator
	registry *Registry
	backend  Backend
}

func NewGateway(authenticator Authenticator, registry *Registry, backend Backend) *Gateway {
	return &Gateway{auth: authenticator, registry: registry, backend: backend}
}

type AudioRequest struct {
	Method        string
	MediaID       string
	Format        string
	MaxBitRate    string
	OffsetSeconds string
	Range         string
	InvalidQuery  bool
}

type OpenedAudio struct {
	Response     navidrome.BinaryResponse
	registration *Registration
	once         sync.Once
}

func (s *OpenedAudio) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.Response.Body != nil {
			_ = s.Response.Body.Close()
		}
		if s.registration != nil {
			s.registration.Close()
		}
	})
}

// OpenAudio authenticates, validates, registers, then re-authenticates before
// making any Navidrome request. This ordering closes the authorization-to-
// registration race: suspension either prevents registration or cancels the
// registered context before an opened upstream can escape lifecycle management.
func (g *Gateway) OpenAudio(ctx context.Context, rawToken string, request AudioRequest) (*OpenedAudio, error) {
	principal, err := g.auth.AuthenticateStream(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	if principal.User.MustChangePassword {
		return nil, ErrPasswordChangeRequired
	}
	if len(principal.TokenHash) != sha256.Size {
		return nil, auth.ErrUnauthenticated
	}
	options, err := validateAudioRequest(request)
	if err != nil {
		return nil, err
	}
	sessionID := hex.EncodeToString(principal.TokenHash)
	registration, err := g.registry.Register(ctx, principal.User.ID, sessionID)
	if err != nil {
		return nil, registryAuthError(err)
	}
	release := true
	defer func() {
		if release {
			registration.Close()
		}
	}()

	confirmed, err := g.auth.AuthenticateStream(registration.Context(), rawToken)
	if err != nil {
		return nil, cancellationError(registration.Context(), err)
	}
	if confirmed.User.MustChangePassword {
		return nil, ErrPasswordChangeRequired
	}
	if confirmed.User.ID != principal.User.ID || subtle.ConstantTimeCompare(confirmed.TokenHash, principal.TokenHash) != 1 {
		return nil, auth.ErrUnauthenticated
	}

	// getSong is the member-scoped authorization/existence check. It happens
	// only after registration and the authoritative account-state recheck.
	if _, err := g.backend.Song(registration.Context(), principal.User.ID, request.MediaID); err != nil {
		return nil, cancellationError(registration.Context(), err)
	}
	response, err := g.backend.OpenStream(registration.Context(), principal.User.ID, request.MediaID, options)
	if err != nil {
		return nil, cancellationError(registration.Context(), err)
	}
	release = false
	return &OpenedAudio{Response: response, registration: registration}, nil
}

func (g *Gateway) OpenArtwork(ctx context.Context, userID int64, artworkID string, options navidrome.ArtworkOptions) (navidrome.BinaryResponse, error) {
	if userID < 1 || !library.ValidArtworkID(artworkID) || (options.Method != "" && options.Method != http.MethodGet && options.Method != http.MethodHead) {
		return navidrome.BinaryResponse{}, ErrInvalidRequest
	}
	return g.backend.OpenArtwork(ctx, userID, artworkID, options)
}

func validateAudioRequest(request AudioRequest) (navidrome.StreamOptions, error) {
	if request.InvalidQuery || !library.ValidResourceID(request.MediaID) {
		return navidrome.StreamOptions{}, ErrInvalidRequest
	}
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodHead {
		return navidrome.StreamOptions{}, ErrInvalidRequest
	}
	if request.Range != "" && !validSingleByteRange(request.Range) {
		return navidrome.StreamOptions{}, ErrInvalidRequest
	}
	maxBitRate, valid := unsignedParameter(request.MaxBitRate)
	if !valid || request.MaxBitRate != "" && (maxBitRate < 32 || maxBitRate > 320) {
		return navidrome.StreamOptions{}, ErrInvalidRequest
	}
	offsetSeconds, valid := unsignedParameter(request.OffsetSeconds)
	if !valid || offsetSeconds > 24*60*60 {
		return navidrome.StreamOptions{}, ErrInvalidRequest
	}

	format := request.Format
	upstreamFormat := ""
	switch format {
	case "", "auto":
		// V1 has no persisted format preference yet. Auto therefore chooses MP3,
		// the cross-browser safe transcode; the web client requests original
		// explicitly after a positive FLAC capability check.
		upstreamFormat = "mp3"
	case "original":
		// Pinned Navidrome v0.64.2 explicitly defines format=raw as the
		// original-file representation. Omitting format can select a player
		// preference, so Flow makes original passthrough explicit.
		upstreamFormat = "raw"
	case "mp3", "opus":
		upstreamFormat = format
	default:
		return navidrome.StreamOptions{}, ErrInvalidRequest
	}
	if upstreamFormat == "raw" && (request.MaxBitRate != "" || request.OffsetSeconds != "") {
		return navidrome.StreamOptions{}, ErrInvalidRequest
	}
	return navidrome.StreamOptions{
		Method: method, Format: upstreamFormat, MaxBitRate: maxBitRate,
		OffsetSeconds: offsetSeconds, Range: request.Range,
	}, nil
}

func unsignedParameter(raw string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	value, err := strconv.Atoi(raw)
	return value, err == nil && value >= 0 && strconv.Itoa(value) == raw
}

func validSingleByteRange(value string) bool {
	if len(value) > 128 || !strings.HasPrefix(value, "bytes=") {
		return false
	}
	spec := strings.TrimPrefix(value, "bytes=")
	if spec == "" || strings.Contains(spec, ",") {
		return false
	}
	if strings.HasPrefix(spec, "-") {
		suffix, err := strconv.ParseUint(strings.TrimPrefix(spec, "-"), 10, 64)
		return err == nil && suffix > 0
	}
	startRaw, endRaw, found := strings.Cut(spec, "-")
	if !found || startRaw == "" {
		return false
	}
	start, err := strconv.ParseUint(startRaw, 10, 64)
	if err != nil {
		return false
	}
	if endRaw == "" {
		return true
	}
	end, err := strconv.ParseUint(endRaw, 10, 64)
	return err == nil && end >= start
}

func registryAuthError(err error) error {
	switch {
	case errors.Is(err, ErrUserBlocked):
		return auth.ErrAccountNotActive
	case errors.Is(err, ErrSessionRevoked):
		return auth.ErrUnauthenticated
	default:
		return err
	}
}

func cancellationError(ctx context.Context, fallback error) error {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, ErrUserBlocked):
		return auth.ErrAccountNotActive
	case errors.Is(cause, ErrSessionRevoked):
		return auth.ErrUnauthenticated
	default:
		return fallback
	}
}
