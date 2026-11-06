package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/membership"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
	"github.com/ifrunruhin12/flow/backend/internal/playback"
	"github.com/ifrunruhin12/flow/backend/internal/playlist"
	"github.com/ifrunruhin12/flow/backend/internal/streaming"
)

const sessionCookie = "flow_session"

type AuthService interface {
	Login(context.Context, string, string, string) (auth.LoginResult, error)
	Authenticate(context.Context, string) (auth.Principal, error)
	Logout(context.Context, string) error
	ChangePassword(context.Context, auth.Principal, string, string) error
}

type MembershipService interface {
	RegisterRequest(context.Context, string, string, string) (int64, error)
	ListRequests(context.Context, string) ([]membership.Request, error)
	ApproveRequest(context.Context, int64, int64, string, string, string) (membership.User, error)
	RejectRequest(context.Context, int64, int64, string, string) error
	CreateMember(context.Context, int64, membership.NewMember, string) (membership.User, error)
	ListUsers(context.Context, membership.UserFilter) ([]membership.User, error)
	Suspend(context.Context, int64, int64, string, string) error
	Reactivate(context.Context, int64, int64, string) error
	Provision(context.Context, int64, int64, string) error
	ResetPassword(context.Context, int64, int64, string, string) error
	Delete(context.Context, int64, int64, bool, string) error
}

type LibraryService interface {
	Albums(context.Context, int64, string, int) (library.AlbumPage, error)
	Album(context.Context, int64, string) (library.AlbumDetail, error)
	Artists(context.Context, int64, string, int, string) (library.ArtistPage, error)
	Artist(context.Context, int64, string) (library.ArtistDetail, error)
	Songs(context.Context, int64, string, int, string) (library.SongPage, error)
	Song(context.Context, int64, string) (library.Song, error)
	Search(context.Context, int64, string, int, int, int, int) (library.SearchResult, error)
	SetStar(context.Context, int64, string, string, bool) error
	Starred(context.Context, int64, string) (library.SearchResult, error)
	ScanStatus(context.Context, int64) (library.ScanStatus, error)
}

type PlaylistService interface {
	List(context.Context, int64) ([]playlist.Item, error)
	Get(context.Context, int64, string) (playlist.Item, error)
	Create(context.Context, int64, string, string, []string) (playlist.Item, error)
	Update(context.Context, int64, string, playlist.Update) (playlist.Item, error)
	Delete(context.Context, int64, string) error
}

type StreamingGateway interface {
	OpenAudio(context.Context, string, streaming.AudioRequest) (*streaming.OpenedAudio, error)
	OpenArtwork(context.Context, int64, string, navidrome.ArtworkOptions) (navidrome.BinaryResponse, error)
}

type PlaybackService interface {
	Get(context.Context, auth.Principal) (playback.Session, error)
	Update(context.Context, auth.Principal, playback.Mutation) (playback.Session, error)
	Clear(context.Context, auth.Principal, *int64) (playback.Session, error)
}

type Services struct {
	Library   LibraryService
	Playlists PlaylistService
	Streaming StreamingGateway
	Playback  PlaybackService
}

type Server struct {
	logger          *slog.Logger
	auth            AuthService
	members         MembershipService
	library         LibraryService
	playlists       PlaylistService
	streaming       StreamingGateway
	playback        PlaybackService
	publicOrigin    string
	cookieSecure    bool
	sessionTTL      time.Duration
	loginIPLimit    *fixedWindowLimiter
	loginUserLimit  *fixedWindowLimiter
	registerIPLimit *fixedWindowLimiter
	handler         http.Handler
}

type contextKey string

const (
	principalKey   contextKey = "principal"
	correlationKey contextKey = "correlation"
)

