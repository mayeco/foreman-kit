package portal

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// seedOverview adds to seedChanges what the pages a person starts from show besides the changes: a
// worker that holds its lease, a question an agent left about a change that is not blocked, a base
// branch that fails on its own, a proposal nobody decided, a cycle that ended and the schedule of a
// daemon.
func seedOverview(t *testing.T, srv *Server, st *store.Store, root string) changesSeed {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	seed := seedChanges(t, st, root)
	// add-search is being built by a worker that is alive: it holds the lease of the change.
	_, err := st.AcquireLock(ctx, store.WorkerLeaseKey("add-search"), store.LockOptions{OwnerType: "worker", PID: os.Getpid(), Slot: 2, TTL: time.Hour})
	must(err)
	// Its agent asked a question, and nothing blocks the change.
	question := filepath.Join(root, ".worktrees", "worker-add-search", "openspec", "changes", "add-search", ".needs-human")
	must(os.WriteFile(question, []byte("Is the search case-sensitive?\n"), 0o644))
	_, err = st.RecordBaseVerification(ctx, store.BaseVerification{RepoPath: "repos/core", BaseSHA: "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d",
		CheckName: "gate:test", Status: store.BaseVerdictFailed, ExitCode: 1, OutsideCommits: 2, Excerpt: "--- FAIL: TestLedger"})
	must(err)
	must(cycle.WriteProposals(root, []map[string]any{
		{"change_name": "add-cache", "status": "proposed", "priority": "high", "source": "exploration", "description": "Cache the feed of widgets", "proposed_at": "2026-11-04T08:00:00Z"},
		{"change_name": "add-old", "status": "rejected", "description": "An old idea", "proposed_at": "2026-10-01T09:00:00Z"},
	}))
	srv.syncBacklog(ctx)
	// The cycle of the seed ended, started ahead of its interval.
	must(st.BeginCycle(ctx, store.Cycle{Stamp: "20261108T120000Z", Trigger: store.CycleTriggerOutOfWork, Strategy: "api-contracts", PID: os.Getpid()}))
	must(st.FinishCycle(ctx, "20261108T120000Z", "done", ""))
	// A daemon says when its next cycle runs, and what keeps it from starting earlier.
	_, err = st.AcquireLock(ctx, "daemon", store.LockOptions{OwnerType: "daemon", PID: os.Getpid(), TTL: time.Hour})
	must(err)
	must(st.SetCycleSchedule(ctx, os.Getpid(), store.CycleSchedule{Interval: 30 * time.Minute, IdleDelay: 15 * time.Minute,
		Latest: time.Now().Add(12 * time.Minute), Earliest: time.Now().Add(-3 * time.Minute), Waits: "queue_full"}))
	return seed
}

