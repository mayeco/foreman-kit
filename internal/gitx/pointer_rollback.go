package gitx

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
)

// PointerRollback is a child repository whose gitlink a commit of the umbrella moved back to an
// ancestor of the commit it recorded before, and that no later commit moved forward past it again: a
// person committed with a stale submodule checkout (git commit -a after a pull that did not update the
// submodule), and every change integrated in that child since the earlier gitlink vanished from the
// umbrella's view, although the child's own history still has it.
type PointerRollback struct {
	Child  string // repos/news
	Commit string // the umbrella commit that moved it back
	From   string // the gitlink before that commit
	To     string // the gitlink it set
	Lost   int    // commits of From the current gitlink does not hold
}

// FindPointerRollback looks at the last limit first-parent commits of ref that changed child's gitlink
// and returns the newest rollback whose earlier gitlink the current gitlink (at ref) still lacks, or
// nil. The child's objects are read from its checkout under root; a commit it does not have is skipped.
func FindPointerRollback(ctx context.Context, root, ref, child string, limit int) *PointerRollback {
	if limit <= 0 {
		limit = 200
	}
	childDir := filepath.Join(root, filepath.FromSlash(child))
	curOut, err := RunGit(ctx, root, "rev-parse", "--verify", "--quiet", ref+":"+child)
	if err != nil {
		return nil
	}
	cur := strings.TrimSpace(curOut)
	// --ignore-submodules=none: with "ignore = all" for the child (.gitmodules, or diff.ignoreSubmodules),
	// git log prints no gitlink line at all.
	out, err := RunGit(ctx, root, "log", "--first-parent", "-n", strconv.Itoa(limit), "--format=C %H", "--raw", "--no-abbrev", "--ignore-submodules=none", ref, "--", child)
	if err != nil {
		return nil
	}
	commit := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "C ") {
			commit = strings.TrimPrefix(line, "C ")
			continue
		}
		// ":160000 160000 <old> <new> M\trepos/news"
		if !strings.HasPrefix(line, ":160000 160000 ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		from, to := f[2], f[3]
		if from == to || from == cur {
			continue
		}
		if !isAncestor(ctx, childDir, to, from) || isAncestor(ctx, childDir, from, cur) {
			continue // a forward move, or one the current gitlink has caught up with
		}
		return &PointerRollback{Child: child, Commit: commit, From: from, To: to, Lost: countRevs(ctx, childDir, cur+".."+from)}
	}
	return nil
}

func isAncestor(ctx context.Context, dir, a, b string) bool {
	_, err := RunGit(ctx, dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}
