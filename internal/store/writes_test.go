package store

import (
	"context"
	"testing"
)

// TestRowsWrittenMovesOnlyWhenSomethingIsWritten: reading through a View leaves the count where it
// was, and a write moves it.
func TestRowsWrittenMovesOnlyWhenSomethingIsWritten(t *testing.T) {
	w := seedPortalWorld(t)
	ctx := context.Background()
	before, err := w.st.RowsWritten(ctx)
	if err != nil || before == 0 {
		t.Fatalf("RowsWritten after seeding = %d, %v", before, err)
	}
	view(t, w.st, func(v *View) error {
		if _, err := v.Dashboard(ctx, 0); err != nil {
			return err
		}
		_, err := v.ListInbox(ctx)
		return err
	})
	if after, err := w.st.RowsWritten(ctx); err != nil || after != before {
		t.Fatalf("reading moved RowsWritten from %d to %d (%v)", before, after, err)
	}
	if _, err := w.st.Resume(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if after, err := w.st.RowsWritten(ctx); err != nil || after <= before {
		t.Fatalf("a write left RowsWritten at %d, from %d (%v)", after, before, err)
	}
}
