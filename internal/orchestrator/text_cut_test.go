package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

func TestCutTextNeverEndsInsideACharacter(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exact", 5, "exact"},
		{"plain ascii text", 5, "plain"},
		{"café au lait", 4, "caf"},  // the cut falls inside the two bytes of the accented e
		{"café au lait", 5, "café"}, // right after it
		{"ab日本", 4, "ab"},           // inside a three-byte character
		{"ab日本", 5, "ab日"},          // between two of them
		{"\U0001f600 smile", 3, ""}, // inside a four-byte character at the start
		{"x\U0001f600", 5, "x\U0001f600"},
		{"anything", 0, ""},
	} {
		got := cutText(tc.in, tc.max)
		if got != tc.want || !utf8.ValidString(got) || len(got) > tc.max && len(tc.in) > tc.max {
			t.Errorf("cutText(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

// The details of a change sent back are kept in conflict_details and, for a Jev send-back, handed to the
// BUILD agent as its rebuild feedback, an argument of the engine's command line. They were cut at byte
// 4000 wherever that fell: inside a multi-byte character, the text ended with bytes that are not UTF-8.
func TestSendBackStoresDetailsCutOnACharacterBoundary(t *testing.T) {
	sb := newMCFSandbox(t, "")
	ctx := context.Background()
	m := sb.newMerger(t, MergerOptions{})

	// Byte 4000 is the second byte of an accented letter.
	details := strings.Repeat("a", 3999) + strings.Repeat("é", 50)
	m.sendBack(ctx, store.ReadyManifest{ChangeName: "add-a", Branch: "feat/spec-add-a"}, "", details)

	q, err := sb.st.GetQuarantine(ctx, "add-a")
	if err != nil || q == nil {
		t.Fatalf("quarantine of add-a = %+v (err %v), want its rebuild request", q, err)
	}
	stored := q.ConflictDetails
	if !utf8.ValidString(stored) {
		t.Fatalf("the stored details end inside a character: % x", stored[len(stored)-4:])
	}
	if len(stored) > 4000 || !strings.HasPrefix(details, stored) || len(stored) < 3999 {
		t.Fatalf("the stored details are %d bytes, want the first 3999 of the %d sent", len(stored), len(details))
	}
}

// The message of a lost commit goes to Jev and into the report sent back to the agent: it was cut at
// byte 2000 the same way.
func TestDetectConcurrentWorkLossCutsALongMessageOnACharacterBoundary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(args ...string) string {
		t.Helper()
		out, err := gitx.RunGit(ctx, dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	commitAt := func(when time.Time, content, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "routes.go"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := when.Format(time.RFC3339)
		t.Setenv("GIT_COMMITTER_DATE", stamp)
		git("add", "-A")
		git("-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--date", stamp, "-m", message)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q", "-b", "main")

	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	commitAt(start.Add(-time.Hour), "keep\n", "feat: routes")
	// Byte 2000 of the message is the second byte of an accented letter.
	message := "fix: " + strings.Repeat("é", 1500)
	onto := commitAt(start.Add(10*time.Minute), "keep\nhotfix\n", message)
	rebased := commitAt(start.Add(20*time.Minute), "keep\nmine\n", "feat: mine")

	lost, err := DetectConcurrentWorkLoss(ctx, dir, "repos/backend", onto, rebased, start)
	if err != nil || len(lost) != 1 {
		t.Fatalf("lost = %+v (err %v), want the teammate's commit", lost, err)
	}
	got := lost[0].Message
	if !utf8.ValidString(got) {
		t.Fatalf("the message of the lost commit ends inside a character: % x", got[len(got)-4:])
	}
	if len(got) > workLossMaxMessageBytes || len(got) < workLossMaxMessageBytes-1 || !strings.HasPrefix(message, got) {
		t.Fatalf("the message is %d bytes, want the first %d of it at most, cut after a whole character", len(got), workLossMaxMessageBytes)
	}
}
