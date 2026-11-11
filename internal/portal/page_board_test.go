package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/store"
)

// seedBoard adds to the overview and to the work of the queue (seedOverview, seedWork) what the
// board draws besides:
//
//	add-feed          a proposal a person approved, which waits for a cycle to queue it
//	tune-plan-prompt  a proposal to edit a prompt, which is not a change
//	add-archive       a change of the backlog that nothing happened to for three days
//	add-legacy        a change that was abandoned
//	add-orphan        a change with records that the backlog does not list
//	add-login         a change integrated two days ago
//	add-base          a change integrated ten days ago
//
// and the tasks of add-billing, in the worktree it was planned in: five, two of them done, under
// two headings. add-search already has two, one of them done.
func seedBoard(t *testing.T, srv *Server, st *store.Store, root string) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ago := func(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }
	const day = 24 * time.Hour

	must(cycle.WriteProposals(root, []map[string]any{
		{"change_name": "add-cache", "status": "proposed", "priority": "high", "source": "exploration", "description": "Cache the feed of widgets", "proposed_at": "2026-11-04T08:00:00Z"},
		{"change_name": "add-feed", "status": "approved", "description": "An RSS feed of the widgets", "proposed_at": "2026-11-05T08:00:00Z",
			"review": map[string]any{"by": "evaluator", "status": "passed"}},
		{"change_name": "tune-plan-prompt", "kind": "prompt", "prompt": "plan", "status": "proposed", "description": "Say the acceptance first", "proposed_at": "2026-11-06T08:00:00Z"},
		{"change_name": "add-old", "status": "rejected", "description": "An old idea", "proposed_at": "2026-10-01T09:00:00Z"},
	}))

	backlog := filepath.Join(root, cycle.BacklogFile)
	items, err := cycle.ReadJSONList(backlog)
	must(err)
	items = append(items, map[string]any{"change_name": "add-archive", "priority": "low", "description": "Archive the widgets nobody opens", "queued_at": ago(3 * day)})
	must(cycle.WriteJSONList(backlog, items))

	history, err := json.Marshal([]map[string]any{
		{"change_name": "add-login", "integrated_at": ago(2 * day), "commit": "c0ffee1234567890", "description": "Sign in with a password"},
		{"change_name": "add-base", "integrated_at": ago(10 * day), "commit": "deadbeef12345678", "description": "The base of the application"},
	})
	must(err)
	must(os.WriteFile(filepath.Join(root, ".foreman-kit", "agent_history.json"), history, 0o644))

	tasks := filepath.Join(root, ".worktrees", "worker-add-billing", "openspec", "changes", "add-billing", "tasks.md")
	must(os.WriteFile(tasks, []byte(billingTasks), 0o644))

	srv.syncBacklog(ctx)
	must(st.SetChangeFirstSeen(ctx, "add-archive", time.Now().Add(-3*day)))
	must(st.MarkChangeDiscarded(ctx, "add-legacy"))
	must(st.EmitEvent(ctx, store.EventInput{Event: "change.start", Change: "add-orphan"}))
}

// seedTwin proposes again a change the kit has records of and the backlog does not list,
// add-orphan: the board then has two cards of that name, one of each kind.
func seedTwin(t *testing.T, srv *Server, root string) {
	t.Helper()
	proposals, err := cycle.ReadJSONList(filepath.Join(root, cycle.ProposalsFile))
	if err != nil {
		t.Fatal(err)
	}
	proposals = append(proposals, map[string]any{"change_name": "add-orphan", "status": "proposed", "description": "The orphan, proposed again", "proposed_at": "2026-11-07T08:00:00Z"})
	if err := cycle.WriteProposals(root, proposals); err != nil {
		t.Fatal(err)
	}
	srv.syncBacklog(context.Background())
}

// billingTasks is the tasks.md of add-billing: tasks under two headings, written the ways a plan
// writes them, and lines that are not tasks.
const billingTasks = `# Tasks

## 1. Invoices

- [x] 1.1 Model the invoice
- [X] 1.2 Number the invoices in ` + "`invoice.Number`" + `
- [ ] 1.3 Send the invoice by mail
  * [ ] 1.3.1 Render it as a PDF

A paragraph that mentions a box [ ] is not a task.

## 2. Coverage ##

1. [ ] Cover the totals with tests
`

// laneOf returns the cards of a lane of the board, as the page draws them.
func laneOf(t *testing.T, body, key string) string {
	t.Helper()
	start := strings.Index(body, `id="lane-`+key+`-cards"`)
	if start < 0 {
		t.Fatalf("the board has no lane %s:\n%s", key, textOf(body))
	}
	rest := body[start:]
	if end := strings.Index(rest[1:], `id="lane-`); end >= 0 {
		// The next lane begins with its own element, whose id comes before that of its cards.
		rest = rest[:end+1]
	}
	return rest
}

var cardNameRe = regexp.MustCompile(`data-card="([^"]+)"`)

