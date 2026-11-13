package gitx

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeFakeGit(t *testing.T, scriptBody string) string {
	t.Helper()
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	content := "#!/bin/sh\n" + scriptBody + "\n"
	if err := os.WriteFile(gitPath, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir
}

func TestCurrentBranch(t *testing.T) {
	ctx := context.Background()

	// 1. Directory that is not a git repository
	notRepo := t.TempDir()
	if br, ok := CurrentBranch(ctx, notRepo); ok || br != "" {
		t.Fatalf("CurrentBranch on non-repo = (%q, %v), want (\"\", false)", br, ok)
	}

	// 2. Repository on a branch
	repo := t.TempDir()
	gitT(t, repo, "init", "--quiet")
	gitT(t, repo, "checkout", "--quiet", "-b", "feat/spec-portable")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "f.txt")
	gitT(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")

	if br, ok := CurrentBranch(ctx, repo); !ok || br != "feat/spec-portable" {
		t.Fatalf("CurrentBranch on branch = (%q, %v), want (\"feat/spec-portable\", true)", br, ok)
	}

	// 3. Detached HEAD
	gitT(t, repo, "checkout", "--quiet", "--detach", "HEAD")
	if br, ok := CurrentBranch(ctx, repo); ok || br != "" {
		t.Fatalf("CurrentBranch on detached HEAD = (%q, %v), want (\"\", false)", br, ok)
	}
}

func TestVersionAndCheckVersion(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name      string
		output    string
		exitCode  int
		wantVer   string
		wantOK    bool
		wantCheck bool
	}{
		{
			name:      "minimum version 2.17.0",
			output:    "git version 2.17.0",
			wantVer:   "2.17.0",
			wantOK:    true,
			wantCheck: true,
		},
		{
			name:      "below minimum 2.16.6",
			output:    "git version 2.16.6",
			wantVer:   "2.16.6",
			wantOK:    true,
			wantCheck: false,
		},
		{
			name:      "major version below minimum",
			output:    "git version 1.9.5",
			wantVer:   "1.9.5",
			wantOK:    true,
			wantCheck: false,
		},
		{
			name:      "Apple vendor suffix",
			output:    "git version 2.39.5 (Apple Git-154)",
			wantVer:   "2.39.5",
			wantOK:    true,
			wantCheck: true,
		},
		{
			name:      "Windows vendor suffix",
			output:    "git version 2.20.1.windows.1",
			wantVer:   "2.20.1",
			wantOK:    true,
			wantCheck: true,
		},
		{
			name:      "two-component version",
			output:    "git version 2.29",
			wantVer:   "2.29.0",
			wantOK:    true,
			wantCheck: true,
		},
		{
			name:      "unreadable version string",
			output:    "git version unknown-custom-build",
			wantVer:   "",
			wantOK:    false,
			wantCheck: false,
		},
		{
			name:      "empty output",
			output:    "",
			wantVer:   "",
			wantOK:    false,
			wantCheck: false,
		},
		{
			name:      "non-zero exit from git --version",
			output:    "git version 2.39.5",
			exitCode:  1,
			wantVer:   "",
			wantOK:    false,
			wantCheck: false,
		},
	}

	origPath := os.Getenv("PATH")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := "printf '%s\\n' " + shellQuote(tc.output)
			if tc.exitCode != 0 {
				script += "\nexit " + strconv.Itoa(tc.exitCode)
			}
			binDir := writeFakeGit(t, script)
			wantPath := filepath.Join(binDir, "git")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)

			ver, gotPath, err := Version(ctx)
			if gotPath != wantPath {
				t.Fatalf("Version path = %q, want %q", gotPath, wantPath)
			}
			if tc.wantOK {
				if err != nil {
					t.Fatalf("Version unexpected error: %v", err)
				}
				if ver != tc.wantVer {
					t.Fatalf("Version = %q, want %q", ver, tc.wantVer)
				}
			} else if err == nil {
				t.Fatalf("expected Version error for %q, got ver=%q", tc.output, ver)
			}

			chkErr := CheckVersion(ctx)
			if tc.wantCheck {
				if chkErr != nil {
					t.Fatalf("CheckVersion unexpected error: %v", chkErr)
				}
			} else {
				if chkErr == nil {
					t.Fatalf("expected CheckVersion to fail for %q", tc.output)
				}
				msg := chkErr.Error()
				if !strings.Contains(msg, MinVersion) || !strings.Contains(msg, wantPath) {
					t.Fatalf("CheckVersion error %q must contain minimum %q and path %q", msg, MinVersion, wantPath)
				}
				if tc.wantOK && !strings.Contains(msg, tc.wantVer) {
					t.Fatalf("CheckVersion error %q must contain found version %q", msg, tc.wantVer)
				}
			}
		})
	}
}

