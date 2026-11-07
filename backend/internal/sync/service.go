package sync

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	stdsync "sync"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/playback"
)

type Library interface {
	Song(context.Context, int64, string) (library.Song, error)
}

type IndividualPlayback interface {
	Get(context.Context, auth.Principal) (playback.Session, error)
}

type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type timer interface{ Stop() bool }

type Scheduler interface {
	AfterFunc(time.Duration, func()) timer
}

type systemScheduler struct{}

func (systemScheduler) AfterFunc(duration time.Duration, function func()) timer {
	return time.AfterFunc(duration, function)
}

type Option func(*Service)

func WithClock(clock Clock) Option       { return func(service *Service) { service.clock = clock } }
func WithRandom(reader io.Reader) Option { return func(service *Service) { service.random = reader } }
func WithScheduler(scheduler Scheduler) Option {
	return func(service *Service) { service.scheduler = scheduler }
}

type Service struct {
	store      Store
	library    Library
	individual IndividualPlayback
	clock      Clock
	random     io.Reader
	scheduler  Scheduler
	executor   *executor
	timerMu    stdsync.Mutex
	deadline   timer
	closed     bool
}

func New(store Store, media Library, individual IndividualPlayback, options ...Option) *Service {
	service := &Service{
		store: store, library: media, individual: individual, clock: systemClock{},
		random: rand.Reader, scheduler: systemScheduler{}, executor: newExecutor(),
	}
	for _, option := range options {
		option(service)
	}
	return service
}

func (s *Service) Close() {
	s.timerMu.Lock()
	s.closed = true
	if s.deadline != nil {
		s.deadline.Stop()
		s.deadline = nil
	}
	s.timerMu.Unlock()
	s.executor.close()
}

func (s *Service) Mode(ctx context.Context) (string, error) {
	snapshot, err := s.store.Load(ctx)
	return snapshot.Mode, err
}

func (s *Service) Snapshot(ctx context.Context) (Snapshot, error) {
	return s.store.Load(ctx)
}

func (s *Service) Recover(ctx context.Context) ([]Result, error) {
	value, err := s.executor.submit(ctx, func(ctx context.Context) (any, error) {
		return s.store.Recover(ctx, s.clock.Now().UTC())
	})
	if err != nil {
		return nil, err
	}
	s.cancelDeadline()
	return value.([]Result), nil
}

func (s *Service) Start(ctx context.Context, principal auth.Principal, seed Seed) (Result, error) {
	if err := requireAdmin(principal); err != nil {
		return Result{}, err
	}
	value, err := s.executor.submit(ctx, func(ctx context.Context) (any, error) {
		return s.start(ctx, principal, seed)
	})
	if err != nil {
		return Result{}, err
	}
	return value.(Result), nil
}

func (s *Service) start(ctx context.Context, principal auth.Principal, seed Seed) (Result, error) {
	snapshot, err := s.store.Load(ctx)
	if err != nil {
		return Result{}, err
	}
	if snapshot.Mode != ModeIndividual || snapshot.Session != nil {
		return Result{}, ErrModeConflict
	}
	queue, index, position, individualRevision, err := s.resolveSeed(ctx, principal, seed)
	if err != nil {
		return Result{}, err
	}
	now := s.clock.Now().UTC()
	proposed := Session{
		State: StateActive, DJUserID: principal.User.ID, CurrentMediaID: queue[index],
		PositionMS: position, RefTS: now, Queue: queue, QueueIndex: index,
	}
	return s.store.Start(ctx, principal, proposed, individualRevision)
}

