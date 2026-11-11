package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// Fetch fetches from remoteName using go-git in memory (with native SSH resolution),
// falling back to system git when ProxyJump/ProxyCommand/GIT_SSH_COMMAND or local worktree transport is used.
func (r *Repo) Fetch(ctx context.Context, remoteName string, refSpecs ...string) error {
	if remoteName == "" {
		remoteName = "origin"
	}
	rem, err := r.repo.Remote(remoteName)
	if err == nil && len(rem.Config().URLs) > 0 {
		remoteURL := rem.Config().URLs[0]
		auth, hostCfg, authErr := BuildSSHAuth(remoteURL)
		if authErr == nil && !NeedsSystemSSH(hostCfg) && !isLocalPathRemote(remoteURL) {
			opts := &git.FetchOptions{
				RemoteName: remoteName,
				Auth:       auth,
				Tags:       git.AllTags,
			}
			for _, rs := range refSpecs {
				if rs != "" {
					opts.RefSpecs = append(opts.RefSpecs, config.RefSpec(rs))
				}
			}
			if hostCfg != nil && hostCfg.HostName != hostCfg.Alias {
				rewritten := rewriteRemoteHost(remoteURL, hostCfg)
				// Carry the configured fetch refspecs: a Remote without them fetches nothing for a
				// refspec-less call and still reports NoErrAlreadyUpToDate.
				fetchSpecs := rem.Config().Fetch
				if len(fetchSpecs) == 0 {
					fetchSpecs = []config.RefSpec{config.RefSpec(fmt.Sprintf(config.DefaultFetchRefSpec, remoteName))}
				}
				tmpRem := git.NewRemote(r.repo.Storer, &config.RemoteConfig{
					Name:  remoteName,
					URLs:  []string{rewritten},
					Fetch: fetchSpecs,
				})
				err = tmpRem.FetchContext(ctx, opts)
			} else {
				err = r.repo.FetchContext(ctx, opts)
			}
			if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
				return nil
			}
		}
	}
	args := []string{"fetch", "--prune", "--quiet", remoteName}
	args = append(args, refSpecs...)
	_, err = RunGit(ctx, r.dir, args...)
	return err
}

// Push pushes refSpecs to remoteName using go-git with native SSH resolution, falling back to system git when needed.
// Sources are resolved to commit hashes before go-git sees them: go-git silently skips a symbolic source such as an
// attached HEAD (returning NoErrAlreadyUpToDate, which used to be reported as a successful push) and does not
// implement git's bare "HEAD"/"<branch>" shorthand. Any refspec it cannot translate goes to the git CLI instead.
// go-git's "already up to date" is not trusted either: it also answers that, having sent nothing, when it
// cannot read the source commit (one git packed after this Repo loaded its pack index, as the auto gc
// behind a commit does). The git CLI then pushes the commit, or confirms there was nothing to push.
func (r *Repo) Push(ctx context.Context, remoteName string, refSpecs ...string) error {
	if remoteName == "" {
		remoteName = "origin"
	}
	rem, err := r.repo.Remote(remoteName)
	if err == nil && len(rem.Config().URLs) > 0 {
		remoteURL := rem.Config().URLs[0]
		auth, hostCfg, authErr := BuildSSHAuth(remoteURL)
		if authErr == nil && !NeedsSystemSSH(hostCfg) && !isLocalPathRemote(remoteURL) {
			if specs, specErr := r.goGitPushRefSpecs(refSpecs); specErr == nil && len(specs) > 0 {
				opts := &git.PushOptions{
					RemoteName: remoteName,
					Auth:       auth,
					RefSpecs:   specs,
				}
				if hostCfg != nil && hostCfg.HostName != hostCfg.Alias {
					rewritten := rewriteRemoteHost(remoteURL, hostCfg)
					tmpRem := git.NewRemote(r.repo.Storer, &config.RemoteConfig{
						Name: remoteName,
						URLs: []string{rewritten},
					})
					err = tmpRem.PushContext(ctx, opts)
				} else {
					err = r.repo.PushContext(ctx, opts)
				}
				if err == nil {
					return nil
				}
			}
		}
	}
	args := []string{"push", remoteName}
	args = append(args, refSpecs...)
	_, err = RunGit(ctx, r.dir, args...)
	return err
}

