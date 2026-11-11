package store

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// TestViewReadsOneExecution: what is on record about one execution comes from its rows: its attempts
// with their idle stretches, the run it is part of, what it continues and what continued it, what ran
// next for its change, the repair it carried out and the gates that verified it.
func TestViewReadsOneExecution(t *testing.T) {
	w := seedPortalWorld(t)
	st := w.st
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 5; i++ {
		must(st.AppendLogChunk(ctx, "api-apply", "combined", fmt.Sprintf("line %d\n", i)))
	}
	longPrompt := strings.Repeat("and the tests? ", 40)
	must(st.CreateExecution(ctx, Execution{ExecID: "billing-follow", Kind: "portal_run", ChangeName: "add-billing", EnginesRequested: "claude",
		PromptText: longPrompt, ParentExecID: "billing-prompt", ParentRelation: "continue", Summary: "and the tests?"}))
	// An execution recorded as running by a process that is gone.
	gone := exec.Command("true")
	must(gone.Run())
	must(st.CreateExecution(ctx, Execution{ExecID: "orphan", Kind: "portal_job", JobKind: "merge", CommandText: "foreman-kit merge", PID: gone.Process.Pid}))

	view(t, st, func(v *View) error {
		rec, err := v.Execution(ctx, "api-apply")
		must(err)
		if rec == nil || rec.PromptText != "build the API" || rec.OwnerGone || rec.Parent != nil || rec.Repair != nil || rec.Continuations == nil || len(rec.Continuations) != 0 {
			t.Fatalf("api-apply = %+v", rec)
		}
		if len(rec.Attempts) != 1 || len(rec.Attempts[0].IdlePeriods) != 1 || rec.Attempts[0].IdlePeriods[0].EndedBy != IdleEndedByActivity {
			t.Errorf("the attempts of api-apply = %+v, want one with an idle stretch it came back from", rec.Attempts)
		}
		if rec.Run == nil || rec.Run.ID != w.apiRun || rec.Run.Result != "ready" {
			t.Errorf("the run of api-apply = %+v", rec.Run)
		}
		if rec.Next == nil || rec.Next.ExecID != "api-repair" {
			t.Errorf("what ran next for the change = %+v, want api-repair", rec.Next)
		}
		if len(rec.GateRounds) != 1 || rec.GateRounds[0].ID != w.failedRound || rec.GateRounds[0].Result != "failed" || len(rec.GateRounds[0].Runs) != 2 ||
			rec.GateRounds[0].Runs[0].GateName != "test" || rec.GateRounds[0].Runs[0].FailureExcerpt != "--- FAIL: TestList" {
			t.Errorf("the gates that verified api-apply = %+v", rec.GateRounds)
		}

		rec, err = v.Execution(ctx, "api-repair")
		must(err)
		if rec.Repair == nil || rec.Repair.AskedReason != "gate_failure" || rec.Repair.AskedGateRoundID != w.failedRound || !rec.Repair.ResultPassed || rec.Repair.RoundNo != 1 {
			t.Errorf("the repair api-repair carried out = %+v", rec.Repair)
		}
		if len(rec.GateRounds) != 1 || rec.GateRounds[0].ID != w.passedRound || rec.Next != nil {
			t.Errorf("api-repair: gates %+v, next %+v; want the round that passed and nothing after it", rec.GateRounds, rec.Next)
		}

		// A prompt a person sent: no run, and the follow-up it got.
		rec, err = v.Execution(ctx, "billing-prompt")
		must(err)
		if rec.Run != nil || len(rec.Continuations) != 1 || rec.Continuations[0].ExecID != "billing-follow" || rec.Continuations[0].ParentRelation != "continue" ||
			len(rec.Continuations[0].PromptText) >= len(longPrompt) || rec.Next == nil || rec.Next.ExecID != "billing-follow" {
			t.Errorf("billing-prompt = run %+v, continuations %+v, next %+v", rec.Run, rec.Continuations, rec.Next)
		}
		rec, err = v.Execution(ctx, "billing-follow")
		must(err)
		if rec.Parent == nil || rec.Parent.ExecID != "billing-prompt" || rec.PromptText != longPrompt || rec.Summary != "and the tests?" {
			t.Errorf("billing-follow = parent %+v, prompt of %d bytes", rec.Parent, len(rec.PromptText))
		}

		// Running now: one whose process is alive, with its agent idle, and one whose process is gone.
		rec, err = v.Execution(ctx, "search-apply")
		must(err)
		if rec.OwnerGone || len(rec.Attempts) != 1 || len(rec.Attempts[0].IdlePeriods) != 1 || rec.Attempts[0].IdlePeriods[0].EndedBy != "" || rec.Attempts[0].IdlePeriods[0].Seconds < 300 {
			t.Errorf("search-apply = owner gone %v, attempts %+v", rec.OwnerGone, rec.Attempts)
		}
		rec, err = v.Execution(ctx, "orphan")
		must(err)
		if !rec.OwnerGone || rec.Status != "running" {
			t.Errorf("an execution whose process is gone = %+v, want it running on record and its owner gone", rec.Execution)
		}
		if rec, err := v.Execution(ctx, "no-such-execution"); rec != nil || err != nil {
			t.Errorf("an execution that does not exist = %+v, %v", rec, err)
		}

		if status, found, err := v.ExecutionStatus(ctx, "api-apply"); status != "succeeded" || !found || err != nil {
			t.Errorf("ExecutionStatus(api-apply) = %q, %v, %v", status, found, err)
		}
		if status, found, err := v.ExecutionStatus(ctx, "no-such-execution"); status != "" || found || err != nil {
			t.Errorf("ExecutionStatus of nothing = %q, %v, %v", status, found, err)
		}

		round, err := v.GateRound(ctx, w.failedRound)
		must(err)
		if round == nil || round.Result != "failed" || round.AfterExecID != "api-apply" || len(round.Runs) != 2 || round.Runs[1].GateName != "lint" {
			t.Errorf("the round of gates = %+v", round)
		}
		if round, err := v.GateRound(ctx, 987654); round != nil || err != nil {
			t.Errorf("a round that does not exist = %+v, %v", round, err)
		}

		events, err := v.EventsOfExecution(ctx, "api-apply")
		must(err)
		var names []string
		for _, e := range events {
			names = append(names, e.Event)
		}
		if got := strings.Join(names, " "); !strings.Contains(got, "agent.idle agent.resumed") {
			t.Errorf("the events of api-apply = %q, want its idle stretch", got)
		}

		names, err = v.ListEngineNames(ctx)
		must(err)
		if !sort.StringsAreSorted(names) || !strings.Contains(" "+strings.Join(names, " ")+" ", " claude ") || !strings.Contains(" "+strings.Join(names, " ")+" ", " opencode ") {
			t.Errorf("the engines on record = %v", names)
		}
		return nil
	})

	// The lists say it too: an execution that only looks alive is told apart from one that runs.
	view(t, st, func(v *View) error {
		page, err := v.ListExecutionsPage(ctx, ExecutionFilter{Status: "running"})
		must(err)
		gone := map[string]bool{}
		for _, e := range page.Items {
			gone[e.ExecID] = e.OwnerGone
		}
		if !gone["orphan"] || gone["search-apply"] || len(gone) < 2 {
			t.Errorf("the running executions and whether their process is gone = %v", gone)
		}
		return nil
	})
}

