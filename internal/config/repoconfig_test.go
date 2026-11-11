package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func asMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		t.Fatalf("not valid YAML: %v\n%s", err, raw)
	}
	return data
}

// TestMigrateRepoConfigMovesEachKeyAndKeepsEveryOtherLine: the repo.config.yaml an older kit wrote
// for a repository, with its comments, is rewritten with each of the five keys under its name of
// today. Every other line stays as it was, and so does every value.
func TestMigrateRepoConfigMovesEachKeyAndKeepsEveryOtherLine(t *testing.T) {
	raw := readFixture(t, "former/repos/web.repo.config.yaml")
	out, notes, err := MigrateRepoConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if want := readFixture(t, "former/repos/web.repo.config.migrated.yaml"); string(out) != string(want) {
		t.Fatalf("the file written is not the expected one:\n%s", out)
	}
	wantNotes := []string{
		"required_services: was requires",
		"setup: was gates.setup",
		"visual_qa.prepare: was service.prepare",
		"regression.commands: was regression",
		"regression.when: was regression_when",
	}
	if strings.Join(notes, "\n") != strings.Join(wantNotes, "\n") {
		t.Fatalf("notes:\n%s\nwant:\n%s", strings.Join(notes, "\n"), strings.Join(wantNotes, "\n"))
	}

	now := asMap(t, out)
	if len(RepoFormerKeys(now)) != 0 {
		t.Fatalf("the file written still holds former keys: %v", RepoFormerKeys(now))
	}
	if got := now["setup"]; got != "RAILS_ENV=test bin/rails db:prepare" {
		t.Errorf("setup = %v", got)
	}
	if got := digMap(now, "visual_qa", "prepare"); !strings.HasPrefix(asText(got), "ln -sf $OPENSPEC_MAIN_ROOT/repos/web/") {
		t.Errorf("visual_qa.prepare = %v", got)
	}
	if got := digMap(now, "required_services", "postgres"); got != "pg_isready -h localhost -p 5432" {
		t.Errorf("required_services.postgres = %v", got)
	}
	if got := digMap(now, "regression", "commands", "test"); got != "bundle exec rspec" {
		t.Errorf("regression.commands.test = %v", got)
	}
	if got, _ := digMap(now, "regression", "when").([]any); len(got) != 4 || got[2] != "!config/locales/**" {
		t.Errorf("regression.when = %v", got)
	}
	if _, isGate := digMap(now, "gates").(map[string]any)["setup"]; isGate {
		t.Error("setup is still a gate")
	}

	// A file of today is returned as it is: update is run more than once.
	again, againNotes, err := MigrateRepoConfig(out)
	if err != nil || string(again) != string(out) || len(againNotes) != 0 {
		t.Fatalf("a file of today was rewritten (%v, notes %v):\n%s", err, againNotes, again)
	}
}

func asText(v any) string {
	s, _ := v.(string)
	return s
}

