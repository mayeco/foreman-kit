package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

func TestCobraCLIBaseStatusAndAccept(t *testing.T) {
	tmp := t.TempDir()
	appDir := filepath.Join(tmp, "sample-app")
	t.Setenv("FOREMANKIT_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Setenv("FOREMANKIT_APP_ROOT", appDir)
	t.Setenv("USER", "carol")
	ctx := context.Background()

	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errBuf bytes.Buffer
		code := Run(ctx, args, &out, &errBuf)
		return code, out.String(), errBuf.String()
	}
	if code, out, errOut := run("init", appDir, "--agents", "claude"); code != 0 {
		t.Fatalf("init failed (%d): %s %s", code, out, errOut)
	}

	if code, out, _ := run("base", "status"); code != 0 || !strings.Contains(out, "no broken base") {
		t.Fatalf("expected no broken base, got (%d) %s", code, out)
	}

	st, err := store.Open(appDir)
	if err != nil {
		t.Fatal(err)
	}
	sha := "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	for _, check := range []string{"regression", "gate:test"} {
		if _, err := st.RecordBaseVerification(ctx, store.BaseVerification{RepoPath: "repos/core", BaseSHA: sha, CheckName: check, Status: store.BaseVerdictFailed, ExitCode: 1, Excerpt: "rspec: 3 failures", PointerSHA: "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432", OutsideCommits: 2, CreatedAt: "2026-11-09T10:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()

	code, out, errOut := run("base", "status")
	if code != 0 {
		t.Fatalf("base status failed (%d): %s", code, errOut)
	}
	for _, want := range []string{"repos/core", "base:            " + sha, "since:           2026-11-09T10:00:00Z", "fails alone:     regression, gate:test", "outside commits: 2 (9f8e7d6c..1a2b3c4d)", "holding:         none", "foreman-kit base accept repos/core 1a2b3c4d", "rspec: 3 failures"} {
		if !strings.Contains(out, want) {
			t.Fatalf("base status lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = run("base", "status", "--json")
	var parsed struct {
		BrokenBases []map[string]any `json:"broken_bases"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &parsed) != nil || len(parsed.BrokenBases) != 1 || parsed.BrokenBases[0]["sha"] != sha || parsed.BrokenBases[0]["outside_commits"] != float64(2) {
		t.Fatalf("unexpected base status --json (%d):\n%s", code, out)
	}
	if code, out, _ := run("status"); code != 0 || !strings.Contains(out, "BASE BROKEN: repos/core at 1a2b3c4d since 2026-11-09T10:00:00Z: regression and test gate fail on the base alone (2 outside commit(s)); holding: none; foreman-kit base accept repos/core 1a2b3c4d to integrate anyway") {
		t.Fatalf("expected the BASE BROKEN line in status (%d):\n%s", code, out)
	}
	if code, out, _ := run("progress"); code != 0 || !strings.Contains(out, "repos/core's base at 1a2b3c4d fails its regression and test gate on its own") {
		t.Fatalf("expected the broken base under Needs a human (%d):\n%s", code, out)
	}

	if code, _, errOut := run("base", "accept"); code != 2 || !strings.Contains(errOut, "usage") {
		t.Fatalf("expected a usage error, got (%d) %s", code, errOut)
	}
	if code, _, errOut := run("base", "accept", "repos/core", "ffff"); code != 1 || !strings.Contains(errOut, "no failed verdict at ffff") {
		t.Fatalf("expected an unknown sha refused, got (%d) %s", code, errOut)
	}
	code, out, errOut = run("base", "accept", "repos/core")
	if code != 0 || !strings.Contains(out, "accepted: repos/core at 1a2b3c4d (2 verdict(s): regression, gate:test)") {
		t.Fatalf("base accept failed (%d): %s %s", code, out, errOut)
	}
	if code, out, _ := run("base", "status"); code != 0 || !strings.Contains(out, "no broken base") {
		t.Fatalf("expected no broken base after accept, got (%d) %s", code, out)
	}
	if code, _, errOut := run("base", "accept", "repos/core"); code != 1 || !strings.Contains(errOut, "repos/core has no broken base") {
		t.Fatalf("expected a second accept refused, got (%d) %s", code, errOut)
	}

	st, err = store.Open(appDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v, _ := st.GetBaseVerification(ctx, "repos/core", sha, "gate:test")
	if v == nil || v.Status != store.BaseVerdictAccepted || v.AcceptedBy != "carol" || v.AcceptedAt == "" {
		t.Fatalf("expected the verdict accepted by carol, got %+v", v)
	}
	evs, _ := st.QueryEvents(ctx, store.EventFilter{Event: "merge.base_accepted"})
	if len(evs) != 1 || evs[0].Payload["by"] != "carol" || evs[0].Payload["sha"] != sha {
		t.Fatalf("expected one merge.base_accepted by carol, got %+v", evs)
	}
}
