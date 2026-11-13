package portal

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// getPageAs asks for a page with the given request headers.
func getPageAs(t *testing.T, srv http.Handler, path string, header map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4567"+path, nil)
	req.Host, req.RemoteAddr = "127.0.0.1:4567", "127.0.0.1:12345"
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

var (
	htmlTagRe   = regexp.MustCompile(`(?s)<(script|style)\b.*?</(script|style)>|<[^>]+>`)
	htmlSpaceRe = regexp.MustCompile(`\s+`)
)

// textOf returns what a person reads on a page: its text, without the tags and with the characters
// its entities stand for.
func textOf(page string) string {
	text := html.UnescapeString(htmlTagRe.ReplaceAllString(page, " "))
	return htmlSpaceRe.ReplaceAllString(text, " ")
}

// mainOf returns the content of a page: what is inside <main>, without the menu and the dialogs.
func mainOf(t *testing.T, page string) string {
	t.Helper()
	start := strings.Index(page, "<main")
	end := strings.Index(page, "</main>")
	if start < 0 || end < start {
		t.Fatalf("the page has no <main>:\n%s", page)
	}
	return page[start:end]
}

func wantOnPage(t *testing.T, page, body string, facts ...string) {
	t.Helper()
	text := textOf(body)
	for _, fact := range facts {
		if !strings.Contains(text, fact) && !strings.Contains(body, fact) {
			t.Errorf("%s does not show %q", page, fact)
		}
	}
}

func wantNotOnPage(t *testing.T, page, body string, facts ...string) {
	t.Helper()
	text := textOf(body)
	for _, fact := range facts {
		if strings.Contains(text, fact) || strings.Contains(body, fact) {
			t.Errorf("%s shows %q", page, fact)
		}
	}
}

// TestTheListOfExecutionsShowsEveryKind: the list draws what is on record about each execution,
// whatever started it, and its filters and pages are addresses.
func TestTheListOfExecutionsShowsEveryKind(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	seedExecutions(t, st)

	code, page, _ := getPage(t, srv, "/executions")
	if code != http.StatusOK {
		t.Fatalf("GET /executions = %d", code)
	}
	body := mainOf(t, page)
	wantOnPage(t, "/executions", body,
		// What each one is, and what it worked on.
		"PLAN · add-api", "BUILD · add-api", "BUILD repair 1 · add-api", "BUILD · add-search",
		"Cycle · 20261108T120000Z", "Agent run · Summarize the open changes", "Follow-up of an agent run · which one first?", "Dispatcher",
		// How each one ended, in the words of the portal.
		"succeeded", "failed", "stopped", "running",
		// The engines, in the order they were tried, and the agent that is idle now.
		"claude", "codex", "hermes", "no activity for",
		// Where each one leads.
		`href="/executions/build-1"`, `href="/executions/run-1"`, `href="/executions/job-1"`, `href="/executions/cycle-20261108T120000Z"`,
		`href="/changes/add-api"`, `href="/executions/new"`,
	)
	if !strings.Contains(page, `data-live-interval="5"`) || !strings.Contains(body, `id="executions-list" data-live-region`) {
		t.Errorf("a list with an execution that runs does not keep itself up to date")
	}
	if !strings.Contains(page, `id="page-read-at"`) || !strings.Contains(textOf(page), "read at") {
		t.Errorf("the page does not say when it was read")
	}

	// The filters.
	_, page, _ = getPage(t, srv, "/executions?kind=portal_run")
	body = mainOf(t, page)
	wantOnPage(t, "/executions?kind=portal_run", body, "Agent run · Summarize the open changes", `aria-selected="true">Agent runs`)
	wantNotOnPage(t, "/executions?kind=portal_run", body, "PLAN · add-api", "Dispatcher", "Cycle · ")
	if strings.Contains(page, "data-live-interval") {
		t.Errorf("a list where nothing runs refreshes itself")
	}

	_, page, _ = getPage(t, srv, "/executions?change=add-api&status=failed")
	body = mainOf(t, page)
	wantOnPage(t, "the list of a change", body, "BUILD · add-api", "Showing only the executions of the change add-api", `href="/executions?status=failed"`, `<option value="failed" selected>`)
	wantNotOnPage(t, "the list of a change", body, "PLAN · add-api", "BUILD · add-search")

	_, page, _ = getPage(t, srv, "/executions?engine=codex")
	wantOnPage(t, "the list of an engine", mainOf(t, page), "BUILD repair 1 · add-api", `<option value="codex" selected>`)

	_, page, _ = getPage(t, srv, "/executions?kind=cycle&status=failed")
	wantOnPage(t, "a list with no match", mainOf(t, page), "No execution matches these filters.")

	// The pages: the address of the next one is the cursor the list gave.
	_, page, _ = getPage(t, srv, "/executions?limit=3")
	body = mainOf(t, page)
	older := regexp.MustCompile(`href="(/executions\?before=[^"]+)"`).FindStringSubmatch(body)
	if older == nil {
		t.Fatalf("a first page of 3 has no link to the older ones:\n%s", body)
	}
	wantNotOnPage(t, "the first page", body, "PLAN · add-api", "Latest")
	next := strings.ReplaceAll(older[1], "&amp;", "&")
	code, page, _ = getPage(t, srv, next)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", next, code)
	}
	body = mainOf(t, page)
	wantOnPage(t, "the second page", body, "Latest", `href="/executions?limit=3"`)
	wantNotOnPage(t, "the second page", body, "BUILD · add-search")

	// What the list cannot read is an error the page says, not an empty list.
	for _, bad := range []string{"/executions?kind=job", "/executions?status=ok", "/executions?before=nonsense"} {
		code, page, _ := getPage(t, srv, bad)
		if code != http.StatusBadRequest || !strings.Contains(page, `data-theme=`) {
			t.Errorf("GET %s = %d, want the error page with 400", bad, code)
		}
	}
}

