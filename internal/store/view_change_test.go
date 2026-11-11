package store

import (
	"context"
	"strings"
	"testing"
)

// TestViewReadsWhatIsOpenForAChange: the record of a change says what the change declares and what
// is open for it now, each from the row that is open: its block, its run and whether that run lost
// its worker, its manifest, its merge attempt, the rebuild it waits for and its idle agent.
func TestViewReadsWhatIsOpenForAChange(t *testing.T) {
	w := seedPortalWorld(t)
	ctx := context.Background()
	names := func(deps []ChangeDependency) string {
		var out []string
		for _, d := range deps {
			out = append(out, d.ChangeName+":"+d.State)
		}
		return strings.Join(out, " ")
	}
	view(t, w.st, func(v *View) error {
		change := func(name string) *ChangeRecord {
			t.Helper()
			rec, err := v.Change(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			if rec == nil {
				t.Fatalf("the change %s has no record", name)
			}
			return rec
		}

		// In the hands of the merger: a manifest open and an attempt in progress.
		api := change("add-api")
		if api.State != ChangeMerging || api.Description != "The API" || api.Runs != 1 || api.Executions != 3 || api.Repairs != 1 {
			t.Errorf("add-api = %+v", api.ChangeOverview)
		}
		if names(api.DependsOn) != "add-base:integrated" || names(api.NeededBy) != "add-ui:queued" || strings.Join(api.Locks, ",") != "db-schema" {
			t.Errorf("add-api depends on [%s], is needed by [%s] and declares the locks %v", names(api.DependsOn), names(api.NeededBy), api.Locks)
		}
		if api.Manifest == nil || api.Manifest.HeadSHA != "abc123" || strings.Join(api.Manifest.Capabilities, ",") != "api,lock:db-schema" ||
			api.Merge == nil || api.Merge.ID != w.secondMerge || api.Run != nil || api.Lost != nil || api.Block != nil || api.Rebuild != nil || api.Idle != nil {
			t.Errorf("what is open for add-api = manifest %+v, merge %+v, run %+v, block %+v", api.Manifest, api.Merge, api.Run, api.Block)
		}

		// Queued behind another change: what holds it back.
		ui := change("add-ui")
		var facts []string
		for _, f := range ui.Facts {
			facts = append(facts, f.Kind+":"+f.OtherChange+":"+f.LockName)
		}
		if ui.State != ChangeQueued || ui.Priority != "high" || names(ui.DependsOn) != "add-api:merging add-base:integrated" ||
			strings.Join(ui.Locks, ",") != "db-schema,routes" || strings.Join(facts, " ") != "dependency:add-api: lock:add-api:db-schema" {
			t.Errorf("add-ui = %s, depends on [%s], locks %v, held back by %v", ui.State, names(ui.DependsOn), ui.Locks, facts)
		}
		if ui.Run != nil || ui.Manifest != nil || ui.Merge != nil || ui.Block != nil {
			t.Errorf("something is open for add-ui: %+v", ui)
		}

		// Building, with its agent idle; and building as far as its run says, with its worker gone.
		search := change("add-search")
		if search.State != ChangeBuilding || search.Run == nil || search.Lost != nil || search.Idle == nil || search.Idle.ExecID != "search-apply" || search.Idle.LimitSeconds != 1200 {
			t.Errorf("add-search = run %+v, lost %+v, idle %+v", search.Run, search.Lost, search.Idle)
		}
		export := change("add-export")
		if export.State != ChangeBuilding || export.Run == nil || export.Lost == nil || export.Lost.ID != export.Run.ID || export.Lost.LeaseExpiredAt == "" || export.Idle != nil {
			t.Errorf("add-export = run %+v, lost %+v", export.Run, export.Lost)
		}

		// Blocked: the block in force and the counters of the change.
		billing := change("add-billing")
		if billing.State != ChangeBlocked || billing.Block == nil || billing.Block.Reason != "gate_failure" || billing.Block.Details != "coverage 91% < 100%" ||
			billing.Block.ChangeRunID != w.billingRun || billing.Block.Quarantine.Status != "blocked" || billing.OpenBlockReason != "gate_failure" || billing.Run != nil {
			t.Errorf("add-billing = %s, block %+v", billing.State, billing.Block)
		}
		if len(billing.Facts) != 1 || billing.Facts[0].Kind != WaitBlock {
			t.Errorf("what holds add-billing back = %+v", billing.Facts)
		}

		// Sent back by the merger and not taken yet.
		report := change("add-report")
		if report.State != ChangeRebuildRequested || report.Rebuild == nil || report.Rebuild.ConflictDetails != "CONFLICT in report.go" || report.Rebuild.ConsumedAt != "" ||
			report.OpenDeferrals != 1 || names(report.DependsOn) != "add-export:building" {
			t.Errorf("add-report = %s, rebuild %+v, deferrals %d, depends on [%s]", report.State, report.Rebuild, report.OpenDeferrals, names(report.DependsOn))
		}

		// Integrated: nothing is open.
		base := change("add-base")
		if base.State != ChangeIntegrated || base.IntegratedCommit != "c0ffee" || names(base.NeededBy) != "add-api:merging add-ui:queued" ||
			base.Run != nil || base.Manifest != nil || base.Merge != nil || base.Block != nil || len(base.Facts) != 0 {
			t.Errorf("add-base = %+v", base)
		}
		if base.DependsOn == nil || base.Locks == nil || base.Facts == nil {
			t.Errorf("a change with nothing declared has lists that are nil: %+v", base)
		}

		if rec, err := v.Change(ctx, "no-such-change"); rec != nil || err != nil {
			t.Errorf("a change the kit has no record of = %+v, %v", rec, err)
		}
		return nil
	})
}
