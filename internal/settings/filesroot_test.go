package settings_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestAWorktreeCutBeforeTheUpdateReadsTheSettingsOfTheMainCheckout: the worktree of a change that
// was being built when `foreman-kit update` rewrote the files still holds the ones an older kit
// read. None of their keys is read, and nothing is read as the value the kit ships either: the
// change goes on with the settings the application has now, in its main checkout. A worktree whose
// files are of today reads its own, and so does one of an application nobody has updated.
func TestAWorktreeCutBeforeTheUpdateReadsTheSettingsOfTheMainCheckout(t *testing.T) {
	main := t.TempDir()
	settingstest.WriteFiles(t, main, map[string]string{
		"pipeline": "dispatch:\n  workers: 5\n",
		"agents":   "limits:\n  idle_timeout: 7m\nroles:\n  build:\n    timeout: 33m\n",
	})
	worktree := filepath.Join(main, ".worktrees", "worker-add-search")
	settingstest.WriteFormerFiles(t, worktree)

	if got := settings.FilesRoot(worktree); got != main {
		t.Fatalf("FilesRoot(%s) = %s, want the main checkout", worktree, got)
	}
	if got := settings.FilesRoot(main); got != main {
		t.Fatalf("FilesRoot of the main checkout = %s", got)
	}
	var notes bytes.Buffer
	r := &settings.Reader{Root: worktree, FilesAlone: true, Stderr: &notes}
	if got := r.Int(settings.Workers); got != 5 {
		t.Errorf("dispatch.workers = %d, want the 5 of the main checkout", got)
	}
	if got := r.Duration(settings.IdleTimeout); got != 7*time.Minute {
		t.Errorf("limits.idle_timeout = %s, want the 7m of the main checkout", got)
	}
	if got := r.Duration(settings.RoleTimeout("build")); got != 33*time.Minute {
		t.Errorf("roles.build.timeout = %s, want the 33m of the main checkout", got)
	}
	// daemon.yaml, which the worktree does not have at all, is the one of the main checkout too.
	if got := r.Duration(settings.CycleEvery); got != 60*time.Minute {
		t.Errorf("cycle.every = %s", got)
	}
	if notes.Len() != 0 {
		t.Errorf("a key was read as the value the kit ships:\n%s", notes.String())
	}
	for _, files := range [][]string{settings.AgentCallFiles, settings.DispatcherFiles, settings.CycleFiles} {
		if why := settings.Unusable(worktree, files...); why != "" {
			t.Errorf("a process of the worktree would wait for %v: %s", files, why)
		}
	}

	// A worktree whose files are of today reads its own.
	ofToday := filepath.Join(main, ".worktrees", "worker-of-today")
	settingstest.WriteFiles(t, ofToday, map[string]string{"pipeline": "dispatch:\n  workers: 3\n"})
	if got := settings.FilesRoot(ofToday); got != ofToday {
		t.Fatalf("FilesRoot of a worktree of today = %s", got)
	}
	if got := (&settings.Reader{Root: ofToday, FilesAlone: true}).Int(settings.Workers); got != 3 {
		t.Errorf("a worktree of today read dispatch.workers = %d, want its own 3", got)
	}

	// Nobody has updated the application: its main checkout holds the former files too. The
	// worktree reads its own, and what reads them says what rewrites them.
	settingstest.WriteFormerFiles(t, main)
	if got := settings.FilesRoot(worktree); got != worktree {
		t.Fatalf("FilesRoot of a worktree of an application that was not updated = %s", got)
	}
	if why := settings.Unusable(worktree, "pipeline"); !strings.Contains(why, ".foreman-kit/pipeline.yaml holds keys an older foreman-kit read") || !strings.Contains(why, "foreman-kit update rewrites it") {
		t.Errorf("the worktree of an application that was not updated: %q", why)
	}
}
