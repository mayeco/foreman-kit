package cycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// The script finds the report of a cycle under the cycle's own name, and otherwise the newest one
// written since the cycle began: never an older report, and never a visual session's.
func TestFindCycleReport(t *testing.T) {
	dir := t.TempDir()
	const stamp = "20261110T232841Z"
	began := time.Now()
	write := func(name string, at time.Time) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("# report\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}

	write("20261110-2229.md", began.Add(-time.Hour))
	write("visual-20261110T233000Z.md", began.Add(2*time.Minute))
	if got := findCycleReport(dir, stamp, began); got != "" {
		t.Fatalf("no report was written in this cycle, got %s", got)
	}
	byMinute := write("20261110-2329.md", began.Add(time.Minute))
	if got := findCycleReport(dir, stamp, began); got != byMinute {
		t.Fatalf("a report named by the minute, written in this cycle: got %q, want %q", got, byMinute)
	}
	named := write(cycleReportName(stamp), began.Add(30*time.Second))
	if got := findCycleReport(dir, stamp, began); got != named {
		t.Fatalf("the report named after the cycle wins: got %q, want %q", got, named)
	}
}

// The judgment is told the name of its report only inside a cycle.
func TestCyclePromptNamesTheReport(t *testing.T) {
	root := bareApp(t)
	strategy := &ExplorationStrategy{Slug: "performance", Name: "Performance", Kind: "improvements", Directive: "Measure."}
	in, err := renderCyclePrompt(root, strategy, nil, cyclePlan{QueueOpen: -1, Stamp: "20261110T232841Z"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(in, "- **Report**: write it as `log/cycle/reports/20261110T232841Z.md`") {
		t.Fatalf("the prompt of a cycle does not name its report:\n%s", in[len(in)-1500:])
	}
	out, err := renderCyclePrompt(root, strategy, nil, cyclePlan{QueueOpen: -1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "**Report**") {
		t.Fatal("a prompt rendered outside a cycle names a report")
	}
}

// A cycle says in the registry what it queued and proposed: what its judgment added, and not what the
// backlog and the proposals already listed. The queue is then what the main checkout lists.
func TestRunCycleSaysWhatItQueued(t *testing.T) {
	a := newQueueApp(t, "proposals:\n  approved_by: human\n")
	a.queue("queued-by-hand")
	const before = `{"change_name": "proposed-before", "description": "made before this cycle", "status": "proposed"}`
	a.write(ProposalsFile, "["+before+"]")
	ctx := context.Background()
	st := a.store()

	const stamp = "20261111T040000Z"
	runQueueCycle(t, a, st, stamp, func(string) {
		a.queue("queued-by-hand", "found-now")
		a.write(ProposalsFile, "["+before+`, {"change_name": "proposed-now", "description": "an improvement", "status": "proposed"}]`)
	})

	queued, err := st.ChangesQueuedByCycle(ctx, stamp)
	if err != nil || strings.Join(queued, ",") != "found-now" {
		t.Errorf("the cycle queued %v (%v), want found-now alone", queued, err)
	}
	for _, name := range []string{"queued-by-hand", "found-now"} {
		if state, err := st.ChangeState(ctx, name); err != nil || state != store.ChangeQueued {
			t.Errorf("%s is %q (%v) after the cycle, want queued: the main checkout lists it", name, state, err)
		}
	}
	var proposed []string
	err = st.View(ctx, func(v *store.View) error {
		rec, err := v.Cycle(ctx, stamp)
		if err != nil || rec == nil {
			return fmt.Errorf("the cycle is not on record: %v", err)
		}
		for _, p := range rec.Proposals {
			proposed = append(proposed, p.ID+" is "+p.Status)
		}
		return nil
	})
	if err != nil || strings.Join(proposed, ",") != "proposed-now is proposed" {
		t.Errorf("the cycle made the proposals %v (%v), want proposed-now alone, proposed: the main checkout lists it", proposed, err)
	}

	// A cycle whose judgment adds nothing says nothing, and takes nothing from the one before.
	const next = "20261111T041500Z"
	runQueueCycle(t, a, st, next, func(string) {})
	if queued, err := st.ChangesQueuedByCycle(ctx, next); err != nil || len(queued) != 0 {
		t.Errorf("a cycle that queued nothing has %v (%v)", queued, err)
	}
	if queued, _ := st.ChangesQueuedByCycle(ctx, stamp); strings.Join(queued, ",") != "found-now" {
		t.Errorf("after the next cycle the first one queued %v, want found-now still", queued)
	}
}
