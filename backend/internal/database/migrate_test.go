package database

import (
	"testing"
	"testing/fstest"
)

func TestLoadMigrationsOrdersAndParses(t *testing.T) {
	files := fstest.MapFS{
		"0002_second.up.sql": {Data: []byte("SELECT 2")},
		"0001_first.up.sql":  {Data: []byte("SELECT 1")},
		"README.md":          {Data: []byte("ignored")},
	}
	items, err := loadMigrations(files, ".up.sql")
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if len(items) != 2 || items[0].version != 1 || items[1].version != 2 {
		t.Fatalf("unexpected migrations: %+v", items)
	}
}

func TestLoadMigrationsRejectsDuplicateVersion(t *testing.T) {
	files := fstest.MapFS{
		"0001_first.up.sql": {Data: []byte("SELECT 1")},
		"0001_again.up.sql": {Data: []byte("SELECT 1")},
	}
	if _, err := loadMigrations(files, ".up.sql"); err == nil {
		t.Fatal("loadMigrations() expected an error")
	}
}