func New(logger *slog.Logger, authService AuthService, membershipService MembershipService, publicOrigin string, cookieSecure bool, sessionTTL time.Duration, optional ...Services) *Server {
	server := &Server{
		logger:          logger,
		auth:            authService,
		members:         membershipService,
		publicOrigin:    strings.TrimRight(publicOrigin, "/"),
		cookieSecure:    cookieSecure,
		sessionTTL:      sessionTTL,
		loginIPLimit:    newFixedWindowLimiter(10, 15*time.Minute),
		loginUserLimit:  newFixedWindowLimiter(20, 15*time.Minute),
		registerIPLimit: newFixedWindowLimiter(3, 24*time.Hour),
	}
	if len(optional) > 0 {
		server.library = optional[0].Library
		server.playlists = optional[0].Playlists
		server.streaming = optional[0].Streaming
		server.playback = optional[0].Playback
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", server.login)
	mux.HandleFunc("POST /api/auth/register-request", server.registerRequest)
	mux.Handle("GET /api/auth/me", server.authenticated(http.HandlerFunc(server.me)))
	mux.Handle("POST /api/auth/logout", server.originChecked(server.authenticated(http.HandlerFunc(server.logout))))
	mux.Handle("POST /api/auth/change-password", server.originChecked(server.authenticated(http.HandlerFunc(server.changePassword))))

	mux.Handle("GET /api/admin/requests", server.admin(http.HandlerFunc(server.listRequests)))
	mux.Handle("POST /api/admin/requests/{id}/approve", server.originChecked(server.admin(http.HandlerFunc(server.approveRequest))))
	mux.Handle("POST /api/admin/requests/{id}/reject", server.originChecked(server.admin(http.HandlerFunc(server.rejectRequest))))
	mux.Handle("GET /api/admin/users", server.admin(http.HandlerFunc(server.listUsers)))
	mux.Handle("POST /api/admin/users", server.originChecked(server.admin(http.HandlerFunc(server.createUser))))
	mux.Handle("POST /api/admin/users/{id}/suspend", server.originChecked(server.admin(http.HandlerFunc(server.suspendUser))))
	mux.Handle("POST /api/admin/users/{id}/reactivate", server.originChecked(server.admin(http.HandlerFunc(server.reactivateUser))))
	mux.Handle("POST /api/admin/users/{id}/retry-provisioning", server.originChecked(server.admin(http.HandlerFunc(server.retryProvisioning))))
	mux.Handle("POST /api/admin/users/{id}/reset-password", server.originChecked(server.admin(http.HandlerFunc(server.resetPassword))))
	mux.Handle("DELETE /api/admin/users/{id}", server.originChecked(server.admin(http.HandlerFunc(server.deleteUser))))
	if server.library != nil {
		mux.Handle("GET /api/library/albums", server.authenticated(http.HandlerFunc(server.listAlbums)))
		mux.Handle("GET /api/library/albums/{albumId}", server.authenticated(http.HandlerFunc(server.getAlbum)))
		mux.Handle("GET /api/library/artists", server.authenticated(http.HandlerFunc(server.listArtists)))
		mux.Handle("GET /api/library/artists/{artistId}", server.authenticated(http.HandlerFunc(server.getArtist)))
		mux.Handle("GET /api/library/songs", server.authenticated(http.HandlerFunc(server.listSongs)))
		mux.Handle("GET /api/library/songs/{songId}", server.authenticated(http.HandlerFunc(server.getSong)))
		mux.Handle("GET /api/library/search", server.authenticated(http.HandlerFunc(server.searchLibrary)))
		mux.Handle("GET /api/library/stars", server.authenticated(http.HandlerFunc(server.listStars)))
		mux.Handle("POST /api/library/stars", server.originChecked(server.authenticated(http.HandlerFunc(server.star))))
		mux.Handle("DELETE /api/library/stars/{kind}/{mediaId}", server.originChecked(server.authenticated(http.HandlerFunc(server.unstar))))
		mux.Handle("GET /api/library/scanner/status", server.authenticated(http.HandlerFunc(server.scanStatus)))
	}
	if server.playlists != nil {
		mux.Handle("GET /api/playlists", server.authenticated(http.HandlerFunc(server.listPlaylists)))
		mux.Handle("GET /api/playlists/{id}", server.authenticated(http.HandlerFunc(server.getPlaylist)))
		mux.Handle("POST /api/playlists", server.originChecked(server.admin(http.HandlerFunc(server.createPlaylist))))
		mux.Handle("PUT /api/playlists/{id}", server.originChecked(server.admin(http.HandlerFunc(server.updatePlaylist))))
		mux.Handle("DELETE /api/playlists/{id}", server.originChecked(server.admin(http.HandlerFunc(server.deletePlaylist))))
	}
	if server.streaming != nil {
		mux.HandleFunc("GET /audio/{mediaId}", server.audio)
		mux.Handle("GET /audio/coverart/{id}", server.authenticated(http.HandlerFunc(server.artwork)))
	}
	if server.playback != nil {
		mux.Handle("GET /api/playback/session", server.authenticated(http.HandlerFunc(server.getPlayback)))
		mux.Handle("PUT /api/playback/session", server.originChecked(server.authenticated(http.HandlerFunc(server.updatePlayback))))
		mux.Handle("DELETE /api/playback/session", server.originChecked(server.authenticated(http.HandlerFunc(server.clearPlayback))))
	}
	server.handler = server.correlated(mux)
	return server
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body is invalid.")
		return
	}
	usernameKey := strings.ToLower(strings.TrimSpace(body.Username))
	if runes := []rune(usernameKey); len(runes) > 128 {
		usernameKey = string(runes[:128])
	}
	if !s.loginIPLimit.Allow(clientIP(r)) || !s.loginUserLimit.Allow(usernameKey) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many login attempts. Try again later.")
		return
	}
	result, err := s.auth.Login(r.Context(), body.Username, body.Password, r.UserAgent())
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Invalid username or password.")
		return
	case errors.Is(err, auth.ErrAccountNotActive):
		writeError(w, http.StatusForbidden, "account_not_active", "This account is not active.")
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "login failed", "error", err, "correlation_id", correlationID(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal_error", "Login could not be completed.")
		return
	}
	s.setSessionCookie(w, result.Token, result.ExpiresAt)
	writeJSON(w, http.StatusOK, map[string]any{"user": result.User})
}

