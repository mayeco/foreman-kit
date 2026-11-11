package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestBaseVerificationsRecordOnceAcceptAndResolve(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenAt(filepath.Join(dir, "state.db"), "ws-base", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if v, err := st.GetBaseVerification(ctx, "repos/core", "aaa", "regression"); err != nil || v != nil {
		t.Fatalf("expected no verdict yet, got %+v (err=%v)", v, err)
	}
	failed := BaseVerification{RepoPath: "repos/core", BaseSHA: "aaa", CheckName: "regression", Status: BaseVerdictFailed, ExitCode: 1, Excerpt: "boom", PointerSHA: "ppp", OutsideCommits: 2, CreatedAt: "2026-11-09T10:00:00Z"}
	if inserted, err := st.RecordBaseVerification(ctx, failed); err != nil || !inserted {
		t.Fatalf("expected the first verdict inserted, got %v (err=%v)", inserted, err)
	}
	// A verdict is computed once: a second one for the same (repo, sha, check) is ignored.
	again := failed
	again.Status = BaseVerdictPassed
	if inserted, err := st.RecordBaseVerification(ctx, again); err != nil || inserted {
		t.Fatalf("expected the second verdict ignored, got %v (err=%v)", inserted, err)
	}
	v, err := st.GetBaseVerification(ctx, "repos/core", "aaa", "regression")
	if err != nil || v == nil || v.Status != BaseVerdictFailed || v.ExitCode != 1 || v.Excerpt != "boom" || v.PointerSHA != "ppp" || v.OutsideCommits != 2 {
		t.Fatalf("unexpected verdict %+v (err=%v)", v, err)
	}
	if _, err := st.RecordBaseVerification(ctx, BaseVerification{RepoPath: "repos/core", BaseSHA: "aaa", CheckName: "gate:test", Status: BaseVerdictFailed, ExitCode: 1, CreatedAt: "2026-11-09T10:05:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordBaseVerification(ctx, BaseVerification{RepoPath: "repos/mobile", BaseSHA: "mmm", CheckName: "regression", Status: BaseVerdictPassed}); err != nil {
		t.Fatal(err)
	}

	broken, err := st.ListBrokenBases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []BrokenBase{{Repo: "repos/core", SHA: "aaa", Since: "2026-11-09T10:00:00Z", FailedChecks: []string{"regression", "gate:test"}, Pointer: "ppp", OutsideCommits: 2, Excerpt: "boom", LastFailedAt: "2026-11-09T10:05:00Z"}}
	if !reflect.DeepEqual(broken, want) {
		t.Fatalf("ListBrokenBases = %+v, want %+v", broken, want)
	}
	if got := broken[0].ChecksText() + " " + broken[0].ChecksVerb(); got != "regression and test gate fail" {
		t.Fatalf("ChecksText = %q", got)
	}
	if list, _ := st.ListBaseVerifications(ctx, "repos/core", "aaa"); len(list) != 2 || list[0].CheckName != "regression" {
		t.Fatalf("ListBaseVerifications = %+v", list)
	}

	// Accepting marks every failed verdict of that sha: the base is no longer broken.
	n, err := st.AcceptBaseVerifications(ctx, "repos/core", "aaa", "alice", time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC))
	if err != nil || n != 2 {
		t.Fatalf("expected 2 verdicts accepted, got %d (err=%v)", n, err)
	}
	v, _ = st.GetBaseVerification(ctx, "repos/core", "aaa", "gate:test")
	if v.Status != BaseVerdictAccepted || v.AcceptedBy != "alice" || v.AcceptedAt != "2026-11-09T11:00:00Z" {
		t.Fatalf("unexpected accepted verdict %+v", v)
	}
	if broken, _ := st.ListBrokenBases(ctx); len(broken) != 0 {
		t.Fatalf("an accepted base is not broken, got %+v", broken)
	}

	// A base the repository moved off is resolved, and can be reopened.
	if _, err := st.RecordBaseVerification(ctx, BaseVerification{RepoPath: "repos/core", BaseSHA: "bbb", CheckName: "regression", Status: BaseVerdictFailed, ExitCode: 3}); err != nil {
		t.Fatal(err)
	}
	if resolved, err := st.ResolveBrokenBases(ctx, "repos/core", "ccc", time.Now()); err != nil || !reflect.DeepEqual(resolved, []string{"bbb"}) {
		t.Fatalf("ResolveBrokenBases = %v (err=%v), want [bbb]", resolved, err)
	}
	if broken, _ := st.ListBrokenBases(ctx); len(broken) != 0 {
		t.Fatalf("a resolved base is not broken, got %+v", broken)
	}
	if resolved, _ := st.ResolveBrokenBases(ctx, "repos/core", "ccc", time.Now()); len(resolved) != 0 {
		t.Fatalf("nothing left to resolve, got %v", resolved)
	}
	if err := st.ReopenBrokenBase(ctx, "repos/core", "bbb"); err != nil {
		t.Fatal(err)
	}
	if broken, _ := st.ListBrokenBases(ctx); len(broken) != 1 || broken[0].SHA != "bbb" || broken[0].ChecksText() != "regression" || broken[0].ChecksVerb() != "fails" {
		t.Fatalf("expected bbb broken again, got %+v", broken)
	}
}

// A failed verdict replaces a passed one (a gate rerun with another change's files found the base
// broken); nothing replaces a failed or an accepted one.
func TestBaseVerificationFailedReplacesOnlyPassed(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenAt(filepath.Join(dir, "state.db"), "ws-base-replace", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	passed := BaseVerification{RepoPath: "repos/core", BaseSHA: "aaa", CheckName: "gate:test", Status: BaseVerdictPassed, CreatedAt: "2026-11-09T10:00:00Z"}
	if inserted, err := st.RecordBaseVerification(ctx, passed); err != nil || !inserted {
		t.Fatalf("expected the passed verdict inserted, got %v (err=%v)", inserted, err)
	}
	failed := BaseVerification{RepoPath: "repos/core", BaseSHA: "aaa", CheckName: "gate:test", Status: BaseVerdictFailed, ExitCode: 1, Excerpt: "boom", CreatedAt: "2026-11-09T11:00:00Z"}
	if written, err := st.RecordBaseVerification(ctx, failed); err != nil || !written {
		t.Fatalf("expected the failed verdict to replace the passed one, got %v (err=%v)", written, err)
	}
	if written, err := st.RecordBaseVerification(ctx, passed); err != nil || written {
		t.Fatalf("a passed verdict must not replace a failed one, got %v (err=%v)", written, err)
	}
	if v, _ := st.GetBaseVerification(ctx, "repos/core", "aaa", "gate:test"); v == nil || v.Status != BaseVerdictFailed || v.Excerpt != "boom" || v.CreatedAt != "2026-11-09T11:00:00Z" {
		t.Fatalf("unexpected verdict %+v", v)
	}
	if n, err := st.AcceptBaseVerifications(ctx, "repos/core", "aaa", "alice", time.Now()); err != nil || n != 1 {
		t.Fatalf("accept: n=%d err=%v", n, err)
	}
	if written, err := st.RecordBaseVerification(ctx, failed); err != nil || written {
		t.Fatalf("a failed verdict must not replace an accepted one, got %v (err=%v)", written, err)
	}
	if v, _ := st.GetBaseVerification(ctx, "repos/core", "aaa", "gate:test"); v == nil || v.Status != BaseVerdictAccepted {
		t.Fatalf("expected the verdict to stay accepted, got %+v", v)
	}
}

// A broken base has one automatic recheck: it deletes the base's failed verdicts (so they are computed
// again) and is never spent twice; passed and accepted verdicts stay.
func TestRecheckBrokenBaseOnce(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenAt(filepath.Join(dir, "state.db"), "ws-base-recheck", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	for _, v := range []BaseVerification{
		{RepoPath: "repos/core", BaseSHA: "aaa", CheckName: "regression", Status: BaseVerdictFailed, ExitCode: 1, CreatedAt: "2026-11-09T10:00:00Z"},
		{RepoPath: "repos/core", BaseSHA: "aaa", CheckName: "gate:test", Status: BaseVerdictFailed, ExitCode: 1, CreatedAt: "2026-11-09T10:30:00Z"},
		{RepoPath: "repos/core", BaseSHA: "aaa", CheckName: "gate:lint", Status: BaseVerdictPassed, CreatedAt: "2026-11-09T10:30:00Z"},
		{RepoPath: "repos/core", BaseSHA: "bbb", CheckName: "regression", Status: BaseVerdictFailed, ExitCode: 1, CreatedAt: "2026-11-09T09:00:00Z"},
	} {
		if _, err := st.RecordBaseVerification(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	bases, err := st.ListBrokenBases(ctx)
	if err != nil || len(bases) != 2 {
		t.Fatalf("expected two broken bases, got %+v (err=%v)", bases, err)
	}
	for _, b := range bases {
		if b.SHA == "aaa" && (b.LastFailedAt != "2026-11-09T10:30:00Z" || b.RecheckedAt != "") {
			t.Fatalf("unexpected broken base %+v", b)
		}
	}

	ok, err := st.RecheckBrokenBase(ctx, "repos/core", "aaa", time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	if err != nil || !ok {
		t.Fatalf("expected the first recheck to be spent, got %v (err=%v)", ok, err)
	}
	verdicts, _ := st.ListBaseVerifications(ctx, "repos/core", "aaa")
	if len(verdicts) != 1 || verdicts[0].CheckName != "gate:lint" {
		t.Fatalf("expected only the passed verdict left on aaa, got %+v", verdicts)
	}
	if at, _ := st.BaseRecheckedAt(ctx, "repos/core", "aaa"); at != "2026-11-09T12:00:00Z" {
		t.Fatalf("BaseRecheckedAt = %q", at)
	}
	// A failed verdict computed again after the recheck is listed with the recheck it already had.
	if _, err := st.RecordBaseVerification(ctx, BaseVerification{RepoPath: "repos/core", BaseSHA: "aaa", CheckName: "regression", Status: BaseVerdictFailed, ExitCode: 1, CreatedAt: "2026-11-09T12:05:00Z"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RecheckBrokenBase(ctx, "repos/core", "aaa", time.Now()); err != nil || ok {
		t.Fatalf("a base must be rechecked automatically once, got %v (err=%v)", ok, err)
	}
	bases, _ = st.ListBrokenBases(ctx)
	for _, b := range bases {
		if b.SHA == "aaa" && b.RecheckedAt != "2026-11-09T12:00:00Z" {
			t.Fatalf("expected the broken base to carry its recheck, got %+v", b)
		}
	}
	if v, _ := st.GetBaseVerification(ctx, "repos/core", "bbb", "regression"); v == nil || v.Status != BaseVerdictFailed {
		t.Fatalf("another base's verdict changed: %+v", v)
	}
}