func TestRepairWorktreeRunsOnlyOnGit229OrLater(t *testing.T) {
	ctx := context.Background()
	origPath := os.Getenv("PATH")

	for _, tc := range []struct {
		ver      string
		wantCall bool
	}{
		{"2.20.1", false},
		{"2.28.9", false},
		{"2.29.0", true},
		{"2.39.5 (Apple Git-154)", true},
	} {
		t.Run(tc.ver, func(t *testing.T) {
			logFile := filepath.Join(t.TempDir(), "git.log")
			script := "if [ \"$1\" = \"--version\" ]; then\n" +
				"  printf 'git version %s\\n' " + shellQuote(tc.ver) + "\n" +
				"  exit 0\n" +
				"fi\n" +
				"printf '%s\\n' \"$*\" >> " + shellQuote(logFile) + "\n"
			binDir := writeFakeGit(t, script)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)

			RepairWorktree(ctx, t.TempDir(), "/tmp/wt")
			logged, _ := os.ReadFile(logFile)
			called := strings.Contains(string(logged), "worktree repair /tmp/wt")
			if called != tc.wantCall {
				t.Fatalf("version %s: worktree repair called = %v (log=%q), want %v", tc.ver, called, string(logged), tc.wantCall)
			}
		})
	}
}

func scanGoSourceForForbiddenGit(fset *token.FileSet, rel string, file *ast.File) []string {
	gitRunners := map[string]bool{
		"RunGit":             true,
		"RunGitToCompletion": true,
		"childGit":           true,
	}
	forbiddenGitArgs := map[string]bool{
		"switch":         true,
		"restore":        true,
		"--show-current": true,
	}

	var violations []string
	// initWithBranch reports an argument list that runs "git init" with -b or --initial-branch,
	// both added in git 2.28; "-b" alone stays legitimate for "git checkout -b".
	initWithBranch := func(args []ast.Expr) (ast.Expr, bool) {
		hasInit := false
		var branchFlag ast.Expr
		for _, arg := range args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			if val == "init" {
				hasInit = true
			}
			if val == "-b" || val == "--initial-branch" || strings.HasPrefix(val, "--initial-branch=") {
				branchFlag = lit
			}
		}
		return branchFlag, hasInit && branchFlag != nil
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if flag, bad := initWithBranch(node.Elts); bad {
				pos := fset.Position(flag.Pos())
				violations = append(violations, pos.String()+": argument list runs git init with an initial branch flag (git 2.28+); use gitx.SetInitialBranch")
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(node.Value)
			if err != nil {
				return true
			}
			if forbiddenGitArgs[val] || strings.Contains(val, "--show-current") {
				pos := fset.Position(node.Pos())
				violations = append(violations, pos.String()+": forbidden git argument literal "+node.Value)
			}
		case *ast.CallExpr:
			fnName := callFuncName(node.Fun)
			if !gitRunners[fnName] {
				return true
			}
			if flag, bad := initWithBranch(node.Args); bad {
				pos := fset.Position(flag.Pos())
				violations = append(violations, pos.String()+": "+fnName+" runs git init with an initial branch flag (git 2.28+); use gitx.SetInitialBranch")
			}
			for _, arg := range node.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				if forbiddenGitArgs[val] {
					pos := fset.Position(lit.Pos())
					violations = append(violations, pos.String()+": "+fnName+" passes "+lit.Value)
				}
				if val == "symbolic-ref" && rel != "internal/gitx/version.go" {
					pos := fset.Position(lit.Pos())
					violations = append(violations, pos.String()+": "+fnName+" runs symbolic-ref directly instead of gitx.CurrentBranch")
				}
			}
		}
		return true
	})
	return violations
}

