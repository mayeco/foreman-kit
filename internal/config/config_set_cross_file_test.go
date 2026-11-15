package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestSetRefusesAValueThatBreaksAnotherFile: raising the workers hands the slot of the visual session
// to a worker. `config set` refuses it, names the file that would break and the key, and leaves the
// files as they were, as `config check` would reject what it wrote.
func TestSetRefusesAValueThatBreaksAnotherFile(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{
		"pipeline":  "dispatch:\n  workers: 2\n",
		"visual_qa": "slot: 3\n",
	})
	pipeline := filepath.Join(root, ".foreman-kit", "pipeline.yaml")
	visual := filepath.Join(root, ".foreman-kit", "visual_qa.yaml")
	pipelineBefore, _ := os.ReadFile(pipeline)
	visualBefore, _ := os.ReadFile(visual)
	var out bytes.Buffer

	for _, workers := range []string{"4", "3"} {
		for _, dryRun := range []bool{false, true} {
			err := Set(root, "pipeline.dispatch.workers", workers, dryRun, &out)
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("Set workers %s (dry run %v) = %v (%T), want a *Refusal", workers, dryRun, err, err)
			}
			for _, want := range []string{"pipeline.dispatch.workers = " + workers, ".foreman-kit/visual_qa.yaml", "slot", "3 is taken"} {
				if !strings.Contains(refusal.Reason, want) {
					t.Errorf("the refusal of workers %s lacks %q: %s", workers, want, refusal.Reason)
				}
			}
			if strings.Contains(refusal.Reason, ".foreman-kit/pipeline.yaml") {
				t.Errorf("the refusal blames the file that was written, not the one that would break: %s", refusal.Reason)
			}
		}
	}
	if got, _ := os.ReadFile(pipeline); string(got) != string(pipelineBefore) {
		t.Errorf("a refused set changed pipeline.yaml:\n%s", got)
	}
	if got, _ := os.ReadFile(visual); string(got) != string(visualBefore) {
		t.Errorf("a refused set changed visual_qa.yaml:\n%s", got)
	}

	// What leaves the files valid is still made, whichever way it goes.
	out.Reset()
	if err := Set(root, "pipeline.dispatch.workers", "2", false, &out); err != nil {
		t.Fatalf("Set workers 2 with slot 3: %v", err)
	}
	if err := Set(root, "pipeline.dispatch.workers", "1", false, &out); err != nil {
		t.Fatalf("Set workers 1 with slot 3: %v", err)
	}
	if got := pipelineString(root, "dispatch.workers"); got != "1" {
		t.Errorf("workers after Set 1 = %q", got)
	}
	for file, rep := range Check(root) {
		if len(rep.Errors) > 0 && (strings.Contains(file, "pipeline.yaml") || strings.Contains(file, "visual_qa.yaml")) {
			t.Errorf("check of %s after the sets: %v", file, rep.Errors)
		}
	}
}

// TestSetDoesNotBlameAChangeForWhatAnotherFileAlreadyLacked: a file that was already invalid with the
// value the set leaves is its own problem, said by check: the set that does not bring it is made.
func TestSetDoesNotBlameAChangeForWhatAnotherFileAlreadyLacked(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{
		"pipeline":  "dispatch:\n  workers: 2\n",
		"visual_qa": "slot: 2\n",
	})
	var out bytes.Buffer
	// slot 2 is already taken by the 2 workers: that stays the file's own problem, which 3 workers
	// do not make worse, and 1 worker mends.
	if err := Set(root, "pipeline.dispatch.workers", "3", false, &out); err != nil {
		t.Fatalf("Set workers 3 brings no problem slot 2 did not have: %v", err)
	}
	if err := Set(root, "pipeline.dispatch.workers", "1", false, &out); err != nil {
		t.Fatalf("Set workers 1 frees slot 2: %v", err)
	}
}

// TestSetKeepsMakingAChangeWhoseOtherFilesAreMissingOrBroken: the files the set does not write are
// judged only when they can be read.
func TestSetKeepsMakingAChangeWhoseOtherFilesAreMissingOrBroken(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{"pipeline": "dispatch:\n  workers: 2\n"})
	if err := os.Remove(filepath.Join(root, ".foreman-kit", "visual_qa.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "daemon.yaml"), []byte("portal:\n   broken: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Set(root, "pipeline.dispatch.workers", "4", false, &out); err != nil {
		t.Fatalf("Set workers 4 with a missing and an unreadable file: %v", err)
	}
	if got := pipelineString(root, "dispatch.workers"); got != "4" {
		t.Errorf("workers = %q, want 4", got)
	}
}

// TestAValueGivenToARunThatBreaksAFileItDoesNotNameIsSaid: --set workers=4 makes the visual session's
// slot 3, written in visual_qa.yaml, a worker's. The run stops before it starts anything, and the
// message names the file.
func TestAValueGivenToARunThatBreaksAFileItDoesNotNameIsSaid(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, map[string]string{
		"pipeline":  "dispatch:\n  workers: 2\n",
		"visual_qa": "slot: 3\n",
	})
	settingstest.Give(t, settings.Workers, "3")
	err := CheckRunOverrides(root)
	if err == nil {
		t.Fatal("--set workers=3 with slot 3 written in visual_qa.yaml was accepted")
	}
	for _, want := range []string{"--set pipeline.dispatch.workers=3", ".foreman-kit/visual_qa.yaml invalid: slot: 3 is taken"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message lacks %q: %v", want, err)
		}
	}

	settingstest.Give(t, settings.Workers, "2")
	if err := CheckRunOverrides(root); err != nil {
		t.Errorf("--set workers=2 leaves slot 3 free: %v", err)
	}
}
