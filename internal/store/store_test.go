package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveDBPathCanonicalFromWorktreesAndSubmodules(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	t.Setenv("FOREMANKIT_STATE_DIR", stateDir)
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", "")

	app1 := filepath.Join(tmp, "work", "my-app")
	wt1 := filepath.Join(app1, ".worktrees", "worker-auth")
	sub1 := filepath.Join(app1, "repos", "backend")
	if err := os.MkdirAll(filepath.Join(app1, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app1, ".foreman-kit/kit.lock"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, ".git"), []byte("gitdir: ../../.git/worktrees/worker-auth\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub1, 0o755); err != nil {
		t.Fatal(err)
	}

	dbMain, idMain, rootMain, err := ResolveDBPath(app1)
	if err != nil {
		t.Fatal(err)
	}
	dbWT, idWT, rootWT, err := ResolveDBPath(wt1)
	if err != nil {
		t.Fatal(err)
	}
	dbSub, idSub, rootSub, err := ResolveDBPath(sub1)
	if err != nil {
		t.Fatal(err)
	}

	if dbMain != dbWT || dbMain != dbSub {
		t.Fatalf("expected identical db path across main, worktree, and submodule; got:\nmain=%s\nwt=%s\nsub=%s", dbMain, dbWT, dbSub)
	}
	if idMain != idWT || idMain != idSub || rootMain != rootWT || rootMain != rootSub {
		t.Fatalf("expected identical workspaceID and canonicalRoot; got %s / %s / %s", idMain, idWT, idSub)
	}

	// Second app with the same folder name in a different parent directory must get a different hash12
	app2 := filepath.Join(tmp, "personal", "my-app")
	if err := os.MkdirAll(filepath.Join(app2, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app2, ".foreman-kit/kit.lock"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbOther, idOther, _, err := ResolveDBPath(app2)
	if err != nil {
		t.Fatal(err)
	}
	if dbOther == dbMain || idOther == idMain {
		t.Fatalf("expected distinct workspace DB for different path with same basename; got %s == %s", dbOther, dbMain)
	}
}

func TestConcurrentLocksAndEvents(t *testing.T) {
	tmp := t.TempDir()
	dbFile := filepath.Join(tmp, "test.db")
	st, err := OpenAt(dbFile, "test-1234567890ab", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	const workers = 20
	const eventsPerWorker = 50

	var winners atomic.Int32
	var wg sync.WaitGroup
	errCh := make(chan error, workers*eventsPerWorker)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			_, err := st.AcquireLock(ctx, "change:auth-flow", LockOptions{
				OwnerType: "worker",
				PID:       os.Getpid(),
				Host:      "localhost",
				Slot:      workerID + 1,
				TTL:       time.Hour,
			})
			if err == nil {
				winners.Add(1)
			} else if err != ErrLockHeld {
				errCh <- fmt.Errorf("unexpected lock error: %w", err)
			}

			for j := 0; j < eventsPerWorker; j++ {
				if err := st.EmitEvent(ctx, EventInput{
					Event:  "worker.tick",
					Change: "auth-flow",
					Payload: map[string]any{
						"worker": workerID,
						"seq":    j,
					},
				}); err != nil {
					errCh <- fmt.Errorf("emit event error: %w", err)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
	if got := winners.Load(); got != 1 {
		t.Fatalf("expected exactly 1 lock winner among %d concurrent goroutines, got %d", workers, got)
	}

	evts, err := st.QueryEvents(ctx, EventFilter{Event: "worker.tick"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != workers*eventsPerWorker {
		t.Fatalf("expected %d events, got %d", workers*eventsPerWorker, len(evts))
	}

	// Verify stale lock reclamation when PID is dead
	if err := st.ReleaseLock(ctx, "change:auth-flow", 0); err != nil {
		t.Fatal(err)
	}
	hostname, _ := os.Hostname()
	_, err = st.AcquireLock(ctx, "change:stale-test", LockOptions{
		OwnerType: "worker",
		PID:       99999999, // non-existent local PID
		Host:      hostname,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	reclaimed, err := st.AcquireLock(ctx, "change:stale-test", LockOptions{
		OwnerType: "worker",
		PID:       os.Getpid(),
		Host:      hostname,
		TTL:       time.Hour,
	})
	if err != nil {
		t.Fatalf("expected stale lock from dead PID to be reclaimed, got err: %v", err)
	}
	if reclaimed.PID != os.Getpid() {
		t.Fatalf("expected reclaimed lock PID %d, got %d", os.Getpid(), reclaimed.PID)
	}
}

func TestMigrateFromFilesEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()

	// Create legacy config/ and log/ layout matching webpropco production structure
	_ = os.MkdirAll(filepath.Join(tmp, ".foreman-kit"), 0o755)
	_ = os.MkdirAll(filepath.Join(tmp, "log", "ready_to_merge"), 0o755)
	_ = os.MkdirAll(filepath.Join(tmp, "log", "blocked", "history"), 0o755)
	_ = os.MkdirAll(filepath.Join(tmp, "log", "cycle"), 0o755)
	_ = os.MkdirAll(filepath.Join(tmp, "log", "portal", "runs"), 0o755)
	_ = os.MkdirAll(filepath.Join(tmp, "openspec", "changes", "archive", "2026-11-07-feat-alpha"), 0o755)

	_ = os.WriteFile(filepath.Join(tmp, ".foreman-kit", "agent_history.json"), []byte(`[{"change_name":"feat-alpha","integrated_at":"2026-11-07T10:00:00Z"}]`), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, ".foreman-kit", "agent_backlog.json"), []byte(`[{"change_name":"feat-beta","priority":"high","description":"Beta feature"}]`), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, ".foreman-kit", "agent_proposals.json"), []byte(`[{"change_name":"feat-gamma","status":"proposed","description":"Gamma proposal"}]`), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, "log", "ready_to_merge", "feat-ready.json"), []byte(`{"change":"feat-ready","branch":"feat/spec-feat-ready","head":"deadbeef"}`), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, "log", "blocked", "feat-blocked.json"), []byte(`{"change":"feat-blocked","reason":"gate_failure","details":"rspec failed"}`), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, "log", "blocked", "history", "feat-blocked-1.json"), []byte(`{"change":"feat-blocked","retry":1,"reason":"gate_failure"}`), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, "log", "events.jsonl"), []byte(
		`{"ts":"2026-11-07T10:00:00Z","event":"agent.exit","pid":123,"change":"feat-alpha","engine":"hermes","phase":"apply"}`+"\n"+
			`{"ts":"2026-11-07T10:05:00Z","event":"merge.integrated","pid":124,"change":"feat-alpha"}`+"\n",
	), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, "openspec", "changes", "archive", "2026-11-07-feat-alpha", "repairs.md"), []byte(
		"# Repairs\n\n## repair-1, 2026-11-07T09:50:00Z, hermes\n\nAsked to fix (gate_failure):\n\n```\nfail\n```\n\nResult: passed.\n",
	), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, "log", "cycle", "cycle-20261107T100000Z.log"), []byte("cycle log"), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, "log", "cycle", "visual_queue.json"), []byte(`{"items":[{"id":"1","change":"feat-alpha","detail":"Check alpha UI","status":"pending"}]}`), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, "log", "portal", "runs", "run-1.json"), []byte(`{"id":"run-1","agent":"hermes","status":"ok"}`), 0o644)

	st, err := OpenAt(filepath.Join(tmp, "migrated.db"), "mig-test", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rep, err := st.MigrateFromFiles(ctx, tmp, nil)
	if err != nil {
		t.Fatalf("MigrateFromFiles failed: %v", err)
	}
	if rep.HistoryItems != 1 || rep.BacklogItems != 1 || rep.Proposals != 1 ||
		rep.ReadyManifests != 1 || rep.BlockedChanges != 1 || rep.BlockHistory != 1 ||
		rep.EventsImported != 2 || rep.RepairRounds != 1 || rep.Cycles != 1 ||
		rep.VisualQAItems != 1 || rep.PortalRunsJobs != 1 {
		t.Fatalf("unexpected migration counts: %+v", rep)
	}
}