// seedRunningCycle starts a cycle a person asked for and leaves it running, on the execution of its
// judgment.
func seedRunningCycle(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	const stamp = "20261110T100000Z"
	if err := st.BeginCycle(ctx, store.Cycle{Stamp: stamp, Trigger: store.CycleTriggerManual, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "cycle-" + stamp, Kind: "cycle", CycleStamp: stamp,
		EnginesRequested: "claude", Phase: "cycle", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginAttempt(store.WithExecution(ctx, "cycle-"+stamp), "cycle-"+stamp, "claude", 0); err != nil {
		t.Fatal(err)
	}
}

func overviewPage(t *testing.T, srv http.Handler, path string) (whole, main string) {
	t.Helper()
	code, whole, _ := getPage(t, srv, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d\n%s", path, code, whole)
	}
	if strings.Contains(whole, "could not be rendered") {
		t.Fatalf("GET %s did not render:\n%s", path, whole)
	}
	return whole, mainOf(t, whole)
}

// TestDashboardSaysWhatTheKitIsDoing: the first page lists facts: how many changes are in each
// state, what runs at this instant, when the next cycle runs and what the kit closed last. What
// waits for a person is one line with the way to it.
func TestDashboardSaysWhatTheKitIsDoing(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seed := seedOverview(t, srv, st, root)

	whole, body := overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard", body,
		"Dashboard", "The kit is active",
		// What waits for a person, by kind, and the way to it.
		"5 things wait for you", "1 blocked change", "1 question from an agent", "1 base branch that fails on its own", "1 worker that is gone", "1 proposal to decide",
		`href="/inbox"`, "Open Needs you",
		// The changes by state: each count is a link to the list in that state.
		`href="/changes?state=queued"`, `href="/changes?state=building"`, `href="/changes?state=integrated"`,
		`href="/changes?state=blocked"`, "1 blocked", `href="/changes?state=rebuild_requested"`, "1 rebuild requested",
		// What runs: the worker of add-search on its BUILD with an idle agent, and the worker of
		// add-export, which is gone.
		"Running now", `href="/changes/add-search"`, `href="/executions/build-live"`, "BUILD", "claude", "no activity for", "stopped at 20m 00s",
		`href="/changes/add-export"`, "worker gone", "Nothing holds the lease of the change",
		// The cycle: what the daemon says about the next one, and the last one.
		"Next cycle", "At the latest", "the daemon runs one every 30m 00s", "It may start earlier, as soon as a worker is free with nothing to start.",
		"Now it waits because as many changes are open as the queue takes.",
		"Last cycle", `href="/cycles/20261108T120000Z"`, "done, on api-contracts", "ahead of its interval, because the pipeline ran out of work",
		// What it closed last.
		"What it closed last", "blocked: gate failure, at the verify stage", "in force",
		"sent back to be rebuilt", fmt.Sprintf(`href="/changes/add-api#merge-%d"`, seed.mergeID), "ready to merge", `href="/changes/add-api#run-`,
		"cycle 20261108T120000Z",
	)
	wantNotOnPage(t, "Dashboard", body, "Guards", "Latest events", "Mission control", "Nothing runs now.", "No daemon says when")
	// The latest first.
	inOrder(t, "what Dashboard says the kit closed last", body[strings.Index(body, "What it closed last"):], "cycle 20261108T120000Z", "blocked: gate failure", "sent back to be rebuilt", "ready to merge")
	if n := strings.Count(body, "btn-primary"); n != 1 {
		t.Errorf("Dashboard has %d primary actions, want one: the way to what waits", n)
	}
	if !strings.Contains(whole, `data-live-interval="8"`) {
		t.Errorf("Dashboard does not keep itself up to date")
	}
	// The menu counts what waits, in the color of what stops work.
	if !regexp.MustCompile(`href="/inbox"[^>]*>(?s:.*?)Needs you(?s:.*?)badge-error[^>]*>5<`).MatchString(whole) {
		t.Errorf("the menu does not count the 5 things that wait, as something that stops work")
	}

	// The API returns the same facts.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/dashboard", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/dashboard = %d: %s", code, raw)
	}
	counts, _ := data["counts"].(map[string]any)
	waits, _ := data["waiting"].(map[string]any)
	next, _ := data["next_cycle"].(map[string]any)
	runs, _ := data["runs"].([]any)
	if counts["building"] != float64(2) || counts["blocked"] != float64(1) || counts["rebuild_requested"] != float64(1) || counts["queued"] != float64(1) ||
		waits["total"] != float64(5) || waits["urgent"] != true || waits["proposals"] != float64(1) ||
		next["waits"] != "queue_full" || next["interval_seconds"] != float64(1800) || next["idle_delay_seconds"] != float64(900) || len(runs) != 2 {
		t.Errorf("GET /api/dashboard = %s", raw)
	}

	// Paused, with nothing else waiting but the pause.
	if _, err := st.Pause(context.Background(), "investigating a gate", "portal", ""); err != nil {
		t.Fatal(err)
	}
	_, body = overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard, paused", body, "The kit is paused: investigating a gate", "6 things wait for you", "the kit is paused")

	// A cycle that runs is one row, with the execution of its judgment: the execution is not listed
	// again on its own. The next cycle cannot start early before its delay has passed.
	seedRunningCycle(t, st)
	if err := st.SetCycleSchedule(context.Background(), os.Getpid(), store.CycleSchedule{Interval: 30 * time.Minute, IdleDelay: 15 * time.Minute,
		Latest: time.Now().Add(29 * time.Minute), Earliest: time.Now().Add(14 * time.Minute), Waits: "delay"}); err != nil {
		t.Fatal(err)
	}
	_, body = overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard, with a cycle running", body, `href="/cycles/20261110T100000Z"`, "cycle 20261110T100000Z", "started by a person", "its judgment",
		"at the soonest, once a worker is free with nothing to start.", "Now it waits because the delay since the last cycle began has not passed.")
	if n := strings.Count(body, `href="/executions/cycle-20261110T100000Z"`); n != 1 {
		t.Errorf("Dashboard links %d time(s) to the execution of the cycle that runs, want once, with its cycle", n)
	}
}

