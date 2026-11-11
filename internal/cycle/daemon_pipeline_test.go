package cycle

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Under the daemon the cycle only judges and records: it neither drains the merge queue nor dispatches, and
// it wakes the continuous dispatcher when it ends.
func TestRunCycleSkipsMergeAndDispatchUnderTheDaemon(t *testing.T) {
	root := bareApp(t)
	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-continuous-cycle", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	var notified atomic.Int32
	var prompt string
	var out strings.Builder
	err = RunCycle(ctx, root, st, CycleOptions{
		SkipWorktreePrep: true,
		Stamp:            "20261109T000000Z",
		SkipMerge:        true,
		SkipDispatch:     true,
		Notify:           func() { notified.Add(1) },
		MergeFunc: func(ctx context.Context, root string, out io.Writer) error {
			t.Error("the cycle drained the merge queue although the daemon merges continuously")
			return nil
		},
		DispatchFunc: func(ctx context.Context, root string, out io.Writer) error {
			t.Error("the cycle dispatched although the daemon dispatches continuously")
			return nil
		},
		AgentExecFunc: func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, out io.Writer) error {
			b, _ := os.ReadFile(promptFile)
			prompt = string(b)
			return nil
		},
	}, &out)
	if err != nil {
		t.Fatalf("RunCycle: %v\n%s", err, out.String())
	}
	if notified.Load() != 1 {
		t.Fatalf("expected the cycle to wake the dispatcher once, got %d", notified.Load())
	}
	for _, want := range []string{
		"merge: continuous",
		"== dispatch (continuous: the daemon's dispatcher picks up what this cycle queued)",
		"== done",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("expected %q in the cycle output:\n%s", want, out.String())
		}
	}
	if !strings.Contains(prompt, "the daemon dispatches continuously") {
		t.Fatalf("expected the judgment prompt to describe continuous dispatching, got:\n%s", prompt)
	}
	// The judgment keeps no count of failed cycles: the queue's cap does the braking.
	if strings.Contains(prompt, "consecutive_failures") {
		t.Fatalf("the judgment is still told to count failed cycles:\n%s", prompt)
	}
}

func TestCyclePromptContinuousDispatchFlag(t *testing.T) {
	root := bareApp(t)
	promptPath := filepath.Join(root, "prompt.md")
	var stdout, stderr strings.Builder
	if rc := RunCyclePromptCLI(context.Background(), root, []string{promptPath, "--dispatch-continuous", "4321"}, &stdout, &stderr); rc != 0 {
		t.Fatalf("cycle-prompt failed: %s", stderr.String())
	}
	b, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "the daemon dispatches continuously (pid 4321)") {
		t.Fatalf("unexpected dispatch line:\n%s", b)
	}
}

type countingNotifier struct {
	ready, backlog atomic.Int32
}

func (n *countingNotifier) ReadyEnqueued()  { n.ready.Add(1) }
func (n *countingNotifier) BacklogChanged() { n.backlog.Add(1) }

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// The watcher wakes the dispatcher when the backlog file changes and both loops when the workspace is
// resumed outside the portal.
func TestWatchPipelineInputsSignalsBacklogEditsAndResume(t *testing.T) {
	root := bareApp(t)
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	backlog := filepath.Join(root, filepath.FromSlash(BacklogFile))
	if err := os.WriteFile(backlog, []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-watch", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := &countingNotifier{}
	done := make(chan error, 1)
	go func() { done <- watchPipelineInputs(ctx, root, st, 20*time.Millisecond, n) }()

	time.Sleep(80 * time.Millisecond)
	if n.backlog.Load() != 0 || n.ready.Load() != 0 {
		t.Fatalf("expected no wake-up while nothing changes, got backlog=%d ready=%d", n.backlog.Load(), n.ready.Load())
	}

	// An atomic replacement (what WriteJSONList does) of the same size still counts as a change.
	tmp := backlog + ".tmp"
	if err := os.WriteFile(tmp, []byte("[ ]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, backlog); err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return n.backlog.Load() >= 1 }) {
		t.Fatal("expected a backlog edit to wake the dispatcher")
	}

	if err := st.SetPaused(ctx, true, "operator"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	before := n.ready.Load()
	if err := st.SetPaused(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return n.ready.Load() > before }) {
		t.Fatal("expected a resume to wake the merger")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned %v", err)
	}
}
