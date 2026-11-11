package store

import (
	"context"
	"testing"
)

// TestViewReturnsTheLatestOperationOfAKind: the latest operation of the kit a person started, among
// the kinds asked for, is returned with how it ended; an execution of another kind, or of another
// class with the same phase, is not one.
func TestViewReturnsTheLatestOperationOfAKind(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	latest := func(kinds ...string) *ExecutionDetail {
		t.Helper()
		var found *ExecutionDetail
		view(t, st, func(v *View) (err error) {
			found, err = v.LatestJob(ctx, kinds...)
			return err
		})
		return found
	}
	if job := latest("preflight", "preflight-services"); job != nil {
		t.Fatalf("the latest preflight with none started = %+v", job)
	}

	must(st.CreateExecution(ctx, Execution{ExecID: "job-preflight", Kind: "portal_job", Phase: "preflight", JobKind: "preflight", CommandText: "foreman-kit preflight"}))
	must(st.FinishExecution(ctx, "job-preflight", "failed", 1))
	must(st.CreateExecution(ctx, Execution{ExecID: "job-dispatch", Kind: "portal_job", Phase: "dispatch", JobKind: "dispatch", CommandText: "foreman-kit dispatch"}))
	must(st.CreateExecution(ctx, Execution{ExecID: "job-services", Kind: "portal_job", Phase: "preflight-services", JobKind: "preflight-services",
		CommandText: "foreman-kit preflight --services"}))
	must(st.FinishExecution(ctx, "job-services", "succeeded", 0))
	// The run of an agent a person started is not an operation of the kit, whatever its phase says.
	must(st.CreateExecution(ctx, Execution{ExecID: "run-preflight", Kind: "portal_run", Phase: "preflight", EnginesRequested: "claude", PromptText: "Run the preflight"}))

	if job := latest("preflight", "preflight-services"); job == nil || job.ExecID != "job-services" || job.Status != "succeeded" || job.JobKind != "preflight-services" {
		t.Errorf("the latest preflight of either kind = %+v", job)
	}
	if job := latest("preflight"); job == nil || job.ExecID != "job-preflight" || job.Status != "failed" || job.ExitCode != 1 || job.CommandText != "foreman-kit preflight" {
		t.Errorf("the latest full preflight = %+v", job)
	}
	if job := latest("dispatch"); job == nil || job.ExecID != "job-dispatch" || job.Status != "running" {
		t.Errorf("the latest dispatch = %+v", job)
	}
	if job := latest("merge"); job != nil {
		t.Errorf("the latest merge, which nobody started = %+v", job)
	}
	if job := latest(); job != nil {
		t.Errorf("the latest operation of no kind = %+v", job)
	}
}
