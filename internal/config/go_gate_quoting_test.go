package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Go test gate foreman-kit setup used to write, and the fixed one.
const (
	brokenGoGate = `pkgs=$(printf "%s\n" "$FOREMANKIT_CHANGED_FILES" | sed -nE "s#^(.+)/[^/]+\.go$#./\1#p; s#^[^/]+\.go$#.#p" | sort -u); go test ${pkgs:-./...}`
	fixedGoGate  = `pkgs=$(printf '%s\n' "$FOREMANKIT_CHANGED_FILES" | sed -nE 's#^(.+)/[^/]+[.]go$#./\1#p; s#^[^/]+[.]go$#.#p' | sort -u); go test ${pkgs:-./...}`
)

func TestDoubleQuotedDollarHash(t *testing.T) {
	cases := map[string]bool{
		brokenGoGate:               true,
		fixedGoGate:                false,
		`echo "$1 and $#"`:         true,
		`echo '$#' "$HOME"`:        false,
		`echo "it\"s $# here"`:     true,
		`echo $#`:                  false,
		`go test ./... && echo ok`: false,
	}
	for cmd, want := range cases {
		if got := doubleQuotedDollarHash(cmd); got != want {
			t.Errorf("doubleQuotedDollarHash(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// TestRepoProblemsFlagsTheOldGoTestGate: applications set up before the fix keep a Go test gate whose
// sed program never runs; config check says so, and how to fix it.
func TestRepoProblemsFlagsTheOldGoTestGate(t *testing.T) {
	for gate, want := range map[string]bool{brokenGoGate: true, fixedGoGate: false} {
		root := t.TempDir()
		dir := filepath.Join(root, "repos", "news")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := "name: news\nbase_branch: main\nkind: cli\ngates:\n  test: " + yamlDoubleQuoted(gate) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "repo.config.yaml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		got := false
		for _, w := range RepoProblems(root, "repos/news").Warnings {
			got = got || strings.Contains(w, `gates.test: "$#" inside double quotes`)
		}
		if got != want {
			t.Errorf("gate %q: warned = %v, want %v (%+v)", gate, got, want, RepoProblems(root, "repos/news"))
		}
	}
}

func yamlDoubleQuoted(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