// TestThePageOfAnExecutionSaysWhatHappened: one page serves every kind of execution, and each fact
// on record about it is on the page.
func TestThePageOfAnExecutionSaysWhatHappened(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	seedExecutions(t, st)
	page := func(id string) string {
		t.Helper()
		code, whole, _ := getPage(t, srv, "/executions/"+id)
		if code != http.StatusOK {
			t.Fatalf("GET /executions/%s = %d\n%s", id, code, whole)
		}
		if strings.Contains(whole, "could not be rendered") {
			t.Fatalf("GET /executions/%s did not render:\n%s", id, whole)
		}
		return mainOf(t, whole)
	}

	// A BUILD that changed engine and failed, with the gates that ran on it.
	body := page("build-1")
	wantOnPage(t, "the page of build-1", body,
		"BUILD · add-api", "What happened", "The agent exited with an error (exit 1): see its output below.",
		"Next for this change:", `href="/executions/repair-1"`, "BUILD repair 1",
		// The attempts: one stalled, the next one failed.
		"stopped by the idle watchdog", "No activity for", "exited with an error", "exit 143", "exit 1",
		// Where it comes from.
		"Worker run", `href="/changes/add-api#run-1"`, "from the backlog", "ready to merge", `href="/changes/add-api"`,
		// What verified it.
		"Verification", "backend · unit-tests", "--- FAIL: TestCreateWidget", "exit 1",
		// Its output, its prompt and its events.
		"codex: the tests fail", "Implement add-api.", "agent.idle", "agent.exit",
		`/api/executions/build-1/log?format=text`,
	)
	wantNotOnPage(t, "the page of build-1", body, "Continue this run", `/stop"`, `/rerun"`, "data-stream=")
	if strings.Index(body, "claude") > strings.Index(body, "codex") {
		t.Errorf("the engines of build-1 are not in the order they were tried")
	}

	// The repair it led to.
	body = page("repair-1")
	wantOnPage(t, "the page of repair-1", body,
		"BUILD repair 1 · add-api", "The agent finished its BUILD repair 1 prompt.", "Nothing has run for this change since.",
		"Repair round 1", "gate failure", "the gates passed afterwards", "api/widget.go | 4 ++--", "passed", "codex: fixed TestCreateWidget")

	// A PLAN, with the versions of what it was given.
	wantOnPage(t, "the page of plan-1", page("plan-1"), "PLAN · add-api", "The agent finished its PLAN prompt.", "aaaa1111", "bbbb2222", "Write the proposal of add-api.")

	// A cycle.
	body = page("cycle-20261108T120000Z")
	wantOnPage(t, "the page of the cycle", body, "Cycle · 20261108T120000Z", "The judge of the cycle finished its prompt.", `href="/cycles/20261108T120000Z"`, "== cycle 20261108T120000Z")

	// An agent run a person started: what followed it, and what can be done with it.
	body = page("run-1")
	wantOnPage(t, "the page of run-1", body,
		"Agent run", "Summarize the open changes", "The agent finished the prompt.", "three changes are open", "and list the risky ones",
		"What followed it", `href="/executions/run-2"`, "which one first?",
		"Continue this run", `data-api="/api/executions/run-1/continue"`, `name="with_context"`, "the same agent: hermes",
		`data-post="/api/executions/run-1/rerun"`, "Run again", "/tmp/workspace")
	wantNotOnPage(t, "the page of run-1", body, `/stop"`)

	body = page("run-2")
	wantOnPage(t, "the page of run-2", body, "Follow-up of an agent run", "Stopped before it finished (SIGTERM, exit 143).", "Continues", `href="/executions/run-1"`, "Summarize the open changes")

	// An operation of the kit.
	body = page("job-1")
	wantOnPage(t, "the page of job-1", body, "Dispatcher", "Dispatcher failed (exit 2): see its output below.", "foreman-kit dispatch", "dispatcher: nothing to do", "runs no agent of its own")
	wantNotOnPage(t, "the page of job-1", body, "Continue this run", `/rerun"`, `/stop"`)

	if code, page, _ := getPage(t, srv, "/executions/no-such-execution"); code != http.StatusNotFound || !strings.Contains(page, "There is no execution") {
		t.Errorf("GET /executions/no-such-execution = %d, want the page that says it does not exist", code)
	}
}