func TestMainRootAndAppRootUmbrellaVsStandaloneRepos(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FOREMANKIT_APP_ROOT", "")

	// C1: standalone project at <tmp>/repos/xyz where <tmp> has no umbrella markers
	standalone := filepath.Join(tmp, "repos", "xyz")
	if err := os.MkdirAll(filepath.Join(standalone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := MainRoot(standalone); got != standalone {
		t.Fatalf("MainRoot(%q) = %q, want %q", standalone, got, standalone)
	}
	if got := AppRoot(standalone); got != standalone {
		t.Fatalf("AppRoot(%q) = %q, want %q", standalone, got, standalone)
	}

	// C1: umbrella project at <tmp>/repos/umbrella (inside a parent named repos) with .foreman-kit/pipeline.yaml
	umbrella := filepath.Join(tmp, "repos", "umbrella")
	wt := filepath.Join(umbrella, ".worktrees", "worker-x")
	if err := os.MkdirAll(filepath.Join(umbrella, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(umbrella, ".foreman-kit", "pipeline.yaml"), []byte("base_branch: main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := MainRoot(wt); got != umbrella {
		t.Fatalf("MainRoot(%q) = %q, want %q", wt, got, umbrella)
	}
}

func TestAppRootAndMainRootInWorktreeWithEnvAppRoot(t *testing.T) {
	tmp := t.TempDir()
	umbrella := filepath.Join(tmp, "umbrella")
	wt := filepath.Join(umbrella, ".worktrees", "worker-x")
	startDir := filepath.Join(wt, "repos", "backend")

	if err := os.MkdirAll(filepath.Join(umbrella, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(umbrella, ".foreman-kit", "pipeline.yaml"), []byte("base_branch: main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(startDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// C4: when FOREMANKIT_APP_ROOT=<umbrella> and startDir is <umbrella>/.worktrees/worker-x/repos/backend
	t.Setenv("FOREMANKIT_APP_ROOT", umbrella)

	if got := AppRoot(startDir); got != wt {
		t.Fatalf("AppRoot(%q) with FOREMANKIT_APP_ROOT=%q = %q, want %q", startDir, umbrella, got, wt)
	}
	if got := MainRoot(startDir); got != umbrella {
		t.Fatalf("MainRoot(%q) with FOREMANKIT_APP_ROOT=%q = %q, want %q", startDir, umbrella, got, umbrella)
	}

	// Also verify when startDir is "" and current working directory is inside the worktree
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()
	if err := os.Chdir(startDir); err != nil {
		t.Fatal(err)
	}
	if got := AppRoot(""); got != wt {
		t.Fatalf("AppRoot(\"\") from worktree cwd = %q, want %q", got, wt)
	}
}

func TestWorkspaceIDAndResolveDBPathSymlinkResolution(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	t.Setenv("FOREMANKIT_STATE_DIR", stateDir)
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", "")

	tmpWS, err := os.MkdirTemp("/tmp", "openspec-symlink-ws-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpWS) })

	if err := os.MkdirAll(filepath.Join(tmpWS, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpWS, ".foreman-kit/kit.lock"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	resolvedWS, err := filepath.EvalSymlinks(tmpWS)
	if err != nil {
		t.Fatal(err)
	}

	symlinkWS := filepath.Join(tmp, "ws-symlink")
	if err := os.Symlink(resolvedWS, symlinkWS); err != nil {
		t.Fatal(err)
	}

	idTmp, rootTmp := WorkspaceID(tmpWS)
	idResolved, rootResolved := WorkspaceID(resolvedWS)
	idSymlink, rootSymlink := WorkspaceID(symlinkWS)

	if idTmp != idResolved || idTmp != idSymlink {
		t.Fatalf("expected identical WorkspaceID across /tmp, resolved path, and symlink; got:\ntmp=%s\nresolved=%s\nsymlink=%s", idTmp, idResolved, idSymlink)
	}
	if rootTmp != rootResolved || rootTmp != rootSymlink {
		t.Fatalf("expected identical canonicalRoot across /tmp, resolved path, and symlink; got:\ntmp=%s\nresolved=%s\nsymlink=%s", rootTmp, rootResolved, rootSymlink)
	}

	dbTmp, _, _, err := ResolveDBPath(tmpWS)
	if err != nil {
		t.Fatal(err)
	}
	dbResolved, _, _, err := ResolveDBPath(resolvedWS)
	if err != nil {
		t.Fatal(err)
	}
	dbSymlink, _, _, err := ResolveDBPath(symlinkWS)
	if err != nil {
		t.Fatal(err)
	}
	if dbTmp != dbResolved || dbTmp != dbSymlink {
		t.Fatalf("expected identical ResolveDBPath across /tmp, resolved path, and symlink; got:\ntmp=%s\nresolved=%s\nsymlink=%s", dbTmp, dbResolved, dbSymlink)
	}

	// Non-existent nested path under /tmp, resolved path, and symlink must also resolve identically
	nonExistentTmp := evalSymlinksBestEffort(filepath.Join(tmpWS, "future", "dir"))
	nonExistentResolved := evalSymlinksBestEffort(filepath.Join(resolvedWS, "future", "dir"))
	nonExistentSymlink := evalSymlinksBestEffort(filepath.Join(symlinkWS, "future", "dir"))
	if nonExistentTmp != nonExistentResolved || nonExistentTmp != nonExistentSymlink {
		t.Fatalf("expected identical evalSymlinksBestEffort for non-existent child path; got:\ntmp=%s\nresolved=%s\nsymlink=%s", nonExistentTmp, nonExistentResolved, nonExistentSymlink)
	}
}

func TestIsLockStaleProcessLivenessAndVisualQATTL(t *testing.T) {
	host, _ := os.Hostname()
	now := time.Now().UTC().Unix()

	// Active process lock with expired TTL IS stale even when PID == os.Getpid()
	liveExpired := LockRecord{
		LockKey:     "cycle",
		OwnerType:   "cycle",
		PID:         os.Getpid(),
		Host:        host,
		AcquiredAt:  now - 10000,
		HeartbeatAt: now - 10000,
		TTLSeconds:  60,
	}
	if !IsLockStale(liveExpired, host, now) {
		t.Fatalf("expected IsLockStale=true when (now - heartbeatAt) > ttl even for live local PID %d", os.Getpid())
	}

	// Active process lock with fresh heartbeat and current PID is NOT stale
	liveActive := LockRecord{
		LockKey:     "cycle",
		OwnerType:   "cycle",
		PID:         os.Getpid(),
		Host:        host,
		AcquiredAt:  now,
		HeartbeatAt: now,
		TTLSeconds:  3600,
	}
	if IsLockStale(liveActive, host, now) {
		t.Fatalf("expected IsLockStale=false for live local PID %d with fresh heartbeat", os.Getpid())
	}

	// Reused PID: process started more than 5s after AcquiredAt IS stale even if TTL has not expired
	if procStart := processStartTimeUnix(os.Getpid()); procStart > 0 {
		reusedPID := LockRecord{
			LockKey:     "cycle",
			OwnerType:   "cycle",
			PID:         os.Getpid(),
			Host:        host,
			AcquiredAt:  procStart - 60,
			HeartbeatAt: now,
			TTLSeconds:  3600,
		}
		if !IsLockStale(reusedPID, host, now) {
			t.Fatalf("expected IsLockStale=true when process start time (%d) > acquiredAt+5 (%d)", procStart, reusedPID.AcquiredAt+5)
		}
	}

	// Dead local PID (999999) IS stale immediately even when TTL is in the future
	deadFuture := LockRecord{
		LockKey:     "cycle",
		OwnerType:   "cycle",
		PID:         999999,
		Host:        host,
		AcquiredAt:  now,
		HeartbeatAt: now,
		TTLSeconds:  3600,
	}
	if !IsLockStale(deadFuture, host, now) {
		t.Fatalf("expected IsLockStale=true for dead local PID 999999")
	}

	// H4: visual_qa lock with dead PID (999999) is NOT stale as long as TTL is in the future
	vqaActive := LockRecord{
		LockKey:     "visual_qa",
		OwnerType:   "visual_qa",
		PID:         999999,
		Host:        host,
		AcquiredAt:  now - 30,
		HeartbeatAt: now - 30,
		TTLSeconds:  1800,
	}
	if IsLockStale(vqaActive, host, now) {
		t.Fatalf("expected IsLockStale=false for visual_qa with dead PID while TTL is in the future")
	}

	// H4: visual_qa lock IS stale once TTL has expired
	vqaExpired := LockRecord{
		LockKey:     "visual_qa",
		OwnerType:   "visual_qa",
		PID:         999999,
		Host:        host,
		AcquiredAt:  now - 3600,
		HeartbeatAt: now - 3600,
		TTLSeconds:  1800,
	}
	if !IsLockStale(vqaExpired, host, now) {
		t.Fatalf("expected IsLockStale=true for visual_qa when TTL has expired")
	}
}

func TestStartLockHeartbeat(t *testing.T) {
	tmp := t.TempDir()
	st, err := OpenAt(filepath.Join(tmp, "hb.db"), "hb-test", tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	host, _ := os.Hostname()
	_, err = st.AcquireLock(ctx, "merger", LockOptions{
		OwnerType: "merger",
		PID:       os.Getpid(),
		Host:      host,
		TTL:       30 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Backdate heartbeat_at to verify background heartbeat refreshes it
	if _, err := st.DB().ExecContext(ctx, `UPDATE locks SET heartbeat_at = 1000 WHERE lock_key = 'merger'`); err != nil {
		t.Fatal(err)
	}

	stop := st.StartLockHeartbeat(ctx, "merger", os.Getpid(), 30*time.Millisecond)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := st.GetLock(ctx, "merger")
		if err != nil {
			t.Fatal(err)
		}
		if rec != nil && rec.HeartbeatAt > 1000 {
			stop()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected StartLockHeartbeat to refresh heartbeat_at")
}