// TestDashboardSaysApartWhatACycleQueuedAndTheBacklogDoesNotList: a change a cycle queued is on
// record before the main checkout lists it, in no state of the pipeline. Dashboard counts it apart
// from the changes that are not in the backlog for another reason, and leads to the cycle that
// queued it, whose page says what it waits for; once the main checkout lists it, it is one more of
// the queue, and if a person then takes it out it is one more of those that are not in the backlog.
func TestDashboardSaysApartWhatACycleQueuedAndTheBacklogDoesNotList(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	ctx := context.Background()
	const first, second = "20261108T120000Z", "20261109T120000Z"
	for _, stamp := range []string{first, second} {
		if err := st.BeginCycle(ctx, store.Cycle{Stamp: stamp}); err != nil {
			t.Fatal(err)
		}
	}
	apart := "queued by a cycle, not in the main checkout"
	unlisted := func(t *testing.T) []string {
		t.Helper()
		code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/dashboard", nil)
		list, listed := data["queued_unlisted"].([]any)
		if code != http.StatusOK || !listed {
			t.Fatalf("GET /api/dashboard = %d, queued_unlisted = %v in %s", code, data["queued_unlisted"], raw)
		}
		var names []string
		for _, it := range list {
			row, _ := it.(map[string]any)
			names = append(names, asStr(row["change_name"])+"@"+asStr(row["cycle_stamp"]))
		}
		return names
	}

	// No cycle queued anything: nothing is said of it.
	_, body := overviewPage(t, srv, "/")
	wantNotOnPage(t, "Dashboard before any cycle queued a change", body, apart)
	if got := unlisted(t); len(got) != 0 {
		t.Errorf("before any cycle queued a change the read lists %v", got)
	}

	// A cycle queues a change in its own copy: Dashboard says so, and leads to that cycle.
	robots := map[string]any{"change_name": "add-robots", "description": "A robots.txt for the widgets"}
	if err := st.RecordCycleWork(ctx, first, store.CycleWork{Queued: []map[string]any{robots}}); err != nil {
		t.Fatal(err)
	}
	_, body = overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard with a change a cycle queued", body, "1 "+apart, `href="/cycles/`+first+`"`)
	// Its count is the only one: the other, of what is not in the backlog, is named in the
	// description of this one and has no chip of its own.
	wantNotOnPage(t, "Dashboard with a change a cycle queued", body, "not in the backlog</a>")
	if got := unlisted(t); len(got) != 1 || got[0] != "add-robots@"+first {
		t.Errorf("the read lists %v, want add-robots of the first cycle", got)
	}

	// Another cycle queues another: the two are in the list of the changes that are in no state.
	humans := map[string]any{"change_name": "add-humans", "description": "A humans.txt for the widgets"}
	if err := st.RecordCycleWork(ctx, second, store.CycleWork{Queued: []map[string]any{humans}}); err != nil {
		t.Fatal(err)
	}
	_, body = overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard with changes two cycles queued", body, "2 "+apart, `href="/changes?state=unknown"`)

	// The main checkout lists the first one: it is queued, and one is left apart.
	backlog := filepath.Join(root, filepath.FromSlash(cycle.BacklogFile))
	if err := cycle.WriteJSONList(backlog, append(backlogItems(t, root), robots)); err != nil {
		t.Fatal(err)
	}
	srv.syncBacklog(ctx)
	_, body = overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard after the main checkout listed one of them", body, "1 "+apart, `href="/cycles/`+second+`"`)
	if got := unlisted(t); len(got) != 1 || got[0] != "add-humans@"+second {
		t.Errorf("after the main checkout listed add-robots the read lists %v", got)
	}

	// A person takes that one out of the backlog. It is not in the backlog, like any change that
	// was taken out, and it is not counted with what waits to be listed.
	kept := []map[string]any{}
	for _, item := range backlogItems(t, root) {
		if asStr(item["change_name"]) != "add-robots" {
			kept = append(kept, item)
		}
	}
	if err := cycle.WriteJSONList(backlog, kept); err != nil {
		t.Fatal(err)
	}
	srv.syncBacklog(ctx)
	_, body = overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard after a person took the listed one out of the backlog", body,
		"1 "+apart, `href="/cycles/`+second+`"`, "1 not in the backlog</a>")
	if got := unlisted(t); len(got) != 1 || got[0] != "add-humans@"+second {
		t.Errorf("after a person took add-robots out of the backlog the read lists %v", got)
	}
}

