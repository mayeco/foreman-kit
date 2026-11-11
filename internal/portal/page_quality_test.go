package portal

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// qualitySeed is what seedQuality recorded that a test asks for by id.
type qualitySeed struct {
	// specsRound is the round in which the specs of add-ui did not validate.
	specsRound int64
}

// seedQuality records what the gates and the evaluator found, on top of the gates of seedExecutions
// (backend · unit-tests failed once and passed once):
//
//	backend   unit-tests passes once more, and lint passes twice
//	openspec  the specs validate twice; the specs of add-api validate, and those of add-ui fail
//	          and then validate
//	frontend  e2e failed forty days ago, and has not run since
//
// and six questions to the evaluator: change_risk approved two changes and sent one of those to a
// person, and rejected a third while the gate was advisory; plan_review approved one plan and
// could not tell about another; scenario_coverage could not be reached.
func seedQuality(t *testing.T, st *store.Store) qualitySeed {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	gate := func(change, repoName, repoPath, name string, exitCode int, ms int64, at time.Time) int64 {
		t.Helper()
		round, err := st.BeginGateRound(ctx, store.GateRound{Context: "cli", ChangeName: change})
		must(err)
		rec := store.GateRunRecord{ChangeName: change, RepoName: repoName, RepoPath: repoPath, GateName: name,
			Command: "make " + name, ExitCode: exitCode, DurationMS: ms}
		if !at.IsZero() {
			rec.CreatedAt = at.UTC().Format(time.RFC3339)
		}
		if exitCode != 0 {
			rec.FailureExcerpt = "FAIL " + name
		}
		must(st.RecordGateRun(store.WithGateRound(ctx, round), rec))
		result := "passed"
		if exitCode != 0 {
			result = "failed"
		}
		must(st.FinishGateRound(ctx, round, result))
		return round
	}
	var seed qualitySeed
	gate("add-api", "backend", "", "unit-tests", 0, 1500, time.Time{})
	gate("add-api", "backend", "", "lint", 0, 300, time.Time{})
	gate("add-ui", "backend", "", "lint", 0, 500, time.Time{})
	gate("add-api", "openspec", store.UmbrellaRepoPath, "specs", 0, 600, time.Time{})
	gate("add-ui", "openspec", store.UmbrellaRepoPath, "specs", 0, 800, time.Time{})
	gate("add-api", "openspec", store.UmbrellaRepoPath, "add-api", 0, 700, time.Time{})
	seed.specsRound = gate("add-ui", "openspec", store.UmbrellaRepoPath, "add-ui", 1, 900, time.Time{})
	gate("add-ui", "openspec", store.UmbrellaRepoPath, "add-ui", 0, 500, time.Time{})
	gate("add-ui", "frontend", "repos/frontend", "e2e", 1, 65000, time.Now().Add(-40*24*time.Hour))

	ask := func(ev store.Evaluation) int64 {
		t.Helper()
		id, err := st.RecordEvaluation(ctx, ev, nil)
		must(err)
		return id
	}
	ask(store.Evaluation{Gate: "change_risk", Mode: "enforce", Status: "passed", ChangeName: "add-api", CostUSD: 0.01})
	risky := ask(store.Evaluation{Gate: "change_risk", Mode: "enforce", Status: "passed", ChangeName: "add-billing", CostUSD: 0.02})
	must(st.SetEvaluationRoute(ctx, risky, "risk_review", map[string]any{"rules": "risk=high"}))
	ask(store.Evaluation{Gate: "change_risk", Mode: "advisory", Status: "failed", ChangeName: "add-ui", Cached: true})
	ask(store.Evaluation{Gate: "plan_review", Mode: "enforce", Status: "passed", ChangeName: "add-api"})
	ask(store.Evaluation{Gate: "plan_review", Mode: "enforce", Status: "abstained", ChangeName: "add-ui"})
	ask(store.Evaluation{Gate: "scenario_coverage", Mode: "advisory", Status: "unavailable", ChangeName: "add-ui"})
	return seed
}

