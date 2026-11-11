package portal

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

const searchSpec = `## Purpose
Full-text search over the widgets of the catalog, by the text of their names.

## ADDED Requirements

### Requirement: Search by text
The system SHALL return the widgets whose name contains the text a person typed.

#### Scenario: A match
- **WHEN** a person searches for "blue"
- **THEN** the list holds the widgets whose name contains "blue"
`

// changesSeed is what seedChanges recorded that a test asks for by id.
type changesSeed struct {
	failedRound int64
	mergeID     int64
}

// seedChanges records changes in the states the pages of a change show, on top of seedExecutions:
//
//	add-api     built with a repair, handed to the merger and sent back by a rebase conflict: it
//	            waits to be built again
//	add-ui      queued behind add-api, with which it shares a lock
//	add-search  being built in its worktree, with its agent idle
//	add-billing blocked by a gate, with a question an agent left for a person in its worktree
//	add-export  being built as far as its run says: nothing holds its lease
func seedChanges(t *testing.T, st *store.Store, root string) changesSeed {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		must(os.MkdirAll(filepath.Dir(path), 0o755))
		must(os.WriteFile(path, []byte(body), 0o644))
	}
	write(filepath.Join(root, ".foreman-kit", "agent_backlog.json"), `[
		{"change_name":"add-api","priority":"high","description":"The API of the widgets.\nIt lists them, creates them and deletes them.\nEvery route is covered by a contract test.\nThe fourth line of its description.","locks":"db-schema","queued_at":"2026-11-02T09:00:00Z"},
		{"change_name":"add-ui","description":"The UI of the widgets","depends_on":["add-api"],"locks":"db-schema, routes","queued_at":"2026-11-02T10:00:00Z"},
		{"change_name":"add-search","priority":"critical","description":"Full-text search","queued_at":"2026-11-03T09:00:00Z"},
		{"change_name":"add-billing","description":"Billing","queued_at":"2026-11-03T11:00:00Z"},
		{"change_name":"add-export","priority":"low","description":"CSV export","queued_at":"2026-11-03T12:00:00Z"}
	]`)
	must(st.SyncJSONToSQLite(ctx, root))
	seedExecutions(t, st)

	var seed changesSeed
	var err error
	must(st.View(ctx, func(v *store.View) error {
		build, err := v.Execution(ctx, "build-1")
		if err != nil {
			return err
		}
		if build == nil || len(build.GateRounds) != 1 || build.GateRounds[0].Result != "failed" {
			t.Fatalf("the seed has no failed round of gates after build-1: %+v", build)
		}
		seed.failedRound = build.GateRounds[0].ID
		return nil
	}))

	// add-api: the merger could not rebase it and asked for it to be built again.
	must(st.EnqueueReady(ctx, store.ReadyManifest{ChangeName: "add-api", Branch: "feat/spec-add-api", Worktree: "/tmp/worker-add-api",
		HeadSHA: "abc1234def5678", Capabilities: []string{"api", "lock:db-schema"}}))
	seed.mergeID, err = st.BeginMergeAttempt(ctx, "add-api")
	must(err)
	must(st.SetMergeAttemptState(ctx, seed.mergeID, "REBASING_CHILDREN"))
	must(st.FinishMergeAttempt(ctx, seed.mergeID, store.MergeSentBack, "CONFLICT (content): Merge conflict in api/widget.go"))
	must(st.CloseReady(ctx, "add-api", store.ManifestSentBack))
	_, err = st.RequestRebuild(ctx, "add-api", "feat/spec-add-api", "<<<<<<< HEAD\nfunc CreateWidget(name string)\n=======\nfunc CreateWidget(ctx context.Context)\n>>>>>>> feat/spec-add-api")
	must(err)

	// add-search: the worktree its worker builds in.
	search := filepath.Join(root, ".worktrees", "worker-add-search", "openspec", "changes", "add-search")
	write(filepath.Join(search, "proposal.md"), "# Proposal: add-search\n\n## Why\n\nPeople cannot find a widget by its name.\n")
	write(filepath.Join(search, "tasks.md"), "- [x] 1.1 Index the names\n- [ ] 1.2 Search them\n")
	write(filepath.Join(search, "specs", "search", "spec.md"), searchSpec)

	// add-billing: blocked after a run, and its agent asked a question.
	billingRun, err := st.BeginChangeRun(ctx, "add-billing", 2, filepath.Join(root, ".worktrees", "worker-add-billing"), "feat/spec-add-billing")
	must(err)
	must(st.BlockChange(store.WithChangeRun(ctx, billingRun, "add-billing"), store.QuarantineRecord{ChangeName: "add-billing", Reason: "gate_failure",
		Details: "coverage 91% < 100%", Stage: "verify"}))
	must(st.FinishChangeRun(ctx, billingRun, "blocked"))
	billing := filepath.Join(root, ".worktrees", "worker-add-billing", "openspec", "changes", "add-billing")
	write(filepath.Join(billing, "proposal.md"), "# Proposal: add-billing\n")
	write(filepath.Join(billing, ".needs-human"), "Which currency do the invoices use?\n")

	// add-export: a run nobody finished, and no lease.
	_, err = st.BeginChangeRun(ctx, "add-export", 3, "/tmp/worker-add-export", "feat/spec-add-export")
	must(err)
	return seed
}

