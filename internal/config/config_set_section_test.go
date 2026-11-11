package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// A pipeline.yaml that lacks a section gets it from config set, with the key, every other line of
// the file kept as it was. What the file lacked before is still its own problem, said by check: set
// only refuses a change that brings a new one.
func TestSetAddsAKeyWhoseSectionIsMissing(t *testing.T) {
	root, _ := setupTestUmbrella(t)
	pipeline := filepath.Join(root, ".foreman-kit", "pipeline.yaml")
	old := "# Pipeline settings\nrepository:\n  base_branch: master\n\ndispatch:\n  workers: 2 # parallel builds\n\ngates:\n  # how long one repository's gates may run\n  timeout: 45m\n"
	if err := os.WriteFile(pipeline, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	var setOut bytes.Buffer
	if err := Set(root, "pipeline.merge.outside_commits", "ignore", false, &setOut); err != nil {
		t.Fatalf("Set pipeline.merge.outside_commits on a pipeline.yaml without merge: failed: %v", err)
	}
	raw, err := os.ReadFile(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	if want := old + "merge:\n  outside_commits: ignore\n"; string(raw) != want {
		t.Fatalf("expected the old file unchanged and the section appended, got:\n%s", raw)
	}
	if !strings.Contains(setOut.String(), "merge.outside_commits nothing (the key was missing) -> ignore") {
		t.Fatalf("set does not say the key was missing:\n%s", setOut.String())
	}
	if got := pipelineString(root, "merge.outside_commits"); got != settings.OutsideCommitsIgnore {
		t.Fatalf("expected ignore after Set, got %q", got)
	}
	// The other keys the file lacks are still reported.
	if rep := Check(root)[settings.BaseBranch.FilePath()]; !hasLine(rep.Errors, "merge.conflict_rebuilds: missing") || hasLine(rep.Errors, "merge.outside_commits") {
		t.Fatalf("check after the set: %v", rep.Errors)
	}
	// A value the key does not take is refused, and the file stays as it was. A caller tells a
	// refusal (the request is wrong) from a failure to read or write (the machine is) by its type.
	for what, set := range map[string]func() error{
		"a value the key does not take":       func() error { return Set(root, "pipeline.dispatch.workers", "0", false, &setOut) },
		"a key an older kit read":             func() error { return Set(root, "pipeline.dispatch.max_workers", "3", false, &setOut) },
		"a key that is not one value":         func() error { return Set(root, "pipeline.repository.protected_paths", "x", false, &setOut) },
		"a key of a file that does not exist": func() error { return Set(root, "daemon.cycle.every", "30m", false, &setOut) },
	} {
		if what == "a key of a file that does not exist" {
			_ = os.Remove(filepath.Join(root, ".foreman-kit", "daemon.yaml"))
		}
		err := set()
		var refusal *Refusal
		if err == nil || !errors.As(err, &refusal) || refusal.Reason != err.Error() {
			t.Fatalf("%s: Set returned %v (%T), want a *Refusal", what, err, err)
		}
	}
	if after, _ := os.ReadFile(pipeline); string(after) != string(raw) {
		t.Fatalf("a refused set changed the file:\n%s", after)
	}
	// A file that is not valid YAML is refused too, and named.
	if err := os.WriteFile(pipeline, []byte("dispatch:\n  workers: 2\n   broken: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = Set(root, "pipeline.dispatch.workers", "3", false, &setOut)
	var refusal *Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, ".foreman-kit/pipeline.yaml is not valid YAML") {
		t.Fatalf("Set on a file that is not YAML returned %v (%T)", err, err)
	}
}

// TestSetInTextAddsTheSectionsAKeyLacks: the key and the sections above it are written after the
// last line of the file, which stays as it was, with or without a final newline.
func TestSetInTextAddsTheSectionsAKeyLacks(t *testing.T) {
	for _, body := range []string{
		"# Pipeline\nrepository:\n  base_branch: main\ndispatch:\n  workers: 2 # parallel builds\n",
		"# Pipeline\nrepository:\n  base_branch: main\ndispatch:\n  workers: 2 # parallel builds",
	} {
		got, err := setInText([]byte(body), settings.OutsideCommits.Parts(), "include")
		if err != nil {
			t.Fatal(err)
		}
		want := strings.TrimSuffix(body, "\n") + "\nmerge:\n  outside_commits: include\n"
		if string(got) != want {
			t.Errorf("got:\n%q\nwant:\n%q", got, want)
		}
	}
	// A key of a section that exists goes after the last line of that section.
	got, err := setInText([]byte("dispatch:\n  workers: 2 # parallel builds\n\ngates:\n  timeout: 45m\n"), settings.PriorityBumpEvery.Parts(), "never")
	if err != nil {
		t.Fatal(err)
	}
	if want := "dispatch:\n  workers: 2 # parallel builds\n  priority_bump_every: never\n\ngates:\n  timeout: 45m\n"; string(got) != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
	// A value replaced on its line keeps the comment that follows it.
	got, err = setInText([]byte("dispatch:\n  workers: 2 # parallel builds\n"), settings.Workers.Parts(), 4)
	if err != nil || string(got) != "dispatch:\n  workers: 4 # parallel builds\n" {
		t.Errorf("got %q, %v", got, err)
	}
	// A list and a mapping are written on the line of their key.
	got, err = setInText([]byte("roles:\n  plan:\n    agents: [claude] # in order\n    args: {}\n"), settings.RoleArgs("plan").Parts(), map[string]any{"claude": "--model a b", "codex": []any{"-m", "x"}})
	if err != nil || string(got) != "roles:\n  plan:\n    agents: [claude] # in order\n    args: {claude: \"--model a b\", codex: [-m, x]}\n" {
		t.Errorf("got %q, %v", got, err)
	}
}
