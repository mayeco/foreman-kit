package cycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
	"github.com/mayeco/foreman-kit/internal/store"
)

// queueApp is an application directory of a queue test: its settings, its backlog and its proposals.
type queueApp struct {
	t    *testing.T
	root string
}

func newQueueApp(t *testing.T, cycleYAML string) queueApp {
	t.Helper()
	a := queueApp{t: t, root: t.TempDir()}
	// A cycle does not run on files that lack a key: the application has the seven the kit ships,
	// with what the test wants different in .foreman-kit/cycle.yaml.
	settingstest.WriteFiles(t, a.root, map[string]string{"cycle": cycleYAML})
	return a
}

// config writes the configuration file called name as the kit ships it, with the keys of override
// in place of the shipped values.
func (a queueApp) config(name, override string) {
	a.t.Helper()
	settingstest.WriteFile(a.t, a.root, name, override)
}

func (a queueApp) write(rel, content string) {
	a.t.Helper()
	path := filepath.Join(a.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		a.t.Fatal(err)
	}
}

// queue writes a backlog that lists names, each with the priority after its colon ("fix-a:critical").
func (a queueApp) queue(names ...string) {
	a.t.Helper()
	a.write(BacklogFile, queueItemsJSON(a.t, names...))
}

func queueItemsJSON(t *testing.T, names ...string) string {
	t.Helper()
	items := []map[string]any{}
	for _, n := range names {
		name, priority, _ := strings.Cut(n, ":")
		item := map[string]any{"change_name": name, "description": "what " + name + " is about"}
		if priority != "" {
			item["priority"] = priority
		}
		items = append(items, item)
	}
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (a queueApp) names(rel string) []string {
	a.t.Helper()
	items, err := ReadJSONList(filepath.Join(a.root, filepath.FromSlash(rel)))
	if err != nil {
		a.t.Fatal(err)
	}
	return backlogNames(items)
}

func (a queueApp) store() *store.Store {
	a.t.Helper()
	st, err := store.OpenAt(filepath.Join(a.t.TempDir(), "openspec.db"), "ws-queue", a.root)
	if err != nil {
		a.t.Fatal(err)
	}
	a.t.Cleanup(func() { _ = st.Close() })
	return st
}

// raiseQueueCap gives the application at root, installed from the seeds, a queue that takes a hundred
// changes: for a test that queues many and is not about the cap.
func raiseQueueCap(t *testing.T, root string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(settings.QueueMaxOpen.FilePath()))
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	const seedLine = "  max_open: 8\n"
	if strings.Count(string(raw), seedLine) != 1 {
		t.Fatalf("the seed of %s does not write %q once", settings.QueueMaxOpen.FilePath(), seedLine)
	}
	if err := os.WriteFile(file, []byte(strings.Replace(string(raw), seedLine, "  max_open: 100\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func eventsNamed(t *testing.T, st *store.Store, event string) []store.EventRecord {
	t.Helper()
	events, err := st.QueryEvents(context.Background(), store.EventFilter{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// A cycle may queue its findings while they fit under the cap, fewer when fewer fit, and none when the
// queue is full, however far over the cap it is.
func TestQuotaIsWhatStillFitsUnderTheCap(t *testing.T) {
	for _, tc := range []struct {
		open, maxOpen, maxFindings int
		wantFindings, wantRoom     int
		wantFull                   bool
	}{
		{open: 0, maxOpen: 8, maxFindings: 3, wantFindings: 3, wantRoom: 8},
		{open: 5, maxOpen: 8, maxFindings: 3, wantFindings: 3, wantRoom: 3},
		{open: 6, maxOpen: 8, maxFindings: 3, wantFindings: 2, wantRoom: 2},
		{open: 7, maxOpen: 8, maxFindings: 3, wantFindings: 1, wantRoom: 1},
		{open: 8, maxOpen: 8, maxFindings: 3, wantFindings: 0, wantRoom: 0, wantFull: true},
		{open: 13, maxOpen: 8, maxFindings: 3, wantFindings: 0, wantRoom: 0, wantFull: true},
		{open: 0, maxOpen: 2, maxFindings: 3, wantFindings: 2, wantRoom: 2},
	} {
		r := settings.At(t.TempDir()).Use(settings.QueueMaxOpen.File(), map[string]any{
			"queue": map[string]any{"max_open": tc.maxOpen, "max_findings": tc.maxFindings},
		})
		q := quotaFor(r, tc.open)
		if !q.Counted || q.Findings != tc.wantFindings || q.Room() != tc.wantRoom || q.Full() != tc.wantFull {
			t.Errorf("%d open of %d, %d findings a cycle: got findings=%d room=%d full=%v counted=%v, want findings=%d room=%d full=%v",
				tc.open, tc.maxOpen, tc.maxFindings, q.Findings, q.Room(), q.Full(), q.Counted, tc.wantFindings, tc.wantRoom, tc.wantFull)
		}
	}

	// Nobody counted: the cycle is held to its findings alone, and the prompt says nothing of the queue.
	q := quotaFor(settings.At(t.TempDir()), -1)
	if q.Counted || q.Findings != 3 || q.MaxOpen != 8 || q.promptLine() != "" {
		t.Errorf("without a count: %+v, prompt line %q", q, q.promptLine())
	}
}

// What the judgment queued over its number leaves the backlog: the lowest priorities first and, among
// equals, the last ones listed. What the backlog already listed stays, whatever its priority.
func TestLimitQueuedKeepsTheMostUrgentFindings(t *testing.T) {
	a := newQueueApp(t, "")
	a.queue("old-low:low", "new-normal", "new-low:low", "new-critical:critical", "old-normal", "new-high:high", "new-unknown:urgent", "new-normal")
	before := map[string]bool{"old-low": true, "old-normal": true}

	kept, removed, err := limitQueued(a.root, before, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(backlogNames(kept), ","); got != "new-critical,new-high" {
		t.Errorf("kept %s, want the critical and the high one", got)
	}
	// new-normal is listed twice: both of its items go.
	if got := strings.Join(backlogNames(removed), ","); got != "new-normal,new-low,new-unknown" || len(removed) != 4 {
		t.Errorf("took out %s (%d items), want new-normal twice, new-low and new-unknown", got, len(removed))
	}
	if got := strings.Join(a.names(BacklogFile), ","); got != "old-low,new-critical,old-normal,new-high" {
		t.Errorf("the backlog lists %s, want what was there and the two kept, in their order", got)
	}

	// Among equals the first listed stay, and a priority the kit does not know is normal.
	a.queue("first", "second:urgent", "third:normal", "fourth:low")
	if kept, removed, err = limitQueued(a.root, nil, 2); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(backlogNames(kept), ",") + " / " + strings.Join(backlogNames(removed), ","); got != "first,second / third,fourth" {
		t.Errorf("kept / took out: %s, want first,second / third,fourth", got)
	}

	// A full queue takes nothing new, and leaves what was there.
	a.queue("was-there", "found-now:critical")
	if kept, removed, err = limitQueued(a.root, map[string]bool{"was-there": true}, 0); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 0 || strings.Join(backlogNames(removed), ",") != "found-now" || strings.Join(a.names(BacklogFile), ",") != "was-there" {
		t.Errorf("with no room: kept %v, took out %v, backlog %v", backlogNames(kept), backlogNames(removed), a.names(BacklogFile))
	}
}

// A backlog within the number is not rewritten, and an item without a usable name is neither counted
// nor taken out: it is no change, and config check reports it.
func TestLimitQueuedLeavesABacklogWithinItsNumberAsItIs(t *testing.T) {
	a := newQueueApp(t, "")
	raw := `[{"change_name":"was-there"},
 {"change_name":"Not A Name","description":"the dispatcher skips it"},
 {"change_name":"found-one"}, {"change_name":"found-two"}]`
	a.write(BacklogFile, raw)
	kept, removed, err := limitQueued(a.root, map[string]bool{"was-there": true}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 || len(removed) != 0 {
		t.Fatalf("kept %d and took out %d, want 2 and 0", len(kept), len(removed))
	}
	if got, _ := os.ReadFile(filepath.Join(a.root, filepath.FromSlash(BacklogFile))); string(got) != raw {
		t.Errorf("the backlog was rewritten although nothing left it:\n%s", got)
	}

	if _, removed, err = limitQueued(a.root, map[string]bool{"was-there": true}, 1); err != nil {
		t.Fatal(err)
	}
	items, _ := ReadJSONList(filepath.Join(a.root, filepath.FromSlash(BacklogFile)))
	if strings.Join(backlogNames(removed), ",") != "found-two" || len(items) != 3 {
		t.Errorf("took out %v and left %d items, want found-two and 3 (the unnamed item stays)", backlogNames(removed), len(items))
	}

	a.write(BacklogFile, `{"not": "a list"}`)
	if _, _, err = limitQueued(a.root, nil, 1); err == nil {
		t.Error("a backlog that is no list was read as one")
	}
}

// "This cycle" tells the judgment how full the queue is and how many findings it may record.
func TestCyclePromptSaysWhatTheQueueTakes(t *testing.T) {
	a := newQueueApp(t, "queue:\n  max_open: 8\nroadmap:\n  file: ROADMAP.md\n")
	a.write("ROADMAP.md", "# Roadmap\n\n1. A feature\n")
	render := func(open int) string {
		t.Helper()
		text, err := renderCyclePrompt(a.root, &ExplorationStrategy{Slug: "regressions", Name: "Regressions", Kind: "defects", Directive: "Look for regressions."}, nil, cyclePlan{QueueOpen: open})
		if err != nil {
			t.Fatal(err)
		}
		_, plan, found := strings.Cut(text, "## This cycle")
		if !found {
			t.Fatalf("the prompt has no \"This cycle\":\n%s", text)
		}
		return plan
	}
	has := func(plan string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(plan, w) {
				t.Errorf("expected %q in:\n%s", w, plan)
			}
		}
	}
	lacks := func(plan string, not ...string) {
		t.Helper()
		for _, n := range not {
			if strings.Contains(plan, n) {
				t.Errorf("did not expect %q in:\n%s", n, plan)
			}
		}
	}

	room := render(3)
	has(room, "at most 3 this cycle. Defects found outside this strategy follow the same rule",
		"- **Queue**: 3 changes are open (queued, building, ready, merging, blocked or waiting to be rebuilt) and `queue.max_open` in `.foreman-kit/cycle.yaml` is 8: there is room for this cycle's findings.",
		"`ROADMAP.md`: propose the next features")

	tight := render(7)
	has(tight, "at most 1 this cycle. Defects found outside",
		"- **Queue**: 7 changes are open", "it takes 1 more, so this cycle records at most 1 finding instead of 3",
		"an approved proposal waits for room", "`ROADMAP.md`: propose the next features")

	full := render(13)
	has(full, "- **Its findings**: none this cycle: the queue is full (see \"Queue\").\n",
		"- **Queue**: full. 13 changes are open", "record no finding this cycle, neither a backlog item nor a proposal, the roadmap included",
		"- **Roadmap**: none this cycle: the queue is full")
	lacks(full, "at most", "always become proposals", "propose the next features")

	one := render(1)
	has(one, "- **Queue**: 1 change is open (")

	// Rendered by hand, without a count: as before the queue had a cap.
	plain := render(-1)
	has(plain, "at most 3 this cycle. Defects found outside")
	lacks(plain, "**Queue**")

	// The command takes the count the cycle made.
	promptPath := filepath.Join(a.root, "prompt.md")
	var stdout, stderr bytes.Buffer
	if rc := RunCyclePromptCLI(context.Background(), a.root, []string{promptPath, "--queue-open", "8"}, &stdout, &stderr); rc != 0 {
		t.Fatalf("cycle-prompt --queue-open 8 failed: %s", stderr.String())
	}
	if b, _ := os.ReadFile(promptPath); !strings.Contains(string(b), "- **Queue**: full. 8 changes are open") {
		t.Errorf("cycle-prompt --queue-open 8 does not say the queue is full:\n%s", b)
	}
	if rc := RunCyclePromptCLI(context.Background(), a.root, []string{promptPath, "--queue-open=many"}, &stdout, &stderr); rc != 2 ||
		!strings.Contains(stderr.String(), "--queue-open takes the number of open changes") {
		t.Errorf("cycle-prompt --queue-open=many: rc=%d, stderr %q", rc, stderr.String())
	}
}

// An approved proposal is queued only while the queue has room. One that does not fit stays approved,
// and a later review queues it once a change has left the queue.
func TestReviewProposalsQueuesOnlyWhileTheQueueHasRoom(t *testing.T) {
	a := newQueueApp(t, "queue:\n  max_open: 3\nproposals:\n  approved_by: human\n")
	a.queue("queued-one", "queued-two")
	a.write(ProposalsFile, `[
		{"change_name": "approved-first", "description": "first", "status": "approved"},
		{"change_name": "already-queued", "description": "its item is in the backlog", "status": "approved"},
		{"change_name": "approved-second", "description": "second", "status": "approved"},
		{"change_name": "still-proposed", "description": "waits for a person", "status": "proposed"}
	]`)
	a.queue("queued-one", "queued-two", "already-queued")
	ctx := context.Background()
	st := a.store()
	status := func() string {
		t.Helper()
		items, err := ReadProposals(a.root)
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, it := range items {
			parts = append(parts, asString(it["change_name"])+"="+asString(it["status"]))
		}
		return strings.Join(parts, " ")
	}

	// Three changes are open of three: nothing enters. A proposal whose item is already in the
	// backlog takes no room, so it is marked queued.
	var out bytes.Buffer
	if err := ReviewProposals(ctx, a.root, st, nil, &out); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != "approved-first=approved already-queued=queued approved-second=approved still-proposed=proposed" {
		t.Fatalf("with a full queue: %s\n%s", got, out.String())
	}
	if got := strings.Join(a.names(BacklogFile), ","); got != "queued-one,queued-two,already-queued" {
		t.Fatalf("the backlog grew although the queue was full: %s", got)
	}
	for _, want := range []string{
		"proposals: 2 waiting for room in the queue, 1 queued, 1 waiting for a human (approval: human, prompt edits: human)",
		"queue: 3 changes open (queue.max_open: 3): approved and waiting for room: approved-first, approved-second",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("expected %q in:\n%s", want, out.String())
		}
	}
	if n := len(eventsNamed(t, st, "proposal.queued")); n != 1 {
		t.Fatalf("%d proposals were announced as queued, want the one that was", n)
	}

	// A change is integrated: there is room for one, and the first approved takes it.
	if err := st.MarkChangeIntegrated(ctx, "queued-one", "abc123"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := ReviewProposals(ctx, a.root, st, nil, &out); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != "approved-first=queued already-queued=queued approved-second=approved still-proposed=proposed" {
		t.Fatalf("with room for one: %s\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "approved and waiting for room: approved-second") {
		t.Fatalf("the review does not say which proposal still waits:\n%s", out.String())
	}

	// A change that is blocked is open although the backlog no longer lists it: still no room.
	if err := st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "blocked-elsewhere", Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkChangeIntegrated(ctx, "queued-two", "abc124"); err != nil {
		t.Fatal(err)
	}
	if err := ReviewProposals(ctx, a.root, st, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := status(); !strings.Contains(got, "approved-second=approved") {
		t.Fatalf("a blocked change did not count as open: %s", got)
	}

	// The cap is raised: the proposal that waited is queued.
	a.config("cycle", "queue:\n  max_open: 10\nproposals:\n  approved_by: human\n")
	if err := ReviewProposals(ctx, a.root, st, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := status(); !strings.Contains(got, "approved-second=queued") {
		t.Fatalf("the proposal that waited was not queued once there was room: %s", got)
	}
}

// `foreman-kit status` and `foreman-kit progress` say how full the queue is and which approved
// proposals wait, so that a cycle that queues nothing is no mystery.
func TestStatusAndProgressSayHowFullTheQueueIs(t *testing.T) {
	a := newQueueApp(t, "queue:\n  max_open: 3\n")
	a.queue("queued-one", "queued-two")
	a.write(ProposalsFile, `[
		{"change_name": "approved-and-waiting", "description": "waits for room", "status": "approved"},
		{"change_name": "prompt-plan-shorter", "kind": "prompt", "prompt": "plan", "status": "approved"},
		{"change_name": "still-proposed", "description": "waits for a person", "status": "proposed"}
	]`)
	ctx := context.Background()
	st := a.store()
	report := func() (status, progress string, pending map[string]any) {
		t.Helper()
		var statusOut, textOut, jsonOut, errOut bytes.Buffer
		if err := Status(ctx, a.root, st, &statusOut); err != nil {
			t.Fatal(err)
		}
		if rc := RunProgressCLI(ctx, a.root, st, []string{"--since", "24h"}, &textOut, &errOut); rc != 0 {
			t.Fatalf("progress failed: %s", errOut.String())
		}
		if rc := RunProgressCLI(ctx, a.root, st, []string{"--since", "24h", "--json"}, &jsonOut, &errOut); rc != 0 {
			t.Fatalf("progress --json failed: %s", errOut.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(jsonOut.Bytes(), &doc); err != nil {
			t.Fatalf("progress --json is not JSON: %v", err)
		}
		pending, _ = doc["pending"].(map[string]any)
		return statusOut.String(), textOut.String(), pending
	}

	status, progress, pending := report()
	if !strings.Contains(status, "open_changes: 2 (queue.max_open: 3)\n") || strings.Contains(status, "QUEUE FULL") {
		t.Errorf("status with room:\n%s", status)
	}
	if !strings.Contains(status, "proposals approved, queued by the next cycle while the queue has room: approved-and-waiting\n") {
		t.Errorf("status does not list the approved proposal, or lists the prompt edit with it:\n%s", status)
	}
	if !strings.Contains(progress, "  open changes: 2 (queue.max_open: 3)\n") || strings.Contains(progress, "the queue is full") {
		t.Errorf("progress with room:\n%s", progress)
	}
	if got := asJSONText(t, pending["queue"]); got != `{"full":false,"max_open":3,"open":2,"room":1}` {
		t.Errorf("progress --json pending.queue = %s", got)
	}
	if got := asJSONText(t, pending["proposals_approved"]); got != `["approved-and-waiting"]` {
		t.Errorf("progress --json pending.proposals_approved = %s", got)
	}

	// A change that is blocked fills the queue although the backlog does not list it.
	if err := st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "blocked-elsewhere", Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	status, progress, pending = report()
	if !strings.Contains(status, "QUEUE FULL: 3 changes open (queue.max_open: 3); the cycle queues nothing new and approved proposals wait until a change is integrated or abandoned\n") {
		t.Errorf("status with a full queue:\n%s", status)
	}
	if !strings.Contains(progress, "  open changes: 3 (queue.max_open: 3): the queue is full, so the cycle queues nothing new\n") ||
		!strings.Contains(progress, "  proposals approved, queued by the next cycle while the queue has room: approved-and-waiting\n") {
		t.Errorf("progress with a full queue:\n%s", progress)
	}
	if got := asJSONText(t, pending["queue"]); got != `{"full":true,"max_open":3,"open":3,"room":0}` {
		t.Errorf("progress --json pending.queue with a full queue = %s", got)
	}
}

func asJSONText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Without a store only the backlog is known, and it still counts.
func TestReviewProposalsCountsTheBacklogWithoutAStore(t *testing.T) {
	a := newQueueApp(t, "queue:\n  max_open: 2\nproposals:\n  approved_by: nobody\n")
	a.queue("queued-one")
	a.write(ProposalsFile, `[
		{"change_name": "proposed-first", "description": "first", "status": "proposed"},
		{"change_name": "proposed-second", "description": "second", "status": "proposed"}
	]`)
	var out bytes.Buffer
	if err := ReviewProposals(context.Background(), a.root, nil, nil, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(a.names(BacklogFile), ","); got != "queued-one,proposed-first" {
		t.Fatalf("the backlog lists %s, want the one proposal that fits\n%s", got, out.String())
	}
	items, _ := ReadProposals(a.root)
	if asString(items[1]["status"]) != "approved" {
		t.Fatalf("the proposal that did not fit is %q, want approved", asString(items[1]["status"]))
	}
}

// runQueueCycle runs one cycle in a, with a judgment that edits the application's files.
func runQueueCycle(t *testing.T, a queueApp, st *store.Store, stamp string, judgment func(prompt string)) string {
	t.Helper()
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	var out bytes.Buffer
	err := RunCycle(context.Background(), a.root, st, CycleOptions{
		SkipWorktreePrep: true,
		Stamp:            stamp,
		SkipMerge:        true,
		SkipDispatch:     true,
		AgentExecFunc: func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, out io.Writer) error {
			b, err := os.ReadFile(promptFile)
			if err != nil {
				return err
			}
			judgment(string(b))
			return nil
		},
	}, &out)
	if err != nil {
		t.Fatalf("RunCycle: %v\n%s", err, out.String())
	}
	return out.String()
}

// A cycle counts the open changes before its judgment, tells the judgment what still fits, and takes
// out of the backlog what the judgment queued over that. What it took out is kept beside its log.
func TestRunCycleHoldsTheJudgmentToWhatTheQueueTakes(t *testing.T) {
	a := newQueueApp(t, "queue:\n  max_open: 5\nproposals:\n  approved_by: human\n")
	a.queue("queued-one", "queued-two")
	a.write(ProposalsFile, `[{"change_name": "approved-before", "description": "approved by a person", "status": "approved"}]`)
	ctx := context.Background()
	st := a.store()
	// Two more are open that the backlog does not list: one blocked, one being built.
	if err := st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "blocked-one", Reason: "gate_failure"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginChangeRun(ctx, "building-one", 1, "/wt/building-one", "feat/spec-building-one"); err != nil {
		t.Fatal(err)
	}

	const stamp = "20261110T060000Z"
	var prompt string
	log := runQueueCycle(t, a, st, stamp, func(p string) {
		prompt = p
		// The judgment ignores what it was told: three findings and a proposal.
		a.queue("queued-one", "queued-two", "found-normal", "found-critical:critical", "found-low:low")
		a.write(ProposalsFile, `[
			{"change_name": "approved-before", "description": "approved by a person", "status": "approved"},
			{"change_name": "proposed-now", "description": "an improvement", "status": "proposed"}
		]`)
	})

	// Four open of five: one finding fits.
	for _, want := range []string{
		"queue: 4 changes open (queue.max_open: 5): this cycle may queue 1 finding\n",
		"queue: the judgment queued 3 changes and this cycle may queue 1 (4 changes open (queue.max_open: 5)): found-normal, found-low taken out of the backlog; they are kept in ",
		"proposals: 1 waiting for room in the queue, 1 waiting for a human",
		"queue: 5 changes open (queue.max_open: 5): approved and waiting for room: approved-before",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("expected %q in the cycle log:\n%s", want, log)
		}
	}
	if !strings.Contains(prompt, "it takes 1 more, so this cycle records at most 1 finding instead of 3") || !strings.Contains(prompt, "at most 1 this cycle") {
		t.Fatalf("the judgment was not told what the queue takes:\n%s", prompt)
	}
	if got := strings.Join(a.names(BacklogFile), ","); got != "queued-one,queued-two,found-critical" {
		t.Fatalf("the backlog lists %s, want what was there and the critical finding", got)
	}

	saved := store.RuntimePath(a.root, "cycle", "not-queued-"+stamp+".json")
	kept, err := ReadJSONList(saved)
	if err != nil || strings.Join(backlogNames(kept), ",") != "found-normal,found-low" || asString(kept[0]["description"]) != "what found-normal is about" {
		t.Fatalf("what the cycle took out is not kept whole in %s: %v (err %v)", saved, kept, err)
	}

	limited := eventsNamed(t, st, "cycle.queue_limited")
	if len(limited) != 1 || fmt.Sprint(limited[0].Payload["open"]) != "4" || fmt.Sprint(limited[0].Payload["max_open"]) != "5" || fmt.Sprint(limited[0].Payload["findings"]) != "1" {
		t.Fatalf("the cycle did not announce that the queue limited it: %+v", limited)
	}
	notQueued := eventsNamed(t, st, "cycle.not_queued")
	if len(notQueued) != 1 || fmt.Sprint(notQueued[0].Payload["changes"]) != "[found-normal found-low]" || notQueued[0].Change != "" {
		t.Fatalf("the cycle did not announce what it took out, or named a change that never was one: %+v", notQueued)
	}
	// Taking an item out does not register it: the kit never met those changes.
	for _, name := range []string{"found-normal", "found-low"} {
		if state, _ := st.ChangeState(ctx, name); state != "" {
			t.Fatalf("%s was registered as %q although it never entered the queue", name, state)
		}
	}
	cycle, err := st.GetCycle(ctx, stamp)
	if err != nil || cycle == nil || cycle.FindingsCount != 2 {
		t.Fatalf("the cycle's row does not count its one finding and its one proposal: %+v (err %v)", cycle, err)
	}
}

// With the queue full the cycle still runs: it verifies and reports, and queues nothing.
func TestRunCycleQueuesNothingWhileTheQueueIsFull(t *testing.T) {
	a := newQueueApp(t, "queue:\n  max_open: 2\nproposals:\n  approved_by: nobody\n")
	a.queue("queued-one", "queued-two")
	st := a.store()

	const stamp = "20261110T070000Z"
	var prompt string
	log := runQueueCycle(t, a, st, stamp, func(p string) {
		prompt = p
		a.queue("queued-one", "queued-two", "found-anyway:critical")
		a.write(ProposalsFile, `[{"change_name": "proposed-anyway", "description": "an improvement", "status": "proposed"}]`)
	})

	for _, want := range []string{
		"queue: 2 changes open (queue.max_open: 2): this cycle queues nothing new; it verifies and reports\n",
		"found-anyway taken out of the backlog",
		"approved and waiting for room: proposed-anyway",
		"== done",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("expected %q in the cycle log:\n%s", want, log)
		}
	}
	if !strings.Contains(prompt, "- **Queue**: full. 2 changes are open") || !strings.Contains(prompt, "- **Its findings**: none this cycle") {
		t.Fatalf("the judgment was not told the queue is full:\n%s", prompt)
	}
	if got := strings.Join(a.names(BacklogFile), ","); got != "queued-one,queued-two" {
		t.Fatalf("the backlog lists %s although the queue was full", got)
	}
	items, _ := ReadProposals(a.root)
	if len(items) != 1 || asString(items[0]["status"]) != "approved" {
		t.Fatalf("the proposal of a full queue must stay approved, waiting for room: %v", items)
	}

	// A cycle with room and a judgment within its number changes nothing and announces nothing.
	b := newQueueApp(t, "")
	b.queue("queued-one")
	stB := b.store()
	log = runQueueCycle(t, b, stB, "20261110T080000Z", func(string) { b.queue("queued-one", "found-one", "found-two") })
	if !strings.Contains(log, "queue: 1 change open (queue.max_open: 8): this cycle may queue 3 findings\n") || strings.Contains(log, "taken out of the backlog") {
		t.Fatalf("a cycle with room:\n%s", log)
	}
	if got := strings.Join(b.names(BacklogFile), ","); got != "queued-one,found-one,found-two" {
		t.Fatalf("the backlog lists %s, want the two findings kept", got)
	}
	if len(eventsNamed(t, stB, "cycle.queue_limited"))+len(eventsNamed(t, stB, "cycle.not_queued")) != 0 {
		t.Fatal("a cycle the queue did not limit announced that it did")
	}
	if _, err := os.Stat(store.RuntimePath(b.root, "cycle", "not-queued-20261110T080000Z.json")); err == nil {
		t.Fatal("a cycle that took nothing out left a file of what it took out")
	}
}

// While as many changes are open as queue.max_open allows, the daemon does not start a cycle before
// its tick: that cycle would queue nothing, so the free workers gain nothing from it. It starts one
// once a change has left the queue.
func TestDaemonDoesNotStartACycleEarlyWhileTheQueueIsFull(t *testing.T) {
	l := startOnDemandLoop(t, 200*time.Millisecond, time.Hour, true, func(l *onDemandLoop) {
		a := queueApp{t: t, root: l.root}
		a.config("cycle", "queue:\n  max_open: 2\n")
		a.queue("waits-for-a-dependency")
		l.someoneRanACycle(10 * time.Minute)
		l.build("built-since-that-cycle")
		if err := l.st.BlockChange(l.ctx, store.QuarantineRecord{ChangeName: "blocked-one", Reason: "gate_failure"}); err != nil {
			t.Fatal(err)
		}
		l.disp.pass(0, 0) // both workers free, and nothing they can start
	})
	manual, early := store.CycleTriggerManual, store.CycleTriggerOutOfWork

	l.waitWaits(advanceQueueFull)
	for i := 0; i < 4; i++ {
		time.Sleep(100 * time.Millisecond)
		l.disp.pass(0, 0)
	}
	l.waitCycles(manual)
	var status bytes.Buffer
	if rc := StatusDaemon(l.ctx, l.root, l.st, &status); rc != 0 || !strings.Contains(status.String(), "as many changes are open as queue.max_open allows, so it waits for the tick") {
		t.Fatalf("daemon status does not say that the queue is full (rc=%d):\n%s", rc, status.String())
	}

	// The blocked change is abandoned: one change open of two, and the cycle runs early.
	if err := l.st.MarkChangeDiscarded(l.ctx, "blocked-one"); err != nil {
		t.Fatal(err)
	}
	l.disp.pass(0, 0)
	l.waitCycles(manual, early)

	l.stop()
	log := l.log.String()
	if n := strings.Count(log, "the queue takes no more (2 changes open (queue.max_open: 2)): the next cycle waits for its tick"); n != 1 {
		t.Fatalf("expected the daemon log to say once that the queue is full, got %d:\n%s", n, log)
	}
	if !strings.Contains(log, "queue: 1 change open (queue.max_open: 2): this cycle may queue 1 finding") {
		t.Fatalf("the early cycle did not count the queue:\n%s", log)
	}
}
