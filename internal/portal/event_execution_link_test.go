package portal

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// TestEventsLinkToTheirOwnExecution: an agent.exit event names the execution it ended, so the portal
// links each one to its own. Two executions of the same change and phase that end within the same
// second are the case a match by change, phase and time gets wrong: both events then go to one of them.
func TestEventsLinkToTheirOwnExecution(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	ctx := context.Background()
	const change = "feat-twice"

	runID, err := st.BeginChangeRun(ctx, change, 1, "/tmp/worker-feat-twice", "feat/spec-feat-twice")
	if err != nil {
		t.Fatal(err)
	}
	runCtx := store.WithChangeRun(ctx, runID, change)
	execs := []string{"exec-first-apply", "exec-second-apply"}
	for _, id := range execs {
		if err := st.CreateExecution(runCtx, store.Execution{ExecID: id, Kind: "worker", ChangeName: change, Phase: "apply", EnginesRequested: "claude"}); err != nil {
			t.Fatal(err)
		}
	}
	// They end one right after the other, the second one first.
	for _, id := range []string{execs[1], execs[0]} {
		if _, err := st.EndExecution(runCtx, id, store.ExecutionEnd{Status: "succeeded", EngineUsed: "claude", AgentExit: true}); err != nil {
			t.Fatal(err)
		}
	}
	// An event of the run that is about no execution has no execution link.
	_ = st.EmitEvent(runCtx, store.EventInput{Event: "change.ready", Change: change})

	code, events, raw, _ := doReq(t, srv, http.MethodGet, "/api/changes/"+change+"/events", nil)
	if code != http.StatusOK {
		t.Fatalf("GET the events of the change = %d: %s", code, raw)
	}
	items, _ := events["items"].([]any)
	linked := map[string]int{}
	for _, it := range items {
		e, _ := it.(map[string]any)
		switch asStr(e["event"]) {
		case "agent.exit":
			linked[asStr(e["exec_id"])]++
		case "change.ready":
			if asStr(e["exec_id"]) != "" {
				t.Errorf("change.ready links to the execution %v", e["exec_id"])
			}
		}
	}
	if linked[execs[0]] != 1 || linked[execs[1]] != 1 || len(linked) != 2 {
		t.Fatalf("each agent.exit must link to its own execution, got %v", linked)
	}
	// The events of the change, as its page lists them, link each exit to its own execution.
	code, page, _ := getPage(t, srv, "/changes/"+change+"/events")
	if code != http.StatusOK {
		t.Fatalf("/changes/%s/events = %d", change, code)
	}
	for _, id := range execs {
		if n := strings.Count(page, `href="/executions/`+id+`"`); n != 1 {
			t.Errorf("the events of the change link %d time(s) to %s, want once", n, id)
		}
	}

	code, body, _ := getPage(t, srv, "/events")
	if code != 200 {
		t.Fatalf("/events = %d", code)
	}
	for _, id := range execs {
		if n := strings.Count(body, `href="/executions/`+id+`"`); n != 1 {
			t.Errorf("/events links %d time(s) to %s, want once", n, id)
		}
	}

	// The page of each execution lists its own exit, not the other's.
	for _, id := range execs {
		code, body, _ := getPage(t, srv, "/executions/"+id)
		if code != 200 {
			t.Fatalf("/executions/%s = %d", id, code)
		}
		if n := strings.Count(body, "agent.exit"); n != 1 {
			t.Errorf("the page of %s shows %d agent.exit events, want its own one", id, n)
		}
	}
}