func (s *Service) resolveSeed(ctx context.Context, principal auth.Principal, seed Seed) ([]string, int, int64, *int64, error) {
	if seed.PositionMS != nil && (*seed.PositionMS < 0 || *seed.PositionMS > MaxPositionMS) {
		return nil, 0, 0, nil, ErrInvalidInput
	}
	var queue []string
	index := 0
	position := int64(0)
	var individualRevision *int64
	if seed.MediaID != nil {
		if !library.ValidResourceID(*seed.MediaID) {
			return nil, 0, 0, nil, ErrInvalidInput
		}
		queue = []string{*seed.MediaID}
	} else {
		if s.individual == nil {
			return nil, 0, 0, nil, ErrInvalidInput
		}
		individual, err := s.individual.Get(ctx, principal)
		if err != nil {
			return nil, 0, 0, nil, err
		}
		if individual.CurrentMediaID == nil || len(individual.Queue) == 0 || individual.QueueIndex < 0 || individual.QueueIndex >= len(individual.Queue) {
			return nil, 0, 0, nil, ErrInvalidInput
		}
		individualRevision = &individual.Revision
		queue = make([]string, len(individual.Queue))
		for queueIndex, item := range individual.Queue {
			queue[queueIndex] = item.MediaID
		}
		index = individual.QueueIndex
		position = individual.PositionMS
	}
	if seed.PositionMS != nil {
		position = *seed.PositionMS
	}
	if !validQueue(queue) || index < 0 || index >= len(queue) {
		return nil, 0, 0, nil, ErrInvalidInput
	}
	songs := make(map[string]library.Song)
	for _, id := range queue {
		if !library.ValidResourceID(id) {
			return nil, 0, 0, nil, ErrInvalidInput
		}
		if _, known := songs[id]; known {
			continue
		}
		song, err := s.library.Song(ctx, principal.User.ID, id)
		if err != nil {
			return nil, 0, 0, nil, err
		}
		if song.ID != id {
			return nil, 0, 0, nil, ErrInvalidInput
		}
		songs[id] = song
	}
	duration := songs[queue[index]].DurationMS
	if err := validatePosition(position, duration); err != nil {
		return nil, 0, 0, nil, err
	}
	return queue, index, normalizePosition(position, duration), individualRevision, nil
}

func (s *Service) AcquireLease(ctx context.Context, principal auth.Principal, connectionID string, takeover bool) (Lease, error) {
	if err := requireAdmin(principal); err != nil {
		return Lease{}, err
	}
	if !validConnectionID(connectionID) {
		return Lease{}, ErrInvalidInput
	}
	value, err := s.executor.submit(ctx, func(ctx context.Context) (any, error) {
		snapshot, err := s.store.Load(ctx)
		if err != nil {
			return nil, err
		}
		if snapshot.Mode != ModeSynchronized || snapshot.Session == nil {
			return nil, ErrNoActiveSession
		}
		current := *snapshot.Session
		if current.State == StateEnded {
			return nil, ErrSessionEnded
		}
		if current.DJUserID != principal.User.ID {
			return nil, ErrForbidden
		}
		now := s.clock.Now().UTC()
		if current.DJGraceDeadline != nil && !now.Before(*current.DJGraceDeadline) {
			if _, err := s.expire(ctx, current, *current.DJGraceDeadline); err != nil {
				return nil, err
			}
			return nil, ErrSessionEnded
		}
		if current.DJConnected && !takeover {
			return nil, ErrLeaseHeld
		}
		raw := make([]byte, 32)
		if _, err := io.ReadFull(s.random, raw); err != nil {
			return nil, fmt.Errorf("generate DJ lease: %w", err)
		}
		token := hex.EncodeToString(raw)
		hash := sha256.Sum256([]byte(token))
		next := copySession(current)
		next.DJLeaseGeneration++
		next.DJLeaseTokenHash = hash[:]
		next.DJConnectionID = &connectionID
		next.DJConnected = true
		next.DJDisconnectedAt = nil
		next.DJGraceDeadline = nil
		next.EventSeq++
		next.UpdatedAt = now
		result, err := s.store.Commit(ctx, &principal, expected(current), next, EventDraft{Type: "dj-reconnected", Payload: map[string]any{
			"djConnected": true, "djLeaseGeneration": next.DJLeaseGeneration,
			"state": next.State, "positionMs": next.PositionMS, "refTs": next.RefTS,
		}}, nil)
		if err != nil {
			return nil, err
		}
		s.cancelDeadline()
		return Lease{Session: *result.Snapshot.Session, Generation: next.DJLeaseGeneration, Token: token}, nil
	})
	if err != nil {
		return Lease{}, err
	}
	return value.(Lease), nil
}

func (s *Service) Mutate(ctx context.Context, principal auth.Principal, mutation Mutation) (Result, error) {
	if err := requireAdmin(principal); err != nil {
		return Result{}, err
	}
	if mutation.IfVersion == nil || *mutation.IfVersion < 0 {
		return Result{}, ErrInvalidInput
	}
	value, err := s.executor.submit(ctx, func(ctx context.Context) (any, error) {
		return s.mutate(ctx, principal, mutation)
	})
	if err != nil {
		return Result{}, err
	}
	return value.(Result), nil
}

