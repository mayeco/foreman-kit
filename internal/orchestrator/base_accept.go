package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// The merger records that a child repository's base fails a check on its own only when it meets
// the failure while integrating a change (base_verify.go). A worker meets it first: its BUILD runs
// the repository's gates on the base the umbrella records, a test that the change never touched
// fails there, and the agent is told to have a person run foreman-kit base accept. With no verdict
// recorded for that base, accept had nothing to accept. VerifyBaseForAccept is what it does then:
// it checks the base alone, the way the merger would, so that a person can accept what fails there.

// VerifyBaseForAccept checks the base sha of child repository repo on its own, in a worktree of its
// own, and records a verdict for each check that has none yet: every gate of its repo.config.yaml
// and, when it has commands, its regression. An empty sha is the commit the umbrella's base branch
// records for repo, the one workers build on. It returns the full sha it checked; the caller accepts
// the verdicts that failed. Nothing of the main checkout is changed.
func VerifyBaseForAccept(ctx context.Context, root string, st *store.Store, repo, sha string, out io.Writer) (string, error) {
	if out == nil {
		out = io.Discard
	}
	if st == nil {
		return "", fmt.Errorf("no state database")
	}
	childDir := filepath.Join(root, filepath.FromSlash(repo))
	if _, err := gitx.RunGit(ctx, childDir, "rev-parse", "--git-dir"); err != nil {
		return "", fmt.Errorf("%s is not checked out in %s: foreman-kit sync checks it out", repo, root)
	}

	umbrella, err := gitx.Open(root)
	if err != nil {
		return "", err
	}
	baseRef := ""
	branch := settings.String(root, settings.BaseBranch)
	for _, ref := range []string{settings.RemoteName(root) + "/" + branch, branch, "HEAD"} {
		if _, err := gitx.RunGit(ctx, root, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			baseRef = ref
			break
		}
	}
	pointer, _ := umbrella.TreeEntryHash(baseRef, repo)
	sha = strings.TrimSpace(sha)
	if sha == "" {
		if pointer == "" {
			return "", fmt.Errorf("%s records no commit for %s: name the base to check", baseRef, repo)
		}
		sha = pointer
	} else {
		full, err := gitx.RunGit(ctx, childDir, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("%s has no commit %s (fetch it first: git -C %s fetch)", repo, sha, repo)
		}
		sha = strings.TrimSpace(full)
	}

	// A worktree of the umbrella at its base, with the child at sha in it: the gates run with the
	// application's settings and bin/, as they do in a worker, and nothing they build or install
	// lands in the main checkout.
	tmp, err := os.MkdirTemp(settings.WorktreesBase(root), "_base_accept-")
	if err != nil {
		if mkErr := os.MkdirAll(settings.WorktreesBase(root), 0o755); mkErr != nil {
			return "", err
		}
		if tmp, err = os.MkdirTemp(settings.WorktreesBase(root), "_base_accept-"); err != nil {
			return "", err
		}
	}
	_ = os.Remove(tmp)
	cleanup := func() {
		done, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_, _ = gitx.RunGit(done, childDir, "worktree", "remove", "--force", filepath.Join(tmp, filepath.FromSlash(repo)))
		_, _ = gitx.RunGit(done, root, "worktree", "remove", "--force", tmp)
		_ = os.RemoveAll(tmp)
		_, _ = gitx.RunGit(done, childDir, "worktree", "prune")
		_, _ = gitx.RunGit(done, root, "worktree", "prune")
	}
	defer cleanup()
	if _, err := gitx.RunGit(ctx, root, "worktree", "add", "--quiet", "--detach", tmp, baseRef); err != nil {
		return "", fmt.Errorf("cannot prepare a worktree of the umbrella at %s: %w", baseRef, err)
	}
	childWT := filepath.Join(tmp, filepath.FromSlash(repo))
	_ = os.RemoveAll(childWT)
	if err := os.MkdirAll(filepath.Dir(childWT), 0o755); err != nil {
		return "", err
	}
	if _, err := gitx.RunGit(ctx, childDir, "worktree", "add", "--quiet", "--detach", childWT, sha); err != nil {
		return "", fmt.Errorf("cannot check %s out at %s: %w", repo, shortSHA(sha), err)
	}
	for _, rel := range localChildConfigs {
		src := filepath.Join(childDir, filepath.FromSlash(rel))
		dst := filepath.Join(childWT, filepath.FromSlash(rel))
		if fileExists(src) && !fileExists(dst) {
			_ = os.MkdirAll(filepath.Dir(dst), 0o755)
			if err := os.Symlink(src, dst); err != nil {
				if b, err := os.ReadFile(src); err == nil {
					_ = os.WriteFile(dst, b, 0o600)
				}
			}
		}
	}

	raw, _, err := config.ReadRepoConfig("", childWT)
	if err != nil {
		return "", fmt.Errorf("%s at %s: %w", repo, shortSHA(sha), err)
	}
	cfg, err := parseChildGateConfig(raw)
	if err != nil {
		return "", fmt.Errorf("%s at %s: invalid repo.config.yaml: %w", repo, shortSHA(sha), err)
	}
	var checks []string
	for key, cmd := range cfg.Gates {
		if cmd = strings.TrimSpace(cmd); key != "setup" && cmd != "" && !strings.Contains(cmd, "{{") {
			checks = append(checks, BaseCheckGatePrefix+key)
		}
	}
	sort.Strings(checks)
	if cfg.hasRegressionCommands() {
		checks = append(checks, BaseCheckRegression)
	}
	if len(checks) == 0 {
		return sha, fmt.Errorf("%s at %s has no gate to check", repo, shortSHA(sha))
	}

	outside := countCommits(ctx, childDir, pointer, sha)
	run := DefaultBaseCheckRunner(st)
	labels := make([]string, 0, len(checks))
	for _, c := range checks {
		labels = append(labels, store.BaseCheckLabel(c))
	}
	fmt.Fprintf(out, "base accept: %s at %s has no verdict yet: checking it on its own (%s)\n", repo, shortSHA(sha), strings.Join(labels, ", "))
	for _, check := range checks {
		if v, err := st.GetBaseVerification(ctx, repo, sha, check); err == nil && v != nil {
			fmt.Fprintf(out, "  %s: %s (recorded before)\n", store.BaseCheckLabel(check), v.Status)
			continue
		}
		res, err := run(ctx, BaseCheckRequest{
			Worktree: tmp,
			Child:    repo,
			Check:    check,
			Slot:     BaseCheckSlot,
			Timeout:  settings.Duration(root, settings.GateTimeout),
		})
		if err != nil {
			return sha, err
		}
		if err := ctx.Err(); err != nil {
			return sha, err
		}
		if res.Inconclusive {
			fmt.Fprintf(out, "  %s: did not run to a result (it could not start): no verdict\n", store.BaseCheckLabel(check))
			continue
		}
		rec := store.BaseVerification{
			RepoPath:       repo,
			BaseSHA:        sha,
			CheckName:      check,
			Status:         store.BaseVerdictPassed,
			ExitCode:       res.ExitCode,
			PointerSHA:     pointer,
			OutsideCommits: outside,
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		}
		if !res.OK {
			rec.Status = store.BaseVerdictFailed
			if rec.ExitCode == 0 {
				rec.ExitCode = 1
			}
			rec.Excerpt = RedactExcerpt(FailureExcerpt(res.Output), 40, 4000)
		}
		if _, err := st.RecordBaseVerification(store.WithGateRound(ctx, res.RoundID), rec); err != nil {
			return sha, err
		}
		fmt.Fprintf(out, "  %s: %s on the base alone\n", store.BaseCheckLabel(check), rec.Status)
	}
	return sha, nil
}
