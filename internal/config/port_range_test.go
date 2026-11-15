package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// TestTheDaemonPortOutsideTCPIsRefused: config set refuses a portal.port above 65535 (and writes
// nothing, also with --dry-run), accepts the last port, and config check reports a file that
// already holds one.
func TestTheDaemonPortOutsideTCPIsRefused(t *testing.T) {
	root, _ := setupTestUmbrella(t)
	file := filepath.Join(root, filepath.FromSlash(settings.DaemonPort.FilePath()))
	if _, err := os.Stat(file); err != nil {
		t.Skipf("the fixture has no %s: %v", settings.DaemonPort.FilePath(), err)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, dryRun := range []bool{false, true} {
		for _, value := range []string{"70000", "65536", "0"} {
			var out bytes.Buffer
			err := Set(root, "daemon.portal.port", value, dryRun, &out)
			var refusal *Refusal
			if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "a whole number from 1 to 65535") {
				t.Errorf("Set(%s, dry-run %v) returned %v (%T), want a *Refusal naming the range", value, dryRun, err, err)
			}
		}
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatalf("a refused set changed the file:\n%s", after)
	}

	var out bytes.Buffer
	if err := Set(root, "daemon.portal.port", "65535", false, &out); err != nil {
		t.Fatalf("Set 65535: %v", err)
	}
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), "port: 65535") {
		t.Fatalf("Set 65535 wrote:\n%s", raw)
	}
	if err := os.WriteFile(file, []byte(strings.Replace(string(raw), "port: 65535", "port: 70000", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if rep := Check(root)[settings.DaemonPort.FilePath()]; !hasLine(rep.Errors, "portal.port", "1 to 65535") {
		t.Fatalf("check of portal.port: 70000: %v", rep.Errors)
	}
}
