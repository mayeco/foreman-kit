package cycle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// BrokenBaseInfo is a broken base (merge.outside_commits: verify) as the reports show it: a child
// repository whose current base fails a check on its own, and the ready changes the merger holds for it.
type BrokenBaseInfo struct {
	store.BrokenBase
	// Held lists the ready changes that touch the repository, sorted by name: the merge queue orders them by
	// the second each was enqueued, so changes built at the same time would be listed in either order.
	Held []string `json:"held"`
	// AcceptCommand integrates them anyway, with their own tests only.
	AcceptCommand string `json:"accept_command"`
	// RecheckAt is when the merger checks this base again on its own, once (in case what failed has
	// healed): merge.base_recheck_after after its last failed verdict. Empty when it already did
	// (RecheckedAt) or that key says never.
	RecheckAt string `json:"recheck_at,omitempty"`
}

// recheckNote says when the base is checked again on its own, or that it already was.
func (b BrokenBaseInfo) recheckNote() string {
	switch {
	case b.RecheckAt != "":
		return "; checked again automatically once at " + b.RecheckAt
	case b.RecheckedAt != "":
		return "; it still failed when checked again automatically at " + b.RecheckedAt
	}
	return ""
}

func shortBaseSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// StatusLine is the line foreman-kit status prints for the broken base.
func (b BrokenBaseInfo) StatusLine() string {
	return fmt.Sprintf("BASE BROKEN: %s at %s since %s: %s %s on the base alone (%d outside commit(s)); holding: %s; %s to integrate anyway%s",
		b.Repo, shortBaseSHA(b.SHA), b.Since, b.ChecksText(), b.ChecksVerb(), b.OutsideCommits, joinOrNone(b.Held), b.AcceptCommand, b.recheckNote())
}

// NeedsHumanText is the item foreman-kit progress lists under "Needs a human" for the broken base.
func (b BrokenBaseInfo) NeedsHumanText() string {
	return fmt.Sprintf("%s's base at %s fails its %s on its own (%d outside commit(s)), holding %s: fix the repository's base, or %s to integrate them anyway%s",
		b.Repo, shortBaseSHA(b.SHA), b.ChecksText(), b.OutsideCommits, joinOrNone(b.Held), b.AcceptCommand, b.recheckNote())
}

func (b BrokenBaseInfo) asMap() map[string]any {
	return map[string]any{
		"repo":            b.Repo,
		"sha":             b.SHA,
		"since":           b.Since,
		"failed_checks":   b.FailedChecks,
		"outside_commits": b.OutsideCommits,
		"pointer":         b.Pointer,
		"held":            b.Held,
		"accept_command":  b.AcceptCommand,
		"recheck_at":      b.RecheckAt,
		"rechecked_at":    b.RecheckedAt,
	}
}

// BrokenBases lists the broken bases of the workspace with the ready changes each holds, read from the
// database and from the umbrella's object database (no fetch). It is empty unless
// merge.outside_commits is verify: in the other modes the merger holds nothing.
func BrokenBases(ctx context.Context, root string, st *store.Store) []BrokenBaseInfo {
	if st == nil || settings.String(root, settings.OutsideCommits) != settings.OutsideCommitsVerify {
		return nil
	}
	broken, err := st.ListBrokenBases(ctx)
	if err != nil || len(broken) == 0 {
		return nil
	}
	ready, _ := st.ListReadyQueue(ctx)
	touchedBy := make(map[string][]string)
	if repo, err := gitx.Open(root); err == nil && len(ready) > 0 {
		baseRef := resolveLocalBaseRef(ctx, root)
		for _, r := range ready {
			head := strings.TrimSpace(r.HeadSHA)
			if head == "" {
				head = r.Branch
			}
			touched, err := repo.TouchedChildren(baseRef, head)
			if err != nil {
				continue
			}
			for _, cp := range touched {
				if r.ChildrenPushed[cp] == "" {
					touchedBy[cp] = append(touchedBy[cp], r.ChangeName)
				}
			}
		}
	}
	out := make([]BrokenBaseInfo, 0, len(broken))
	for _, b := range broken {
		held := touchedBy[b.Repo]
		if held == nil {
			held = []string{}
		}
		sort.Strings(held)
		out = append(out, BrokenBaseInfo{
			BrokenBase:    b,
			Held:          held,
			AcceptCommand: fmt.Sprintf("foreman-kit base accept %s %s", b.Repo, shortBaseSHA(b.SHA)),
			RecheckAt:     baseRecheckAt(root, b),
		})
	}
	return out
}

// baseRecheckAt is when the merger checks broken base b again on its own (Merger.baseRecheckDue):
// merge.base_recheck_after after its last failed verdict; "" when it already did, or that key says
// never.
func baseRecheckAt(root string, b store.BrokenBase) string {
	delay := settings.Duration(root, settings.BaseRecheckAfter)
	if b.RecheckedAt != "" || delay <= 0 {
		return ""
	}
	last, err := time.Parse(time.RFC3339, b.LastFailedAt)
	if err != nil {
		return ""
	}
	return last.Add(delay).UTC().Format(time.RFC3339)
}

