package portal

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// TestBacklogSaysWhyEachChangeWaits: the backlog lists the changes of the queue by priority, each
// with the state the database gives it and the facts on record that hold it back. Only a change
// that waits with nothing open can be taken out, and the dispatcher is asked what starts next.
func TestBacklogSaysWhyEachChangeWaits(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)

	whole, body := overviewPage(t, srv, "/backlog")
	wantOnPage(t, "the backlog", body,
		"Backlog", "Simulate dispatch", `data-post="/api/dispatcher/simulations"`,
		// The form that adds a change.
		"Add a change to the backlog", `data-api="/api/backlog"`, `name="change_name"`, `name="priority"`, `name="depends_on"`, `name="description"`,
		// Each change with its state, and what holds it back.
		`href="/changes/add-search"`, "critical", "building",
		`href="/changes/add-api"`, "rebuild requested", "its last run: ready to merge",
		`href="/changes/add-ui"`, "It depends on", "which is rebuild requested.",
		`href="/changes/add-billing"`, "It is blocked: gate failure", "It waits for a person.",
		`href="/changes/add-export"`,
		// The one that waits with nothing open can be taken out.
		`data-post="/api/backlog/add-ui"`, `data-method="DELETE"`,
	)
	wantNotOnPage(t, "the backlog", body, `data-post="/api/backlog/add-search"`, `data-post="/api/backlog/add-billing"`, `data-post="/api/backlog/add-api"`,
		"The backlog is empty.", "data-plan")
	inOrder(t, "the backlog, by priority and then by how long each has waited", body,
		"add-search", "add-api", "add-ui", "add-billing", "add-export")
	if n := strings.Count(body, "btn-primary"); n != 1 {
		t.Errorf("the backlog has %d primary actions, want one: adding a change", n)
	}
	if !strings.Contains(whole, `data-live-interval="15"`) {
		t.Errorf("the backlog does not keep itself up to date")
	}
	// The menu leads to the backlog and to the merge queue.
	wantOnPage(t, "the menu", whole, `href="/backlog"`, `href="/merge"`)

	// The proposals, by status.
	_, body = overviewPage(t, srv, "/backlog/proposals")
	wantOnPage(t, "the proposals", body, "add-cache", "Cache the feed of widgets", "high priority", "from exploration", "waits for a decision",
		`data-post="/api/proposals/add-cache/approve"`, `data-post="/api/proposals/add-cache/reject"`, `data-post="/api/proposals/review"`,
		`href="/backlog/proposals?status=rejected"`, `aria-selected="true"`)
	wantNotOnPage(t, "the proposals that wait", body, "add-old", "Simulate dispatch")
	_, body = overviewPage(t, srv, "/backlog/proposals?status=rejected")
	wantOnPage(t, "the rejected proposals", body, "add-old", "An old idea", "rejected")
	wantNotOnPage(t, "the rejected proposals", body, "add-cache", `data-post="/api/proposals/add-old/approve"`)
	_, body = overviewPage(t, srv, "/backlog/proposals?status=integrated")
	wantOnPage(t, "a status no proposal has", body, "No proposal has this status.")
	if code, _, _ := getPage(t, srv, "/backlog/proposals?status=pending"); code != http.StatusBadRequest {
		t.Errorf("GET /backlog/proposals?status=pending = %d, want 400", code)
	}

	// The API returns the queue with its facts, and the proposals.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/backlog", nil)
	queue, _ := data["queue"].([]any)
	proposals, _ := data["proposals"].(map[string]any)
	items, _ := proposals["items"].([]any)
	if code != http.StatusOK || len(queue) != 5 || data["status"] != "proposed" || len(items) != 1 {
		t.Fatalf("GET /api/backlog = %d %s", code, raw)
	}
	byName := map[string]map[string]any{}
	for _, it := range queue {
		c, _ := it.(map[string]any)
		byName[asStr(c["change_name"])] = c
	}
	facts, _ := byName["add-ui"]["facts"].([]any)
	if len(facts) != 1 || facts[0].(map[string]any)["kind"] != "dependency" || facts[0].(map[string]any)["other_change"] != "add-api" ||
		byName["add-ui"]["state"] != "queued" || byName["add-billing"]["state"] != "blocked" {
		t.Errorf("the queue of /api/backlog = %s", raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/backlog?status=nope", nil)
	if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
		t.Errorf("GET /api/backlog?status=nope = %d %s", code, raw)
	}
}