func (s *Service) mutate(ctx context.Context, principal auth.Principal, mutation Mutation) (Result, error) {
	snapshot, err := s.store.Load(ctx)
	if err != nil {
		return Result{}, err
	}
	if snapshot.Mode != ModeSynchronized || snapshot.Session == nil {
		return Result{}, ErrNoActiveSession
	}
	current := *snapshot.Session
	if current.State == StateEnded {
		return Result{}, ErrSessionEnded
	}
	hash, err := leaseHash(mutation.Lease)
	if err != nil || !leaseMatches(current, principal, mutation.Lease, hash) {
		return Result{}, ErrStaleLease
	}
	if current.Version != *mutation.IfVersion {
		return Result{}, ErrVersionConflict
	}
	now := s.clock.Now().UTC()
	next := copySession(current)
	next.EventSeq++
	next.Version++
	next.UpdatedAt = now
	payload := map[string]any{"action": mutation.Action}
	switch mutation.Action {
	case "play":
		if current.State != StatePaused || current.DJGraceDeadline != nil {
			return Result{}, ErrIllegalTransition
		}
		song, err := s.authorizeDestination(ctx, principal.User.ID, current.CurrentMediaID)
		if err != nil {
			return Result{}, err
		}
		if err := validatePosition(current.PositionMS, song.DurationMS); err != nil {
			return Result{}, err
		}
		next.PositionMS = normalizePosition(current.PositionMS, song.DurationMS)
		next.State = StateActive
		next.RefTS = now
	case "pause":
		if current.State != StateActive {
			return Result{}, ErrIllegalTransition
		}
		next.PositionMS = CurrentPosition(current, now)
		next.State = StatePaused
		next.RefTS = now
	case "seek":
		if mutation.PositionMS == nil {
			return Result{}, ErrInvalidInput
		}
		song, err := s.authorizeDestination(ctx, principal.User.ID, current.CurrentMediaID)
		if err != nil {
			return Result{}, err
		}
		if err := validatePosition(*mutation.PositionMS, song.DurationMS); err != nil {
			return Result{}, err
		}
		next.PositionMS = normalizePosition(*mutation.PositionMS, song.DurationMS)
		next.RefTS = now
	case "next", "prev":
		direction := 1
		if mutation.Action == "prev" {
			direction = -1
		}
		destination := current.QueueIndex + direction
		if destination < 0 || destination >= len(current.Queue) {
			return Result{}, ErrIllegalTransition
		}
		if _, err := s.authorizeDestination(ctx, principal.User.ID, current.Queue[destination]); err != nil {
			return Result{}, err
		}
		next.QueueIndex = destination
		next.CurrentMediaID = next.Queue[destination]
		next.PositionMS = 0
		next.RefTS = now
	case "queue":
		if mutation.QueueEdit == nil {
			return Result{}, ErrInvalidInput
		}
		selectionChanged, err := s.editQueue(ctx, principal.User.ID, &next, *mutation.QueueEdit)
		if err != nil {
			return Result{}, err
		}
		if selectionChanged {
			next.PositionMS = 0
			next.RefTS = now
		}
		payload["queue"] = next.Queue
	default:
		return Result{}, ErrInvalidInput
	}
	payload["state"] = next.State
	payload["currentMediaId"] = next.CurrentMediaID
	payload["positionMs"] = next.PositionMS
	payload["refTs"] = next.RefTS
	payload["queueIndex"] = next.QueueIndex
	return s.store.Commit(ctx, &principal, expected(current), next, EventDraft{Type: "state-changed", Payload: payload}, nil)
}

func (s *Service) End(ctx context.Context, principal auth.Principal, ifVersion *int64, lease LeaseCredentials) (Result, error) {
	if err := requireAdmin(principal); err != nil {
		return Result{}, err
	}
	if ifVersion == nil || *ifVersion < 0 {
		return Result{}, ErrInvalidInput
	}
	value, err := s.executor.submit(ctx, func(ctx context.Context) (any, error) {
		snapshot, err := s.store.Load(ctx)
		if err != nil {
			return nil, err
		}
		if snapshot.Mode != ModeSynchronized || snapshot.Session == nil {
			return nil, ErrNoActiveSession
		}
		current := *snapshot.Session
		hash, err := leaseHash(lease)
		if err != nil || !leaseMatches(current, principal, lease, hash) {
			return nil, ErrStaleLease
		}
		if current.Version != *ifVersion {
			return nil, ErrVersionConflict
		}
		return s.end(ctx, &principal, current, "admin", s.clock.Now().UTC())
	})
	if err != nil {
		return Result{}, err
	}
	s.cancelDeadline()
	return value.(Result), nil
}