// TestThePageOfARunningExecutionFollowsIt: while an execution runs, its page says what it is doing,
// follows its output through the stream, and reads the rest of itself again without the output.
func TestThePageOfARunningExecutionFollowsIt(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	seedExecutions(t, st)
	ctx := context.Background()

	code, page, _ := getPage(t, srv, "/executions/build-live")
	if code != http.StatusOK {
		t.Fatalf("GET /executions/build-live = %d", code)
	}
	body := mainOf(t, page)
	wantOnPage(t, "the page of a running BUILD", body,
		"BUILD · add-search", "What it is doing", "Running, but claude has shown no activity for", "the idle watchdog stops it at 20m 00s (limits.idle_timeout)",
		"running for", "idle now, stopped at 20m 00s", "reading the specs", "following the output",
		`data-stream="/api/executions/build-live/log/stream"`, `id="exec-head" data-live-region`, `id="exec-outcome" data-live-region`, `id="exec-attempts" data-live-region`, `id="exec-events" data-live-region`)
	if !strings.Contains(page, `data-live-interval="5"`) {
		t.Errorf("the page of a running execution does not keep itself up to date")
	}
	// The stream starts after the output the page already shows.
	after := regexp.MustCompile(`data-stream-after="(\d+)"`).FindStringSubmatch(body)
	chunks, err := st.ReadLogChunks(ctx, "build-live", 0)
	if err != nil || len(chunks) != 1 || after == nil || after[1] == "0" {
		t.Fatalf("the stream of the page starts after %v; the output has the chunks %+v (%v)", after, chunks, err)
	}
	// The output is not a region the page reads again: the stream brings what is written.
	if strings.Contains(body, `<pre id="exec-output" data-live-region`) {
		t.Errorf("the output of a running execution is read again with the page as well as streamed")
	}
	// A worker is not stopped from the portal.
	wantNotOnPage(t, "the page of a running BUILD", body, `/stop"`)

	// Asked for by the script that keeps it up to date, the page leaves the output out.
	code, refreshed := getPageAs(t, srv, "/executions/build-live", map[string]string{"X-Portal-Refresh": "regions"})
	if code != http.StatusOK || strings.Contains(refreshed, "reading the specs") || !strings.Contains(refreshed, `id="exec-attempts" data-live-region`) {
		t.Errorf("the page read again for its regions = %d; it has the output: %v", code, strings.Contains(refreshed, "reading the specs"))
	}

	// An agent run that runs can be stopped, and waits for its first output.
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "run-live", Kind: "portal_run", EnginesRequested: "claude", PromptText: "work for a while"}); err != nil {
		t.Fatal(err)
	}
	_, page, _ = getPage(t, srv, "/executions/run-live")
	body = mainOf(t, page)
	wantOnPage(t, "the page of a running agent run", body, "Running now: the agent is working on the prompt.", "Nothing has been written yet.",
		`data-post="/api/executions/run-live/stop"`, `data-refresh="regions"`, `data-stream="/api/executions/run-live/log/stream"`, `data-stream-after="0"`)
	wantNotOnPage(t, "the page of a running agent run", body, `/rerun"`)

	// An execution recorded as running whose process is gone does not pretend to run.
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "run-orphan", Kind: "portal_run", EnginesRequested: "claude", PromptText: "left behind", PID: gone.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	_, page, _ = getPage(t, srv, "/executions/run-orphan")
	body = mainOf(t, page)
	wantOnPage(t, "the page of an execution whose process is gone", body, "Recorded as running, but the process that ran it", "is gone", "Stop it to close its record.", `data-post="/api/executions/run-orphan/stop"`)
	if strings.Contains(page, "data-live-interval") || strings.Contains(body, "data-stream=") {
		t.Errorf("the page of an execution whose process is gone follows it as if it ran")
	}
	_, page, _ = getPage(t, srv, "/executions?kind=portal_run&status=running")
	wantOnPage(t, "the list", mainOf(t, page), "The process that ran it is gone")
}

