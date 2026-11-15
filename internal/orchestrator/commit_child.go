package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mayeco/foreman-kit/internal/gitx"
)

// commitChild commits what is dirty in the child repository at childDir and reports whether it made a
// commit. "git status --porcelain" is not empty for a child whose only change is content inside one of
// its own nested submodules (" M vendor/inner"), yet "git add -A" cannot stage that and "git commit"
// then exits 1 with "no changes added to commit": nothing is staged in that case, no commit is made
// and none is reported as rejected. A commit that fails with something staged is reported with its
// cause, as a hook's rejection only when the child has a hook that can reject it.
func (d *Dispatcher) commitChild(ctx context.Context, childDir, cp, message string) (bool, error) {
	if _, err := gitx.RunGit(ctx, childDir, "add", "-A"); err != nil {
		return false, err
	}
	staged, err := gitx.RunGit(ctx, childDir, "diff", "--cached", "--name-only", "--ignore-submodules=none")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(staged) == "" {
		return false, nil
	}
	out, err := gitx.RunGit(ctx, childDir, "commit", "--quiet", "-m", message)
	if err == nil {
		return true, nil
	}
	cause := strings.TrimSpace(out)
	if cause == "" {
		cause = err.Error()
	}
	if hooks := commitHooksOf(ctx, childDir); len(hooks) > 0 {
		return false, NewBlockedError("gate_failure", fmt.Sprintf("commit hook rejected %s (%s):\n%s", cp, strings.Join(hooks, ", "), cause))
	}
	details := fmt.Sprintf("git commit failed in %s:\n%s", cp, cause)
	if dirty := dirtyNestedSubmodules(ctx, childDir); len(dirty) > 0 {
		details += fmt.Sprintf("\nNested submodule content git cannot commit from %s: %s", cp, strings.Join(dirty, ", "))
	}
	return false, NewBlockedError("gate_failure", details)
}

// commitHooksOf returns the hooks of the repository at dir that run during "git commit" and can reject
// it (pre-commit, prepare-commit-msg and commit-msg), by the name of each that is an executable file.
func commitHooksOf(ctx context.Context, dir string) []string {
	var found []string
	for _, name := range []string{"pre-commit", "prepare-commit-msg", "commit-msg"} {
		out, err := gitx.RunGit(ctx, dir, "rev-parse", "--git-path", "hooks/"+name)
		if err != nil {
			continue
		}
		path := strings.TrimSpace(out)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			found = append(found, name)
		}
	}
	return found
}

// dirtyNestedSubmodules returns the paths of the nested submodules of the repository at dir whose
// working tree holds edits or untracked files, the dirt "git add" cannot stage in the repository.
func dirtyNestedSubmodules(ctx context.Context, dir string) []string {
	out, err := gitx.RunGit(ctx, dir, "status", "--porcelain=v2", "--ignore-submodules=none")
	if err != nil {
		return nil
	}
	var dirty []string
	for _, line := range strings.Split(out, "\n") {
		// "1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>": <sub> is "S<c><m><u>" for a submodule, each
		// flag a letter when its commit changed, its tracked content is modified or it has untracked files.
		fields := strings.SplitN(line, " ", 9)
		if len(fields) < 9 || fields[0] != "1" || !strings.HasPrefix(fields[2], "S") || len(fields[2]) < 4 {
			continue
		}
		if fields[2][2] != '.' || fields[2][3] != '.' {
			dirty = append(dirty, fields[8])
		}
	}
	return dirty
}
