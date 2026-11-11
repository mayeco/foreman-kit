package portal

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// seedVisual asks for four checks in a browser over the changes of seedOverview: the cycle that
// ended asked for the first, a session took the second, the third ended as verified, and the fourth
// waits and is about no change. It returns their numbers.
func seedVisual(t *testing.T, st *store.Store) []string {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	for i, item := range []store.VisualQAItem{
		{What: "the list of widgets shows their names", ChangeName: "add-ui"},
		{What: "the search page shows results", ChangeName: "add-search"},
		{What: "the invoice prints on one page", ChangeName: "add-billing"},
		{What: "the menu closes with Escape"},
	} {
		at := ctx
		if i == 0 {
			at = store.WithCycle(ctx, "20261108T120000Z")
		}
		id, err := st.EnqueueVisualQANext(at, item)
		must(err)
		ids = append(ids, id)
	}
	must(st.StartVisualQA(ctx, ids[1], "session:20261110T090000Z"))
	must(st.ResolveVisualQA(ctx, ids[2], "verified", "one page, as asked"))
	return ids
}

// seedVisualSession leaves the plan and the report of a visual QA session that ended among the
// files of the kit, and returns its stamp.
func seedVisualSession(t *testing.T, root string) string {
	t.Helper()
	const stamp = "20261109T100000Z"
	for path, text := range map[string]string{
		store.RuntimePath(root, "cycle", "visual-"+stamp+".md"):            "# Visual QA session " + stamp + "\n\n1. The list of widgets shows their names (add-ui).\n2. The search page shows results (add-search).\n",
		store.RuntimePath(root, "cycle", "reports", "visual-"+stamp+".md"): "# Report\n\n| Check | Verdict |\n| --- | --- |\n| The list of widgets shows their names | verified |\n| The search page shows results | regression: the page is empty |\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return stamp
}

// rowOfCheck returns the row of a check in the page of visual QA.
func rowOfCheck(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `>#`+id+`</td>`)
	if start < 0 {
		t.Fatalf("the page of visual QA has no row for the check %s:\n%s", id, textOf(body))
	}
	rest := body[start:]
	if end := strings.Index(rest, `</tr>`); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// setVisualQA turns the visual QA of the application of a test on or off, in its file.
func setVisualQA(t *testing.T, root string, on bool) {
	t.Helper()
	path := filepath.Join(root, settings.VisualQAEnabled.FilePath())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := settings.VisualQAEnabled.Path() + ": "
	from, to := key+"true", key+"false"
	if on {
		from, to = to, from
	}
	if !strings.Contains(string(raw), from) {
		t.Fatalf("%s does not say %q", path, from)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), from, to, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestVisualQAListsTheChecksWithTheirStatusAndTheSessionInProgress: the page lists the checks the
// latest first, whole or in one status, each with what to look at, its change, who asked for it and
// how it ended; a check that waits has the control that records its verdict. The session in
// progress is the lock of visual QA, and the page says when no session is open.
func TestVisualQAListsTheChecksWithTheirStatusAndTheSessionInProgress(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	ids := seedVisual(t, st)
	ctx := context.Background()

	whole, body := overviewPage(t, srv, "/visual")
	wantOnPage(t, "the page of visual QA", body,
		"Visual QA", "Ask for a check", `data-api="/api/visual-qa/items"`, `name="what"`, `name="change_name"`,
		`href="/visual/sessions"`, "Sessions",
		// No session is open: the page offers to begin one.
		`data-post="/api/visual-qa/session/begin"`, "Begin session",
		// The statuses that hold a check, each with how many.
		`href="/visual?status=pending"`, `href="/visual?status=in_progress"`, `href="/visual?status=verified"`,
	)
	wantNotOnPage(t, "the page of visual QA with no session", body, "End session", "does not run visual QA", `href="/visual?status=dropped"`)
	if strings.Contains(whole, `data-live-interval`) {
		t.Errorf("the page of visual QA keeps itself up to date with no session open")
	}
	// The latest check first.
	at := func(id string) int { return strings.Index(body, `>#`+id+`</td>`) }
	if !(at(ids[3]) < at(ids[2]) && at(ids[2]) < at(ids[1]) && at(ids[1]) < at(ids[0])) {
		t.Errorf("the checks do not come the latest first")
	}

	first := rowOfCheck(t, body, ids[0])
	wantOnPage(t, "a check a cycle asked for", first,
		wordingOf(vocVisualStatus, "pending").Text, "the list of widgets shows their names", `href="/changes/add-ui"`,
		"by the cycle", `href="/cycles/20261108T120000Z"`,
		// It waits: a person can say how it ended.
		`data-api="/api/visual-qa/items/`+ids[0]+`/verdict"`, `name="verdict"`, `name="note"`, "Say how the check #"+ids[0]+" ended",
	)
	taken := rowOfCheck(t, body, ids[1])
	wantOnPage(t, "a check a session took", taken, wordingOf(vocVisualStatus, "in_progress").Text, "the search page shows results", "taken",
		`data-api="/api/visual-qa/items/`+ids[1]+`/verdict"`)
	// The note of a check a session took names the session, which is not what was seen.
	wantNotOnPage(t, "a check a session took", taken, "session:20261110T090000Z")
	ended := rowOfCheck(t, body, ids[2])
	wantOnPage(t, "a check that ended", ended, wordingOf(vocVisualStatus, "verified").Text, "the invoice prints on one page", "one page, as asked", "ended")
	wantNotOnPage(t, "a check that ended", ended, "data-api=", "Say how the check")
	wantOnPage(t, "a check about no change", rowOfCheck(t, body, ids[3]), "the menu closes with Escape", "none")

	// One status, and a page at a time.
	_, pending := overviewPage(t, srv, "/visual?status=pending")
	wantOnPage(t, "the checks that wait", pending, "the list of widgets shows their names", "the menu closes with Escape")
	wantNotOnPage(t, "the checks that wait", pending, "the search page shows results", "the invoice prints on one page")
	_, firstPage := overviewPage(t, srv, "/visual?limit=2")
	wantOnPage(t, "the first page of two checks", firstPage, "the menu closes with Escape", "the invoice prints on one page", `href="/visual?before=`+ids[2]+`&amp;limit=2"`, "Older")
	wantNotOnPage(t, "the first page of two checks", firstPage, "the search page shows results")
	_, second := overviewPage(t, srv, "/visual?limit=2&before="+ids[2])
	wantOnPage(t, "the second page", second, "the search page shows results", "the list of widgets shows their names", "Latest", "These are the oldest.")

	// The API returns the same read.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/visual?status=in_progress", nil)
	items, _ := data["items"].([]any)
	counts, _ := data["counts"].(map[string]any)
	setup, _ := data["setup"].(map[string]any)
	if code != http.StatusOK || len(items) != 1 || data["status"] != "in_progress" || data["session"] != nil ||
		counts["pending"] != float64(2) || counts["in_progress"] != float64(1) || counts["verified"] != float64(1) || counts["dropped"] != float64(0) ||
		setup["enabled"] != true || setup["where"] != settings.VisualQAEnabled.Where() {
		t.Fatalf("GET /api/visual?status=in_progress = %d %s", code, raw)
	}
	if c, _ := items[0].(map[string]any); c["id"] != ids[1] || c["what"] != "the search page shows results" || c["change_name"] != "add-search" || c["started_at"] == nil {
		t.Errorf("the check a session took in /api/visual = %v", items[0])
	}
	for _, path := range []string{"/api/visual?status=queued", "/api/visual?before=x", "/api/visual?limit=0"} {
		code, data, raw, _ := doReq(t, srv, http.MethodGet, path, nil)
		if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
			t.Errorf("GET %s = %d %s", path, code, raw)
		}
	}

	// A session opens: the page says since when and until when, offers to end it, and keeps itself
	// up to date.
	if _, err := st.AcquireLock(ctx, "visual_qa", store.LockOptions{OwnerType: "visual_qa", PID: os.Getpid(), Stamp: "20261110T090000Z", TTL: 70 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	whole, body = overviewPage(t, srv, "/visual")
	wantOnPage(t, "the page of visual QA with a session open", body,
		"A session is open since", "it stops counting as open", `data-post="/api/visual-qa/session/end"`, "End session",
		`href="/visual/sessions/20261110T090000Z"`)
	wantNotOnPage(t, "the page of visual QA with a session open", body, "Begin session")
	if !strings.Contains(whole, `data-live-interval="15"`) || !strings.Contains(body, `id="visual-checks" data-live-region`) {
		t.Errorf("the page of visual QA does not keep itself up to date while a session is open")
	}
	_, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/visual", nil)
	if session, _ := data["session"].(map[string]any); session["stamp"] != "20261110T090000Z" || session["started_at"] == nil || session["expires_at"] == nil {
		t.Errorf("the session in progress in /api/visual = %v in %s", data["session"], raw)
	}

	// An application that does not run visual QA says so, with where it is turned on.
	setVisualQA(t, root, false)
	_, body = overviewPage(t, srv, "/visual")
	wantOnPage(t, "the page of an application that does not run visual QA", body, "does not run visual QA", settings.VisualQAEnabled.Where())
}

// TestAPersonAsksForACheckAndSaysHowItEnded: a check is asked for with what to look at and, when it
// is about one, a change the kit knows; its verdict is recorded once, while it waits. The registry
// gets no change from a name typed in the request.
func TestAPersonAsksForACheckAndSaysHowItEnded(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	ctx := context.Background()

	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/visual-qa/items", map[string]any{"what": "  the totals add up on the invoice  ", "change_name": "add-billing"})
	id := asStr(data["id"])
	if code != http.StatusCreated || id == "" || data["status"] != "pending" || data["what"] != "the totals add up on the invoice" || data["change_name"] != "add-billing" || data["created_at"] == nil {
		t.Fatalf("POST /api/visual-qa/items = %d %s", code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/visual-qa/items", map[string]any{"what": "the footer shows the year"})
	other := asStr(data["id"])
	if code != http.StatusCreated || other == id || data["change_name"] != nil {
		t.Fatalf("a check about no change = %d %s", code, raw)
	}
	_, body := overviewPage(t, srv, "/visual")
	wantOnPage(t, "the check a person asked for", rowOfCheck(t, body, id), "the totals add up on the invoice", `href="/changes/add-billing"`)

	// What the request refuses.
	for name, tc := range map[string]struct {
		body map[string]any
		code int
		kind string
	}{
		"nothing to look at":             {map[string]any{"what": "  "}, http.StatusBadRequest, "invalid_input"},
		"a name that is not of a change": {map[string]any{"what": "x", "change_name": "Not A Change"}, http.StatusBadRequest, "invalid_input"},
		"a change the kit does not know": {map[string]any{"what": "x", "change_name": "no-such-change"}, http.StatusNotFound, "not_found"},
		"a field the action has not":     {map[string]any{"what": "x", "op": "request"}, http.StatusBadRequest, "invalid_input"},
	} {
		code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/visual-qa/items", tc.body)
		if c, _ := errorOf(data); code != tc.code || c != tc.kind {
			t.Errorf("%s: POST /api/visual-qa/items = %d %s", name, code, raw)
		}
	}
	if state, err := st.ChangeState(ctx, "no-such-change"); err != nil || state != "" {
		t.Errorf("a check asked for a change the kit does not know left the change on record as %q (%v)", state, err)
	}

	// Its verdict, once.
	verdict := "/api/visual-qa/items/" + id + "/verdict"
	code, data, raw, _ = doReq(t, srv, http.MethodPost, verdict, map[string]any{"verdict": "regression", "note": "the total is off by one cent"})
	if code != http.StatusOK || data["id"] != id || data["status"] != "regression" || data["verdict_note"] != "the total is off by one cent" || data["resolved_at"] == nil {
		t.Fatalf("POST %s = %d %s", verdict, code, raw)
	}
	_, body = overviewPage(t, srv, "/visual")
	row := rowOfCheck(t, body, id)
	wantOnPage(t, "a check with its verdict", row, wordingOf(vocVisualStatus, "regression").Text, "the total is off by one cent")
	wantNotOnPage(t, "a check with its verdict", row, "data-api=")
	for name, tc := range map[string]struct {
		path string
		body map[string]any
		code int
		kind string
	}{
		"a check that already ended":       {verdict, map[string]any{"verdict": "verified"}, http.StatusConflict, "conflict"},
		"a verdict that is not one":        {"/api/visual-qa/items/" + other + "/verdict", map[string]any{"verdict": "fine"}, http.StatusBadRequest, "invalid_input"},
		"a verdict of the kit of before":   {"/api/visual-qa/items/" + other + "/verdict", map[string]any{"verdict": "passed"}, http.StatusBadRequest, "invalid_input"},
		"no verdict":                       {"/api/visual-qa/items/" + other + "/verdict", map[string]any{"note": "looks fine"}, http.StatusBadRequest, "invalid_input"},
		"a check that is not on record":    {"/api/visual-qa/items/9999/verdict", map[string]any{"verdict": "verified"}, http.StatusNotFound, "not_found"},
		"a number that is not of a check":  {"/api/visual-qa/items/abc/verdict", map[string]any{"verdict": "verified"}, http.StatusNotFound, "not_found"},
		"a field the action does not take": {"/api/visual-qa/items/" + other + "/verdict", map[string]any{"verdict": "verified", "status": "done"}, http.StatusBadRequest, "invalid_input"},
	} {
		code, data, raw, _ := doReq(t, srv, http.MethodPost, tc.path, tc.body)
		if c, _ := errorOf(data); code != tc.code || c != tc.kind {
			t.Errorf("%s: POST %s = %d %s", name, tc.path, code, raw)
		}
	}
	// The other check still waits: nothing above recorded a verdict for it.
	_, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/visual?status=pending", nil)
	if items, _ := data["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != other {
		t.Errorf("the checks that wait after the refused verdicts = %s", raw)
	}

	// An application that does not run visual QA takes no check.
	setVisualQA(t, root, false)
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/visual-qa/items", map[string]any{"what": "the header is sticky"})
	if c, msg := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, settings.VisualQAEnabled.Where()) {
		t.Errorf("a check asked of an application that does not run visual QA = %d %s", code, raw)
	}
}

// TestAPersonOpensAndClosesAVisualSession: beginning a session takes the checks that wait and writes
// their plan, which the page of the session shows; one session is open at a time; ending it gives
// back the checks it did not get to. Each answers 409 when there is nothing for it to do.
func TestAPersonOpensAndClosesAVisualSession(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)
	ids := seedVisual(t, st)

	// No session is open: there is none to end.
	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/visual-qa/session/end", nil)
	if c, _ := errorOf(data); code != http.StatusConflict || c != "conflict" {
		t.Fatalf("ending a session with none open = %d %s", code, raw)
	}

	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/visual-qa/session/begin", nil)
	session, _ := data["session"].(map[string]any)
	stamp := asStr(session["stamp"])
	if code != http.StatusOK || stamp == "" || session["started_at"] == nil || session["expires_at"] == nil {
		t.Fatalf("POST /api/visual-qa/session/begin = %d %s", code, raw)
	}
	// The checks that waited are the session's now.
	_, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/visual", nil)
	counts, _ := data["counts"].(map[string]any)
	if counts["pending"] != float64(0) || counts["in_progress"] != float64(3) {
		t.Errorf("the checks by status once a session began = %v in %s", counts, raw)
	}
	// Its plan is a file of the kit: the list of sessions has it, and its page shows it.
	_, body := overviewPage(t, srv, "/visual/sessions")
	wantOnPage(t, "the sessions of visual QA", body, `href="/visual/sessions/`+stamp+`"`, "open now", "no report", "End session")
	_, body = overviewPage(t, srv, "/visual/sessions/"+stamp)
	wantOnPage(t, "the page of a session", body, stamp, "This session is open now", "Plan", "visual-"+stamp+".md",
		"the list of widgets shows their names", "the menu closes with Escape")
	wantNotOnPage(t, "the page of a session with no report", body, `?file=report`)
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/visual/sessions/"+stamp, nil)
	plan, _ := data["plan"].(map[string]any)
	if code != http.StatusOK || data["stamp"] != stamp || data["open"] != true || data["report"] != nil || !strings.Contains(asStr(plan["text"]), "the list of widgets shows their names") {
		t.Errorf("GET /api/visual/sessions/%s = %d %s", stamp, code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/visual/sessions", nil)
	if sessions, _ := data["sessions"].([]any); code != http.StatusOK || len(sessions) != 1 || data["total"] != float64(1) || sessions[0].(map[string]any)["stamp"] != stamp {
		t.Errorf("GET /api/visual/sessions = %d %s", code, raw)
	}

	// One session at a time.
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/visual-qa/session/begin", nil)
	if c, msg := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, stamp) {
		t.Errorf("beginning a session with one open = %d %s", code, raw)
	}

	// The session writes its report, and a person records the verdict of one check before it ends.
	report := store.RuntimePath(root, "cycle", "reports", "visual-"+stamp+".md")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("# Visual QA\n\nThe list of widgets shows their names: verified.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, raw, _ := doReq(t, srv, http.MethodPost, "/api/visual-qa/items/"+ids[0]+"/verdict", map[string]any{"verdict": "verified", "note": "names shown"}); code != http.StatusOK {
		t.Fatalf("the verdict of a check of the session = %d %s", code, raw)
	}
	_, body = overviewPage(t, srv, "/visual/sessions/"+stamp)
	wantOnPage(t, "the page of a session with its report", body, "Report", "Plan", "The list of widgets shows their names: verified.", `href="/visual/sessions/`+stamp+`?file=plan"`)
	_, body = overviewPage(t, srv, "/visual/sessions/"+stamp+"?file=plan")
	wantOnPage(t, "the plan of a session with a report", body, "the menu closes with Escape")
	if code, _, _ := getPage(t, srv, "/visual/sessions/"+stamp+"?file=log"); code != http.StatusBadRequest {
		t.Errorf("GET a file a session has not = %d, want 400", code)
	}

	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/visual-qa/session/end", nil)
	if code != http.StatusOK || !strings.Contains(asStr(data["output"]), stamp) {
		t.Fatalf("POST /api/visual-qa/session/end = %d %s", code, raw)
	}
	// The check it did not get to waits again, and the one with its verdict keeps it. The check
	// another session took before (seedVisual) is still that session's.
	_, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/visual", nil)
	counts, _ = data["counts"].(map[string]any)
	if data["session"] != nil || counts["pending"] != float64(1) || counts["in_progress"] != float64(1) || counts["verified"] != float64(2) {
		t.Errorf("the checks once the session ended = %v, session %v, in %s", counts, data["session"], raw)
	}
	_, body = overviewPage(t, srv, "/visual/sessions")
	wantOnPage(t, "the sessions once the session ended", body, `href="/visual/sessions/`+stamp+`"`, "it wrote its report", "Begin session")
	wantNotOnPage(t, "the sessions once the session ended", body, "open now")

	// A paused kit opens no session; nor does a stamp that is not one name a file.
	if _, err := st.Pause(context.Background(), "maintenance", "cli", ""); err != nil {
		t.Fatal(err)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/visual-qa/session/begin", nil)
	if c, msg := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "paused") {
		t.Errorf("beginning a session of a paused kit = %d %s", code, raw)
	}
	for _, path := range []string{"/visual/sessions/20200101T000000Z", "/visual/sessions/..%2Fconfig", "/visual/20261108T120000Z"} {
		if code, _, _ := getPage(t, srv, path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}
