// Package playback persists individual transport intent. The browser, not this
// package, owns audio transport. All state is scoped to a Flow principal.
package playback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
)

const (
	MaxQueueItems   = 200
	MaxPositionMS   = int64(86_400_000)
	MaxRevision     = int64(9_007_199_254_740_991) // exact JSON integers in browsers
	PreviousRestart = int64(3_000)
)

var (
	ErrInvalidInput           = errors.New("invalid playback input")
	ErrPasswordChangeRequired = errors.New("password change required")
	ErrSynchronizedMode       = errors.New("individual playback is unavailable in synchronized mode")
)

type QueueItem struct {
	MediaID string    `json:"mediaId"`
	AddedAt time.Time `json:"addedAt"`
}

type Session struct {
	Queue          []QueueItem `json:"queue"`
	QueueIndex     int         `json:"queueIndex"`
	CurrentMediaID *string     `json:"currentMediaId"`
	PositionMS     int64       `json:"positionMs"`
	Volume         float64     `json:"volume"`
	Shuffle        bool        `json:"shuffle"`
	Status         string      `json:"status"`
	Revision       int64       `json:"revision"`
	UpdatedAt      time.Time   `json:"updatedAt"`
}

func DefaultSession(now time.Time) Session {
	return Session{Queue: []QueueItem{}, Volume: 1, Status: "paused", UpdatedAt: now.UTC()}
}

type ConflictError struct {
	Current Session
}

func (*ConflictError) Error() string { return "playback revision or checkpoint identity conflict" }

type Move struct {
	From *int `json:"from"`
	To   *int `json:"to"`
}

type QueueEdit struct {
	Add    *[]string `json:"add,omitempty"`
	Remove *[]int    `json:"remove,omitempty"`
	Move   *Move     `json:"move,omitempty"`
}

type Mutation struct {
	Queue             *[]QueueItem `json:"queue,omitempty"`
	QueueIndex        *int         `json:"queueIndex,omitempty"`
	CurrentMediaID    *string      `json:"currentMediaId,omitempty"`
	PositionMS        *int64       `json:"positionMs,omitempty"`
	Volume            *float64     `json:"volume,omitempty"`
	Shuffle           *bool        `json:"shuffle,omitempty"`
	Status            *string      `json:"status,omitempty"`
	QueueEdit         *QueueEdit   `json:"queueEdit,omitempty"`
	Command           *string      `json:"command,omitempty"`
	IfRevision        *int64       `json:"ifRevision"`
	HasCurrentMediaID bool         `json:"-"` // distinguish explicit null from omission
}

// UnmarshalJSON preserves nullable currentMediaId but rejects null for the
// other fields, unknown fields, and non-object bodies. Revision 0 must never be
// inferred from a missing field. decodeBody also bounds size/trailing values.
func (m *Mutation) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return ErrInvalidInput
	}
	for name, value := range fields {
		if name != "currentMediaId" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrInvalidInput
		}
	}
	type plain Mutation
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return ErrInvalidInput
	}
	*m = Mutation(value)
	_, m.HasCurrentMediaID = fields["currentMediaId"]
	return nil
}

type Store interface {
	Load(context.Context, auth.Principal) (Session, error)
	CompareAndSwap(context.Context, auth.Principal, int64, Session) (Session, error)
}

type modeAwareStore interface {
	CompareAndSwapInMode(context.Context, auth.Principal, int64, Session, string) (Session, error)
}

type Library interface {
	Song(context.Context, int64, string) (library.Song, error)
}

type Service struct {
	store   Store
	library Library
	mode    interface {
		Mode(context.Context) (string, error)
	}
	now func() time.Time
}

func New(store Store, media Library) *Service {
	return &Service{store: store, library: media, now: time.Now}
}

func (s *Service) SetModeGate(mode interface {
	Mode(context.Context) (string, error)
}) {
	s.mode = mode
}

func validPrincipal(principal auth.Principal) error {
	if principal.User.ID < 1 || len(principal.TokenHash) != sha256.Size {
		return auth.ErrUnauthenticated
	}
	if principal.User.Status != "active" || principal.User.ProvisioningState != "complete" {
		return auth.ErrAccountNotActive
	}
	if principal.User.MustChangePassword {
		return ErrPasswordChangeRequired
	}
	return nil
}

func (s *Service) Get(ctx context.Context, principal auth.Principal) (Session, error) {
	if err := validPrincipal(principal); err != nil {
		return Session{}, err
	}
	return s.store.Load(ctx, principal)
}

func (s *Service) Clear(ctx context.Context, principal auth.Principal, revision *int64) (Session, error) {
	empty := []QueueItem{}
	return s.Update(ctx, principal, Mutation{IfRevision: revision, Queue: &empty})
}

