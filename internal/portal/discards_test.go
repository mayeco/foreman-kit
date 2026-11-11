package portal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoResultIsDiscardedWithoutAReason: where the portal throws a result away on purpose, by
// assigning it to the blank identifier, the line says why, on it or just above. An error that is
// dropped without a word is one nobody decided to drop.
//
// It reads the source: it finds what is discarded in the open. A call whose error nobody assigned
// at all is found by TestNoCallLeavesItsErrorUnread, which reads the types of the package.
func TestNoResultIsDiscardedWithoutAReason(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		// The lines a comment ends on, and the lines a comment is on.
		ends, on := map[int]bool{}, map[int]bool{}
		for _, group := range parsed.Comments {
			ends[fset.Position(group.End()).Line] = true
			for line := fset.Position(group.Pos()).Line; line <= fset.Position(group.End()).Line; line++ {
				on[line] = true
			}
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) == 0 {
				return true
			}
			for _, lhs := range assign.Lhs {
				if id, ok := lhs.(*ast.Ident); !ok || id.Name != "_" {
					return true
				}
			}
			checked++
			line := fset.Position(assign.Pos()).Line
			if !on[line] && !ends[line-1] {
				t.Errorf("%s:%d discards a result and does not say why: say it on the line or just above", file, line)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("the test found nothing discarded: it no longer reads the package")
	}
}
