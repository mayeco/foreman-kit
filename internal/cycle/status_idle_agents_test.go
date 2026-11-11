package cycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// TestStatusShowsAnAgentThatIsIdle: while an agent is idle, `foreman-kit status` says which one, on
// what, for how long and when the watchdog stops it. Before, a stalled agent showed only once it had
// been stopped.
func TestStatusShowsAnAgentThatIsIdle(t *testing.T) {
	root := t.TempDir()
	st, err := store.OpenAt(filepath.Join(root, "state.db"), "status-idle-test", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	status := func() string {
		t.Helper()
		var out bytes.Buffer
		if err := Status(ctx, root, st, &out); err != nil {
			t.Fatalf("Status: %v", err)
		}
		return out.String()
	}
	if got := status(); strings.Contains(got, "AGENT IDLE") {
		t.Fatalf("no agent runs, yet the status names an idle one:\n%s", got)
	}

	if err := st.CreateExecution(ctx, store.Execution{ExecID: "exec-apply", Kind: "worker", ChangeName: "add-search", Phase: "apply", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	attempt, err := st.BeginAttempt(ctx, "exec-apply", "claude", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := status(); strings.Contains(got, "AGENT IDLE") {
		t.Fatalf("a working agent is shown as idle:\n%s", got)
	}

	if err := st.BeginIdlePeriod(ctx, attempt, time.Now(), 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	got := status()
	if !strings.Contains(got, "AGENT IDLE: claude on add-search (apply): no activity for ") ||
		!strings.Contains(got, ", stopped at 20 min (limits.idle_timeout)") {
		t.Fatalf("the status does not show the idle agent:\n%s", got)
	}

	if err := st.EndIdlePeriod(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if got := status(); strings.Contains(got, "AGENT IDLE") {
		t.Fatalf("an agent that works again is still shown as idle:\n%s", got)
	}
}