// TestALongOutputShowsItsEndAndOffersTheWhole: the page of an execution shows the end of a long
// output, says so, and gives the address of the whole.
func TestALongOutputShowsItsEndAndOffersTheWhole(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	if err := st.CreateExecution(ctx, store.Execution{ExecID: "run-long", Kind: "portal_run", EnginesRequested: "claude", PromptText: "print a lot"}); err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 1023) + "\n"
	if err := st.AppendLogChunk(ctx, "run-long", "combined", "THE FIRST LINE\n"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if err := st.AppendLogChunk(ctx, "run-long", "combined", line); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AppendLogChunk(ctx, "run-long", "combined", "THE LAST LINE\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishExecution(ctx, "run-long", "succeeded", 0); err != nil {
		t.Fatal(err)
	}

	_, page, _ := getPage(t, srv, "/executions/run-long")
	body := mainOf(t, page)
	wantOnPage(t, "the page of a long output", body, "THE LAST LINE", "showing the end", `href="/executions/run-long?output=all"`)
	wantNotOnPage(t, "the page of a long output", body, "THE FIRST LINE")

	_, page, _ = getPage(t, srv, "/executions/run-long?output=all")
	body = mainOf(t, page)
	wantOnPage(t, "the page with the whole output", body, "THE FIRST LINE", "THE LAST LINE")
	wantNotOnPage(t, "the page with the whole output", body, "showing the end")
}

func TestTheFormThatRunsAnAgent(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	code, page, _ := getPage(t, srv, "/executions/new")
	if code != http.StatusOK {
		t.Fatalf("GET /executions/new = %d", code)
	}
	body := mainOf(t, page)
	wantOnPage(t, "/executions/new", body, "New agent run", `method="post"`, `data-api="/api/executions"`, `name="engine"`, `name="prompt"`,
		`<option value="claude" selected>`, `<option value="hermes">`, `<option value="auto">`)
	if n := strings.Count(body, "btn-primary"); n != 1 {
		t.Errorf("the form has %d primary actions, want one", n)
	}
}

