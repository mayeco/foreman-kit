package settings_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// leaves returns every path of data that ends in a value: a scalar, a list, or an empty mapping.
func leaves(data any, prefix string) []string {
	m, ok := data.(map[string]any)
	if !ok || len(m) == 0 {
		return []string{prefix}
	}
	var out []string
	for k, v := range m {
		out = append(out, leaves(v, prefix+"."+k)...)
	}
	sort.Strings(out)
	return out
}

// at returns the value data holds at path, and whether the path is written at all.
func at(data map[string]any, path []string) (any, bool) {
	var cur any = data
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func seedOf(t *testing.T, file settings.File) map[string]any {
	t.Helper()
	raw, err := assets.ReadSeed(file.Path)
	if err != nil {
		t.Fatalf("%s has no seed: %v", file.Path, err)
	}
	var seed map[string]any
	if err := yaml.Unmarshal(raw, &seed); err != nil {
		t.Fatalf("the seed of %s is not valid YAML: %v", file.Path, err)
	}
	return seed
}

// TestEverySeedMatchesTheCatalog compares each seed with the catalog, key by key: a seed writes every
// key of its file with the value the catalog ships for it, and writes no key the catalog does not
// know. A key added to one and not to the other fails here.
func TestEverySeedMatchesTheCatalog(t *testing.T) {
	for _, file := range settings.Files() {
		seed := seedOf(t, file)
		known := map[string]bool{}
		for _, s := range settings.In(file.Name) {
			known[string(s.Key)] = true
			got, written := at(seed, s.Key.Parts())
			if !written {
				t.Errorf("%s: the seed does not write %s", file.Path, s.Key.Path())
				continue
			}
			if problem := s.Problem(got); problem != "" {
				t.Errorf("%s: the seed writes %s: %s, which %s", file.Path, s.Key.Path(), asJSON(t, got), problem)
			}
			want := s.Shipped()
			if want == nil {
				continue // a key without a value of the kit's own: the seed only has to write it
			}
			if s.Kind == settings.KindCommands {
				continue // the commands of the agents the seed lists; internal/config compares them
			}
			if asJSON(t, got) != asJSON(t, want) {
				t.Errorf("%s: the seed writes %s: %s, the catalog ships %s", file.Path, s.Key.Path(), asJSON(t, got), asJSON(t, want))
			}
		}

		for _, leaf := range leaves(seed, file.Name) {
			key := settings.Key(leaf)
			if known[leaf] || settings.IsContent(key) {
				continue
			}
			inside := false
			for k := range known {
				// What a list or a mapping holds (the command of an agent) is its key's.
				if strings.HasPrefix(leaf, k+".") {
					inside = true
					break
				}
			}
			if !inside {
				t.Errorf("%s: the seed writes %s, which the catalog does not know", file.Path, key.Path())
			}
		}

		// The seed has no problem of its own: every key written, nothing else.
		r := (&settings.Reader{FilesAlone: true}).Use(file.Name, seed)
		for _, p := range r.Problems(file.Name) {
			t.Errorf("the seed has a problem: %s", p)
		}
	}
}

// TestCatalogDefinesEveryKeyOnce checks the catalog against itself: each key belongs to a file,
// says what it decides and who reads it, and ships a value it accepts itself.
func TestCatalogDefinesEveryKeyOnce(t *testing.T) {
	all := settings.All()
	if len(all) != 88 {
		t.Errorf("the catalog has %d keys, want the 88 an application has", len(all))
	}
	perFile := map[string]int{}
	for _, s := range all {
		perFile[s.Key.File()]++
		if _, ok := settings.FileNamed(s.Key.File()); !ok {
			t.Errorf("%s belongs to no configuration file", s.Key)
		}
		if s.Means == "" || s.ReadBy == "" {
			t.Errorf("%s does not say what it decides or who reads it", s.Key)
		}
		if s.Kind == settings.KindEnum && len(s.Values) == 0 {
			t.Errorf("%s is an enum without values", s.Key)
		}
		if s.Never && s.Zero {
			t.Errorf("%s takes both never and 0s: a reader cannot tell them apart", s.Key)
		}
		for name, v := range map[string]any{"value the kit ships": s.Default, "seed value": s.Seed} {
			if v == nil {
				continue
			}
			if problem := s.Problem(v); problem != "" {
				t.Errorf("%s: its own %s %v %s", s.Key, name, v, problem)
			}
		}
		if s.Shipped() == nil && s.Key != settings.ExplorationStrategies {
			t.Errorf("%s ships no value: foreman-kit update could not write it in a file that lacks it", s.Key)
		}
		if got, ok := settings.Lookup(s.Key); !ok || got.Key != s.Key {
			t.Errorf("%s cannot be looked up", s.Key)
		}
	}
	want := map[string]int{"pipeline": 20, "agents": 20, "cycle": 12, "daemon": 5, "visual_qa": 7, "exploration": 2, "evaluator": 22}
	if asJSON(t, perFile) != asJSON(t, want) {
		t.Errorf("keys per file: %v, want %v", perFile, want)
	}
	var names []string
	for _, f := range settings.Files() {
		names = append(names, f.Name)
		if f.About == "" || f.ReadBy == "" {
			t.Errorf("%s does not say what it decides or who reads it", f.Path)
		}
	}
	if got := strings.Join(names, " "); got != "pipeline agents cycle daemon visual_qa exploration evaluator" {
		t.Errorf("the files, in order: %s", got)
	}
	if settings.Workers.Path() != "dispatch.workers" || settings.Workers.FilePath() != ".foreman-kit/pipeline.yaml" ||
		settings.Workers.Where() != "dispatch.workers in .foreman-kit/pipeline.yaml" {
		t.Errorf("a key does not name its path and its file: %q, %q", settings.Workers.Path(), settings.Workers.FilePath())
	}
	if settings.VisualQASlot.File() != "visual_qa" || settings.VisualQASlot.Path() != "slot" {
		t.Errorf("a key of .foreman-kit/visual_qa.yaml does not name its file: %q, %q", settings.VisualQASlot.File(), settings.VisualQASlot.Path())
	}
}

func TestParseAcceptsWhatEachKindDescribes(t *testing.T) {
	cases := []struct {
		key  settings.Key
		raw  any
		want any
		ok   bool
	}{
		{settings.Workers, 4, 4, true},
		{settings.Workers, "4", 4, true}, // a file may quote it
		{settings.Workers, 4.0, 4, true},
		{settings.Workers, 0, nil, false},
		{settings.Workers, 2.5, nil, false},
		{settings.Workers, true, nil, false},
		{settings.Workers, "never", nil, false},
		{settings.AgentRetries, 0, 0, true},
		{settings.AgentRetries, -1, nil, false},
		{settings.OutageGiveUpAfter, 3, 3, true},
		{settings.OutageGiveUpAfter, "never", 0, true},
		{settings.OutageGiveUpAfter, 0, nil, false}, // never is a word, not a number
		{settings.BaseBranch, "release/1.x", "release/1.x", true},
		{settings.BaseBranch, "not a branch", nil, false},
		{settings.OutsideCommits, "ignore", "ignore", true},
		{settings.OutsideCommits, "change", nil, false}, // a word of before
		{settings.VisualQAEnabled, false, false, true},
		{settings.VisualQAEnabled, "false", nil, false},
		{settings.VisualQASlot, "none", 0, true},
		{settings.VisualQASlot, 97, 97, true},
		{settings.VisualQASlot, "97", 97, true},
		{settings.VisualQASlot, false, nil, false}, // how it said none before
		{settings.VisualQASlot, -1, nil, false},
		{settings.EvaluatorTimeout, "30s", 30 * time.Second, true},
		{settings.EvaluatorTimeout, "1h30m", 90 * time.Minute, true},
		{settings.EvaluatorTimeout, "30", nil, false},
		{settings.EvaluatorTimeout, 30, nil, false},
		{settings.EvaluatorTimeout, "500ms", nil, false},
		{settings.EvaluatorTimeout, "never", nil, false}, // this setting has to end
		{settings.EvaluatorTimeout, "0s", nil, false},
		{settings.IdleTimeout, "never", time.Duration(0), true},
		{settings.IdleTimeout, "0s", nil, false},
		{settings.AgentRetryAfter, "0s", time.Duration(0), true},
		{settings.AgentRetryAfter, "never", nil, false},
		{settings.CycleEveryWhenOutOfWork, "15m", 15 * time.Minute, true},
		{settings.CycleEveryWhenOutOfWork, "10s", nil, false}, // a judgment every ten seconds
		{settings.CycleEveryWhenOutOfWork, "never", time.Duration(0), true},
		{settings.RoadmapFile, "", "", true},
		{settings.DiagnosticsBaseURL, "", "", true},
		{settings.DiagnosticsModel, "", nil, false},
		{settings.GateMode("plan_review"), "enforce", "enforce", true},
		{settings.GateMode("plan_review"), false, nil, false}, // YAML reads a bare off as false
	}
	for _, c := range cases {
		s := settings.Of(c.key)
		got, ok := s.Parse(c.raw)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: Parse(%#v) = %#v, %v; want %#v, %v", c.key, c.raw, got, ok, c.want, c.ok)
		}
		if (s.Problem(c.raw) == "") != c.ok {
			t.Errorf("%s: Problem(%#v) = %q", c.key, c.raw, s.Problem(c.raw))
		}
	}
	if got := settings.Of(settings.Workers).Problem(0); got != "must be a whole number above 0" {
		t.Errorf("unexpected wording: %q", got)
	}
	if got := settings.Of(settings.OutsideCommits).Problem("x"); got != "must be verify, ignore or include" {
		t.Errorf("unexpected wording: %q", got)
	}
	if got := settings.Of(settings.CycleEveryWhenOutOfWork).Problem("10s"); got != "must be a time with its unit (30s, 15m, 2h), 1m or more, or never" {
		t.Errorf("unexpected wording: %q", got)
	}
	if got := settings.Of(settings.GateMode("plan_review")).Problem(false); !strings.Contains(got, "quote it") {
		t.Errorf("a bare off is not explained: %q", got)
	}
}

func TestFormatDurationWritesWhatParseDurationReads(t *testing.T) {
	for text, want := range map[string]string{"7200s": "2h", "45m": "45m", "90m": "1h30m", "90s": "1m30s", "3661s": "1h1m1s", "60m": "1h"} {
		d, ok := settings.ParseDuration(text)
		if !ok {
			t.Fatalf("%s is not read", text)
		}
		if got := settings.FormatDuration(d); got != want {
			t.Errorf("%s is written %s, want %s", text, got, want)
		}
		if back, ok := settings.ParseDuration(settings.FormatDuration(d)); !ok || back != d {
			t.Errorf("%s does not read back as written", text)
		}
	}
	if got := settings.FormatDuration(0); got != "0s" {
		t.Errorf("no time at all is written %q", got)
	}
}

func writeConfig(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReaderTakesTheFileAndNoVariable: one value per key, from its file. No environment variable
// changes it. A key that is missing or holds something it does not accept is said once, by name,
// and read as the value the kit ships.
func TestReaderTakesTheFileAndNoVariable(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, ".foreman-kit/pipeline.yaml", "repository:\n  base_branch: main\ndispatch:\n  workers: 4\n  priority_bump_every: soon\ngates:\n  timeout: 10m\nblocked_changes:\n  retries:\n")
	writeConfig(t, root, ".foreman-kit/visual_qa.yaml", "enabled: false\nslot: 97\nskills: [browser-qa, 7]\n")
	writeConfig(t, root, ".foreman-kit/cycle.yaml", "roadmap:\n  file: \"\"\n")
	writeConfig(t, root, ".foreman-kit/evaluator.yaml", "timeout: 2s\ngates:\n  plan_review:\n    mode: enforce\n")

	var stderr bytes.Buffer
	r := &settings.Reader{Root: root, FilesAlone: true, Stderr: &stderr}

	if got := r.String(settings.BaseBranch); got != "main" {
		t.Errorf("base_branch = %q", got)
	}
	if got := r.Int(settings.Workers); got != 4 {
		t.Errorf("workers = %d", got)
	}
	if got := r.Duration(settings.GateTimeout); got != 10*time.Minute {
		t.Errorf("gates.timeout = %v", got)
	}
	if got := r.Seconds(settings.GateTimeout); got != 600 {
		t.Errorf("gates.timeout in seconds = %d", got)
	}
	if r.Bool(settings.VisualQAEnabled) {
		t.Error("enabled: false was read as true")
	}
	if got := r.Slot(settings.VisualQASlot); got != 97 {
		t.Errorf("slot = %d", got)
	}
	if got := r.Strings(settings.VisualQASkills); asJSON(t, got) != `["browser-qa","7"]` {
		t.Errorf("skills = %v", got)
	}
	if got := r.Duration(settings.EvaluatorTimeout); got != 2*time.Second {
		t.Errorf("timeout = %v", got)
	}
	if got := r.String(settings.GateMode("plan_review")); got != "enforce" {
		t.Errorf("plan_review = %q", got)
	}
	// An empty text is a value where the key says so: no roadmap.
	if got := r.String(settings.RoadmapFile); got != "" {
		t.Errorf("roadmap.file = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("a key the file writes was announced: %s", stderr.String())
	}

	// A key the file does not write, or writes with nothing after it: said once, and read as the
	// value the kit ships.
	for i := 0; i < 2; i++ {
		if got := r.Duration(settings.LocksStaleAfter); got != 2*time.Hour {
			t.Errorf("locks.stale_after = %v", got)
		}
		if got := r.Int(settings.BlockedRetries); got != 1 {
			t.Errorf("blocked_changes.retries = %d", got)
		}
	}
	for _, want := range []string{
		"settings: locks.stale_after in .foreman-kit/pipeline.yaml is missing; reading it as 2h\n",
		"settings: blocked_changes.retries in .foreman-kit/pipeline.yaml is missing; reading it as 1\n",
	} {
		if n := strings.Count(stderr.String(), want); n != 1 {
			t.Errorf("stderr says %q %d times, want once:\n%s", want, n, stderr.String())
		}
	}
	// Not a value of the key: the same, with what it takes.
	stderr.Reset()
	if got := r.Duration(settings.PriorityBumpEvery); got != 6*time.Hour {
		t.Errorf("priority_bump_every = %v", got)
	}
	if want := `settings: dispatch.priority_bump_every in .foreman-kit/pipeline.yaml must be a time with its unit (30s, 15m, 2h), or never, not "soon"; reading it as 6h`; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
	// A gate the file does not list is a missing key like any other.
	stderr.Reset()
	if got := r.String(settings.GateMode("change_risk")); got != "advisory" {
		t.Errorf("a gate the file does not list is read as %q", got)
	}
	if !strings.Contains(stderr.String(), "gates.change_risk.mode in .foreman-kit/evaluator.yaml is missing") {
		t.Errorf("stderr = %q", stderr.String())
	}
	// A file that is not there is said once, whatever number of its keys is read.
	stderr.Reset()
	if got := r.Duration(settings.CycleEvery); got != time.Hour {
		t.Errorf("cycle.every = %v", got)
	}
	if got := r.Int(settings.DaemonPort); got != 4567 {
		t.Errorf("portal.port = %d", got)
	}
	if n := strings.Count(stderr.String(), ".foreman-kit/daemon.yaml is missing"); n != 1 || strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("a missing file was reported %d times:\n%s", n, stderr.String())
	}

	// No environment variable changes what a key holds: the ones an older foreman-kit read are
	// not read, whatever they say. A value for one run is given with --set.
	stderr.Reset()
	for name, value := range map[string]string{
		"OPENSPEC_WORKERS": "8", "OPENSPEC_LLM_ENABLED": "1", "OPENSPEC_GATE_TIMEOUT": "90",
		"AGENT_IDLE_SECONDS": "0", "AGENT_RETRY_DELAY_SECONDS": "0", "OPENSPEC_AGING_HOURS": "2",
		"AGENT_TIMEOUT_SECONDS": "5", "OPENSPEC_PROTECTED_PATHS": "repo.config.yaml  deploy/",
		"OPENSPEC_BASE_BRANCH": "release", "AGENT_ENGINE": "codex", "AGENT_ENGINE_BUILD": "codex",
		"AGENT_ORDER": "random", "AGENT_PERMISSIONS": "restricted", "JEV_MODEL": "jev-9", "JEV_BASE_URL": "http://localhost:1",
	} {
		t.Setenv(name, value)
	}
	env := &settings.Reader{Root: root, Stderr: &stderr}
	if got := env.Int(settings.Workers); got != 4 {
		t.Errorf("OPENSPEC_WORKERS changed dispatch.workers to %d", got)
	}
	if env.Bool(settings.DiagnosticsEnabled) {
		t.Error("OPENSPEC_LLM_ENABLED turned the diagnostics on")
	}
	if got := env.Duration(settings.GateTimeout); got != 10*time.Minute {
		t.Errorf("OPENSPEC_GATE_TIMEOUT changed gates.timeout to %v", got)
	}
	if got := env.Duration(settings.RoleTimeout("build")); got != 2*time.Hour {
		t.Errorf("AGENT_TIMEOUT_SECONDS changed roles.build.timeout to %v", got)
	}
	if got := env.String(settings.BaseBranch); got != "main" {
		t.Errorf("OPENSPEC_BASE_BRANCH changed repository.base_branch to %q", got)
	}
	if got := env.String(settings.AgentSelection); got != settings.SelectionInOrder {
		t.Errorf("AGENT_ORDER changed selection to %q", got)
	}
	if got := env.String(settings.AgentPermissions); got != settings.PermissionsFull {
		t.Errorf("AGENT_PERMISSIONS changed permissions to %q", got)
	}
	if got := env.String(settings.EvaluatorModel); got == "jev-9" {
		t.Error("JEV_MODEL changed the model of the evaluator")
	}

	d := env.Describe(settings.Workers)
	if d.Value != 4 || d.Default != 2 || !d.Set || d.Problem != "" || d.File != ".foreman-kit/pipeline.yaml" || d.OverriddenBy != "" ||
		strings.Contains(d.Means, "OPENSPEC_WORKERS") {
		t.Errorf("Describe(workers) = %+v", d)
	}
	// What a page needs to show the key and to take a new value for it: its kind, what it accepts
	// in words, and the value in effect.
	if d.Kind != settings.KindInt || d.Accepts != "a whole number above 0" || d.InEffect != "4" {
		t.Errorf("Describe(workers): kind %q, accepts %q, in effect %q", d.Kind, d.Accepts, d.InEffect)
	}
	for _, s := range settings.All() {
		if strings.Contains(s.Explained(), "it for one run)") {
			t.Errorf("%s still names a variable that overrides it: %s", s.Key, s.Explained())
		}
	}
	if d := r.Describe(settings.LocksStaleAfter); d.Set || d.Value != nil || d.Default != "2h" || d.Problem != "missing" || d.InEffect != "" {
		t.Errorf("Describe of a missing key = %+v", d)
	}
	if d := r.Describe(settings.PriorityBumpEvery); !d.Set || d.Value != "soon" || !strings.HasPrefix(d.Problem, "must be a time") || d.InEffect != "" ||
		d.Kind != settings.KindDuration || d.Accepts != "a time with its unit (30s, 15m, 2h), or never" {
		t.Errorf("Describe of a key that holds something else = %+v", d)
	}
	if d := r.Describe(settings.BaseBranch); d.Default != "main" {
		t.Errorf("the base branch shows the value the seed writes: %+v", d)
	}
}

// TestReaderReadsNothingFromAFileThatIsNotYAML: every key of the file is read as the value the kit
// ships, the base branch has none, and the file is reported once however many keys are read.
func TestReaderReadsNothingFromAFileThatIsNotYAML(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, ".foreman-kit/pipeline.yaml", "repository:\n  base_branch: develop\ndispatch:\n  workers: 5\ndispatch:\n  workers: 6\n")
	var stderr bytes.Buffer
	r := &settings.Reader{Root: root, FilesAlone: true, Stderr: &stderr}
	if got := r.String(settings.BaseBranch); got != "" {
		t.Errorf("base_branch = %q: a branch nobody can read is no branch", got)
	}
	if got := r.Int(settings.Workers); got != 2 {
		t.Errorf("workers = %d, want the value the kit ships", got)
	}
	if n := strings.Count(stderr.String(), "not valid YAML"); n != 1 {
		t.Errorf("the file was reported %d times:\n%s", n, stderr.String())
	}
	if _, present, err := r.Data("pipeline"); !present || err == nil {
		t.Errorf("Data must say the file is there and cannot be read: %v %v", present, err)
	}
	if problems := r.Problems("pipeline"); len(problems) != 1 || !strings.Contains(problems[0].Text, "not valid YAML") {
		t.Errorf("the problems of a file that cannot be read: %v", problems)
	}

	// A file held by the caller is read as given.
	held := settings.At(root).Use("cycle", map[string]any{"budget": map[string]any{"total": "30m"}})
	if got := held.Duration(settings.CycleBudget); got != 30*time.Minute {
		t.Errorf("a file handed to the reader gave %v", got)
	}
}

