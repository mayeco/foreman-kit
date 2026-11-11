package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// subIgnoreUmbrella returns an umbrella whose .gitmodules marks its child repos/app "ignore = all" (a
// setting teams use to keep a dirty submodule out of git status), with the child at its first commit.
// With it, git's porcelain commands (status, diff, log --raw) report no change of the child's gitlink,
// committed or not, unless they are told --ignore-submodules=none.
func subIgnoreUmbrella(t *testing.T) (umbrella, child string) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	base := t.TempDir()
	childSrc := filepath.Join(base, "app-origin")
	if err := os.MkdirAll(childSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, childSrc, "init", "--quiet", "-b", "main")
	gitT(t, childSrc, "commit", "--quiet", "--allow-empty", "-m", "app: first")

	umbrella = filepath.Join(base, "umbrella")
	if err := os.MkdirAll(umbrella, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, umbrella, "init", "--quiet", "-b", "main")
	gitT(t, umbrella, "submodule", "--quiet", "add", childSrc, "repos/app")
	gitT(t, umbrella, "config", "-f", ".gitmodules", "submodule.repos/app.ignore", "all")
	gitT(t, umbrella, "add", ".gitmodules")
	gitT(t, umbrella, "commit", "--quiet", "-m", "umbrella: add repos/app")
	return umbrella, filepath.Join(umbrella, "repos", "app")
}

// subIgnoreRecord commits the child's current HEAD as its gitlink and returns the umbrella commit.
func subIgnoreRecord(t *testing.T, umbrella, message string) string {
	t.Helper()
	if err := StageGitlinks(context.Background(), umbrella, "repos/app"); err != nil {
		t.Fatal(err)
	}
	gitT(t, umbrella, "commit", "--quiet", "-m", message)
	return gitT(t, umbrella, "rev-parse", "HEAD")
}

// TestStageGitlinksStagesAChildMarkedIgnoreAll: since git 2.54, "git add repos/app" skips such a child
// with exit status 0, and the commit that follows keeps the old gitlink.
func TestStageGitlinksStagesAChildMarkedIgnoreAll(t *testing.T) {
	umbrella, child := subIgnoreUmbrella(t)
	gitT(t, child, "commit", "--quiet", "--allow-empty", "-m", "app: second")
	second := gitT(t, child, "rev-parse", "HEAD")

	if err := StageGitlinks(context.Background(), umbrella, "repos/app"); err != nil {
		t.Fatal(err)
	}
	// "160000 <sha> 0\trepos/app"
	if entry := strings.Fields(gitT(t, umbrella, "ls-files", "-s", "repos/app")); len(entry) < 2 || entry[1] != second {
		t.Fatalf("the index records repos/app as %q, want its checkout %s", entry, second)
	}
}

func subIgnoreHas(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// TestChangedPathsListsAMovedGitlinkWhateverTheSubmoduleIgnoreSetting: the gates decide which child
// repositories a change touched from ChangedPaths; with "ignore = all" the child never appeared in it.
func TestChangedPathsListsAMovedGitlinkWhateverTheSubmoduleIgnoreSetting(t *testing.T) {
	ctx := context.Background()
	umbrella, child := subIgnoreUmbrella(t)
	before := gitT(t, umbrella, "rev-parse", "HEAD")

	gitT(t, child, "commit", "--quiet", "--allow-empty", "-m", "app: second")
	got, err := ChangedPaths(ctx, umbrella, "")
	if err != nil {
		t.Fatal(err)
	}
	if !subIgnoreHas(got, "repos/app") {
		t.Fatalf("ChangedPaths = %q, want repos/app: its checkout moved past the recorded gitlink", got)
	}

	subIgnoreRecord(t, umbrella, "umbrella: record app's second commit")
	got, err = ChangedPaths(ctx, umbrella, before)
	if err != nil {
		t.Fatal(err)
	}
	if !subIgnoreHas(got, "repos/app") {
		t.Fatalf("ChangedPaths since %s = %q, want repos/app: its gitlink was committed since", before, got)
	}
}

// TestFindPointerRollbackSeesGitlinksWhateverTheSubmoduleIgnoreSetting: with "ignore = all", git log --raw
// printed no gitlink line, so a rollback was never found.
func TestFindPointerRollbackSeesGitlinksWhateverTheSubmoduleIgnoreSetting(t *testing.T) {
	ctx := context.Background()
	umbrella, child := subIgnoreUmbrella(t)
	first := gitT(t, child, "rev-parse", "HEAD")

	gitT(t, child, "commit", "--quiet", "--allow-empty", "-m", "app: second")
	second := gitT(t, child, "rev-parse", "HEAD")
	subIgnoreRecord(t, umbrella, "umbrella: record app's second commit")

	// A stale checkout of the child committed with the umbrella: the gitlink moves back.
	gitT(t, child, "checkout", "--quiet", first)
	rollback := subIgnoreRecord(t, umbrella, "umbrella: unrelated commit made with a stale child checkout")

	rb := FindPointerRollback(ctx, umbrella, "HEAD", "repos/app", 200)
	if rb == nil || rb.Commit != rollback || rb.From != second || rb.To != first || rb.Lost != 1 {
		t.Fatalf("rollback = %+v, want %s moving repos/app back from %s to %s (1 commit lost)", rb, rollback, second, first)
	}
}