// TestDashboardOfAWorkspaceWithNothing: with nothing on record the first page says so in each of
// its parts, and has no action that stands out.
func TestDashboardOfAWorkspaceWithNothing(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	whole, body := overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard", body, "The kit is active", "Nothing runs now.", "No daemon says when: a cycle runs when a person starts one.",
		"No cycle has ended yet.", "Nothing has closed yet.", "Run a cycle", "Run the dispatcher", "Run the merger")
	wantNotOnPage(t, "Dashboard", body, "wait for you", "btn-primary", "Outside the pipeline")
	if regexp.MustCompile(`href="/inbox"[^>]*>(?s:.*?)</a>`).FindString(whole) == "" || strings.Contains(regexp.MustCompile(`href="/inbox"[^>]*>(?s:.*?)</a>`).FindString(whole), "badge") {
		t.Errorf("the menu shows a count for Needs you when nothing waits")
	}
}

// TestNeedsYouListsWhatWaits: everything that waits for a person is in one page, a group for each
// kind, with the buttons of that kind. What stops work comes before the proposals.
func TestNeedsYouListsWhatWaits(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	if _, err := st.Pause(context.Background(), "investigating a gate", "portal", ""); err != nil {
		t.Fatal(err)
	}

	_, body := overviewPage(t, srv, "/inbox")
	wantOnPage(t, "Needs you", body,
		"Needs you",
		// The pause.
		"The kit is paused", "investigating a gate", "a person, from the portal", `data-post="/api/workspace/resume"`,
		// The blocked change, with the question its agent left, and the three decisions.
		"Blocked changes", `href="/changes/add-billing"`, "gate failure", "at the verify stage", "coverage 91% < 100%", "Which currency do the invoices use?",
		`data-post="/api/changes/add-billing/unblock"`, `{"reset_retries":false}`, `{"reset_retries":true}`, `data-post="/api/changes/add-billing/risk-approval"`,
		`href="/changes/add-billing/intervene"`,
		// The question about a change that is not blocked.
		"Questions from agents", `href="/changes/add-search"`, "Is the search case-sensitive?", `href="/changes/add-search/intervene"`, `data-post="/api/changes/add-search/unblock"`,
		// The base that fails on its own.
		"Base branches that fail on their own", "No change can merge into repos/core.", "test gate", "2 commits were pushed to it outside the kit.", "--- FAIL: TestLedger",
		`data-post="/api/bases/accept"`, `{"repo":"repos/core","sha":"1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"}`,
		// After the base is mended, its gates are run from here to see whether it passes.
		`data-post="/api/gates/runs"`, "Run the gates on the base",
		// The worker that is gone.
		"Workers that are gone", `href="/changes/add-export"`, "nothing holds the lease of the change", `data-post="/api/changes/add-export/reclaim"`,
		// The proposal nobody decided.
		"Proposals to decide", "add-cache", "Cache the feed of widgets", "high priority", "from exploration",
		`data-post="/api/proposals/add-cache/approve"`, `data-post="/api/proposals/add-cache/reject"`,
	)
	wantNotOnPage(t, "Needs you", body, "Nothing waits for you.", "add-old", "An old idea", "btn-primary")
	inOrder(t, "Needs you", body, "The kit is paused", "Blocked changes", "Questions from agents", "Base branches that fail on their own", "Workers that are gone", "Proposals to decide")
	// The question of a blocked change is told with its block, once.
	if n := strings.Count(body, "Which currency do the invoices use?"); n != 1 {
		t.Errorf("the question of add-billing is on the page %d times, want once, with its block", n)
	}

	// One kind alone.
	_, body = overviewPage(t, srv, "/inbox?kind=proposals")
	wantOnPage(t, "Needs you, the proposals alone", body, "Proposals to decide", "add-cache")
	wantNotOnPage(t, "Needs you, the proposals alone", body, "Blocked changes", "Workers that are gone", "The kit is paused")

	// The API returns the same rows, and one kind when asked.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/inbox", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/inbox = %d: %s", code, raw)
	}
	n := func(key string) int { list, _ := data[key].([]any); return len(list) }
	if data["count"] != float64(6) || data["urgent"] != true || data["pause"] == nil || n("blocked") != 1 || n("questions") != 1 ||
		n("broken_bases") != 1 || n("lost_runs") != 1 || n("proposals") != 1 {
		t.Errorf("GET /api/inbox = %s", raw)
	}
	blocked, _ := data["blocked"].([]any)[0].(map[string]any)
	if blocked["change_name"] != "add-billing" || blocked["reason"] != "gate_failure" || blocked["question"] != "Which currency do the invoices use?" || blocked["has_worktree"] != true {
		t.Errorf("the blocked change of /api/inbox = %v", blocked)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/inbox?kind=lost_runs", nil)
	if code != http.StatusOK || data["count"] != float64(6) || data["kind"] != "lost_runs" || data["pause"] != nil ||
		len(data["lost_runs"].([]any)) != 1 || len(data["blocked"].([]any)) != 0 || len(data["proposals"].([]any)) != 0 {
		t.Errorf("GET /api/inbox?kind=lost_runs = %d %s", code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/inbox?kind=nope", nil)
	if c, msg := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" || !strings.Contains(msg, "nope") {
		t.Errorf("GET /api/inbox?kind=nope = %d %s", code, raw)
	}

	// A decision takes its row off the list: the portal brings the mirror of the proposals up to date
	// after its own edit, since no page does.
	if code, _, raw, _ := doReq(t, srv, http.MethodPost, "/api/proposals/add-cache/reject", map[string]any{}); code != http.StatusOK {
		t.Fatalf("rejecting add-cache = %d: %s", code, raw)
	}
	_, body = overviewPage(t, srv, "/inbox")
	wantNotOnPage(t, "Needs you after the proposal was rejected", body, "Proposals to decide", "add-cache")
}

// TestNeedsYouWithNothing: an empty list says what would appear in it.
func TestNeedsYouWithNothing(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	whole, body := overviewPage(t, srv, "/inbox")
	wantOnPage(t, "Needs you", body, "Nothing waits for you.", "A blocked change, a question from an agent")
	wantNotOnPage(t, "Needs you", body, "Blocked changes", "Proposals to decide", "data-post")
	if !strings.Contains(whole, `data-live-interval="15"`) {
		t.Errorf("Needs you does not keep itself up to date")
	}
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/inbox", nil)
	if code != http.StatusOK || data["count"] != float64(0) || data["urgent"] != false || data["blocked"] == nil || data["lost_runs"] == nil || data["proposals"] == nil {
		t.Errorf("GET /api/inbox of a workspace with nothing = %d %s", code, raw)
	}
}

// TestTheListOfChanges: the list shows the state the database gives each change, and its filter, its
// search and its total cover every change.
func TestTheListOfChanges(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)

	_, body := overviewPage(t, srv, "/changes")
	wantOnPage(t, "the list of changes", body,
		"Changes", `href="/changes/add-api"`, `href="/changes/add-ui"`, `href="/changes/add-search"`, `href="/changes/add-billing"`, `href="/changes/add-export"`,
		"rebuild requested", "building", "blocked", "queued", "gate failure", "critical", "high", "Full-text search",
		"1–5 of 5 changes",
		// The states some change is in, each with its count.
		`href="/changes?state=building"`, `href="/changes?state=blocked"`, `href="/changes?state=queued"`, `href="/changes?state=rebuild_requested"`,
	)
	// A row is a link to the story of its change: it has no action of its own.
	wantNotOnPage(t, "the list of changes", body, "data-post", "Intervene", `href="/changes?state=integrated"`, `href="/changes?state=ready"`, "Previous", "Next")
	for state, n := range map[string]int{"building": 2, "blocked": 1, "queued": 1, "rebuild_requested": 1} {
		tab := regexp.MustCompile(`href="/changes\?state=` + state + `"[^>]*>[^<]*<span[^>]*>(\d+)</span>`).FindStringSubmatch(body)
		if tab == nil || tab[1] != fmt.Sprint(n) {
			t.Errorf("the tab of %s counts %v, want %d", state, tab, n)
		}
	}

	// One state, and a search: both are part of the address.
	_, body = overviewPage(t, srv, "/changes?state=blocked")
	wantOnPage(t, "the blocked changes", body, `href="/changes/add-billing"`, "1–1 of 1 change", `aria-selected="true"`)
	wantNotOnPage(t, "the blocked changes", body, `href="/changes/add-api"`, `href="/changes/add-search"`)
	_, body = overviewPage(t, srv, "/changes?q=search")
	wantOnPage(t, "the changes that match a search", body, `href="/changes/add-search"`, `value="search"`, "1–1 of 1 change", "Clear the search")
	wantNotOnPage(t, "the changes that match a search", body, `href="/changes/add-billing"`)
	_, body = overviewPage(t, srv, "/changes?q=no-such-thing")
	wantOnPage(t, "a search that matches nothing", body, "No change matches your search.")
	_, body = overviewPage(t, srv, "/changes?state=merging")
	wantOnPage(t, "a state no change is in", body, "No change is in this state.")

	// What an address cannot ask for is a problem of the request, said as one.
	for _, path := range []string{"/changes?state=active", "/changes?page=0", "/changes?page=x"} {
		code, page, _ := getPage(t, srv, path)
		if code != http.StatusBadRequest || strings.Contains(page, `href="/changes/add-api"`) {
			t.Errorf("GET %s = %d, want 400 and no list", path, code)
		}
	}

	// The API returns the page, with the counts of the filters.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/changes?state=building", nil)
	items, _ := data["items"].([]any)
	counts, _ := data["counts"].(map[string]any)
	if code != http.StatusOK || len(items) != 2 || data["total"] != float64(2) || data["page"] != float64(1) || data["state"] != "building" ||
		counts["building"] != float64(2) || counts["blocked"] != float64(1) || counts["integrated"] != float64(0) {
		t.Fatalf("GET /api/changes?state=building = %d %s", code, raw)
	}
	if first, _ := items[0].(map[string]any); first["state"] != "building" || first["change_name"] == nil || first["last_activity_at"] == nil {
		t.Errorf("a change of /api/changes = %v", items[0])
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/changes?state=nope", nil)
	if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
		t.Errorf("GET /api/changes?state=nope = %d %s", code, raw)
	}
}

