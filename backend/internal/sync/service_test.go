package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	stdsync "sync"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
	"github.com/ifrunruhin12/flow/backend/internal/playback"
)

var syncTestNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

type memoryStore struct {
	mu     stdsync.Mutex
	mode   string
	nextID int64
	state  *Session
	events []Event
}

func newMemoryStore() *memoryStore { return &memoryStore{mode: ModeIndividual, nextID: 1} }

func (s *memoryStore) Load(context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(), nil
}

func (s *memoryStore) Start(_ context.Context, _ auth.Principal, proposed Session, _ *int64) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != ModeIndividual || s.state != nil && s.state.State != StateEnded {
		return Result{}, ErrModeConflict
	}
	proposed.ID = s.nextID
	s.nextID++
	proposed.Version = 0
	proposed.EventSeq = 1
	proposed.CreatedAt = proposed.RefTS
	proposed.UpdatedAt = proposed.RefTS
	s.state = pointerSession(proposed)
	s.mode = ModeSynchronized
	event := s.appendEvent(proposed, EventDraft{Type: "session-started", Payload: map[string]any{"mode": s.mode, "session": proposed}}, proposed.RefTS)
	return result(s.mode, proposed, event), nil
}

func (s *memoryStore) Commit(_ context.Context, _ *auth.Principal, expectedState Expected, next Session, draft EventDraft, nextMode *string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != ModeSynchronized || s.state == nil {
		return Result{}, ErrModeConflict
	}
	current := *s.state
	if current.ID != expectedState.SessionID || current.Version != expectedState.Version {
		return Result{}, ErrVersionConflict
	}
	if current.EventSeq != expectedState.EventSeq || current.State == StateEnded {
		return Result{}, ErrStateConflict
	}
	if next.EventSeq != current.EventSeq+1 {
		return Result{}, ErrInvalidInput
	}
	s.state = pointerSession(next)
	if nextMode != nil {
		s.mode = *nextMode
	}
	event := s.appendEvent(next, draft, next.UpdatedAt)
	return result(s.mode, next, event), nil
}

func (s *memoryStore) Recover(_ context.Context, now time.Time) ([]Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil || s.state.State == StateEnded {
		s.mode = ModeIndividual
		return nil, nil
	}
	next := copySession(*s.state)
	next.PositionMS = CurrentPosition(next, now)
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
	reason := "platform-restart"
	next.EndReason = &reason
	s.state = pointerSession(next)
	s.mode = ModeIndividual
	event := s.appendEvent(next, EventDraft{Type: "session-ended", Payload: map[string]any{"reason": reason, "mode": s.mode}}, now)
	return []Result{result(s.mode, next, event)}, nil
}

func (s *memoryStore) snapshot() Snapshot {
	snapshot := Snapshot{Mode: s.mode}
	if s.state != nil && s.state.State != StateEnded {
		state := copySession(*s.state)
		snapshot.Session = &state
		snapshot.ActiveSharedSessionID = &state.ID
	}
	return snapshot
}

func (s *memoryStore) appendEvent(session Session, draft EventDraft, now time.Time) Event {
	payload, _ := json.Marshal(draft.Payload)
	event := Event{ID: int64(len(s.events) + 1), SharedSessionID: session.ID, EventSeq: session.EventSeq, Version: session.Version, Type: draft.Type, Payload: payload, ServerTS: now}
	s.events = append(s.events, event)
	return event
}

func pointerSession(session Session) *Session {
	copy := copySession(session)
	return &copy
}

type fakeClock struct {
	mu  stdsync.Mutex
	now time.Time
}

type fakeTimer struct{ stopped bool }

func (t *fakeTimer) Stop() bool { wasActive := !t.stopped; t.stopped = true; return wasActive }

type fakeScheduler struct {
	mu       stdsync.Mutex
	function func()
	timer    *fakeTimer
}

func (s *fakeScheduler) AfterFunc(_ time.Duration, function func()) timer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.function = function
	s.timer = &fakeTimer{}
	return s.timer
}

