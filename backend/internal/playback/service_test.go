package playback

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
)

var testNow = time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)

func pointer[T any](value T) *T { return &value }

func testPrincipal(id int64) auth.Principal {
	return auth.Principal{User: auth.User{ID: id, Role: "member", Status: "active", ProvisioningState: "complete"}, TokenHash: make([]byte, 32)}
}

type memoryStore struct {
	mu     sync.Mutex
	states map[int64]Session
}

func clone(state Session) Session {
	state.Queue = slices.Clone(state.Queue)
	return state
}

func (s *memoryStore) Load(_ context.Context, principal auth.Principal) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[principal.User.ID]
	if !ok {
		state = DefaultSession(testNow)
		s.states[principal.User.ID] = state
	}
	return clone(state), nil
}

func (s *memoryStore) CompareAndSwap(_ context.Context, principal auth.Principal, revision int64, next Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.states[principal.User.ID]
	if current.Revision != revision {
		return Session{}, &ConflictError{Current: clone(current)}
	}
	next.Revision = revision + 1
	next.UpdatedAt = testNow.Add(time.Duration(next.Revision) * time.Second)
	s.states[principal.User.ID] = clone(next)
	return clone(next), nil
}

type testLibrary struct {
	mu     sync.Mutex
	calls  []int64
	errors map[string]error
	before func(string)
}

func (l *testLibrary) Song(_ context.Context, id int64, media string) (library.Song, error) {
	l.mu.Lock()
	l.calls = append(l.calls, id)
	err := l.errors[media]
	before := l.before
	l.mu.Unlock()
	if before != nil {
		before(media)
	}
	if err != nil {
		return library.Song{}, err
	}
	return library.Song{ID: media, DurationMS: 120_000}, nil
}

func testService() (*Service, *testLibrary) {
	l := &testLibrary{errors: map[string]error{}}
	s := New(&memoryStore{states: map[int64]Session{}}, l)
	s.now = func() time.Time { return testNow }
	return s, l
}

func update(t *testing.T, s *Service, state Session, m Mutation) Session {
	t.Helper()
	m.IfRevision = pointer(state.Revision)
	next, err := s.Update(context.Background(), testPrincipal(1), m)
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != state.Revision+1 || !next.UpdatedAt.After(state.UpdatedAt) {
		t.Fatalf("accepted command did not advance revision/timestamp: %+v", next)
	}
	return next
}

func start(t *testing.T, s *Service, ids ...string) Session {
	t.Helper()
	state, err := s.Get(context.Background(), testPrincipal(1))
	if err != nil {
		t.Fatal(err)
	}
	queue := make([]QueueItem, len(ids))
	for index, id := range ids {
		queue[index] = QueueItem{MediaID: id}
	}
	return update(t, s, state, Mutation{Queue: &queue, Status: pointer("playing"), QueueIndex: pointer(0), PositionMS: pointer(int64(0))})
}

func TestDefaultOwnershipAndAuthorization(t *testing.T) {
	s, media := testService()
	state, err := s.Get(context.Background(), testPrincipal(1))
	if err != nil || !reflect.DeepEqual(state, DefaultSession(testNow)) {
		t.Fatalf("default = %+v, error = %v", state, err)
	}
	state = start(t, s, "song-a")
	other, err := s.Get(context.Background(), testPrincipal(2))
	if err != nil || len(other.Queue) != 0 || other.Revision != 0 {
		t.Fatalf("another account observed playback: %+v, %v", other, err)
	}
	_, err = s.Update(context.Background(), testPrincipal(2), Mutation{IfRevision: pointer(state.Revision), Volume: pointer(.1)})
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Current.Revision != 0 || len(conflict.Current.Queue) != 0 {
		t.Fatalf("another account's revision was accepted or leaked: %v", err)
	}
	for _, id := range media.calls {
		if id != 1 {
			t.Fatalf("media lookup borrowed another principal: %d", id)
		}
	}
	for _, test := range []struct {
		name string
		user auth.Principal
		want error
	}{
		{"anonymous", auth.Principal{}, auth.ErrUnauthenticated},
		{"suspended", func() auth.Principal { p := testPrincipal(1); p.User.Status = "suspended"; return p }(), auth.ErrAccountNotActive},
		{"incomplete", func() auth.Principal { p := testPrincipal(1); p.User.ProvisioningState = "failed"; return p }(), auth.ErrAccountNotActive},
		{"password gate", func() auth.Principal { p := testPrincipal(1); p.User.MustChangePassword = true; return p }(), ErrPasswordChangeRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := s.Get(context.Background(), test.user); !errors.Is(err, test.want) {
				t.Fatalf("read authorization = %v", err)
			}
			if _, err := s.Update(context.Background(), test.user, Mutation{IfRevision: pointer(state.Revision), Volume: pointer(.2)}); !errors.Is(err, test.want) {
				t.Fatalf("write authorization = %v", err)
			}
		})
	}
}

