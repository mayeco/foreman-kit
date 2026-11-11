package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ConcurrentCommitsOnFiles caps the texts it returns in bytes: each concurrent commit's message and diff,
// and the change's own diff. They were cut wherever the cap fell, also inside a multi-byte character, and
// those texts are stored as the details of a send-back and handed to the BUILD agent as an argument.
func TestConcurrentCommitsOnFilesCutsItsTextsOnACharacterBoundary(t *testing.T) {
	// Two alignments: with two-byte characters a cut at a fixed byte offset splits a character in one of
	// them, whatever the length of the headers that come before the text.
	for _, pad := range []string{"", "a"} {
		t.Run("pad="+pad, func(t *testing.T) {
			ctx := context.Background()
			dir, git := newTestRepo(t)
			commitAt := func(when time.Time, msg string) string {
				t.Helper()
				stamp := when.Format(time.RFC3339)
				t.Setenv("GIT_COMMITTER_DATE", stamp)
				git("add", "-A")
				git("commit", "-q", "--date", stamp, "-m", msg)
				return git("rev-parse", "HEAD")
			}
			notes := filepath.Join(dir, "notes.md")
			long := pad + strings.Repeat("é", 4000)

			start := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.WriteFile(notes, []byte("first\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			commitAt(start.Add(-time.Hour), "early")
			// A teammate's commit after the change started, with a long line and a long message.
			if err := os.WriteFile(notes, []byte("first\n"+long+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			onto := commitAt(start.Add(5*time.Minute), pad+strings.Repeat("é", 1500))
			// The change adds another long line to the same file.
			if err := os.WriteFile(notes, []byte("first\n"+long+"\n"+long+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			rebased := commitAt(start.Add(10*time.Minute), "feat: more notes")

			cs, diff, err := ConcurrentCommitsOnFiles(ctx, dir, "repos/docs", onto, rebased, start)
			if err != nil {
				t.Fatal(err)
			}
			if len(cs) != 1 {
				t.Fatalf("want the one concurrent commit on notes.md, got %d", len(cs))
			}
			if len(cs[0].Message) > workLossMaxMessageBytes || len(cs[0].Message) < workLossMaxMessageBytes-utf8.UTFMax {
				t.Fatalf("the message was not cut at its cap: %d bytes", len(cs[0].Message))
			}
			if !strings.Contains(cs[0].Diff, "(diff truncated)") || !strings.Contains(diff, "(diff truncated)") {
				t.Fatalf("the diffs were not cut at their caps: %d and %d bytes", len(cs[0].Diff), len(diff))
			}
			for name, text := range map[string]string{
				"the concurrent commit's message": cs[0].Message,
				"the concurrent commit's diff":    cs[0].Diff,
				"the change's diff":               diff,
			} {
				if !utf8.ValidString(text) {
					t.Errorf("%s was cut inside a character: it is not valid UTF-8 any more", name)
				}
			}
		})
	}
}