func (s *fakeScheduler) Fire() {
	s.mu.Lock()
	function := s.function
	s.mu.Unlock()
	if function != nil {
		function()
	}
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type fakeLibrary struct {
	errors    map[string]error
	durations map[string]int64
}

func (l *fakeLibrary) Song(_ context.Context, _ int64, id string) (library.Song, error) {
	if err := l.errors[id]; err != nil {
		return library.Song{}, err
	}
	duration := l.durations[id]
	if duration == 0 {
		duration = 120_000
	}
	return library.Song{ID: id, DurationMS: duration}, nil
}

type fakeIndividual struct{ session playback.Session }

func (i *fakeIndividual) Get(context.Context, auth.Principal) (playback.Session, error) {
	state := i.session
	state.Queue = slices.Clone(state.Queue)
	return state, nil
}

func syncPrincipal(role string) auth.Principal {
	return auth.Principal{User: auth.User{ID: 1, Role: role, Status: "active", ProvisioningState: "complete"}, TokenHash: make([]byte, 32)}
}

func syncFixture(t *testing.T) (*Service, *memoryStore, *fakeClock, auth.Principal) {
	t.Helper()
	store := newMemoryStore()
	clock := &fakeClock{now: syncTestNow}
	individual := &fakeIndividual{session: playback.Session{
		Queue: []playback.QueueItem{{MediaID: "a"}, {MediaID: "b"}}, QueueIndex: 0,
		CurrentMediaID: pointer("a"), PositionMS: 1_000, Status: "playing",
	}}
	service := New(store, &fakeLibrary{errors: map[string]error{}, durations: map[string]int64{}}, individual,
		WithClock(clock), WithRandom(bytes.NewReader(bytes.Repeat([]byte{0x5a}, 256))))
	t.Cleanup(service.Close)
	return service, store, clock, syncPrincipal("admin")
}

func pointer[T any](value T) *T { return &value }

func acquire(t *testing.T, service *Service, principal auth.Principal, connection string, takeover bool) Lease {
	t.Helper()
	lease, err := service.AcquireLease(context.Background(), principal, connection, takeover)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func mutate(t *testing.T, service *Service, principal auth.Principal, lease Lease, version int64, action string, configure func(*Mutation)) Result {
	t.Helper()
	mutation := Mutation{Action: action, IfVersion: &version, Lease: LeaseCredentials{Generation: lease.Generation, ConnectionID: *lease.Session.DJConnectionID, Token: lease.Token}}
	if configure != nil {
		configure(&mutation)
	}
	result, err := service.Mutate(context.Background(), principal, mutation)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

const (
	connectionOne = "11111111-1111-4111-8111-111111111111"
	connectionTwo = "22222222-2222-4222-8222-222222222222"
)

func TestStateMachineContentTransitionsAndModePreservation(t *testing.T) {
	service, store, clock, admin := syncFixture(t)
	started, err := service.Start(context.Background(), admin, Seed{})
	if err != nil {
		t.Fatal(err)
	}
	state := *started.Snapshot.Session
	if started.Snapshot.Mode != ModeSynchronized || state.Version != 0 || state.EventSeq != 1 || state.CurrentMediaID != "a" || state.PositionMS != 1_000 || state.State != StateActive {
		t.Fatalf("start=%+v", started)
	}
	lease := acquire(t, service, admin, connectionOne, false)
	if lease.Generation != 1 || lease.Session.EventSeq != 2 || !lease.Session.DJConnected {
		t.Fatalf("lease=%+v", lease)
	}
	clock.Advance(5 * time.Second)
	paused := mutate(t, service, admin, lease, 0, "pause", nil)
	state = *paused.Snapshot.Session
	if state.PositionMS != 6_000 || state.State != StatePaused || state.Version != 1 || state.EventSeq != 3 {
		t.Fatalf("pause=%+v", state)
	}
	seeked := mutate(t, service, admin, lease, 1, "seek", func(m *Mutation) { m.PositionMS = pointer(int64(20_000)) })
	if seeked.Snapshot.Session.PositionMS != 20_000 || seeked.Snapshot.Session.State != StatePaused || seeked.Snapshot.Session.Version != 2 {
		t.Fatalf("seek=%+v", seeked.Snapshot.Session)
	}
	played := mutate(t, service, admin, lease, 2, "play", nil)
	if played.Snapshot.Session.State != StateActive || played.Snapshot.Session.Version != 3 {
		t.Fatalf("play=%+v", played.Snapshot.Session)
	}
	next := mutate(t, service, admin, lease, 3, "next", nil)
	if next.Snapshot.Session.CurrentMediaID != "b" || next.Snapshot.Session.QueueIndex != 1 || next.Snapshot.Session.PositionMS != 0 || next.Snapshot.Session.Version != 4 {
		t.Fatalf("next=%+v", next.Snapshot.Session)
	}
	previous := mutate(t, service, admin, lease, 4, "prev", nil)
	if previous.Snapshot.Session.CurrentMediaID != "a" || previous.Snapshot.Session.QueueIndex != 0 || previous.Snapshot.Session.Version != 5 {
		t.Fatalf("prev=%+v", previous.Snapshot.Session)
	}
	add := []string{"c"}
	queued := mutate(t, service, admin, lease, 5, "queue", func(m *Mutation) { m.QueueEdit = &QueueEdit{Add: &add} })
	if !slices.Equal(queued.Snapshot.Session.Queue, []string{"a", "b", "c"}) || queued.Snapshot.Session.Version != 6 {
		t.Fatalf("queue=%+v", queued.Snapshot.Session)
	}
	ended, err := service.End(context.Background(), admin, pointer(int64(6)), LeaseCredentials{Generation: lease.Generation, ConnectionID: connectionOne, Token: lease.Token})
	if err != nil {
		t.Fatal(err)
	}
	if ended.Snapshot.Mode != ModeIndividual || ended.Snapshot.Session.State != StateEnded || *ended.Snapshot.Session.EndReason != "admin" || ended.Snapshot.Session.Version != 6 {
		t.Fatalf("end=%+v", ended)
	}
	if store.events[len(store.events)-1].Type != "session-ended" || store.events[len(store.events)-1].EventSeq != ended.Snapshot.Session.EventSeq {
		t.Fatal("terminal event missing")
	}
}

func TestDJLeaseDisconnectReconnectGraceAndZombieFencing(t *testing.T) {
	service, _, clock, admin := syncFixture(t)
	if _, err := service.Start(context.Background(), admin, Seed{}); err != nil {
		t.Fatal(err)
	}
	first := acquire(t, service, admin, connectionOne, false)
	clock.Advance(10 * time.Second)
	disconnected, err := service.DisconnectDJ(context.Background(), admin, LeaseCredentials{Generation: first.Generation, ConnectionID: connectionOne, Token: first.Token})
	if err != nil {
		t.Fatal(err)
	}
	state := disconnected.Snapshot.Session
	if state.State != StatePaused || state.PositionMS != 11_000 || state.DJConnected || state.DJDisconnectedAt == nil || !state.DJDisconnectedAt.Equal(clock.Now()) || state.DJGraceDeadline == nil || state.Version != 0 {
		t.Fatalf("disconnect=%+v", state)
	}
	repeated, err := service.DisconnectDJ(context.Background(), admin, LeaseCredentials{Generation: first.Generation, ConnectionID: connectionOne, Token: first.Token})
	if err != nil || repeated.Event.ID != 0 || repeated.Snapshot.Session.EventSeq != state.EventSeq {
		t.Fatalf("idempotent disconnect=%+v err=%v", repeated, err)
	}
	if _, err := service.Mutate(context.Background(), admin, Mutation{Action: "play", IfVersion: pointer(int64(0)), Lease: LeaseCredentials{Generation: first.Generation, ConnectionID: connectionOne, Token: first.Token}}); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("zombie command=%v", err)
	}
	clock.Advance(30 * time.Second)
	second := acquire(t, service, admin, connectionTwo, false)
	if second.Generation != 2 || second.Session.DJGraceDeadline != nil || second.Session.State != StatePaused {
		t.Fatalf("reconnect=%+v", second)
	}
	if _, err := service.Mutate(context.Background(), admin, Mutation{Action: "play", IfVersion: pointer(int64(0)), Lease: LeaseCredentials{Generation: first.Generation, ConnectionID: connectionOne, Token: first.Token}}); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("old lease survived reconnect: %v", err)
	}
	played := mutate(t, service, admin, second, 0, "play", nil)
	if played.Snapshot.Session.State != StateActive {
		t.Fatal("new lease could not resume")
	}
	if _, err := service.DisconnectDJ(context.Background(), admin, LeaseCredentials{Generation: second.Generation, ConnectionID: connectionTwo, Token: second.Token}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := service.Snapshot(context.Background())
	deadline := *snapshot.Session.DJGraceDeadline
	clock.Advance(GracePeriod)
	expired, err := service.ExpireGrace(context.Background(), snapshot.Session.ID, deadline)
	if err != nil {
		t.Fatal(err)
	}
	if expired.Snapshot.Mode != ModeIndividual || *expired.Snapshot.Session.EndReason != "grace-expiry" {
		t.Fatalf("expiry=%+v", expired)
	}
	if _, err := service.ExpireGrace(context.Background(), snapshot.Session.ID, deadline); !errors.Is(err, ErrNoActiveSession) {
		t.Fatalf("repeated expiry=%v", err)
	}
}

func TestExpiredGraceCannotBeReacquired(t *testing.T) {
	service, _, clock, admin := syncFixture(t)
	if _, err := service.Start(context.Background(), admin, Seed{}); err != nil {
		t.Fatal(err)
	}
	lease := acquire(t, service, admin, connectionOne, false)
	if _, err := service.DisconnectDJ(context.Background(), admin, LeaseCredentials{Generation: lease.Generation, ConnectionID: connectionOne, Token: lease.Token}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(GracePeriod)
	if _, err := service.AcquireLease(context.Background(), admin, connectionTwo, false); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("expired grace acquisition=%v", err)
	}
	snapshot, err := service.Snapshot(context.Background())
	if err != nil || snapshot.Mode != ModeIndividual || snapshot.Session != nil {
		t.Fatalf("expired snapshot=%+v err=%v", snapshot, err)
	}
}

func TestPersistedGraceTimerEndsSession(t *testing.T) {
	store := newMemoryStore()
	clock := &fakeClock{now: syncTestNow}
	scheduler := &fakeScheduler{}
	individual := &fakeIndividual{session: playback.Session{Queue: []playback.QueueItem{{MediaID: "a"}}, QueueIndex: 0, CurrentMediaID: pointer("a"), Status: "playing"}}
	service := New(store, &fakeLibrary{errors: map[string]error{}, durations: map[string]int64{}}, individual,
		WithClock(clock), WithScheduler(scheduler), WithRandom(bytes.NewReader(bytes.Repeat([]byte{0x6b}, 64))))
	t.Cleanup(service.Close)
	admin := syncPrincipal("admin")
	if _, err := service.Start(context.Background(), admin, Seed{}); err != nil {
		t.Fatal(err)
	}
	lease := acquire(t, service, admin, connectionOne, false)
	if _, err := service.DisconnectDJ(context.Background(), admin, LeaseCredentials{Generation: lease.Generation, ConnectionID: connectionOne, Token: lease.Token}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(GracePeriod)
	scheduler.Fire()
	snapshot, err := service.Snapshot(context.Background())
	if err != nil || snapshot.Mode != ModeIndividual || snapshot.Session != nil || store.state == nil || *store.state.EndReason != "grace-expiry" {
		t.Fatalf("timer expiry snapshot=%+v stored=%+v err=%v", snapshot, store.state, err)
	}
}

func TestConcurrentCommandsAndLeaseAcquisitionAreFenced(t *testing.T) {
	service, store, _, admin := syncFixture(t)
	if _, err := service.Start(context.Background(), admin, Seed{}); err != nil {
		t.Fatal(err)
	}
	first := acquire(t, service, admin, connectionOne, false)
	lease := LeaseCredentials{Generation: first.Generation, ConnectionID: connectionOne, Token: first.Token}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := service.Mutate(context.Background(), admin, Mutation{Action: "pause", IfVersion: pointer(int64(0)), Lease: lease})
			results <- err
		}()
	}
	firstErr, secondErr := <-results, <-results
	if (firstErr == nil) == (secondErr == nil) {
		t.Fatalf("concurrent results=%v,%v", firstErr, secondErr)
	}
	loser := firstErr
	if loser == nil {
		loser = secondErr
	}
	if !errors.Is(loser, ErrVersionConflict) {
		t.Fatalf("loser=%v", loser)
	}
	seen := map[int64]bool{}
	for _, event := range store.events {
		if seen[event.EventSeq] {
			t.Fatalf("duplicate event sequence %d", event.EventSeq)
		}
		seen[event.EventSeq] = true
	}
	if _, err := service.AcquireLease(context.Background(), admin, connectionTwo, false); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second lease=%v", err)
	}
	taken := acquire(t, service, admin, connectionTwo, true)
	if taken.Generation != 2 {
		t.Fatalf("takeover generation=%d", taken.Generation)
	}
	if _, err := service.Mutate(context.Background(), admin, Mutation{Action: "play", IfVersion: pointer(int64(1)), Lease: lease}); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("fenced connection command=%v", err)
	}
}