func (s *Service) Update(ctx context.Context, principal auth.Principal, mutation Mutation) (Session, error) {
	if err := validPrincipal(principal); err != nil {
		return Session{}, err
	}
	if s.mode != nil {
		mode, err := s.mode.Mode(ctx)
		if err != nil {
			return Session{}, err
		}
		if mode != "individual" {
			return Session{}, ErrSynchronizedMode
		}
	}
	if err := validateMutation(mutation); err != nil {
		return Session{}, err
	}
	current, err := s.store.Load(ctx, principal)
	if err != nil {
		return Session{}, err
	}
	if current.Revision != *mutation.IfRevision {
		return Session{}, &ConflictError{Current: current}
	}
	next, err := apply(current, mutation, s.now().UTC())
	if err != nil {
		return Session{}, err
	}
	// Lookups are request-scoped and deduplicated, not a persistent metadata copy.
	checked := make(map[string]library.Song)
	lookup := func(id string) (library.Song, error) {
		if song, exists := checked[id]; exists {
			return song, nil
		}
		song, err := s.library.Song(ctx, principal.User.ID, id)
		if err != nil {
			return library.Song{}, err
		}
		if song.ID != id {
			return library.Song{}, navidrome.ErrMalformed
		}
		checked[id] = song
		return song, nil
	}
	if mutation.Queue != nil {
		for _, item := range next.Queue {
			if _, err := lookup(item.MediaID); err != nil {
				return Session{}, err
			}
		}
	}
	if mutation.QueueEdit != nil && mutation.QueueEdit.Add != nil {
		for _, id := range *mutation.QueueEdit.Add {
			if _, err := lookup(id); err != nil {
				return Session{}, err
			}
		}
	}
	selected := !sameCurrent(current, next)
	needsPositionCheck := mutation.PositionMS != nil
	playing := mutation.Status != nil && *mutation.Status == "playing"
	explicitSelection := mutation.Command == nil && (mutation.QueueIndex != nil || hasMediaID(mutation))
	retainedRemoval := mutation.QueueEdit != nil && mutation.QueueEdit.Remove != nil
	pausingRetained := mutation.Status != nil && *mutation.Status == "paused" &&
		!selected && !explicitSelection && mutation.Queue == nil && mutation.QueueEdit == nil && mutation.Command == nil
	if next.CurrentMediaID != nil && (selected && !retainedRemoval || explicitSelection || needsPositionCheck || playing) {
		song, err := lookup(*next.CurrentMediaID)
		if err != nil {
			if !pausingRetained || !(errors.Is(err, navidrome.ErrNotFound) ||
				errors.Is(err, navidrome.ErrAuthorization) || errors.Is(err, navidrome.ErrUnavailable)) {
				return Session{}, err
			}
		}
		if err == nil && song.DurationMS > 0 {
			if next.PositionMS > song.DurationMS+1_000 {
				return Session{}, ErrInvalidInput
			}
			next.PositionMS = min(next.PositionMS, song.DurationMS)
		}
	}
	// The earlier read is used only to construct/validate the proposed state.
	// This conditional database write is the authority; a competing commit wins
	// even when it occurs during a slow member-scoped Navidrome lookup.
	if s.mode != nil {
		if store, ok := s.store.(modeAwareStore); ok {
			return store.CompareAndSwapInMode(ctx, principal, *mutation.IfRevision, next, "individual")
		}
	}
	return s.store.CompareAndSwap(ctx, principal, *mutation.IfRevision, next)
}

func validateMutation(m Mutation) error {
	if m.IfRevision == nil || *m.IfRevision < 0 || *m.IfRevision >= MaxRevision {
		return ErrInvalidInput
	}
	if m.Queue == nil && m.QueueIndex == nil && !hasMediaID(m) && m.PositionMS == nil &&
		m.Volume == nil && m.Shuffle == nil && m.Status == nil && m.QueueEdit == nil && m.Command == nil {
		return ErrInvalidInput
	}
	if m.QueueIndex != nil && *m.QueueIndex < 0 || m.PositionMS != nil && (*m.PositionMS < 0 || *m.PositionMS > MaxPositionMS) {
		return ErrInvalidInput
	}
	if m.Volume != nil && (math.IsNaN(*m.Volume) || math.IsInf(*m.Volume, 0) || *m.Volume < 0 || *m.Volume > 1) {
		return ErrInvalidInput
	}
	if m.Status != nil && *m.Status != "playing" && *m.Status != "paused" {
		return ErrInvalidInput
	}
	if m.CurrentMediaID != nil && !library.ValidResourceID(*m.CurrentMediaID) {
		return ErrInvalidInput
	}
	if m.QueueEdit != nil && (m.Queue != nil || m.QueueIndex != nil || hasMediaID(m) || m.Command != nil) {
		return ErrInvalidInput
	}
	if m.Command != nil {
		if m.Queue != nil || m.QueueEdit != nil || m.Volume != nil || m.Shuffle != nil || m.Status != nil {
			return ErrInvalidInput
		}
		switch *m.Command {
		case "next", "previous":
		case "ended", "checkpoint":
			if m.CurrentMediaID == nil || m.QueueIndex == nil || m.PositionMS == nil {
				return ErrInvalidInput
			}
		default:
			return ErrInvalidInput
		}
	}
	return nil
}

