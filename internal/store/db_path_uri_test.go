package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Directory names that SQLite's file: URI syntax would misread: '#' and '?' end the file name, '%41'
// is decoded to 'A', and a space is not allowed unescaped.
var uriHostileDirs = []string{"same#01", "st#ate", "st?x", "st%41te", "with space", "a%3Fb#c?d"}

func pausedRows(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM workspace_pauses WHERE resumed_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestOpenAtCreatesTheDatabaseExactlyAtPathsWithURICharacters: the database is the file the caller
// named, no sibling file appears, and a reopen sees the rows written before.
func TestOpenAtCreatesTheDatabaseExactlyAtPathsWithURICharacters(t *testing.T) {
	for _, name := range uriHostileDirs {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, name)
			dbPath := filepath.Join(dir, "state.db")

			st, err := OpenAt(dbPath, "uri-test", dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetPaused(context.Background(), true, "uri"); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dbPath); err != nil {
				t.Fatalf("the database is not at %s: %v", dbPath, err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != name {
				t.Fatalf("%s holds %v, want only %q (a stray database was created)", parent, entries, name)
			}
			inDir, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range inDir {
				if !strings.HasPrefix(e.Name(), "state.db") {
					t.Fatalf("%s holds the unexpected file %q", dir, e.Name())
				}
			}

			again, err := OpenAt(dbPath, "uri-test", dir)
			if err != nil {
				t.Fatal(err)
			}
			defer again.Close()
			if n := pausedRows(t, again); n != 1 {
				t.Fatalf("reopened database has %d open pauses, want the 1 written before", n)
			}
		})
	}
}

// TestTwoDatabasesWhosePathsDifferAfterAQuestionMarkDoNotShareAFile: before the path was escaped,
// <tmp>/st?x/a.db and <tmp>/st?y/b.db both wrote the file <tmp>/st.
func TestTwoDatabasesWhosePathsDifferAfterAQuestionMarkDoNotShareAFile(t *testing.T) {
	parent := t.TempDir()
	one, err := OpenAt(filepath.Join(parent, "st?x", "a.db"), "one", parent)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := OpenAt(filepath.Join(parent, "st?y", "b.db"), "two", parent)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if err := one.SetPaused(context.Background(), true, "one"); err != nil {
		t.Fatal(err)
	}
	if n := pausedRows(t, two); n != 0 {
		t.Fatalf("the second database sees %d pause(s) of the first", n)
	}
	if _, err := os.Stat(filepath.Join(parent, "st")); !os.IsNotExist(err) {
		t.Fatalf("a stray file %s exists: %v", filepath.Join(parent, "st"), err)
	}
}

// TestResolveDBPathAndOpenHonorADBPathWithAHash: FOREMANKIT_DB_PATH containing '#' opens that file.
func TestResolveDBPathAndOpenHonorADBPathWithAHash(t *testing.T) {
	parent := t.TempDir()
	want := filepath.Join(parent, "st#ate", "x.db")
	t.Setenv("FOREMANKIT_DB_PATH", want)
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(parent, "st#ate"))

	root := t.TempDir()
	got, _, _, err := ResolveDBPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ResolveDBPath = %q, want %q", got, want)
	}
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetPaused(context.Background(), true, "hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("the database is not at %s: %v", want, err)
	}
	if _, err := os.Stat(filepath.Join(parent, "st")); !os.IsNotExist(err) {
		t.Fatalf("a stray file %s exists: %v", filepath.Join(parent, "st"), err)
	}
}

// TestLegacyDatabaseUnderAPathWithURICharactersIsSetAside: setAsideLegacyDB reads the legacy file at
// the real path and renames it, instead of opening the part of the path before the '#'.
func TestLegacyDatabaseUnderAPathWithURICharactersIsSetAside(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "leg#acy?%41")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "state.db")
	legacy, err := sql.Open("sqlite", sqliteURI(dbPath, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE workspace_state (id INTEGER PRIMARY KEY, workspace_id TEXT);
		CREATE TABLE events (id INTEGER PRIMARY KEY, event TEXT);
		INSERT INTO events (event) VALUES ('kept');
	`); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	st, err := OpenAt(dbPath, "legacy-uri", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var version int
	_ = st.DB().QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != SchemaVersion() {
		t.Fatalf("the new database has user_version %d, want %d", version, SchemaVersion())
	}
	matches, _ := filepath.Glob(filepath.Join(parent, "leg*", "state.db.legacy-*"))
	var aside string
	for _, m := range matches {
		if !strings.HasSuffix(m, "-wal") && !strings.HasSuffix(m, "-shm") {
			aside = m
		}
	}
	if aside == "" {
		t.Fatalf("the legacy database was not set aside next to %s", dbPath)
	}
	old, err := sql.Open("sqlite", sqliteURI(aside, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var kept string
	if err := old.QueryRow(`SELECT event FROM events`).Scan(&kept); err != nil || kept != "kept" {
		t.Fatalf("the set-aside database lost its rows: %q %v", kept, err)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 1 {
		t.Fatalf("%s holds %v, want only the directory", parent, entries)
	}
}

func TestSqliteURIEscapesWhatSQLiteWouldParse(t *testing.T) {
	cases := []struct{ path, query, want string }{
		{"/tmp/a/state.db", "x=1", "file:///tmp/a/state.db?x=1"},
		{"/tmp/st#ate/x.db", "", "file:///tmp/st%23ate/x.db"},
		{"/tmp/st?x/x.db", "q", "file:///tmp/st%3Fx/x.db?q"},
		{"/tmp/st%41te/x.db", "q", "file:///tmp/st%2541te/x.db?q"},
		{"/tmp/with space/x.db", "", "file:///tmp/with%20space/x.db"},
		{"rel/x.db", "q", "file:rel/x.db?q"},
	}
	for _, c := range cases {
		if got := sqliteURI(c.path, c.query); got != c.want {
			t.Errorf("sqliteURI(%q, %q) = %q, want %q", c.path, c.query, got, c.want)
		}
	}
}