// TestProblemsNameWhatAFileLacksAndWhatItShouldNotHold: a key of today that is missing or invalid,
// a key of before with where it went, and a key nothing ever read.
func TestProblemsNameWhatAFileLacksAndWhatItShouldNotHold(t *testing.T) {
	seed := seedOf(t, settings.Files()[0])
	gates := seed["gates"].(map[string]any)
	delete(gates, "timeout")
	seed["dispatch"].(map[string]any)["workers"] = 0
	seed["limits"] = map[string]any{"gate_seconds": 2700, "agent_budget_usd": 5}
	seed["base_branch"] = "main"
	seed["dispatch"].(map[string]any)["wokers"] = 3

	r := (&settings.Reader{FilesAlone: true}).Use("pipeline", seed)
	var got []string
	for _, p := range r.Problems("pipeline") {
		if p.File != ".foreman-kit/pipeline.yaml" {
			t.Errorf("a problem of another file: %+v", p)
		}
		got = append(got, p.Text)
	}
	sort.Strings(got)
	want := []string{
		"base_branch: no longer read: it is repository.base_branch in .foreman-kit/pipeline.yaml (foreman-kit update rewrites the file)",
		"dispatch.wokers: not a setting of this file (a typo?)",
		"dispatch.workers: must be a whole number above 0 (is 0)",
		"gates.timeout: missing (the kit ships 45m)",
		"limits.agent_budget_usd: no longer read: a spending cap is an option of the agent's own command (--max-budget-usd in commands.claude of .foreman-kit/agents.yaml)",
		"limits.gate_seconds: no longer read: it is gates.timeout in .foreman-kit/pipeline.yaml (foreman-kit update rewrites the file)",
	}
	if asJSON(t, got) != asJSON(t, want) {
		t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A file that is not there has one problem, and says what writes it.
	missing := (&settings.Reader{Root: t.TempDir(), FilesAlone: true}).Problems("daemon")
	if len(missing) != 1 || missing[0].Text != "missing (foreman-kit update writes it)" {
		t.Errorf("a missing file: %v", missing)
	}

	// What .foreman-kit/agents.yaml held before: a role was a list, extra_args a mapping per agent.
	agents := map[string]any{
		"roles":      map[string]any{"plan": []any{"claude"}},
		"order":      "fallback",
		"extra_args": map[string]any{"claude": map[string]any{"plan": "--model x", "default": "--model y"}},
	}
	var texts []string
	for _, p := range (&settings.Reader{FilesAlone: true}).Use("agents", agents).Problems("agents") {
		if strings.Contains(p.Text, "no longer read") {
			texts = append(texts, p.Text)
		}
	}
	sort.Strings(texts)
	wantFormer := []string{
		"extra_args: no longer read: it is roles.plan.args in .foreman-kit/agents.yaml (foreman-kit update rewrites the file)",
		"order: no longer read: it is selection in .foreman-kit/agents.yaml (foreman-kit update rewrites the file)",
		"roles.plan: no longer read: it is roles.plan.agents in .foreman-kit/agents.yaml (foreman-kit update rewrites the file)",
	}
	if asJSON(t, texts) != asJSON(t, wantFormer) {
		t.Errorf("former keys of .foreman-kit/agents.yaml:\n%s\nwant:\n%s", strings.Join(texts, "\n"), strings.Join(wantFormer, "\n"))
	}
}

// TestMovedGivesEachKeyWhatItsFormerKeysHeld: the values an application wrote before the seven files
// arrive at the keys of today in the shape those take.
func TestMovedGivesEachKeyWhatItsFormerKeysHeld(t *testing.T) {
	old := map[settings.Key]any{
		"pipeline.base_branch":                              "develop",
		"pipeline.limits.agent_seconds":                     1800,
		"pipeline.limits.agent_idle_seconds":                0,
		"pipeline.limits.agent_retry_delay_seconds":         0,
		"pipeline.dispatch.aging_hours":                     0,
		"pipeline.dispatch.blocked_retry_hours":             2,
		"pipeline.gates.merge_changed_files":                "change",
		"pipeline.llm.provider":                             "gemini",
		"agents.order":                                      "spread",
		"agents.permissions":                                "all",
		"agents.roles.build":                                []any{"codex", "claude"},
		"agents.extra_args":                                 map[string]any{"claude": map[string]any{"plan": "--model a", "default": "--model b"}, "codex": "-m c"},
		"cycle.qa_time_budget_minutes":                      50,
		"cycle.visual_qa.minutes":                           40,
		"cycle.visual_qa.slot":                              false,
		"cycle.daemon.idle_delay_minutes":                   0,
		"cycle.proposals.approval":                          "auto",
		"cycle.exploration.max_new_backlog_items_per_cycle": 4,
		"cycle.memory.max_chars":                            0,
		"evaluator.on_unavailable":                          "skip",
		"exploration.order":                                 "rotation",
	}
	read := func(key settings.Key) any { return old[key] }
	want := map[settings.Key]any{
		settings.BaseBranch:               "develop",
		settings.RoleTimeout("plan"):      "30m",
		settings.RoleTimeout("build"):     "30m",
		settings.RoleTimeout("cycle"):     "1h5m", // the budget and the margin the kit used to add
		settings.IdleTimeout:              "never",
		settings.AgentRetryAfter:          "0s",
		settings.PriorityBumpEvery:        "never",
		settings.BlockedRetryAfter:        "2h",
		settings.BaseRecheckAfter:         "2h",
		settings.OutsideCommits:           "ignore",
		settings.DiagnosticsProvider:      "google",
		settings.AgentSelection:           "least_busy",
		settings.AgentPermissions:         "full",
		settings.RoleAgents("build"):      []any{"codex", "claude"},
		settings.RoleArgs("plan"):         map[string]any{"claude": "--model a", "codex": "-m c"},
		settings.RoleArgs("cycle"):        map[string]any{"claude": "--model b", "codex": "-m c"},
		settings.CycleBudget:              "50m",
		settings.VisualQATimeBudget:       "40m",
		settings.VisualQAExpiresAfter:     "1h10m", // the budget and the margin the kit used to add
		settings.VisualQASlot:             "none",
		settings.CycleEveryWhenOutOfWork:  "never",
		settings.ProposalsApprovedBy:      "nobody",
		settings.QueueMaxFindings:         4,
		settings.VisualQAMaxFindings:      4,
		settings.MemoryMaxChars:           12000, // 0 meant the value the kit ships
		settings.EvaluatorWhenUnavailable: "pass",
		settings.ExplorationSelection:     "in_order",
	}
	for key, value := range want {
		move, ok := settings.Moved(key, read)
		if !ok || asJSON(t, move.Value) != asJSON(t, value) {
			t.Errorf("%s: moved %v (%v), want %v", key, move.Value, ok, value)
		}
		if ok && settings.Of(key).Problem(move.Value) != "" {
			t.Errorf("%s: the value moved, %v, is not one it takes", key, move.Value)
		}
	}
	// With no automatic retry of a blocked change there was no recheck of a base either.
	old["pipeline.dispatch.blocked_retries"] = 0
	if move, ok := settings.Moved(settings.BaseRecheckAfter, read); !ok || move.Value != "never" {
		t.Errorf("base_recheck_after with blocked_retries 0: %v, %v", move.Value, ok)
	}
	// A wait of 0 hours was never shorter than five minutes.
	old["pipeline.dispatch.blocked_retries"], old["pipeline.dispatch.blocked_retry_hours"] = 1, 0
	if move, ok := settings.Moved(settings.BaseRecheckAfter, read); !ok || move.Value != "5m" {
		t.Errorf("base_recheck_after with blocked_retry_hours 0: %v, %v", move.Value, ok)
	}
	// A key nobody wrote before moves nothing, and a word the kit never knew is said to be lost.
	if _, ok := settings.Moved(settings.GateTimeout, read); ok {
		t.Error("a key no former key was written for moved something")
	}
	old["agents.order"] = "sideways"
	if move, ok := settings.Moved(settings.AgentSelection, read); ok || move.Lost != "agents.order" {
		t.Errorf("a word that does not convert: %+v, %v", move, ok)
	}
	// A key that kept its name and already holds a word of today is left alone.
	old["agents.permissions"] = "restricted"
	if move, ok := settings.Moved(settings.AgentPermissions, read); ok || move.Lost != "" {
		t.Errorf("a value of today was moved: %+v", move)
	}

	if !settings.HasFormer("pipeline", map[string]any{"limits": map[string]any{"gate_seconds": 60}}) ||
		!settings.HasFormer("agents", map[string]any{"permissions": "all"}) ||
		!settings.HasFormer("agents", map[string]any{"roles": map[string]any{"plan": []any{"claude"}}}) {
		t.Error("a file written for an older foreman-kit is not told apart")
	}
	if settings.HasFormer("agents", map[string]any{"permissions": "full", "roles": map[string]any{"plan": map[string]any{"agents": []any{"claude"}}}}) {
		t.Error("a file of today is taken for one of before")
	}
}

// TestUnusableSaysWhyAProcessStartsNoNewWork: a process reads the files of its own and starts
// nothing new while one of them lacks a key, holds a value the key does not accept or writes a key
// nothing reads. The answer names the first problem and counts the others, names a file an older
// kit wrote as that, and is empty for files that are whole, whatever the files of another process
// hold.
func TestUnusableSaysWhyAProcessStartsNoNewWork(t *testing.T) {
	whole := func() *settings.Reader {
		r := &settings.Reader{FilesAlone: true}
		for _, f := range settings.Files() {
			r.Use(f.Name, seedOf(t, f))
		}
		return r
	}
	for name, files := range map[string][]string{
		"dispatcher": settings.DispatcherFiles, "merger": settings.MergerFiles, "cycle": settings.CycleFiles,
		"visual session": settings.VisualSessionFiles, "daemon": settings.DaemonFiles, "agent call": settings.AgentCallFiles,
	} {
		if why := whole().Unusable(files...); why != "" {
			t.Errorf("the %s waits on the files the kit ships: %s", name, why)
		}
		for _, file := range files {
			if _, ok := settings.FileNamed(file); !ok {
				t.Errorf("the %s reads %q, which is not a configuration file", name, file)
			}
		}
	}

	// One key missing: the answer names it, and how to see the rest.
	pipeline := seedOf(t, settings.Files()[0])
	delete(pipeline["gates"].(map[string]any), "timeout")
	r := whole().Use("pipeline", pipeline)
	if got, want := r.Unusable(settings.DispatcherFiles...), ".foreman-kit/pipeline.yaml: gates.timeout: missing (the kit ships 45m) (foreman-kit config check)"; got != want {
		t.Errorf("one key missing:\n%s\nwant:\n%s", got, want)
	}
	// The daemon and an agent call do not read that file: they do not wait for it.
	if why := r.Unusable(settings.DaemonFiles...) + r.Unusable(settings.AgentCallFiles...); why != "" {
		t.Errorf("a process waits for a file it does not read: %s", why)
	}

	// More than one: the first, and how many more.
	pipeline["dispatch"].(map[string]any)["workers"] = 0
	pipeline["dispatch"].(map[string]any)["wokers"] = 3
	r = whole().Use("pipeline", pipeline)
	if got := r.Unusable(settings.MergerFiles...); !strings.HasPrefix(got, ".foreman-kit/pipeline.yaml: dispatch.workers: must be a whole number above 0 (is 0)") ||
		!strings.HasSuffix(got, "(and 2 more: foreman-kit config check lists them)") {
		t.Errorf("three problems: %s", got)
	}

	// A file an older kit wrote is named as that, with the command that rewrites it.
	former := map[string]any{"base_branch": "main", "dispatch": map[string]any{"max_workers": 2}}
	r = whole().Use("pipeline", former)
	if got, want := r.Unusable(settings.DispatcherFiles...), ".foreman-kit/pipeline.yaml holds keys an older foreman-kit read (base_branch): foreman-kit update rewrites it"; got != want {
		t.Errorf("a file of an older kit:\n%s\nwant:\n%s", got, want)
	}

	// A file that is not there.
	if got, want := settings.Unusable(t.TempDir(), settings.DaemonFiles...), ".foreman-kit/daemon.yaml: missing (foreman-kit update writes it) (foreman-kit config check)"; got != want {
		t.Errorf("a missing file:\n%s\nwant:\n%s", got, want)
	}
}
