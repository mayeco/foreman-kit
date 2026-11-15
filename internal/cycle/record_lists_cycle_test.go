package cycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// listsCycleApp is an application with a bare origin and a main checkout whose base holds the memory
// and the three JSON lists of the kit, each valid, so that a cycle runs in its own worktree and
// records to origin.
type listsCycleApp struct {
	t          *testing.T
	root, bare string
	st         *store.Store
}

func newListsCycleApp(t *testing.T, cycleYAML string) listsCycleApp {
	t.Helper()
	stopTestGit(t)
	t.Setenv(settings.RunOverridesEnv, "")
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))

	base := t.TempDir()
	a := listsCycleApp{t: t, bare: filepath.Join(base, "origin.git"), root: filepath.Join(base, "root")}
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", a.bare)
	gitIn(t, base, "clone", "-q", a.bare, a.root)
	settingstest.WriteFiles(t, a.root, map[string]string{"cycle": cycleYAML})
	if _, err := memory.Init(context.Background(), a.root, "demo"); err != nil {
		t.Fatal(err)
	}
	for rel, items := range map[string][]map[string]any{
		BacklogFile:   {{"change_name": "queued-by-hand", "description": "what queued-by-hand is about"}},
		ProposalsFile: {{"change_name": "proposed-before", "description": "an improvement", "status": "proposed"}},
		HistoryFile:   {{"change_name": "integrated-before", "description": "what integrated-before was about", "status": "integrated"}},
	} {
		if err := WriteJSONList(filepath.Join(a.root, filepath.FromSlash(rel)), items); err != nil {
			t.Fatal(err)
		}
	}
	writeRel(t, a.root, ".gitignore", ".worktrees/\nlog/\n")
	gitIn(t, a.root, "add", "-A")
	gitIn(t, a.root, "commit", "-q", "-m", "an application with one change queued by a person")
	gitIn(t, a.root, "push", "-q", "origin", "HEAD:main")
	gitIn(t, a.root, "branch", "-q", "--set-upstream-to=origin/main")
	a.st = openTestStore(t, a.root)
	return a
}

// onOrigin returns rel as origin's main holds it.
func (a listsCycleApp) onOrigin(rel string) string {
	a.t.Helper()
	return gitIn(a.t, a.bare, "show", "main:"+rel)
}

// report returns where the judgment of the cycle stamp writes its report.
func (a listsCycleApp) report(stamp string) string {
	return store.RuntimePath(a.root, "cycle", "reports", cycleReportName(stamp))
}

// run runs one cycle whose judgment calls judge in the cycle's worktree and then writes its report.
func (a listsCycleApp) run(stamp string, judge func(workdir string)) string {
	a.t.Helper()
	var out bytes.Buffer
	err := RunCycle(context.Background(), a.root, a.st, CycleOptions{
		Stamp:        stamp,
		SkipMerge:    true,
		SkipDispatch: true,
		AgentExecFunc: func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, out io.Writer) error {
			judge(workdir)
			report := a.report(stamp)
			if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
				return err
			}
			return os.WriteFile(report, []byte("# Cycle "+stamp+"\n\n## Findings\n\n- the judgment's own findings\n"), 0o644)
		},
	}, &out)
	if err != nil {
		a.t.Fatalf("RunCycle %s: %v\n%s", stamp, err, out.String())
	}
	return out.String()
}

// appendItem adds an item named name to the list rel in workdir.
func appendItem(t *testing.T, workdir, rel, name string) {
	t.Helper()
	path := filepath.Join(workdir, filepath.FromSlash(rel))
	items, err := ReadJSONList(path)
	if err != nil {
		t.Fatal(err)
	}
	items = append(items, map[string]any{"change_name": name, "description": "what " + name + " is about", "status": "proposed"})
	if err := WriteJSONList(path, items); err != nil {
		t.Fatal(err)
	}
}

// truncateMidItem adds an item named name to the list rel in workdir and cuts the file off inside it,
// as a judgment that ran out of time while it wrote the file leaves it.
func truncateMidItem(t *testing.T, workdir, rel, name string) []byte {
	t.Helper()
	appendItem(t, workdir, rel, name)
	path := filepath.Join(workdir, filepath.FromSlash(rel))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := b[:bytes.Index(b, []byte(name))+len(name)/2]
	if err := os.WriteFile(path, cut, 0o644); err != nil {
		t.Fatal(err)
	}
	return cut
}