func changePage(t *testing.T, srv http.Handler, path string) (whole, main string) {
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

// inOrder fails when the facts are not on the page in the order given.
func inOrder(t *testing.T, page, body string, facts ...string) {
	t.Helper()
	text := textOf(body)
	last := -1
	for _, fact := range facts {
		at := strings.Index(text, fact)
		if at < 0 {
			t.Errorf("%s does not show %q", page, fact)
			return
		}
		if at < last {
			t.Errorf("%s shows %q before what comes before it in the order %q", page, fact, facts)
			return
		}
		last = at
	}
}

// TestTheStoryOfAChangeIsMadeOfItsRows: the page of a change tells what happened to it from the rows
// of each fact, the latest first: its worker runs with their executions, gates and repairs, the
// attempts of the merger, and what the merger asked for. A change sent back by a rebase conflict
// says so, with the conflict.
func TestTheStoryOfAChangeIsMadeOfItsRows(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seed := seedChanges(t, st, root)

	_, body := changePage(t, srv, "/changes/add-api")
	wantOnPage(t, "the page of add-api", body,
		// What it is now, and what that means.
		"add-api", "rebuild requested", "The merger sent it back: the dispatcher builds it again on its next pass.", "high priority",
		"The API of the widgets.", "Show all of it",
		// What it declares.
		"Needed by", `href="/changes/add-ui"`, "queued", "db-schema", "1 worker run · 3 executions · 1 repair",
		`href="/executions?change=add-api"`,
		// What is open for it.
		"for it to be built again", "no worker has taken it yet",
		// The merger: what it asked for, and the attempt that led to it.
		"The merger asked to build it again", "not taken yet", "func CreateWidget(ctx context.Context)",
		"Merge attempt #"+strconv.FormatInt(seed.mergeID, 10), "sent back to be rebuilt", "CONFLICT (content): Merge conflict in api/widget.go",
		"It got as far as rebasing the repositories.",
		"Handed to the merger", "abc1234def", "lock:db-schema",
		// The worker run, by its executions, each with what followed from it.
		"Worker run #1", "ready to merge", "from the backlog", "Built, and every gate passed.",
		`href="/executions/plan-1"`, `href="/executions/build-1"`, `href="/executions/repair-1"`,
		"claude showed no activity for", "stopped by the idle watchdog",
		`href="/gates/`+strconv.FormatInt(seed.failedRound, 10)+`"`, "backend · unit-tests", "failed",
		"Repair round 1, asked because of gate failure", "the gates passed afterwards",
	)
	// The latest first; inside a run, in the order it ran.
	inOrder(t, "the story of add-api", body, "The merger asked to build it again", "Merge attempt #", "Handed to the merger", "Worker run #1")
	inOrder(t, "the worker run of add-api", body, "Worker run #1", "PLAN", "BUILD", "Gates #"+strconv.FormatInt(seed.failedRound, 10), "BUILD repair 1", "Repair round 1")
	wantNotOnPage(t, "the page of add-api", body, "It waits for you", "Its worker is gone", `/unblock"`, "Intervene")
	if n := strings.Count(body, `aria-current="step"`); n != 1 || !strings.Contains(body, "again</span>") {
		t.Errorf("the rail of a change to be built again marks %d stage(s), want Building, marked as again", n)
	}

	// The story pages by cursor: every episode once, whatever the size of a page.
	var kinds []string
	path := "/changes/add-api?limit=1"
	for page := 0; page < 10 && path != ""; page++ {
		_, body := changePage(t, srv, path)
		for _, id := range regexp.MustCompile(`<article id="([a-z]+)-`).FindAllStringSubmatch(body, -1) {
			kinds = append(kinds, id[1])
		}
		next := regexp.MustCompile(`href="(/changes/add-api\?[^"]*before=[^"]+)"`).FindStringSubmatch(body)
		path = ""
		if next != nil {
			path = strings.ReplaceAll(next[1], "&amp;", "&")
		}
	}
	if got := strings.Join(kinds, " "); got != "rebuild merge manifest run" {
		t.Errorf("the story read one episode a page = [%s], want the four episodes once each, the latest first", got)
	}

	// Its mirror in the API: the change and the same episodes.
	code, api, raw, _ := doReq(t, srv, http.MethodGet, "/api/changes/add-api", nil)
	change, _ := api["change"].(map[string]any)
	items, _ := api["items"].([]any)
	if code != http.StatusOK || change["state"] != "rebuild_requested" || change["has_worktree"] != false || len(items) != 4 {
		t.Fatalf("GET /api/changes/add-api = %d: %s", code, raw)
	}
	var apiKinds []string
	for _, it := range items {
		apiKinds = append(apiKinds, asStr(it.(map[string]any)["kind"]))
	}
	rebuild, _ := change["rebuild"].(map[string]any)
	neededBy, _ := change["needed_by"].([]any)
	if strings.Join(apiKinds, " ") != "rebuild merge manifest run" || rebuild == nil || len(neededBy) != 1 {
		t.Errorf("GET /api/changes/add-api: episodes %v, rebuild %v, needed by %v", apiKinds, rebuild, neededBy)
	}
	run, _ := items[3].(map[string]any)["run"].(map[string]any)
	if execs, _ := run["executions"].([]any); len(execs) != 3 {
		t.Errorf("the worker run in the API has %d executions, want 3: %v", len(execs), run)
	}
	_, api, _, _ = doReq(t, srv, http.MethodGet, "/api/changes/add-api?limit=2", nil)
	if items, _ := api["items"].([]any); len(items) != 2 || asStr(api["next"]) == "" {
		t.Errorf("a page of two episodes = %d episodes and the cursor %q", len(items), asStr(api["next"]))
	}
	for _, bad := range []string{"/api/changes/add-api?before=nonsense", "/api/changes/add-api?limit=0"} {
		if code, body, _, _ := doReq(t, srv, http.MethodGet, bad, nil); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d %v, want 400", bad, code, body)
		}
	}
	code, api, _, _ = doReq(t, srv, http.MethodGet, "/api/changes/no-such-change", nil)
	if c, _ := errorOf(api); code != http.StatusNotFound || c != "not_found" {
		t.Errorf("GET /api/changes/no-such-change = %d %v", code, api)
	}
	if code, page, _ := getPage(t, srv, "/changes/no-such-change"); code != http.StatusNotFound || !strings.Contains(page, "no record of a change named no-such-change") {
		t.Errorf("GET /changes/no-such-change = %d, want the page that says the kit has no record of it", code)
	}
}

