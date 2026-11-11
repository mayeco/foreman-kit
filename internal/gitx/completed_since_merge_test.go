package gitx

import (
	"fmt"
	"strings"
	"testing"
)

// TestCompletedSinceMatchesGitLogWithMergeCommits builds integrations c0..c8 on main with a kit-upgrade
// branch forked at c2 and merged with --no-ff after c8. CompletedSince(c5, HEAD) must list exactly what
// `git log --reverse c5..HEAD` lists (the walk used to stop at c5 while still emitting c1 and c2, which
// are reachable from c5 through the side branch).
func TestCompletedSinceMatchesGitLogWithMergeCommits(t *testing.T) {
	repo := t.TempDir()
	gitT(t, repo, "init", "--quiet", "-b", "main")
	commit := func(msg string) string {
		gitT(t, repo, "commit", "--quiet", "--allow-empty", "-m", msg)
		return gitT(t, repo, "rev-parse", "HEAD")
	}
	integrate := func(i int) string { return commit(fmt.Sprintf("integrate c%d\n\nOpenSpec-Change: c%d", i, i)) }
	for i := 0; i <= 2; i++ {
		integrate(i)
	}
	gitT(t, repo, "checkout", "--quiet", "-b", "kit-upgrade")
	commit("kit upgrade")
	gitT(t, repo, "checkout", "--quiet", "main")
	var c5 string
	for i := 3; i <= 8; i++ {
		if sha := integrate(i); i == 5 {
			c5 = sha
		}
	}
	gitT(t, repo, "merge", "--quiet", "--no-ff", "-m", "merge kit upgrade", "kit-upgrade")

	r, err := Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.CompletedSince(c5, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ic := range got {
		names = append(names, ic.ChangeName)
	}
	var want []string
	for _, line := range strings.Split(gitT(t, repo, "log", "--reverse", "--format=%(trailers:key=OpenSpec-Change,valueonly)", c5+"..HEAD"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			want = append(want, line)
		}
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("CompletedSince(c5, HEAD) = %v, git log c5..HEAD = %v", names, want)
	}

	all, err := r.CompletedSince("", "HEAD")
	if err != nil || len(all) != 9 || all[0].ChangeName != "c0" || all[8].ChangeName != "c8" {
		t.Fatalf("expected c0..c8 oldest first without a base, got %+v (err=%v)", all, err)
	}
}
