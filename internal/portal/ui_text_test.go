package portal

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Truncated text stays valid UTF-8: event payloads and agent output are full of multi-byte runes.
func TestTruncationNeverSplitsAUTF8Sequence(t *testing.T) {
	ctx := eventContext(map[string]any{"x": strings.Repeat("é", 50)}, 21)
	if !utf8.ValidString(ctx) || !strings.HasSuffix(ctx, "…") || utf8.RuneCountInString(ctx) != 22 {
		t.Fatalf("eventContext = %q", ctx)
	}
	if got := truncateRunes("Canción de cuna", 6); got != "Canció…" {
		t.Fatalf("truncateRunes = %q", got)
	}
	if got := truncateRunes("short", 10); got != "short" {
		t.Fatalf("truncateRunes = %q", got)
	}
	if got := truncateRunes("anything", 0); got != "anything" {
		t.Fatalf("truncateRunes with no limit = %q", got)
	}
}

// Day labels count calendar days, also across a DST change (a 23-hour day).
func TestCalendarDaysBetweenAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no time zone data: %v", err)
	}
	now := time.Date(2026, 3, 9, 10, 0, 0, 0, ny) // the day after spring forward
	for _, c := range []struct {
		when time.Time
		want int
	}{
		{time.Date(2026, 3, 9, 1, 0, 0, 0, ny), 0},
		{time.Date(2026, 3, 8, 9, 0, 0, 0, ny), 1},
		{time.Date(2026, 3, 7, 23, 0, 0, 0, ny), 2},
		{time.Date(2026, 11, 1, 12, 0, 0, 0, ny), 1}, // fall back: a 25-hour day
	} {
		n := now
		if c.when.Month() == 11 {
			n = time.Date(2026, 11, 2, 9, 0, 0, 0, ny)
		}
		if got := calendarDaysBetween(c.when, n, ny); got != c.want {
			t.Fatalf("calendarDaysBetween(%s, %s) = %d, want %d", c.when, n, got, c.want)
		}
	}
}