func TestTransportSelectionCheckpointAndBoundaries(t *testing.T) {
	s, _ := testService()
	state := start(t, s, "a", "b", "c")
	state = update(t, s, state, Mutation{PositionMS: pointer(int64(12_000)), Status: pointer("paused")})
	if state.Status != "paused" || state.PositionMS != 12_000 {
		t.Fatal("pause did not persist actual position")
	}
	state = update(t, s, state, Mutation{Status: pointer("playing")})
	state = update(t, s, state, Mutation{Command: pointer("checkpoint"), QueueIndex: pointer(0), CurrentMediaID: pointer("a"), PositionMS: pointer(int64(22_000))})
	state = update(t, s, state, Mutation{QueueIndex: pointer(1)})
	if *state.CurrentMediaID != "b" || state.PositionMS != 0 {
		t.Fatal("selection failed to reset position")
	}
	state = update(t, s, state, Mutation{Command: pointer("previous"), PositionMS: pointer(int64(4_000))})
	if state.QueueIndex != 1 || state.PositionMS != 0 {
		t.Fatal("previous restart threshold failed")
	}
	state = update(t, s, state, Mutation{Command: pointer("previous"), PositionMS: pointer(int64(3_000))})
	if state.QueueIndex != 0 {
		t.Fatal("previous did not move backward")
	}
	state = update(t, s, state, Mutation{Command: pointer("previous")})
	if state.QueueIndex != 0 || state.PositionMS != 0 {
		t.Fatal("previous at beginning failed")
	}
	state = update(t, s, state, Mutation{Command: pointer("next"), PositionMS: pointer(int64(30_000))})
	if state.QueueIndex != 1 || state.PositionMS != 0 {
		t.Fatal("next did not move/reset")
	}
	state = update(t, s, state, Mutation{Command: pointer("ended"), CurrentMediaID: pointer("b"), QueueIndex: pointer(1), PositionMS: pointer(int64(120_000))})
	if state.QueueIndex != 2 || state.Status != "playing" || state.PositionMS != 0 {
		t.Fatal("natural end did not advance")
	}
	state = update(t, s, state, Mutation{Command: pointer("ended"), CurrentMediaID: pointer("c"), QueueIndex: pointer(2), PositionMS: pointer(int64(120_000))})
	if state.QueueIndex != 2 || state.Status != "paused" || state.PositionMS != 120_000 {
		t.Fatal("last end did not pause at end")
	}
	state = update(t, s, state, Mutation{Volume: pointer(.4324), Shuffle: pointer(true)})
	cleared, err := s.Clear(context.Background(), testPrincipal(1), pointer(state.Revision))
	if err != nil || cleared.Revision != state.Revision+1 || cleared.Volume != .432 || !cleared.Shuffle || cleared.CurrentMediaID != nil || cleared.PositionMS != 0 || cleared.Status != "paused" || len(cleared.Queue) != 0 {
		t.Fatalf("clear = %+v, %v", cleared, err)
	}
	for _, command := range []string{"next", "previous"} {
		cleared = update(t, s, cleared, Mutation{Command: &command})
		if cleared.QueueIndex != 0 || cleared.CurrentMediaID != nil {
			t.Fatal("empty queue command corrupted state")
		}
	}
}

