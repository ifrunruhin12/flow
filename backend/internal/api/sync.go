package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	flowsync "github.com/ifrunruhin12/flow/backend/internal/sync"
)

func (s *Server) getPlatformMode(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r) {
		return
	}
	snapshot, err := s.synchronization.Snapshot(r.Context())
	if err != nil {
		s.handleSynchronizationError(w, r, err)
		return
	}
	response := map[string]any{"mode": snapshot.Mode}
	if snapshot.ActiveSharedSessionID != nil {
		response["activeSharedSessionId"] = *snapshot.ActiveSharedSessionID
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getSynchronizedState(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r) {
		return
	}
	snapshot, err := s.synchronization.Snapshot(r.Context())
	if err != nil {
		s.handleSynchronizationError(w, r, err)
		return
	}
	if snapshot.Mode != flowsync.ModeSynchronized || snapshot.Session == nil {
		writeError(w, http.StatusServiceUnavailable, "sync_unavailable", "There is no active synchronized session.")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) setPlatformMode(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r) {
		return
	}
	var body struct {
		Mode      string         `json:"mode"`
		Seed      *flowsync.Seed `json:"seed,omitempty"`
		IfVersion *int64         `json:"ifVersion,omitempty"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The platform mode request is invalid.")
		return
	}
	principal := principalFrom(r.Context())
	ctx := flowsync.WithCorrelationID(r.Context(), correlationID(r.Context()))
	var (
		result flowsync.Result
		err    error
	)
	switch body.Mode {
	case flowsync.ModeSynchronized:
		if body.IfVersion != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "A synchronized-mode start does not accept ifVersion.")
			return
		}
		seed := flowsync.Seed{}
		if body.Seed != nil {
			seed = *body.Seed
		}
		result, err = s.synchronization.Start(ctx, principal, seed)
	case flowsync.ModeIndividual:
		if body.Seed != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "An individual-mode transition does not accept a seed.")
			return
		}
		lease, leaseErr := synchronizationLease(r)
		if leaseErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Valid DJ lease headers are required.")
			return
		}
		result, err = s.synchronization.End(ctx, principal, body.IfVersion, lease)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "The platform mode is invalid.")
		return
	}
	if err != nil {
		s.handleSynchronizationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Snapshot)
}

func (s *Server) mutateSynchronized(w http.ResponseWriter, r *http.Request) {
	if !validQuery(w, r) {
		return
	}
	action := r.PathValue("action")
	if action == "" {
		action = strings.TrimPrefix(r.URL.Path, "/api/sync/session/")
	}
	var body struct {
		IfVersion  *int64              `json:"ifVersion"`
		PositionMS *int64              `json:"positionMs,omitempty"`
		Add        *[]string           `json:"add,omitempty"`
		Remove     *[]int              `json:"remove,omitempty"`
		Move       *flowsync.QueueMove `json:"move,omitempty"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The synchronized playback request is invalid.")
		return
	}
	lease, err := synchronizationLease(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Valid DJ lease headers are required.")
		return
	}
	mutation := flowsync.Mutation{Action: action, IfVersion: body.IfVersion, PositionMS: body.PositionMS, Lease: lease}
	if action == "queue" {
		mutation.QueueEdit = &flowsync.QueueEdit{Add: body.Add, Remove: body.Remove, Move: body.Move}
	} else if body.Add != nil || body.Remove != nil || body.Move != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Queue fields are valid only for the queue command.")
		return
	}
	if action != "seek" && body.PositionMS != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Position is valid only for the seek command.")
		return
	}
	result, err := s.synchronization.Mutate(r.Context(), principalFrom(r.Context()), mutation)
	if err != nil {
		s.handleSynchronizationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Snapshot)
}

func synchronizationLease(r *http.Request) (flowsync.LeaseCredentials, error) {
	generationRaw := strings.TrimSpace(r.Header.Get("X-Flow-DJ-Generation"))
	generation, err := strconv.ParseInt(generationRaw, 10, 64)
	if err != nil || generation < 1 || strconv.FormatInt(generation, 10) != generationRaw {
		return flowsync.LeaseCredentials{}, flowsync.ErrInvalidInput
	}
	connection := strings.TrimSpace(r.Header.Get("X-Flow-DJ-Connection"))
	token := strings.TrimSpace(r.Header.Get("X-Flow-DJ-Lease"))
	if connection == "" || token == "" {
		return flowsync.LeaseCredentials{}, flowsync.ErrInvalidInput
	}
	return flowsync.LeaseCredentials{Generation: generation, ConnectionID: connection, Token: token}, nil
}

func (s *Server) handleSynchronizationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "A valid session is required.")
	case errors.Is(err, auth.ErrAccountNotActive):
		writeError(w, http.StatusForbidden, "account_not_active", "This account is not active.")
	case errors.Is(err, flowsync.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "Administrator DJ authority is required.")
	case errors.Is(err, flowsync.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_request", "The synchronized playback values are invalid.")
	case errors.Is(err, flowsync.ErrStaleLease):
		s.writeSynchronizationConflict(w, r, "stale_lease", "The DJ lease belongs to a newer connection.")
	case errors.Is(err, flowsync.ErrLeaseHeld):
		s.writeSynchronizationConflict(w, r, "lease_held", "Another DJ connection currently owns the lease.")
	case errors.Is(err, flowsync.ErrVersionConflict):
		s.writeSynchronizationConflict(w, r, "version_conflict", "The synchronized content version changed.")
	case errors.Is(err, flowsync.ErrModeConflict), errors.Is(err, flowsync.ErrStateConflict), errors.Is(err, flowsync.ErrIllegalTransition):
		s.writeSynchronizationConflict(w, r, "conflict", "The synchronized command conflicts with the current state.")
	case errors.Is(err, flowsync.ErrNoActiveSession):
		writeError(w, http.StatusServiceUnavailable, "sync_unavailable", "There is no active synchronized session.")
	case errors.Is(err, flowsync.ErrSessionEnded):
		writeError(w, http.StatusGone, "session_ended", "The synchronized session has ended.")
	default:
		s.handleM2Error(w, r, err)
	}
}

func (s *Server) writeSynchronizationConflict(w http.ResponseWriter, r *http.Request, code, message string) {
	response := map[string]any{"error": map[string]string{"code": code, "message": message}}
	if snapshot, err := s.synchronization.Snapshot(r.Context()); err == nil {
		response["currentState"] = snapshot
	}
	writeJSON(w, http.StatusConflict, response)
}