// markedPeriodRe finds the period that leads to href, marked as the one the page shows.
func markedPeriodRe(href string) *regexp.Regexp {
	return regexp.MustCompile(`<a role="tab" href="` + regexp.QuoteMeta(href) + `"[^>]*aria-selected="true"`)
}

// rowOf returns the row of a table that shows name, as a person reads it.
func rowOf(t *testing.T, body, name string) string {
	t.Helper()
	for _, row := range strings.Split(body, "<tr")[1:] {
		if end := strings.Index(row, "</tr>"); end >= 0 {
			row = row[:end]
		}
		if text := textOf("<tr" + row); strings.Contains(text, name) {
			return text
		}
	}
	t.Fatalf("no row of the page shows %q:\n%s", name, textOf(body))
	return ""
}

// wantInRow fails when the row that shows name does not show the facts, each after the one before.
func wantInRow(t *testing.T, body, name string, facts ...string) {
	t.Helper()
	row := rowOf(t, body, name)
	rest := row
	for _, fact := range facts {
		at := strings.Index(rest, fact)
		if at < 0 {
			t.Errorf("the row of %s does not show %q after what comes before it in %q: %s", name, fact, facts, row)
			return
		}
		rest = rest[at+len(fact):]
	}
}

// TestAssessments: the page says, for each gate of each repository, how many times it ran,
// how many of those failed and how long they took, and leads to the round of its last failure; and
// for each gate of the evaluator, how it answered and what was done about its answers. The numbers
// are of a period, which is part of the address.
func TestAssessments(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seed := seedOverview(t, srv, st, root)
	quality := seedQuality(t, st)

	whole, body := overviewPage(t, srv, "/quality")
	wantOnPage(t, "the assessments", body,
		"Assessments", "what was recorded since",
		// The period is part of the address, and the one in force is marked.
		`href="/quality?since=24h"`, `href="/quality"`, `href="/quality?since=30d"`, `href="/quality?since=all"`,
		"24 hours", "7 days", "30 days", "Everything",
		// What the gates add up to.
		"10 runs of 4 gates in 2 repositories", "2 failed", "20% of them", "Together they took", "14s",
		// What the evaluator adds up to.
		"6 questions on 3 gates: it approved 3", "50% of them", "1 of its answers came from the cache", "$0.0300",
		"The orchestrator acts on the answers of an enforced gate", `href="/events?event=evaluator.answer"`,
	)
	if n := strings.Count(body, `aria-selected="true"`); n != 1 || !markedPeriodRe("/quality").MatchString(body) {
		t.Errorf("the page marks %d periods, want the one of the address with no period: 7 days", n)
	}
	// A gate that ran forty days ago is not of the last seven.
	wantNotOnPage(t, "the gates of seven days", body, "frontend", "No gate ran in this period.", "The evaluator was not asked in this period.")

	// Each gate, with the round of its last failure one click away.
	wantInRow(t, body, "backend · unit-tests", "3", "1", "33%", "3.2s", "4.2s", "9.6s")
	wantInRow(t, body, "backend · lint", "2", "none", "400ms", "500ms", "800ms", "none in this period")
	wantInRow(t, body, "openspec · specs", "2", "none", "700ms", "800ms", "1.4s", "none in this period")
	// The umbrella validates the specs of each change with a gate named after it: they are one gate.
	wantInRow(t, body, "openspec · the specs of each change", "validated for 2 changes", "3", "1", "33%", "700ms", "900ms", "2.1s")
	wantNotOnPage(t, "the gates", body, "openspec · add-api", "openspec · add-ui")
	wantOnPage(t, "the gates", body,
		fmt.Sprintf(`href="/gates/%d"`, seed.failedRound), fmt.Sprintf(", in the round of gates #%d", seed.failedRound),
		fmt.Sprintf(`href="/gates/%d"`, quality.specsRound))
	inOrder(t, "the gates, by repository and then by gate", body,
		"backend · lint", "backend · unit-tests", "openspec · the specs of each change", "openspec · specs")

	// Each gate of the evaluator: an approved answer can still have been acted on.
	wantInRow(t, body, "change risk", "advisory for 1", "enforced for 2", "3", "2", "67%", "rejected", "1", "risk review", "1", "1", "$0.0300")
	wantInRow(t, body, "plan review", "enforced", "2", "1", "50%", "could not tell", "1", "none", "0", "$0.0000")
	wantInRow(t, body, "scenario coverage", "advisory", "1", "0", "0%", "unavailable", "1", "none", "0", "$0.0000")
	if row := rowOf(t, body, "plan review"); strings.Contains(row, "enforced for") {
		t.Errorf("a gate asked in one mode says for how many: %s", row)
	}

	// The page starts nothing and does not ask for itself again: its numbers are of a period.
	if strings.Contains(body, "btn-primary") || strings.Contains(body, "data-post") || strings.Contains(whole, "data-live-interval") {
		t.Errorf("the page of the assessments has an action or keeps itself up to date")
	}
	// The menu leads to it.
	wantOnPage(t, "the menu", whole, `href="/quality"`, "Assessments")

	// Everything on record has the gate that failed forty days ago, with how long it took.
	_, body = overviewPage(t, srv, "/quality?since=all")
	wantOnPage(t, "the gates of everything on record", body, "everything on record", "11 runs of 5 gates in 3 repositories", "3 failed", "27% of them")
	wantInRow(t, body, "frontend · e2e", "1", "1", "100%", "1m 05s", "1m 05s", "1m 05s")
	wantNotOnPage(t, "the gates of everything on record", body, "what was recorded since")
	if !markedPeriodRe("/quality?since=all").MatchString(body) {
		t.Errorf("the page of everything on record does not mark its period")
	}
	// A period no button offers is a period all the same, and none of the buttons is marked.
	_, body = overviewPage(t, srv, "/quality?since=60d")
	wantOnPage(t, "the gates of sixty days", body, "frontend · e2e", "11 runs of 5 gates")
	if strings.Contains(body, `aria-selected="true"`) {
		t.Errorf("a period no button offers marks one of them")
	}

	// A period in which nothing was recorded says so, twice.
	_, body = overviewPage(t, srv, "/quality?since=2100-01-01")
	wantOnPage(t, "a period with nothing", body, "No gate ran in this period.", "The evaluator was not asked in this period.")
	wantNotOnPage(t, "a period with nothing", body, "<table", "Together they took")

	if code, page, _ := getPage(t, srv, "/quality?since=yesterday"); code != http.StatusBadRequest || !strings.Contains(textOf(page), `"yesterday" is none of them`) {
		t.Errorf("GET /quality?since=yesterday = %d, want 400 with what a period is", code)
	}
}