// goGitPushRefSpecs converts git CLI push refspecs ("HEAD:refs/heads/main", "HEAD", "feature", "+src:dst",
// ":dst") into explicit "<commit>:refs/heads/<dst>" refspecs that go-git pushes faithfully, fast-forward
// checks included.
func (r *Repo) goGitPushRefSpecs(refSpecs []string) ([]config.RefSpec, error) {
	var out []config.RefSpec
	for _, raw := range refSpecs {
		rs := strings.TrimSpace(raw)
		if rs == "" {
			continue
		}
		force := strings.HasPrefix(rs, "+")
		rs = strings.TrimPrefix(rs, "+")
		src, dst, hasDst := strings.Cut(rs, ":")
		if !hasDst {
			// "git push <remote> <name>" updates the same name on the remote; "HEAD" means the current branch.
			name := src
			if name == "HEAD" {
				head, err := r.repo.Reference(plumbing.HEAD, false)
				if err != nil {
					return nil, err
				}
				if head.Type() != plumbing.SymbolicReference {
					return nil, fmt.Errorf("gitx: cannot push HEAD without a destination from a detached HEAD in %s", r.dir)
				}
				name = head.Target().String()
			}
			src, dst = name, name
		}
		if src == "" {
			out = append(out, config.RefSpec(":"+qualifyBranchRef(dst)))
			continue
		}
		h, err := r.resolveHash(src)
		if err != nil {
			return nil, err
		}
		spec := h.String() + ":" + qualifyBranchRef(dst)
		if force {
			spec = "+" + spec
		}
		out = append(out, config.RefSpec(spec))
	}
	return out, nil
}

// qualifyBranchRef returns name as a full reference name, treating a short name as a branch.
func qualifyBranchRef(name string) string {
	if strings.HasPrefix(name, "refs/") {
		return name
	}
	return plumbing.NewBranchReferenceName(name).String()
}

// AddWorktree creates or resets a worktree at wtPath on branch starting at startPoint. It forgets a
// registration left for wtPath whose directory is gone, and only that one. It never runs "git worktree
// prune": that deletes the administrative directory (index, HEAD, the git directories of its child
// repositories and the commits only they hold) of every worktree whose path cannot be reached right now,
// which is all of them after the umbrella was moved or mounted at another path.
func (r *Repo) AddWorktree(ctx context.Context, wtPath, branch, startPoint string, detach bool) error {
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(wtPath); os.IsNotExist(err) {
		// Still registered, it would keep its branch checked out: -B refuses to move such a branch.
		_, _ = RunGit(ctx, r.dir, "worktree", "remove", "--force", wtPath)
	}
	if detach {
		if _, err := os.Stat(filepath.Join(wtPath, ".git")); err == nil {
			_, err := RunGit(ctx, wtPath, "checkout", "--detach", "--force", startPoint)
			if err == nil {
				_, _ = RunGit(ctx, wtPath, "clean", "-fd")
				return nil
			}
			_ = r.RemoveWorktree(ctx, wtPath)
		}
		_, err := RunGit(ctx, r.dir, "worktree", "add", "--detach", "--force", wtPath, startPoint)
		return err
	}

	if _, err := os.Stat(filepath.Join(wtPath, ".git")); err == nil {
		_, err := RunGit(ctx, wtPath, "checkout", "-B", branch, startPoint)
		return err
	}
	_, err := RunGit(ctx, r.dir, "worktree", "add", "--force", "-B", branch, wtPath, startPoint)
	return err
}

// RemoveWorktree removes the worktree at wtPath and its registration, and no other worktree's (see
// AddWorktree).
func (r *Repo) RemoveWorktree(ctx context.Context, wtPath string) error {
	_, err := RunGit(ctx, r.dir, "worktree", "remove", "--force", wtPath)
	_ = os.RemoveAll(wtPath)
	if err != nil {
		// git refuses a worktree whose .git it cannot read; once the directory is gone it drops the
		// registration without reading it.
		_, _ = RunGit(ctx, r.dir, "worktree", "remove", "--force", wtPath)
	}
	return nil
}

// IsWorktreeRoot reports whether dir is the top level of its own usable working tree. It is false for a
// stale gitfile (for example after the umbrella was moved or remounted) and for a directory without .git,
// in which git would silently operate on the enclosing repository instead.
func IsWorktreeRoot(ctx context.Context, dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	out, err := RunGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	// The same directory, not the same spelling: on a case-insensitive file system dir may be written
	// in another case than git reports the top level with.
	top, err1 := os.Stat(strings.TrimSuffix(out, "\n"))
	want, err2 := os.Stat(dir)
	return err1 == nil && err2 == nil && os.SameFile(top, want)
}

