package cycle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// installedWithAFormerKit turns a fresh installation into one written by the kit that had five
// configuration files with keys of other names: its seeds of then (internal/config/testdata/former),
// edited the way a team edits them, no daemon.yaml and no visual_qa.yaml, and a lock that holds the
// checksums of those seeds. With renamed, the evaluator is still named after its provider too: its
// file, the value that approves proposals and the placeholder of the prompts. It commits the result.
func installedWithAFormerKit(t *testing.T, ctx context.Context, appDir string, renamed bool) {
	t.Helper()
	if err := Init(ctx, appDir, KitOptions{Agents: "claude", Version: "9.0.0-test"}, io.Discard); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	lockPath := filepath.Join(appDir, KitLockFile)
	rawLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var lock KitLock
	if err := json.Unmarshal(rawLock, &lock); err != nil {
		t.Fatal(err)
	}

	edits := map[string][]string{
		"pipeline": {"base_branch: master", "base_branch: main", "workers: 2", "workers: 3", "gate_seconds: 2700", "gate_seconds: 900", "agent_budget_usd: 5", "agent_budget_usd: 7"},
		"agents": {
			"  build: [claude]", "  build: [claude] # ours",
			"permissions: all", "permissions: restricted",
			"extra_args: {}", "extra_args:\n  claude: \"--model x-model\"",
		},
		"cycle":       {"  slot: false", "  slot: 97", "  interval_minutes: 60", "  interval_minutes: 30"},
		"exploration": {},
		"evaluator":   {"mode: advisory # off | advisory | enforce", "mode: off # off | advisory | enforce"},
	}
	for name, pairs := range edits {
		raw, err := os.ReadFile(filepath.Join("..", "config", "testdata", "former", "config", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		rel := ".foreman-kit/" + name + ".yaml"
		// The lock of that installation holds the checksums of the seeds it was installed from.
		lock.Files[rel] = LockFileEntry{Class: "seed", SHA256: assets.SHA256Bytes(raw)}
		text := string(raw)
		for i := 0; i+1 < len(pairs); i += 2 {
			if !strings.Contains(text, pairs[i]) {
				t.Fatalf("the former seed of %s does not hold %q", rel, pairs[i])
			}
			text = strings.Replace(text, pairs[i], pairs[i+1], 1)
		}
		if err := os.WriteFile(filepath.Join(appDir, filepath.FromSlash(rel)), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{".foreman-kit/daemon.yaml", ".foreman-kit/visual_qa.yaml"} {
		if err := os.Remove(filepath.Join(appDir, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
		delete(lock.Files, rel)
	}

	if renamed {
		rewrite := func(rel, old, new string) {
			t.Helper()
			p := filepath.Join(appDir, filepath.FromSlash(rel))
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), old) {
				t.Fatalf("%s does not contain %q", rel, old)
			}
			if err := os.WriteFile(p, []byte(strings.ReplaceAll(string(raw), old, new)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Rename(filepath.Join(appDir, filepath.FromSlash(evaluator.PolicyFile)), filepath.Join(appDir, filepath.FromSlash(evaluator.LegacyPolicyFile))); err != nil {
			t.Fatal(err)
		}
		lock.Files[evaluator.LegacyPolicyFile] = lock.Files[evaluator.PolicyFile]
		delete(lock.Files, evaluator.PolicyFile)
		rewrite(CycleFile, "  approval: evaluator\n", "  approval: jev # set by the team\n")
		rewrite(".foreman-kit/prompts/plan.md", "{{evaluator_hint}}", "{{jev_hint}}")
		rewrite(".foreman-kit/prompts/build.md", "{{evaluator_hint}}", "{{jev_hint}}")
		lock.Files[".foreman-kit/prompts/plan.md"] = LockFileEntry{Class: "seed", SHA256: strings.Repeat("0", 64)}
	}

	updated, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, updated, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "-A"},
		{"commit", "-qm", "an application installed with a former kit"},
	} {
		if _, err := gitx.RunGit(ctx, appDir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
}

// TestUpdateRewritesTheFilesOfAnApplicationInstalledWithAFormerKit: `foreman-kit update` leaves an
// application written for a kit with other files and other keys reading its own settings in the
// seven files of today, committed on a branch, with nothing left for a person to merge by hand in
// them. Afterwards nothing is pending and every file checks clean.
func TestUpdateRewritesTheFilesOfAnApplicationInstalledWithAFormerKit(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		name := "after the evaluator was renamed"
		if renamed {
			name = "before the evaluator was renamed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			appDir := t.TempDir()
			installedWithAFormerKit(t, ctx, appDir, renamed)

			if !config.ConfigNeedsRewrite(appDir) {
				t.Fatal("an installation with the keys of a former kit must be seen as pending")
			}
			if renamed {
				if !evaluatorRenamePending(appDir) {
					t.Fatal("an installation with the old names must be seen as pending")
				}
				if _, err := evaluator.LoadPolicy(appDir); err == nil || !strings.Contains(err.Error(), "run foreman-kit update") {
					t.Fatalf("the policy under its old name must not be replaced by the seed, got %v", err)
				}
			}
			// Before the update the files are errors of check, not read some other way.
			if rep := config.Check(appDir)[".foreman-kit/pipeline.yaml"]; len(rep.Errors) == 0 {
				t.Fatal("check accepts a file written for a former kit")
			}

			var out bytes.Buffer
			if err := Upgrade(ctx, appDir, KitOptions{Version: "9.0.0-test"}, &out); err != nil {
				t.Fatalf("Upgrade failed: %v\n%s", err, out.String())
			}
			said := out.String()
			for _, want := range []string{
				"rewrote .foreman-kit/pipeline.yaml with the keys of this foreman-kit",
				"gates.timeout: 15m (was limits.gate_seconds: 900 in .foreman-kit/pipeline.yaml)",
				"rewrote .foreman-kit/agents.yaml",
				"commands.claude: --max-budget-usd 7 is written in its run and restricted commands",
				"created .foreman-kit/daemon.yaml",
				"cycle.every: 30m (was daemon.interval_minutes: 30 in .foreman-kit/cycle.yaml)",
				"created .foreman-kit/visual_qa.yaml",
				"slot: 97 (was visual_qa.slot: 97 in .foreman-kit/cycle.yaml)",
				"committed on branch kit-upgrade-v9.0.0-test",
			} {
				if !strings.Contains(said, want) {
					t.Errorf("the update does not say %q:\n%s", want, said)
				}
			}
			if renamed && !strings.Contains(said, "renamed "+evaluator.LegacyPolicyFile+" to "+evaluator.PolicyFile) {
				t.Errorf("the update did not say it renamed the policy:\n%s", said)
			}

			// Each value the team had set is where the kit of today reads it.
			r := &settings.Reader{Root: appDir, FilesAlone: true}
			for key, want := range map[settings.Key]string{
				settings.BaseBranch:               "main",
				settings.Workers:                  "3",
				settings.GateTimeout:              "15m",
				settings.AgentPermissions:         "restricted",
				settings.RoleTimeout("build"):     "2h",
				settings.CycleEvery:               "30m",
				settings.CycleEveryWhenOutOfWork:  "15m",
				settings.VisualQASlot:             "97",
				settings.ProposalsApprovedBy:      "evaluator",
				settings.GateMode("plan_review"):  "off",
				settings.EvaluatorWhenUnavailable: "pass",
			} {
				if got := r.Text(key); got != want {
					t.Errorf("%s = %s, want %s", key, got, want)
				}
			}
			cfg := LoadAgentsConfig(appDir)
			if strings.Join(cfg.Agents, ",") != "claude" || strings.Join(cfg.Roles["build"], ",") != "claude" || strings.Join(cfg.Roles["cycle"], ",") != "claude" {
				t.Errorf("the agents and their roles: %+v", cfg)
			}
			if got := strings.Join(cfg.ExtraArgsByRole["claude"]["build"], " "); got != "--model x-model" {
				t.Errorf("claude's arguments in build: %q", got)
			}
			if run := config.Load(appDir).Commands["claude"].Run; !strings.Contains(run.(string), "--max-budget-usd 7 ") {
				t.Errorf("claude's command lost the cap the application had set: %v", run)
			}
			pol, err := evaluator.LoadPolicy(appDir)
			if err != nil || pol.Gates["plan_review"].Mode != "off" || len(pol.Gates["plan_review"].Questions) == 0 {
				t.Fatalf("the application's own gates were lost: %v, %+v", err, pol)
			}

			// Nothing is left to merge by hand in the files the update rewrote, and nothing is pending.
			for _, f := range settings.Files() {
				if fileExists(filepath.Join(appDir, filepath.FromSlash(f.Path)) + ".new") {
					t.Errorf("%s.new was left beside a file the update rewrote", f.Path)
				}
			}
			if fileExists(filepath.Join(appDir, filepath.FromSlash(evaluator.LegacyPolicyFile))) {
				t.Errorf("%s is still there", evaluator.LegacyPolicyFile)
			}
			for _, rel := range []string{".foreman-kit/prompts/plan.md", ".foreman-kit/prompts/build.md"} {
				raw, err := os.ReadFile(filepath.Join(appDir, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), legacyHintPlaceholder) || !strings.Contains(string(raw), hintPlaceholder) {
					t.Errorf("%s still has the old placeholder", rel)
				}
			}
			if config.ConfigNeedsRewrite(appDir) || evaluatorRenamePending(appDir) {
				t.Error("something is still pending after the update")
			}
			files := config.Check(appDir)
			for _, f := range settings.Files() {
				for _, problem := range files[f.Path].Errors {
					t.Errorf("%s after the update: %s", f.Path, problem)
				}
			}

			// It is one commit on the upgrade branch, and the lock names the files of today.
			if dirty, err := gitx.RunGit(ctx, appDir, "status", "--porcelain"); err != nil || strings.TrimSpace(dirty) != "" {
				t.Errorf("the update left changes uncommitted (%v):\n%s", err, dirty)
			}
			shown, err := gitx.RunGit(ctx, appDir, "show", "--stat", "--format=%s", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"chore(kit): upgrade the OpenSpec kit to v9.0.0-test", ".foreman-kit/daemon.yaml", ".foreman-kit/visual_qa.yaml", ".foreman-kit/agents.yaml", ".foreman-kit/pipeline.yaml"} {
				if !strings.Contains(shown, want) {
					t.Errorf("the commit does not hold %s:\n%s", want, shown)
				}
			}
			newLock, err := ReadKitLock(appDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range settings.Files() {
				if _, ok := newLock.Files[f.Path]; !ok {
					t.Errorf("the lock does not name %s", f.Path)
				}
			}
			if _, ok := newLock.Files[evaluator.LegacyPolicyFile]; ok {
				t.Errorf("the lock still names %s", evaluator.LegacyPolicyFile)
			}

			// A second update has nothing to do.
			out.Reset()
			if err := Upgrade(ctx, appDir, KitOptions{Version: "9.0.0-test"}, &out); err != nil || !strings.Contains(out.String(), "nothing to do") {
				t.Errorf("a second update: %v\n%s", err, out.String())
			}

			// The daemon of that application reads what the team had set.
			if got := LoadDaemonConfig(appDir); got.Every != 30*time.Minute || got.EveryWhenOutOfWork != 15*time.Minute {
				t.Errorf("the daemon reads %+v", got)
			}
		})
	}
}