// cardsOf lists the cards of a lane, in the order the page draws them.
func cardsOf(t *testing.T, body, key string) []string {
	t.Helper()
	var names []string
	for _, m := range cardNameRe.FindAllStringSubmatch(laneOf(t, body, key), -1) {
		names = append(names, m[1])
	}
	return names
}

// cardOf returns one card of the board.
func cardOf(t *testing.T, body, name string) string {
	t.Helper()
	start := strings.Index(body, `data-card="`+name+`"`)
	if start < 0 {
		t.Fatalf("the board has no card %s", name)
	}
	// Up to the next card, or to the end of its lane: a card holds lists of its own.
	rest := body[start:]
	for _, next := range []string{`data-card="`, `id="lane-`} {
		if end := strings.Index(rest[1:], next); end >= 0 {
			rest = rest[:end+1]
		}
	}
	return rest
}

// TestTheBoardPutsEachChangeInTheLaneOfItsState: the board has seven lanes, and the card of a
// change is in the lane its state puts it in, once. A change the backlog does not list is on the
// board all the same while something is open for it. A card says what its lane is about, and has a
// button for each lane a person can move it to, and none when only the kit moves it.
func TestTheBoardPutsEachChangeInTheLaneOfItsState(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	seedWork(t, st, root)
	seedBoard(t, srv, st, root)

	whole, body := overviewPage(t, srv, "/board")
	// The two lanes at the ends are folded until the address opens them.
	_, opened := overviewPage(t, srv, "/board?open=discarded,integrated")
	var lanes []string
	for _, m := range regexp.MustCompile(`data-lane="([a-z]+)"`).FindAllStringSubmatch(body, -1) {
		lanes = append(lanes, m[1])
	}
	if got := strings.Join(lanes, " "); got != "discarded proposals backlog building blocked merge integrated" {
		t.Errorf("the lanes of the board = %s", got)
	}
	wantOnPage(t, "the titles of the lanes", body, "Discarded", "Proposals", "Backlog", "Building", "Blocked", "Merge queue", "Integrated")
	for lane, want := range map[string][]string{
		// The proposal that waits for a decision first; the one to edit a prompt is not a change.
		laneProposals: {"add-cache", "add-feed"},
		// By priority, and then by how long each has been queued.
		laneBacklog:  {"add-api", "add-ui", "add-archive"},
		laneBuilding: {"add-search", "add-export"},
		laneBlocked:  {"add-billing"},
		// The one being merged, and then the one that waits. Neither is in the backlog file.
		laneMerge: {"add-cart", "add-wishlist"},
	} {
		if got := cardsOf(t, body, lane); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("the cards of %s = %v, want %v", lane, got, want)
		}
		if got := cardsOf(t, opened, lane); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("with the folded lanes open, the cards of %s = %v, want %v", lane, got, want)
		}
	}
	for lane, want := range map[string][]string{
		laneDiscarded: {"add-orphan", "add-legacy", "add-old"},
		// The change integrated ten days ago is not of the last seven.
		laneIntegrated: {"add-login"},
	} {
		if got := cardsOf(t, opened, lane); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("the cards of %s = %v, want %v", lane, got, want)
		}
		if strings.Contains(body, `id="lane-`+lane+`-cards"`) {
			t.Errorf("the lane %s shows its cards before it is opened", lane)
		}
	}
	for _, name := range []string{"add-api", "add-ui", "add-search", "add-export", "add-billing", "add-cart", "add-wishlist", "add-cache"} {
		if n := strings.Count(body, `data-card="`+name+`"`); n != 1 {
			t.Errorf("the board has %d cards of %s, want one", n, name)
		}
	}
	wantNotOnPage(t, "the board", body, "tune-plan-prompt", "add-base")

	// Each lane says how many cards it has, and the two at the ends are folded.
	for lane, n := range map[string]string{laneDiscarded: "3", laneProposals: "2", laneBacklog: "3", laneBuilding: "2", laneBlocked: "1", laneMerge: "2", laneIntegrated: "1"} {
		if !regexp.MustCompile(`id="lane-` + lane + `-count"[^>]*>` + n + `<`).MatchString(body) {
			t.Errorf("the lane %s does not say it has %s cards", lane, n)
		}
	}
	// A folded lane is opened by a link, and folded again by another: the address says which are
	// open, so the refresh of the page leaves them as they are.
	wantOnPage(t, "the folded lanes", body, `href="/board?open=discarded"`, `href="/board?open=integrated"`)
	wantOnPage(t, "the opened lanes", opened, `href="/board?open=integrated"`, `href="/board?open=discarded"`, "Fold",
		"Abandoned changes, rejected proposals, and changes the backlog no longer lists.", "The changes integrated in the last 7 days.")
	if code, _, _ := getPage(t, srv, "/board?open=backlog"); code != http.StatusBadRequest {
		t.Errorf("GET /board?open=backlog = %d, want 400: Backlog is not a lane that folds", code)
	}

	// What each card says.
	wantOnPage(t, "the card of a proposal that waits", cardOf(t, body, "add-cache"), "waits for a decision", "high", `href="/backlog/proposals?status=proposed"`)
	wantOnPage(t, "the card of an approved proposal", cardOf(t, body, "add-feed"), "approved", "A cycle queues it while the queue has room.", "The evaluator:")
	wantOnPage(t, "the card of a change to build again", cardOf(t, body, "add-api"), "rebuild requested", "high", `href="/changes/add-api"`)
	wantOnPage(t, "the card of a change that waits for another", cardOf(t, body, "add-ui"), "It depends on", "add-api")
	wantOnPage(t, "the card of a change nothing happened to", cardOf(t, body, "add-archive"), "Nothing happened to it for", "3d 0h")
	wantNotOnPage(t, "the card of a change queued today", cardOf(t, body, "add-ui"), "Nothing happened to it for")
	wantOnPage(t, "the card of a change being built", cardOf(t, body, "add-search"), "BUILD", "claude", "No activity for", "Question for a person:", "Is the search case-sensitive?")
	wantOnPage(t, "the card of a change whose worker is gone", cardOf(t, body, "add-export"), "Its worker is gone")
	wantOnPage(t, "the card of a blocked change", cardOf(t, body, "add-billing"), "gate failure", "The kit retried it 0 times and rebuilt it 0 times before.", "Which currency do the invoices use?")
	wantOnPage(t, "the card of a change being merged", cardOf(t, body, "add-cart"), "running the gates", "attempt #")
	wantOnPage(t, "the card of a change that waits for the merger", cardOf(t, body, "add-wishlist"), "It waits for the merger since")
	wantOnPage(t, "the card of an integrated change", cardOf(t, opened, "add-login"), "Integrated", "c0ffee1234")
	wantOnPage(t, "the card of a change the backlog does not list", cardOf(t, opened, "add-orphan"), "not in the backlog")
	wantOnPage(t, "the card of an abandoned change", cardOf(t, opened, "add-legacy"), "discarded")

	// The tasks of a change that was planned: how many are done, as a bar, and each with its box.
	search := cardOf(t, body, "add-search")
	wantOnPage(t, "the tasks of a change being built", search, "1 of 2 tasks", `value="1" max="2"`, "Done: 1.1 Index the names", "To do: 1.2 Search them")
	if !strings.Contains(search, `<details id="tasks-add-search" class="group/tasks" open>`) {
		t.Errorf("the tasks of a change being built are not open:\n%s", search)
	}
	billing := cardOf(t, body, "add-billing")
	wantOnPage(t, "the tasks of a blocked change", billing, "2 of 5 tasks", `value="2" max="5"`,
		"1. Invoices", "Done: 1.1 Model the invoice", "Done: 1.2 Number the invoices in invoice.Number", `<code class="font-mono">invoice.Number</code>`, "To do: 1.3 Send the invoice by mail", "To do: 1.3.1 Render it as a PDF",
		"2. Coverage", "To do: Cover the totals with tests")
	wantNotOnPage(t, "the tasks of a blocked change", billing, "A paragraph that mentions a box", "## ", "Coverage ##")
	if !strings.Contains(billing, `<details id="tasks-add-billing" class="group/tasks">`) {
		t.Errorf("the tasks of a change nobody builds start open:\n%s", billing)
	}
	// A change that was not planned has no tasks, and its card no bar.
	wantNotOnPage(t, "the card of a change that was not planned", cardOf(t, body, "add-ui"), "<progress", "tasks")

	// The buttons of a card: one for each lane a person can move it to.
	moves := func(name string) string {
		var out []string
		for _, m := range regexp.MustCompile(`data-move-to="([a-z]+)" data-post="([^"]+)"`).FindAllStringSubmatch(cardOf(t, opened, name), -1) {
			out = append(out, m[1]+" "+m[2])
		}
		return strings.Join(out, ", ")
	}
	for name, want := range map[string]string{
		"add-cache":    "backlog /api/proposals/add-cache/approve, discarded /api/proposals/add-cache/reject",
		"add-feed":     "discarded /api/proposals/add-feed/reject",
		"add-api":      "discarded /api/changes/add-api/abandon",
		"add-ui":       "discarded /api/changes/add-ui/abandon",
		"add-billing":  "backlog /api/changes/add-billing/unblock, discarded /api/changes/add-billing/abandon",
		"add-export":   "backlog /api/changes/add-export/reclaim",
		"add-wishlist": "discarded /api/changes/add-wishlist/abandon",
		// The kit alone moves these: a worker builds one, the merger integrates another, and the
		// rest are done.
		"add-search": "", "add-cart": "", "add-login": "", "add-legacy": "", "add-orphan": "", "add-old": "",
	} {
		if got := moves(name); got != want {
			t.Errorf("the moves of %s = %q, want %q", name, got, want)
		}
		card := cardOf(t, opened, name)
		if draggable := strings.Contains(card[:strings.Index(card, ">")], `draggable="true"`); draggable != (want != "") {
			t.Errorf("the card of %s is draggable: %v, and its moves are %q", name, draggable, want)
		}
	}
	// A move that is not undone asks first; the others act at once.
	for name, asks := range map[string]bool{"add-ui": true, "add-export": false} {
		if got := strings.Contains(cardOf(t, body, name), "data-confirm="); got != asks {
			t.Errorf("the move of %s asks before acting: %v, want %v", name, got, asks)
		}
	}
	wantOnPage(t, "the button that lifts a block", cardOf(t, body, "add-billing"), `data-body="{&#34;reset_retries&#34;:false}"`, `data-refresh="regions"`)
	wantOnPage(t, "the button that rejects a proposal", cardOf(t, body, "add-cache"), `data-confirm="Reject add-cache? It is not proposed again."`, `data-danger="1"`)
	wantOnPage(t, "the button that abandons a change", cardOf(t, body, "add-ui"), `data-confirm-title="Abandon this change?"`, `data-danger="1"`)

	// The page has no action of its own, keeps itself up to date, and the menu leads to it.
	if strings.Contains(body, "btn-primary") {
		t.Errorf("the board has a primary action")
	}
	if !strings.Contains(whole, `data-live-interval="10"`) {
		t.Errorf("the board does not keep itself up to date")
	}
	wantOnPage(t, "the menu", whole, `href="/board"`, "Board")
	wantOnPage(t, "the foot of the board", body, "for 24 hours")
}