// DisconnectDJ is the trusted M4B connection-lifecycle boundary. It is not an
// unrestricted public REST operation; the hub supplies the authenticated owner
// and the lease bound to the connection that actually closed.
func (s *Service) DisconnectDJ(ctx context.Context, principal auth.Principal, lease LeaseCredentials) (Result, error) {
	if err := requireAdmin(principal); err != nil {
		return Result{}, err
	}
	value, err := s.executor.submit(ctx, func(ctx context.Context) (any, error) {
		snapshot, err := s.store.Load(ctx)
		if err != nil {
			return nil, err
		}
		if snapshot.Mode != ModeSynchronized || snapshot.Session == nil {
			return nil, ErrNoActiveSession
		}
		current := *snapshot.Session
		hash, err := leaseHash(lease)
		if err != nil || !leaseIdentityMatches(current, principal, lease, hash) {
			return nil, ErrStaleLease
		}
		if !current.DJConnected {
			return resultFromSnapshot(snapshot), nil
		}
		now := s.clock.Now().UTC()
		deadline := now.Add(GracePeriod)
		next := copySession(current)
		next.PositionMS = CurrentPosition(current, now)
		next.RefTS = now
		next.State = StatePaused
		next.DJConnected = false
		next.DJDisconnectedAt = &now
		next.DJGraceDeadline = &deadline
		next.EventSeq++
		next.UpdatedAt = now
		accepted, err := s.store.Commit(ctx, nil, expected(current), next, EventDraft{Type: "dj-disconnected", Payload: map[string]any{
			"state": StatePaused, "positionMs": next.PositionMS, "refTs": now,
			"disconnectedAt": now, "graceUntil": deadline,
		}}, nil)
		if err != nil {
			return nil, err
		}
		s.armDeadline(next.ID, deadline)
		return accepted, nil
	})
	if err != nil {
		return Result{}, err
	}
	return value.(Result), nil
}

func (s *Service) ExpireGrace(ctx context.Context, sessionID int64, deadline time.Time) (Result, error) {
	value, err := s.executor.submit(ctx, func(ctx context.Context) (any, error) {
		snapshot, err := s.store.Load(ctx)
		if err != nil {
			return nil, err
		}
		if snapshot.Mode != ModeSynchronized || snapshot.Session == nil || snapshot.Session.ID != sessionID {
			return nil, ErrNoActiveSession
		}
		return s.expire(ctx, *snapshot.Session, deadline.UTC())
	})
	if err != nil {
		return Result{}, err
	}
	s.cancelDeadline()
	return value.(Result), nil
}

func (s *Service) expire(ctx context.Context, current Session, deadline time.Time) (Result, error) {
	now := s.clock.Now().UTC()
	if current.DJConnected || current.DJGraceDeadline == nil || !current.DJGraceDeadline.Equal(deadline) || now.Before(deadline) {
		return Result{}, ErrStateConflict
	}
	return s.end(ctx, nil, current, "grace-expiry", now)
}

func (s *Service) end(ctx context.Context, principal *auth.Principal, current Session, reason string, now time.Time) (Result, error) {
	next := copySession(current)
	next.PositionMS = CurrentPosition(current, now)
	next.RefTS = now
	next.State = StateEnded
	next.EventSeq++
	next.DJConnected = false
	next.DJDisconnectedAt = nil
	next.DJGraceDeadline = nil
	next.DJConnectionID = nil
	next.DJLeaseTokenHash = nil
	next.UpdatedAt = now
	next.EndedAt = &now
	next.EndReason = &reason
	mode := ModeIndividual
	return s.store.Commit(ctx, principal, expected(current), next, EventDraft{Type: "session-ended", Payload: map[string]any{
		"reason": reason, "mode": ModeIndividual,
	}}, &mode)
}

func (s *Service) authorizeDestination(ctx context.Context, userID int64, mediaID string) (library.Song, error) {
	if !library.ValidResourceID(mediaID) {
		return library.Song{}, ErrInvalidInput
	}
	song, err := s.library.Song(ctx, userID, mediaID)
	if err != nil {
		return library.Song{}, err
	}
	if song.ID != mediaID {
		return library.Song{}, ErrInvalidInput
	}
	return song, nil
}