// TestAChangeSaysWhatItWaitsFor: what stops a change is at the top of its page, with the action that
// answers it: the block and the question of an agent, the worker that is gone, the idle agent, the
// change it waits for.
func TestAChangeSaysWhatItWaitsFor(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedChanges(t, st, root)

	// Blocked, and asked a question.
	whole, body := changePage(t, srv, "/changes/add-billing")
	wantOnPage(t, "the page of add-billing", body,
		"blocked", "Stopped until a person decides: nothing runs for it.",
		"It waits for you: gate failure", "at the verify stage", "coverage 91% < 100%",
		"Question for a person:", "Which currency do the invoices use?",
		`data-post="/api/changes/add-billing/unblock"`, `{"reset_retries":false}`, `{"reset_retries":true}`,
		`data-post="/api/changes/add-billing/risk-approval"`,
		"Blocked: gate failure", "in force", "Worker run #", `href="/changes/add-billing/intervene"`,
	)
	if n := strings.Count(body, "btn-primary"); n != 1 {
		t.Errorf("the page of a blocked change has %d primary actions, want one: Unblock", n)
	}
	if strings.Contains(whole, "data-live-interval") {
		t.Errorf("the page of a change nothing runs for refreshes itself")
	}
	if !regexp.MustCompile(`aria-current="step"[^>]*>(?s:.*?)Building(?s:.*?)blocked</span>`).MatchString(body) {
		t.Errorf("the rail of a change blocked while it was built does not mark Building as blocked")
	}

	// Its worker is gone.
	_, body = changePage(t, srv, "/changes/add-export")
	wantOnPage(t, "the page of add-export", body, "building", "Its worker is gone", "nothing holds the lease of the change", `data-post="/api/changes/add-export/reclaim"`, "low priority")
	wantNotOnPage(t, "the page of add-export", body, "It waits for you")

	// Being built, with its agent idle: the page keeps itself up to date.
	whole, body = changePage(t, srv, "/changes/add-search")
	wantOnPage(t, "the page of add-search", body, "building", "A worker is planning or building it now.", "critical priority",
		"Its agent, claude, shows no activity.", "no activity for", `href="/executions/build-live"`, "Worker run #", "running",
		"idle now, stopped at 20m 00s", `href="/changes/add-search/intervene"`)
	if !strings.Contains(whole, `data-live-interval="10"`) || !strings.Contains(body, `id="change-story" data-live-region`) {
		t.Errorf("the page of a change being built does not keep itself up to date")
	}

	// Queued behind another change.
	_, body = changePage(t, srv, "/changes/add-ui")
	wantOnPage(t, "the page of add-ui", body, "queued", "In the backlog with nothing open", "Depends on", `href="/changes/add-api"`, "rebuild requested",
		"db-schema", "routes", "Nothing has happened to this change yet.")
	// The change it waits for is named with a link to it, and with the state it is in.
	if !regexp.MustCompile(`It waits for\s+add-api\s*, which is\s+rebuild requested`).MatchString(textOf(body)) {
		t.Errorf("the page of add-ui does not say that it waits for add-api, and in which state that one is")
	}
	wantNotOnPage(t, "the page of add-ui", body, "Intervene", "It waits for you", "It declares the lock db-schema, and so does")
	if n := strings.Count(body, "btn-primary"); n != 0 {
		t.Errorf("the page of a queued change has %d primary actions, want none", n)
	}
}

