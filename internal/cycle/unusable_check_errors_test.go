package cycle

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// `foreman-kit status` and the processes that start work say what `foreman-kit config check`
// rejects, not only what the catalog does: a role that names an agent with no commands, and a
// visual session slot that a worker takes.
func TestStatusAndRequireUsableSayWhatConfigCheckRejects(t *testing.T) {
	root := bareApp(t)
	st := openTestStore(t, root)
	status := func() string {
		t.Helper()
		var out bytes.Buffer
		if err := Status(context.Background(), root, st, &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := status(); !strings.Contains(got, "CONFIG: OK\n") {
		t.Fatalf("status on whole files:\n%s", got)
	}

	settingstest.WriteFile(t, root, "agents", "roles:\n  plan:\n    agents: [ghost-agent]\n")
	if got := status(); !strings.Contains(got, "CONFIG: .foreman-kit/agents.yaml: ") || !strings.Contains(got, "ghost-agent") ||
		!strings.Contains(got, "what reads that file starts nothing new until it is mended\n") || strings.Contains(got, "CONFIG: OK") {
		t.Fatalf("status on an agents.yaml that config check rejects:\n%s", got)
	}
	if err := RequireUsable(root, settings.CycleFiles...); err == nil || !strings.Contains(err.Error(), "ghost-agent") {
		t.Fatalf("the cycle starts on an agents.yaml that config check rejects: %v", err)
	}
	if err := RequireUsable(root, settings.VisualSessionFiles...); err != nil {
		t.Fatalf("the visual session does not read agents.yaml: %v", err)
	}

	settingstest.WriteFile(t, root, "agents", "")
	settingstest.WriteFile(t, root, "pipeline", "repository:\n  base_branch: main\ndispatch:\n  workers: 4\n")
	settingstest.WriteFile(t, root, "visual_qa", "slot: 3\n")
	if got := status(); !strings.Contains(got, "CONFIG: .foreman-kit/visual_qa.yaml: ") || !strings.Contains(got, "3 is taken") {
		t.Fatalf("status on a visual session slot a worker takes:\n%s", got)
	}
	for who, files := range map[string][]string{"cycle": settings.CycleFiles, "visual session": settings.VisualSessionFiles} {
		if err := RequireUsable(root, files...); err == nil || !strings.Contains(err.Error(), "3 is taken") {
			t.Fatalf("the %s starts on a slot a worker takes: %v", who, err)
		}
	}
}
