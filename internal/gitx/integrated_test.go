package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A change is integrated on a ref when the ref's tree archives it under
// openspec/changes/archive/<date>-<name>/ or a commit of its history carries its OpenSpec-Change
// trailer; what the working tree holds, or a ref that does not exist, adds nothing.
func TestIntegratedOnReadsTheArchiveAndTheTrailersOfTheRef(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "--quiet", "-b", "master")
	write := func(rel string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("openspec/changes/archive/2026-01-01-archived-by-hand/proposal.md")
	write("openspec/changes/archive/README.md")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "--quiet", "-m", "chore: archive by hand")
	write("other.txt")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "--quiet", "-m", "feat(merged-by-trailer): merged\n\nOpenSpec-Change: merged-by-trailer")
	// Only in the working tree: not on the ref.
	write("openspec/changes/archive/2026-02-02-only-on-disk/proposal.md")

	got := IntegratedOn(context.Background(), dir, "master")
	for _, name := range []string{"archived-by-hand", "merged-by-trailer"} {
		if !got[name] {
			t.Errorf("IntegratedOn(master) = %v, want it to hold %s", got, name)
		}
	}
	if got["only-on-disk"] || got["README.md"] {
		t.Errorf("IntegratedOn(master) = %v holds a change archived only in the working tree, or an undated entry", got)
	}
	if none := IntegratedOn(context.Background(), dir, "no-such-ref"); len(none) != 0 {
		t.Errorf("IntegratedOn(no-such-ref) = %v, want nothing", none)
	}
}
