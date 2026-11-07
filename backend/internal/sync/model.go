// Package sync implements the persisted, single-writer synchronized playback
// state machine. Audio transport and WebSocket delivery are separate concerns.
package sync

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
)

const (
	ModeIndividual   = "individual"
	ModeSynchronized = "synchronized"

	StateActive = "active"
	StatePaused = "paused"
	StateEnded  = "ended"

	MaxQueueItems = 200
	MaxPositionMS = int64(86_400_000)
	GracePeriod   = 90 * time.Second
)

var (
	ErrInvalidInput      = errors.New("invalid synchronized playback input")
	ErrForbidden         = errors.New("synchronized playback requires administrator authority")
	ErrNoActiveSession   = errors.New("no active synchronized session")
	ErrModeConflict      = errors.New("platform mode conflicts with command")
	ErrVersionConflict   = errors.New("synchronized content version conflict")
	ErrStateConflict     = errors.New("synchronized state changed")
	ErrIllegalTransition = errors.New("illegal synchronized state transition")
	ErrStaleLease        = errors.New("stale DJ lease")
	ErrLeaseHeld         = errors.New("DJ lease is held by another connection")
	ErrSessionEnded      = errors.New("synchronized session ended")
)

type Session struct {
	ID                int64      `json:"id"`
	Version           int64      `json:"version"`
	EventSeq          int64      `json:"eventSeq"`
	State             string     `json:"state"`
	DJUserID          int64      `json:"djUserId"`
	DJLeaseGeneration int64      `json:"djLeaseGeneration"`
	DJLeaseTokenHash  []byte     `json:"-"`
	DJConnectionID    *string    `json:"-"`
	DJConnected       bool       `json:"djConnected"`
	DJDisconnectedAt  *time.Time `json:"djDisconnectedAt,omitempty"`
	DJGraceDeadline   *time.Time `json:"djGraceDeadline,omitempty"`
	CurrentMediaID    string     `json:"currentMediaId"`
	PositionMS        int64      `json:"positionMs"`
	RefTS             time.Time  `json:"refTs"`
	Queue             []string   `json:"queue"`
	QueueIndex        int        `json:"queueIndex"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	EndedAt           *time.Time `json:"endedAt,omitempty"`
	EndReason         *string    `json:"endReason,omitempty"`
}

type Snapshot struct {
	Mode                  string   `json:"mode"`
	ActiveSharedSessionID *int64   `json:"activeSharedSessionId,omitempty"`
	Session               *Session `json:"session,omitempty"`
}

type Event struct {
	ID              int64           `json:"id"`
	SharedSessionID int64           `json:"sharedSessionId"`
	EventSeq        int64           `json:"eventSeq"`
	Version         int64           `json:"version"`
	Type            string          `json:"type"`
	Payload         json.RawMessage `json:"payload"`
	ServerTS        time.Time       `json:"serverTs"`
}

type Result struct {
	Snapshot Snapshot `json:"snapshot"`
	Event    Event    `json:"event"`
}

type Seed struct {
	MediaID    *string `json:"mediaId,omitempty"`
	PositionMS *int64  `json:"positionMs,omitempty"`
}

type LeaseCredentials struct {
	Generation   int64
	ConnectionID string
	Token        string
}

type Lease struct {
	Session    Session `json:"session"`
	Generation int64   `json:"generation"`
	Token      string  `json:"leaseToken"`
}

type QueueMove struct {
	From *int `json:"from"`
	To   *int `json:"to"`
}

type QueueEdit struct {
	Add    *[]string  `json:"add,omitempty"`
	Remove *[]int     `json:"remove,omitempty"`
	Move   *QueueMove `json:"move,omitempty"`
}

type Mutation struct {
	Action     string
	IfVersion  *int64
	PositionMS *int64
	QueueEdit  *QueueEdit
	Lease      LeaseCredentials
}

type Expected struct {
	SessionID int64
	Version   int64
	EventSeq  int64
}

type EventDraft struct {
	Type    string
	Payload any
}

func validPrincipal(principal auth.Principal) error {
	if principal.User.ID < 1 || len(principal.TokenHash) != 32 {
		return auth.ErrUnauthenticated
	}
	if principal.User.Status != "active" || principal.User.ProvisioningState != "complete" {
		return auth.ErrAccountNotActive
	}
	if principal.User.MustChangePassword {
		return ErrForbidden
	}
	return nil
}

func requireAdmin(principal auth.Principal) error {
	if err := validPrincipal(principal); err != nil {
		return err
	}
	if principal.User.Role != "admin" {
		return ErrForbidden
	}
	return nil
}

func CurrentPosition(session Session, now time.Time) int64 {
	position := session.PositionMS
	if session.State == StateActive && now.After(session.RefTS) {
		position += now.Sub(session.RefTS).Milliseconds()
	}
	return min(MaxPositionMS, max(int64(0), position))
}

func validatePosition(position, duration int64) error {
	if position < 0 || position > MaxPositionMS {
		return ErrInvalidInput
	}
	if duration > 0 && position > duration+1_000 {
		return ErrInvalidInput
	}
	return nil
}

func normalizePosition(position, duration int64) int64 {
	if duration > 0 {
		return min(position, duration)
	}
	return position
}

func validQueue(queue []string) bool {
	if len(queue) == 0 || len(queue) > MaxQueueItems {
		return false
	}
	for _, id := range queue {
		if strings.TrimSpace(id) == "" {
			return false
		}
	}
	return true
}

func validConnectionID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func validLeaseToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func leaseMatches(session Session, principal auth.Principal, lease LeaseCredentials, tokenHash []byte) bool {
	return session.DJConnected && leaseIdentityMatches(session, principal, lease, tokenHash)
}

func leaseIdentityMatches(session Session, principal auth.Principal, lease LeaseCredentials, tokenHash []byte) bool {
	return session.DJUserID == principal.User.ID &&
		session.DJLeaseGeneration == lease.Generation && session.DJConnectionID != nil &&
		*session.DJConnectionID == lease.ConnectionID && len(session.DJLeaseTokenHash) == 32 &&
		len(tokenHash) == 32 && subtle.ConstantTimeCompare(session.DJLeaseTokenHash, tokenHash) == 1
}

type DriftReport struct {
	PositionMS int64
	BufferMS   int64
	Seeking    bool
	Rate       float64
}

type DriftDecision struct {
	DriftMS          int64 `json:"driftMs"`
	TargetPositionMS int64 `json:"targetPositionMs,omitempty"`
	Correct          bool  `json:"correct"`
	Deferred         bool  `json:"deferred"`
}

func DecideDrift(session Session, report DriftReport, now time.Time, threshold int64) DriftDecision {
	target := CurrentPosition(session, now)
	drift := report.PositionMS - target
	decision := DriftDecision{DriftMS: drift, TargetPositionMS: target}
	if session.State != StateActive || threshold <= 0 || math.Abs(float64(drift)) <= float64(threshold) {
		return decision
	}
	if report.Seeking {
		decision.Deferred = true
		return decision
	}
	decision.Correct = true
	return decision
}