// TestTheListOfChangesPages: more changes than a page holds are reached page by page, and each
// change is on one page.
func TestTheListOfChangesPages(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	var backlog []map[string]any
	for i := 1; i <= changesPerPage+7; i++ {
		backlog = append(backlog, map[string]any{"change_name": fmt.Sprintf("add-part-%02d", i), "description": "One part", "queued_at": fmt.Sprintf("2026-11-01T10:%02d:00Z", i)})
	}
	if err := cycle.WriteJSONList(filepath.Join(root, cycle.BacklogFile), backlog); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncJSONToSQLite(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	link := regexp.MustCompile(`href="/changes/(add-part-\d+)"`)
	seen := map[string]int{}
	_, first := overviewPage(t, srv, "/changes")
	wantOnPage(t, "the first page of the changes", first, fmt.Sprintf("1–%d of %d changes", changesPerPage, changesPerPage+7), `href="/changes?page=2"`, "Next")
	wantNotOnPage(t, "the first page of the changes", first, "Previous")
	_, second := overviewPage(t, srv, "/changes?page=2")
	wantOnPage(t, "the second page of the changes", second, fmt.Sprintf("%d–%d of %d changes", changesPerPage+1, changesPerPage+7, changesPerPage+7), `href="/changes"`, "Previous")
	wantNotOnPage(t, "the second page of the changes", second, "Next", "page=3")
	for _, body := range []string{first, second} {
		for _, m := range link.FindAllStringSubmatch(body, -1) {
			seen[m[1]]++
		}
	}
	if len(seen) != changesPerPage+7 {
		t.Errorf("the two pages list %d changes, want %d", len(seen), changesPerPage+7)
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("%s is listed %d times", name, n)
		}
	}
	// A page past the end is the last one.
	_, past := overviewPage(t, srv, "/changes?page=9")
	wantOnPage(t, "a page past the end", past, fmt.Sprintf("%d–%d of %d changes", changesPerPage+1, changesPerPage+7, changesPerPage+7))
}

