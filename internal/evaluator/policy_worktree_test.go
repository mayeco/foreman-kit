package evaluator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

func writePolicyFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A worktree cut before the policy file was renamed still holds it under its old name. Once the
// application was updated, a command run from that worktree reads the policy of the main checkout:
// it is neither refused nor answered with the seed's gates. An application that was never updated is
// still refused, from its worktrees as from its main checkout.
func TestAWorktreeCutBeforeTheRenameReadsThePolicyOfTheMainCheckout(t *testing.T) {
	main := t.TempDir()
	worktree := filepath.Join(main, ".worktrees", "worker-old-change")
	writePolicyFile(t, main, ".foreman-kit/pipeline.yaml", "base_branch: main\n")
	writePolicyFile(t, worktree, ".foreman-kit/pipeline.yaml", "base_branch: main\n")
	writePolicyFile(t, worktree, LegacyPolicyFile, "gates:\n  plan_review:\n    mode: advisory\n")

	// Not updated yet: the old name is all there is, in both places.
	writePolicyFile(t, main, LegacyPolicyFile, "gates:\n  plan_review:\n    mode: advisory\n")
	for _, root := range []string{main, worktree} {
		if _, err := LoadPolicy(root); err == nil || !strings.Contains(err.Error(), "run foreman-kit update") {
			t.Fatalf("LoadPolicy(%s) must refuse an application that was not updated, got %v", root, err)
		}
	}

	// Updated: the main checkout has the file under its new name, with the application's own mode.
	if err := os.Remove(filepath.Join(main, filepath.FromSlash(LegacyPolicyFile))); err != nil {
		t.Fatal(err)
	}
	writePolicyFile(t, main, PolicyFile, "gates:\n  plan_review:\n    mode: enforce\n")
	for _, root := range []string{main, worktree} {
		pol, err := LoadPolicy(root)
		if err != nil {
			t.Fatalf("LoadPolicy(%s) after the update: %v", root, err)
		}
		if mode := pol.GateMode("plan_review"); mode != "enforce" {
			t.Fatalf("LoadPolicy(%s) read plan_review as %q, want the main checkout's enforce", root, mode)
		}
	}
}

// A worktree cut before `foreman-kit update` rewrote the files holds the policy an older kit
// read, under today's name. A gate asked from that worktree is judged by the policy of the main
// checkout, the one the application has now, and the limits of a question are its own too.
func TestAWorktreeCutBeforeTheUpdateReadsThePolicyOfTheMainCheckout(t *testing.T) {
	main := t.TempDir()
	settingstest.WriteFiles(t, main, map[string]string{"evaluator": "retries: 4\ngates:\n  plan_review:\n    mode: enforce\n  scenario_coverage:\n    mode: off\n"})
	worktree := filepath.Join(main, ".worktrees", "worker-old-change")
	settingstest.WriteFormerFiles(t, worktree)

	for _, root := range []string{main, worktree} {
		pol, err := LoadPolicy(root)
		if err != nil {
			t.Fatalf("LoadPolicy(%s): %v", root, err)
		}
		if mode := pol.GateMode("plan_review"); mode != "enforce" {
			t.Errorf("LoadPolicy(%s) read plan_review as %q, want the main checkout's enforce", root, mode)
		}
		if mode := pol.GateMode("scenario_coverage"); mode != "off" {
			t.Errorf("LoadPolicy(%s) read scenario_coverage as %q, want the main checkout's off", root, mode)
		}
		if pol.Retries != 4 {
			t.Errorf("LoadPolicy(%s) read retries as %d, want the main checkout's 4", root, pol.Retries)
		}
	}
}

// What `--set` gave a run holds for the evaluator of that run: every gate off, one gate in another
// mode, another model or endpoint. A policy loaded before the run was given a value still reads it
// when the value is used.
func TestAValueGivenForOneRunHoldsForTheEvaluator(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{"evaluator": "model: jev-file\ngates:\n  plan_review:\n    mode: enforce\n  scenario_coverage:\n    mode: enforce\n"})
	t.Setenv("TYPESAFE_API_KEY", "ts-test-key")

	before, err := LoadPolicy(root)
	if err != nil {
		t.Fatal(err)
	}
	if before.Disabled() || before.GateMode("plan_review") != "enforce" || before.EffectiveModel() != "jev-file" {
		t.Fatalf("the policy of the file: disabled %v, plan_review %s, model %s", before.Disabled(), before.GateMode("plan_review"), before.EffectiveModel())
	}

	settingstest.Give(t, settings.GateMode("plan_review"), "off")
	settingstest.Give(t, settings.EvaluatorModel, "jev-for-this-run")
	settingstest.Give(t, settings.EvaluatorBaseURL, "http://127.0.0.1:9")
	pol, err := LoadPolicy(root)
	if err != nil {
		t.Fatal(err)
	}
	if pol.GateMode("plan_review") != "off" || pol.GateMode("scenario_coverage") != "enforce" {
		t.Errorf("gates of the run: plan_review %s, scenario_coverage %s", pol.GateMode("plan_review"), pol.GateMode("scenario_coverage"))
	}
	for _, p := range []*Policy{pol, before} {
		if p.EffectiveModel() != "jev-for-this-run" || p.EffectiveBaseURL() != "http://127.0.0.1:9" {
			t.Errorf("model %s at %s, want what the run was given", p.EffectiveModel(), p.EffectiveBaseURL())
		}
	}

	settingstest.Give(t, settings.EvaluatorEnabled, "false")
	for _, p := range []*Policy{pol, before} {
		if !p.Disabled() || p.GateMode("scenario_coverage") != "off" || len(p.ActiveGates()) != 0 {
			t.Errorf("a run given evaluator.enabled=false still has gates: %v", p.ActiveGates())
		}
	}
}
