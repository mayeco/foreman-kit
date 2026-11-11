package orchestrator

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// After a heartbeat gap longer than locks.stale_after (a laptop asleep), another dispatcher may
// reclaim a worker's lease (store.IsLockStale judges by the TTL first) and build the same change. The
// worker that lost its lease must stop and record nothing for the change: no the merge queue entry, no
// quarantine, and the new owner's lease row stays as it is.
func TestWorkerStopsAndRecordsNothingWhenItsLeaseIsTakenOver(t *testing.T) {
	sb := newMCFSandbox(t, "")
	const change = "lease-taken-over"
	mustWriteFile(t, filepath.Join(sb.umbrella, ".foreman-kit", "agent_backlog.json"),
		`[{"change_name": "`+change+`", "description": "Add a feature file to core", "priority": "high"}]`)

	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()

	buildStopped := false
	disp, err := NewDispatcher(DispatcherOptions{
		Root:       sb.umbrella,
		Store:      sb.st,
		MaxWorkers: 1,
		Out:        io.Discard,
		AgentRunner: func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
			chgDir := filepath.Join(req.Worktree, "openspec", "changes", req.Change)
			if req.Role == "plan" {
				mustWriteFile(t, filepath.Join(chgDir, "proposal.md"), "# Proposal\n\n## Why\nCore needs a feature file.\n\n## What Changes\n- Add feature.txt to repos/core.\n")
				mustWriteFile(t, filepath.Join(chgDir, "design.md"), "# Design\n\n## Goals\n- Only core changes.\n")
				mustWriteFile(t, filepath.Join(chgDir, "tasks.md"), "# Tasks\n\n- [ ] 1.1 Create feature.txt in repos/core\n")
				mustWriteFile(t, filepath.Join(chgDir, "specs", req.Change+"-cap", "spec.md"),
					"## Purpose\nDescribes the feature file this test change adds to the core repository and nowhere else.\n\n"+
						"## ADDED Requirements\n\n### Requirement: Core feature file\nThe core repository SHALL provide feature.txt.\n\n"+
						"#### Scenario: File present\n- **WHEN** built\n- **THEN** feature.txt exists\n")
				return AgentRunResult{ExitCode: 0, UsedEngine: "stub"}, nil
			}
			// While the build runs, another dispatcher judged the lease stale by its TTL and took it.
			now := time.Now().UTC().Unix()
			if _, err := sb.st.DB().Exec(`UPDATE locks SET pid = ?, acquired_at = ? * 1000, heartbeat_at = ? * 1000 WHERE lock_key = ?`,
				holder.Process.Pid, now, now, store.WorkerLeaseKey(req.Change)); err != nil {
				t.Error(err)
			}
			select {
			case <-ctx.Done():
				buildStopped = true
			case <-time.After(8 * time.Second):
			}
			// An agent that finishes anyway: the worker must still record nothing.
			mustWriteFile(t, filepath.Join(req.Worktree, "repos", "core", "feature.txt"), "core payload\n")
			mustWriteFile(t, filepath.Join(chgDir, "tasks.md"), "# Tasks\n\n- [x] 1.1 Create feature.txt in repos/core\n")
			return AgentRunResult{ExitCode: 0, UsedEngine: "stub"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := disp.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = disp.Close()

	ctx := context.Background()
	if !buildStopped {
		t.Error("the build agent kept running after the worker lost its lease")
	}
	if inReadyQueue(ctx, sb.st, change) {
		t.Error("a worker that lost its lease queued the change for the merger")
	}
	if q, _ := sb.st.GetQuarantine(ctx, change); q != nil {
		t.Errorf("a worker that lost its lease quarantined the change: %+v", q)
	}
	l, _ := sb.st.GetLock(ctx, store.WorkerLeaseKey(change))
	if l == nil || l.PID != holder.Process.Pid {
		t.Errorf("the new owner's lease was released or changed: %+v", l)
	}
	// The worker ends "lost", which the dispatcher keeps out of its results. (Run then tries the
	// change again in its next pass and finds the lease held: "busy".)
	var finished []any
	for _, ev := range sb.events(t, "change.finished") {
		if ev.Change == change {
			finished = append(finished, ev.Payload["result"])
		}
	}
	if len(finished) == 0 || finished[0] != "lost" {
		t.Errorf("want the worker to end \"lost\", got results %v", finished)
	}
	if res := disp.resultsCopy()[change]; res != "" && res != "busy" {
		t.Errorf("the dispatcher recorded an outcome for the change: %q", res)
	}
}
