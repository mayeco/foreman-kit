package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// environmentUses is what the sources of the kit do with the environment: for each variable, the
// files that read it, the files that set it for what the kit runs, and the files that write its
// name as a text of its own (a list of names to read, a set of names to take out).
type environmentUses struct {
	read, handed, named map[string][]string
}

// readEnvironmentUses parses every source file of the kit that is not a test and collects how it
// uses the environment. The table of `foreman-kit config show env` itself is skipped: it names
// the variables, it does not use them.
func readEnvironmentUses(t *testing.T) environmentUses {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find the sources of the kit")
	}
	repo := filepath.Join(filepath.Dir(self), "..", "..")
	uses := environmentUses{read: map[string][]string{}, handed: map[string][]string{}, named: map[string][]string{}}
	add := func(into map[string][]string, name, file string) {
		if files := into[name]; len(files) == 0 || files[len(files)-1] != file {
			into[name] = append(into[name], file)
		}
	}
	variable := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// A text that is "NAME=" and a value with no space in it is how an entry of a child's
	// environment is written ("FOREMANKIT_SLOT=%d", "TMPDIR="+dir); with a space it is a command
	// line that starts by assigning a variable ("CGO_ENABLED=0 go vet ./...").
	entry := regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=\S*$`)
	text := func(e ast.Expr) (string, *ast.BasicLit) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", nil
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return "", nil
		}
		return s, lit
	}

	files := 0
	fset := token.NewFileSet()
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repo, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			files++
			rel, _ := filepath.Rel(repo, path)
			rel = filepath.ToSlash(rel)
			// The first argument of a call that reads or sets a variable is that use, and not a
			// name written on its own.
			used := map[*ast.BasicLit]bool{}
			ast.Inspect(parsed, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncDecl:
					if rel == "internal/config/kitconfig.go" && n.Name.Name == "environmentVariables" {
						return false
					}
				case *ast.CallExpr:
					if len(n.Args) == 0 {
						return true
					}
					name, lit := text(n.Args[0])
					if lit == nil || !variable.MatchString(name) {
						return true
					}
					callee := ""
					switch fun := n.Fun.(type) {
					case *ast.Ident:
						callee = fun.Name
					case *ast.SelectorExpr:
						callee = fun.Sel.Name
					}
					switch callee {
					case "Getenv", "LookupEnv":
						add(uses.read, name, rel)
						used[lit] = true
					case "setEnv", "setVar":
						// The two functions that build the environment of a gate and of an agent.
						add(uses.handed, name, rel)
						used[lit] = true
					case "Setenv", "Unsetenv":
						// The process changes its own environment: what it starts inherits it, and
						// the entries written for a child ("NAME=") are what says it is handed.
						used[lit] = true
					}
				case *ast.IndexExpr:
					// The environment a gate is given, read as a map.
					if id, ok := n.X.(*ast.Ident); ok && id.Name == "envMap" {
						if name, lit := text(n.Index); lit != nil && variable.MatchString(name) {
							add(uses.read, name, rel)
							used[lit] = true
						}
					}
				case *ast.BasicLit:
					if used[n] {
						return true
					}
					s, lit := text(n)
					if lit == nil {
						return true
					}
					if variable.MatchString(s) {
						add(uses.named, s, rel)
					} else if m := entry.FindStringSubmatch(s); m != nil {
						add(uses.handed, m[1], rel)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 50 {
		t.Fatalf("only %d source files were read: the sources of the kit were not found at %s", files, repo)
	}
	return uses
}

// TestEveryVariableTheKitReadsIsNamed: no environment variable changes a setting, and what the
// environment still says to the kit (the secrets, where things are on this machine, the switches
// of its own tests) and what the kit says through it to what it runs are all named by
// `foreman-kit config show env`. A variable the code reads or hands and that list does not name
// is a switch nobody can see; a line of the list the code no longer honours says what is not true;
// and a variable an older foreman-kit read is read nowhere: it is only looked for, to say that it
// is still set.
func TestEveryVariableTheKitReadsIsNamed(t *testing.T) {
	uses := readEnvironmentUses(t)

	read, handed, removed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range environmentVariables() {
		if !v.read && !v.handed && !v.removed {
			t.Errorf("the line of %s says neither that the kit reads it, nor that it hands it, nor that it takes it out", v.names)
		}
		for _, name := range strings.Split(v.names, ", ") {
			if v.read {
				read[name] = true
			}
			if v.handed {
				handed[name] = true
			}
			if v.removed {
				removed[name] = true
			}
		}
	}
	former := map[string]bool{}
	for _, v := range settings.FormerVariables() {
		former[v.Name] = true
	}
	// What the operating system, git, SSH and Go define is not the kit's to name: the kit reads it
	// as any program does, and gives git, ps and go (when it builds itself from its sources) what
	// they need to answer the same on every machine.
	system := map[string]bool{}
	for _, name := range []string{"PATH", "HOME", "USER", "TMPDIR", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR",
		"SSH_AUTH_SOCK", "GIT_CONFIG_COUNT", "GIT_SSH_COMMAND", "GIT_TERMINAL_PROMPT", "LANGUAGE", "TZ", "CGO_ENABLED"} {
		system[name] = true
	}

	sorted := func(m map[string][]string) []string {
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}
	for _, name := range sorted(uses.read) {
		switch where := strings.Join(uses.read[name], ", "); {
		case former[name]:
			t.Errorf("%s reads %s, a variable the kit no longer reads: its key is read through the settings", where, name)
		case !read[name] && !system[name]:
			t.Errorf("%s reads %s, which no line of foreman-kit config show env names as read", where, name)
		}
	}
	for _, name := range sorted(uses.handed) {
		if !handed[name] && !system[name] {
			t.Errorf("%s sets %s for what the kit runs, which no line of foreman-kit config show env names as set by the kit", strings.Join(uses.handed[name], ", "), name)
		}
	}

	// The other way: a line says what the code does.
	for _, v := range environmentVariables() {
		for _, name := range strings.Split(v.names, ", ") {
			if v.read && len(uses.read[name]) == 0 && len(uses.named[name]) == 0 {
				t.Errorf("foreman-kit config show env says the kit reads %s, and nothing in its sources does", name)
			}
			// What --set gave a run travels under the name of a constant, never written out.
			if v.handed && len(uses.handed[name]) == 0 && name != settings.RunOverridesEnv {
				t.Errorf("foreman-kit config show env says the kit sets %s for what it runs, and nothing in its sources does", name)
			}
			if v.removed && len(uses.named[name]) == 0 {
				t.Errorf("foreman-kit config show env says the kit takes %s out of the environment of what it runs, and nothing in its sources names it", name)
			}
		}
	}
}

// TestWhatIsSetNowIsWhatTheKitActsOn: `config show env` says what of the environment is set now
// for the variables the kit reads or takes out, never the value of a secret, and nothing for the
// ones it only hands, which it writes over for what it runs.
func TestWhatIsSetNowIsWhatTheKitActsOn(t *testing.T) {
	t.Setenv("FOREMANKIT_HOME", "/tmp/kit-home")
	t.Setenv("TYPESAFE_API_KEY", "a-secret-key")
	t.Setenv("GH_TOKEN", "a-secret-token")
	t.Setenv("CI", "true")
	t.Setenv("FOREMANKIT_ROLE", "build")

	// What each line says is set now, by variable: "NAME=value", or "NAME is set" for a secret.
	// Other variables of the same lines may be set where the test runs, and say nothing here.
	named, setNow := map[string]bool{}, map[string]string{}
	for _, row := range envView() {
		names, _ := row["variables"].(string)
		for _, name := range strings.Split(names, ", ") {
			named[name] = true
		}
		now, _ := row["set_now"].([]string)
		for _, entry := range now {
			name, _, _ := strings.Cut(strings.TrimSuffix(entry, " is set"), "=")
			if previous, twice := setNow[name]; twice {
				t.Errorf("%s is said set now twice: %q and %q", name, previous, entry)
			}
			setNow[name] = entry
		}
	}
	find := func(name string) string {
		if !named[name] {
			t.Fatalf("no line of config show env names %s", name)
		}
		return setNow[name]
	}
	if got := find("FOREMANKIT_HOME"); got != "FOREMANKIT_HOME=/tmp/kit-home" {
		t.Errorf("FOREMANKIT_HOME set now = %q, want its value", got)
	}
	if got := find("TYPESAFE_API_KEY"); got != "TYPESAFE_API_KEY is set" {
		t.Errorf("TYPESAFE_API_KEY set now = %q, want only that it is set", got)
	}
	if got := find("GH_TOKEN"); got != "GH_TOKEN is set" {
		t.Errorf("GH_TOKEN set now = %q, want only that it is set", got)
	}
	if got := find("CI"); got != "" {
		t.Errorf("CI set now = %q, want nothing: the kit writes over it for the gates", got)
	}
	if got := find("FOREMANKIT_ROLE"); got != "" {
		t.Errorf("FOREMANKIT_ROLE set now = %q, want nothing: the kit writes over it for each agent", got)
	}
	for name, now := range setNow {
		if strings.Contains(now, "a-secret") {
			t.Errorf("config show env prints the secret %s holds: %q", name, now)
		}
	}
}

// TestTheReadmeNamesTheSameVariables: the table of variables of the README is the list of
// `foreman-kit config show env`, name for name. A variable one of the two names and the other
// does not is a line somebody forgot.
func TestTheReadmeNamesTheSameVariables(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find the sources of the kit")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, table, found := strings.Cut(string(raw), "\n| Variable | Purpose |\n")
	if !found {
		t.Fatal("the README has no table of variables (| Variable | Purpose |)")
	}
	table, _, _ = strings.Cut(table, "\n\n")
	name := regexp.MustCompile("`([A-Z][A-Z0-9_]*)`")
	inReadme := map[string]bool{}
	for _, line := range strings.Split(table, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		for _, m := range name.FindAllStringSubmatch(cells[1], -1) {
			inReadme[m[1]] = true
		}
	}
	if len(inReadme) < 10 {
		t.Fatalf("only %d variables were read from the README's table: its shape changed", len(inReadme))
	}
	listed := map[string]bool{}
	for _, v := range environmentVariables() {
		for _, n := range strings.Split(v.names, ", ") {
			listed[n] = true
			if !inReadme[n] {
				t.Errorf("foreman-kit config show env names %s, and the README's table of variables does not", n)
			}
		}
	}
	for n := range inReadme {
		if !listed[n] {
			t.Errorf("the README's table of variables names %s, and foreman-kit config show env does not", n)
		}
	}
}
