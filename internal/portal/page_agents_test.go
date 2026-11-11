package portal

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// seedAgents adds to the executions of seedExecutions what the registry has of the engines: claude
// answered its last ping, a run found codex out of quota and set it apart for half an hour, and
// this process runs claude for the BUILD of add-search.
func seedAgents(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.RecordEngineHealth(ctx, store.EngineHealthRecord{Engine: "claude", Installed: true, Status: "ok", LastLatencyMS: 420}))
	must(st.RecordEngineHealth(ctx, store.EngineHealthRecord{Engine: "codex", Installed: true, Status: "quota_exceeded", LastError: "hit your usage limit",
		CooldownUntil: time.Now().Add(30 * time.Minute).Unix()}))
	must(st.ClaimAgent(store.WithExecution(ctx, "build-live"), store.AgentClaimID(1, os.Getpid()), "claude", "build", "add-search"))
}

// rowOfAgent returns the row of an engine in the page of agents.
func rowOfAgent(t *testing.T, body, name string) string {
	t.Helper()
	start := strings.Index(body, `<p class="font-mono font-semibold">`+name+`</p>`)
	if start < 0 {
		t.Fatalf("the page of agents has no row for %s:\n%s", name, textOf(body))
	}
	rest := body[start:]
	if end := strings.Index(rest, `</tr>`); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// TestAgentsShowsEachEngineWithItsHealthWhoRunsItAndItsAttempts: the page lists the engines this
// application uses and the ones the registry knows, each with its health as the last check left it,
// the processes that run it now, and its attempts since a date: how they ended, how many times the
// idle watchdog stopped it and the longest pause it came back from. The application's own setup is
// named with the place a person changes it at, and nothing is counted from a log.
func TestAgentsShowsEachEngineWithItsHealthWhoRunsItAndItsAttempts(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedExecutions(t, st)
	seedAgents(t, st)

	whole, body := overviewPage(t, srv, "/agents")
	wantOnPage(t, "the page of agents", body,
		"Agents", "24 hours", "7 days", "30 days", "Everything", `href="/executions/new"`, "New agent run",
		// How the application runs its agents, each setting with where it is written.
		settings.AgentSelection.Where(), settings.AgentPermissions.Where(), settings.IdleTimeout.Where(),
		"It stops an agent that shows no activity for",
	)
	// The page keeps itself up to date while a process runs an engine.
	if !strings.Contains(whole, `data-live-interval="10"`) || !strings.Contains(body, `id="agents-list" data-live-region`) {
		t.Errorf("the page of agents does not keep itself up to date while an engine is in use")
	}

	// claude: this application uses it, it answered its ping, this process runs it for a BUILD whose
	// agent shows no activity, and of its three attempts one ended well, one was stopped for
	// inactivity and one is still going.
	claude := rowOfAgent(t, body, "claude")
	wantOnPage(t, "the row of claude", claude,
		"used here", wordingOf(vocEngineHealth, "ok").Text, "answered in", "420ms",
		`href="/executions/build-live"`, "build", `href="/changes/add-search"`,
		"No activity for", "it is stopped at",
		"1 still going", "1 ended well, 50% of the 2 that ended", wordingOf(vocAttemptOutcome, "idle_stalled").Text,
		"Stopped 1 time", "in 2 pauses",
		`data-post="/api/engines/claude/ping"`, `data-refresh="regions"`,
	)
	wantNotOnPage(t, "the row of claude", claude, "leaves it for last", "not one the kit ships")

	// codex: the application does not use it, and it is on the page because the registry knows it: a
	// run found it out of quota and the kit leaves it for last.
	codex := rowOfAgent(t, body, "codex")
	wantOnPage(t, "the row of codex", codex,
		wordingOf(vocEngineHealth, "quota_exceeded").Text, "hit your usage limit", "The kit leaves it for last, and takes it back",
		"1 ended well, 50% of the 2 that ended", wordingOf(vocAttemptOutcome, "error").Text, "no pause",
		`data-post="/api/engines/codex/ping"`,
	)
	wantNotOnPage(t, "the row of codex", codex, "used here", "still going")

	// hermes: the application uses it and nothing checked it or made it try a prompt: the agent runs
	// of the seed never got to an attempt.
	hermes := rowOfAgent(t, body, "hermes")
	wantOnPage(t, "the row of hermes", hermes, "used here", wordingOf(vocEngineHealth, "unknown").Text, "none in this period")
	// The engines this application uses come first.
	at := func(name string) int { return strings.Index(body, `<p class="font-mono font-semibold">`+name+`</p>`) }
	if !(at("claude") < at("hermes") && at("hermes") < at("codex")) {
		t.Errorf("the rows are not the engines the application uses and then the one the registry knows: claude at %d, hermes at %d, codex at %d",
			at("claude"), at("hermes"), at("codex"))
	}

	// Every engine the kit can run is listed when the address asks for all of them.
	_, all := overviewPage(t, srv, "/agents?show=all")
	for name := range cycle.SupportedAgents {
		rowOfAgent(t, all, name)
	}
	if n := strings.Count(all, `<p class="font-mono font-semibold">`); n != len(cycle.SupportedAgents) {
		t.Errorf("the page lists %d engines when asked for all, want the %d the kit can run", n, len(cycle.SupportedAgents))
	}
	wantOnPage(t, "the page with every engine", all, `href="/agents"`, "Leave out the engines")

	// A period with no attempt: the registry still says how each engine is.
	_, later := overviewPage(t, srv, "/agents?since=2100-01-01T00:00:00Z")
	wantOnPage(t, "a period with no attempt", rowOfAgent(t, later, "claude"), wordingOf(vocEngineHealth, "ok").Text, "none in this period")

	// The API returns the same read.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/agents?since=all", nil)
	agents, _ := data["agents"].([]any)
	if code != http.StatusOK || len(agents) == 0 || data["since"] != nil {
		t.Fatalf("GET /api/agents?since=all = %d %s", code, raw)
	}
	byName := map[string]map[string]any{}
	for _, it := range agents {
		a, _ := it.(map[string]any)
		byName[asStr(a["name"])] = a
	}
	hidden, _ := data["hidden"].(float64)
	if len(agents)+int(hidden) != len(cycle.SupportedAgents) {
		t.Errorf("/api/agents lists %d engines and leaves out %v, and the kit can run %d", len(agents), data["hidden"], len(cycle.SupportedAgents))
	}
	c := byName["claude"]
	stat, _ := c["stat"].(map[string]any)
	inUse, _ := c["in_use"].([]any)
	idle, _ := c["idle"].([]any)
	if c["health"] != "ok" || c["selected"] != true || c["known"] != true || len(inUse) != 1 || len(idle) != 1 ||
		stat["attempts"] != float64(3) || stat["running"] != float64(1) || stat["stalls"] != float64(1) || stat["idle_periods"] != float64(2) {
		t.Errorf("claude in /api/agents = %v", c)
	}
	if use, _ := inUse[0].(map[string]any); use["role"] != "build" || use["change_name"] != "add-search" || use["exec_id"] != "build-live" || use["owner_pid"] != float64(os.Getpid()) {
		t.Errorf("the process that runs claude = %v", inUse[0])
	}
	if x := byName["codex"]; x["health"] != "quota_exceeded" || x["selected"] != false || x["cooldown_until"] == nil || x["last_error"] != "hit your usage limit" {
		t.Errorf("codex in /api/agents = %v", x)
	}
	setup, _ := data["setup"].(map[string]any)
	limit, _ := setup["idle_timeout"].(map[string]any)
	if limit["where"] != settings.IdleTimeout.Where() || limit["value"] == "" {
		t.Errorf("the idle limit in /api/agents = %v", setup["idle_timeout"])
	}

	// A setting the kit cannot use is shown as that, with what is wrong: the page puts no value in
	// its place.
	agentsFile := filepath.Join(root, settings.AgentSelection.FilePath())
	written, err := os.ReadFile(agentsFile)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(written), "\n") {
		if !strings.HasPrefix(line, settings.AgentSelection.Path()+":") {
			kept = append(kept, line)
		}
	}
	if len(kept) == len(strings.Split(string(written), "\n")) {
		t.Fatalf("%s does not write %s", agentsFile, settings.AgentSelection.Path())
	}
	if err := os.WriteFile(agentsFile, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	_, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/agents", nil)
	setup, _ = data["setup"].(map[string]any)
	if selection, _ := setup["selection"].(map[string]any); selection["value"] != "" || selection["problem"] != "missing" || selection["where"] != settings.AgentSelection.Where() {
		t.Errorf("a selection the file does not write = %v in %s", setup["selection"], raw)
	}
	_, body = overviewPage(t, srv, "/agents")
	wantOnPage(t, "the page of agents with a setting that is missing", body, "missing", settings.AgentSelection.Where())

	// What the address cannot ask for.
	for _, path := range []string{"/api/agents?since=yesterday", "/api/agents?show=missing"} {
		code, data, raw, _ := doReq(t, srv, http.MethodGet, path, nil)
		if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
			t.Errorf("GET %s = %d %s", path, code, raw)
		}
	}
}

