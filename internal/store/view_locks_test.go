package store

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestViewListsTheLocksAndSaysWhichNothingHolds: the locks and the leases on record come the one
// taken first first, each with its holder, the change of a worker lease and when it stops counting.
// A lock whose process is gone, or whose time ran out, is stale; reading the locks releases none.
func TestViewListsTheLocksAndSaysWhichNothingHolds(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// A process that is gone: the test binary, run with no test to run.
	gone := exec.Command(os.Args[0], "-test.run=^$")
	must(gone.Run())

	// This process holds the lock of the daemon and the lease of a change; a process that is gone
	// left the lock of the merger; and a visual QA session, which outlives the command that opened
	// it, holds its lock under the stamp of the session.
	_, err := st.AcquireLock(ctx, "daemon", LockOptions{OwnerType: "daemon", TTL: time.Minute})
	must(err)
	_, err = st.AcquireLock(ctx, WorkerLeaseKey("add-api"), LockOptions{OwnerType: "worker", Slot: 2, TTL: 5 * time.Minute})
	must(err)
	_, err = st.AcquireLock(ctx, "merger", LockOptions{OwnerType: "merger", PID: gone.Process.Pid, TTL: time.Hour})
	must(err)
	_, err = st.AcquireLock(ctx, "visual_qa", LockOptions{OwnerType: "visual_qa", PID: gone.Process.Pid, Stamp: "20261110T090000Z", TTL: time.Hour})
	must(err)

	byKey := func() map[string]HeldLock {
		t.Helper()
		out := map[string]HeldLock{}
		view(t, st, func(v *View) error {
			locks, err := v.ListLocks(ctx)
			if err != nil {
				return err
			}
			if len(locks) != 4 {
				t.Fatalf("ListLocks returns %d locks, want 4: %+v", len(locks), locks)
			}
			for i, l := range locks {
				if i > 0 && l.AcquiredAt < locks[i-1].AcquiredAt {
					t.Errorf("the locks do not come the one taken first first: %+v", locks)
				}
				out[l.Key] = l
			}
			return nil
		})
		return out
	}

	locks := byKey()
	daemon := locks["daemon"]
	if daemon.OwnerType != "daemon" || daemon.PID != os.Getpid() || daemon.Stale || daemon.ChangeName != "" || daemon.Host == "" {
		t.Errorf("the lock of the daemon = %+v", daemon)
	}
	taken, errTaken := time.Parse(time.RFC3339, daemon.HeartbeatAt)
	expires, errExpires := time.Parse(time.RFC3339, daemon.ExpiresAt)
	if errTaken != nil || errExpires != nil || expires.Sub(taken) != time.Minute {
		t.Errorf("the lock of the daemon was last renewed at %q and stops counting at %q, want a minute apart", daemon.HeartbeatAt, daemon.ExpiresAt)
	}
	if lease := locks[WorkerLeaseKey("add-api")]; lease.ChangeName != "add-api" || lease.Slot != 2 || lease.OwnerType != "worker" || lease.Stale {
		t.Errorf("the lease of a change = %+v", lease)
	}
	if merger := locks["merger"]; !merger.Stale || merger.PID != gone.Process.Pid {
		t.Errorf("a lock whose process is gone = %+v, want it stale", merger)
	}
	// The session holds its lock though the command that opened it is gone.
	if session := locks["visual_qa"]; session.Stale || session.Stamp != "20261110T090000Z" {
		t.Errorf("the lock of a visual QA session = %+v", session)
	}

	// The time of the daemon's lock runs out: it is stale, and still on record.
	if _, err := st.DB().ExecContext(ctx, `UPDATE locks SET heartbeat_at = heartbeat_at - 120000 WHERE lock_key = 'daemon'`); err != nil {
		t.Fatal(err)
	}
	if daemon := byKey()["daemon"]; !daemon.Stale {
		t.Errorf("a lock whose time ran out = %+v, want it stale", daemon)
	}
}