// TestViewReadsTheOutputOfAnExecution: the output is read whole, by its end, or chunk by chunk from a
// cursor. A chunk is never cut, so the end of an output starts where a write started.
func TestViewReadsTheOutputOfAnExecution(t *testing.T) {
	w := seedPortalWorld(t)
	st := w.st
	ctx := context.Background()
	const whole = "line 1\nline 2\nline 3\nline 4\nline 5\n"
	for i := 1; i <= 5; i++ {
		if err := st.AppendLogChunk(ctx, "api-apply", "combined", fmt.Sprintf("line %d\n", i)); err != nil {
			t.Fatal(err)
		}
	}
	// Bytes are counted, not characters.
	if err := st.AppendLogChunk(ctx, "api-repair", "combined", "añadió ✓\n"); err != nil {
		t.Fatal(err)
	}

	view(t, st, func(v *View) error {
		tail, err := v.LogTail(ctx, "api-apply", 0)
		if err != nil || tail.Content != whole || tail.Bytes != int64(len(whole)) || tail.Truncated || tail.LastID == 0 {
			t.Errorf("the whole output = %+v, %v", tail, err)
		}
		// 15 bytes hold the last two chunks (14 bytes) and not the third.
		tail, err = v.LogTail(ctx, "api-apply", 15)
		if err != nil || tail.Content != "line 4\nline 5\n" || !tail.Truncated || tail.Bytes != int64(len(whole)) {
			t.Errorf("the end of the output in 15 bytes = %+v, %v", tail, err)
		}
		// Less than one chunk: the last chunk, whole.
		tail, err = v.LogTail(ctx, "api-apply", 3)
		if err != nil || tail.Content != "line 5\n" || !tail.Truncated {
			t.Errorf("the end of the output in 3 bytes = %+v, %v", tail, err)
		}
		if tail, err := v.LogTail(ctx, "api-apply", len(whole)); err != nil || tail.Content != whole || tail.Truncated {
			t.Errorf("an output that fits exactly = %+v, %v", tail, err)
		}
		if tail, err := v.LogTail(ctx, "job-gates", 100); err != nil || tail.Content != "" || tail.Bytes != 0 || tail.LastID != 0 || tail.Truncated {
			t.Errorf("the output of an execution that wrote none = %+v, %v", tail, err)
		}
		if tail, err := v.LogTail(ctx, "api-repair", 0); err != nil || tail.Bytes != int64(len("añadió ✓\n")) || tail.Bytes == int64(len([]rune("añadió ✓\n"))) {
			t.Errorf("the size of an output with characters of several bytes = %d, %v; want %d bytes", tail.Bytes, err, len("añadió ✓\n"))
		}

		var read strings.Builder
		after, pages := "", 0
		for {
			page, err := v.ReadLogChunks(ctx, "api-apply", after, 2)
			if err != nil {
				return err
			}
			pages++
			for _, c := range page.Items {
				if c.ExecID != "api-apply" || c.Stream != "combined" {
					t.Errorf("a chunk of the output = %+v", c)
				}
				read.WriteString(c.Content)
			}
			if after = page.Next; after == "" {
				break
			}
		}
		if read.String() != whole || pages != 3 {
			t.Errorf("the output read two chunks at a time = %q in %d pages, want it whole in 3", read.String(), pages)
		}
		for _, bad := range []string{"x", "-1", "1.5"} {
			if _, err := v.ReadLogChunks(ctx, "api-apply", bad, 0); !errors.Is(err, ErrBadCursor) {
				t.Errorf("ReadLogChunks after %q = %v, want ErrBadCursor", bad, err)
			}
		}
		if page, err := v.ReadLogChunks(ctx, "no-such-execution", "", 0); err != nil || len(page.Items) != 0 || page.Next != "" {
			t.Errorf("the output of nothing = %+v, %v", page, err)
		}
		return nil
	})
}
