package store

import (
	sc "CodeAgent/internal/session/config"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestProductNewSchemaAndOldDatabaseUntouched(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "new.sqlite")
	s, e := Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Init(ctx); e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	for _, enabled := range []bool{true, false} {
		id := "off"
		if enabled {
			id = "on"
		}
		e = s.CreateSession(ctx, sc.SessionInfo{ID: id, Title: id, WorkingDir: t.TempDir(), CreatedAt: now, UpdatedAt: now, MemoryEnabled: enabled})
		if e != nil {
			t.Fatal(e)
		}
		info, e := s.GetSession(ctx, id)
		if e != nil || info.MemoryEnabled != enabled {
			t.Fatal("round trip", e)
		}
	}
	oldPath := filepath.Join(t.TempDir(), "old.sqlite")
	raw, e := sql.Open("sqlite", oldPath)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = raw.Exec("CREATE TABLE sessions(id TEXT); INSERT INTO sessions VALUES ('preserved'); PRAGMA user_version=5"); e != nil {
		t.Fatal(e)
	}
	raw.Close()
	old, e := Open(ctx, oldPath)
	if e != nil {
		t.Fatal(e)
	}
	defer old.Close()
	if e = old.Init(ctx); e == nil {
		t.Fatal("old DB accepted")
	}
	var version int
	if e = old.db.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 5 {
		t.Fatal("old version mutated", e)
	}
	var id string
	if e = old.db.QueryRow("SELECT id FROM sessions").Scan(&id); e != nil || id != "preserved" {
		t.Fatal("old row mutated", e)
	}
}
