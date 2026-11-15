package orchestrator

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// unreadableBacklogSandbox returns a dispatcher whose log goes to the returned buffer, the file of its
// backlog, and a reader of the backlog.unreadable events recorded so far (their `error` payloads).
func unreadableBacklogSandbox(t *testing.T) (*Dispatcher, *bytes.Buffer, string, func() []string) {
	t.Helper()
	ctx := context.Background()
	root, _, _, st := setupTestWorkspaceWithSubmodule(t)
	var out bytes.Buffer
	d, err := NewDispatcher(DispatcherOptions{Root: root, Store: st, MaxWorkers: 1, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	events := func() []string {
		evs, err := st.QueryEvents(ctx, store.EventFilter{Event: "backlog.unreadable"})
		if err != nil {
			t.Fatal(err)
		}
		var causes []string
		for _, ev := range evs {
			c, _ := ev.Payload["error"].(string)
			causes = append(causes, c)
		}
		return causes
	}
	return d, &out, filepath.Join(root, ".foreman-kit", "agent_backlog.json"), events
}

func unreadableLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "BACKLOG UNREADABLE") {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestUnreadableBacklogNamesItsCause: loadBacklog reported every broken backlog with the nil error of
// the file read that had succeeded, so the log said "BACKLOG UNREADABLE: <file>: ; ..." and the
// backlog.unreadable event carried error="". The log line and the event name the parse error with its
// offset, or the wrong shape.
func TestUnreadableBacklogNamesItsCause(t *testing.T) {
	ctx := context.Background()
	d, out, file, events := unreadableBacklogSandbox(t)

	cases := []struct {
		content string
		want    string
	}{
		{`[{"change_name": "x",, }]`, "invalid character ',' looking for beginning of object key string (at byte 22)"},
		{`{"items": [{"change_name": "y"}]}`, "is not a JSON list of changes"},
		{`[{"change_name": "z" `, "unexpected end of JSON input (at byte 21)"},
	}
	for i, c := range cases {
		mustWriteFile(t, file, c.content)
		d.loadBacklog(ctx, false)
		lines := unreadableLines(out.String())
		if len(lines) != i+1 {
			t.Fatalf("after %q: %d BACKLOG UNREADABLE line(s), want %d:\n%s", c.content, len(lines), i+1, out.String())
		}
		last := lines[len(lines)-1]
		if !strings.Contains(last, ": "+c.want+"; nothing new starts from it") {
			t.Errorf("after %q the log line does not name the cause %q:\n%s", c.content, c.want, last)
		}
		if strings.Contains(last, "valid JSON") {
			t.Errorf("after %q the log line still says valid JSON:\n%s", c.content, last)
		}
		got := events()
		if len(got) != i+1 || got[len(got)-1] != c.want {
			t.Errorf("after %q the backlog.unreadable events are %q, want the last to be %q", c.content, got, c.want)
		}
	}
}

// TestTheSameUnreadableBacklogIsReportedOnce: the same breakage, loaded again and again by the loop,
// is one log line and one event.
func TestTheSameUnreadableBacklogIsReportedOnce(t *testing.T) {
	ctx := context.Background()
	d, out, file, events := unreadableBacklogSandbox(t)
	mustWriteFile(t, file, `[{"change_name": "x",, }]`)
	for i := 0; i < 4; i++ {
		d.loadBacklog(ctx, false)
	}
	if n := len(unreadableLines(out.String())); n != 1 {
		t.Errorf("%d BACKLOG UNREADABLE lines for one breakage, want 1:\n%s", n, out.String())
	}
	if n := len(events()); n != 1 {
		t.Errorf("%d backlog.unreadable events for one breakage, want 1", n)
	}
}

// TestABacklogBrokenAgainIsReportedAgain: a long-lived daemon (Serve) kept one dispatcher, whose reports
// were never forgotten, so only the first breakage of its lifetime was logged and recorded.
func TestABacklogBrokenAgainIsReportedAgain(t *testing.T) {
	ctx := context.Background()
	d, out, file, events := unreadableBacklogSandbox(t)
	broken := `[{"change_name": "x",, }]`

	mustWriteFile(t, file, broken)
	d.loadBacklog(ctx, false)
	mustWriteFile(t, file, `[]`)
	d.loadBacklog(ctx, false)
	mustWriteFile(t, file, broken)
	d.loadBacklog(ctx, false)
	d.loadBacklog(ctx, false)

	if n := len(unreadableLines(out.String())); n != 2 {
		t.Errorf("broken, fixed, broken again logged %d time(s), want 2:\n%s", n, out.String())
	}
	got := events()
	if len(got) != 2 || got[0] == "" || got[0] != got[1] {
		t.Errorf("broken, fixed, broken again recorded %q, want the same cause twice", got)
	}
}