// A judgment that leaves one of the three lists cut off mid-item: origin keeps that file as it was
// and receives the lesson in the cycle's record commit, the main checkout's backlog stays readable,
// the log names the file and its parse error, and the report says what was not recorded.
func TestRunCycleRestoresAListTheJudgmentLeftAsInvalidJSON(t *testing.T) {
	for i, rel := range []string{BacklogFile, ProposalsFile, HistoryFile} {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
			stamp := []string{"20261115T090000Z", "20261115T091500Z", "20261115T093000Z"}[i]
			before := a.onOrigin(rel)
			var cut []byte
			log := a.run(stamp, func(workdir string) {
				pitfall(t, workdir, "- **Cut-off lists**: the record keeps them out of the base (fix-cycle-records-invalid-json-lists)")
				cut = truncateMidItem(t, workdir, rel, "found-by-the-judgment")
			})

			after := a.onOrigin(rel)
			if after != before {
				t.Fatalf("origin's %s changed:\n%s\nwant what it held before the cycle:\n%s\n%s", rel, after, before, log)
			}
			if err := jsonListProblem([]byte(after)); err != nil {
				t.Fatalf("origin's %s is not a valid list: %v", rel, err)
			}
			if subject := strings.TrimSpace(gitIn(t, a.bare, "log", "-1", "--format=%s", "main")); subject != "chore(memory): scheduled cycle "+stamp {
				t.Fatalf("origin's last commit is %q, want the cycle's record commit\n%s", subject, log)
			}
			changed := gitIn(t, a.bare, "show", "--name-only", "--format=", "main")
			if !strings.Contains(changed, memory.PitfallsFile) || strings.Contains(changed, rel) {
				t.Fatalf("the record commit changes\n%s\nwant the lesson and not %s", changed, rel)
			}
			if got := a.onOrigin(memory.PitfallsFile); !strings.Contains(got, "**Cut-off lists**") {
				t.Fatalf("origin did not receive the lesson:\n%s", got)
			}

			backlog := filepath.Join(a.root, filepath.FromSlash(BacklogFile))
			if problem := config.BacklogProblem(backlog); problem != "" {
				t.Fatalf("the main checkout's backlog is unreadable after the sync: %s", problem)
			}
			if items, err := ReadJSONList(backlog); err != nil || strings.Join(backlogNames(items), ",") != "queued-by-hand" {
				t.Fatalf("the main checkout's backlog lists %v (err %v), want queued-by-hand alone", backlogNames(items), err)
			}

			kept := keptCopies(t, a.root, filepath.Base(rel))
			if len(kept) != 1 {
				t.Fatalf("want one kept copy of %s, got %v", rel, kept)
			}
			if b, err := os.ReadFile(kept[0]); err != nil || !bytes.Equal(b, cut) {
				t.Fatalf("the kept copy %s does not hold the bytes the judgment left (err %v)", kept[0], err)
			}
			line := "record: " + rel + " is not a valid JSON list (unexpected end of JSON input); not recorded: restored to the version HEAD holds, the rejected content is kept as " + kept[0] + "\n"
			if !strings.Contains(log, line) {
				t.Fatalf("expected %q in the cycle log:\n%s", line, log)
			}

			report, err := os.ReadFile(a.report(stamp))
			if err != nil {
				t.Fatal(err)
			}
			section := "\n## Not recorded\n"
			at := strings.Index(string(report), section)
			if at < 0 || !strings.HasPrefix(string(report), "# Cycle "+stamp+"\n") {
				t.Fatalf("the report does not end with a Not recorded section:\n%s", report)
			}
			tail := string(report[at:])
			if strings.Contains(tail[len(section):], "\n## ") {
				t.Fatalf("the Not recorded section is not the report's last:\n%s", report)
			}
			bullet := "- `" + rel + "`: unexpected end of JSON input; the rejected content is kept as " + kept[0] + "\n"
			if !strings.Contains(tail, bullet) {
				t.Fatalf("expected %q in the report's Not recorded section:\n%s", bullet, report)
			}
		})
	}
}

// A judgment that queues one valid item within the queue's room: origin receives it in the cycle's
// record commit, and neither the log nor the report says anything was not recorded.
func TestRunCycleRecordsAValidJSONEdit(t *testing.T) {
	a := newListsCycleApp(t, "proposals:\n  approved_by: human\n")
	const stamp = "20261115T100000Z"
	log := a.run(stamp, func(workdir string) {
		appendItem(t, workdir, BacklogFile, "found-by-the-judgment")
	})
	if got := a.onOrigin(BacklogFile); !strings.Contains(got, `"found-by-the-judgment"`) || jsonListProblem([]byte(got)) != nil {
		t.Fatalf("origin's backlog does not list what the judgment queued:\n%s\n%s", got, log)
	}
	if subject := strings.TrimSpace(gitIn(t, a.bare, "log", "-1", "--format=%s", "main")); subject != "chore(memory): scheduled cycle "+stamp {
		t.Fatalf("origin's last commit is %q, want the cycle's record commit\n%s", subject, log)
	}
	if strings.Contains(log, "is not a valid JSON list") {
		t.Fatalf("a valid backlog was rejected:\n%s", log)
	}
	if report, err := os.ReadFile(a.report(stamp)); err != nil || strings.Contains(string(report), "## Not recorded") {
		t.Fatalf("the report of a clean record says something was not recorded (err %v):\n%s", err, report)
	}
}

// With the queue full the judgment's backlog cannot be checked against the cap when it does not parse;
// once rejected, origin's backlog lists nothing the judgment added.
func TestRunCycleKeepsTheQueueCapWhenTheJudgmentLeavesInvalidJSON(t *testing.T) {
	a := newListsCycleApp(t, "queue:\n  max_open: 1\nproposals:\n  approved_by: human\n")
	const stamp = "20261115T110000Z"
	log := a.run(stamp, func(workdir string) {
		truncateMidItem(t, workdir, BacklogFile, "found-while-the-queue-is-full")
	})
	if !strings.Contains(log, "queue: 1 changes open (queue.max_open: 1)") && !strings.Contains(log, "queue: 1 change open (queue.max_open: 1)") {
		t.Fatalf("the cycle did not find the queue full:\n%s", log)
	}
	got := a.onOrigin(BacklogFile)
	items, err := parseJSONListBytes([]byte(got))
	if err != nil || jsonListProblem([]byte(got)) != nil {
		t.Fatalf("origin's backlog is not a valid list (%v):\n%s", err, got)
	}
	if names := strings.Join(backlogNames(items), ","); names != "queued-by-hand" {
		t.Fatalf("origin's backlog lists %s, want nothing the judgment added\n%s", names, log)
	}
}