// resolveLocalBaseRef returns the umbrella's base branch as the last fetch saw it (origin/<base>),
// falling back to the local branch and then HEAD.
func resolveLocalBaseRef(ctx context.Context, root string) string {
	base := ResolveBaseBranch(root)
	remote := settings.RemoteName(root)
	for _, ref := range []string{remote + "/" + base, base} {
		if _, err := gitx.RunGit(ctx, root, "rev-parse", "--verify", "--quiet", ref); err == nil {
			return ref
		}
	}
	return "HEAD"
}

// NormalizeRepoArg turns "core" into "repos/core" and leaves "repos/core" as it is.
func NormalizeRepoArg(repo string) string {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo != "" && !strings.Contains(repo, "/") {
		repo = "repos/" + repo
	}
	return repo
}

// BaseVerifier checks the base sha of child repository repo on its own and records a verdict for each
// of its checks, as the merger would (orchestrator.VerifyBaseForAccept). An empty sha is the commit the
// umbrella's base branch records for repo. It returns the full sha it checked.
type BaseVerifier func(ctx context.Context, repo, sha string) (string, error)

// AcceptBrokenBase marks the failed verdicts of repository repo's base sha accepted by `by` and emits
// merge.base_accepted: the merger then integrates the changes it held there. The tests that failed on
// that base alone stay accepted for them and for the changes built on it afterwards (until another
// commit is pushed outside the pipeline); a change failing any other test is quarantined.
// An empty sha takes the repository's current broken base; a short one is matched as a prefix. It
// returns the base it accepted and how many verdicts it marked.
//
// A base nothing has checked yet has no verdict to accept: a worker meets a test that fails on the
// base before the merger ever does. With verify, such a base is checked on its own first, and what
// fails there is accepted; a base that passes every check is refused, since the failure is then the
// change's.
func AcceptBrokenBase(ctx context.Context, st *store.Store, repo, sha, by string, verify ...BaseVerifier) (store.BrokenBase, int, error) {
	repo = NormalizeRepoArg(repo)
	sha = strings.TrimSpace(sha)
	if repo == "" {
		return store.BrokenBase{}, 0, fmt.Errorf("name the repository, e.g. repos/core")
	}
	if st == nil {
		return store.BrokenBase{}, 0, fmt.Errorf("no state database")
	}
	broken, err := st.ListBrokenBases(ctx)
	if err != nil {
		return store.BrokenBase{}, 0, err
	}
	var target *store.BrokenBase
	for i := range broken {
		b := broken[i]
		if b.Repo == repo && (sha == "" || strings.HasPrefix(b.SHA, sha)) {
			target = &b
		}
	}
	failedAt := func(sha string) (*store.BrokenBase, error) {
		verdicts, err := st.ListBaseVerifications(ctx, repo, "")
		if err != nil {
			return nil, err
		}
		var found *store.BrokenBase
		for _, v := range verdicts {
			if v.Status == store.BaseVerdictFailed && strings.HasPrefix(v.BaseSHA, sha) {
				if found == nil {
					found = &store.BrokenBase{Repo: repo, SHA: v.BaseSHA, Since: v.CreatedAt, Pointer: v.PointerSHA, OutsideCommits: v.OutsideCommits}
				}
				if v.BaseSHA == found.SHA {
					found.FailedChecks = append(found.FailedChecks, v.CheckName)
				}
			}
		}
		return found, nil
	}
	if target == nil && sha != "" {
		if target, err = failedAt(sha); err != nil {
			return store.BrokenBase{}, 0, err
		}
	}
	if target == nil && len(verify) > 0 && verify[0] != nil {
		checked, err := verify[0](ctx, repo, sha)
		if err != nil {
			if sha == "" {
				return store.BrokenBase{}, 0, fmt.Errorf("%s has no broken base, and its base could not be checked: %w", repo, err)
			}
			return store.BrokenBase{}, 0, fmt.Errorf("%s has no failed verdict at %s, and that base could not be checked: %w", repo, sha, err)
		}
		if target, err = failedAt(checked); err != nil {
			return store.BrokenBase{}, 0, err
		}
		if target == nil {
			if verdicts, err := st.ListBaseVerifications(ctx, repo, checked); err == nil {
				for _, v := range verdicts {
					if v.Status == store.BaseVerdictAccepted {
						return store.BrokenBase{}, 0, fmt.Errorf("%s at %s was accepted already (by %s)", repo, shortBaseSHA(checked), v.AcceptedBy)
					}
				}
			}
			return store.BrokenBase{}, 0, fmt.Errorf("%s at %s passes every check on its own: what fails is the change's, not the base's", repo, shortBaseSHA(checked))
		}
	}
	if target == nil {
		if sha == "" {
			return store.BrokenBase{}, 0, fmt.Errorf("%s has no broken base", repo)
		}
		return store.BrokenBase{}, 0, fmt.Errorf("%s has no failed verdict at %s", repo, sha)
	}
	if strings.TrimSpace(by) == "" {
		by = "unknown"
	}
	n, err := st.AcceptBaseVerifications(ctx, repo, target.SHA, by, time.Now().UTC())
	if err != nil {
		return *target, 0, err
	}
	if n == 0 {
		return *target, 0, fmt.Errorf("%s at %s has no failed verdict left to accept", repo, shortBaseSHA(target.SHA))
	}
	_ = st.EmitEvent(ctx, store.EventInput{
		Event: "merge.base_accepted",
		Payload: map[string]any{
			"repo":          repo,
			"sha":           target.SHA,
			"by":            by,
			"failed_checks": target.FailedChecks,
		},
	})
	return *target, n, nil
}