func hasMediaID(m Mutation) bool { return m.HasCurrentMediaID || m.CurrentMediaID != nil }

func apply(current Session, m Mutation, now time.Time) (Session, error) {
	next := current
	next.Queue = slices.Clone(current.Queue)
	if m.Volume != nil {
		next.Volume = math.Round(*m.Volume*1_000) / 1_000
	}
	if m.Shuffle != nil {
		next.Shuffle = *m.Shuffle
	}
	if m.Status != nil {
		next.Status = *m.Status
	}
	if m.Command != nil {
		if hasMediaID(m) && (m.CurrentMediaID == nil || current.CurrentMediaID == nil || *m.CurrentMediaID != *current.CurrentMediaID) ||
			m.QueueIndex != nil && *m.QueueIndex != current.QueueIndex {
			return Session{}, &ConflictError{Current: current}
		}
		if (*m.Command == "checkpoint" || *m.Command == "ended") && current.Status != "playing" {
			return Session{}, &ConflictError{Current: current}
		}
		if m.PositionMS != nil {
			next.PositionMS = *m.PositionMS
		}
		switch *m.Command {
		case "next", "ended":
			advance(&next, 1)
		case "previous":
			if next.PositionMS > PreviousRestart {
				next.PositionMS = 0
			} else {
				advance(&next, -1)
			}
		case "checkpoint":
		}
	} else {
		if m.Queue != nil {
			queue, err := replacement(current.Queue, *m.Queue, now)
			if err != nil {
				return Session{}, err
			}
			next.Queue = queue
			reconcileSelection(current, &next)
		}
		if m.QueueEdit != nil {
			if err := editQueue(current, &next, *m.QueueEdit, now); err != nil {
				return Session{}, err
			}
		}
		if m.QueueIndex != nil {
			next.QueueIndex = *m.QueueIndex
		}
		if hasMediaID(m) {
			if m.CurrentMediaID == nil {
				if len(next.Queue) != 0 {
					return Session{}, ErrInvalidInput
				}
			} else if m.QueueIndex == nil {
				next.QueueIndex = slices.IndexFunc(next.Queue, func(item QueueItem) bool { return item.MediaID == *m.CurrentMediaID })
			}
		}
		if len(next.Queue) == 0 {
			if next.QueueIndex != 0 || m.PositionMS != nil && *m.PositionMS != 0 || m.Status != nil && *m.Status == "playing" || m.CurrentMediaID != nil {
				return Session{}, ErrInvalidInput
			}
		} else {
			if next.QueueIndex < 0 || next.QueueIndex >= len(next.Queue) {
				return Session{}, ErrInvalidInput
			}
			id := next.Queue[next.QueueIndex].MediaID
			if m.CurrentMediaID != nil && id != *m.CurrentMediaID {
				return Session{}, ErrInvalidInput
			}
			next.CurrentMediaID = &id
		}
		if !sameCurrent(current, next) {
			next.PositionMS = 0
		}
		if m.PositionMS != nil {
			next.PositionMS = *m.PositionMS
		}
	}
	if len(next.Queue) == 0 {
		next.Queue = []QueueItem{}
		next.QueueIndex = 0
		next.CurrentMediaID = nil
		next.PositionMS = 0
		next.Status = "paused"
	}
	return next, nil
}

func replacement(old, requested []QueueItem, now time.Time) ([]QueueItem, error) {
	if len(requested) > MaxQueueItems {
		return nil, ErrInvalidInput
	}
	result := make([]QueueItem, len(requested))
	used := make([]bool, len(old))
	for index, item := range requested {
		if !library.ValidResourceID(item.MediaID) {
			return nil, ErrInvalidInput
		}
		stamp := freshTimestamp(old, now).Add(time.Duration(index) * time.Microsecond)
		for oldIndex, previous := range old {
			if !used[oldIndex] && item.MediaID == previous.MediaID && (item.AddedAt.IsZero() || item.AddedAt.Equal(previous.AddedAt)) {
				used[oldIndex] = true
				stamp = previous.AddedAt
				break
			}
		}
		result[index] = QueueItem{MediaID: item.MediaID, AddedAt: stamp.UTC()}
	}
	return result, nil
}

