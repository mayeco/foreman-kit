package cycle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// With a visual QA session in progress a cycle judges in its own worktree and leaves the main
// checkout as it is, so the main checkout does not list what the cycle queued or proposed until the
// session ends. Through all of that the change stays the cycle's: on record and in no queue while the
// main checkout lags behind, never the next cycle's, and queued once the main checkout lists it. So
// does the proposal, which nobody is asked to decide before the main checkout lists it: a decision
// is written in the file of that checkout.
func TestWhatACycleQueuedStaysItsOwnWhileTheMainCheckoutLags(t *testing.T) {
	for name, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		t.Setenv(name, value)
	}
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))

	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", bare)
	root := filepath.Join(base, "root")
	gitIn(t, base, "clone", "-q", bare, root)
	settingstest.WriteFiles(t, root, map[string]string{"cycle": "proposals:\n  approved_by: human\n"})
	backlogOf := func(dir string) string { return filepath.Join(dir, filepath.FromSlash(BacklogFile)) }
	if err := os.WriteFile(backlogOf(root), []byte(queueItemsJSON(t, "queued-by-hand")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".worktrees/\nlog/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "an application with one change queued by a person")
	gitIn(t, root, "push", "-q", "origin", "HEAD:main")
	gitIn(t, root, "branch", "-q", "--set-upstream-to=origin/main")

	ctx := context.Background()
	st := openTestStore(t, root)
	// A person reviews in a browser: the session holds the main checkout as it is.
	if _, err := st.AcquireLock(ctx, "visual_qa", store.LockOptions{OwnerType: "visual_qa", PID: os.Getpid(), Stamp: "20261111T030000Z", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	// runCycle runs one cycle whose judgment queues the changes and makes the proposals named, in
	// the worktree it is given.
	runCycle := func(stamp string, queues, proposes []string) {
		t.Helper()
		var out bytes.Buffer
		err := RunCycle(ctx, root, st, CycleOptions{
			Stamp:        stamp,
			SkipMerge:    true,
			SkipDispatch: true,
			AgentExecFunc: func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, out io.Writer) error {
				if len(queues) > 0 {
					items, err := ReadJSONList(backlogOf(workdir))
					if err != nil {
						return err
					}
					for _, name := range queues {
						items = append(items, map[string]any{"change_name": name, "description": "what " + name + " is about"})
					}
					if err := WriteJSONList(backlogOf(workdir), items); err != nil {
						return err
					}
				}
				if len(proposes) > 0 {
					items, err := ReadProposals(workdir)
					if err != nil {
						return err
					}
					for _, name := range proposes {
						items = append(items, map[string]any{"change_name": name, "status": "proposed", "description": "what " + name + " would improve"})
					}
					return WriteProposals(workdir, items)
				}
				return nil
			},
		}, &out)
		if err != nil {
			t.Fatalf("RunCycle %s: %v\n%s", stamp, err, out.String())
		}
	}
	queuedBy := func(stamp string) string {
		t.Helper()
		names, err := st.ChangesQueuedByCycle(ctx, stamp)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(names, ",")
	}
	stateOf := func(change string) string {
		t.Helper()
		state, err := st.ChangeState(ctx, change)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	listedInMain := func() string {
		t.Helper()
		items, err := ReadJSONList(backlogOf(root))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(backlogNames(items), ",")
	}
	// proposedBy returns the proposals the registry says the cycle stamp made, each with its status.
	proposedBy := func(stamp string) string {
		t.Helper()
		var made []string
		err := st.View(ctx, func(v *store.View) error {
			rec, err := v.Cycle(ctx, stamp)
			if err != nil || rec == nil {
				return fmt.Errorf("the cycle %s is not on record: %v", stamp, err)
			}
			for _, p := range rec.Proposals {
				made = append(made, p.ID+" is "+p.Status)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(made, ",")
	}
	// waiting returns the proposals a person is asked to decide.
	waiting := func() string {
		t.Helper()
		var ids []string
		err := st.View(ctx, func(v *store.View) error {
			in, err := v.ListInbox(ctx)
			for _, p := range in.Proposals {
				ids = append(ids, p.ID)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(ids, ",")
	}
	proposedInMain := func() string {
		t.Helper()
		items, err := ReadProposals(root)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(backlogNames(items), ",")
	}

	const first, second, third = "20261111T040000Z", "20261111T041500Z", "20261111T043000Z"

	// The first cycle queues a change and makes a proposal. They are published, and the main
	// checkout does not list them.
	runCycle(first, []string{"found-by-the-first"}, []string{"proposed-by-the-first"})
	if got := strings.TrimSpace(gitIn(t, bare, "show", "main:"+BacklogFile)); !strings.Contains(got, "found-by-the-first") {
		t.Fatalf("the cycle did not publish what it queued; the base branch lists:\n%s", got)
	}
	if got := strings.TrimSpace(gitIn(t, bare, "show", "main:"+ProposalsFile)); !strings.Contains(got, "proposed-by-the-first") {
		t.Fatalf("the cycle did not publish what it proposed; the base branch lists:\n%s", got)
	}
	if got, proposed := listedInMain(), proposedInMain(); got != "queued-by-hand" || proposed != "" {
		t.Fatalf("the main checkout lists %s and the proposals %q during a visual QA session, want it left as it was", got, proposed)
	}
	if by, state := queuedBy(first), stateOf("found-by-the-first"); by != "found-by-the-first" || state != store.ChangeUnknown {
		t.Errorf("after the first cycle it queued %q and the change is %q, want its own and in no queue", by, state)
	}
	if state := stateOf("queued-by-hand"); state != store.ChangeQueued {
		t.Errorf("the change a person queued is %q, want queued: the main checkout lists it", state)
	}
	// The proposal is the cycle's, and nobody is asked to decide it: the file a decision is written
	// in does not list it.
	if by, asked := proposedBy(first), waiting(); by != "proposed-by-the-first is "+store.ProposalUnknown || asked != "" {
		t.Errorf("after the first cycle it proposed %q and %q waits for a decision, want its own, unknown, and nothing waiting", by, asked)
	}

	// The cycles that follow find the change and the proposal in their worktree, which comes from the
	// base branch. They are not theirs, and the change does not go in and out of the queue.
	runCycle(second, nil, nil)
	runCycle(third, []string{"found-by-the-third"}, nil)
	if by, state := queuedBy(first), stateOf("found-by-the-first"); by != "found-by-the-first" || state != store.ChangeUnknown {
		t.Errorf("two cycles later the first one queued %q and its change is %q, want its own and in no queue", by, state)
	}
	if by := queuedBy(second); by != "" {
		t.Errorf("a cycle that queued nothing has %q", by)
	}
	if by := queuedBy(third); by != "found-by-the-third" {
		t.Errorf("the third cycle queued %q, want found-by-the-third alone", by)
	}
	if by, asked := proposedBy(first), waiting(); by != "proposed-by-the-first is "+store.ProposalUnknown || asked != "" {
		t.Errorf("two cycles later the first one proposed %q and %q waits for a decision, want its own, unknown, and nothing waiting", by, asked)
	}
	if by := proposedBy(second) + proposedBy(third); by != "" {
		t.Errorf("the cycles that proposed nothing have %q", by)
	}

	// The session ends and the next cycle brings the main checkout up to date: the changes are
	// queued, each still under the cycle that queued it, and the person's under none. The proposal
	// waits for a decision, still under the cycle that made it.
	if err := st.ReleaseLock(ctx, "visual_qa", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	const last = "20261111T044500Z"
	runCycle(last, nil, nil)
	if got, proposed := listedInMain(), proposedInMain(); got != "queued-by-hand,found-by-the-first,found-by-the-third" || proposed != "proposed-by-the-first" {
		t.Fatalf("after the session the main checkout lists %s and the proposals %q", got, proposed)
	}
	for change, stamp := range map[string]string{"found-by-the-first": first, "found-by-the-third": third} {
		if by, state := queuedBy(stamp), stateOf(change); by != change || state != store.ChangeQueued {
			t.Errorf("after the session %s is %q and its cycle queued %q, want queued and its own", change, state, by)
		}
	}
	if by, asked := proposedBy(first), waiting(); by != "proposed-by-the-first is proposed" || asked != "proposed-by-the-first" {
		t.Errorf("after the session the first cycle proposed %q and %q waits for a decision, want its own, proposed, and waiting", by, asked)
	}
	if by := queuedBy(last) + proposedBy(last); by != "" {
		t.Errorf("the cycle that brought the main checkout up to date has %q, want nothing: it queued and proposed nothing", by)
	}
}
