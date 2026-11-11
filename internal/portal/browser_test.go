package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// browserPage is a page the browser tests open, with the name a failure reports it by.
type browserPage struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// TestThePagesInABrowser opens the pages the redesign has rewritten in a headless Chromium, over a
// portal that holds the states the render tests draw, and runs the checks of web/portal/tests on
// each of them: it loads without an error, it passes the automatic accessibility check, every
// control is reached with the keyboard, and on a phone nothing overflows sideways and every control
// is tall enough for a finger. A page a later step rewrites joins the list in that step.
//
// It needs Node, the packages of web/portal and the Chromium of Playwright, which `go test` asks
// nobody for: it runs when PORTAL_BROWSER is set, as `npm test` in web/portal sets it. `npm run
// test:serve` there keeps the same portal up instead, for a person to open its pages.
func TestThePagesInABrowser(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER") == "" {
		t.Skip("the pages are opened in a browser by `npm test` in web/portal, which sets PORTAL_BROWSER=1")
	}
	srv, st, root := setupTestPortal(t)
	seed := seedOverview(t, srv, st, root)
	// The daemon of this portal was started with a value: Dashboard and Processes say which.
	seedDaemonGiven(t, st, "pipeline.dispatch.workers=4")
	// Dashboard also draws a cycle that runs, and the pages of the queue have something in each part.
	seedRunningCycle(t, st)
	seedWork(t, st, root)
	seedQuality(t, st)
	seedBoard(t, srv, st, root)
	// A change and a proposal of the same name, a cycle that queued a change, and what the registry
	// has of the engines.
	seedTwin(t, srv, root)
	seedCycleWork(t, st, root)
	// The same cycle queued another change, which the main checkout does not list: Dashboard counts it
	// apart, and the page of the cycle says what it waits for.
	apart := map[string]any{"change_name": "add-robots", "priority": "low", "description": "A robots.txt for the widgets"}
	if err := st.RecordCycleWork(context.Background(), "20261108T120000Z", store.CycleWork{Queued: []map[string]any{apart}}); err != nil {
		t.Fatal(err)
	}
	seedAgents(t, st)
	seedVisual(t, st)
	session := seedVisualSession(t, root)
	// A kit with enough on record to say which settings it would change.
	recommending(srv, 1)
	web := httptest.NewServer(srv)
	defer web.Close()

	pages := []browserPage{
		{"what the kit is doing now", "/"},
		{"what waits for a person", "/inbox"},
		{"the board of changes", "/board"},
		{"the board with its folded lanes open", "/board?open=discarded,integrated"},
		{"the list of changes", "/changes"},
		{"the list of changes, searched in one state", "/changes?state=building&q=add"},
		{"the backlog", "/backlog"},
		{"the proposals of the backlog", "/backlog/proposals"},
		{"the proposals that were rejected", "/backlog/proposals?status=rejected"},
		{"the merge queue", "/merge"},
		{"the list of cycles", "/cycles"},
		{"a cycle that ended, with its report", "/cycles/20261108T120000Z"},
		{"a cycle that runs", "/cycles/20261110T100000Z"},
		{"the list of events", "/events"},
		{"the events of a change", "/events?change=add-api"},
		{"an event", "/events/1"},
		{"what the gates and the evaluator found", "/quality"},
		{"what the gates and the evaluator found in everything on record", "/quality?since=all"},
		{"the gates and the evaluator in a period with nothing", "/quality?since=2100-01-01"},
		{"the checks of visual QA", "/visual"},
		{"the checks of visual QA that wait", "/visual?status=pending"},
		{"the sessions of visual QA", "/visual/sessions"},
		{"a session of visual QA, with its report", "/visual/sessions/" + session},
		{"the agents", "/agents"},
		{"the health of the kit", "/system/health"},
		{"the settings of the application", "/system/settings"},
		{"the settings of one file", "/system/settings?file=" + settings.Agents.File()},
		{"the editor of a setting", "/system/settings?edit=" + string(settings.Workers)},
		{"the editor of a setting the portal does not change", "/system/settings?edit=" + string(settings.AgentCommands)},
		{"what the kit would change of its settings", "/system/recommendations"},
		{"the prompts of the application", "/system/prompts"},
		{"the prompt of a role", "/system/prompts/build"},
		{"a sample of the prompt of a role", "/system/prompts/plan?view=sample"},
		{"the setup of the application", "/system/setup"},
		{"the processes of the kit", "/system/processes"},
		{"the look of the portal, with every theme of the menu", "/system/appearance"},
		{"the look of the portal, compared with a theme", "/system/appearance?compare=dracula"},
		{"what this browser opened lately", "/recent"},
		{"every agent the kit can run, in everything on record", "/agents?show=all&since=all"},
		{"the list of executions", "/executions"},
		{"the list of executions, filtered", "/executions?kind=portal_run&status=stopped"},
		{"the form that runs an agent", "/executions/new"},
		{"a worker execution that failed", "/executions/build-1"},
		{"an agent run a person started", "/executions/run-1"},
		{"an execution that runs now", "/executions/build-live"},
		{"the judgment of a cycle that runs", "/executions/cycle-20261110T100000Z"},
		{"a change sent back by the merger", "/changes/add-api"},
		{"a blocked change", "/changes/add-billing"},
		{"a change being built", "/changes/add-search"},
		{"a queued change", "/changes/add-ui"},
		{"a change whose worker is gone", "/changes/add-export"},
		{"the files of a change", "/changes/add-search/files"},
		{"the events of a change", "/changes/add-api/events"},
		{"the worktree of a change", "/changes/add-search/intervene"},
		{"a round of gates", "/gates/" + strconv.FormatInt(seed.failedRound, 10)},
	}
	list, err := json.Marshal(pages)
	if err != nil {
		t.Fatal(err)
	}
	// PORTAL_BROWSER=serve keeps that portal up for a person to open, instead of running the checks.
	// With go test -v the addresses below are printed as soon as it is up.
	if os.Getenv("PORTAL_BROWSER") == "serve" {
		fmt.Fprintf(os.Stderr, "the portal of the browser tests is at %s for ten minutes\n", web.URL)
		for _, p := range pages {
			fmt.Fprintf(os.Stderr, "  %s%s  (%s)\n", web.URL, p.Path, p.Name)
		}
		time.Sleep(10 * time.Minute)
		return
	}
	dir, err := filepath.Abs(filepath.Join("..", "..", "web", "portal"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "playwright-core")); err != nil {
		t.Fatalf("the packages of web/portal are not installed (run `npm ci` there): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "--test-reporter=spec", filepath.Join("tests", "pages.test.mjs"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PORTAL_URL="+web.URL, "PORTAL_PAGES="+string(list),
		// A page with an action a person can take, a page that keeps itself up to date, a page
		// with an action that answers with something to read (what the dispatcher would start), and
		// the board, whose cards are dragged.
		"PORTAL_ACTION_PAGE=/changes/add-billing", "PORTAL_LIVE_PAGE=/executions/build-live", "PORTAL_REPORT_PAGE=/backlog",
		"PORTAL_BOARD_PAGE=/board")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the pages do not pass in a browser (%v):\n%s", err, out)
	}
	t.Logf("%s", out)
}