// TestTheBoardOfAWorkspaceWithNothing: every lane says what would be in it.
func TestTheBoardOfAWorkspaceWithNothing(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	_, body := overviewPage(t, srv, "/board")
	wantOnPage(t, "an empty board", body, "The board has no card yet.", "Discarded", "Proposals", "Backlog", "Building", "Blocked", "Merge queue", "Integrated")
	if strings.Contains(body, "data-card=") || strings.Count(body, `data-lane="`) != 7 {
		t.Errorf("an empty board has a card, or not its seven lanes")
	}
	// A lane with no card is a strip: it has no room for cards until one comes.
	if strings.Contains(body, `-cards"`) {
		t.Errorf("an empty board draws the cards of a lane")
	}
	_, body = overviewPage(t, srv, "/board?open=integrated")
	wantOnPage(t, "an empty lane that was opened", body, "No change was integrated in these days.")
}

// TestTheBoardInTheAPI: /api/board returns the lanes with their cards and, for each card, the
// request that moves it to each lane a person can move it to.
func TestTheBoardInTheAPI(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	seedWork(t, st, root)
	seedBoard(t, srv, st, root)

	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/board", nil)
	lanes, _ := data["lanes"].([]any)
	if code != http.StatusOK || len(lanes) != 7 || data["stalled_after_seconds"] != float64(86400) || data["integrated_days"] != float64(7) {
		t.Fatalf("GET /api/board = %d %s", code, raw)
	}
	byKey := map[string]map[string]any{}
	var keys []string
	for _, it := range lanes {
		lane, _ := it.(map[string]any)
		byKey[asStr(lane["key"])] = lane
		keys = append(keys, asStr(lane["key"]))
	}
	if got := strings.Join(keys, " "); got != "discarded proposals backlog building blocked merge integrated" {
		t.Errorf("the lanes of /api/board = %s", got)
	}
	// Only two lanes take a card from a person, and the two at the ends are folded.
	for key, lane := range byKey {
		accepts, folded := key == laneBacklog || key == laneDiscarded, key == laneDiscarded || key == laneIntegrated
		if lane["accepts"] != accepts || lane["folded"] != folded {
			t.Errorf("the lane %s of /api/board: accepts %v, folded %v", key, lane["accepts"], lane["folded"])
		}
	}
	card := func(lane, name string) map[string]any {
		cards, _ := byKey[lane]["cards"].([]any)
		for _, it := range cards {
			if c, _ := it.(map[string]any); c["name"] == name {
				return c
			}
		}
		t.Fatalf("the lane %s of /api/board has no card %s: %s", lane, name, raw)
		return nil
	}
	billing := card(laneBlocked, "add-billing")
	moves, _ := billing["moves"].([]any)
	if billing["kind"] != "change" || billing["state"] != "blocked" || billing["urgent"] != true || len(moves) != 2 {
		t.Fatalf("the card of add-billing = %v", billing)
	}
	first, _ := moves[0].(map[string]any)
	if first["to"] != "backlog" || first["action"] != "unblock" || first["method"] != "POST" || first["href"] != "/api/changes/add-billing/unblock" || first["body"] != `{"reset_retries":false}` {
		t.Errorf("the first move of add-billing = %v", first)
	}
	if archive := card(laneBacklog, "add-archive"); archive["stalled_seconds"] == nil || archive["stalled_seconds"].(float64) < 3*86400-60 {
		t.Errorf("the card of add-archive = %v, want it stalled for three days", archive)
	}
	search := card(laneBuilding, "add-search")
	if search["run"] == nil || search["idle"] == nil || search["question"] != "Is the search case-sensitive?" {
		t.Errorf("the card of add-search = %v", search)
	}
	tasks, _ := search["tasks"].(map[string]any)
	items, _ := tasks["items"].([]any)
	if tasks["done"] != float64(1) || tasks["total"] != float64(2) || tasks["source"] != "worktree" || len(items) != 2 {
		t.Fatalf("the tasks of add-search = %v", search["tasks"])
	}
	if first, _ := items[0].(map[string]any); first["text"] != "1.1 Index the names" || first["done"] != true {
		t.Errorf("the first task of add-search = %v", items[0])
	}
	// The cards of a folded lane are listed, and their tasks are read once the lane is opened.
	if legacy := card(laneDiscarded, "add-legacy"); legacy["state"] != "discarded" {
		t.Errorf("the card of add-legacy = %v", legacy)
	}
	if feed := card(laneProposals, "add-feed"); feed["kind"] != "proposal" || feed["state"] != "approved" {
		t.Errorf("the card of add-feed = %v", feed)
	}
	if moves, isList := card(laneBuilding, "add-search")["moves"].([]any); !isList || len(moves) != 0 {
		t.Errorf("the moves of add-search = %v, want an empty list", card(laneBuilding, "add-search")["moves"])
	}
}