// TestMigrateRepoConfigInTheShapesPeopleWrite: the keys move whatever way the file writes them (a
// value on one line or a block below its key, a list written either way, more or less
// indentation, values left empty), and the lines around them keep their blank lines and comments.
func TestMigrateRepoConfigInTheShapesPeopleWrite(t *testing.T) {
	for name, c := range map[string]struct {
		in, want string
	}{
		"only requires": {
			in: "name: api\nrequires:\n  redis: \"redis-cli ping\"\ngates:\n  test: \"go test ./...\"\n",
			want: "name: api\nrequired_services:\n  redis: \"redis-cli ping\"\ngates:\n  test: \"go test ./...\"\n" +
				"\nregression:\n  commands:\n    test: go test ./...\n",
		},
		"values on one line": {
			in: "name: api\nrequires: {}\ngates:\n  setup: make deps # once per slot\n  test: \"go test ./...\"\nregression: {full: \"go test -count=1 ./...\"}\nregression_when: [\"internal/**\"]\n",
			want: "name: api\nrequired_services: {}\nsetup: make deps # once per slot\n\ngates:\n  test: \"go test ./...\"\n" +
				"regression:\n  commands: {full: \"go test -count=1 ./...\"}\n  when: [\"internal/**\"]\n",
		},
		"a block scalar and a list of its own lines": {
			in: "gates:\n  setup: |\n    bin/setup\n    bin/rails db:prepare\n  test: bin/test\nregression:\n  full: bin/test --all\nregression_when:\n  - \"app/**\"\n  - \"!app/assets/**\"\n",
			want: "setup: |\n  bin/setup\n  bin/rails db:prepare\n\ngates:\n  test: bin/test\n" +
				"regression:\n  commands:\n    full: bin/test --all\n  when:\n    - \"app/**\"\n    - \"!app/assets/**\"\n",
		},
		"a list at the margin": {
			in:   "gates:\n  test: bin/test\nregression:\n  full: bin/test --all\nregression_when:\n- \"app/**\"\n",
			want: "gates:\n  test: bin/test\nregression:\n  commands:\n    full: bin/test --all\n  when:\n    - \"app/**\"\n",
		},
		"four spaces": {
			in: "service:\n    dev_command: bin/dev\n    # before the session\n    prepare: bin/prepare\ngates:\n    setup: bin/setup\n    test: bin/test\nname: api\n",
			want: "service:\n    dev_command: bin/dev\n\n# before the session\nvisual_qa:\n  prepare: bin/prepare\nsetup: bin/setup\n\ngates:\n    test: bin/test\n" +
				"\nregression:\n  commands:\n    test: bin/test\n\nname: api\n",
		},
		"setup was the only gate": {
			in:   "gates:\n  setup: bin/setup\n",
			want: "setup: bin/setup\n\ngates: {}\n",
		},
		"regression_when above regression": {
			in:   "regression_when: [\"db/**\"]\n\nregression:\n  full: bin/test --all\n",
			want: "regression:\n  commands:\n    full: bin/test --all\n  when: [\"db/**\"]\n",
		},
		"blank lines and comments around what moves": {
			in:   "name: api\n\n\n# the suite\nregression:   # nightly too\n  full: bin/test --all\n\n# what triggers it\nregression_when: [\"db/**\"]   # migrations\n\nstack: go\n\n",
			want: "name: api\n\n\n# the suite\nregression:   # nightly too\n  commands:\n    full: bin/test --all\n  # what triggers it\n  when: [\"db/**\"]   # migrations\n\nstack: go\n\n",
		},
		"values left empty": {
			in:   "service:\n  dev_command: bin/dev\n  prepare: \"\"\nrequires: {}\ngates:\n  setup: \"\"   # e.g. bin/setup\n  test: \"\"\nregression:\n  test: \"{{REGRESSION_COMMAND}}\"\nregression_when: []\n",
			want: "service:\n  dev_command: bin/dev\n\nvisual_qa:\n  prepare: \"\"\nrequired_services: {}\nsetup: \"\"   # e.g. bin/setup\n\ngates:\n  test: \"\"\nregression:\n  commands: {}\n  when: []\n",
		},
		"a regression of today beside a former key": {
			in:   "regression:\n    commands:\n        full: bin/test --all\nregression_when:\n  - \"db/**\"\ngates:\n  test: bin/test\n",
			want: "regression:\n    commands:\n        full: bin/test --all\n    when:\n      - \"db/**\"\ngates:\n  test: bin/test\n",
		},
		"a regression of today is left as it is": {
			in:   "requires: {}\ngates:\n  test: bin/test\nregression:\n  commands: {}\n",
			want: "required_services: {}\ngates:\n  test: bin/test\nregression:\n  commands: {}\n",
		},
	} {
		out, _, err := MigrateRepoConfig([]byte(c.in))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(out) != c.want {
			t.Errorf("%s:\n%s\nwant:\n%s", name, out, c.want)
		}
		if again, _, err := MigrateRepoConfig(out); err != nil || string(again) != string(out) {
			t.Errorf("%s: the file written is rewritten again (%v):\n%s", name, err, again)
		}
	}
}