func (s *Server) registerRequest(w http.ResponseWriter, r *http.Request) {
	if !s.registerIPLimit.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many membership requests. Try again later.")
		return
	}
	var body struct {
		DisplayName     string `json:"displayName"`
		Email           string `json:"email"`
		DesiredUsername string `json:"desiredUsername"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body is invalid.")
		return
	}
	id, err := s.members.RegisterRequest(r.Context(), body.DisplayName, body.Email, body.DesiredUsername)
	if s.handleMembershipError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"requestId": id})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	principal := principalFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"user": principal.User})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	rawToken := cookieToken(r)
	if err := s.auth.Logout(r.Context(), rawToken); err != nil {
		s.logger.ErrorContext(r.Context(), "logout failed", "error", err, "correlation_id", correlationID(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal_error", "Logout could not be completed.")
		return
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body is invalid.")
		return
	}
	err := s.auth.ChangePassword(r.Context(), principalFrom(r.Context()), body.CurrentPassword, body.NewPassword)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "The current password is incorrect.")
	case errors.Is(err, auth.ErrPasswordUnchanged):
		writeError(w, http.StatusBadRequest, "invalid_request", "The new password must be different.")
	case errors.Is(err, auth.ErrInvalidPassword):
		writeError(w, http.StatusBadRequest, "invalid_request", "The new password does not meet the password policy.")
	case errors.Is(err, auth.ErrPasswordExternallyManaged):
		writeError(w, http.StatusForbidden, "forbidden", "The administrator password is managed by deployment configuration.")
	case err != nil:
		s.logger.ErrorContext(r.Context(), "password change failed", "error", err, "correlation_id", correlationID(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal_error", "The password could not be changed.")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	requests, err := s.members.ListRequests(r.Context(), r.URL.Query().Get("status"))
	if s.handleMembershipError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": requests})
}

func (s *Server) approveRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Note              string `json:"note"`
		TemporaryPassword string `json:"temporaryPassword"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body is invalid.")
		return
	}
	principal := principalFrom(r.Context())
	user, err := s.members.ApproveRequest(r.Context(), id, principal.User.ID, body.TemporaryPassword, body.Note, correlationID(r.Context()))
	if err != nil && user.ID != 0 {
		s.logger.WarnContext(r.Context(), "member approval provisioning failed", "error", err, "user_id", user.ID, "correlation_id", correlationID(r.Context()))
		if errors.Is(err, membership.ErrNavidromeUnavailable) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": "navidrome_unavailable", "message": "Navidrome integration must be verified before provisioning."}, "user": user})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": "provisioning_failed", "message": "The account was created, but Navidrome provisioning requires an admin retry."}, "user": user})
		return
	}
	if s.handleMembershipError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": user})
}

