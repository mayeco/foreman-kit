package portal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// templateCommentRe finds the comments of a template: what they say is not shown to anybody.
var templateCommentRe = regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)

// TestNoSettingIsNamedByHand: the portal names a setting and a configuration file only through the
// catalog of the kit (a constant of internal/settings, Key.Path, Key.Where, File.Path), never by its
// text. A key written by hand in a page or in a message keeps its old name when the catalog renames
// it, and nothing fails: the page then names a key that does not exist. This test reads what the
// portal shows (the text of its templates and of its scripts, and the string literals of its Go
// files) and names every place that writes a key, the path of a key inside its file, or a
// configuration file.
func TestNoSettingIsNamedByHand(t *testing.T) {
	// What is looked for: each key as `foreman-kit config set` takes it, its path inside its file
	// when the path has more than one part (a path of one word is a word of the language), and each
	// file by its path and by its name.
	needles := map[string]string{}
	for _, s := range settings.All() {
		needles[string(s.Key)] = "the setting " + string(s.Key)
		if strings.Contains(s.Key.Path(), ".") {
			needles[s.Key.Path()] = "the path of the setting " + string(s.Key)
		}
	}
	for _, f := range settings.Files() {
		needles[f.Path] = "the configuration file " + f.Path
		needles[path.Base(f.Path)] = "the configuration file " + f.Path
	}
	check := func(where, text string) {
		for needle, what := range needles {
			if strings.Contains(text, needle) {
				t.Errorf("%s writes %q, %s, by hand: take it from the catalog of settings", where, needle, what)
			}
		}
	}

	// The templates and the scripts, without their comments.
	shown, err := filepath.Glob(filepath.Join("..", "assets", "portal_ui", "templates", "*.html"))
	if err != nil || len(shown) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	scripts, err := filepath.Glob(filepath.Join("..", "assets", "portal_ui", "js", "*.js"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("no scripts: %v", err)
	}
	for _, file := range append(shown, scripts...) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		check(filepath.Base(file), templateCommentRe.ReplaceAllString(string(raw), ""))
	}

	// The string literals of the Go files of the portal that are not tests: a comment may name a
	// key, a text a person reads may not.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			check(fset.Position(lit.Pos()).String(), text)
			return true
		})
	}
}