func (s *Service) editQueue(ctx context.Context, userID int64, session *Session, edit QueueEdit) (bool, error) {
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
		return false, ErrInvalidInput
	}
	oldIndex := session.QueueIndex
	selectionChanged := false
	switch {
	case edit.Add != nil:
		if len(*edit.Add) == 0 || len(session.Queue)+len(*edit.Add) > MaxQueueItems {
			return false, ErrInvalidInput
		}
		for _, id := range *edit.Add {
			if _, err := s.authorizeDestination(ctx, userID, id); err != nil {
				return false, err
			}
		}
		session.Queue = append(session.Queue, (*edit.Add)...)
	case edit.Remove != nil:
		if len(*edit.Remove) == 0 || len(*edit.Remove) >= len(session.Queue) {
			return false, ErrInvalidInput
		}
		removed := make(map[int]bool, len(*edit.Remove))
		for _, index := range *edit.Remove {
			if index < 0 || index >= len(session.Queue) || removed[index] {
				return false, ErrInvalidInput
			}
			removed[index] = true
		}
		queue := make([]string, 0, len(session.Queue)-len(removed))
		newIndex := -1
		for index, id := range session.Queue {
			if removed[index] {
				continue
			}
			if index == oldIndex {
				newIndex = len(queue)
			}
			queue = append(queue, id)
		}
		if newIndex < 0 {
			for index := oldIndex + 1; index < len(session.Queue); index++ {
				if !removed[index] {
					newIndex = index - countRemovedBefore(removed, index)
					break
				}
			}
			if newIndex < 0 {
				for index := oldIndex - 1; index >= 0; index-- {
					if !removed[index] {
						newIndex = index - countRemovedBefore(removed, index)
						break
					}
				}
			}
		}
		if newIndex < 0 || newIndex >= len(queue) {
			return false, ErrInvalidInput
		}
		if _, err := s.authorizeDestination(ctx, userID, queue[newIndex]); err != nil {
			return false, err
		}
		selectionChanged = removed[oldIndex]
		session.Queue = queue
		session.QueueIndex = newIndex
	case edit.Move != nil:
		if edit.Move.From == nil || edit.Move.To == nil {
			return false, ErrInvalidInput
		}
		from, to := *edit.Move.From, *edit.Move.To
		if from < 0 || from >= len(session.Queue) || to < 0 || to >= len(session.Queue) || from == to {
			return false, ErrInvalidInput
		}
		item := session.Queue[from]
		session.Queue = slices.Delete(session.Queue, from, from+1)
		session.Queue = slices.Insert(session.Queue, to, item)
		switch {
		case oldIndex == from:
			session.QueueIndex = to
		case from < oldIndex && to >= oldIndex:
			session.QueueIndex--
		case from > oldIndex && to <= oldIndex:
			session.QueueIndex++
		}
	}
	if !validQueue(session.Queue) || session.QueueIndex < 0 || session.QueueIndex >= len(session.Queue) {
		return false, ErrInvalidInput
	}
	session.CurrentMediaID = session.Queue[session.QueueIndex]
	return selectionChanged, nil
}

func countRemovedBefore(removed map[int]bool, index int) int {
	count := 0
	for removedIndex := range removed {
		if removedIndex < index {
			count++
		}
	}
	return count
}

func leaseHash(lease LeaseCredentials) ([]byte, error) {
	if lease.Generation < 1 || !validConnectionID(lease.ConnectionID) || !validLeaseToken(lease.Token) {
		return nil, ErrInvalidInput
	}
	hash := sha256.Sum256([]byte(lease.Token))
	return hash[:], nil
}

func expected(session Session) Expected {
	return Expected{SessionID: session.ID, Version: session.Version, EventSeq: session.EventSeq}
}

func copySession(session Session) Session {
	session.Queue = slices.Clone(session.Queue)
	session.DJLeaseTokenHash = slices.Clone(session.DJLeaseTokenHash)
	return session
}

func resultFromSnapshot(snapshot Snapshot) Result {
	return Result{Snapshot: snapshot}
}

func (s *Service) armDeadline(sessionID int64, deadline time.Time) {
	s.timerMu.Lock()
	defer s.timerMu.Unlock()
	if s.closed {
		return
	}
	if s.deadline != nil {
		s.deadline.Stop()
	}
	delay := max(time.Duration(0), deadline.Sub(s.clock.Now()))
	s.deadline = s.scheduler.AfterFunc(delay, func() { s.expireFromTimer(sessionID, deadline) })
}

func (s *Service) expireFromTimer(sessionID int64, deadline time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err := s.ExpireGrace(ctx, sessionID, deadline)
	cancel()
	if err == nil || errors.Is(err, ErrNoActiveSession) || errors.Is(err, ErrStateConflict) || errors.Is(err, ErrSessionEnded) {
		return
	}
	s.timerMu.Lock()
	defer s.timerMu.Unlock()
	if !s.closed {
		s.deadline = s.scheduler.AfterFunc(time.Second, func() { s.expireFromTimer(sessionID, deadline) })
	}
}

func (s *Service) cancelDeadline() {
	s.timerMu.Lock()
	defer s.timerMu.Unlock()
	if s.deadline != nil {
		s.deadline.Stop()
		s.deadline = nil
	}
}
