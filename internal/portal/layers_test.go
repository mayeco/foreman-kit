package portal

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The portal has three layers. A handler reads a request, calls and answers; a service holds the
// logic of an area; the store reads and writes. What tells them apart can be checked: a handler does
// not open files or start processes, and a service does not know it is behind HTTP. This test reads
// the imports of every file of the package and holds each file to the layer its name puts it in.

// layerOf returns the layer of a file by its name, "" when the name puts it in none.
func layerOf(file string) string {
	switch {
	case strings.HasPrefix(file, "api_"), strings.HasPrefix(file, "page_"), file == "routes.go", file == "respond.go":
		return "handler"
	case file == "errors.go", file == "labels.go":
		return "shared"
	case strings.HasPrefix(file, "svc_"):
		return "service"
	case file == "server.go", file == "token_limiter.go":
		return "server"
	case file == "kit.go":
		return "kit"
	}
	return ""
}

// forbiddenImports lists, by layer, the packages a file of that layer must not import.
var forbiddenImports = map[string][]string{
	// A handler touches neither files nor processes nor the database: it calls a service or the store.
	"handler": {"os", "os/exec", "syscall", "path/filepath", "database/sql"},
	// A service is not behind HTTP: it takes values and returns values and errors.
	"service": {"net/http", "html/template"},
	// The server listens and checks requests; it reads its environment, and starts nothing.
	"server": {"os/exec", "syscall", "database/sql"},
	// The kit is where processes start: nothing is forbidden to it but HTTP.
	"kit": {"net/http", "html/template"},
	// What every layer shares (the problems, the words for a state) depends on none of them.
	"shared": {"net/http", "os", "os/exec", "syscall", "path/filepath", "database/sql"},
}

func TestEachFileKeepsToTheImportsOfItsLayer(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		layer := layerOf(file)
		if layer == "" {
			t.Errorf("%s is in no layer: name it api_*.go or page_*.go (a handler) or svc_*.go (a service)", file)
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, forbidden := range forbiddenImports[layer] {
				if path == forbidden {
					t.Errorf("%s is a %s file and imports %q", file, layer, path)
				}
			}
		}
	}
	if _, err := os.Stat("server.go"); err != nil {
		t.Fatalf("the test does not run in the directory of the package: %v", err)
	}
}
