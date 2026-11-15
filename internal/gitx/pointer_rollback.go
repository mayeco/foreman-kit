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
	Lost   int    // commits of From the current gitlink does not hold (always > 0)
}

// FindPointerRollback looks at the last limit first-parent commits of ref that changed child's gitlink
// and returns the newest rollback whose earlier gitlink the current gitlink (at ref) still lacks, or
// nil. The child's objects are read from its checkout under root. Only integrated work counts: the
// earlier gitlink must be on the child's remote base branch (remote/<base_branch> of its
// repo.config.yaml), so a person who moved it back on purpose from a commit the base never took is not
// reported. What the checkout cannot prove is not a rollback: nil when it lacks the current gitlink or
// its remote base branch, and a move whose gitlinks it lacks is skipped.
func FindPointerRollback(ctx context.Context, root, ref, remote, child string, limit int) *PointerRollback {
	if limit <= 0 {
		limit = 200
	}
	if remote == "" {
		return nil
	}
	childDir := filepath.Join(root, filepath.FromSlash(child))
	curOut, err := RunGit(ctx, root, "rev-parse", "--verify", "--quiet", ref+":"+child)
	if err != nil {
		return nil
	}
	cur := strings.TrimSpace(curOut)
	if !hasCommit(ctx, childDir, cur) {
		return nil // not fetched yet: the check cannot decide
	}
	branch := "main"
	if r, err := Open(root); err == nil {
		branch = r.ChildBaseBranch(child)
	}
	base := remote + "/" + branch
	if !hasCommit(ctx, childDir, base) {
		return nil
	}
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
		if !hasCommit(ctx, childDir, from) || !hasCommit(ctx, childDir, to) {
			continue // the checkout cannot tell which way it moved
		}
		if !isAncestor(ctx, childDir, to, from) || isAncestor(ctx, childDir, from, cur) {
			continue // a forward move, or one the current gitlink has caught up with
		}
		if !isAncestor(ctx, childDir, from, base) {
			continue // never integrated on the child's base: a deliberate move back
		}
		lost := countRevs(ctx, childDir, cur+".."+from)
		if lost <= 0 {
			continue
		}
		return &PointerRollback{Child: child, Commit: commit, From: from, To: to, Lost: lost}
	}
	return nil
}

func isAncestor(ctx context.Context, dir, a, b string) bool {
	_, err := RunGit(ctx, dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// hasCommit reports whether the repository at dir holds rev as a commit.
func hasCommit(ctx context.Context, dir, rev string) bool {
	_, err := RunGit(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return err == nil
}
