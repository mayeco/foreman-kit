package gitx

import (
	"strings"
	"testing"
)

// TestCompletedSinceRefusesRevisionsThatAreNotCommits: like git log base..head, CompletedSince errors
// (naming the revision) when a non-empty base or the head does not resolve to a commit, instead of
// listing the whole history; an empty base still lists everything and HasCommitWithTrailer keeps working.
func TestCompletedSinceRefusesRevisionsThatAreNotCommits(t *testing.T) {
	repo := t.TempDir()
	gitT(t, repo, "init", "--quiet", "-b", "main")
	for _, name := range []string{"alpha", "beta", "gamma"} {
		gitT(t, repo, "commit", "--quiet", "--allow-empty", "-m", "integrate "+name+"\n\nOpenSpec-Change: "+name)
	}
	tree := gitT(t, repo, "rev-parse", "HEAD^{tree}")

	r, err := Open(repo)
	if err != nil {
		t.Fatal(err)
	}

	all, err := r.CompletedSince("", "HEAD")
	if err != nil || len(all) != 3 {
		t.Fatalf("CompletedSince(\"\", HEAD) = %+v, %v; want the 3 changes", all, err)
	}
	newer, err := r.CompletedSince("HEAD~1", "HEAD")
	if err != nil || len(newer) != 1 || newer[0].ChangeName != "gamma" {
		t.Fatalf("CompletedSince(HEAD~1, HEAD) = %+v, %v; want gamma only", newer, err)
	}
	if ok, err := r.HasCommitWithTrailer("HEAD", "alpha"); err != nil || !ok {
		t.Fatalf("HasCommitWithTrailer(HEAD, alpha) = %v, %v", ok, err)
	}

	for _, tc := range []struct{ name, base, head string }{
		{"unknown base name", "nosuch", "HEAD"},
		{"mistyped base", "HEAD~1x", "HEAD"},
		{"absent base commit", "0123456789abcdef0123456789abcdef01234567", "HEAD"},
		{"base that is a tree", tree, "HEAD"},
		{"unknown head", "HEAD~1", "nosuchhead"},
		{"head that is a tree", "", tree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.CompletedSince(tc.base, tc.head)
			if err == nil {
				t.Fatalf("CompletedSince(%q, %q) = %+v, want an error", tc.base, tc.head, got)
			}
			rev := tc.base
			if strings.Contains(tc.name, "head") {
				rev = tc.head
			}
			if !strings.Contains(err.Error(), rev) {
				t.Fatalf("error %q does not name %q", err, rev)
			}
		})
	}
}