// AcceptingUser names who accepts a base from the command line or the portal: $USER.
func AcceptingUser() string {
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		return u
	}
	return "unknown"
}

// RunBaseCLI implements `foreman-kit base status [--json]` and `foreman-kit base accept <repo> [<sha>]`.
func RunBaseCLI(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer) int {
	return RunBaseCLIWith(ctx, root, st, args, stdout, stderr, nil)
}

// RunBaseCLIWith is RunBaseCLI with the verifier `base accept` checks a base with when nothing has
// checked it yet (AcceptBrokenBase).
func RunBaseCLIWith(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer, verify BaseVerifier) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	usage := "usage: foreman-kit base status [--json] | foreman-kit base accept <repo> [<sha>]"
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	case "status":
		asJSON := false
		for _, a := range args[1:] {
			if a == "--json" {
				asJSON = true
			} else {
				fmt.Fprintf(stderr, "base status: unknown argument %s\n", a)
				return 2
			}
		}
		bases := BrokenBases(ctx, root, st)
		if asJSON {
			if bases == nil {
				bases = []BrokenBaseInfo{}
			}
			b, _ := json.MarshalIndent(map[string]any{"broken_bases": bases}, "", "  ")
			fmt.Fprintln(stdout, string(b))
			return 0
		}
		if len(bases) == 0 {
			fmt.Fprintln(stdout, "no broken base: every repository's base passes on its own, or was accepted")
			return 0
		}
		for _, b := range bases {
			fmt.Fprintf(stdout, "%s\n", b.Repo)
			fmt.Fprintf(stdout, "  base:            %s\n", b.SHA)
			fmt.Fprintf(stdout, "  since:           %s\n", b.Since)
			fmt.Fprintf(stdout, "  fails alone:     %s\n", strings.Join(b.FailedChecks, ", "))
			if b.Pointer != "" {
				fmt.Fprintf(stdout, "  outside commits: %d (%s..%s)\n", b.OutsideCommits, shortBaseSHA(b.Pointer), shortBaseSHA(b.SHA))
			} else {
				fmt.Fprintf(stdout, "  outside commits: %d\n", b.OutsideCommits)
			}
			fmt.Fprintf(stdout, "  holding:         %s\n", joinOrNone(b.Held))
			switch {
			case b.RecheckAt != "":
				fmt.Fprintf(stdout, "  checked again:   automatically, once, at %s\n", b.RecheckAt)
			case b.RecheckedAt != "":
				fmt.Fprintf(stdout, "  checked again:   at %s, and it still failed\n", b.RecheckedAt)
			}
			fmt.Fprintf(stdout, "  to integrate anyway (the tests failing here stay accepted, for later changes too; any other failure is the change's): %s\n", b.AcceptCommand)
			if ex := strings.TrimSpace(b.Excerpt); ex != "" {
				fmt.Fprintln(stdout, "  output on the base alone:")
				for _, l := range strings.Split(ex, "\n") {
					fmt.Fprintf(stdout, "    %s\n", l)
				}
			}
		}
		return 0
	case "accept":
		if len(args) < 2 || len(args) > 3 {
			fmt.Fprintln(stderr, "usage: foreman-kit base accept <repo> [<sha>]")
			return 2
		}
		sha := ""
		if len(args) == 3 {
			sha = args[2]
		}
		b, n, err := AcceptBrokenBase(ctx, st, args[1], sha, AcceptingUser(), verify)
		if err != nil {
			fmt.Fprintf(stderr, "base accept: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "accepted: %s at %s (%d verdict(s): %s); the merger integrates its held changes with their own tests on its next pass\n",
			b.Repo, shortBaseSHA(b.SHA), n, strings.Join(b.FailedChecks, ", "))
		return 0
	}
	fmt.Fprintln(stderr, usage)
	return 2
}