// TestAPingRecordsTheHealthOfTheEngine: a person pings an engine from the portal. The ping answers
// 200 whether the engine answered or not, with how it went and the engine as the registry has it
// afterwards; the page then shows that health. A name the kit runs no engine under is 404.
func TestAPingRecordsTheHealthOfTheEngine(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	seedExecutions(t, st)
	seedAgents(t, st)
	kit := kitOf(srv)

	// An engine that answers: healthy, and no longer set apart.
	kit.ping = func(_ context.Context, _ string, engine string) PingResult {
		return PingResult{Engine: engine, Status: "ok", Health: "ok", LatencyMS: 5, Output: "pong"}
	}
	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/engines/codex/ping", nil)
	engine, _ := data["engine"].(map[string]any)
	ping, _ := data["ping"].(map[string]any)
	if code != http.StatusOK || data["ok"] != true || data["summary"] != "codex answered in 5ms" || ping["output"] != "pong" ||
		engine["engine"] != "codex" || engine["health"] != "ok" || engine["cooldown_until"] != nil || engine["last_error"] != nil {
		t.Fatalf("POST /api/engines/codex/ping = %d %s", code, raw)
	}
	_, body := overviewPage(t, srv, "/agents")
	wantOnPage(t, "codex after a ping that answered", rowOfAgent(t, body, "codex"), wordingOf(vocEngineHealth, "ok").Text, "answered in", "5ms")
	wantNotOnPage(t, "codex after a ping that answered", rowOfAgent(t, body, "codex"), "leaves it for last", "hit your usage limit")

	// An engine that does not: the ping did what it was asked, and says what it found. A kit that
	// only says the ping failed leaves the engine as misconfigured.
	kit.ping = func(_ context.Context, _ string, engine string) PingResult {
		return PingResult{Engine: engine, Status: "failed", Health: "auth_failed", Code: 1, LatencyMS: 900, Output: "please run /login"}
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/engines/claude/ping", nil)
	engine, _ = data["engine"].(map[string]any)
	if code != http.StatusOK || data["ok"] != false || data["summary"] != "claude did not answer: exit 1: please run /login" ||
		engine["health"] != "auth_failed" || engine["last_error"] != "exit 1: please run /login" || engine["consecutive_failures"] != float64(1) {
		t.Fatalf("a ping that failed = %d %s", code, raw)
	}
	_, body = overviewPage(t, srv, "/agents")
	wantOnPage(t, "claude after a ping that failed", rowOfAgent(t, body, "claude"), wordingOf(vocEngineHealth, "auth_failed").Text, "exit 1: please run /login")
	kit.ping = func(_ context.Context, _ string, engine string) PingResult {
		return PingResult{Engine: engine, Status: "failed", Code: 2}
	}
	if _, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/engines/hermes/ping", nil); data["ok"] != false || data["engine"].(map[string]any)["health"] != "config_error" {
		t.Errorf("a ping whose kit says only that it failed = %s", raw)
	}

	// The stand-in of the simulations is pinged without the kit: it is there when AGENT_STUB names it.
	t.Setenv("AGENT_STUB", "")
	if code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/engines/stub/ping", nil); code != http.StatusOK || data["ok"] != false ||
		data["ping"].(map[string]any)["status"] != "not_installed" {
		t.Errorf("POST /api/engines/stub/ping with no AGENT_STUB = %d %s", code, raw)
	}

	// What the action refuses: an engine the kit does not run, a body with a field, another method.
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/engines/nope/ping", nil)
	if c, msg := errorOf(data); code != http.StatusNotFound || c != "not_found" || !strings.Contains(msg, "nope") {
		t.Errorf("a ping of an engine the kit does not run = %d %s", code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/engines/claude/ping", map[string]any{"agent": "codex"})
	if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
		t.Errorf("a ping with a body = %d %s", code, raw)
	}
	if code, _, _, hdr := doReq(t, srv, http.MethodGet, "/api/engines/claude/ping", nil); code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "POST" {
		t.Errorf("GET on the ping = %d (Allow %q), want 405 with Allow: POST", code, hdr.Get("Allow"))
	}
}
