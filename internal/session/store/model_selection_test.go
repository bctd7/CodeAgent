package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"CodeAgent/internal/session/config"
	sessql "CodeAgent/internal/session/sql"
)

func TestModelSelectionMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := strings.Replace(sessql.CreateTablesSQL, "    model_selection_json TEXT,\n", "", 1)
	if _, err := db.Exec(oldSchema + sessql.CreateIndexesSQL + sessql.SetUserVersionV2SQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO sessions (id,title,working_dir,created_at,updated_at) VALUES ('old','kept','',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := s.GetSession(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "kept" || info.ModelSelection != nil {
		t.Fatal("legacy data modified or default invented")
	}
	if err := s.WithTx(context.Background(), func(tx *Tx) error {
		return tx.UpdateModelSelection(context.Background(), "old", config.ModelSelection{Ref: "p/m"}, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	info, err = s.GetSession(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if info.ModelSelection == nil || info.ModelSelection.Ref != "p/m" {
		t.Fatal("selection not stored")
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("version %d", version)
	}
}

func TestModelSelectionMigrationRollbackAndFutureVersion(t *testing.T) {
	for _, future := range []bool{false, true} {
		t.Run(map[bool]string{false: "malformed", true: "future"}[future], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "reject.db")
			s, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if future {
				if _, err := s.db.Exec(`PRAGMA user_version=99`); err != nil {
					t.Fatal(err)
				}
			} else {
				schema := strings.Replace(sessql.CreateTablesSQL, "    model_selection_json TEXT,\n", "", 1)
				if _, err := s.db.Exec(schema + sessql.SetUserVersionV2SQL + `; ALTER TABLE sessions ADD COLUMN model_selection_json TEXT`); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Init(context.Background()); err == nil {
				t.Fatal("invalid version/structure accepted")
			}
			var version int
			s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
			if (future && version != 99) || (!future && version != 2) {
				t.Fatal("failed migration changed version")
			}
		})
	}
}

func TestUnversionedAndV1MigrateToModelSelection(t *testing.T) {
	for _, version := range []int{0, 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			schema := strings.Replace(sessql.CreateTablesSQL, "    model_selection_json TEXT,\n", "", 1)
			// These older schemas predate tool-result association columns.
			schema = strings.Replace(schema, "    assistant_entry_id TEXT REFERENCES entries(id),\n    tool_call_id TEXT,\n", "", 1)
			if _, err := db.Exec(schema + fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := db.Exec(`INSERT INTO sessions (id,title,working_dir,created_at,updated_at) VALUES ('legacy','kept','',?,?)`, now, now); err != nil {
				t.Fatal(err)
			}
			db.Close()
			s, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := s.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			info, err := s.GetSession(context.Background(), "legacy")
			if err != nil {
				t.Fatal(err)
			}
			if info.Title != "kept" || info.ModelSelection != nil {
				t.Fatal("legacy metadata lost or default invented")
			}
			var current int
			if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil || current != 3 {
				t.Fatal(current, err)
			}
		})
	}
}