func TestQueueOccurrencesRemovalReorderAndShuffle(t *testing.T) {
	s, _ := testService()
	state := start(t, s, "a", "b", "a", "c")
	state = update(t, s, state, Mutation{QueueIndex: pointer(2), PositionMS: pointer(int64(5_000))})
	selected := state.Queue[2]
	state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Move: &Move{From: pointer(2), To: pointer(0)}}})
	if state.QueueIndex != 0 || !sameItem(state.Queue[0], selected) || state.PositionMS != 5_000 {
		t.Fatal("move lost duplicate occurrence or position")
	}
	state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Remove: pointer([]int{1})}})
	if state.QueueIndex != 0 || !sameItem(state.Queue[0], selected) || state.PositionMS != 5_000 {
		t.Fatal("noncurrent removal moved the playback head")
	}
	state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Remove: pointer([]int{0})}})
	if *state.CurrentMediaID != "b" || state.PositionMS != 0 || state.QueueIndex != 0 {
		t.Fatal("current removal did not select successor")
	}
	state = update(t, s, state, Mutation{QueueIndex: pointer(1)})
	state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Remove: pointer([]int{1})}})
	if *state.CurrentMediaID != "b" || state.QueueIndex != 0 {
		t.Fatal("last removal did not select predecessor")
	}
	state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Add: pointer([]string{"b", "a", "c"})}})
	if sameItem(state.Queue[0], state.Queue[1]) {
		t.Fatal("duplicate additions must have distinct occurrences even with a fixed clock")
	}
	canonical := slices.Clone(state.Queue)
	state = update(t, s, state, Mutation{Shuffle: pointer(true)})
	if !reflect.DeepEqual(canonical, state.Queue) {
		t.Fatal("shuffle rewrote canonical queue")
	}
	order := PlaybackOrder(state)
	if !reflect.DeepEqual(order, PlaybackOrder(clone(state))) {
		t.Fatal("shuffle was not reproducible from persisted state")
	}
	state = update(t, s, state, Mutation{QueueIndex: pointer(order[0])})
	for _, index := range order[1:] {
		state = update(t, s, state, Mutation{Command: pointer("next")})
		if state.QueueIndex != index {
			t.Fatal("shuffle did not traverse its stable permutation")
		}
	}
	state = update(t, s, state, Mutation{Command: pointer("next")})
	if state.Status != "paused" || state.QueueIndex != order[len(order)-1] {
		t.Fatal("shuffle wrapped or corrupted index at end")
	}
	state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Remove: pointer([]int{0, 1, 2, 3})}})
	if len(state.Queue) != 0 || state.Status != "paused" {
		t.Fatal("remove-all failed")
	}
	state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Add: pointer([]string{"a"})}})
	if *state.CurrentMediaID != "a" || state.Status != "paused" {
		t.Fatal("empty addition unexpectedly played")
	}
}

