package proc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

func TestRegressionHitExclusions(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		files    []string
		wantHit  string
	}{
		{"positive hit", []string{"app/models/**"}, []string{"README.md", "app/models/user.rb"}, "app/models/user.rb"},
		{"no positive match", []string{"app/models/**"}, []string{"README.md"}, ""},
		{"excluded file", []string{"config/**", "!config/locales/**"}, []string{"config/locales/es.yml"}, ""},
		{"exclusion leaves another file", []string{"config/**", "!config/locales/**"}, []string{"config/locales/es.yml", "config/initializers/x.rb"}, "config/initializers/x.rb"},
		{"order does not matter", []string{"!config/locales/**", "config/**"}, []string{"config/locales/es.yml"}, ""},
		{"order does not matter, hit", []string{"!config/locales/**", "config/**"}, []string{"config/initializers/x.rb"}, "config/initializers/x.rb"},
		{"double star in the middle", []string{"app/**/models/*.rb", "!app/**/legacy/**"}, []string{"app/legacy/v1/models/old.rb"}, ""},
		{"double star in the middle, hit", []string{"app/**/models/*.rb", "!app/**/legacy/**"}, []string{"app/admin/models/user.rb"}, "app/admin/models/user.rb"},
		{"braces", []string{"db/{migrate,schema}/**", "!db/migrate/*.txt"}, []string{"db/migrate/notes.txt"}, ""},
		{"braces, hit", []string{"db/{migrate,schema}/**", "!db/migrate/*.txt"}, []string{"db/schema/structure.sql"}, "db/schema/structure.sql"},
		{"exclusion with braces", []string{"config/**", "!config/{locales,i18n}/**"}, []string{"config/i18n/fr.yml"}, ""},
		{"only exclusions", []string{"!config/locales/**", "!docs/**"}, []string{"app/models/user.rb", "config/initializers/x.rb"}, ""},
		{"invalid entries ignored", []string{"!", "!!config/**", "app/**"}, []string{"config/x.rb"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, ok := RegressionHit(tc.patterns, tc.files)
			if hit != tc.wantHit || ok != (tc.wantHit != "") {
				t.Fatalf("RegressionHit(%q, %q) = (%q, %v), want %q", tc.patterns, tc.files, hit, ok, tc.wantHit)
			}
		})
	}
}

func TestMatchRegressionWhenExcludedNote(t *testing.T) {
	m := MatchRegressionWhen([]string{"config/**", "!config/locales/**"}, []string{"README.md", "config/locales/es.yml"})
	if m.Triggered() {
		t.Fatalf("expected no trigger, got %+v", m)
	}
	want := "---- [info] core: config/locales/es.yml matches regression.when but !config/locales/** excludes it"
	if got := m.ExcludedNote("core"); got != want {
		t.Fatalf("ExcludedNote = %q, want %q", got, want)
	}
	// A file that triggers the regression silences the note.
	m = MatchRegressionWhen([]string{"config/**", "!config/locales/**"}, []string{"config/locales/es.yml", "config/routes.rb"})
	if !m.Triggered() || m.ExcludedNote("core") != "" {
		t.Fatalf("expected a hit and no note, got %+v note=%q", m, m.ExcludedNote("core"))
	}
	// No exclusion applied: no note.
	if note := MatchRegressionWhen([]string{"app/**"}, []string{"README.md"}).ExcludedNote("core"); note != "" {
		t.Fatalf("expected no note, got %q", note)
	}
}

// foreman-kit gates applies regression.when exclusions: an excluded file runs only the gates and
// says why, a file outside the exclusion runs the regression.
func TestRunGatesRegressionWhenExclusions(t *testing.T) {
	root, st := setupProcTestWorkspace(t)
	ctx := context.Background()

	dir := filepath.Join(root, "repos", "backend")
	if err := os.MkdirAll(filepath.Join(dir, "config", "locales"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
	} {
		if _, err := gitx.RunGit(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	repoCfg := `base_branch: main
gates:
  test: "echo test_ok"
regression:
  commands:
    full: "echo regression_full_ran"
  when:
    - "config/**"
    - "!config/locales/**"
`
	_ = os.WriteFile(filepath.Join(dir, "repo.config.yaml"), []byte(repoCfg), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "config", "locales", "es.yml"), []byte("es: {}\n"), 0o644)
	_, _ = gitx.RunGit(ctx, dir, "add", "-A")
	_, _ = gitx.RunGit(ctx, dir, "commit", "-m", "init backend")
	_ = os.WriteFile(filepath.Join(root, ".gitmodules"), []byte("[submodule \"backend\"]\n\tpath = repos/backend\n\turl = ./repos/backend\n\tbranch = main\n"), 0o644)
	_, _ = gitx.RunGit(ctx, root, "add", ".gitmodules")
	_, _ = gitx.RunGit(ctx, root, "commit", "-m", "add gitmodules")

	run := func() (string, string) {
		t.Helper()
		var outBuf, errBuf bytes.Buffer
		res, err := RunGates(ctx, GatesOptions{Root: root, Repos: []string{"backend"}, Slot: 2, Store: st, Stdout: &outBuf, Stderr: &errBuf})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("RunGates failed: res=%+v err=%v\nout:\n%s\nerr:\n%s", res, err, outBuf.String(), errBuf.String())
		}
		return outBuf.String(), errBuf.String()
	}

	// A new translation (the child has no gitlink here, so its untracked files are what changed): excluded.
	_ = os.WriteFile(filepath.Join(dir, "config", "locales", "fr.yml"), []byte("fr: {salut: ça va}\n"), 0o644)
	out, errOut := run()
	if strings.Contains(out, "regression_full_ran") {
		t.Fatalf("an excluded file ran the regression:\n%s", out)
	}
	if !strings.Contains(errOut, "---- [info] backend: config/locales/fr.yml matches regression.when but !config/locales/** excludes it") {
		t.Fatalf("expected the exclusion note, got:\n%s", errOut)
	}

	// An initializer: triggers the regression.
	_ = os.MkdirAll(filepath.Join(dir, "config", "initializers"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "config", "initializers", "x.rb"), []byte("X = 1\n"), 0o644)
	out, errOut = run()
	if !strings.Contains(out, "regression_full_ran") || !strings.Contains(errOut, "config/initializers/x.rb matches regression.when") {
		t.Fatalf("expected the regression to run for config/initializers/x.rb:\nout:\n%s\nerr:\n%s", out, errOut)
	}
	if strings.Contains(errOut, "excludes it") {
		t.Fatalf("no exclusion note expected when another file triggers the regression:\n%s", errOut)
	}
}
