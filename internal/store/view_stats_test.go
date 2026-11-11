package store

import (
	"context"
	"testing"
	"time"
)

// TestTheValidationOfEachChangeIsOneGate: the umbrella validates the specs of each change with a
// gate named after the change. GateStats counts those runs as one gate and says how many changes it
// ran for, so the gates of a workspace do not grow by one with every change of its backlog. A gate
// of another repository that happens to carry the name of a change, and a gate of the umbrella that
// is not named after one, keep their names.
func TestTheValidationOfEachChangeIsOneGate(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	record := func(change, repoName, repoPath, gate string, exitCode int, ms int64) int64 {
		t.Helper()
		round, err := st.BeginGateRound(ctx, GateRound{Context: "cli", ChangeName: change})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.RecordGateRun(WithGateRound(ctx, round), GateRunRecord{ChangeName: change, RepoName: repoName, RepoPath: repoPath,
			GateName: gate, Command: "openspec validate", ExitCode: exitCode, DurationMS: ms}); err != nil {
			t.Fatal(err)
		}
		result := "passed"
		if exitCode != 0 {
			result = "failed"
		}
		if err := st.FinishGateRound(ctx, round, result); err != nil {
			t.Fatal(err)
		}
		return round
	}
	record("add-api", "openspec", UmbrellaRepoPath, "specs", 0, 100)
	record("add-api", "openspec", UmbrellaRepoPath, "add-api", 0, 200)
	record("add-ui", "openspec", UmbrellaRepoPath, "add-ui", 1, 300)
	lastFailed := record("add-ui", "openspec", UmbrellaRepoPath, "add-ui", 1, 400)
	record("add-ui", "openspec", UmbrellaRepoPath, "add-ui", 0, 300)
	record("add-ui", "openspec", UmbrellaRepoPath, "links", 0, 10)
	record("add-ui", "api", "repos/api", "add-api", 0, 50)

	view(t, st, func(v *View) error {
		gates, err := v.GateStats(ctx, time.Time{})
		if err != nil {
			return err
		}
		got := map[string]GateStat{}
		for _, g := range gates {
			got[g.RepoName+"/"+g.GateName] = g
		}
		if len(gates) != 4 {
			t.Errorf("gates = %+v, want the gate of the API, the validation of the changes, links and specs", gates)
		}
		changes := got["openspec/"]
		if changes.RepoPath != UmbrellaRepoPath || changes.Changes != 2 || changes.Runs != 4 || changes.Failures != 2 ||
			changes.TotalMS != 1200 || changes.MaxMS != 400 || changes.AvgMS != 300 ||
			changes.LastFailedRoundID != lastFailed || changes.LastFailedAt == "" || changes.LastRunAt == "" {
			t.Errorf("the validation of the changes = %+v", changes)
		}
		for _, name := range []string{"openspec/specs", "openspec/links", "api/add-api"} {
			if g := got[name]; g.Runs != 1 || g.Changes != 0 || g.Failures != 0 || g.LastFailedRoundID != 0 || g.LastFailedAt != "" {
				t.Errorf("gate %s = %+v", name, g)
			}
		}
		// The gates of a repository come together, and the validation of the changes before the
		// gates with a name.
		var order []string
		for _, g := range gates {
			order = append(order, g.RepoName+"/"+g.GateName)
		}
		want := []string{"api/add-api", "openspec/", "openspec/links", "openspec/specs"}
		for i := range want {
			if i >= len(order) || order[i] != want[i] {
				t.Errorf("order of the gates = %v, want %v", order, want)
				break
			}
		}
		return nil
	})
}

// TestEvaluationStatsSayWhatWasDoneAboutTheAnswers: next to how the evaluator answered a gate, the
// aggregate says in which mode the gate was asked and which answers a rule of the gate turned into
// an action. An approved answer is not the end of it: the rules of change_risk send an approved
// change to a person when one of its answers says it is risky.
func TestEvaluationStatsSayWhatWasDoneAboutTheAnswers(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	ask := func(ev Evaluation) int64 {
		t.Helper()
		id, err := st.RecordEvaluation(ctx, ev, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	ask(Evaluation{Gate: "change_risk", Mode: "enforce", Status: "passed", ChangeName: "add-api", CostUSD: 0.01})
	risky := ask(Evaluation{Gate: "change_risk", Mode: "enforce", Status: "passed", ChangeName: "add-login", CostUSD: 0.02})
	if err := st.SetEvaluationRoute(ctx, risky, "risk_review", map[string]any{"rules": "risk=high"}); err != nil {
		t.Fatal(err)
	}
	ask(Evaluation{Gate: "change_risk", Mode: "advisory", Status: "failed", ChangeName: "add-ui", Cached: true})
	ask(Evaluation{Gate: "plan_review", Status: "skipped", ChangeName: "add-ui"})

	view(t, st, func(v *View) error {
		evals, err := v.EvaluationStats(ctx, time.Time{})
		if err != nil {
			return err
		}
		if len(evals) != 2 || evals[0].Gate != "change_risk" || evals[1].Gate != "plan_review" {
			t.Fatalf("evaluations = %+v", evals)
		}
		risk, plan := evals[0], evals[1]
		if risk.Evaluations != 3 || risk.Passed != 2 || risk.ByStatus["failed"] != 1 || risk.Cached != 1 ||
			risk.ByMode["enforce"] != 2 || risk.ByMode["advisory"] != 1 || len(risk.ByMode) != 2 ||
			risk.ByRoute["risk_review"] != 1 || len(risk.ByRoute) != 1 || risk.CostUSD < 0.0299 || risk.CostUSD > 0.0301 {
			t.Errorf("evaluations of change_risk = %+v", risk)
		}
		// An evaluation that records no mode is counted under none, and one nothing was done about
		// is counted under no action.
		if plan.Evaluations != 1 || plan.ByStatus["skipped"] != 1 || len(plan.ByMode) != 0 || plan.ByRoute != nil {
			t.Errorf("evaluations of plan_review = %+v", plan)
		}
		return nil
	})
}