func (s *Server) rejectRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body is invalid.")
		return
	}
	principal := principalFrom(r.Context())
	err := s.members.RejectRequest(r.Context(), id, principal.User.ID, body.Note, correlationID(r.Context()))
	if s.handleMembershipError(w, r, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.members.ListUsers(r.Context(), membership.UserFilter{
		Status: r.URL.Query().Get("status"), Role: r.URL.Query().Get("role"), ProvisioningState: r.URL.Query().Get("provisioningState"),
	})
	if s.handleMembershipError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": users})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username          string `json:"username"`
		DisplayName       string `json:"displayName"`
		Email             string `json:"email"`
		TemporaryPassword string `json:"temporaryPassword"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body is invalid.")
		return
	}
	principal := principalFrom(r.Context())
	user, err := s.members.CreateMember(r.Context(), principal.User.ID, membership.NewMember{
		Username: body.Username, DisplayName: body.DisplayName, Email: body.Email, TemporaryPassword: body.TemporaryPassword,
	}, correlationID(r.Context()))
	if err != nil && user.ID != 0 {
		s.logger.WarnContext(r.Context(), "direct member provisioning failed", "error", err, "user_id", user.ID, "correlation_id", correlationID(r.Context()))
		if errors.Is(err, membership.ErrNavidromeUnavailable) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": "navidrome_unavailable", "message": "Navidrome integration must be verified before provisioning."}, "user": user})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": "provisioning_failed", "message": "The account was created, but Navidrome provisioning requires an admin retry."}, "user": user})
		return
	}
	if s.handleMembershipError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": user})
}

func (s *Server) suspendUser(w http.ResponseWriter, r *http.Request) {
	s.adminUserAction(w, r, func(ctx context.Context, userID, actorID int64, correlation string) error {
		var body struct {
			Reason string `json:"reason"`
		}
		if err := decodeOptionalBody(w, r, &body); err != nil {
			return fmt.Errorf("%w: body", membership.ErrInvalidInput)
		}
		return s.members.Suspend(ctx, userID, actorID, body.Reason, correlation)
	})
}

func (s *Server) reactivateUser(w http.ResponseWriter, r *http.Request) {
	s.adminUserAction(w, r, func(ctx context.Context, userID, actorID int64, correlation string) error {
		return s.members.Reactivate(ctx, userID, actorID, correlation)
	})
}

func (s *Server) retryProvisioning(w http.ResponseWriter, r *http.Request) {
	s.adminUserAction(w, r, func(ctx context.Context, userID, actorID int64, correlation string) error {
		return s.members.Provision(ctx, userID, actorID, correlation)
	})
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	s.adminUserAction(w, r, func(ctx context.Context, userID, actorID int64, correlation string) error {
		var body struct {
			TemporaryPassword string `json:"temporaryPassword"`
		}
		if err := decodeBody(w, r, &body); err != nil {
			return fmt.Errorf("%w: body", membership.ErrInvalidInput)
		}
		return s.members.ResetPassword(ctx, userID, actorID, body.TemporaryPassword, correlation)
	})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	hard := r.URL.Query().Get("hard") == "true"
	principal := principalFrom(r.Context())
	err := s.members.Delete(r.Context(), id, principal.User.ID, hard, correlationID(r.Context()))
	if errors.Is(err, membership.ErrCompensationPending) {
		s.logger.WarnContext(r.Context(), "member delete compensation pending", "error", err, "user_id", id, "correlation_id", correlationID(r.Context()))
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "compensation_pending"})
		return
	}
	if s.handleMembershipError(w, r, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listAlbums(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r, "cursor", "limit") {
		return
	}
	limit, err := library.ParseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	result, err := s.library.Albums(r.Context(), principalFrom(r.Context()).User.ID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) getAlbum(w http.ResponseWriter, r *http.Request) {
	result, err := s.library.Album(r.Context(), principalFrom(r.Context()).User.ID, r.PathValue("albumId"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listArtists(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r, "cursor", "limit", "query") {
		return
	}
	limit, err := library.ParseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	result, err := s.library.Artists(r.Context(), principalFrom(r.Context()).User.ID, r.URL.Query().Get("cursor"), limit, r.URL.Query().Get("query"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) getArtist(w http.ResponseWriter, r *http.Request) {
	result, err := s.library.Artist(r.Context(), principalFrom(r.Context()).User.ID, r.PathValue("artistId"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listSongs(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r, "cursor", "limit", "albumId") {
		return
	}
	limit, err := library.ParseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	result, err := s.library.Songs(r.Context(), principalFrom(r.Context()).User.ID, r.URL.Query().Get("cursor"), limit, r.URL.Query().Get("albumId"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) getSong(w http.ResponseWriter, r *http.Request) {
	result, err := s.library.Song(r.Context(), principalFrom(r.Context()).User.ID, r.PathValue("songId"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) searchLibrary(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r, "q", "limit", "albumCount", "artistCount", "songCount") {
		return
	}
	limit, err := library.ParseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	albumCount, err := optionalCount(r.URL.Query(), "albumCount")
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	artistCount, err := optionalCount(r.URL.Query(), "artistCount")
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	songCount, err := optionalCount(r.URL.Query(), "songCount")
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	result, err := s.library.Search(r.Context(), principalFrom(r.Context()).User.ID, r.URL.Query().Get("q"), limit, albumCount, artistCount, songCount)
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listStars(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r, "kind") {
		return
	}
	result, err := s.library.Starred(r.Context(), principalFrom(r.Context()).User.ID, r.URL.Query().Get("kind"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) star(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MediaID string `json:"mediaId"`
		Kind    string `json:"kind"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body is invalid.")
		return
	}
	if err := s.library.SetStar(r.Context(), principalFrom(r.Context()).User.ID, body.Kind, body.MediaID, true); err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unstar(w http.ResponseWriter, r *http.Request) {
	if err := s.library.SetStar(r.Context(), principalFrom(r.Context()).User.ID, r.PathValue("kind"), r.PathValue("mediaId"), false); err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) scanStatus(w http.ResponseWriter, r *http.Request) {
	result, err := s.library.ScanStatus(r.Context(), principalFrom(r.Context()).User.ID)
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listPlaylists(w http.ResponseWriter, r *http.Request) {
	items, err := s.playlists.List(r.Context(), principalFrom(r.Context()).User.ID)
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) getPlaylist(w http.ResponseWriter, r *http.Request) {
	item, err := s.playlists.Get(r.Context(), principalFrom(r.Context()).User.ID, r.PathValue("id"))
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string   `json:"name"`
		Comment string   `json:"comment"`
		SongIDs []string `json:"songIds"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body is invalid.")
		return
	}
	item, err := s.playlists.Create(r.Context(), principalFrom(r.Context()).User.ID, body.Name, body.Comment, body.SongIDs)
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) updatePlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          *string   `json:"name"`
		Comment       *string   `json:"comment"`
		SongIDs       *[]string `json:"songIds"`
		RemoveIndices []int     `json:"removeIndices"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request body is invalid.")
		return
	}
	item, err := s.playlists.Update(r.Context(), principalFrom(r.Context()).User.ID, r.PathValue("id"), playlist.Update{
		Name: body.Name, Comment: body.Comment, SongIDs: body.SongIDs, RemoveIndices: body.RemoveIndices,
	})
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	if err := s.playlists.Delete(r.Context(), principalFrom(r.Context()).User.ID, r.PathValue("id")); err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) audio(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	opened, err := s.streaming.OpenAudio(r.Context(), cookieToken(r), streaming.AudioRequest{
		Method: r.Method, MediaID: r.PathValue("mediaId"), Format: r.URL.Query().Get("format"),
		MaxBitRate: query.Get("maxBitRate"), OffsetSeconds: query.Get("offsetSeconds"), Range: r.Header.Get("Range"),
		InvalidQuery: !validAudioQuery(query),
	})
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	defer opened.Close()
	copyBinaryHeaders(w.Header(), opened.Response.Header)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(opened.Response.StatusCode)
	if r.Method == http.MethodHead || opened.Response.StatusCode == http.StatusNotModified {
		return
	}
	if _, err := io.Copy(w, opened.Response.Body); err != nil && r.Context().Err() == nil {
		s.logger.WarnContext(r.Context(), "audio stream ended before completion", "correlation_id", correlationID(r.Context()))
	}
}

