package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// formerApp writes, in a new directory, the configuration files an application had before the seven
// files of today: the seeds of that kit, edited by edit (file name => old text, new text pairs).
func formerApp(t *testing.T, edits map[string][]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pipeline", "agents", "cycle", "exploration", "evaluator"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "former", "config", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		// update renames the kit's former names in the file before it rewrites the keys (cycle.RewriteFormerNames).
		text := strings.ReplaceAll(string(raw), "openspec-kit", "foreman-kit")
		pairs := edits[name]
		for i := 0; i+1 < len(pairs); i += 2 {
			if !strings.Contains(text, pairs[i]) {
				t.Fatalf("the former seed of config/%s.yaml does not hold %q", name, pairs[i])
			}
			text = strings.Replace(text, pairs[i], pairs[i+1], 1)
		}
		if err := os.WriteFile(filepath.Join(root, ".foreman-kit", name+".yaml"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func applyMigration(t *testing.T, root string) *Migration {
	t.Helper()
	m, err := MigrateConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range m.Changed {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), m.Files[rel], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// An application that never edited its files comes out with the seeds of today, but for what the
// kit shipped differently then and the application ran with: its base branch, and a PLAN call that
// had the two hours every call had.
func TestMigrateConfigOfAnUntouchedApplicationGivesTheSeeds(t *testing.T) {
	root := formerApp(t, nil)
	m := applyMigration(t, root)
	if got := strings.Join(m.Changed, " "); got != ".foreman-kit/pipeline.yaml .foreman-kit/agents.yaml .foreman-kit/cycle.yaml .foreman-kit/daemon.yaml .foreman-kit/visual_qa.yaml .foreman-kit/exploration.yaml .foreman-kit/evaluator.yaml" {
		t.Fatalf("files rewritten: %s", got)
	}
	for _, f := range settings.Files() {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		seed, err := RenderSeed(f.Name, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(string(seed), "  base_branch: main\n", "  base_branch: master\n", 1)
		want = strings.Replace(want, "    timeout: 30m # one call of this role\n", "    timeout: 2h # one call of this role\n", 1)
		if string(got) != want {
			t.Errorf("%s is not the seed of today:\n%s", f.Path, firstDifference(string(got), want))
		}
	}
	if ConfigNeedsRewrite(root) {
		t.Error("the files just rewritten need rewriting again")
	}
	if again, err := MigrateConfig(root); err != nil || again.Pending() || len(again.Notes) > 0 {
		t.Errorf("a second migration: %+v, %v", again, err)
	}
	// Nothing the application had chosen was there to name: the notes only say which files changed
	// and which keys nothing reads any more.
	for _, note := range m.Notes {
		if strings.Contains(note, "REVIEW") {
			t.Errorf("an untouched application has something to review: %s", note)
		}
	}
}

func firstDifference(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			return "line " + strings.TrimSpace(strings.Join([]string{"", g[i], "want:", w[i]}, "\n"))
		}
	}
	return "the files differ in length"
}

// What an application chose arrives at the keys of today, in the shape those take; what cannot be
// moved is said, and nothing is lost without a word.
func TestMigrateConfigMovesWhatAnApplicationChose(t *testing.T) {
	root := formerApp(t, map[string][]string{
		"pipeline": {
			"base_branch: master", "base_branch: develop",
			"workers: 2", "workers: 3",
			"wait_seconds: 3600", "wait_seconds: 600",
			"aging_hours: 6", "aging_hours: 0",
			"blocked_retry_hours: 1", "blocked_retry_hours: 3",
			"agent_seconds: 7200", "agent_seconds: 1800",
			"agent_idle_seconds: 1200", "agent_idle_seconds: 0",
			"agent_retry_delay_seconds: 120", "agent_retry_delay_seconds: 0",
			"agent_budget_usd: 5", "agent_budget_usd: 4",
			"gate_seconds: 2700", "gate_seconds: 600",
			"\n  merge_changed_files: verify", "\n  merge_changed_files: change",
			"provider: openai", "provider: gemini",
			"max_retries: 2", "max_retries: 3",
		},
		"agents": {
			"agents: [claude]", "agents: [claude, codex, mybot]",
			"  plan:  [claude]", "  plan:  [claude, codex]",
			"  build: [claude]", "  build: [codex, mybot]",
			"order: fallback", "order: spread",
			`  claude: "claude -p {{prompt}} --output-format json --permission-mode bypassPermissions --no-session-persistence"`,
			`  claude: "claude -p {{prompt}} --output-format json --permission-mode bypassPermissions --no-session-persistence"` + "\n" +
				`  codex: "codex exec --json --budget {{budget_usd}} -- {{prompt}}"` + "\n" +
				"  mybot:\n    run: mybot --go\n    stdin_null: true",
			"extra_args: {}", "extra_args:\n  claude: {plan: \"--model opus\", default: \"--model haiku\"}\n  codex: [\"-m\", \"gpt-x\"]",
		},
		"cycle": {
			"qa_time_budget_minutes: 45", "qa_time_budget_minutes: 60",
			"max_new_backlog_items_per_cycle: 3", "max_new_backlog_items_per_cycle: 5",
			"approval: evaluator", "approval: auto",
			"  enabled: true\n  minutes: 40", "  enabled: false\n  minutes: 30",
			"  slot: false", "  slot: 97",
			"max_chars: 12000", "max_chars: 0",
			"  skills: []", "  skills: [browser-qa]",
			"  web: false", "  web: true",
			"  port: 4567", "  port: 5000",
			"idle_delay_minutes: 15", "idle_delay_minutes: 0",
		},
		"exploration": {
			"\norder: rotation", "\norder: random",
		},
		"evaluator": {
			"max_retries: 2", "max_retries: 0",
			"on_unavailable: skip # skip | block", "on_unavailable: block",
			"mode: advisory # off | advisory | enforce", "mode: enforce",
			"instructions: Does the proposal state a specific problem and an outcome that can be checked?", "instructions: Is the problem this application's own?",
		},
	})
	m := applyMigration(t, root)

	r := &settings.Reader{Root: root, FilesAlone: true}
	for key, want := range map[settings.Key]string{
		settings.BaseBranch:               "develop",
		settings.Workers:                  "3",
		settings.PriorityBumpEvery:        "never",
		settings.BlockedRetryAfter:        "3h",
		settings.BaseRecheckAfter:         "3h",
		settings.GateTimeout:              "10m",
		settings.OutsideCommits:           "ignore",
		settings.DiagnosticsProvider:      "google",
		settings.DiagnosticsRetries:       "3",
		settings.RoleTimeout("plan"):      "30m",
		settings.RoleTimeout("build"):     "30m",
		settings.RoleTimeout("cycle"):     "1h15m", // the budget of an hour and the margin of fifteen minutes
		settings.IdleTimeout:              "never",
		settings.AgentRetryAfter:          "0s",
		settings.AgentSelection:           "least_busy",
		settings.AgentPermissions:         "full",
		settings.CycleBudget:              "1h",
		settings.QueueMaxFindings:         "5",
		settings.ProposalsApprovedBy:      "nobody",
		settings.MemoryMaxChars:           "12000",
		settings.CycleEvery:               "1h",
		settings.CycleEveryWhenOutOfWork:  "never",
		settings.DaemonPortal:             "true",
		settings.DaemonPort:               "5000",
		settings.VisualQAEnabled:          "false",
		settings.VisualQATimeBudget:       "30m",
		settings.VisualQAExpiresAfter:     "1h", // its budget and the margin of thirty minutes
		settings.VisualQAMaxFindings:      "5",
		settings.VisualQASlot:             "97",
		settings.ExplorationSelection:     "random",
		settings.EvaluatorRetries:         "0",
		settings.EvaluatorWhenUnavailable: "block",
		settings.EvaluatorEnabled:         "true",
		settings.GateMode("plan_review"):  "enforce",
	} {
		if got := r.Text(key); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	if got := strings.Join(r.Strings(settings.CycleSkills), ",") + "|" + strings.Join(r.Strings(settings.VisualQASkills), ","); got != "browser-qa|browser-qa" {
		t.Errorf("the QA skills of each session: %s", got)
	}

	cfg := Load(root)
	if got := strings.Join(cfg.Roles["plan"], ",") + "|" + strings.Join(cfg.Roles["build"], ",") + "|" + strings.Join(cfg.Roles["cycle"], ","); got != "claude,codex|codex,mybot|claude" {
		t.Errorf("the agents of each role: %s", got)
	}
	for role, want := range map[string]string{"plan": "--model opus", "build": "--model haiku", "cycle": "--model haiku"} {
		if got := strings.Join(ExtraArgsForRole(root, "claude", role, nil), " "); got != want {
			t.Errorf("claude's arguments in %s: %q, want %q", role, got, want)
		}
	}
	if got := strings.Join(ExtraArgsForRole(root, "codex", "build", nil), " "); got != "-m gpt-x" {
		t.Errorf("codex's arguments in build: %q", got)
	}
	// claude never ran the line the file held: the command the binary built is written, with the
	// cap the application had set.
	claude := cfg.Commands["claude"]
	if want := strings.Replace(SUPPORTED["claude"].Run, "--max-budget-usd 5 ", "--max-budget-usd 4 ", 1); claude.Run != want {
		t.Errorf("claude's run command:\n%v\nwant:\n%s", claude.Run, want)
	}
	if !strings.Contains(claude.Restricted.(string), "--max-budget-usd 4 ") || claude.Ping != SUPPORTED["claude"].Ping {
		t.Errorf("claude's restricted and ping commands: %v | %v", claude.Restricted, claude.Ping)
	}
	// A command a person wrote is kept, with what the binary used to add written in it.
	if got := cfg.Commands["codex"].Run; got != "codex exec --json --budget 4 {{args}} -- {{prompt}}" {
		t.Errorf("codex's command: %v", got)
	}
	mybot := cfg.Commands["mybot"]
	if mybot.Run != "mybot --go {{args}} {{prompt}}" || mybot.StdinNull == nil || !*mybot.StdinNull {
		t.Errorf("mybot's commands: %+v", mybot)
	}

	// The strategies and the gates stay the application's own.
	exploration, _ := os.ReadFile(filepath.Join(root, ".foreman-kit", "exploration.yaml"))
	if strings.Contains(string(exploration), "needs:") || strings.Count(string(exploration), "session: visual_qa") != 4 || strings.Count(string(exploration), "session: cycle") != 6 {
		t.Errorf("the sessions of the strategies:\n%s", exploration)
	}
	evaluatorText, _ := os.ReadFile(filepath.Join(root, ".foreman-kit", "evaluator.yaml"))
	if !strings.Contains(string(evaluatorText), "instructions: Is the problem this application's own?") {
		t.Error("a question the application had rewritten was lost")
	}

	notes := strings.Join(m.Notes, "\n")
	for _, want := range []string{
		"gates.timeout: 10m (was limits.gate_seconds: 600 in .foreman-kit/pipeline.yaml)",
		"roles.build.agents: [codex, mybot] (was roles.build: [codex, mybot] in .foreman-kit/agents.yaml)",
		"commands.claude: --max-budget-usd 4 is written in its run and restricted commands",
		"commands.codex: kept as you wrote it",
		"cycle.every_when_out_of_work: never (was daemon.idle_delay_minutes: 0 in .foreman-kit/cycle.yaml)",
		"dispatch.wait_seconds: 600 in .foreman-kit/pipeline.yaml is no longer read",
		"max_consecutive_failures: 2 in .foreman-kit/cycle.yaml is no longer read",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q:\n%s", want, notes)
		}
	}
	// A value the kit ships anyway moves without a word.
	if strings.Contains(notes, "dispatch.priority_bump_every: 6h") || strings.Contains(notes, "retries: 1 (was") {
		t.Errorf("a value of the kit's own was announced:\n%s", notes)
	}

	files := Check(root)
	for _, f := range settings.Files() {
		for _, problem := range files[f.Path].Errors {
			// mybot is not installed here; that is the only thing check may hold against the files.
			t.Errorf("%s after the migration: %s", f.Path, problem)
		}
	}
	if again, err := MigrateConfig(root); err != nil || again.Pending() {
		t.Errorf("a second migration has files to rewrite: %v, %v", again.Changed, err)
	}
}

// What cannot be moved is not dropped quietly: the kit's value is written and a person is told.
func TestMigrateConfigSaysWhatItCouldNotMove(t *testing.T) {
	root := formerApp(t, map[string][]string{
		"pipeline": {"\n  merge_changed_files: verify", "\n  merge_changed_files: everything"},
		"agents": {
			"permissions: all", "permissions: restricted",
			`  claude: "claude -p {{prompt}} --output-format json --permission-mode bypassPermissions --no-session-persistence"`,
			`  claude: "claude -p {{prompt}} --model mine"`,
		},
		"evaluator": {"  failure_triage:\n", "  failure_triage_was_here:\n"},
	})
	// A gate the file does not have at all, and one of the application's own that nothing asks.
	m := applyMigration(t, root)
	notes := strings.Join(m.Notes, "\n")
	for _, want := range []string{
		"merge.outside_commits: REVIEW: gates.merge_changed_files: everything in .foreman-kit/pipeline.yaml does not fit it",
		"the kit's value verify was written",
		"commands.claude: REVIEW: it has no restricted command, and permissions is restricted",
		"gates.failure_triage: REVIEW: added with mode off, as it was while the file did not have this gate; the kit ships it as enforce",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes do not say %q:\n%s", want, notes)
		}
	}
	r := &settings.Reader{Root: root, FilesAlone: true}
	if got := r.String(settings.GateMode("failure_triage")); got != settings.GateOff {
		t.Errorf("a gate the file did not have was turned on: %s", got)
	}
	if got := Load(root).Commands["claude"].Run; got != "claude -p {{prompt}} --model mine {{args}}" {
		t.Errorf("claude's own command: %v", got)
	}
	// The command a person wrote has no restricted form: check says the agent cannot be used.
	if rep := Check(root)[AgentsFile]; !hasLine(rep.Errors, "commands.claude.restricted: missing", "cannot be used while permissions is restricted") {
		t.Errorf("check after the migration: %v", rep.Errors)
	}
	var evaluatorData map[string]any
	raw, _ := os.ReadFile(filepath.Join(root, ".foreman-kit", "evaluator.yaml"))
	if err := yaml.Unmarshal(raw, &evaluatorData); err != nil {
		t.Fatal(err)
	}
	if _, kept := evaluatorData["gates"].(map[string]any)["failure_triage_was_here"]; !kept {
		t.Error("a gate of the application's own was dropped")
	}

	// A file that is not valid YAML is left for a person, and the others are rewritten.
	broken := formerApp(t, nil)
	if err := os.WriteFile(filepath.Join(broken, ".foreman-kit", "cycle.yaml"), []byte("queue:\n  max_open: 8\n max_open: 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bm, err := MigrateConfig(broken)
	if err != nil {
		t.Fatal(err)
	}
	if _, rewritten := bm.Files[".foreman-kit/cycle.yaml"]; rewritten || !strings.Contains(strings.Join(bm.Notes, "\n"), ".foreman-kit/cycle.yaml is not valid YAML") {
		t.Errorf("a file that cannot be read: %v\n%s", bm.Changed, strings.Join(bm.Notes, "\n"))
	}
	if _, rewritten := bm.Files[".foreman-kit/pipeline.yaml"]; !rewritten {
		t.Error("the other files were not rewritten")
	}
}

// A file of today that lacks one key gets it, with the value the kit ships, and keeps its own.
func TestMigrateConfigAddsAKeyAFileOfTodayLacks(t *testing.T) {
	root := t.TempDir()
	for _, f := range settings.Files() {
		text, err := RenderSeed(f.Name, nil)
		if err != nil {
			t.Fatal(err)
		}
		if f.Name == "daemon" {
			text = []byte(strings.Replace(strings.Replace(string(text), "  every: 60m", "  every: 2h", 1), "  port: 4567 # the next free one when it is taken, unless --port names one\n", "", 1))
		}
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, text, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := applyMigration(t, root)
	if got := strings.Join(m.Changed, " "); got != ".foreman-kit/daemon.yaml" {
		t.Fatalf("files rewritten: %q", got)
	}
	r := &settings.Reader{Root: root, FilesAlone: true}
	if r.Int(settings.DaemonPort) != 4567 || r.Duration(settings.CycleEvery) != 2*time.Hour {
		t.Errorf("port %d, every %v", r.Int(settings.DaemonPort), r.Duration(settings.CycleEvery))
	}
	if len(r.Problems("daemon")) > 0 {
		t.Errorf("the file still has problems: %v", r.Problems("daemon"))
	}
}

func TestSessionsOfStrategies(t *testing.T) {
	block := "strategies:\n  - slug: a\n    name: A\n\n  - slug: b\n    needs: browser\n    name: B\n  - slug: c\n    session: visual_qa\n    needs: browser\n  - slug: d\n    session: cycle\n    directive: >-\n      text\n"
	got, browser := sessionsOfStrategies(block)
	want := "strategies:\n  - slug: a\n    session: cycle\n    name: A\n\n  - slug: b\n    session: visual_qa\n    name: B\n  - slug: c\n    session: visual_qa\n  - slug: d\n    session: cycle\n    directive: >-\n      text\n"
	if got != want || browser != 2 {
		t.Errorf("got (%d with a browser):\n%s\nwant:\n%s", browser, got, want)
	}
	if same, n := sessionsOfStrategies("strategies: []\n"); same != "strategies: []\n" || n != 0 {
		t.Errorf("no strategy: %q, %d", same, n)
	}
}