// TestTheFilesEventsAndWorktreeOfAChange: the other pages of a change.
func TestTheFilesEventsAndWorktreeOfAChange(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedChanges(t, st, root)

	// Files: the OpenSpec files of the change, from where they are now.
	_, body := changePage(t, srv, "/changes/add-search/files")
	wantOnPage(t, "the files of add-search", body, "Read from the worktree of the change", "proposal.md", "tasks.md", "specs/search/spec.md",
		`href="/changes/add-search/files/specs/search/spec.md"`, "People cannot find a widget by its name.", `aria-current="page"`)
	_, body = changePage(t, srv, "/changes/add-search/files/specs/search/spec.md")
	wantOnPage(t, "a spec of add-search", body, "The system SHALL return the widgets whose name contains the text a person typed.")
	_, body = changePage(t, srv, "/changes/add-ui/files")
	wantOnPage(t, "the files of a change with none", body, "This change has no OpenSpec files on disk.")
	for _, outside := range []string{
		"/changes/add-search/files/..%2F..%2F..%2F..%2F..%2Fconfig%2Fagents.yaml",
		"/changes/add-search/files/specs%2F..%2F..%2Fadd-billing%2Fproposal.md",
		"/changes/add-search/files/not-a-file.md",
	} {
		if code, page, _ := getPage(t, srv, outside); code != http.StatusNotFound || strings.Contains(page, "agents:") {
			t.Errorf("GET %s = %d, want 404: a page shows only the files of its change", outside, code)
		}
	}
	code, api, raw, _ := doReq(t, srv, http.MethodGet, "/api/changes/add-search/files/tasks.md", nil)
	if code != http.StatusOK || api["name"] != "tasks.md" || api["source"] != "worktree" || !strings.Contains(asStr(api["content"]), "1.2 Search them") || api["truncated"] != false {
		t.Errorf("GET /api/changes/add-search/files/tasks.md = %d: %s", code, raw)
	}
	if code, _, _, _ := doReq(t, srv, http.MethodGet, "/api/changes/add-search/files/..%2Ftasks.md", nil); code != http.StatusNotFound {
		t.Errorf("a file outside the list of the change = %d, want 404", code)
	}

	// Events: what was recorded about the change, filtered and paged where it is stored.
	_, body = changePage(t, srv, "/changes/add-api/events")
	wantOnPage(t, "the events of add-api", body, "agent.exit", "agent.idle", "agent.stalled", `href="/executions/build-1"`, "Every type (")
	_, body = changePage(t, srv, "/changes/add-api/events?event=agent.idle")
	wantOnPage(t, "the idle events of add-api", body, `<option value="agent.idle" selected>`, "has shown no activity")
	wantNotOnPage(t, "the idle events of add-api", body, "The agent finished its prompt.")
	_, body = changePage(t, srv, "/changes/add-api/events?event=no.such.event")
	wantOnPage(t, "events of a type the change has none of", body, "No event of this type.")
	code, api, raw, _ = doReq(t, srv, http.MethodGet, "/api/changes/add-api/events?limit=2", nil)
	events, _ := api["items"].([]any)
	if code != http.StatusOK || len(events) != 2 || asStr(api["next"]) == "" {
		t.Fatalf("GET /api/changes/add-api/events?limit=2 = %d: %s", code, raw)
	}
	seen := map[float64]bool{}
	cursor := ""
	for page := 0; page < 50; page++ {
		_, api, _, _ := doReq(t, srv, http.MethodGet, "/api/changes/add-api/events?limit=2&before="+url.QueryEscape(cursor), nil)
		items, _ := api["items"].([]any)
		for _, it := range items {
			e := it.(map[string]any)
			id, _ := e["id"].(float64)
			if seen[id] || e["change"] != "add-api" {
				t.Fatalf("the events of add-api by cursor repeat %v or are of another change: %v", id, e)
			}
			seen[id] = true
		}
		if cursor = asStr(api["next"]); cursor == "" {
			break
		}
	}
	all, err := st.QueryEvents(context.Background(), store.EventFilter{ChangeName: "add-api"})
	if err != nil || len(seen) != len(all) || len(all) < 5 {
		t.Errorf("paging reached %d events of add-api and the store has %d (%v)", len(seen), len(all), err)
	}

	// Intervene: what a person can do in the worktree.
	_, body = changePage(t, srv, "/changes/add-search/intervene")
	wantOnPage(t, "intervene in add-search", body, `method="post"`, `data-api="/api/changes/add-search/prompts"`, `name="engine"`, `name="prompt"`,
		`<option value="claude" selected>`, `data-post="/api/changes/add-search/validate"`, `data-post="/api/changes/add-search/gates"`,
		`data-post="/api/changes/add-search/reset"`, `data-danger="1"`, `href="/changes/add-search/intervene?from=plan"`, "No prompt was sent from the portal to this worktree.")
	_, body = changePage(t, srv, "/changes/add-search/intervene?from=plan")
	wantOnPage(t, "intervene from the PLAN prompt", body, "the PLAN prompt a worker would get now", "add-search")
	_, body = changePage(t, srv, "/changes/add-search/intervene?from=build-live")
	wantOnPage(t, "intervene from the prompt of an execution", body, "the prompt of an earlier execution", "Implement add-search.")
	if code, _, _ := getPage(t, srv, "/changes/add-search/intervene?from=plan-1"); code != http.StatusNotFound {
		t.Errorf("a prompt taken from the execution of another change = %d, want 404", code)
	}
	_, body = changePage(t, srv, "/changes/add-ui/intervene")
	wantOnPage(t, "intervene in a change with no worktree", body, "This change has no worktree now", `href="/executions/new"`)
	wantNotOnPage(t, "intervene in a change with no worktree", body, `data-api="/api/changes/add-ui/prompts"`, `/reset"`)
}

