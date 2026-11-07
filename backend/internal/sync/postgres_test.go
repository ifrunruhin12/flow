package sync

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/database"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/playback"
	"github.com/ifrunruhin12/flow/backend/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

type blockingLibrary struct {
	entered chan struct{}
	release chan struct{}
}

func (l *blockingLibrary) Song(_ context.Context, _ int64, id string) (library.Song, error) {
	if id == "b" {
		close(l.entered)
		<-l.release
	}
	return library.Song{ID: id, DurationMS: 180_000}, nil
}

// This test uses separate executors against one real PostgreSQL schema. It
// verifies that row locks and constraints, rather than the process-local
// executor alone, enforce the M4A invariants.
func TestSynchronizationPostgresTransactionsAndRecovery(t *testing.T) {
	dsn := os.Getenv("FLOW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FLOW_TEST_DATABASE_URL is not set; live PostgreSQL synchronization is unverified")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	base, err := database.Open(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("flow_sync_%d", time.Now().UnixNano())
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		base.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = base.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE")
		base.Close()
	})
	schemaURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := schemaURL.Query()
	query.Set("search_path", schema)
	schemaURL.RawQuery = query.Encode()
	pool, err := database.Open(ctx, schemaURL.String(), 12)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.MigrateUp(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	principal := insertSynchronizationAdmin(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE session_state SET queue='[{"mediaId":"a","addedAt":"2026-11-07T12:00:00Z"}]',
		queue_index=0,current_media_id='a',position_ms=1000,playback_status='playing' WHERE user_id=$1`, principal.User.ID); err != nil {
		t.Fatal(err)
	}
	media := &fakeLibrary{errors: map[string]error{}, durations: map[string]int64{"a": 180_000, "b": 180_000}}
	individual := &fakeIndividual{session: playbackSession("a", 1_000)}
	first := New(NewPostgresStore(pool), media, individual, WithClock(&fakeClock{now: syncTestNow}))
	second := New(NewPostgresStore(pool), media, individual, WithClock(&fakeClock{now: syncTestNow}))
	defer first.Close()
	defer second.Close()

	t.Run("stale individual seed cannot enter synchronized mode", func(t *testing.T) {
		staleState := playbackSession("a", 1_000)
		staleState.Revision = 9
		service := New(NewPostgresStore(pool), media, &fakeIndividual{session: staleState}, WithClock(&fakeClock{now: syncTestNow}))
		defer service.Close()
		if _, err := service.Start(ctx, principal, Seed{}); !errors.Is(err, ErrStateConflict) {
			t.Fatalf("stale seed start=%v", err)
		}
		var mode string
		if err := pool.QueryRow(ctx, `SELECT mode FROM settings WHERE id=1`).Scan(&mode); err != nil || mode != ModeIndividual {
			t.Fatalf("stale seed mode=%q err=%v", mode, err)
		}
	})

	t.Run("concurrent mode start has one winner", func(t *testing.T) {
		ready := make(chan struct{})
		results := make(chan error, 2)
		for _, service := range []*Service{first, second} {
			go func(service *Service) {
				<-ready
				_, err := service.Start(ctx, principal, Seed{})
				results <- err
			}(service)
		}
		close(ready)
		succeeded, conflicted := 0, 0
		for range 2 {
			err := <-results
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrModeConflict):
				conflicted++
			default:
				t.Fatalf("concurrent start=%v", err)
			}
		}
		if succeeded != 1 || conflicted != 1 {
			t.Fatalf("starts succeeded=%d conflicted=%d", succeeded, conflicted)
		}
	})

	var winning Lease
	t.Run("concurrent lease acquisition is fenced", func(t *testing.T) {
		type leaseResult struct {
			lease Lease
			err   error
		}
		ready := make(chan struct{})
		results := make(chan leaseResult, 2)
		connections := []string{connectionOne, connectionTwo}
		for index, service := range []*Service{first, second} {
			connection := connections[index]
			go func(service *Service) {
				<-ready
				lease, err := service.AcquireLease(ctx, principal, connection, false)
				results <- leaseResult{lease: lease, err: err}
			}(service)
		}
		close(ready)
		succeeded, rejected := 0, 0
		for range 2 {
			result := <-results
			if result.err == nil {
				succeeded++
				winning = result.lease
			} else if errors.Is(result.err, ErrLeaseHeld) || errors.Is(result.err, ErrStateConflict) {
				rejected++
			} else {
				t.Fatalf("concurrent lease=%v", result.err)
			}
		}
		if succeeded != 1 || rejected != 1 || winning.Generation != 1 || winning.Session.DJConnectionID == nil {
			t.Fatalf("leases succeeded=%d rejected=%d winner=%+v", succeeded, rejected, winning)
		}
	})

	credentials := LeaseCredentials{Generation: winning.Generation, ConnectionID: *winning.Session.DJConnectionID, Token: winning.Token}
	t.Run("concurrent content commands allocate one sequence", func(t *testing.T) {
		ready := make(chan struct{})
		results := make(chan error, 2)
		for _, service := range []*Service{first, second} {
			go func(service *Service) {
				<-ready
				_, err := service.Mutate(ctx, principal, Mutation{Action: "pause", IfVersion: pointer(int64(0)), Lease: credentials})
				results <- err
			}(service)
		}
		close(ready)
		succeeded, conflicted := 0, 0
		for range 2 {
			err := <-results
			if err == nil {
				succeeded++
			} else if errors.Is(err, ErrVersionConflict) {
				conflicted++
			} else {
				t.Fatalf("concurrent mutation=%v", err)
			}
		}
		if succeeded != 1 || conflicted != 1 {
			t.Fatalf("mutations succeeded=%d conflicted=%d", succeeded, conflicted)
		}
		rows, err := pool.Query(ctx, `SELECT event_seq FROM sync_events ORDER BY event_seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var sequences []int64
		for rows.Next() {
			var sequence int64
			if err := rows.Scan(&sequence); err != nil {
				t.Fatal(err)
			}
			sequences = append(sequences, sequence)
		}
		if fmt.Sprint(sequences) != "[1 2 3]" {
			t.Fatalf("event sequences=%v", sequences)
		}
	})

	t.Run("event failure rolls checkpoint back", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_sync_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.type='state-changed' THEN RAISE EXCEPTION 'test event failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_sync_event BEFORE INSERT ON sync_events FOR EACH ROW EXECUTE FUNCTION reject_sync_event()`); err != nil {
			t.Fatal(err)
		}
		position := int64(5_000)
		if _, err := first.Mutate(ctx, principal, Mutation{Action: "seek", IfVersion: pointer(int64(1)), PositionMS: &position, Lease: credentials}); err == nil {
			t.Fatal("event failure committed mutation")
		}
		var version, sequence, positionAfter int64
		if err := pool.QueryRow(ctx, `SELECT version,event_seq,position_ms FROM shared_sessions WHERE state <> 'ended'`).Scan(&version, &sequence, &positionAfter); err != nil {
			t.Fatal(err)
		}
		if version != 1 || sequence != 3 || positionAfter != 0 {
			t.Fatalf("rolled state version=%d seq=%d position=%d", version, sequence, positionAfter)
		}
		if _, err := pool.Exec(ctx, `DROP TRIGGER reject_sync_event ON sync_events; DROP FUNCTION reject_sync_event()`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("commit rechecks account after media validation", func(t *testing.T) {
		media := &blockingLibrary{entered: make(chan struct{}), release: make(chan struct{})}
		service := New(NewPostgresStore(pool), media, individual, WithClock(&fakeClock{now: syncTestNow}))
		defer service.Close()
		add := []string{"b"}
		result := make(chan error, 1)
		go func() {
			_, err := service.Mutate(ctx, principal, Mutation{Action: "queue", IfVersion: pointer(int64(1)), QueueEdit: &QueueEdit{Add: &add}, Lease: credentials})
			result <- err
		}()
		select {
		case <-media.entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET status='suspended' WHERE id=$1`, principal.User.ID); err != nil {
			t.Fatal(err)
		}
		close(media.release)
		if err := <-result; !errors.Is(err, auth.ErrAccountNotActive) {
			t.Fatalf("suspended command=%v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET status='active' WHERE id=$1`, principal.User.ID); err != nil {
			t.Fatal(err)
		}
		var version int64
		if err := pool.QueryRow(ctx, `SELECT version FROM shared_sessions WHERE state <> 'ended'`).Scan(&version); err != nil || version != 1 {
			t.Fatalf("suspended write version=%d err=%v", version, err)
		}
	})

	t.Run("individual CAS is atomically gated by platform mode", func(t *testing.T) {
		store := playback.NewPostgresStore(pool)
		current, err := store.Load(ctx, principal)
		if err != nil {
			t.Fatal(err)
		}
		next := current
		next.Volume = .25
		if _, err := store.CompareAndSwapInMode(ctx, principal, current.Revision, next, ModeIndividual); !errors.Is(err, playback.ErrSynchronizedMode) {
			t.Fatalf("individual write during sync=%v", err)
		}
		preserved, err := store.Load(ctx, principal)
		if err != nil || preserved.Revision != current.Revision || preserved.Volume != current.Volume {
			t.Fatalf("individual state changed=%+v err=%v", preserved, err)
		}
	})

	t.Run("fresh process recovery safe-ends without individual mutation", func(t *testing.T) {
		freshPool, err := database.Open(ctx, schemaURL.String(), 4)
		if err != nil {
			t.Fatal(err)
		}
		defer freshPool.Close()
		restarted := New(NewPostgresStore(freshPool), media, individual, WithClock(&fakeClock{now: syncTestNow.Add(time.Minute)}))
		defer restarted.Close()
		recovered, err := restarted.Recover(ctx)
		if err != nil || len(recovered) != 1 || recovered[0].Snapshot.Mode != ModeIndividual || *recovered[0].Snapshot.Session.EndReason != "platform-restart" {
			t.Fatalf("recovery=%+v err=%v", recovered, err)
		}
		if repeated, err := restarted.Recover(ctx); err != nil || len(repeated) != 0 {
			t.Fatalf("repeated recovery=%+v err=%v", repeated, err)
		}
		var mediaID string
		var position int64
		if err := freshPool.QueryRow(ctx, `SELECT current_media_id,position_ms FROM session_state WHERE user_id=$1`, principal.User.ID).Scan(&mediaID, &position); err != nil || mediaID != "a" || position != 1_000 {
			t.Fatalf("individual state media=%q position=%d err=%v", mediaID, position, err)
		}
	})

	t.Run("M4A migration down and up preserves individual state", func(t *testing.T) {
		if err := database.MigrateDownLatest(ctx, pool, migrations.Files); err != nil {
			t.Fatal(err)
		}
		if err := database.MigrateUp(ctx, pool, migrations.Files); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_state WHERE user_id=$1`, principal.User.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("individual row count=%d err=%v", count, err)
		}
	})
}

func insertSynchronizationAdmin(t *testing.T, ctx context.Context, pool *pgxpool.Pool) auth.Principal {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO credential_key_versions(version,state) VALUES(1,'active')`); err != nil {
		t.Fatal(err)
	}
	principal := auth.Principal{User: auth.User{Username: "sync-admin", DisplayName: "Sync Admin", Role: "admin", Status: "active", ProvisioningState: "complete"}}
	if err := pool.QueryRow(ctx, `INSERT INTO users(username,display_name,password_hash,role,status,provisioning_state,
		navidrome_username,navidrome_user_id,navidrome_password_enc,navidrome_cred_key_version,is_navidrome_admin)
		VALUES('sync-admin','Sync Admin','test-only-hash','admin','active','complete','sync-admin','nd-admin',$1,1,true) RETURNING id`, []byte("ciphertext")).Scan(&principal.User.ID); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("sync-admin-session"))
	principal.TokenHash = hash[:]
	if _, err := pool.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')`, principal.TokenHash, principal.User.ID); err != nil {
		t.Fatal(err)
	}
	return principal
}

func playbackSession(mediaID string, position int64) playback.Session {
	return playback.Session{Queue: []playback.QueueItem{{MediaID: mediaID}}, QueueIndex: 0, CurrentMediaID: pointer(mediaID), PositionMS: position, Status: "playing", Volume: 1}
}
