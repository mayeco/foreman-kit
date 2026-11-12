package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// fastForwardLimit is how long the commands that rewrite a checkout may take once a fast-forward has
// begun. They are not stopped with their caller (see FastForwardCheckout), and a hook that hangs must
// not hold a process that was told to stop for ever.
const fastForwardLimit = 2 * time.Minute

// ToCompletion returns a context for the commands that rewrite a working tree: it does not end when
// ctx does, because git stopped while it writes a file leaves the file cut short, and stopped between
// the files and the branch it leaves a checkout whose every pulled file counts as a local edit. It
// ends by itself after fastForwardLimit. The caller asks ctx first: one already told to stop begins
// nothing (RunGitToCompletion does both for a single command).
func ToCompletion(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), fastForwardLimit)
}

// RunGitToCompletion runs a git command that rewrites the working tree at dir (a merge, a reset, a
// checkout): a caller already told to stop begins nothing, and a command that began is not stopped
// with its caller.
func RunGitToCompletion(ctx context.Context, dir string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	apply, done := ToCompletion(ctx)
	defer done()
	return RunGit(apply, dir, args...)
}

// FastForwardResult says what FastForwardCheckout did.
type FastForwardResult struct {
	// Pulled is how many commits the checkout moved forward (0: nothing done).
	Pulled int
	// Skipped names why a checkout behind its upstream was left as it was ("" when up to date or
	// pulled): it is on another branch, has commits of its own, or has local edits.
	Skipped string
}

// FastForwardCheckout moves the checkout at dir forward to <remote>/<branch> when that is safe: it is
// on branch, has no commits of its own, and has no local edits, except in restorable files whose
// content is a version a commit it is about to pull already holds (the merger writes the backlog
// and history it published into the main checkout); those are restored first. Child repositories
// are not touched. The caller fetches first.
//
// Once it begins to rewrite the checkout it ends, even when ctx ends meanwhile: the restore and the
// merge are two commands, and a daemon told to stop between them, or while git writes a file, would
// leave the checkout with the backlog of an older commit or with half of one. A caller already told
// to stop begins nothing.
//
// restorable holds pathspecs: a file, or a pattern such as ".foreman-kit/agent_history.*.json" for the files
// a rotation of the history creates. Such a file is new to HEAD: it is untracked in the checkout until
// the fast-forward brings the commit that added it.
func FastForwardCheckout(ctx context.Context, dir, remote, branch string, restorable []string) FastForwardResult {
	upstream := remote + "/" + branch
	if _, err := RunGit(ctx, dir, "rev-parse", "--verify", "--quiet", upstream+"^{commit}"); err != nil {
		return FastForwardResult{}
	}
	behind := countRevs(ctx, dir, "HEAD.."+upstream)
	if behind <= 0 {
		return FastForwardResult{}
	}
	if cur, ok := CurrentBranch(ctx, dir); !ok || cur != branch {
		return FastForwardResult{Skipped: "not on " + branch}
	}
	if countRevs(ctx, dir, upstream+"..HEAD") > 0 {
		return FastForwardResult{Skipped: "it has commits not on " + upstream}
	}
	args := []string{"status", "--porcelain", "--ignore-submodules=all", "--", "."}
	for _, f := range restorable {
		args = append(args, ":!"+f)
	}
	if other, _ := RunGit(ctx, dir, args...); strings.TrimSpace(other) != "" {
		return FastForwardResult{Skipped: "it has local edits"}
	}
	restore, err := DirtyPaths(ctx, dir, restorable...)
	if err != nil {
		return FastForwardResult{Skipped: "its local edits cannot be listed"}
	}
	for _, f := range restore {
		if !publishedBetween(ctx, dir, f, "HEAD.."+upstream) {
			return FastForwardResult{Skipped: "it has unpublished edits in " + f}
		}
	}
	if ctx.Err() != nil {
		return FastForwardResult{}
	}
	apply, done := ToCompletion(ctx)
	defer done()
	for _, f := range restore {
		if err := RestorePublished(apply, dir, f); err != nil {
			return FastForwardResult{Skipped: "cannot restore " + f}
		}
	}
	if _, err := RunGit(apply, dir, "merge", "--ff-only", "--quiet", upstream); err != nil {
		return FastForwardResult{Skipped: "the fast-forward failed"}
	}
	return FastForwardResult{Pulled: behind}
}

// DirtyPaths returns the paths matching pathspecs that the checkout at dir holds in another state than
// HEAD: changed, deleted, or untracked.
func DirtyPaths(ctx context.Context, dir string, pathspecs ...string) ([]string, error) {
	if len(pathspecs) == 0 {
		return nil, nil
	}
	args := append([]string{"--no-optional-locks", "status", "--porcelain", "-z", "-uall", "--ignore-submodules=all", "--"}, pathspecs...)
	out, err := RunGit(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		paths = append(paths, f[3:])
		if f[0] == 'R' || f[0] == 'C' || f[1] == 'R' || f[1] == 'C' {
			i++ // the path it was renamed or copied from follows
		}
	}
	return paths, nil
}

// RestorePublished puts the file rel of the checkout at dir back as HEAD has it, for a file whose content
// a commit about to be pulled already holds. When HEAD does not have the file there is nothing git can
// restore: its untracked copy is removed, or it would stop the fast-forward that brings the file.
// A caller already told to stop restores nothing, and a restore that began is not cut short.
//
// Whether HEAD has the file is asked in a way that tells "it does not" from "git could not say": a
// git that was stopped, or failed, used to be taken for "it does not", and the backlog of the main
// checkout was removed when its daemon was told to stop at that moment.
func RestorePublished(ctx context.Context, dir, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, done := ToCompletion(ctx)
	defer done()
	inHead, err := RunGit(ctx, dir, "--literal-pathspecs", "ls-tree", "--name-only", "HEAD", "--", rel)
	if err != nil {
		return err
	}
	if strings.TrimSpace(inHead) == "" {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	_, err = RunGit(ctx, dir, "--literal-pathspecs", "checkout", "-q", "HEAD", "--", rel)
	return err
}

func countRevs(ctx context.Context, dir, rng string) int {
	out, err := RunGit(ctx, dir, "rev-list", "--count", rng)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return -1
	}
	return n
}

// publishedBetween reports whether the working copy of rel equals rel in one of the commits of rng.
func publishedBetween(ctx context.Context, dir, rel, rng string) bool {
	blobOut, err := RunGit(ctx, dir, "hash-object", "--", rel)
	if err != nil {
		return false
	}
	blob := strings.TrimSpace(blobOut)
	revs, err := RunGit(ctx, dir, "rev-list", rng, "--", rel)
	if err != nil {
		return false
	}
	for _, c := range strings.Fields(revs) {
		if got, err := RunGit(ctx, dir, "rev-parse", "-q", "--verify", c+":"+rel); err == nil && strings.TrimSpace(got) == blob {
			return true
		}
	}
	return false
}