func TestAuthorizationValidationUnavailableMediaAndQueueEdges(t *testing.T) {
	service, _, _, admin := syncFixture(t)
	member := syncPrincipal("member")
	if _, err := service.Start(context.Background(), member, Seed{MediaID: pointer("a")}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member start=%v", err)
	}
	if _, err := service.Start(context.Background(), admin, Seed{MediaID: pointer("../bad")}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid media=%v", err)
	}
	if _, err := service.Start(context.Background(), admin, Seed{}); err != nil {
		t.Fatal(err)
	}
	lease := acquire(t, service, admin, connectionOne, false)
	if _, err := service.Mutate(context.Background(), member, Mutation{Action: "pause", IfVersion: pointer(int64(0)), Lease: LeaseCredentials{Generation: lease.Generation, ConnectionID: connectionOne, Token: lease.Token}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member mutation=%v", err)
	}
	if _, err := service.Mutate(context.Background(), admin, Mutation{Action: "prev", IfVersion: pointer(int64(0)), Lease: LeaseCredentials{Generation: lease.Generation, ConnectionID: connectionOne, Token: lease.Token}}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("previous boundary=%v", err)
	}
	unchanged, err := service.Snapshot(context.Background())
	if err != nil || unchanged.Session.Version != 0 || unchanged.Session.EventSeq != 2 {
		t.Fatalf("rejected command advanced state=%+v err=%v", unchanged, err)
	}
	removeAll := []int{0, 1}
	if _, err := service.Mutate(context.Background(), admin, Mutation{Action: "queue", IfVersion: pointer(int64(0)), QueueEdit: &QueueEdit{Remove: &removeAll}, Lease: LeaseCredentials{Generation: lease.Generation, ConnectionID: connectionOne, Token: lease.Token}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty shared queue=%v", err)
	}
}

func TestUnavailableMediaNeverBecomesSharedContent(t *testing.T) {
	store := newMemoryStore()
	clock := &fakeClock{now: syncTestNow}
	media := &fakeLibrary{errors: map[string]error{"missing": navidrome.ErrUnavailable}, durations: map[string]int64{}}
	individual := &fakeIndividual{session: playback.Session{Queue: []playback.QueueItem{{MediaID: "a"}}, QueueIndex: 0, CurrentMediaID: pointer("a"), Status: "playing"}}
	service := New(store, media, individual, WithClock(clock), WithRandom(bytes.NewReader(bytes.Repeat([]byte{0x7c}, 64))))
	t.Cleanup(service.Close)
	admin := syncPrincipal("admin")
	if _, err := service.Start(context.Background(), admin, Seed{MediaID: pointer("missing")}); !errors.Is(err, navidrome.ErrUnavailable) {
		t.Fatalf("unavailable start=%v", err)
	}
	if snapshot, _ := service.Snapshot(context.Background()); snapshot.Mode != ModeIndividual || snapshot.Session != nil {
		t.Fatalf("unavailable media created session: %+v", snapshot)
	}
	if _, err := service.Start(context.Background(), admin, Seed{}); err != nil {
		t.Fatal(err)
	}
	lease := acquire(t, service, admin, connectionOne, false)
	add := []string{"missing"}
	if _, err := service.Mutate(context.Background(), admin, Mutation{Action: "queue", IfVersion: pointer(int64(0)), QueueEdit: &QueueEdit{Add: &add}, Lease: LeaseCredentials{Generation: lease.Generation, ConnectionID: connectionOne, Token: lease.Token}}); !errors.Is(err, navidrome.ErrUnavailable) {
		t.Fatalf("unavailable queue=%v", err)
	}
	snapshot, _ := service.Snapshot(context.Background())
	if snapshot.Session.Version != 0 || snapshot.Session.EventSeq != 2 || !slices.Equal(snapshot.Session.Queue, []string{"a"}) {
		t.Fatalf("unavailable queue advanced state: %+v", snapshot.Session)
	}
}

func TestPrincipalAndPositionValidation(t *testing.T) {
	service, _, _, admin := syncFixture(t)
	for name, principal := range map[string]auth.Principal{
		"unauthenticated": {},
		"suspended":       {User: auth.User{ID: 1, Role: "admin", Status: "suspended", ProvisioningState: "complete"}, TokenHash: make([]byte, 32)},
		"password":        {User: auth.User{ID: 1, Role: "admin", Status: "active", ProvisioningState: "complete", MustChangePassword: true}, TokenHash: make([]byte, 32)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Start(context.Background(), principal, Seed{MediaID: pointer("a")}); err == nil {
				t.Fatal("invalid principal started synchronized mode")
			}
		})
	}
	if _, err := service.Start(context.Background(), admin, Seed{MediaID: pointer("a"), PositionMS: pointer(int64(121_001))}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("position beyond duration=%v", err)
	}
	if _, err := service.Start(context.Background(), admin, Seed{MediaID: pointer("a")}); err != nil {
		t.Fatal(err)
	}
	lease := acquire(t, service, admin, connectionOne, false)
	position := int64(121_001)
	if _, err := service.Mutate(context.Background(), admin, Mutation{Action: "seek", IfVersion: pointer(int64(0)), PositionMS: &position, Lease: LeaseCredentials{Generation: lease.Generation, ConnectionID: connectionOne, Token: lease.Token}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("seek beyond duration=%v", err)
	}
}

func TestQueueRemovalAndMovePreserveDeterministicOccurrence(t *testing.T) {
	service, _, clock, admin := syncFixture(t)
	if _, err := service.Start(context.Background(), admin, Seed{}); err != nil {
		t.Fatal(err)
	}
	lease := acquire(t, service, admin, connectionOne, false)
	add := []string{"a", "c"}
	queued := mutate(t, service, admin, lease, 0, "queue", func(m *Mutation) { m.QueueEdit = &QueueEdit{Add: &add} })
	current := mutate(t, service, admin, lease, queued.Snapshot.Session.Version, "next", nil)
	clock.Advance(5 * time.Second)
	removed := []int{0}
	kept := mutate(t, service, admin, lease, current.Snapshot.Session.Version, "queue", func(m *Mutation) { m.QueueEdit = &QueueEdit{Remove: &removed} })
	if kept.Snapshot.Session.CurrentMediaID != "b" || kept.Snapshot.Session.QueueIndex != 0 || kept.Snapshot.Session.RefTS != syncTestNow {
		t.Fatalf("removing before current changed playback anchor: %+v", kept.Snapshot.Session)
	}
	move := QueueMove{From: pointer(0), To: pointer(2)}
	moved := mutate(t, service, admin, lease, kept.Snapshot.Session.Version, "queue", func(m *Mutation) { m.QueueEdit = &QueueEdit{Move: &move} })
	if moved.Snapshot.Session.CurrentMediaID != "b" || moved.Snapshot.Session.QueueIndex != 2 || !slices.Equal(moved.Snapshot.Session.Queue, []string{"a", "c", "b"}) {
		t.Fatalf("move current occurrence=%+v", moved.Snapshot.Session)
	}
	removeCurrent := []int{2}
	selected := mutate(t, service, admin, lease, moved.Snapshot.Session.Version, "queue", func(m *Mutation) { m.QueueEdit = &QueueEdit{Remove: &removeCurrent} })
	if selected.Snapshot.Session.CurrentMediaID != "c" || selected.Snapshot.Session.QueueIndex != 1 || selected.Snapshot.Session.PositionMS != 0 || !selected.Snapshot.Session.RefTS.Equal(clock.Now()) {
		t.Fatalf("current removal did not select previous deterministically: %+v", selected.Snapshot.Session)
	}
}

func TestRestartRecoveryAndAuthoritativeTiming(t *testing.T) {
	service, store, clock, admin := syncFixture(t)
	if _, err := service.Start(context.Background(), admin, Seed{}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(7 * time.Second)
	recovered, err := service.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Snapshot.Mode != ModeIndividual || recovered[0].Snapshot.Session.PositionMS != 8_000 || *recovered[0].Snapshot.Session.EndReason != "platform-restart" {
		t.Fatalf("recovery=%+v", recovered)
	}
	if again, err := service.Recover(context.Background()); err != nil || len(again) != 0 {
		t.Fatalf("idempotent recovery=%+v err=%v", again, err)
	}
	if len(store.events) != 2 || store.events[1].Version != 0 || store.events[1].EventSeq != 2 {
		t.Fatalf("recovery events=%+v", store.events)
	}
	session := Session{State: StateActive, PositionMS: 10_000, RefTS: syncTestNow}
	if position := CurrentPosition(session, syncTestNow.Add(2*time.Second)); position != 12_000 {
		t.Fatalf("position=%d", position)
	}
	decision := DecideDrift(session, DriftReport{PositionMS: 8_000}, syncTestNow.Add(2*time.Second), 2_000)
	if !decision.Correct || decision.DriftMS != -4_000 || decision.TargetPositionMS != 12_000 {
		t.Fatalf("drift=%+v", decision)
	}
	decision = DecideDrift(session, DriftReport{PositionMS: 8_000, Seeking: true}, syncTestNow.Add(2*time.Second), 2_000)
	if decision.Correct || !decision.Deferred {
		t.Fatalf("seeking drift=%+v", decision)
	}
}

func TestRestartRecoveryEndsPausedAndGraceSessions(t *testing.T) {
	for _, state := range []string{"paused", "grace"} {
		t.Run(state, func(t *testing.T) {
			service, _, _, admin := syncFixture(t)
			if _, err := service.Start(context.Background(), admin, Seed{}); err != nil {
				t.Fatal(err)
			}
			lease := acquire(t, service, admin, connectionOne, false)
			if state == "paused" {
				mutate(t, service, admin, lease, 0, "pause", nil)
			} else if _, err := service.DisconnectDJ(context.Background(), admin, LeaseCredentials{Generation: lease.Generation, ConnectionID: connectionOne, Token: lease.Token}); err != nil {
				t.Fatal(err)
			}
			recovered, err := service.Recover(context.Background())
			if err != nil || len(recovered) != 1 || recovered[0].Snapshot.Session.State != StateEnded || *recovered[0].Snapshot.Session.EndReason != "platform-restart" {
				t.Fatalf("%s recovery=%+v err=%v", state, recovered, err)
			}
		})
	}
}
