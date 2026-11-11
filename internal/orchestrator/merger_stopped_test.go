package orchestrator

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// A daemon stop cancels the integration in flight: its gates, git and push fail because they were
// cancelled, which says nothing about the change. The merger must record nothing against it: no
// block, no pause, the manifest stays in the merge queue and the attempt is closed as stopped, so the
// next merger integrates the change from the start.
func TestMergerStoppedMidIntegrationRecordsNothingAgainstTheChange(t *testing.T) {
	cases := []struct {
		name string
		// gate is what the gate runner answers once the merger was stopped.
		gate func(ctx context.Context) (GateRunResult, error)
	}{
		{"a gate that fails because it was stopped", func(context.Context) (GateRunResult, error) {
			return GateRunResult{Output: "stopped", ExitCode: 143}, nil
		}},
		{"a gate runner that returns the cancellation", func(ctx context.Context) (GateRunResult, error) {
			return GateRunResult{}, ctx.Err()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bg := context.Background()
			umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
			const change = "stopped-mid-gates"
			buildReadyManifest(t, ctx, umbrellaDir, st, change)

			gatesStarted := make(chan struct{})
			hub := NewHub()
			var log syncBuffer
			m, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Hub: hub, Out: &log,
				GateRunner: func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
					close(gatesStarted)
					<-ctx.Done()
					return tc.gate(ctx)
				}})
			if err != nil {
				t.Fatal(err)
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- m.Serve(ctx) }()
			hub.ReadyEnqueued()
			select {
			case <-gatesStarted:
			case <-time.After(20 * time.Second):
				t.Fatal("the merger never reached the gates")
			}

			cancel() // the daemon stops
			select {
			case err := <-serveDone:
				if err != nil {
					t.Fatalf("Serve returned %v after a stop", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Serve kept running after it was stopped")
			}

			if q, _ := st.GetQuarantine(bg, change); q != nil && q.Status == "blocked" {
				t.Fatalf("the change was quarantined by a merger that was only stopped: %s", q.Reason)
			}
			if hasEvent(bg, st, "change.blocked", change) {
				t.Fatal("a change.blocked event was recorded for a merger that was only stopped")
			}
			if !inReadyQueue(bg, st, change) {
				t.Fatal("the change left the merge queue")
			}
			if ws, _ := st.GetWorkspaceState(bg); ws.Paused {
				t.Fatalf("workspace paused: %s", ws.PauseReason)
			}
			attempts, err := st.ListMergeAttempts(bg, change, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(attempts) != 1 {
				t.Fatalf("expected one merge attempt, got %d: %+v", len(attempts), attempts)
			}
			if attempts[0].Result != store.MergeStopped {
				t.Fatalf("the attempt of a stopped merger was closed as %q (%s), want %q",
					attempts[0].Result, attempts[0].ResultReason, store.MergeStopped)
			}
			if strings.Contains(log.String(), "blocked (") {
				t.Fatalf("the log reports a block that does not exist:\n%s", log.String())
			}

			// The next merger integrates it from the start.
			next, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Out: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			if err := next.Run(bg); err != nil {
				t.Fatal(err)
			}
			if !integratedOnOrigin(umbrellaDir, change) {
				evts, _ := st.QueryEvents(bg, store.EventFilter{ChangeName: change})
				t.Fatalf("the next merger did not integrate the change a stop left in the queue; events: %+v", evts)
			}
		})
	}
}

// A decision the merger took is recorded whole even when the stop arrives while it is being written:
// a block is the block row, the manifest out of the queue and the attempt's result together, never
// the attempt's result alone.
func TestMergerDecisionIsRecordedWholeWhenStoppedWhileWritingIt(t *testing.T) {
	bg := context.Background()
	stoppedCtx := func() context.Context {
		ctx, cancel := context.WithCancel(bg)
		cancel()
		return ctx
	}

	t.Run("a block", func(t *testing.T) {
		umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
		const change = "blocked-while-stopping"
		buildReadyManifest(t, bg, umbrellaDir, st, change)
		m, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Out: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		manifest := readyManifestOf(t, st, change)
		if m.attemptID, err = st.BeginMergeAttempt(bg, change); err != nil {
			t.Fatal(err)
		}

		m.blockChange(store.WithMergeAttempt(stoppedCtx(), m.attemptID, change), manifest, NewBlockedError("gate_failure", "unit tests failed"))

		q, _ := st.GetQuarantine(bg, change)
		if q == nil || q.Status != "blocked" || q.Reason != "gate_failure" {
			t.Fatalf("the block was not recorded: %+v", q)
		}
		if inReadyQueue(bg, st, change) {
			t.Fatal("the blocked change stayed in the merge queue")
		}
		if !hasEvent(bg, st, "change.blocked", change) {
			t.Fatal("no change.blocked event was recorded")
		}
		attempts, _ := st.ListMergeAttempts(bg, change, 10)
		if len(attempts) != 1 || attempts[0].Result != store.MergeBlocked {
			t.Fatalf("expected the attempt closed as blocked, got %+v", attempts)
		}
	})

	t.Run("a send-back", func(t *testing.T) {
		umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
		const change = "sent-back-while-stopping"
		buildReadyManifest(t, bg, umbrellaDir, st, change)
		m, err := NewMerger(MergerOptions{Root: umbrellaDir, Store: st, Out: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		manifest := readyManifestOf(t, st, change)
		if m.attemptID, err = st.BeginMergeAttempt(bg, change); err != nil {
			t.Fatal(err)
		}

		m.sendBack(store.WithMergeAttempt(stoppedCtx(), m.attemptID, change), manifest, "rebase_conflict", "CONFLICT (content): routes.go")

		if inReadyQueue(bg, st, change) {
			t.Fatal("the change sent back stayed in the merge queue")
		}
		if state, _ := st.ChangeState(bg, change); state != "rebuild_requested" {
			t.Fatalf("expected the change to wait for its rebuild, got state %q", state)
		}
		if !hasEvent(bg, st, "merge.sent_back", change) {
			t.Fatal("no merge.sent_back event was recorded")
		}
		attempts, _ := st.ListMergeAttempts(bg, change, 10)
		if len(attempts) != 1 || attempts[0].Result != store.MergeSentBack {
			t.Fatalf("expected the attempt closed as sent back, got %+v", attempts)
		}
	})
}

func readyManifestOf(t *testing.T, st *store.Store, change string) store.ReadyManifest {
	t.Helper()
	items, err := st.ListReadyQueue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ChangeName == change {
			return it
		}
	}
	t.Fatalf("%s is not in the merge queue", change)
	return store.ReadyManifest{}
}
