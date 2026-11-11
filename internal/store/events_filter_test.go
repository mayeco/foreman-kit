package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestQueryEventsNewestAndAfterID verifies that Limit with Newest returns the newest events (callers
// such as the daemon watcher and the portal used to get the oldest ones), that AfterID is an
// incremental cursor, and that MaxEventID reports the newest id.
func TestQueryEventsNewestAndAfterID(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenAt(filepath.Join(dir, "state.db"), "ws-events", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := st.EmitEvent(ctx, EventInput{Event: "test.event"}); err != nil {
			t.Fatal(err)
		}
	}
	newest, err := st.QueryEvents(ctx, EventFilter{Limit: 2, Newest: true})
	if err != nil || len(newest) != 2 || newest[0].ID != 4 || newest[1].ID != 5 {
		t.Fatalf("expected the newest two events (4, 5) in ascending order, got %+v (err=%v)", newest, err)
	}
	oldest, _ := st.QueryEvents(ctx, EventFilter{Limit: 2})
	if len(oldest) != 2 || oldest[0].ID != 1 {
		t.Fatalf("expected Limit without Newest to keep returning the oldest events, got %+v", oldest)
	}
	after, _ := st.QueryEvents(ctx, EventFilter{AfterID: 3})
	if len(after) != 2 || after[0].ID != 4 {
		t.Fatalf("expected AfterID 3 to return events 4 and 5, got %+v", after)
	}
	if id, err := st.MaxEventID(ctx); err != nil || id != 5 {
		t.Fatalf("MaxEventID = %d (err=%v), want 5", id, err)
	}
}
