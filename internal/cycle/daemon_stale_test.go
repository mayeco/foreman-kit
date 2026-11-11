package cycle

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

func plantDaemonRow(t *testing.T, st *store.Store, pid int, acquiredAt int64) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), `
		INSERT INTO locks (lock_key, owner_type, pid, host, acquired_at, heartbeat_at, ttl_seconds, metadata_json)
		VALUES ('daemon', 'daemon', ?, '', ? * 1000, ? * 1000, 86400, '{}')
		ON CONFLICT(lock_key) DO UPDATE SET pid = excluded.pid, acquired_at = excluded.acquired_at, heartbeat_at = excluded.heartbeat_at
	`, pid, acquiredAt, acquiredAt); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonStatusAndStopIgnoreStaleRows plants daemon lock rows left by a crash: one carrying this
// process's PID from before it started (a restarted container), and one whose PID now belongs to an
// unrelated process started later (PID reuse after a reboot). Neither may be reported as running, and
// `daemon stop` must never signal the unrelated process.
func TestDaemonStatusAndStopIgnoreStaleRows(t *testing.T) {
	root := t.TempDir()
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-daemon-stale", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	tenMinutesAgo := time.Now().UTC().Unix() - 600

	plantDaemonRow(t, st, os.Getpid(), tenMinutesAgo)
	var out bytes.Buffer
	StatusDaemon(ctx, root, st, &out)
	if !strings.Contains(out.String(), "daemon: stopped") {
		t.Fatalf("a row from an earlier process with our PID was reported as running: %q", out.String())
	}

	sleeper := exec.Command("sleep", "30")
	sleeper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	reused := sleeper.Process.Pid
	go func() { _ = sleeper.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-reused, syscall.SIGKILL) })

	plantDaemonRow(t, st, reused, tenMinutesAgo)
	out.Reset()
	StopDaemon(ctx, root, st, &out, &out)
	time.Sleep(200 * time.Millisecond)
	if !store.IsPIDAlive(reused) {
		t.Fatalf("daemon stop signaled an unrelated process that reused the daemon's PID (output %q)", out.String())
	}
	if !strings.Contains(out.String(), "daemon: not running") {
		t.Fatalf("expected `daemon stop` to report not running, got %q", out.String())
	}
}