func (s *Server) artwork(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "The artwork request is invalid.")
		return
	}
	response, err := s.streaming.OpenArtwork(r.Context(), principalFrom(r.Context()).User.ID, r.PathValue("id"), navidrome.ArtworkOptions{
		Method: r.Method, IfNoneMatch: r.Header.Get("If-None-Match"), IfModifiedSince: r.Header.Get("If-Modified-Since"),
	})
	if err != nil {
		s.handleM2Error(w, r, err)
		return
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	copyBinaryHeaders(w.Header(), response.Header)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(response.StatusCode)
	if r.Method == http.MethodHead || response.StatusCode == http.StatusNotModified {
		return
	}
	if response.Body != nil {
		if _, err := io.Copy(w, response.Body); err != nil && r.Context().Err() == nil {
			s.logger.WarnContext(r.Context(), "artwork response ended before completion", "user_id", principalFrom(r.Context()).User.ID, "correlation_id", correlationID(r.Context()))
		}
	}
}

func (s *Server) adminUserAction(w http.ResponseWriter, r *http.Request, action func(context.Context, int64, int64, string) error) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	principal := principalFrom(r.Context())
	err := action(r.Context(), id, principal.User.ID, correlationID(r.Context()))
	if s.handleMembershipError(w, r, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := cookieToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "A valid session is required.")
			return
		}
		principal, err := s.auth.Authenticate(r.Context(), token)
		switch {
		case errors.Is(err, auth.ErrUnauthenticated):
			writeError(w, http.StatusUnauthorized, "unauthenticated", "A valid session is required.")
			return
		case errors.Is(err, auth.ErrAccountNotActive):
			writeError(w, http.StatusForbidden, "account_not_active", "This account is not active.")
			return
		case err != nil:
			s.logger.ErrorContext(r.Context(), "session authentication failed", "error", err, "correlation_id", correlationID(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal_error", "The session could not be verified.")
			return
		}
		if principal.User.MustChangePassword && !restrictedPathAllowed(r.URL.Path) {
			writeError(w, http.StatusForbidden, "password_change_required", "Change the temporary password before continuing.")
			return
		}
		if r.URL.Path != "/api/auth/logout" {
			s.setSessionCookie(w, token, principal.ExpiresAt)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, principal)))
	})
}