// TestARiskBlockIsLiftedByAcceptingTheRisk: the card of a change blocked for its risk goes back to
// the backlog by accepting that risk, not by unblocking it: unblocked without it, the gate that
// flagged the change blocks it again.
func TestARiskBlockIsLiftedByAcceptingTheRisk(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	ctx := context.Background()
	run, err := st.BeginChangeRun(ctx, "add-ui", 1, "/tmp/worker-add-ui", "feat/spec-add-ui")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BlockChange(store.WithChangeRun(ctx, run, "add-ui"), store.QuarantineRecord{ChangeName: "add-ui", Reason: "risk_review", Details: "risk=high", Stage: "verify"}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishChangeRun(ctx, run, "blocked"); err != nil {
		t.Fatal(err)
	}
	_, body := overviewPage(t, srv, "/board")
	card := cardOf(t, body, "add-ui")
	wantOnPage(t, "the card of a change blocked for its risk", card, "risk review", `data-move-to="backlog" data-post="/api/changes/add-ui/risk-approval"`, "Accept the risk of add-ui?")
	wantNotOnPage(t, "the card of a change blocked for its risk", card, "/api/changes/add-ui/unblock")
}

// TestAbandonTakesAChangeOutOfTheWorkOfTheKit: abandoning a change takes it out of the backlog file
// and of the merge queue, lifts its block, spends the rebuild the merger asked for and discards it,
// as `foreman-kit abandon` does. A change a worker builds or the merger integrates is not
// abandoned from the portal, nor one that is already done.
func TestAbandonTakesAChangeOutOfTheWorkOfTheKit(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	seedWork(t, st, root)
	seedBoard(t, srv, st, root)
	ctx := context.Background()
	inBacklog := func(name string) bool {
		items, err := cycle.ReadJSONList(filepath.Join(root, cycle.BacklogFile))
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if asStr(it["change_name"]) == name {
				return true
			}
		}
		return false
	}
	state := func(name string) string {
		s, err := st.ChangeState(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// A change that waits in the backlog.
	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/changes/add-ui/abandon", nil)
	if code != http.StatusOK || data["state"] != store.ChangeDiscarded || state("add-ui") != store.ChangeDiscarded || inBacklog("add-ui") {
		t.Fatalf("POST /api/changes/add-ui/abandon = %d %s (state %s, in the backlog file: %v)", code, raw, state("add-ui"), inBacklog("add-ui"))
	}
	if !inBacklog("add-api") || !inBacklog("add-billing") {
		t.Errorf("abandoning one change took another out of the backlog file")
	}

	// A blocked change: its block is closed as abandoned, and the question its agent left is removed.
	question := filepath.Join(root, ".worktrees", "worker-add-billing", "openspec", "changes", "add-billing", ".needs-human")
	if _, err := os.Stat(question); err != nil {
		t.Fatalf("the seed left no question for add-billing: %v", err)
	}
	code, _, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/add-billing/abandon", nil)
	if code != http.StatusOK || state("add-billing") != store.ChangeDiscarded || inBacklog("add-billing") {
		t.Fatalf("POST /api/changes/add-billing/abandon = %d %s (state %s)", code, raw, state("add-billing"))
	}
	if _, err := os.Stat(question); !os.IsNotExist(err) {
		t.Errorf("the question of an abandoned change is still in its worktree: %v", err)
	}
	if q, err := st.GetQuarantine(ctx, "add-billing"); err != nil || (q != nil && q.Status == "blocked") {
		t.Errorf("the quarantine of an abandoned change = %+v, %v", q, err)
	}
	var resolution string
	if err := st.View(ctx, func(v *store.View) error {
		rec, err := v.Change(ctx, "add-billing")
		if err == nil && rec != nil && rec.Block == nil {
			resolution = "closed"
		}
		return err
	}); err != nil || resolution != "closed" {
		t.Errorf("the block of an abandoned change is still open (%v)", err)
	}

	// A change the merger asked to build again, and one that waits in the merge queue.
	code, _, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/add-api/abandon", nil)
	if code != http.StatusOK || state("add-api") != store.ChangeDiscarded {
		t.Fatalf("POST /api/changes/add-api/abandon = %d %s (state %s)", code, raw, state("add-api"))
	}
	code, _, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/add-wishlist/abandon", nil)
	if code != http.StatusOK || state("add-wishlist") != store.ChangeDiscarded {
		t.Fatalf("POST /api/changes/add-wishlist/abandon = %d %s (state %s)", code, raw, state("add-wishlist"))
	}
	var waiting []string
	if err := st.View(ctx, func(v *store.View) error {
		mq, err := v.MergeQueue(ctx, "", 1)
		for _, m := range mq.Waiting {
			waiting = append(waiting, m.ChangeName)
		}
		return err
	}); err != nil || strings.Contains(strings.Join(waiting, " "), "add-wishlist") {
		t.Errorf("the merge queue after abandoning add-wishlist = %v, %v", waiting, err)
	}

	// What is not abandoned from the portal, each with why.
	for name, want := range map[string]string{
		"add-search": "a worker is building add-search",
		"add-cart":   "the merger is integrating add-cart",
		"add-login":  "add-login is integrated",
		"add-legacy": "add-legacy is already discarded",
	} {
		code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/changes/"+name+"/abandon", nil)
		if c, msg := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(strings.ToLower(msg), want) {
			t.Errorf("POST /api/changes/%s/abandon = %d %s; want 409 saying %q", name, code, raw, want)
		}
	}
	if state("add-search") != store.ChangeBuilding || state("add-cart") != store.ChangeMerging {
		t.Errorf("a refused abandon changed a state: add-search is %s, add-cart is %s", state("add-search"), state("add-cart"))
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/no-such-change/abandon", nil)
	if c, _ := errorOf(data); code != http.StatusNotFound || c != "not_found" {
		t.Errorf("POST /api/changes/no-such-change/abandon = %d %s", code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/changes/add-export/abandon", map[string]any{"force": true})
	if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
		t.Errorf("POST /api/changes/add-export/abandon with a field it does not take = %d %s", code, raw)
	}

	// The board shows the abandoned changes where their state says.
	_, body := overviewPage(t, srv, "/board?open=discarded")
	discarded := strings.Join(cardsOf(t, body, laneDiscarded), " ")
	for _, name := range []string{"add-ui", "add-billing", "add-api", "add-wishlist"} {
		if !strings.Contains(discarded, name) || strings.Count(body, `data-card="`+name+`"`) != 1 {
			t.Errorf("after abandoning %s, the cards of Discarded = %s", name, discarded)
		}
	}
	// Blocked has no card left: it is a strip, with no room for cards.
	if strings.Contains(body, `id="lane-blocked-cards"`) || !regexp.MustCompile(`id="lane-blocked-count"[^>]*>0<`).MatchString(body) {
		t.Errorf("the lane of the blocked changes still has a card after add-billing was abandoned")
	}
}

// TestTheTasksOfAChangeAreTheOnesTheMergerCounts: the tasks a card shows are the lines the
// orchestrator counts when it audits the tasks of a change before its merge, so the bar of a card
// and the block "incomplete tasks" never disagree. A change with no tasks.md, or with one that
// lists no task, has no tasks to show.
func TestTheTasksOfAChangeAreTheOnesTheMergerCounts(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	seedBoard(t, srv, st, root)
	dir := filepath.Join(root, ".worktrees", "worker-add-billing", "openspec", "changes", "add-billing")

	tasks := srv.changes.Tasks("add-billing")
	if tasks == nil {
		t.Fatal("add-billing has no tasks")
	}
	done, total := orchestrator.CountTasksInFile(filepath.Join(dir, "tasks.md"))
	if tasks.Done != done || tasks.Total != total || tasks.Done != 2 || tasks.Total != 5 || len(tasks.Items) != 5 || tasks.Source != "worktree" {
		t.Errorf("the tasks of add-billing = %d of %d (%d listed, from %s); the orchestrator counts %d of %d", tasks.Done, tasks.Total, len(tasks.Items), tasks.Source, done, total)
	}
	want := []Task{
		{Section: "1. Invoices", Text: "1.1 Model the invoice", Done: true},
		{Section: "1. Invoices", Text: "1.2 Number the invoices in `invoice.Number`", Done: true},
		{Section: "1. Invoices", Text: "1.3 Send the invoice by mail"},
		{Section: "1. Invoices", Text: "1.3.1 Render it as a PDF"},
		{Section: "2. Coverage", Text: "Cover the totals with tests"},
	}
	for i, task := range tasks.Items {
		if i < len(want) && task != want[i] {
			t.Errorf("task %d of add-billing = %+v, want %+v", i+1, task, want[i])
		}
	}

	// Every way a plan writes a task, with the line endings of another system.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "tasks.md")
	must(os.WriteFile(path, []byte("- [x] a\r\n* [X] b\r\n+ [ ] c\r\n1. [ ] d\r\n2) [x] e\r\n\t- [ ] f\r\n- [ ]\r\n-[ ] not a task\r\n- [y] nor this\r\n"), 0o644))
	tasks = srv.changes.Tasks("add-billing")
	done, total = orchestrator.CountTasksInFile(path)
	if tasks == nil || tasks.Done != done || tasks.Total != total || done != 3 || total != 7 {
		t.Fatalf("the tasks of a file written every way = %+v; the orchestrator counts %d of %d", tasks, done, total)
	}
	if last := tasks.Items[len(tasks.Items)-1]; last.Text != "" || last.Done || tasks.Items[5].Text != "f" {
		t.Errorf("the last tasks = %+v", tasks.Items[5:])
	}

	// No task, no file, no such change.
	must(os.WriteFile(path, []byte("# Tasks\n\nNothing is planned yet.\n"), 0o644))
	if got := srv.changes.Tasks("add-billing"); got != nil {
		t.Errorf("a tasks.md that lists no task = %+v, want none", got)
	}
	must(os.Remove(path))
	if got := srv.changes.Tasks("add-billing"); got != nil {
		t.Errorf("a change with no tasks.md = %+v, want none", got)
	}
	for _, name := range []string{"add-ui", "no-such-change", "../add-search", ""} {
		if got := srv.changes.Tasks(name); got != nil {
			t.Errorf("the tasks of %q = %+v, want none", name, got)
		}
	}
}

// TestAFoldedLaneCountsEveryCardItHasAndSaysWhereTheRestAre: a folded lane shows the latest cards
// of each kind and counts all of them, however many pages of the lists of the store they fill. The
// cards it leaves out are one link away.
func TestAFoldedLaneCountsEveryCardItHasAndSaysWhereTheRestAre(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// More rejected proposals than a page of the list of the store holds, the first of them to edit
	// a prompt, which are not cards; and more changes integrated in the last days than a page
	// holds, besides one from before those days.
	const rejected, prompts, integrated = store.MaxPageSize + 12, 5, store.MaxPageSize + 7
	now := time.Now().UTC()
	var proposals []map[string]any
	for i := 0; i < rejected; i++ {
		p := map[string]any{"change_name": fmt.Sprintf("idea-%03d", i), "status": "rejected", "description": "An idea nobody took",
			"proposed_at": now.Add(-time.Duration(rejected-i) * time.Hour).Format(time.RFC3339)}
		if i < prompts {
			p["kind"], p["prompt"] = "prompt", "plan"
		}
		proposals = append(proposals, p)
	}
	must(cycle.WriteProposals(root, proposals))
	history := []map[string]any{{"change_name": "done-before", "integrated_at": now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
		"commit": "deadbeef12345678", "description": "Integrated before the days of the lane"}}
	for i := 0; i < integrated; i++ {
		history = append(history, map[string]any{"change_name": fmt.Sprintf("done-%03d", i), "integrated_at": now.Add(-time.Duration(i+1) * 30 * time.Minute).Format(time.RFC3339),
			"commit": fmt.Sprintf("%016x", i+1), "description": "A change that was integrated"})
	}
	raw, err := json.Marshal(history)
	must(err)
	must(os.WriteFile(filepath.Join(root, ".foreman-kit", "agent_history.json"), raw, 0o644))
	srv.syncBacklog(context.Background())

	for _, address := range []string{"/api/board", "/api/board?open=discarded,integrated"} {
		code, data, body, _ := doReq(t, srv, http.MethodGet, address, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", address, code, body)
		}
		lanes, _ := data["lanes"].([]any)
		for _, it := range lanes {
			lane, _ := it.(map[string]any)
			key := asStr(lane["key"])
			var want int
			var first, last, label, href string
			switch key {
			case laneDiscarded:
				// The latest proposal first; the ones to edit a prompt are the oldest.
				want, first, last = rejected-prompts, fmt.Sprintf("idea-%03d", rejected-1), fmt.Sprintf("idea-%03d", rejected-boardFoldedCards)
				label, href = "The rest of the rejected proposals", "/backlog/proposals?status=rejected"
			case laneIntegrated:
				want, first, last = integrated, "done-000", fmt.Sprintf("done-%03d", boardFoldedCards-1)
				label, href = "The rest of the integrated changes", "/changes?state=integrated"
			default:
				if lane["count"] != float64(0) {
					t.Errorf("%s: the lane %s has %v cards, want none", address, key, lane["count"])
				}
				continue
			}
			cards, _ := lane["cards"].([]any)
			if lane["count"] != float64(want) || len(cards) != boardFoldedCards {
				t.Fatalf("%s: the lane %s counts %v cards and holds %d, want %d and %d", address, key, lane["count"], len(cards), want, boardFoldedCards)
			}
			name := func(i int) string {
				c, _ := cards[i].(map[string]any)
				return asStr(c["name"])
			}
			if name(0) != first || name(len(cards)-1) != last {
				t.Errorf("%s: the cards of %s go from %s to %s, want from %s to %s", address, key, name(0), name(len(cards)-1), first, last)
			}
			more, _ := lane["more"].([]any)
			if len(more) != 1 {
				t.Fatalf("%s: the lane %s says where the rest are %d times: %v", address, key, len(more), lane["more"])
			}
			if rest, _ := more[0].(map[string]any); rest["label"] != label || rest["href"] != href {
				t.Errorf("%s: the rest of the lane %s = %v", address, key, rest)
			}
		}
	}

	// The page says the same numbers, folded and open, and links the rest once the lane is open.
	_, folded := overviewPage(t, srv, "/board")
	_, opened := overviewPage(t, srv, "/board?open=discarded,integrated")
	for _, body := range []string{folded, opened} {
		for lane, n := range map[string]int{laneDiscarded: rejected - prompts, laneIntegrated: integrated} {
			if !regexp.MustCompile(fmt.Sprintf(`id="lane-%s-count"[^>]*>%d<`, lane, n)).MatchString(body) {
				t.Errorf("the lane %s does not say it has %d cards", lane, n)
			}
		}
	}
	wantOnPage(t, "the board with its folded lanes open", opened,
		`href="/backlog/proposals?status=rejected"`, "The rest of the rejected proposals", `href="/changes?state=integrated"`, "The rest of the integrated changes")
	wantNotOnPage(t, "the board", opened, "done-before", "idea-000", "The rest of the discarded changes")
	wantNotOnPage(t, "the board with its lanes folded", folded, "The rest of the")
	if n := strings.Count(opened, `data-card="`); n != 2*boardFoldedCards {
		t.Errorf("the board draws %d cards, want %d", n, 2*boardFoldedCards)
	}
}

// TestAChangeAndAProposalOfTheSameNameAreTwoCards: the kit can have records of a change the backlog
// does not list while a proposal of the same name waits for a decision. Each is a card in its own
// lane, and the page tells one from the other by its kind.
func TestAChangeAndAProposalOfTheSameNameAreTwoCards(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	seedWork(t, st, root)
	seedBoard(t, srv, st, root)
	seedTwin(t, srv, root)

	_, body := overviewPage(t, srv, "/board?open=discarded")
	if n := strings.Count(body, `data-card="add-orphan"`); n != 2 {
		t.Fatalf("the board has %d cards of add-orphan, want two", n)
	}
	for lane, kind := range map[string]string{laneDiscarded: cardChange, laneProposals: cardProposal} {
		if !strings.Contains(laneOf(t, body, lane), `data-card="add-orphan" data-kind="`+kind+`"`) {
			t.Errorf("the lane %s has no card of add-orphan of the kind %s", lane, kind)
		}
	}
	// The proposal is the one a person decides on; the change has no move.
	wantOnPage(t, "the card of the proposal", cardOf(t, laneOf(t, body, laneProposals), "add-orphan"),
		"The orphan, proposed again", `data-post="/api/proposals/add-orphan/approve"`, `data-post="/api/proposals/add-orphan/reject"`)
	wantNotOnPage(t, "the card of the change", cardOf(t, laneOf(t, body, laneDiscarded), "add-orphan"), "data-post=")
}

// TestCodeSpansOfALineOfMarkdown: what a line writes between backticks is drawn as code, and
// nothing a line holds is taken for markup.
func TestCodeSpansOfALineOfMarkdown(t *testing.T) {
	for text, want := range map[string]string{
		"plain text":          "plain text",
		"run `go test ./...`": `run <code class="font-mono">go test ./...</code>`,
		"`a` and `b`":         `<code class="font-mono">a</code> and <code class="font-mono">b</code>`,
		"a ` alone":           "a ` alone",
		"`a` then ` alone":    `<code class="font-mono">a</code> then ` + "` alone",
		"<b>bold</b> `<i>`":   `&lt;b&gt;bold&lt;/b&gt; <code class="font-mono">&lt;i&gt;</code>`,
		"":                    "",
	} {
		if got := string(codeSpans(text)); got != want {
			t.Errorf("codeSpans(%q) = %q, want %q", text, got, want)
		}
	}
}

// TestEveryStateOfAChangeHasItsLane: a change in a state the board has no lane for would be on no
// lane at all.
func TestEveryStateOfAChangeHasItsLane(t *testing.T) {
	lanes := map[string]bool{}
	for _, lane := range boardLanes {
		lanes[lane.Key] = true
	}
	for _, state := range store.ChangeStates {
		if lane := laneOfState[state]; !lanes[lane] {
			t.Errorf("the state %s of a change has the lane %q, which the board does not have", state, lane)
		}
	}
	if len(laneOfState) != len(store.ChangeStates) {
		t.Errorf("the board gives a lane to %d states, and a change has %d", len(laneOfState), len(store.ChangeStates))
	}
}