// TestBacklogOfAWorkspaceWithNothing: an empty queue says how a change gets into it.
func TestBacklogOfAWorkspaceWithNothing(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	_, body := overviewPage(t, srv, "/backlog")
	wantOnPage(t, "an empty backlog", body, "The backlog is empty.", "Add a change to the backlog")
	_, body = overviewPage(t, srv, "/backlog/proposals")
	wantOnPage(t, "no proposals", body, "No proposal has this status.")
}

// TestTheMergeQueue: the merge queue shows what the merger does now, the manifests that wait with
// the capabilities each touches, the base branches that fail on their own with their decision, and
// the latest attempts with their result and the reason the merger gave.
func TestTheMergeQueue(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seed := seedOverview(t, srv, st, root)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// As the seed leaves it: nothing waits, the merger is idle, and one attempt was sent back.
	whole, body := overviewPage(t, srv, "/merge")
	wantOnPage(t, "the merge queue", body,
		"Merge queue", "Run the merger", `data-post="/api/merger/runs"`,
		// The gates of the base, from the head of the page and from the base that fails.
		"Run the gates on the base", `data-post="/api/gates/runs"`,
		"The merger is idle", "Nothing waits to be merged.",
		"Base branches that fail on their own", "No change can merge into repos/core.", `data-post="/api/bases/accept"`,
		"The latest attempts", `href="/changes/add-api"`, fmt.Sprintf(`href="/changes/add-api#merge-%d"`, seed.mergeID),
		"sent back to be rebuilt", "CONFLICT (content): Merge conflict in api/widget.go",
	)
	if n := strings.Count(body, `data-post="/api/gates/runs"`); n != 2 {
		t.Errorf("the merge queue offers the gates of the base %d times, want twice: at its head, and next to the base that fails", n)
	}
	if n := strings.Count(body, "btn-primary"); n != 0 {
		t.Errorf("the merge queue has %d primary actions, want none: nothing waits", n)
	}
	if !strings.Contains(whole, `data-live-interval="10"`) {
		t.Errorf("the merge queue does not keep itself up to date")
	}

	// Two manifests wait: the merger works on one, and has not tried the other.
	must(st.EnqueueReady(ctx, store.ReadyManifest{ChangeName: "add-cart", Branch: "feat/spec-add-cart", Worktree: "/tmp/worker-add-cart",
		HeadSHA: "1111111222222233333334444444", Capabilities: []string{"cart", "lock:db-schema"}}))
	must(st.EnqueueReady(ctx, store.ReadyManifest{ChangeName: "add-wishlist", Branch: "feat/spec-add-wishlist", Worktree: "/tmp/worker-add-wishlist",
		HeadSHA: "5555555666666677777778888888", Capabilities: []string{"wishlist"}}))
	attempt, err := st.BeginMergeAttempt(ctx, "add-cart")
	must(err)
	must(st.SetMergeAttemptState(ctx, attempt, "RUNNING_GATES"))

	_, body = overviewPage(t, srv, "/merge")
	wantOnPage(t, "the merge queue with work", body,
		"The merger now", `href="/changes/add-cart"`, "running the gates", fmt.Sprintf("attempt #%d", attempt),
		"Waiting to be merged", "1111111222", "feat/spec-add-cart", "cart", "lock:db-schema", "The merger tried it 1 time.", "in progress",
		`href="/changes/add-wishlist"`, "wishlist", "The merger has not tried it yet.",
	)
	wantNotOnPage(t, "the merge queue with work", body, "The merger is idle", "Nothing waits to be merged.")
	inOrder(t, "the manifests, the one that waits for longest first", body[strings.Index(body, "Waiting to be merged"):], "add-cart", "add-wishlist")

	// A stopped attempt is a stop, not a failure: it is told as one, without the color of an error.
	must(st.FinishMergeAttempt(ctx, attempt, "stopped", "the daemon was restarted"))
	_, body = overviewPage(t, srv, "/merge")
	stopped := body[strings.Index(body, "The latest attempts"):]
	wantOnPage(t, "the attempts after one was stopped", stopped, "stopped", "the daemon was restarted", "The merger was stopped before it finished: it tries again.")
	if i := strings.Index(stopped, "the daemon was restarted"); i < 0 || strings.Contains(stopped[strings.LastIndex(stopped[:i], "<tr"):i], "badge-error") {
		t.Errorf("a stopped attempt is drawn as an error")
	}

	// The kit paused: the merger starts nothing, and the page says so instead of offering it.
	if _, err := st.Pause(ctx, "looking at a gate", "portal", ""); err != nil {
		t.Fatal(err)
	}
	_, body = overviewPage(t, srv, "/merge")
	wantOnPage(t, "the merge queue of a paused kit", body, "The kit is paused: the merger starts nothing until it is resumed.")
	wantNotOnPage(t, "the merge queue of a paused kit", body, `data-post="/api/merger/runs"`)

	// The API returns the same, and pages the attempts that ended.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/merge?limit=1", nil)
	waiting, _ := data["waiting"].([]any)
	recent, _ := data["recent"].([]any)
	bases, _ := data["broken_bases"].([]any)
	if code != http.StatusOK || len(waiting) != 2 || len(recent) != 1 || len(bases) != 1 || data["next"] == nil || data["current"] != nil {
		t.Fatalf("GET /api/merge?limit=1 = %d %s", code, raw)
	}
	first, _ := waiting[0].(map[string]any)
	caps, _ := first["capabilities"].([]any)
	if first["change_name"] != "add-cart" || len(caps) != 2 || first["attempts"] != float64(1) {
		t.Errorf("the first manifest of /api/merge = %v", first)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/merge?limit=1&before="+asStr(data["next"]), nil)
	recent, _ = data["recent"].([]any)
	if code != http.StatusOK || len(recent) != 1 || recent[0].(map[string]any)["change_name"] != "add-api" || data["next"] != nil {
		t.Errorf("the second page of the attempts = %d %s", code, raw)
	}
	// An earlier page of the attempts shows them, and not what waits now.
	_, body = overviewPage(t, srv, "/merge?limit=1&before="+fmt.Sprint(attempt))
	wantOnPage(t, "an earlier page of the attempts", body, "Earlier attempts", `href="/changes/add-api"`, "Latest")
	wantNotOnPage(t, "an earlier page of the attempts", body, "Waiting to be merged", "The merger now")
	if code, _, _ := getPage(t, srv, "/merge?before=x"); code != http.StatusBadRequest {
		t.Errorf("GET /merge?before=x = %d, want 400", code)
	}
}

// seedCycleWork makes the cycle that ended in the overview (seedOverview) the one that queued a
// change, add-sitemap, as a cycle does after its judgment: the backlog gets the change, the cycle
// says it added it, and the registry mirrors the backlog.
func seedCycleWork(t *testing.T, st *store.Store, root string) {
	t.Helper()
	ctx := context.Background()
	backlog := filepath.Join(root, cycle.BacklogFile)
	items, err := cycle.ReadJSONList(backlog)
	if err != nil {
		t.Fatal(err)
	}
	item := map[string]any{"change_name": "add-sitemap", "priority": "low", "description": "A sitemap of the widgets", "queued_at": "2026-11-08T12:05:00Z"}
	if err := cycle.WriteJSONList(backlog, append(items, item)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCycleWork(ctx, "20261108T120000Z", store.CycleWork{Queued: []map[string]any{item}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncJSONToSQLite(ctx, root); err != nil {
		t.Fatal(err)
	}
}

// writeCycleReport writes the report of a cycle that did not record one, the way the judge leaves
// it: among the reports of the kit, named after a minute in which the cycle ran and written during
// it. It returns the name of the file. The cycle of a seed begins and ends in the same instant, by
// the clock of the store, which runs a moment ahead of the clock of the machine after a burst of
// rows: the minute and the instant are taken from the row, not from the clock of the machine.
func writeCycleReport(t *testing.T, st *store.Store, root, stamp, text string) string {
	t.Helper()
	ctx := context.Background()
	var rec *store.CycleRecord
	if err := st.View(ctx, func(db *store.View) (err error) {
		rec, err = db.Cycle(ctx, stamp)
		return err
	}); err != nil || rec == nil {
		t.Fatalf("the cycle %s of the seed: %v", stamp, err)
	}
	began, err := time.Parse(time.RFC3339, rec.StartedAt)
	if err != nil {
		t.Fatalf("the cycle %s began at %q: %v", stamp, rec.StartedAt, err)
	}
	name := began.UTC().Format("20060102-1504") + ".md"
	report := store.RuntimePath(root, "cycle", "reports", name)
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	written := began.Add(time.Second)
	if err := os.Chtimes(report, written, written); err != nil {
		t.Fatal(err)
	}
	return name
}

// TestCyclesAreReadFromTheirTable: the list of cycles and the page of one show what the table of
// cycles holds (how each ended, its strategy, what started it) with what was recorded under it and
// the report it wrote. Nothing is worked out of a log.
func TestCyclesAreReadFromTheirTable(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	seedRunningCycle(t, st)
	seedCycleWork(t, st, root)
	ctx := context.Background()
	const ended, running = "20261108T120000Z", "20261110T100000Z"
	if err := st.UpdateCycle(ctx, store.Cycle{Stamp: ended, JudgeEngine: "claude", FindingsCount: 3, UmbrellaSHA: "0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueVisualQANext(store.WithCycle(ctx, ended), store.VisualQAItem{What: "the list of widgets shows their names", ChangeName: "add-ui"}); err != nil {
		t.Fatal(err)
	}
	reportName := writeCycleReport(t, st, root, ended, "# Cycle report\n\nThree findings in the API contracts.\n")

	whole, body := overviewPage(t, srv, "/cycles")
	wantOnPage(t, "the list of cycles", body,
		"Cycles", "Run a cycle", `data-post="/api/cycles"`,
		`href="/cycles/`+running+`"`, "running", "started by a person", "running for",
		`href="/cycles/`+ended+`"`, "done", "api-contracts", "ahead of its interval, because the pipeline ran out of work", "claude",
	)
	inOrder(t, "the cycles, the latest first", body, running, ended)
	if !strings.Contains(whole, `data-live-interval="15"`) {
		t.Errorf("the list of cycles does not keep itself up to date while a cycle runs")
	}

	_, body = overviewPage(t, srv, "/cycles/"+ended)
	wantOnPage(t, "the page of a cycle that ended", body,
		ended, "done", "api-contracts", "claude", "0123456789", `href="/events?cycle_stamp=`+ended+`"`,
		"ahead of its interval, because the pipeline ran out of work",
		// What was recorded under it, and its report.
		"a change it queued", `href="/changes/add-sitemap"`, "A sitemap of the widgets",
		"Check in a browser", "the list of widgets shows their names", `href="/changes/add-ui"`,
		"Its report", reportName, "the report written while this cycle ran", "Three findings in the API contracts.",
		// The cycle after it.
		`href="/cycles/`+running+`"`, "Newer",
	)
	wantNotOnPage(t, "the page of a cycle that ended", body, "Older", "No report is found for this cycle.")

	whole, body = overviewPage(t, srv, "/cycles/"+running)
	wantOnPage(t, "the page of a cycle that runs", body,
		running, "running", "started by a person", "running for",
		"Its judgment", `href="/executions/cycle-`+running+`"`, "claude",
		`href="/cycles/`+ended+`"`, "Older",
	)
	wantNotOnPage(t, "the page of a cycle that runs", body, "Newer", "What was recorded under it")
	// The part of what it produced is there, empty, for the refresh of the page to fill once the
	// cycle has published its work.
	if !strings.Contains(whole, `data-live-interval="10"`) || !strings.Contains(body, `id="cycle-produced" data-live-region`) {
		t.Errorf("the page of a cycle that runs does not keep itself up to date")
	}

	// A cycle that is not on record, and the tabs of the page of before, are not found.
	for _, path := range []string{"/cycles/20200101T000000Z", "/cycles/" + ended + "/log"} {
		if code, _, _ := getPage(t, srv, path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}

	// The API returns the rows, and one cycle with what is under it and its report.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/cycles?limit=1", nil)
	items, _ := data["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["stamp"] != running || data["next"] == nil {
		t.Fatalf("GET /api/cycles?limit=1 = %d %s", code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/cycles?limit=1&before="+asStr(data["next"]), nil)
	items, _ = data["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["stamp"] != ended || data["next"] != nil {
		t.Errorf("the second page of the cycles = %d %s", code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/cycles/"+ended, nil)
	checks, _ := data["visual_qa"].([]any)
	rep, _ := data["report"].(map[string]any)
	if code != http.StatusOK || data["stamp"] != ended || data["findings_count"] != float64(3) || data["newer"] != running || len(checks) != 1 ||
		!strings.Contains(asStr(rep["text"]), "Three findings") || rep["name"] != reportName || rep["recorded"] != false {
		t.Errorf("GET /api/cycles/%s = %d %s", ended, code, raw)
	}
	queued, _ := data["queued"].([]any)
	if len(queued) != 1 {
		t.Fatalf("GET /api/cycles/%s lists %d changes the cycle queued, want one: %s", ended, len(queued), raw)
	}
	if c, _ := queued[0].(map[string]any); c["change_name"] != "add-sitemap" || c["state"] != "queued" || c["description"] != "A sitemap of the widgets" {
		t.Errorf("the change the cycle queued = %v", c)
	}
	// A cycle that queued nothing says so with an empty list, not with a missing one.
	_, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/cycles/"+running, nil)
	if none, listed := data["queued"].([]any); !listed || len(none) != 0 {
		t.Errorf("GET /api/cycles/%s: queued = %v in %s", running, data["queued"], raw)
	}

	// A report the cycle recorded is its report, whatever else was written while it ran; a path
	// outside the reports of the kit is not read.
	recorded := store.RuntimePath(root, "cycle", "reports", ended+".md")
	if err := os.WriteFile(recorded, []byte("# The recorded report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateCycle(ctx, store.Cycle{Stamp: ended, ReportPath: recorded, FindingsCount: -1}); err != nil {
		t.Fatal(err)
	}
	_, body = overviewPage(t, srv, "/cycles/"+ended)
	wantOnPage(t, "a cycle with a recorded report", body, ended+".md", "the report the cycle recorded", "The recorded report")
	wantNotOnPage(t, "a cycle with a recorded report", body, "Three findings in the API contracts.")
	outside := filepath.Join(root, ".foreman-kit", "agents.yaml")
	if err := os.WriteFile(outside, []byte("secret: yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateCycle(ctx, store.Cycle{Stamp: ended, ReportPath: outside, FindingsCount: -1}); err != nil {
		t.Fatal(err)
	}
	_, body = overviewPage(t, srv, "/cycles/"+ended)
	wantNotOnPage(t, "a cycle whose recorded report is outside the reports of the kit", body, "secret: yes")
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/cycles/20200101T000000Z", nil)
	if c, _ := errorOf(data); code != http.StatusNotFound || c != "not_found" {
		t.Errorf("GET /api/cycles of a cycle that is not on record = %d %s", code, raw)
	}

	// A change the cycle queued in its own copy, which the backlog of the main checkout does not
	// list: the page says so next to it, and of the one the backlog lists it says nothing.
	wantNotOnPage(t, "a cycle whose change the backlog lists", body, "the main checkout does not list it")
	apart := map[string]any{"change_name": "add-robots", "priority": "low", "description": "A robots.txt for the widgets", "queued_at": "2026-11-08T12:06:00Z"}
	// The same of a proposal it made: the page of the cycle lists it as its own, and it waits for no
	// decision until the main checkout lists it, since the decision is written in the file of that
	// checkout.
	made := map[string]any{"change_name": "add-humans", "status": "proposed", "description": "A humans.txt for the widgets"}
	if err := st.RecordCycleWork(ctx, ended, store.CycleWork{Queued: []map[string]any{apart}, Proposed: []map[string]any{made}}); err != nil {
		t.Fatal(err)
	}
	_, body = overviewPage(t, srv, "/cycles/"+ended)
	wantOnPage(t, "a cycle with a change the main checkout does not list", body,
		`href="/changes/add-robots"`, "not in the backlog", "a change it queued: the main checkout does not list it, or not yet",
		`href="/changes/add-sitemap"`,
		"add-humans", wordingOf(vocProposalStatus, store.ProposalUnknown).Text, "A humans.txt for the widgets",
	)
	if words := wordingOf(vocProposalStatus, store.ProposalUnknown); words.Text == store.ProposalUnknown || words.Means == "" {
		t.Errorf("a proposal the main checkout does not list has no words of its own: %+v", words)
	}
	for _, path := range []string{"/inbox", "/board", "/backlog/proposals"} {
		_, body = overviewPage(t, srv, path)
		wantNotOnPage(t, path+" with a proposal the main checkout does not list", body, "add-humans")
	}
}

// TestEventsAreNarrowedByWhatTheyAreAbout: the list of events is narrowed by the columns an event
// carries (its type, its change, its execution, its cycle), each filter keeps the others, and the
// page of one event leads to every row it names.
func TestEventsAreNarrowedByWhatTheyAreAbout(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	ctx := context.Background()
	if err := st.EmitEvent(store.WithCycle(ctx, "20261108T120000Z"), store.EventInput{Event: "cycle.strategy", Payload: map[string]any{"strategy": "api-contracts", "nested": map[string]any{"a": 1}}}); err != nil {
		t.Fatal(err)
	}
	all, err := st.QueryEvents(ctx, store.EventFilter{})
	if err != nil || len(all) < 5 {
		t.Fatalf("the seed recorded %d events (err %v)", len(all), err)
	}
	var ofExecution, ofCycle store.EventRecord
	types := map[string]bool{}
	for _, e := range all {
		types[e.Event] = true
		if e.ExecID != "" && e.Change != "" {
			ofExecution = e
		}
		if e.CycleStamp != "" {
			ofCycle = e
		}
	}
	if ofExecution.ID == 0 || ofCycle.ID == 0 {
		t.Fatalf("the seed has no event of an execution of a change, or none of a cycle: %+v %+v", ofExecution, ofCycle)
	}

	_, body := overviewPage(t, srv, "/events")
	wantOnPage(t, "the list of events", body, "Events", `name="event"`, fmt.Sprintf("Every type (%d)", len(all)),
		fmt.Sprintf(`href="/events/%d"`, ofExecution.ID), `href="/changes/`+ofExecution.Change+`"`, "its execution")
	for name := range types {
		if !strings.Contains(body, `<option value="`+name+`"`) {
			t.Errorf("the list of events does not offer the type %s", name)
		}
	}
	wantNotOnPage(t, "the list of events", body, "Search any field", "What the list is narrowed to")

	// Narrowed to a change and a type: the filters are part of the address, and each keeps the other.
	_, body = overviewPage(t, srv, "/events?change="+ofExecution.Change+"&event="+ofExecution.Event)
	wantOnPage(t, "the events of a change, of one type", body, "What the list is narrowed to", `value="`+ofExecution.Change+`"`,
		`<option value="`+ofExecution.Event+`" selected>`, `href="/events?event=`+ofExecution.Event+`"`, fmt.Sprintf(`href="/events/%d"`, ofExecution.ID))
	wantNotOnPage(t, "the events of a change, of one type", body, fmt.Sprintf(`href="/events/%d"`, ofCycle.ID))
	_, body = overviewPage(t, srv, "/events?cycle_stamp=20261108T120000Z")
	wantOnPage(t, "the events of a cycle", body, fmt.Sprintf(`href="/events/%d"`, ofCycle.ID), `href="/cycles/20261108T120000Z"`, "cycle.strategy")
	_, body = overviewPage(t, srv, "/events?exec_id="+ofExecution.ExecID+"&event=no.such.event")
	wantOnPage(t, "a filter that matches nothing", body, "No event matches.")

	// What an address cannot ask for is a problem of the request.
	for _, path := range []string{"/events?change_run_id=x", "/events?gate_round_id=-1", "/events?before=x", "/events?limit=0"} {
		if code, _, _ := getPage(t, srv, path); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, code)
		}
	}

	// One event: what it carries, and the way to each row it names.
	_, body = overviewPage(t, srv, fmt.Sprintf("/events/%d", ofExecution.ID))
	wantOnPage(t, "the page of an event", body, fmt.Sprintf("Event #%d", ofExecution.ID), ofExecution.Event, "What it carries", "Show the event as it was recorded",
		"What it is about", `href="/changes/`+ofExecution.Change+`"`, `href="/executions/`+ofExecution.ExecID+`"`,
		"Events like it", `href="/events?event=`+ofExecution.Event+`"`, `href="/events?change=`+ofExecution.Change+`"`, `href="/events?exec_id=`+ofExecution.ExecID+`"`)
	_, body = overviewPage(t, srv, fmt.Sprintf("/events/%d", ofCycle.ID))
	wantOnPage(t, "the page of an event of a cycle", body, "cycle.strategy", "strategy", "api-contracts", "nested", `href="/cycles/20261108T120000Z"`, `href="/events?cycle_stamp=20261108T120000Z"`)
	for _, path := range []string{"/events/0", "/events/999999999", "/events/abc"} {
		if code, _, _ := getPage(t, srv, path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}

	// The API pages by cursor and returns the event as it is.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/events?limit=2", nil)
	items, _ := data["items"].([]any)
	if code != http.StatusOK || len(items) != 2 || data["next"] == nil {
		t.Fatalf("GET /api/events?limit=2 = %d %s", code, raw)
	}
	latest := items[0].(map[string]any)["id"]
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/events?limit=2&before="+asStr(data["next"]), nil)
	items, _ = data["items"].([]any)
	if code != http.StatusOK || len(items) == 0 || items[0].(map[string]any)["id"] == latest {
		t.Errorf("the second page of the events = %d %s", code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, fmt.Sprintf("/api/events/%d", ofCycle.ID), nil)
	if code != http.StatusOK || data["event"] != "cycle.strategy" || data["cycle_stamp"] != "20261108T120000Z" || data["payload"] == nil {
		t.Errorf("GET /api/events/%d = %d %s", ofCycle.ID, code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/events?merge_attempt_id=abc", nil)
	if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
		t.Errorf("GET /api/events?merge_attempt_id=abc = %d %s", code, raw)
	}
}

// TestAnAgentASignalEndedIsNotAnError: an agent that the kit, a person or the system stopped with a
// signal ends with a code above 128. That is a stop, told as one; a code of its own is an error.
func TestAnAgentASignalEndedIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		code       int
		tone, says string
	}{
		{0, "", "finished its prompt"},
		{1, "error", "exited with an error (code 1)"},
		{127, "error", "exited with an error (code 127)"},
		{143, "warning", "stopped before it finished, by the signal TERM (exit code 143)"},
		{137, "warning", "by the signal KILL"},
		{130, "warning", "by the signal INT"},
		{139, "warning", "by the signal 11"},
	} {
		event := map[string]any{"event": "agent.exit", "code": float64(tc.code)}
		if got := eventTone(event); got != tc.tone {
			t.Errorf("an agent that exits with %d has the tone %q, want %q", tc.code, got, tc.tone)
		}
		if got := eventDescription(event); !strings.Contains(got, tc.says) {
			t.Errorf("an agent that exits with %d is described as %q, want it to say %q", tc.code, got, tc.says)
		}
	}
}

// seedWork adds to the overview what the pages of the queue and of the cycles draw besides: two
// manifests in the merge queue, one of them being merged, and a check in a browser and a report
// under the cycle that ended.
func seedWork(t *testing.T, st *store.Store, root string) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.EnqueueReady(ctx, store.ReadyManifest{ChangeName: "add-cart", Branch: "feat/spec-add-cart", Worktree: "/tmp/worker-add-cart",
		HeadSHA: "1111111222222233333334444444", Capabilities: []string{"cart", "lock:db-schema"}}))
	must(st.EnqueueReady(ctx, store.ReadyManifest{ChangeName: "add-wishlist", Branch: "feat/spec-add-wishlist", Worktree: "/tmp/worker-add-wishlist",
		HeadSHA: "5555555666666677777778888888", Capabilities: []string{"wishlist"}}))
	attempt, err := st.BeginMergeAttempt(ctx, "add-cart")
	must(err)
	must(st.SetMergeAttemptState(ctx, attempt, "RUNNING_GATES"))

	const ended = "20261108T120000Z"
	must(st.UpdateCycle(ctx, store.Cycle{Stamp: ended, JudgeEngine: "claude", FindingsCount: 3}))
	_, err = st.EnqueueVisualQANext(store.WithCycle(ctx, ended), store.VisualQAItem{What: "the list of widgets shows their names", ChangeName: "add-ui"})
	must(err)
	writeCycleReport(t, st, root, ended, "# Cycle report\n\nThree findings in the API contracts:\n\n- a route answers 200 where its spec says 201\n- two fields are not in the contract\n")
}
