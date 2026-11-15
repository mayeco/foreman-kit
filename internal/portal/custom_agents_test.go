package portal

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// declareCustomAgent writes an agents.yaml in root that declares the agent my-agent the way an
// application does for its own binary: under commands:, in agents: and in the plan and build roles.
// The binary is a script that prints pong, so nothing real runs. It returns the name of the agent.
func declareCustomAgent(t *testing.T, root string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "my-agent")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho pong\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "agents: [my-agent, claude]\n" +
		"roles:\n" +
		"  plan:\n    agents: [my-agent]\n    timeout: 30m\n    args: {}\n" +
		"  build:\n    agents: [my-agent, claude]\n    timeout: 2h\n    args: {}\n" +
		"  cycle:\n    agents: [claude]\n    timeout: 1h\n    args: {}\n" +
		"commands:\n  my-agent:\n    run: " + script + " run {{prompt}}\n    ping: " + script + " ping {{prompt}}\n"
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "agents.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return "my-agent"
}

func TestARunStartsOnADeclaredCustomAgent(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	agent := declareCustomAgent(t, root)
	given := make(chan string, 4)
	kitOf(srv).agent = func(_ context.Context, _, engine, _ string) (string, int, error) {
		given <- engine
		return "done", 0, nil
	}

	code, body, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{"prompt": "say hello", "engine": agent})
	id, ex := startedOf(t, code, body, raw)
	if got := <-given; got != agent || ex["engines_requested"] != agent {
		t.Errorf("a run on %s: the kit was given %q and the execution asked for %v", agent, got, ex["engines_requested"])
	}
	waitExecution(t, st, id)
	waitWork(t, srv.exec, id)
}

func TestARunOnTheDefaultEngineCanBeRepeatedAndContinuedWhenItIsACustomAgent(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	agent := declareCustomAgent(t, root)
	given := make(chan string, 4)
	kitOf(srv).agent = func(_ context.Context, _, engine, _ string) (string, int, error) {
		given <- engine
		return "done", 0, nil
	}

	// No engine is asked for: the run goes to the first agent of the application, a custom one.
	code, body, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{"prompt": "say hello"})
	first, ex := startedOf(t, code, body, raw)
	if ex["engines_requested"] != agent || <-given != agent {
		t.Fatalf("a run with no engine recorded %v, want %s", ex["engines_requested"], agent)
	}
	waitExecution(t, st, first)
	waitWork(t, srv.exec, first)

	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/executions/"+first+"/rerun", map[string]any{})
	again, ex := startedOf(t, code, body, raw)
	if ex["engines_requested"] != agent || <-given != agent {
		t.Errorf("the rerun recorded %v, want %s", ex["engines_requested"], agent)
	}
	waitExecution(t, st, again)
	waitWork(t, srv.exec, again)

	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/executions/"+first+"/continue", map[string]any{"prompt": "and again"})
	cont, ex := startedOf(t, code, body, raw)
	if ex["engines_requested"] != agent || <-given != agent {
		t.Errorf("the follow-up recorded %v, want %s", ex["engines_requested"], agent)
	}
	waitExecution(t, st, cont)
	waitWork(t, srv.exec, cont)
}

func TestAnUndeclaredEngineIsStillRefused(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	declareCustomAgent(t, root)
	before, err := st.ListExecutions(context.Background(), "portal_run", "", 100)
	if err != nil {
		t.Fatal(err)
	}

	code, body, raw, _ := doReq(t, srv, http.MethodPost, "/api/executions", map[string]any{"prompt": "x", "engine": "not-declared"})
	if c, msg := errorOf(body); code != http.StatusBadRequest || c != "invalid_input" || !strings.Contains(msg, "not-declared") {
		t.Errorf("a run on an undeclared engine = %d %s, want 400", code, raw)
	}
	code, body, raw, _ = doReq(t, srv, http.MethodPost, "/api/engines/not-declared/ping", nil)
	if c, msg := errorOf(body); code != http.StatusNotFound || c != "not_found" || !strings.Contains(msg, "not-declared") {
		t.Errorf("a ping of an undeclared engine = %d %s, want 404", code, raw)
	}
	// A name that would split the list the kit is given is no engine, even when the file names it.
	for _, name := range []string{"a,b", "two words"} {
		if srv.exec.declares(name) {
			t.Errorf("%q counts as declared", name)
		}
	}
	after, err := st.ListExecutions(context.Background(), "portal_run", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("a refused run wrote %d execution rows", len(after)-len(before))
	}
}

func TestEnginesListsTheDeclaredCustomAgentsWithTheirRoles(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	agent := declareCustomAgent(t, root)

	var found *EngineChoice
	engines := srv.exec.Engines()
	for i := range engines {
		if engines[i].Name == agent {
			found = &engines[i]
		}
	}
	if found == nil {
		t.Fatalf("the engines %+v leave out the declared agent %s", engines, agent)
	}
	if !found.Selected || !found.Installed || found.Label == "" || strings.Join(found.Roles, ",") != "plan,build" {
		t.Errorf("the declared agent as an engine = %+v, want selected, installed, with the roles plan and build", *found)
	}
	if got := srv.exec.DefaultEngine(); got != agent {
		t.Errorf("the default engine = %q, want the first agent of the application: %s", got, agent)
	}

	// The API and the pages say the same.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/agents", nil)
	var row map[string]any
	for _, it := range data["agents"].([]any) {
		if a, _ := it.(map[string]any); a["name"] == agent {
			row = a
		}
	}
	roles, _ := row["roles"].([]any)
	if code != http.StatusOK || row == nil || row["selected"] != true || row["known"] != true || len(roles) != 2 || roles[0] != "plan" || roles[1] != "build" {
		t.Fatalf("GET /api/agents: the row of %s = %v (%d %s)", agent, row, code, raw)
	}
	_, page, _ := getPage(t, srv, "/executions/new")
	wantOnPage(t, "/executions/new", mainOf(t, page), `<option value="`+agent+`" selected>`)
	_, agents := overviewPage(t, srv, "/agents")
	wantOnPage(t, "the row of the declared agent", rowOfAgent(t, agents, agent), "/api/engines/"+agent+"/ping")
}

func TestAPingOfADeclaredCustomAgentRunsItsPingCommandAndRecordsTheHealth(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	agent := declareCustomAgent(t, root)
	// The kit of the portal itself, which runs the ping command the application declares: a script.
	real := NewKit(st, "unused")
	kitOf(srv).ping = func(ctx context.Context, root, engine string) PingResult { return real.PingEngine(ctx, root, engine) }

	code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/engines/"+agent+"/ping", nil)
	engine, _ := data["engine"].(map[string]any)
	ping, _ := data["ping"].(map[string]any)
	if code != http.StatusOK || data["ok"] != true || ping["output"] != "pong" || engine["engine"] != agent || engine["health"] != "ok" {
		t.Fatalf("POST /api/engines/%s/ping = %d %s", agent, code, raw)
	}
	var recorded string
	err := st.View(context.Background(), func(v *store.View) error {
		engines, err := v.ListEngines(context.Background())
		for _, e := range engines {
			if e.Engine == agent {
				recorded = e.Health
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if recorded != "ok" {
		t.Errorf("the health recorded for %s = %q, want ok", agent, recorded)
	}
}