// TestMigrateRepoConfigWritesTheGatesAsTheRegressionThatRanThem: the regression of a repository with
// no command of its own ran its gates, whether its file wrote an empty regression or none. Nothing
// stands in for a missing value any more, so the gates that run are written as its regression
// commands, in their order, and the repository goes on checking what it checked. A repository
// without a gate that runs has no regression, as before.
func TestMigrateRepoConfigWritesTheGatesAsTheRegressionThatRanThem(t *testing.T) {
	in := "gates:\n  setup: bin/setup\n  lint: bin/lint   # fast\n  typecheck: \"\"\n  test: \"bin/test: all\"\n  build: |\n    bin/build\n    bin/pack\nregression:   # the sweep\n  test: \"{{REGRESSION_COMMAND}}\"\nregression_when: []\n"
	out, notes, err := MigrateRepoConfig([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := "setup: bin/setup\n\ngates:\n  lint: bin/lint   # fast\n  typecheck: \"\"\n  test: \"bin/test: all\"\n  build: |\n    bin/build\n    bin/pack\n" +
		"regression:   # the sweep\n  commands:\n    lint: bin/lint\n    test: 'bin/test: all'\n    build: |\n      bin/build\n      bin/pack\n  when: []\n"
	if string(out) != want {
		t.Fatalf("\n%s\nwant:\n%s", out, want)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "regression.commands: REVIEW: it had no command of its own, so its regression ran the gates") {
		t.Fatalf("the person is not told that the gates were written as the regression: %v", notes)
	}
	now := asMap(t, out)
	for name, command := range map[string]string{"lint": "bin/lint", "test": "bin/test: all", "build": "bin/build\nbin/pack\n"} {
		if got := digMap(now, "regression", "commands", name); got != command {
			t.Errorf("regression.commands.%s = %q, want %q", name, got, command)
		}
	}

	// A file that wrote no regression at all ran its gates too.
	out, notes, err = MigrateRepoConfig([]byte("requires: {}\ngates:\n  test: bin/test\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "required_services: {}\ngates:\n  test: bin/test\n\nregression:\n  commands:\n    test: bin/test\n"; string(out) != want {
		t.Fatalf("\n%s\nwant:\n%s", out, want)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "regression.commands: REVIEW:") {
		t.Fatalf("notes: %v", notes)
	}

	out, notes, err = MigrateRepoConfig([]byte("name: docs\ngates:\n  test: \"\"\nregression_when: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "name: docs\ngates:\n  test: \"\"\nregression:\n  commands: {}\n  when: []\n"; string(out) != want {
		t.Fatalf("\n%s\nwant:\n%s", out, want)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "regression.commands: empty, as it was: this repository has no regression") {
		t.Fatalf("notes: %v", notes)
	}
}

// TestMigrateRepoConfigLeavesWhatItCannotMove: a file written in a way the rewrite does not follow
// is left as it is, with the reason, instead of being written wrong.
func TestMigrateRepoConfigLeavesWhatItCannotMove(t *testing.T) {
	for name, in := range map[string]string{
		"gates on one line":              "gates: {setup: bin/setup, test: bin/test}\n",
		"service on one line":            "service: {dev_command: bin/dev, prepare: bin/prepare}\n",
		"not YAML":                       "gates:\n  setup: [\n",
		"a key written under both":       "requires: {}\nrequired_services:\n  db: pg_isready\n",
		"setup written under both":       "setup: bin/setup\ngates:\n  setup: bin/other\n",
		"prepare beside visual_qa":       "service:\n  prepare: bin/prepare\nvisual_qa:\n  prepare: bin/other\n",
		"when written under both":        "regression:\n  when: [\"a/**\"]\nregression_when: [\"b/**\"]\n",
		"commands of both kinds":         "regression:\n  commands:\n    full: bin/test\n  quick: bin/quick\n",
		"a today regression on one line": "regression: {commands: {full: bin/test}}\nregression_when: [\"b/**\"]\n",
	} {
		if out, _, err := MigrateRepoConfig([]byte(in)); err == nil {
			t.Errorf("%s: rewritten as\n%s", name, out)
		}
	}

	root := t.TempDir()
	dir := filepath.Join(root, "repos", "api")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RepoConfigFile), []byte("gates: {setup: bin/setup, test: bin/test}\nrequires: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := MigrateRepoConfigs(root)
	if len(m.Files) != 0 || len(m.Notes) != 1 || !strings.Contains(m.Notes[0], "REVIEW: repos/api/repo.config.yaml could not be rewritten (gates is written on one line)") ||
		!strings.Contains(m.Notes[0], "gates.setup is setup, requires is required_services") {
		t.Fatalf("a file that cannot be rewritten: files %v, notes %v", m.Files, m.Notes)
	}
}

// TestRepoFormerKeysTellsTheFileOfAnOlderKit: the five former keys are told from the keys of today,
// and a file of today holds none.
func TestRepoFormerKeysTellsTheFileOfAnOlderKit(t *testing.T) {
	names := func(raw string) string {
		var out []string
		for _, f := range RepoFormerKeys(asMap(t, []byte(raw))) {
			out = append(out, f.Was)
		}
		return strings.Join(out, " ")
	}
	for raw, want := range map[string]string{
		"gates:\n  setup: \"\"\n":                        "gates.setup",
		"service:\n  prepare: x\n":                       "service.prepare",
		"requires: {}\n":                                 "requires",
		"regression_when: []\n":                          "regression_when",
		"regression:\n  full: bin/test\n":                "regression",
		"regression:\n  commands: bin/test\n":            "regression",
		"regression:\n  commands:\n    full: bin/test\n": "",
		"regression:\n  commands: {}\n  when: []\n":      "",
		"regression: {}\n":                               "",
		"setup: x\nvisual_qa:\n  prepare: y\ngates:\n  test: z\nrequired_services: {}\n": "",
	} {
		if got := names(raw); got != want {
			t.Errorf("%q: former keys %q, want %q", raw, got, want)
		}
	}
	if got := RepoOfAnOlderKit(asMap(t, []byte("requires: {}\ngates:\n  setup: x\n"))); got != "holds keys an older foreman-kit read (gates.setup, requires): foreman-kit update rewrites it" {
		t.Errorf("RepoOfAnOlderKit = %q", got)
	}
}

// TestReadRepoConfigOfACheckoutCutBeforeTheRename: the worktree of a change that was being built
// when the keys were renamed holds the file an older kit read. Its keys are not read: the file of
// the same repository in the main checkout is, where foreman-kit update rewrote it. When that one
// is of an older kit too, nothing is read and the error says what rewrites it.
func TestReadRepoConfigOfACheckoutCutBeforeTheRename(t *testing.T) {
	main := t.TempDir()
	write := func(dir, content string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, RepoConfigFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// What makes main an umbrella, and worker a worktree of it.
	if err := os.MkdirAll(filepath.Join(main, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, ".foreman-kit", "pipeline.yaml"), []byte("repository:\n  base_branch: main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	former := "gates:\n  setup: bin/setup\n  test: bin/test\n"
	today := "setup: bin/setup\ngates:\n  test: bin/test --main\n"
	inMain := filepath.Join(main, "repos", "web")
	inWorktree := filepath.Join(main, ".worktrees", "worker-add-search", "repos", "web")
	write(inMain, today)
	write(inWorktree, former)

	raw, path, err := ReadRepoConfig("", inWorktree)
	if err != nil || string(raw) != today || path != filepath.Join(inMain, RepoConfigFile) {
		t.Fatalf("a worktree of an older kit read %q from %s (%v), want the file of the main checkout", raw, path, err)
	}
	// Told where the main checkout is, the same.
	if raw, _, err := ReadRepoConfig(main, inWorktree); err != nil || string(raw) != today {
		t.Fatalf("with the main checkout named: %q (%v)", raw, err)
	}
	// A worktree whose file is of today reads its own.
	write(inWorktree, "setup: bin/setup\ngates:\n  test: bin/test --worktree\n")
	if raw, path, err := ReadRepoConfig("", inWorktree); err != nil || !strings.Contains(string(raw), "--worktree") || path != filepath.Join(inWorktree, RepoConfigFile) {
		t.Fatalf("a worktree of today read %q from %s (%v)", raw, path, err)
	}
	// Both of an older kit: nothing is read.
	write(inWorktree, former)
	write(inMain, former)
	if _, _, err := ReadRepoConfig("", inWorktree); err == nil || !strings.Contains(err.Error(), "repo.config.yaml holds keys an older foreman-kit read (gates.setup): foreman-kit update rewrites it") {
		t.Fatalf("both files of an older kit: %v", err)
	}
	if _, _, err := ReadRepoConfig("", inMain); err == nil {
		t.Fatal("the main checkout's file of an older kit was read")
	}
	if got := RepoConfigsUnusable(main); got != "repos/web/repo.config.yaml holds keys an older foreman-kit read (gates.setup): foreman-kit update rewrites it" {
		t.Fatalf("RepoConfigsUnusable = %q", got)
	}
	if !RepoConfigsNeedRewrite(main) {
		t.Fatal("RepoConfigsNeedRewrite is false")
	}

	// update rewrites the file of the main checkout: nothing waits for it any more.
	m := MigrateRepoConfigs(main)
	if len(m.Files) != 1 || len(m.Repos) != 1 || m.Repos[0] != "repos/web" {
		t.Fatalf("MigrateRepoConfigs: files %v, repos %v, notes %v", m.Files, m.Repos, m.Notes)
	}
	write(inMain, string(m.Files["repos/web/repo.config.yaml"]))
	if got := RepoConfigsUnusable(main); got != "" {
		t.Fatalf("after the rewrite: %q", got)
	}
	if raw, _, err := ReadRepoConfig("", inWorktree); err != nil || !strings.HasPrefix(string(raw), "setup: bin/setup\n") {
		t.Fatalf("the worktree after the rewrite: %q (%v)", raw, err)
	}
}

// TestRepoProblemsNamesEachFormerKey: `foreman-kit config check` names each key an older kit read
// with the key that holds its value now, and warns about a repository that has gates and no
// regression command, since its gates no longer stand in for it.
func TestRepoProblemsNamesEachFormerKey(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "repos", "core")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(content string) FileProblems {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, RepoConfigFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return RepoProblems(root, "repos/core")
	}
	rep := write("base_branch: main\nrequires: {}\ngates:\n  setup: bin/setup\n  test: bin/test\nregression:\n  full: bin/test --all\nregression_when: []\nservice:\n  prepare: bin/prepare\n")
	for _, want := range []string{
		"gates.setup: no longer read: it is setup, because it is not a gate",
		"service.prepare: no longer read: it is visual_qa.prepare, because only the visual session runs it",
		"requires: no longer read: it is required_services",
		"regression: no longer read: it is regression.commands, because an empty regression runs nothing, instead of the gates",
		"regression_when: no longer read: it is regression.when",
	} {
		if !hasProblem(rep.Errors, want) {
			t.Errorf("config check does not say %q: %v", want, rep.Errors)
		}
	}
	if len(rep.Errors) != 5 {
		t.Errorf("errors: %v", rep.Errors)
	}

	rep = write("base_branch: main\nsetup: bin/setup\ngates:\n  test: bin/test\nregression:\n  commands: {}\n  when: []\n")
	if len(rep.Errors) != 0 || !hasProblem(rep.Warnings, "no regression command (regression.commands): the cycle's sweep runs nothing in this repository") {
		t.Errorf("a repository with gates and no regression: %+v", rep)
	}
	rep = write("base_branch: main\nsetup: bin/setup\ngates:\n  test: bin/test\nregression:\n  commands:\n    full: bin/test --all\n  when: [\"app/**\"]\nvisual_qa:\n  prepare: bin/prepare\nrequired_services:\n  db: pg_isready\n")
	if len(rep.Errors) != 0 || hasProblem(rep.Warnings, "regression") {
		t.Errorf("a whole file of today: %+v", rep)
	}
	rep = write("base_branch: main\nsetup: [a]\ngates:\n  test: bin/test\nregression: full\nvisual_qa:\n  prepare: [x]\nrequired_services: [x]\n")
	for _, want := range []string{"setup: must be a command (text)", "regression: must be a section with commands and when", "visual_qa.prepare: must be a command (text)", "required_services: must map a name to the command that checks it"} {
		if !hasProblem(rep.Errors, want) {
			t.Errorf("config check does not say %q: %v", want, rep.Errors)
		}
	}
}
