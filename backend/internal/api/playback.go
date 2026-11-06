package api

import (
	"errors"
	"net/http"

	"github.com/ifrunruhin12/flow/backend/internal/playback"
)

func (s *Server) getPlayback(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r) {
		return
	}
	state, err := s.playback.Get(r.Context(), principalFrom(r.Context()))
	if err != nil {
		s.handlePlaybackError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) updatePlayback(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r) {
		return
	}
	var body playback.Mutation
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The playback request body is invalid.")
		return
	}
	state, err := s.playback.Update(r.Context(), principalFrom(r.Context()), body)
	if err != nil {
		s.handlePlaybackError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) clearPlayback(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r) {
		return
	}
	var body struct {
		IfRevision *int64 `json:"ifRevision"`
	}
	if err := decodeBody(w, r, &body); err != nil || body.IfRevision == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "A playback revision is required.")
		return
	}
	state, err := s.playback.Clear(r.Context(), principalFrom(r.Context()), body.IfRevision)
	if err != nil {
		s.handlePlaybackError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handlePlaybackError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *playback.ConflictError
	switch {
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":           map[string]string{"code": "conflict", "message": "Playback changed. Reload the current session."},
			"currentRevision": conflict.Current.Revision,
			"currentSession":  conflict.Current,
		})
	case errors.Is(err, playback.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_request", "The supplied playback values are invalid.")
	case errors.Is(err, playback.ErrPasswordChangeRequired):
		writeError(w, http.StatusForbidden, "password_change_required", "Change the temporary password before continuing.")
	default:
		s.handleM2Error(w, r, err)
	}
}