// AbortInterruptedRebase aborts a rebase left in progress in dir (for example when git was killed
// mid-rebase) and reports whether one was found.
func AbortInterruptedRebase(ctx context.Context, dir string) bool {
	found := false
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		out, err := RunGit(ctx, dir, "rev-parse", "--git-path", name)
		if err != nil {
			continue
		}
		p := strings.TrimSpace(out)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := os.Stat(p); err == nil {
			found = true
		}
	}
	if found {
		_, _ = RunGit(ctx, dir, "rebase", "--abort")
	}
	return found
}

// RebaseResult holds the outcome of a branch rebase.
type RebaseResult struct {
	OK              bool
	ContentConflict bool
	Output          string
}

// rebaseCleanupTimeout bounds the "rebase --abort" that RebaseBranch runs after a failed rebase.
const rebaseCleanupTimeout = 2 * time.Minute

// RebaseBranch rebases the current branch in dir onto ontoRef, aborting cleanly if conflicts occur.
// A conflict is recognised from unmerged index entries as well as from git's messages (which a
// translated git would not match), and the abort runs on a context detached from ctx, so a
// cancellation mid-rebase never leaves rebase-merge/ behind.
func RebaseBranch(ctx context.Context, dir, ontoRef string) RebaseResult {
	out, err := RunGit(ctx, dir, "rebase", ontoRef)
	if err == nil {
		return RebaseResult{OK: true, Output: out}
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebaseCleanupTimeout)
	defer cancel()
	if ctx.Err() != nil {
		_, _ = RunGit(cleanupCtx, dir, "rebase", "--abort")
		return RebaseResult{OK: false, Output: out}
	}
	conflict := strings.Contains(out, "CONFLICT (") || strings.Contains(out, "error: could not apply")
	if !conflict {
		if unmerged, uErr := RunGit(cleanupCtx, dir, "diff", "--name-only", "--diff-filter=U"); uErr == nil && strings.TrimSpace(unmerged) != "" {
			conflict = true
		}
	}
	_, _ = RunGit(cleanupCtx, dir, "rebase", "--abort")
	return RebaseResult{
		OK:              false,
		ContentConflict: conflict,
		Output:          out,
	}
}

// StatusPorcelain returns git status --porcelain output for dir without acquiring optional index.lock.
func StatusPorcelain(ctx context.Context, dir string, untracked bool) (string, error) {
	uFlag := "-uno"
	if untracked {
		uFlag = "-uall"
	}
	return RunGit(ctx, dir, "--no-optional-locks", "status", "--porcelain", uFlag)
}

// ChangedPaths returns, unquoted and de-duplicated, the paths `git diff --name-only -z <sinceRev>` reports
// (when sinceRev is non-empty) plus every path `git status --porcelain -z -uall` reports, including both
// sides of renames and copies. Without -z, git C-quotes paths with spaces or non-ASCII bytes; without
// --no-renames, git diff names only the destination of a rename committed since sinceRev; without
// --ignore-submodules=none, neither command lists a child repository whose gitlink moved when
// .gitmodules says "ignore = all" for it (or diff.ignoreSubmodules does).
func ChangedPaths(ctx context.Context, dir, sinceRev string) ([]string, error) {
	seen := make(map[string]bool)
	var paths []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if sinceRev != "" {
		out, err := RunGit(ctx, dir, "diff", "--name-only", "-z", "--no-renames", "--ignore-submodules=none", sinceRev, "--")
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(out, "\x00") {
			add(p)
		}
	}
	out, err := RunGit(ctx, dir, "--no-optional-locks", "status", "--porcelain", "-z", "-uall", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		add(f[3:])
		if f[0] == 'R' || f[0] == 'C' || f[1] == 'R' || f[1] == 'C' {
			if i+1 < len(fields) {
				add(fields[i+1])
				i++
			}
		}
	}
	return paths, nil
}

func isLocalPathRemote(u string) bool {
	return strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".") || strings.HasPrefix(u, "file://")
}

func rewriteRemoteHost(remoteURL string, cfg *SSHHostConfig) string {
	if cfg == nil {
		return remoteURL
	}
	port := cfg.Port
	if port == "" {
		port = "22"
	}
	if m := scpLikeRemoteRe.FindStringSubmatch(remoteURL); m != nil {
		return fmt.Sprintf("ssh://%s@%s:%s/%s", cfg.User, cfg.HostName, port, strings.TrimPrefix(m[3], "/"))
	}
	return remoteURL
}
