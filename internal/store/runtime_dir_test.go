package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeDirLivesBesideStateDBNotUnderLog(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_STATE_DIR", stateDir)
	root := t.TempDir()

	rd := RuntimeDir(root)
	id, _ := WorkspaceID(root)
	if want := filepath.Join(stateDir, id); rd != want {
		t.Fatalf("RuntimeDir = %q, want %q", rd, want)
	}
	if st, err := os.Stat(rd); err != nil || !st.IsDir() {
		t.Fatalf("RuntimeDir not created: %v", err)
	}
	if got := RuntimePath(root, "cycle", "x.log"); got != filepath.Join(rd, "cycle", "x.log") {
		t.Fatalf("RuntimePath = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "log")); !os.IsNotExist(err) {
		t.Fatalf("log/ must not be created under the workspace root")
	}

	dbPath, _, _, err := ResolveDBPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dbPath) != stateDir {
		t.Fatalf("db %q should live in %q", dbPath, stateDir)
	}
}

func TestRuntimeDirFollowsExplicitDBPath(t *testing.T) {
	dbDir := t.TempDir()
	t.Setenv("FOREMANKIT_DB_PATH", filepath.Join(dbDir, "sim.db"))
	root := t.TempDir()
	if rd := RuntimeDir(root); !strings.HasPrefix(rd, dbDir+string(os.PathSeparator)) {
		t.Fatalf("RuntimeDir %q should be under %q", rd, dbDir)
	}
}

func TestResolveDBPathMovesLegacyLogDB(t *testing.T) {
	kitHome := t.TempDir()
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_STATE_DIR", "")
	t.Setenv("FOREMANKIT_HOME", kitHome)
	root := t.TempDir()
	legacy := filepath.Join(root, "log", "openspec.db")
	_ = os.MkdirAll(filepath.Dir(legacy), 0o755)
	_ = os.WriteFile(legacy, []byte("db"), 0o644)
	_ = os.WriteFile(legacy+"-wal", []byte("wal"), 0o644)

	dbPath, _, _, err := ResolveDBPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dbPath) != filepath.Join(kitHome, "state") {
		t.Fatalf("db %q not in kit state dir", dbPath)
	}
	if b, _ := os.ReadFile(dbPath); string(b) != "db" {
		t.Fatalf("legacy db not moved")
	}
	if b, _ := os.ReadFile(dbPath + "-wal"); string(b) != "wal" {
		t.Fatalf("legacy wal not moved")
	}
	if fileExists(legacy) {
		t.Fatalf("legacy db still under log/")
	}
}
