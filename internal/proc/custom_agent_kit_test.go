package proc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/config"
)

// TestKitVerificationDoesNotAskForTheSkillsOfACustomAgent: the preflight's check of kit.lock compares
// the agents the kit installs skills for, so an agent it does not ship is no drift.
func TestKitVerificationDoesNotAskForTheSkillsOfACustomAgent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	claude := []string{"claude"}
	custom := map[string]any{"run": "myagent {{args}} {{prompt}}", "ping": "myagent --version"}
	data := map[string]any{"commands": map[string]any{"myagent": custom}}
	raw, _ := json.Marshal(data)
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "agents.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(root, []string{"claude", "myagent"}, map[string][]string{"plan": claude, "build": claude, "cycle": claude}, ""); err != nil {
		t.Fatal(err)
	}
	for lockAgents, want := range map[string]bool{"claude": true, "claude,myagent": true, "codex": false} {
		lock, _ := json.Marshal(map[string]any{"version": "1.0.0", "agents": strings.Split(lockAgents, ","), "files": map[string]any{}})
		if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "kit.lock"), lock, 0o644); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if got := VerifyKitInstallation(root, &out); got != want {
			t.Errorf("a lock for %s: verified %v, want %v\n%s", lockAgents, got, want, out.String())
		}
	}
}
