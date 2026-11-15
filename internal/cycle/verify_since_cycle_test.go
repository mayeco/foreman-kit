package cycle

import (
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// A judgment that exits non-zero verified nothing: its cycle still records and ends without error,
// and its row says so instead of `done`.
func TestACycleRecordsAFailedJudgment(t *testing.T) {
	a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
	opts := func(o *CycleOptions) {
		o.AgentExecFunc = func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, w io.Writer) error {
			return errors.New("agent_exec exited 75")
		}
	}
	log, err, row := outcomeCycle(t, a, "20261115T120000Z", nil, opts)
	if err != nil {
		t.Fatalf("RunCycle: %v\n%s", err, log)
	}
	if row.Outcome != "judgment failed" {
		t.Fatalf("outcome is %q, want judgment failed\n%s", row.Outcome, log)
	}
	for _, want := range []string{"cycle: agent exited non-zero", "== record", "== done"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q\n%s", want, log)
		}
	}

	// The same cycle with a judgment that exits zero is done.
	_, err, row = outcomeCycle(t, a, "20261115T121000Z", nil, nil)
	if err != nil || row.Outcome != "done" {
		t.Fatalf("a judgment that exits zero: err %v, outcome %q, want done", err, row.Outcome)
	}
}

var verifyLineSHA = regexp.MustCompile("completed-since ([0-9a-f]{7,40})`")

// verifyCycle runs a cycle of a whose judgment is judge and returns the commit its prompt tells the
// judgment to verify since, the error RunCycle ended on and the cycle's row.
func verifyCycle(t *testing.T, a listsCycleApp, stamp string, judge func(workdir string) error) (string, error, *store.Cycle) {
	t.Helper()
	var prompt string
	var out bytesWriter
	err := RunCycle(context.Background(), a.root, a.st, CycleOptions{
		Stamp:        stamp,
		SkipMerge:    true,
		SkipDispatch: true,
		AgentExecFunc: func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, w io.Writer) error {
			raw, rerr := os.ReadFile(promptFile)
			if rerr != nil {
				t.Errorf("the prompt %s is unreadable: %v", promptFile, rerr)
			}
			prompt = string(raw)
			return judge(workdir)
		},
	}, &out)
	row, gerr := a.st.GetCycle(context.Background(), stamp)
	if gerr != nil || row == nil {
		t.Fatalf("cycle %s has no row (err %v)\n%s", stamp, gerr, out.String())
	}
	m := verifyLineSHA.FindStringSubmatch(prompt)
	if m == nil {
		t.Fatalf("the prompt of %s has no Verify line:\n%s\n%s", stamp, prompt, out.String())
	}
	return m[1], err, row
}

// bytesWriter is a log that can be read back.
type bytesWriter struct{ b []byte }

func (w *bytesWriter) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }
func (w *bytesWriter) String() string              { return string(w.b) }

// integrate lands one more change on the application's base, as the merger does.
func integrate(t *testing.T, a listsCycleApp, name string) {
	t.Helper()
	writeRel(t, a.root, name+".txt", name+"\n")
	gitIn(t, a.root, "add", "-A")
	gitIn(t, a.root, "commit", "-q", "-m", "chore(merge): integrate "+name)
	gitIn(t, a.root, "push", "-q", "origin", "HEAD:main")
}

// After a cycle whose judgment failed, and after one that broke its scope, the next cycle verifies
// since the last cycle that judged (the install commit when none did), not since the commit the
// failed one ran on: the changes integrated before it are never listed otherwise.
func TestTheNextCycleVerifiesSinceTheLastCycleThatJudged(t *testing.T) {
	a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
	writeRel(t, a.root, ".foreman-kit/kit.lock", "kit\n")
	gitIn(t, a.root, "add", "-A")
	gitIn(t, a.root, "commit", "-q", "-m", "install the kit")
	gitIn(t, a.root, "push", "-q", "origin", "HEAD:main")
	install := gitIn(t, a.root, "rev-parse", "HEAD")[:40]
	integrate(t, a, "add-feature")

	ok := func(string) error { return nil }
	fail := func(string) error { return errors.New("agent_exec exited 75") }
	outside := func(workdir string) error {
		writeRel(t, workdir, "outside-the-scope.txt", "the judgment wrote this\n")
		return nil
	}
	short := func(sha string) string { return sha[:7] }
	same := func(got, want string) bool { return len(got) >= 7 && len(want) >= len(got) && want[:len(got)] == got }

	// No cycle yet: the install commit, whatever the first cycle's judgment does.
	since, err, c1 := verifyCycle(t, a, "20261115T130000Z", fail)
	if err != nil || c1.Outcome != "judgment failed" || !same(since, install) {
		t.Fatalf("first cycle: since %s (install %s), err %v, outcome %q", since, short(install), err, c1.Outcome)
	}
	integrate(t, a, "add-second")

	// The failed cycle is not the point: the second still verifies since the install commit.
	since, err, c2 := verifyCycle(t, a, "20261115T131000Z", ok)
	if err != nil || c2.Outcome != "done" || !same(since, install) {
		t.Fatalf("after a failed judgment: since %s, want the install commit %s (err %v, outcome %q)", since, short(install), err, c2.Outcome)
	}
	if c1.UmbrellaSHA == "" || c1.UmbrellaSHA == c2.UmbrellaSHA || same(since, c1.UmbrellaSHA) {
		t.Fatalf("the failed cycle ran on %s and the next on %s: the fixture does not tell them apart", c1.UmbrellaSHA, c2.UmbrellaSHA)
	}
	integrate(t, a, "add-third")

	// A scope violation ends the cycle with nothing recorded and pauses the workspace.
	since, err, c3 := verifyCycle(t, a, "20261115T132000Z", outside)
	if !errors.Is(err, ErrScopeViolation) || c3.Outcome != "scope violation" || !same(since, c2.UmbrellaSHA) {
		t.Fatalf("scope violation cycle: since %s want %s, err %v, outcome %q", since, short(c2.UmbrellaSHA), err, c3.Outcome)
	}
	if _, err := a.st.Resume(context.Background(), "test"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := os.Remove(a.root + "/.worktrees/_cycle/outside-the-scope.txt"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	integrate(t, a, "add-fourth")

	// The cycle after the scope violation verifies since the last cycle that judged, c2.
	since, err, c4 := verifyCycle(t, a, "20261115T133000Z", ok)
	if err != nil || c4.Outcome != "done" || !same(since, c2.UmbrellaSHA) {
		t.Fatalf("after a scope violation: since %s, want %s (err %v, outcome %q)", since, short(c2.UmbrellaSHA), err, c4.Outcome)
	}
	if c3.UmbrellaSHA == c2.UmbrellaSHA || same(since, c3.UmbrellaSHA) {
		t.Fatalf("the scope violation ran on %s, as the cycle before: the fixture does not tell them apart", c3.UmbrellaSHA)
	}

	// After a cycle that judged, its own commit.
	since, err, _ = verifyCycle(t, a, "20261115T134000Z", ok)
	if err != nil || !same(since, c4.UmbrellaSHA) {
		t.Fatalf("after a done cycle: since %s, want %s (err %v)", since, short(c4.UmbrellaSHA), err)
	}
}