func TestInvalidInputsAndUnavailableMediaDoNotWrite(t *testing.T) {
	s, media := testService()
	state := start(t, s, "a", "b")
	tests := []Mutation{
		{}, {IfRevision: pointer(int64(-1)), Volume: pointer(.5)},
		{QueueIndex: pointer(2)}, {QueueIndex: pointer(-1)},
		{PositionMS: pointer(int64(-1))}, {PositionMS: pointer(MaxPositionMS + 1)},
		{PositionMS: pointer(int64(122_000))}, {Volume: pointer(-.1)}, {Volume: pointer(1.1)},
		{Status: pointer("buffering")}, {CurrentMediaID: pointer("../a")},
		{CurrentMediaID: pointer("b"), QueueIndex: pointer(0)},
		{HasCurrentMediaID: true}, {Queue: pointer([]QueueItem{{MediaID: "http://internal"}})},
		{QueueEdit: &QueueEdit{Add: pointer([]string{})}},
		{QueueEdit: &QueueEdit{Remove: pointer([]int{0, 0})}},
		{QueueEdit: &QueueEdit{Remove: pointer([]int{5})}},
		{QueueEdit: &QueueEdit{Move: &Move{From: pointer(0), To: pointer(2)}}},
		{QueueEdit: &QueueEdit{Move: &Move{}}},
		{QueueEdit: &QueueEdit{Add: pointer([]string{"a"}), Remove: pointer([]int{0})}},
		{QueueEdit: &QueueEdit{Add: pointer([]string{"a"})}, QueueIndex: pointer(0)},
		{Command: pointer("repeat")}, {Command: pointer("checkpoint"), PositionMS: pointer(int64(1))},
		{Command: pointer("next"), Volume: pointer(.5)},
	}
	for index, mutation := range tests {
		if index > 1 {
			mutation.IfRevision = pointer(state.Revision)
		}
		if _, err := s.Update(context.Background(), testPrincipal(1), mutation); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid input %d returned %v", index, err)
		}
	}
	oversized := make([]QueueItem, MaxQueueItems+1)
	if _, err := s.Update(context.Background(), testPrincipal(1), Mutation{IfRevision: pointer(state.Revision), Queue: &oversized}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("oversized queue accepted")
	}
	for _, cause := range []error{navidrome.ErrNotFound, navidrome.ErrAuthorization, navidrome.ErrUnavailable} {
		media.errors["missing"] = cause
		if _, err := s.Update(context.Background(), testPrincipal(1), Mutation{IfRevision: pointer(state.Revision), QueueEdit: &QueueEdit{Add: pointer([]string{"missing"})}}); !errors.Is(err, cause) {
			t.Fatalf("unavailable media = %v", err)
		}
	}
	latest, _ := s.Get(context.Background(), testPrincipal(1))
	if !reflect.DeepEqual(state, latest) {
		t.Fatal("a rejected mutation changed durable state")
	}
	media.errors["a"] = navidrome.ErrNotFound
	state = update(t, s, state, Mutation{Volume: pointer(.5)})
	state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Move: &Move{From: pointer(0), To: pointer(1)}}})
	state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Remove: pointer([]int{1})}})
	if *state.CurrentMediaID != "b" {
		t.Fatal("unavailable current item could not be removed")
	}
}

func TestCheckpointRevisionAndIdentityRace(t *testing.T) {
	s, media := testService()
	state := start(t, s, "a", "b")
	entered, release := make(chan struct{}), make(chan struct{})
	media.before = func(id string) {
		if id == "a" {
			close(entered)
			<-release
		}
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.Update(context.Background(), testPrincipal(1), Mutation{IfRevision: pointer(state.Revision), Command: pointer("checkpoint"), CurrentMediaID: pointer("a"), QueueIndex: pointer(0), PositionMS: pointer(int64(10_000))})
		result <- err
	}()
	<-entered
	selected := update(t, s, state, Mutation{QueueIndex: pointer(1)})
	close(release)
	var conflict *ConflictError
	if err := <-result; !errors.As(err, &conflict) || conflict.Current.Revision != selected.Revision || *conflict.Current.CurrentMediaID != "b" {
		t.Fatalf("checkpoint race failed: %v", err)
	}
	for _, command := range []string{"checkpoint", "ended"} {
		_, err := s.Update(context.Background(), testPrincipal(1), Mutation{IfRevision: pointer(selected.Revision), Command: &command, CurrentMediaID: pointer("a"), QueueIndex: pointer(0), PositionMS: pointer(int64(10_000))})
		if !errors.As(err, &conflict) {
			t.Fatal("rebased stale audio identity was accepted")
		}
	}
}