func (s *Server) admin(next http.Handler) http.Handler {
	return s.authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principalFrom(r.Context()).User.Role != "admin" {
			writeError(w, http.StatusForbidden, "forbidden", "Administrator access is required.")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) originChecked(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/")
		if origin == "" {
			if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
				if parsed, err := url.Parse(referer); err == nil {
					origin = parsed.Scheme + "://" + parsed.Host
				}
			}
		}
		if origin != s.publicOrigin {
			writeError(w, http.StatusForbidden, "forbidden", "The request origin is not allowed.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) correlated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Correlation-Id"))
		if len(id) < 1 || len(id) > 128 || strings.ContainsAny(id, "\r\n") {
			id = newCorrelationID()
		}
		w.Header().Set("X-Correlation-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), correlationKey, id)))
	})
}

func (s *Server) handleMembershipError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, membership.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_request", "The supplied values are invalid.")
	case errors.Is(err, membership.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
	case errors.Is(err, membership.ErrConflict), errors.Is(err, membership.ErrProvisioningState), errors.Is(err, membership.ErrProvisioningBusy):
		writeError(w, http.StatusConflict, "conflict", "The operation conflicts with the current state.")
	case errors.Is(err, membership.ErrNavidromeUnavailable):
		writeError(w, http.StatusServiceUnavailable, "navidrome_unavailable", "Navidrome integration is unavailable.")
	default:
		s.logger.ErrorContext(r.Context(), "membership operation failed", "error", err, "correlation_id", correlationID(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal_error", "The operation could not be completed.")
	}
	return true
}

func (s *Server) handleM2Error(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return
	}
	var upstreamHTTP *navidrome.HTTPError
	var rateLimit *navidrome.RateLimitError
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "A valid session is required.")
	case errors.Is(err, auth.ErrAccountNotActive):
		writeError(w, http.StatusForbidden, "account_not_active", "This account is not active.")
	case errors.Is(err, streaming.ErrPasswordChangeRequired):
		writeError(w, http.StatusForbidden, "password_change_required", "Change the temporary password before continuing.")
	case errors.Is(err, library.ErrInvalidInput), errors.Is(err, playlist.ErrInvalidInput), errors.Is(err, streaming.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", "The supplied values are invalid.")
	case errors.Is(err, streaming.ErrStreamLimit):
		writeError(w, http.StatusTooManyRequests, "rate_limited", "The concurrent stream limit has been reached.")
	case errors.Is(err, navidrome.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "The requested media resource was not found.")
	case errors.Is(err, navidrome.ErrAuthorization):
		writeError(w, http.StatusForbidden, "forbidden", "The media resource is not available to this account.")
	case errors.As(err, &rateLimit):
		if rateLimit.RetryAfter != "" {
			w.Header().Set("Retry-After", rateLimit.RetryAfter)
		}
		writeError(w, http.StatusTooManyRequests, "rate_limited", "The media service is temporarily rate limited.")
	case errors.Is(err, navidrome.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "rate_limited", "The media service is temporarily rate limited.")
	case errors.Is(err, navidrome.ErrUnavailable), errors.Is(err, navidrome.ErrAuthentication),
		errors.Is(err, navidrome.ErrCredentialUnavailable), errors.Is(err, navidrome.ErrCredentialInvalidated),
		errors.Is(err, membership.ErrNavidromeUnavailable), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "navidrome_unavailable", "The media service is temporarily unavailable.")
	case errors.Is(err, navidrome.ErrMalformed):
		writeError(w, http.StatusBadGateway, "bad_gateway", "The media service returned an invalid response.")
	case errors.As(err, &upstreamHTTP):
		switch {
		case upstreamHTTP.StatusCode == http.StatusNotFound:
			writeError(w, http.StatusNotFound, "not_found", "The requested media resource was not found.")
		case upstreamHTTP.StatusCode == http.StatusUnauthorized || upstreamHTTP.StatusCode == http.StatusForbidden:
			writeError(w, http.StatusServiceUnavailable, "navidrome_unavailable", "The media service is temporarily unavailable.")
		case upstreamHTTP.StatusCode == http.StatusTooManyRequests:
			writeError(w, http.StatusTooManyRequests, "rate_limited", "The media service is temporarily rate limited.")
		case upstreamHTTP.StatusCode >= 500:
			writeError(w, http.StatusServiceUnavailable, "navidrome_unavailable", "The media service is temporarily unavailable.")
		default:
			writeError(w, http.StatusBadGateway, "bad_gateway", "The media service could not complete the request.")
		}
	default:
		s.logger.ErrorContext(r.Context(), "media operation failed", "error", err, "correlation_id", correlationID(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal_error", "The media operation could not be completed.")
	}
}

