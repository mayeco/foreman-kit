package gitx

import (
	"context"
	"path/filepath"
	"testing"
)

// rollbackIntegrate commits message on the child's remote base branch (main of the app-origin
// repository subIgnoreUmbrella cloned it from), fetches it into the child and checks it out: work the
// pipeline integrated. It returns the commit.
func rollbackIntegrate(t *testing.T, umbrella, child, message string) string {
	t.Helper()
	origin := filepath.Join(filepath.Dir(umbrella), "app-origin")
	gitT(t, origin, "commit", "--quiet", "--allow-empty", "-m", message)
	sha := gitT(t, origin, "rev-parse", "HEAD")
	gitT(t, child, "fetch", "--quiet", "origin")
	gitT(t, child, "checkout", "--quiet", sha)
	return sha
}

// TestFindPointerRollbackReportsARealRollbackWithAPositiveCount: the earlier gitlink is on the child's
// remote base and the current gitlink lacks it.
func TestFindPointerRollbackReportsARealRollbackWithAPositiveCount(t *testing.T) {
	ctx := context.Background()
	umbrella, child := subIgnoreUmbrella(t)
	first := gitT(t, child, "rev-parse", "HEAD")
	rollbackIntegrate(t, umbrella, child, "app: second")
	third := rollbackIntegrate(t, umbrella, child, "app: third")
	subIgnoreRecord(t, umbrella, "umbrella: record app's third commit")

	gitT(t, child, "checkout", "--quiet", first)
	rollback := subIgnoreRecord(t, umbrella, "umbrella: commit made with a stale child checkout")

	rb := FindPointerRollback(ctx, umbrella, "HEAD", "origin", "repos/app", 200)
	if rb == nil || rb.Commit != rollback || rb.From != third || rb.To != first || rb.Lost != 2 {
		t.Fatalf("rollback = %+v, want %s moving repos/app back from %s to %s (2 commits lost)", rb, rollback, third, first)
	}
}

// TestFindPointerRollbackReportsNothingWhenTheChildLacksTheCurrentGitlink: the umbrella records a child
// commit the checkout has not fetched (it descends from the earlier gitlink). The ancestry check failed
// on the missing object and reported a rollback with -1 integrated commits missing.
func TestFindPointerRollbackReportsNothingWhenTheChildLacksTheCurrentGitlink(t *testing.T) {
	ctx := context.Background()
	umbrella, child := subIgnoreUmbrella(t)
	first := gitT(t, child, "rev-parse", "HEAD")
	rollbackIntegrate(t, umbrella, child, "app: second")
	subIgnoreRecord(t, umbrella, "umbrella: record app's second commit")
	gitT(t, child, "checkout", "--quiet", first)
	subIgnoreRecord(t, umbrella, "umbrella: commit made with a stale child checkout")

	// A teammate integrates a third commit and records it; this checkout never fetched it.
	origin := filepath.Join(filepath.Dir(umbrella), "app-origin")
	gitT(t, origin, "commit", "--quiet", "--allow-empty", "-m", "app: third")
	third := gitT(t, origin, "rev-parse", "HEAD")
	gitT(t, umbrella, "update-index", "--cacheinfo", "160000,"+third+",repos/app")
	gitT(t, umbrella, "commit", "--quiet", "-m", "umbrella: record app's third commit")

	if rb := FindPointerRollback(ctx, umbrella, "HEAD", "origin", "repos/app", 200); rb != nil {
		t.Fatalf("rollback = %+v, want none: the child's checkout lacks the current gitlink %s", rb, third)
	}
}

// TestFindPointerRollbackReportsNothingWhenTheEarlierGitlinkIsNotOnTheChildBase: a person moved the
// gitlink back on purpose from a commit the child's remote base branch never took.
func TestFindPointerRollbackReportsNothingWhenTheEarlierGitlinkIsNotOnTheChildBase(t *testing.T) {
	ctx := context.Background()
	umbrella, child := subIgnoreUmbrella(t)
	first := gitT(t, child, "rev-parse", "HEAD")
	gitT(t, child, "commit", "--quiet", "--allow-empty", "-m", "app: never pushed")
	local := gitT(t, child, "rev-parse", "HEAD")
	subIgnoreRecord(t, umbrella, "umbrella: record app's local commit")

	gitT(t, child, "checkout", "--quiet", first)
	subIgnoreRecord(t, umbrella, "umbrella: app back to its first commit, on purpose")
	// The base moves on without the local commit.
	rollbackIntegrate(t, umbrella, child, "app: second")
	gitT(t, child, "checkout", "--quiet", first)

	if rb := FindPointerRollback(ctx, umbrella, "HEAD", "origin", "repos/app", 200); rb != nil {
		t.Fatalf("rollback = %+v, want none: %s is not on the child's origin/main", rb, local)
	}
}