func TestInstantsAndDurationsAsThePortalSaysThem(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.Local)
	// An instant of an earlier day reads as its date and hour. One of today reads as how long ago it
	// was, measured against the clock, so it is not checked here.
	for at, want := range map[time.Time]string{
		now.Add(-20 * time.Hour):             "Nov 9, 19:00",
		now.Add(-15*time.Hour - time.Second): "Nov 9, 23:59",
		now.AddDate(0, 0, -40):               "Oct 1, 15:00",
		now.AddDate(-1, 0, 0):                "Nov 10 2025, 15:00",
	} {
		if got := atText(at, now); got != want {
			t.Errorf("atText(%s) = %q, want %q", at.Format(time.RFC3339), got, want)
		}
	}
	drawn := string(atHTML(now.Add(-48 * time.Hour).UTC().Format(time.RFC3339)))
	if !strings.Contains(drawn, `<time class="whitespace-nowrap" datetime="`) || !strings.Contains(drawn, `title="`) || !strings.Contains(drawn, now.Add(-48*time.Hour).Format("MST")) {
		t.Errorf("an instant is drawn as %s, want a <time> with the exact instant and its zone", drawn)
	}
	if got := string(atHTML("")); !strings.Contains(got, "–") {
		t.Errorf("no instant is drawn as %q", got)
	}

	for d, want := range map[time.Duration]string{
		850 * time.Millisecond:                "850ms",
		42 * time.Second:                      "42s",
		3*time.Minute + 12*time.Second:        "3m 12s",
		20 * time.Minute:                      "20m 00s",
		2*time.Hour + 5*time.Minute:           "2h 05m",
		28*time.Hour + 30*time.Minute:         "1d 4h",
		-time.Second:                          "0ms",
		59*time.Second + 600*time.Millisecond: "1m 00s",
	} {
		if got := durationText(d); got != want {
			t.Errorf("durationText(%v) = %q, want %q", d, got, want)
		}
	}
	start := now.Add(-90 * time.Second).UTC().Format(time.RFC3339)
	if got := tookText(start, now.UTC().Format(time.RFC3339), time.Time{}); got != "1m 30s" {
		t.Errorf("tookText of 90 seconds = %q", got)
	}
	if got := tookText(start, "", now.Add(30*time.Second)); got != "2m 00s" {
		t.Errorf("tookText of something that still runs = %q, want the time until the page was read", got)
	}
	if got := tookText(start, start, now); got != "under 1s" {
		t.Errorf("tookText of an instant = %q", got)
	}
	if got := tookText("", "", now); got != "–" {
		t.Errorf("tookText with no start = %q", got)
	}
	// A count of whole seconds: none of them is less than a second, not "0ms".
	for n, want := range map[int]string{0: "under 1s", 1: "1s", 300: "5m 00s", 1200: "20m 00s"} {
		if got := secondsText(n); got != want {
			t.Errorf("secondsText(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestAProblemReadsAsASentence(t *testing.T) {
	for in, want := range map[string]string{
		"the prompt is empty":                  "The prompt is empty.",
		`"nope" is not an agent the kit knows`: `"nope" is not an agent the kit knows.`,
		"There is no page at /x.":              "There is no page at /x.",
		"is it running?":                       "Is it running?",
		"  spaced  ":                           "Spaced.",
		"":                                     "",
		"cycle.queue.max_open must be a whole number above 0, not \"0\"": "cycle.queue.max_open must be a whole number above 0, not \"0\".",
		"nope.x is not a setting":           "nope.x is not a setting.",
		"probe-e is already in the backlog": "probe-e is already in the backlog.",
		"/tmp/x is missing":                 "/tmp/x is missing.",
		"blocked: the gate failed":          "Blocked: the gate failed.",
	} {
		if got := sentence(in, false); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
	a := answerOf(errConflict("the execution %s is not running: it ended as %s", "run-1", "stopped"))
	if a.status != http.StatusConflict || a.code != "conflict" || a.message != "The execution run-1 is not running: it ended as stopped." {
		t.Errorf("answerOf(a conflict) = %+v", a)
	}
	// A message that opens with an interpolated value keeps it as it came, even a plain word.
	if a := answerOf(errConflict("%s is already in the backlog", "probe")); a.message != "probe is already in the backlog." {
		t.Errorf("answerOf(an identifier first) = %+v", a)
	}
	// A message passed through whole is not flagged, and its dotted first word is still kept.
	if a := answerOf(errInvalid("%s", "nope.x is not a setting")); a.status != http.StatusBadRequest || a.message != "nope.x is not a setting." {
		t.Errorf("answerOf(a refusal passed through) = %+v", a)
	}
	if a := answerOf(errInvalid("%s", "the value is empty")); a.message != "The value is empty." {
		t.Errorf("answerOf(a plain message passed through) = %+v", a)
	}
}
