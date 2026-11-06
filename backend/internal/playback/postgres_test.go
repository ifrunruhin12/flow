package playback

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"reflect"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/database"
	"github.com/ifrunruhin12/flow/backend/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests exercise the real pgx store, including SQL CAS and lifecycle
// locking. They skip without a disposable PostgreSQL URL, never use a memory
// substitute, and create/drop only their uniquely named test schema.
func TestIndividualPlaybackPostgres(t *testing.T) {
	dsn := os.Getenv("FLOW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FLOW_TEST_DATABASE_URL is not set; live PostgreSQL playback is unverified")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	base, err := database.Open(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("flow_playback_%d", time.Now().UnixNano())
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
	pool, err := database.Open(ctx, schemaURL.String(), 8)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { pool.Close() }()
	// Upgrade over real M1 rows, not just an empty schema.
	previous := fstest.MapFS{}
	for _, name := range []string{"0001_m0_foundation.up.sql", "0002_m1_auth_membership.up.sql"} {
		data, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		previous[name] = &fstest.MapFile{Data: data}
	}
	if err := database.MigrateUp(ctx, pool, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO credential_key_versions(version,state) VALUES(1,'active')`); err != nil {
		t.Fatal(err)
	}
	first := insertPlaybackAccount(t, ctx, pool, "before-m3")
	if err := database.MigrateUp(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	second := insertPlaybackAccount(t, ctx, pool, "after-m3")
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_state WHERE user_id IN ($1,$2)`, first.User.ID, second.User.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("backfill/trigger row count=%d err=%v", count, err)
	}
	media := &testLibrary{errors: map[string]error{}}
	service := New(NewPostgresStore(pool), media)
	state, err := service.Get(ctx, first)
	if err != nil || state.Revision != 0 || state.Queue == nil || state.Volume != 1 || state.Status != "paused" {
		t.Fatalf("Postgres default=%+v error=%v", state, err)
	}
	queue := []QueueItem{{MediaID: "a"}, {MediaID: "b"}}
	state, err = service.Update(ctx, first, Mutation{IfRevision: pointer(state.Revision), Queue: &queue, Status: pointer("playing"), PositionMS: pointer(int64(7_000))})
	if err != nil || state.PositionMS != 7_000 || state.Revision != 1 {
		t.Fatalf("Postgres update=%+v err=%v", state, err)
	}
	other, err := service.Get(ctx, second)
	if err != nil || other.Revision != 0 || len(other.Queue) != 0 {
		t.Fatalf("cross-user state=%+v err=%v", other, err)
	}

	t.Run("competing conditional writes", func(t *testing.T) {
		ready := make(chan struct{})
		results := make(chan error, 2)
		var wait sync.WaitGroup
		for _, volume := range []float64{.2, .8} {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-ready
				next := state
				next.Volume = volume
				_, err := NewPostgresStore(pool).CompareAndSwap(ctx, first, state.Revision, next)
				results <- err
			}()
		}
		close(ready)
		wait.Wait()
		close(results)
		succeeded, conflicted := 0, 0
		for err := range results {
			var conflict *ConflictError
			if err == nil {
				succeeded++
			} else if errors.As(err, &conflict) && conflict.Current.Revision == state.Revision+1 {
				conflicted++
			} else {
				t.Fatalf("competing SQL write=%v", err)
			}
		}
		if succeeded != 1 || conflicted != 1 {
			t.Fatalf("SQL CAS successes=%d conflicts=%d", succeeded, conflicted)
		}
	})
	state, err = service.Get(ctx, first)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("checkpoint loses to track change during lookup", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		media.before = func(id string) {
			if id == "a" {
				close(entered)
				<-release
			}
		}
		result := make(chan error, 1)
		go func() {
			_, err := service.Update(ctx, first, Mutation{IfRevision: pointer(state.Revision), Command: pointer("checkpoint"), CurrentMediaID: pointer("a"), QueueIndex: pointer(0), PositionMS: pointer(int64(10_000))})
			result <- err
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		selected, err := service.Update(ctx, first, Mutation{IfRevision: pointer(state.Revision), QueueIndex: pointer(1)})
		close(release)
		if err != nil {
			t.Fatal(err)
		}
		var conflict *ConflictError
		if err := <-result; !errors.As(err, &conflict) || *conflict.Current.CurrentMediaID != "b" || conflict.Current.Revision != selected.Revision || conflict.Current.PositionMS != 0 {
			t.Fatalf("checkpoint overwrote track selection: %v", err)
		}
		media.before = nil
	})
	state, err = service.Get(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	state, err = service.Update(ctx, first, Mutation{IfRevision: pointer(state.Revision), Command: pointer("checkpoint"), CurrentMediaID: pointer("b"), QueueIndex: pointer(1), PositionMS: pointer(int64(19_000))})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("new pool and service restore durable state", func(t *testing.T) {
		pool.Close()
		pool, err = database.Open(ctx, schemaURL.String(), 8)
		if err != nil {
			t.Fatal(err)
		}
		service = New(NewPostgresStore(pool), media)
		restored, err := service.Get(ctx, first)
		if err != nil || !reflect.DeepEqual(restored, state) {
			t.Fatalf("fresh process dependencies lost state: %+v err=%v", restored, err)
		}
	})

	t.Run("session and status recheck after remote validation", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		media.before = func(id string) {
			if id == "a" {
				close(entered)
				<-release
			}
		}
		result := make(chan error, 1)
		go func() {
			_, err := service.Update(ctx, first, Mutation{IfRevision: pointer(state.Revision), QueueIndex: pointer(0)})
			result <- err
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET status='suspended' WHERE id=$1`, first.User.ID); err != nil {
			close(release)
			t.Fatal(err)
		}
		close(release)
		if err := <-result; !errors.Is(err, auth.ErrAccountNotActive) {
			t.Fatalf("suspension race accepted mutation: %v", err)
		}
		media.before = nil
		if _, err := pool.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, first.User.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Get(ctx, first); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("revoked login read state: %v", err)
		}
		var revision int64
		if err := pool.QueryRow(ctx, `SELECT revision FROM session_state WHERE user_id=$1`, first.User.ID).Scan(&revision); err != nil || revision != state.Revision {
			t.Fatal("revoked/suspended client wrote state")
		}
	})

	t.Run("migration down and reapply preserve M1 accounts", func(t *testing.T) {
		if err := database.MigrateDownLatest(ctx, pool, migrations.Files); err != nil {
			t.Fatal(err)
		}
		if err := database.MigrateUp(ctx, pool, migrations.Files); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_state`).Scan(&count); err != nil || count != 2 {
			t.Fatalf("migration reapply row count=%d err=%v", count, err)
		}
	})
}

func insertPlaybackAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, username string) auth.Principal {
	t.Helper()
	principal := testPrincipal(0)
	err := pool.QueryRow(ctx, `INSERT INTO users(username,display_name,password_hash,role,status,provisioning_state,
		navidrome_username,navidrome_user_id,navidrome_password_enc,navidrome_cred_key_version)
		VALUES($1,$1,'test-only-hash','member','active','complete',$1,$1,$2,1) RETURNING id`, username, []byte("test-ciphertext")).Scan(&principal.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(username))
	principal.TokenHash = hash[:]
	if _, err := pool.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')`, principal.TokenHash, principal.User.ID); err != nil {
		t.Fatal(err)
	}
	return principal
}