func TestPortableGitCommandsGuard(t *testing.T) {
	// Verify the guard catches forbidden git runner calls while allowing denied-tool text in internal/config.
	for _, bad := range []string{
		`package p; func f() { gitx.RunGit(ctx, d, "switch", "main") }`,
		`package p; func f() { gitx.RunGit(ctx, d, "restore", "a.txt") }`,
		`package p; func f() { gitx.RunGit(ctx, d, "branch", "--show-current") }`,
		`package p; func f() { gitx.RunGit(ctx, d, "init", "-b", "main") }`,
		`package p; func f() { gitx.RunGit(ctx, d, "init", "--bare", "--initial-branch=main", p) }`,
		`package p; func f() { gitx.RunGit(ctx, d, "init", "--initial-branch", "main") }`,
		`package p; var a = []string{"init", "-b", "main"}`,
		`package p; var a = [][]string{{"init", "-b", "main"}, {"config", "k", "v"}}`,
	} {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, "internal/orchestrator/sample.go", bad, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := scanGoSourceForForbiddenGit(fset, "internal/orchestrator/sample.go", parsed); len(got) == 0 {
			t.Fatalf("expected guard to reject %s", bad)
		}
	}
	for _, ok := range []string{
		`package p; func f() { gitx.RunGit(ctx, d, "checkout", "-b", "x") }`,
		`package p; func f() { gitx.RunGit(ctx, d, "init", "--bare", p) }`,
		`package p; var a = [][]string{{"init"}, {"checkout", "-b", "x"}}`,
	} {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, "internal/orchestrator/sample.go", ok, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := scanGoSourceForForbiddenGit(fset, "internal/orchestrator/sample.go", parsed); len(got) != 0 {
			t.Fatalf("expected guard to allow %s, got %v", ok, got)
		}
	}
	allowed := `package config; const denied = "Bash(git commit *) Bash(git checkout *) Bash(git switch *)"`
	fsetAllowed := token.NewFileSet()
	parsedAllowed, err := parser.ParseFile(fsetAllowed, "internal/config/sample.go", allowed, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := scanGoSourceForForbiddenGit(fsetAllowed, "internal/config/sample.go", parsedAllowed); len(got) != 0 {
		t.Fatalf("expected guard to allow denied-tool text in internal/config, got %v", got)
	}

	moduleRoot := filepath.Clean(filepath.Join("..", ".."))
	if _, err := os.Stat(filepath.Join(moduleRoot, "go.mod")); err != nil {
		t.Fatalf("cannot locate module root at %s: %v", moduleRoot, err)
	}

	var violations []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "bin" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		violations = append(violations, scanGoSourceForForbiddenGit(fset, rel, file)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("non-portable git command usage found in non-test sources:\n  %s", strings.Join(violations, "\n  "))
	}
}

func callFuncName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	default:
		return ""
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func TestSetInitialBranchPointsHeadOfANewRepository(t *testing.T) {
	ctx := context.Background()
	for _, bare := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "repo")
		args := []string{"init"}
		if bare {
			args = append(args, "--bare")
		}
		if _, err := RunGit(ctx, t.TempDir(), append(args, dir)...); err != nil {
			t.Fatalf("git init (bare=%v): %v", bare, err)
		}
		if err := SetInitialBranch(ctx, dir, "trunk"); err != nil {
			t.Fatalf("SetInitialBranch (bare=%v): %v", bare, err)
		}
		out, err := RunGit(ctx, dir, "symbolic-ref", "HEAD")
		if err != nil {
			t.Fatalf("symbolic-ref HEAD (bare=%v): %v", bare, err)
		}
		if got := strings.TrimSpace(out); got != "refs/heads/trunk" {
			t.Fatalf("bare=%v: HEAD = %q, want refs/heads/trunk", bare, got)
		}
	}
}
