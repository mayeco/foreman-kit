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

func TestStripGoComments(t *testing.T) {
	src := "a := \"http://x\" // drop me\nb := `raw // kept`\nc := '/' /* gone\nstill gone */ d\n"
	got := stripGoComments(src)
	for _, keep := range []string{`"http://x"`, "`raw // kept`", `'/'`, " d\n"} {
		if !strings.Contains(got, keep) {
			t.Errorf("lost %q in %q", keep, got)
		}
	}
	for _, drop := range []string{"drop me", "gone"} {
		if strings.Contains(got, drop) {
			t.Errorf("kept comment %q in %q", drop, got)
		}
	}
	if strings.Count(got, "\n") != strings.Count(src, "\n") {
		t.Errorf("newlines changed: %q", got)
	}
}

// goWebRepo writes a net/http Go repository whose main.go holds body.
func goWebRepo(t *testing.T, files map[string]string) RepoDetection {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/a\n\ngo 1.24\n"
	writeTree(t, dir, files)
	return DetectRepoStack(dir)
}

func TestDetectGoPortFromAFlag(t *testing.T) {
	cases := []struct {
		name, main, want string
	}{
		{"cobra IntVar beats a listen literal", `package main
import "net/http"
func main() {
	cmd.Flags().IntVar(&port, "port", 4567, "portal port")
	http.ListenAndServe(":9090", nil)
}`, "4567"},
		{"cobra shorthand", `package main
import "net/http"
func main() { cmd.Flags().IntP("port", "p", 8081, "port"); _ = http.Server{} }`, "8081"},
		{"flag package beats an env default", `package main
import ("flag"; "net/http")
func main() {
	flag.Int("port", 8082, "port")
	http.ListenAndServe(":"+envOr("PORT", "4000"), nil)
}`, "8082"},
		{"named constant", `package main
import ("flag"; "net/http")
const defaultPort = 7070
func main() { flag.IntVar(&port, "port", defaultPort, "port"); _ = &http.Server{} }`, "7070"},
		{"grouped constant", `package main
import ("flag"; "net/http")
const (
	name = "x"
	defaultPort int = 7171
)
func main() { flag.IntVar(&port, "port", defaultPort, "port"); _ = &http.Server{} }`, "7171"},
		{"zero default documented in the usage", `package main
import "net/http"
func main() {
	rootCmd.Flags().IntVar(&rootPort, "port", 0, "TCP port (default 4567)")
	_ = http.Server{}
}`, "4567"},
		{"string flag", `package main
import "net/http"
func main() { flag.String("port", "8083", "port"); _ = http.Server{} }`, "8083"},
		{"example in a comment", `package main
import "net/http"
// envOr("PORT", "4000")
func main() { _ = http.Server{} }`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			found := goWebRepo(t, map[string]string{"main.go": c.main})
			if found.Kind != "web" {
				t.Fatalf("kind = %q, want web", found.Kind)
			}
			if found.Port != c.want {
				t.Fatalf("port = %q, want %q", found.Port, c.want)
			}
			if c.want == "" && found.HealthEndpoint != "" {
				t.Fatalf("health = %q with no port", found.HealthEndpoint)
			}
		})
	}
}

func TestDetectGoConstantFromTheFlagsPackageOnly(t *testing.T) {
	found := goWebRepo(t, map[string]string{
		"cmd/srv/main.go":      "package main\nimport \"net/http\"\nfunc main() { flag.IntVar(&p, \"port\", defaultPort, \"port\"); _ = http.Server{} }\n",
		"cmd/srv/defaults.go":  "package main\nconst defaultPort = 6060\n",
		"internal/x/consts.go": "package x\nconst defaultPort = 1111\n",
	})
	if found.Port != "6060" {
		t.Fatalf("port = %q, want 6060", found.Port)
	}
}

func TestDetectGoHealthOnlyFromARoute(t *testing.T) {
	cases := []struct {
		name, main, want string
	}{
		{"comment only", `package main
import "net/http"
// mux.HandleFunc("/healthz", h)
func main() { http.ListenAndServe(":9090", nil) }`, ""},
		{"string value only", `package main
import "net/http"
func main() { probe := "/health"; _ = probe; http.ListenAndServe(":9090", nil) }`, ""},
		{"method-prefixed route", `package main
import "net/http"
func main() {
	cmd.Flags().IntVar(&port, "port", 4567, "port")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h)
	_ = http.Server{}
}`, "http://localhost:4567/healthz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := goWebRepo(t, map[string]string{"main.go": c.main}).HealthEndpoint; got != c.want {
				t.Fatalf("health = %q, want %q", got, c.want)
			}
		})
	}
}

// TestDetectGoWritesNoGuessedPort: a Go web repository whose port is not in the code gets an
// empty port and health endpoint in the config add-repo writes, not 8080.
func TestDetectGoWritesNoGuessedPort(t *testing.T) {
	found := goWebRepo(t, map[string]string{
		"main.go": "package main\nimport \"net/http\"\nfunc main() { s := &http.Server{}; _ = s }\n",
	})
	if found.Kind != "web" || found.Port != "" || found.HealthEndpoint != "" {
		t.Fatalf("kind/port/health = %q/%q/%q, want web and empty", found.Kind, found.Port, found.HealthEndpoint)
	}
	text, err := RenderRepoConfig("", "svc", found, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		// The template's section for the dev server (named "dev" in the specification).
		Service map[string]any `yaml:"service"`
	}
	if err := yaml.Unmarshal([]byte(text), &cfg); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, text)
	}
	for _, k := range []string{"port", "health_endpoint"} {
		v, ok := cfg.Service[k]
		if !ok || v != "" {
			t.Fatalf("service.%s = %#v (present %v), want \"\"\n%s", k, v, ok, text)
		}
	}
}

// TestDetectGoOnTheForemanRepository: the kit's own repository serves its portal on 4567
// with the probe /healthz, and detection must say so.
func TestDetectGoOnTheForemanRepository(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	found := DetectRepoStack(root)
	if found.Port != "4567" || found.HealthEndpoint != "http://localhost:4567/healthz" {
		t.Fatalf("port/health = %q/%q, want 4567 and http://localhost:4567/healthz", found.Port, found.HealthEndpoint)
	}
}