func TestCompetingMutationsRejectOneAndReturnCurrent(t *testing.T) {
	s, _ := testService()
	state, _ := s.Get(context.Background(), testPrincipal(1))
	results := make(chan error, 2)
	for _, volume := range []float64{.2, .8} {
		go func() {
			_, err := s.Update(context.Background(), testPrincipal(1), Mutation{IfRevision: pointer(state.Revision), Volume: &volume})
			results <- err
		}()
	}
	success, conflicts := 0, 0
	for range 2 {
		err := <-results
		var conflict *ConflictError
		if err == nil {
			success++
		} else if errors.As(err, &conflict) && conflict.Current.Revision == 1 {
			conflicts++
		} else {
			t.Fatalf("unexpected race result %v", err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", success, conflicts)
	}
}

func TestMutationJSONRejectsMissingNullAndUnknownValues(t *testing.T) {
	for _, data := range []string{
		`null`, `[]`, `{"ifRevision":null,"volume":0.5}`, `{"ifRevision":0,"volume":null}`,
		`{"ifRevision":0,"userId":2}`, `{"ifRevision":0.5,"volume":0.5}`,
		`{"ifRevision":0,"queueEdit":{"move":{"from":0,"to":1,"userId":2}}}`,
	} {
		var m Mutation
		if err := json.Unmarshal([]byte(data), &m); err == nil {
			t.Fatalf("invalid JSON contract accepted: %s", data)
		}
	}
	var m Mutation
	if err := json.Unmarshal([]byte(`{"ifRevision":0,"queue":[],"currentMediaId":null}`), &m); err != nil || !m.HasCurrentMediaID {
		t.Fatalf("explicit empty selection = %v", err)
	}
}

func TestRetainedUnavailableMediaCanBeRemovedAndPaused(t *testing.T) {
	for _, cause := range []error{navidrome.ErrNotFound, navidrome.ErrAuthorization, navidrome.ErrUnavailable} {
		t.Run(cause.Error(), func(t *testing.T) {
			s, media := testService()
			state := start(t, s, "a", "b")
			media.errors["a"], media.errors["b"] = cause, cause
			state = update(t, s, state, Mutation{Status: pointer("paused"), PositionMS: pointer(int64(5_000))})
			if state.Status != "paused" || state.PositionMS != 5_000 {
				t.Fatal("library failure prevented durable pause/checkpoint")
			}
			_, err := s.Update(context.Background(), testPrincipal(1), Mutation{IfRevision: pointer(state.Revision), CurrentMediaID: pointer("a")})
			if !errors.Is(err, cause) {
				t.Fatalf("explicit unavailable selection bypassed validation: %v", err)
			}
			state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Remove: pointer([]int{0})}})
			if len(state.Queue) != 1 || *state.CurrentMediaID != "b" || state.PositionMS != 0 {
				t.Fatal("unavailable successor blocked removal or corrupted selection")
			}
			state = update(t, s, state, Mutation{QueueEdit: &QueueEdit{Remove: pointer([]int{0})}})
			if len(state.Queue) != 0 || state.Status != "paused" {
				t.Fatal("unavailable queue could not be emptied")
			}
		})
	}
}

func TestQueueReplacementPreservesOccurrenceAndRejectsForgedTimestamps(t *testing.T) {
	s, _ := testService()
	state := start(t, s, "a", "b", "a")
	state = update(t, s, state, Mutation{QueueIndex: pointer(2), PositionMS: pointer(int64(9_000))})
	selected := state.Queue[2]
	queue := []QueueItem{selected, state.Queue[0], {MediaID: "c", AddedAt: testNow.Add(24 * time.Hour)}}
	state = update(t, s, state, Mutation{Queue: &queue})
	if state.QueueIndex != 0 || state.PositionMS != 9_000 || !sameItem(state.Queue[0], selected) {
		t.Fatal("replacement confused duplicate media occurrences")
	}
	if state.Queue[2].AddedAt.Equal(queue[2].AddedAt) || !state.Queue[2].AddedAt.Before(testNow.Add(time.Second)) {
		t.Fatal("client supplied the authoritative occurrence timestamp")
	}
}

func TestCheckpointCannotOverwriteQueueMutation(t *testing.T) {
	s, media := testService()
	state := start(t, s, "a", "b")
	entered, release := make(chan struct{}), make(chan struct{})
	media.before = func(id string) {
		if id == "a" {
			close(entered)
			<-release
		}
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.Update(context.Background(), testPrincipal(1), Mutation{IfRevision: pointer(state.Revision), Command: pointer("checkpoint"), CurrentMediaID: pointer("a"), QueueIndex: pointer(0), PositionMS: pointer(int64(10_000))})
		result <- err
	}()
	<-entered
	added := update(t, s, state, Mutation{QueueEdit: &QueueEdit{Add: pointer([]string{"c"})}})
	close(release)
	var conflict *ConflictError
	if err := <-result; !errors.As(err, &conflict) || conflict.Current.Revision != added.Revision || len(conflict.Current.Queue) != 3 || conflict.Current.PositionMS != 0 {
		t.Fatalf("checkpoint overwrote a newer queue mutation: %v", err)
	}
}