func optionalCount(values url.Values, key string) (int, error) {
	raw, present := values[key]
	if !present {
		return -1, nil
	}
	if len(raw) != 1 {
		return 0, library.ErrInvalidInput
	}
	value, err := optionalUnsigned(raw[0])
	if err != nil {
		return 0, library.ErrInvalidInput
	}
	return value, nil
}

func optionalUnsigned(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || strconv.Itoa(value) != raw {
		return 0, streaming.ErrInvalidRequest
	}
	return value, nil
}

func onlyQueryKeys(values url.Values, allowed ...string) bool {
	accepted := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		accepted[key] = struct{}{}
	}
	for key, items := range values {
		if _, ok := accepted[key]; !ok || len(items) != 1 {
			return false
		}
	}
	return true
}

func validAudioQuery(values url.Values) bool {
	if !onlyQueryKeys(values, "format", "maxBitRate", "offsetSeconds") {
		return false
	}
	for _, items := range values {
		if items[0] == "" {
			return false
		}
	}
	return true
}

func validQuery(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	if onlyQueryKeys(r.URL.Query(), allowed...) {
		return true
	}
	writeError(w, http.StatusBadRequest, "invalid_request", "The query parameters are invalid.")
	return false
}

func copyBinaryHeaders(destination, source http.Header) {
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if value := source.Get(name); value != "" {
			destination.Set(name, value)
		}
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: int(s.sessionTTL.Seconds())})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteLaxMode, Expires: time.Unix(1, 0), MaxAge: -1})
}

func decodeBody(w http.ResponseWriter, r *http.Request, destination any) error {
	reader := http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeOptionalBody(w http.ResponseWriter, r *http.Request, destination any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return decodeBody(w, r, destination)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "The resource id is invalid.")
		return 0, false
	}
	return id, true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	parsed := net.ParseIP(host)
	if parsed != nil && (parsed.IsLoopback() || parsed.IsPrivate()) {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); net.ParseIP(forwarded) != nil {
			return forwarded
		}
	}
	return host
}

func cookieToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func restrictedPathAllowed(path string) bool {
	return path == "/api/auth/me" || path == "/api/auth/change-password" || path == "/api/auth/logout"
}

func principalFrom(ctx context.Context) auth.Principal {
	principal, _ := ctx.Value(principalKey).(auth.Principal)
	return principal
}

func correlationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey).(string)
	return id
}

func newCorrelationID() string {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("flow-%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(random)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
