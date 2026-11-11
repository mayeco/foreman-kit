package portal

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestNoCallLeavesItsErrorUnread: a call of the package that returns an error has it read. The
// statement that is only the call, and the `go` or the `defer` of one, drop the error without
// anybody having decided to; what is dropped on purpose is assigned to the blank identifier, where
// TestNoResultIsDiscardedWithoutAReason asks for the reason.
//
// It is the rule of errcheck, which needs the types of the package: the test reads them the way
// `go vet` does, from what the go command compiled of each package the portal imports.
func TestNoCallLeavesItsErrorUnread(t *testing.T) {
	pkg, err := build.Default.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range pkg.GoFiles {
		parsed, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, parsed)
	}
	found, looked := unreadErrors(t, compiledImports(t), fset, files)
	if looked == 0 {
		t.Fatal("the test looked at no call: it no longer reads the package")
	}
	for _, call := range found {
		t.Errorf("%s: handle it, or assign it to _ and say why", call)
	}
}

// TestTheRuleOfUnreadErrorsFindsWhatItIsFor: the rule names the three ways of dropping an error
// without a word, and none of the ways of reading one or of dropping it in the open.
func TestTheRuleOfUnreadErrorsFindsWhatItIsFor(t *testing.T) {
	const source = `package sample

import (
	"bytes"
	"fmt"
	"os"
)

func drops() {
	os.Remove("a")
	defer os.Remove("b")
	go os.Remove("c")
	fmt.Fprintln(os.Stderr, "d")
}

func reads() error {
	var text bytes.Buffer
	text.WriteString("e")
	fmt.Fprintf(&text, "%d", 1)
	_ = os.Remove("f")
	if err := os.Remove("g"); err != nil {
		return err
	}
	println("h")
	return os.Remove("i")
}
`
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "sample.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	found, looked := unreadErrors(t, compiledImports(t), fset, []*ast.File{parsed})
	want := []string{
		"sample.go:10 calls os.Remove and nothing reads its error",
		"sample.go:11 defers os.Remove and nothing reads its error",
		"sample.go:12 starts os.Remove and nothing reads its error",
		"sample.go:13 calls fmt.Fprintln and nothing reads its error",
	}
	if strings.Join(found, "\n") != strings.Join(want, "\n") {
		t.Errorf("the rule found\n%s\nand what drops an error in the sample is\n%s", strings.Join(found, "\n"), strings.Join(want, "\n"))
	}
	if looked != 7 {
		t.Errorf("the rule looked at %d calls of the sample, which has 7 that stand alone", looked)
	}
}

// unreadErrors reads the types of the files of one package and returns each call that returns an
// error nothing reads, with how many calls that stand alone it looked at.
func unreadErrors(t *testing.T, compiled map[string]string, fset *token.FileSet, files []*ast.File) (found []string, looked int) {
	t.Helper()
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := compiled[path]
		if !ok {
			return nil, fmt.Errorf("the go command compiled nothing for %q", path)
		}
		return os.Open(file)
	})}
	if _, err := conf.Check(files[0].Name.Name, fset, files, info); err != nil {
		t.Fatalf("the package does not type-check from the source the test read: %v", err)
	}

	errorType := types.Universe.Lookup("error").Type()
	returnsError := func(call *ast.CallExpr) bool {
		switch result := info.TypeOf(call).(type) {
		case nil:
			return false
		case *types.Tuple:
			for i := 0; i < result.Len(); i++ {
				if types.Identical(result.At(i).Type(), errorType) {
					return true
				}
			}
			return false
		default:
			return types.Identical(result, errorType)
		}
	}
	check := func(call *ast.CallExpr, how string) {
		looked++
		if !returnsError(call) || neverFails(info, call) {
			return
		}
		at := fset.Position(call.Pos())
		found = append(found, fmt.Sprintf("%s:%d %s %s and nothing reads its error", at.Filename, at.Line, how, types.ExprString(call.Fun)))
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch stmt := n.(type) {
			case *ast.ExprStmt:
				if call, ok := stmt.X.(*ast.CallExpr); ok {
					check(call, "calls")
				}
			case *ast.GoStmt:
				check(stmt.Call, "starts")
			case *ast.DeferStmt:
				check(stmt.Call, "defers")
			}
			return true
		})
	}
	return found, looked
}

// compiledImports asks the go command for what it compiled of every package the portal needs: the
// file that holds the types each one exports, by the path it is imported with.
func compiledImports(t *testing.T) map[string]string {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("go", "list", "-export", "-deps", "-f", "{{if .Export}}{{.ImportPath}}\t{{.Export}}{{end}}", ".")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -export -deps: %v\n%s", err, stderr.String())
	}
	compiled := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if path, file, ok := strings.Cut(line, "\t"); ok {
			compiled[path] = file
		}
	}
	return compiled
}

// neverFails reports whether a call writes to something whose writes always return a nil error, by
// the contract its documentation states: a bytes.Buffer, a strings.Builder, or a hash. What fmt
// prints into one of them fails only if the write does, so it does not fail either.
func neverFails(info *types.Info, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if selection, ok := info.Selections[sel]; ok {
		return strings.HasPrefix(sel.Sel.Name, "Write") && writesAlwaysSucceed(selection.Recv())
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "fmt" || !strings.HasPrefix(fn.Name(), "Fprint") || len(call.Args) == 0 {
		return false
	}
	return writesAlwaysSucceed(info.TypeOf(call.Args[0]))
}

func writesAlwaysSucceed(t types.Type) bool {
	if t == nil {
		return false
	}
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	switch named.Obj().Pkg().Path() + "." + named.Obj().Name() {
	case "bytes.Buffer", "strings.Builder", "hash.Hash":
		return true
	}
	return false
}
