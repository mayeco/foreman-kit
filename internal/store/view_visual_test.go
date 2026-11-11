package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestViewReadsTheChecksInABrowserAndTheSessionInProgress: the checks come the latest first, a page
// at a time, whole or in one status, with the cycle that asked for each and when a session took it;
// their count by status has an entry for every status; and the session in progress is the lock of
// visual QA while its time has not run out.
func TestViewReadsTheChecksInABrowserAndTheSessionInProgress(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const stamp = "20261108T120000Z"
	must(st.BeginCycle(ctx, Cycle{Stamp: stamp}))

	// Five checks: a cycle asked for the first, a session took the second, the third and the fourth
	// ended, and the fifth waits.
	var ids []string
	for i, item := range []VisualQAItem{
		{What: "the list of widgets shows their names", ChangeName: "add-ui"},
		{What: "the search page shows results", ChangeName: "add-search"},
		{What: "the invoice prints on one page"},
		{What: "the export downloads a file", ChangeName: "add-export"},
		{What: "the menu closes with Escape"},
	} {
		at := ctx
		if i == 0 {
			at = WithCycle(ctx, stamp)
		}
		id, err := st.EnqueueVisualQANext(at, item)
		must(err)
		ids = append(ids, id)
	}
	must(st.StartVisualQA(ctx, ids[1], "session 20261110T090000Z"))
	must(st.ResolveVisualQA(ctx, ids[2], "verified", "one page, as asked"))
	must(st.ResolveVisualQA(ctx, ids[3], "regression", "the file is empty"))

	view(t, st, func(v *View) error {
		first, err := v.ListVisualQAPage(ctx, "", "", 2)
		if err != nil {
			return err
		}
		if len(first.Items) != 2 || first.Items[0].ItemID != ids[4] || first.Items[1].ItemID != ids[3] || first.Next != ids[3] {
			t.Fatalf("the first page of checks = %+v", first)
		}
		if c := first.Items[1]; c.Status != "regression" || c.VerdictNote != "the file is empty" || c.ChangeName != "add-export" || c.ResolvedAt == "" || c.CycleStamp != "" {
			t.Errorf("a check that found a regression = %+v", c)
		}
		var rest []VisualQACheck
		for cursor := first.Next; cursor != ""; {
			page, err := v.ListVisualQAPage(ctx, "", cursor, 2)
			if err != nil {
				return err
			}
			rest = append(rest, page.Items...)
			cursor = page.Next
		}
		if len(rest) != 3 || rest[0].ItemID != ids[2] || rest[1].ItemID != ids[1] || rest[2].ItemID != ids[0] {
			t.Fatalf("the pages after the first hold %+v", rest)
		}
		if c := rest[1]; c.Status != "in_progress" || c.StartedAt == "" || c.ResolvedAt != "" || c.VerdictNote != "session 20261110T090000Z" {
			t.Errorf("a check a session took = %+v", c)
		}
		if c := rest[2]; c.Status != "pending" || c.CycleStamp != stamp || c.What != "the list of widgets shows their names" || c.CreatedAt == "" {
			t.Errorf("a check a cycle asked for = %+v", c)
		}

		pending, err := v.ListVisualQAPage(ctx, "pending", "", 0)
		if err != nil {
			return err
		}
		if len(pending.Items) != 2 || pending.Items[0].ItemID != ids[4] || pending.Items[1].ItemID != ids[0] || pending.Next != "" {
			t.Errorf("the checks that wait = %+v", pending)
		}
		if none, err := v.ListVisualQAPage(ctx, "dropped", "", 0); err != nil || len(none.Items) != 0 || none.Items == nil {
			t.Errorf("the checks that were dropped = %+v, %v", none, err)
		}
		if _, err := v.ListVisualQAPage(ctx, "", "not-a-cursor", 0); !errors.Is(err, ErrBadCursor) {
			t.Errorf("a cursor no page handed out = %v, want ErrBadCursor", err)
		}

		// One check, by its id.
		one, err := v.VisualQACheck(ctx, ids[3])
		if err != nil || one == nil || *one != first.Items[1] {
			t.Errorf("the check %s read by its id = %+v (%v), and the list gives %+v", ids[3], one, err, first.Items[1])
		}
		for _, id := range []string{"999", "0", "-3", "x", ""} {
			if none, err := v.VisualQACheck(ctx, id); err != nil || none != nil {
				t.Errorf("the check %q = %+v, %v, want none", id, none, err)
			}
		}

		counts, err := v.CountVisualQAByStatus(ctx)
		if err != nil {
			return err
		}
		want := map[string]int{"pending": 2, "in_progress": 1, "verified": 1, "regression": 1, "not-visual": 0, "cannot-verify": 0, "dropped": 0}
		if fmt.Sprint(counts) != fmt.Sprint(want) || len(counts) != len(VisualQAStatuses) {
			t.Errorf("the checks by status = %v, want %v", counts, want)
		}

		if session, err := v.VisualSession(ctx); err != nil || session != nil {
			t.Errorf("the session in progress with no lock = %+v, %v", session, err)
		}
		return nil
	})

	// A session opens: it holds the lock of visual QA until it ends or its time runs out.
	_, err := st.AcquireLock(ctx, "visual_qa", LockOptions{OwnerType: "visual_qa", PID: os.Getpid(), Stamp: "20261110T090000Z", TTL: 90 * time.Minute})
	must(err)
	view(t, st, func(v *View) error {
		session, err := v.VisualSession(ctx)
		if err != nil || session == nil {
			t.Fatalf("the session in progress = %+v, %v", session, err)
		}
		started, errStart := time.Parse(time.RFC3339, session.StartedAt)
		expires, errEnd := time.Parse(time.RFC3339, session.ExpiresAt)
		if session.Stamp != "20261110T090000Z" || session.PID != os.Getpid() || errStart != nil || errEnd != nil || expires.Sub(started) != 90*time.Minute {
			t.Errorf("the session in progress = %+v", session)
		}
		return nil
	})
	// A session whose time ran out is not open, though its lock is still on record.
	if _, err := st.DB().ExecContext(ctx, `UPDATE locks SET acquired_at = acquired_at - ?, heartbeat_at = heartbeat_at - ? WHERE lock_key = 'visual_qa'`,
		(91 * time.Minute).Milliseconds(), (91 * time.Minute).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	view(t, st, func(v *View) error {
		if session, err := v.VisualSession(ctx); err != nil || session != nil {
			t.Errorf("a session whose time ran out = %+v, %v", session, err)
		}
		return nil
	})
}
