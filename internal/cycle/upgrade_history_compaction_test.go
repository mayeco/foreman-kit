package cycle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/history"
)

// An update at the same kit version regroups the small rotated history files an older kit left (one
// per rotation) into files of history.MaxLines lines, and commits the files it removed too.
func TestUpgradeCompactsSmallRotatedHistory(t *testing.T) {
	ctx := context.Background()
	appDir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = appDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q", "-b", "master")
	if err := Init(ctx, appDir, KitOptions{Agents: "claude", Version: "4.0.0-go"}, nil); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	const files, per = 40, 30
	for i := 0; i < files; i++ {
		var b strings.Builder
		b.WriteString("[\n")
		for j := 0; j < per; j++ {
			fmt.Fprintf(&b, `{"change_name":"c-%04d"}`, i*per+j)
			if j < per-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString("]\n")
		path := filepath.Join(appDir, ".foreman-kit", fmt.Sprintf("agent_history.%d.json", files-i))
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "app with small rotated history")

	var out bytes.Buffer
	if err := Upgrade(ctx, appDir, KitOptions{Version: "4.0.0-go"}, &out); err != nil {
		t.Fatalf("Upgrade failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "regrouped") {
		t.Fatalf("expected the update to say it regrouped the history, got:\n%s", out.String())
	}
	rotated := RotatedHistoryRelPaths(appDir)
	if len(rotated) != 2 {
		t.Fatalf("want 2 rotated files for %d changes, got %v", files*per, rotated)
	}
	full, err := os.ReadFile(filepath.Join(appDir, rotated[1]))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(full, []byte("\n")); got != history.MaxLines {
		t.Fatalf("%s has %d lines, want %d", rotated[1], got, history.MaxLines)
	}
	all, err := ReadAllHistory(appDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != files*per || all[0]["change_name"] != "c-0000" || all[len(all)-1]["change_name"] != fmt.Sprintf("c-%04d", files*per-1) {
		t.Fatalf("history lost or reordered: %d changes", len(all))
	}
	if status := git("status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("the update left uncommitted files:\n%s", status)
	}
}
