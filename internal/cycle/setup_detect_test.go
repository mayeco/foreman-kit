package cycle

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestGoFastTestCmdSelectsTheChangedPackages runs the Go test gate with "go test" replaced
// by echo: in double quotes the shell expanded the "$#" of ".go$#" to 0, sed failed and the
// gate tested every package on every change without anyone noticing.
func TestGoFastTestCmdSelectsTheChangedPackages(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	for _, d := range []string{"internal/news", "internal/auth", "cmd/server"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	probe := strings.Replace(GoFastTestCmd, "go test ${pkgs:-./...}", `printf '%s\n' ${pkgs:-./...}`, 1)
	if probe == GoFastTestCmd {
		t.Fatal("GoFastTestCmd no longer ends with go test ${pkgs:-./...}")
	}
	run := func(changed string) []string {
		t.Helper()
		cmd := exec.Command("sh", "-c", probe)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "FOREMANKIT_CHANGED_FILES="+changed)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gate failed: %v\n%s", err, out)
		}
		if strings.Contains(string(out), "sed:") {
			t.Fatalf("sed failed: %s", out)
		}
		got := strings.Fields(string(out))
		sort.Strings(got)
		return got
	}
	cases := []struct {
		changed string
		want    string
	}{
		{"internal/news/store.go\nREADME.md", "./internal/news"},
		{"internal/news/store.go\ninternal/auth/auth_test.go\nmain.go", ". ./internal/auth ./internal/news"},
		{"internal/gone/old.go", "./..."},
		{"data/articles.json", "./..."},
		{"internal/news/store.go\ngo.mod", "./..."},
		{"", "./..."},
	}
	for _, c := range cases {
		if got := strings.Join(run(c.changed), " "); got != c.want {
			t.Errorf("changed %q: packages %q, want %q", c.changed, got, c.want)
		}
	}
}

func TestDetectGoNetHTTPPortFromAnEnvironmentDefault(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod": "module example.com/news\n\ngo 1.24\n",
		"cmd/server/main.go": `package main
import ("net/http"; "os")
func main() {
	http.HandleFunc("/up", func(http.ResponseWriter, *http.Request) {})
	http.ListenAndServe(":"+envOr("PORT", "4000"), nil)
}
func envOr(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }
`,
	})
	found := DetectRepoStack(dir)
	if found.Kind != "web" || found.Stack != "go-web" {
		t.Fatalf("kind/stack = %s/%s, want web/go-web", found.Kind, found.Stack)
	}
	if found.Port != "4000" || found.HealthEndpoint != "http://localhost:4000/up" {
		t.Fatalf("port/health = %q/%q, want 4000 and http://localhost:4000/up", found.Port, found.HealthEndpoint)
	}
}

func TestDetectGoKeepsALiteralListenPort(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod":  "module example.com/a\n\ngo 1.24\n",
		"main.go": "package main\nimport \"net/http\"\nfunc main() { http.ListenAndServe(\":9090\", nil) }\n",
	})
	if got := DetectRepoStack(dir).Port; got != "9090" {
		t.Fatalf("port = %q, want 9090", got)
	}
}

// TestDetectDependencyFreeNodeServer: a Node server on node:http with no framework is a web
// application, with its port and health endpoint read from the code.
func TestDetectDependencyFreeNodeServer(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"package.json":            `{"name":"site","type":"module","scripts":{"start":"node server.js","lint":"node scripts/lint.js","test":"node --test test/"}}`,
		"server.js":               "import { createServer } from \"node:http\";\nconst port = Number(process.env.PORT || 3000);\ncreateServer(handler).listen(port);\n",
		"lib/app.js":              "export function handler(req, res) { if (req.url === '/up') { res.end('ok') } }\n",
		"node_modules/x/index.js": "createServer(); app.listen(9999); '/healthz'\n",
	})
	found := DetectRepoStack(dir)
	if found.Kind != "web" {
		t.Fatalf("kind = %q, want web", found.Kind)
	}
	if found.Port != "3000" || found.HealthEndpoint != "http://localhost:3000/up" {
		t.Fatalf("port/health = %q/%q, want 3000 and http://localhost:3000/up", found.Port, found.HealthEndpoint)
	}
	if found.DevCommand != "npm run start" {
		t.Fatalf("dev command = %q", found.DevCommand)
	}
}

func TestDetectNodeLibraryStaysUndecided(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"package.json": `{"name":"lib","scripts":{"test":"node --test"}}`,
		"index.js":     "export const add = (a, b) => a + b;\n",
	})
	if found := DetectRepoStack(dir); found.Kind != "" || found.Port != "" {
		t.Fatalf("kind/port = %q/%q, want both undetected", found.Kind, found.Port)
	}
}

// TestRenderRepoConfigKeepsShellOperatorsReadable: the gates are reviewed by a person, so
// & < > stay as they are instead of JSON's HTML escapes (&), and the YAML stays valid.
func TestRenderRepoConfigKeepsShellOperatorsReadable(t *testing.T) {
	found := RepoDetection{
		Stack: "go-web", Kind: "web", Port: "4000", HealthEndpoint: "http://localhost:4000/up",
		Gates: map[string]string{"lint": `test -z "$(gofmt -l .)" && go vet ./...`, "test": GoFastTestCmd, "build": "go build ./... >/dev/null"},
		Setup: "make db < seed.sql",
	}
	text, err := RenderRepoConfig("", "news", found, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, esc := range []string{"u0026", "u003c", "u003e"} {
		if strings.Contains(text, `\`+esc) {
			t.Fatalf("rendered config has the HTML escape %s:\n%s", esc, text)
		}
	}
	var cfg struct {
		Setup string            `yaml:"setup"`
		Gates map[string]string `yaml:"gates"`
	}
	if err := yaml.Unmarshal([]byte(text), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Gates["lint"] != found.Gates["lint"] || cfg.Gates["test"] != GoFastTestCmd || cfg.Setup != found.Setup {
		t.Fatalf("setup and gates did not round-trip: setup %q, gates %#v", cfg.Setup, cfg.Gates)
	}
	if _, isGate := cfg.Gates["setup"]; isGate {
		t.Fatalf("setup was rendered as a gate: %#v", cfg.Gates)
	}
}