// TestAssessmentsInTheAPI: /api/quality returns what the page draws.
func TestAssessmentsInTheAPI(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seed := seedOverview(t, srv, st, root)
	quality := seedQuality(t, st)

	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/quality", nil)
	gates, _ := data["gates"].([]any)
	evaluations, _ := data["evaluations"].([]any)
	if code != http.StatusOK || len(gates) != 4 || len(evaluations) != 3 {
		t.Fatalf("GET /api/quality = %d %s", code, raw)
	}
	since, err := time.Parse(time.RFC3339, asStr(data["since"]))
	if age := time.Since(since); err != nil || age < 7*24*time.Hour-time.Minute || age > 7*24*time.Hour+time.Minute {
		t.Errorf("since of /api/quality = %v (%v), want seven days ago", data["since"], err)
	}
	byGate := map[string]map[string]any{}
	for _, it := range gates {
		g, _ := it.(map[string]any)
		byGate[asStr(g["repo_name"])+"/"+asStr(g["gate_name"])] = g
	}
	unit := byGate["backend/unit-tests"]
	if unit["runs"] != float64(3) || unit["failures"] != float64(1) || unit["avg_ms"] != float64(3200) || unit["max_ms"] != float64(4200) ||
		unit["total_ms"] != float64(9600) || unit["last_failed_round_id"] != float64(seed.failedRound) || asStr(unit["last_failed_at"]) == "" {
		t.Errorf("backend/unit-tests in /api/quality = %v", unit)
	}
	changes := byGate["openspec/"]
	if changes["changes"] != float64(2) || changes["runs"] != float64(3) || changes["failures"] != float64(1) ||
		changes["last_failed_round_id"] != float64(quality.specsRound) || changes["repo_path"] != store.UmbrellaRepoPath {
		t.Errorf("the validation of the changes in /api/quality = %v", changes)
	}
	if lint := byGate["backend/lint"]; lint["failures"] != float64(0) || lint["last_failed_round_id"] != nil || lint["changes"] != nil {
		t.Errorf("backend/lint in /api/quality = %v", lint)
	}
	byName := map[string]map[string]any{}
	for _, it := range evaluations {
		e, _ := it.(map[string]any)
		byName[asStr(e["gate"])] = e
	}
	risk := byName["change_risk"]
	statuses, _ := risk["by_status"].(map[string]any)
	modes, _ := risk["by_mode"].(map[string]any)
	routes, _ := risk["by_route"].(map[string]any)
	if risk["evaluations"] != float64(3) || risk["passed"] != float64(2) || risk["cached"] != float64(1) || statuses["failed"] != float64(1) ||
		modes["enforce"] != float64(2) || modes["advisory"] != float64(1) || routes["risk_review"] != float64(1) {
		t.Errorf("change_risk in /api/quality = %v", risk)
	}
	if plan := byName["plan_review"]; plan["by_route"] != nil {
		t.Errorf("plan_review in /api/quality = %v, want no action", plan)
	}

	// Everything on record has no instant it starts from; an instant is a period, too.
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/quality?since=all", nil)
	if gates, _ := data["gates"].([]any); code != http.StatusOK || len(gates) != 5 || data["since"] != nil {
		t.Errorf("GET /api/quality?since=all = %d %s", code, raw)
	}
	at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/quality?since="+at, nil)
	if gates, _ := data["gates"].([]any); code != http.StatusOK || len(gates) != 4 || data["since"] != at {
		t.Errorf("GET /api/quality?since=%s = %d %s", at, code, raw)
	}
	// A period with nothing answers two empty lists, not two nulls.
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/quality?since=2100-01-01", nil)
	gates, isList := data["gates"].([]any)
	evaluations, alsoList := data["evaluations"].([]any)
	if code != http.StatusOK || !isList || !alsoList || len(gates) != 0 || len(evaluations) != 0 {
		t.Errorf("GET /api/quality?since=2100-01-01 = %d %s", code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/quality?since=yesterday", nil)
	if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
		t.Errorf("GET /api/quality?since=yesterday = %d %s", code, raw)
	}
}

// TestAssessmentsOfAWorkspaceWithNothing: before a gate runs and the evaluator is asked,
// the page says when each of them will be.
func TestAssessmentsOfAWorkspaceWithNothing(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	_, body := overviewPage(t, srv, "/quality")
	wantOnPage(t, "the gates of a workspace with nothing", body, "No gate ran in this period.", "A gate runs when a worker verifies what it built",
		"The evaluator was not asked in this period.")
}

// TestTimesMeasuredInMillisecondsAndShares: the run of a gate keeps a tenth of a second under ten
// seconds, and a share never rounds something that happened down to nothing, nor something that
// did not always happen up to always.
func TestTimesMeasuredInMillisecondsAndShares(t *testing.T) {
	for ms, want := range map[int64]string{
		0: "0ms", 850: "850ms", 999: "999ms", 1000: "1.0s", 1066: "1.1s", 2672: "2.7s", 9940: "9.9s", 9960: "10s", 42000: "42s", 65000: "1m 05s",
	} {
		if got := millisText(ms); got != want {
			t.Errorf("millisText(%d) = %q, want %q", ms, got, want)
		}
	}
	for _, c := range []struct {
		part, whole int
		want        string
	}{
		{0, 71, "0%"}, {3, 71, "4%"}, {1, 3, "33%"}, {2, 3, "67%"}, {71, 71, "100%"}, {1, 1000, "under 1%"}, {999, 1000, "over 99%"}, {5, 0, ""},
	} {
		if got := shareText(c.part, c.whole); got != c.want {
			t.Errorf("shareText(%d, %d) = %q, want %q", c.part, c.whole, got, c.want)
		}
	}
}