// TestABlockForRiskOffersTheApprovalFirst: a change blocked for its risk is lifted by accepting that
// risk, so that is the first decision of its row and the action of its page. A block for any other
// reason offers to unblock first.
func TestABlockForRiskOffersTheApprovalFirst(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	ctx := context.Background()
	if err := cycle.WriteJSONList(filepath.Join(root, cycle.BacklogFile), []map[string]any{
		{"change_name": "add-login-limit", "description": "Limit the attempts to log in"},
		{"change_name": "add-report", "description": "A monthly report"},
	}); err != nil {
		t.Fatal(err)
	}
	srv.syncBacklog(ctx)
	if err := st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "add-login-limit", Reason: "risk_review", Details: "it changes how a session is kept"}); err != nil {
		t.Fatal(err)
	}
	if err := st.BlockChange(ctx, store.QuarantineRecord{ChangeName: "add-report", Reason: "gate_failure", Details: "lint failed"}); err != nil {
		t.Fatal(err)
	}
	risk := regexp.MustCompile(`data-post="/api/changes/([a-z-]+)/(risk-approval|unblock)"`)
	first := func(body, change string) string {
		for _, m := range risk.FindAllStringSubmatch(body, -1) {
			if m[1] == change {
				return m[2]
			}
		}
		return ""
	}

	_, body := overviewPage(t, srv, "/inbox")
	if got := first(body, "add-login-limit"); got != "risk-approval" {
		t.Errorf("the first decision offered for a change blocked for its risk is %q, want the approval of the risk", got)
	}
	if got := first(body, "add-report"); got != "unblock" {
		t.Errorf("the first decision offered for a change blocked by a gate is %q, want to unblock it", got)
	}

	// On the page of the change, that first decision is the action of the page.
	primary := regexp.MustCompile(`class="[^"]*btn-primary[^"]*" data-post="/api/changes/[a-z-]+/([a-z-]+)"`)
	for change, want := range map[string]string{"add-login-limit": "risk-approval", "add-report": "unblock"} {
		_, body := changePage(t, srv, "/changes/"+change)
		got := primary.FindAllStringSubmatch(body, -1)
		if len(got) != 1 || got[0][1] != want {
			t.Errorf("the action of the page of %s is %v, want %s alone", change, got, want)
		}
	}

	// Accepting the risk lifts the block that was for it.
	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/changes/add-login-limit/risk-approval", map[string]any{})
	if code != http.StatusOK || data["state"] == "blocked" || data["block"] != nil {
		t.Fatalf("accepting the risk of add-login-limit = %d %s", code, raw)
	}
	_, body = overviewPage(t, srv, "/inbox")
	wantNotOnPage(t, "Needs you after the risk was accepted", body, `href="/changes/add-login-limit"`)
	wantOnPage(t, "Needs you after the risk was accepted", body, `href="/changes/add-report"`)
}