// TestARoundOfGatesHasItsPage: the story of a change and the page of an execution name a round of
// gates in a line; its page has the result of each gate, with the command it ran and how it failed.
func TestARoundOfGatesHasItsPage(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seed := seedChanges(t, st, root)
	id := strconv.FormatInt(seed.failedRound, 10)

	_, body := changePage(t, srv, "/gates/"+id)
	wantOnPage(t, "the page of a round of gates", body, "Gates #"+id+" · add-api", "verification by the worker", "failed", "1 gate, 1 failed",
		"backend · unit-tests", "go test ./...", "exit 1", "--- FAIL: TestCreateWidget",
		`href="/changes/add-api"`, `href="/executions/build-1"`, `href="/changes/add-api#run-1"`)

	code, api, raw, _ := doReq(t, srv, http.MethodGet, "/api/gates/"+id, nil)
	runs, _ := api["runs"].([]any)
	if code != http.StatusOK || api["result"] != "failed" || api["after_exec_id"] != "build-1" || api["failed"] != float64(1) || len(runs) != 1 ||
		runs[0].(map[string]any)["gate_name"] != "unit-tests" {
		t.Errorf("GET /api/gates/%s = %d: %s", id, code, raw)
	}
	for _, missing := range []string{"/gates/987654", "/gates/first", "/gates/0", "/gates/-1"} {
		if code, _, _ := getPage(t, srv, missing); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", missing, code)
		}
	}
	if code, body, _, _ := doReq(t, srv, http.MethodGet, "/api/gates/987654", nil); code != http.StatusNotFound {
		t.Errorf("GET /api/gates/987654 = %d %v, want 404", code, body)
	}

	// The page of the execution the round verified links to it.
	_, body = changePage(t, srv, "/executions/build-1")
	wantOnPage(t, "the page of build-1", body, `href="/gates/`+id+`"`, `href="/changes/add-api#run-1"`)
}