func editQueue(current Session, next *Session, edit QueueEdit, now time.Time) error {
	operations := 0
	if edit.Add != nil {
		operations++
	}
	if edit.Remove != nil {
		operations++
	}
	if edit.Move != nil {
		operations++
	}
	if operations != 1 {
		return ErrInvalidInput
	}
	switch {
	case edit.Add != nil:
		if len(*edit.Add) == 0 || len(next.Queue)+len(*edit.Add) > MaxQueueItems {
			return ErrInvalidInput
		}
		stamp := freshTimestamp(current.Queue, now)
		for index, id := range *edit.Add {
			if !library.ValidResourceID(id) {
				return ErrInvalidInput
			}
			next.Queue = append(next.Queue, QueueItem{MediaID: id, AddedAt: stamp.Add(time.Duration(index) * time.Microsecond)})
		}
	case edit.Remove != nil:
		if len(*edit.Remove) == 0 {
			return ErrInvalidInput
		}
		removed := make(map[int]bool)
		for _, index := range *edit.Remove {
			if index < 0 || index >= len(next.Queue) || removed[index] {
				return ErrInvalidInput
			}
			removed[index] = true
		}
		next.Queue = []QueueItem{}
		for index, item := range current.Queue {
			if !removed[index] {
				next.Queue = append(next.Queue, item)
			}
		}
	case edit.Move != nil:
		if edit.Move.From == nil || edit.Move.To == nil {
			return ErrInvalidInput
		}
		from, to := *edit.Move.From, *edit.Move.To
		if from < 0 || from >= len(next.Queue) || to < 0 || to >= len(next.Queue) {
			return ErrInvalidInput
		}
		item := next.Queue[from]
		next.Queue = slices.Delete(next.Queue, from, from+1)
		next.Queue = slices.Insert(next.Queue, to, item)
	}
	reconcileSelection(current, next)
	return nil
}

func reconcileSelection(current Session, next *Session) {
	next.QueueIndex = 0
	if len(next.Queue) == 0 {
		next.CurrentMediaID = nil
		return
	}
	if len(current.Queue) > 0 {
		// Retain the selected occurrence, then the first surviving successor,
		// then the nearest surviving predecessor. IDs alone cannot identify
		// duplicate occurrences.
		candidates := []int{current.QueueIndex}
		for index := current.QueueIndex + 1; index < len(current.Queue); index++ {
			candidates = append(candidates, index)
		}
		for index := current.QueueIndex - 1; index >= 0; index-- {
			candidates = append(candidates, index)
		}
		for _, oldIndex := range candidates {
			index := slices.IndexFunc(next.Queue, func(item QueueItem) bool { return sameItem(item, current.Queue[oldIndex]) })
			if index >= 0 {
				next.QueueIndex = index
				break
			}
		}
	}
	id := next.Queue[next.QueueIndex].MediaID
	next.CurrentMediaID = &id
	if !sameCurrent(current, *next) {
		next.PositionMS = 0
	}
}

func sameItem(a, b QueueItem) bool { return a.MediaID == b.MediaID && a.AddedAt.Equal(b.AddedAt) }

func sameCurrent(a, b Session) bool {
	if len(a.Queue) == 0 || len(b.Queue) == 0 {
		return len(a.Queue) == 0 && len(b.Queue) == 0
	}
	return sameItem(a.Queue[a.QueueIndex], b.Queue[b.QueueIndex])
}

// PlaybackOrder is derived, never persisted over the canonical queue. Stable
// hashes provide a reproducible V1 shuffle with no history, wrap, or repeat mode.
func PlaybackOrder(session Session) []int {
	order := make([]int, len(session.Queue))
	hashes := make([][32]byte, len(session.Queue))
	for index, item := range session.Queue {
		order[index] = index
		hashes[index] = sha256.Sum256([]byte(item.MediaID + "\x00" + item.AddedAt.UTC().Format(time.RFC3339Nano)))
	}
	if session.Shuffle {
		sort.SliceStable(order, func(a, b int) bool {
			return bytes.Compare(hashes[order[a]][:], hashes[order[b]][:]) < 0
		})
	}
	return order
}

func advance(session *Session, direction int) {
	if len(session.Queue) == 0 {
		return
	}
	order := PlaybackOrder(*session)
	index := slices.Index(order, session.QueueIndex)
	if destination := index + direction; destination >= 0 && destination < len(order) {
		session.QueueIndex = order[destination]
		session.PositionMS = 0
		id := session.Queue[session.QueueIndex].MediaID
		session.CurrentMediaID = &id
	} else if direction > 0 {
		session.Status = "paused"
	} else {
		session.PositionMS = 0
	}
}

func freshTimestamp(old []QueueItem, now time.Time) time.Time {
	for _, item := range old {
		if !now.After(item.AddedAt) {
			now = item.AddedAt.Add(time.Microsecond)
		}
	}
	return now.UTC()
}
