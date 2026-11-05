package database

import (
	"context"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/migrations"
)

const migrationTestSchema = "flow_migration_runner_test"

func TestMigrationsAgainstPostgres(t *testing.T) {
	databaseURL := os.Getenv("FLOW_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FLOW_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	basePool, err := Open(ctx, databaseURL, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := basePool.Exec(ctx, `DROP SCHEMA IF EXISTS `+migrationTestSchema+` CASCADE`); err != nil {
		basePool.Close()
		t.Fatal(err)
	}
	if _, err := basePool.Exec(ctx, `CREATE SCHEMA `+migrationTestSchema); err != nil {
		basePool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = basePool.Exec(cleanupContext, `DROP SCHEMA IF EXISTS `+migrationTestSchema+` CASCADE`)
		basePool.Close()
	})
	schemaURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := schemaURL.Query()
	query.Set("search_path", migrationTestSchema)
	schemaURL.RawQuery = query.Encode()
	pool, err := Open(ctx, schemaURL.String(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsSeen <- MigrateUp(ctx, pool, migrations.Files)
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent MigrateUp() error = %v", err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE version IN (1, 2)").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("migration row count = %d", count)
	}
	var m1Columns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema=$1 AND table_name='users'
		  AND column_name IN ('navidrome_username', 'navidrome_cleanup_state')`, migrationTestSchema).Scan(&m1Columns); err != nil {
		t.Fatal(err)
	}
	if m1Columns != 2 {
		t.Fatalf("M1 users schema column count = %d", m1Columns)
	}
	if err := MigrateDownLatest(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUp(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
}
