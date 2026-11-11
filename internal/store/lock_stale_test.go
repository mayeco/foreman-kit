package store

import (
	"os"
	"testing"
	"time"
)

// TestIsLockStaleOwnPIDRowFromAnEarlierProcess verifies that a row carrying this process's PID is stale
// when it was written before this process started (a container restart reuses PID 1, for example), and
// live when this process wrote it.
func TestIsLockStaleOwnPIDRowFromAnEarlierProcess(t *testing.T) {
	now := time.Now().UTC().Unix()
	old := LockRecord{LockKey: "daemon", PID: os.Getpid(), AcquiredAt: selfStartUnix - 600, HeartbeatAt: now, TTLSeconds: 7200}
	if !IsLockStale(old, "", now) {
		t.Fatal("a row with our PID written before this process started must be stale")
	}
	mine := LockRecord{LockKey: "daemon", PID: os.Getpid(), AcquiredAt: now, HeartbeatAt: now, TTLSeconds: 7200}
	if IsLockStale(mine, "", now) {
		t.Fatal("a row this process wrote must not be stale")
	}
}
