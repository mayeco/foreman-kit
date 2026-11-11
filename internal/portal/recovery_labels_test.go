package portal

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPortalPagesNameNoLegacyLogFiles: the portal labelled its panels and empty states with files the
// kit no longer writes (log/events.jsonl, log/blocked_retries.json, log/PAUSE ...): its state is in
// SQLite and the runtime directory. A person looking for "log/blocked_retries.json" finds nothing.
func TestPortalPagesNameNoLegacyLogFiles(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedPortal(t, srv, st, root)
	legacy := regexp.MustCompile(`log/(events\.jsonl|blocked|blocked_retries\.json|rebuilds|deferrals\.json|dispatch_results\.json|PAUSE|workers)`)
	for _, p := range []string{"/", "/events", "/system/processes", "/system/health", "/changes", "/changes/feat-alpha", "/changes/feat-alpha/events", "/changes/feat-alpha/files", "/changes/blocked-gamma", "/changes/ready-delta", "/executions"} {
		code, body, _ := getPage(t, srv, p)
		if code != 200 {
			t.Errorf("%s = %d", p, code)
			continue
		}
		if m := legacy.FindString(body); m != "" {
			t.Errorf("%s names the legacy file %q", p, m)
		}
	}
}

// TestTabListsAdaptWithoutScrolling: a change page has up to four tabs and the system pages six. As
// flex items they shrank to fit a narrow window, their icon wrapped above the label inside a tab of
// fixed height and the label was clipped; the first fix made the list scroll sideways and hid the last
// tabs. A tab keeps its width and the list adapts: rows wrap, and on a narrow window the inactive tabs
// of a change page show only their icon (their label stays for screen readers).
func TestTabListsAdaptWithoutScrolling(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedPortal(t, srv, st, root)
	list := regexp.MustCompile(`<div role="tablist" class="([^"]*)"`)
	tab := regexp.MustCompile(`<a role="tab"[^>]*class="([^"]*)"`)
	for _, p := range []string{"/changes/feat-alpha", "/system/health", "/changes", "/changes/feat-alpha/events"} {
		_, body, _ := getPage(t, srv, p)
		lists := list.FindAllStringSubmatch(body, -1)
		if len(lists) == 0 {
			t.Errorf("%s has no tab list", p)
		}
		for _, m := range lists {
			if strings.Contains(m[1], "overflow-x-auto") || strings.Contains(m[1], "flex-nowrap") {
				t.Errorf("%s: tab list %q scrolls sideways instead of adapting", p, m[1])
			}
		}
		for _, m := range tab.FindAllStringSubmatch(body, -1) {
			classes := " " + m[1] + " "
			if !strings.Contains(classes, " shrink-0 ") || !strings.Contains(classes, " flex-nowrap ") {
				t.Errorf("%s: tab class %q can shrink or wrap its icon", p, m[1])
			}
			if strings.Contains(classes, " tab-active ") != strings.Contains(classes, " text-base-content ") ||
				!strings.Contains(classes, " tab-active ") && !strings.Contains(classes, " text-base-content/80 ") {
				t.Errorf("%s: tab class %q: an inactive tab needs text-base-content/80 (daisyUI's 50%% was 4.4:1, under WCAG AA), the active one full text-base-content", p, m[1])
			}
		}
	}
	_, body, _ := getPage(t, srv, "/changes/feat-alpha")
	if !strings.Contains(body, `<span class="max-sm:sr-only">Files</span>`) || !strings.Contains(body, `<span class="max-sm:sr-only">Events</span>`) {
		t.Error("an inactive tab of a change page does not collapse to its icon on a narrow window")
	}
	if !regexp.MustCompile(`aria-selected="true"[^>]*><svg[^>]*>.*?</svg><span>Story</span>`).MatchString(body) {
		t.Error("the active tab of a change page must keep its label")
	}
}

// TestViewerSizeFollowsItsContent: viewers fed a document (the prompts) passed no size, and said
// "0 B" over a full page of text.
func TestViewerSizeFollowsItsContent(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedPortal(t, srv, st, root)
	for _, p := range []string{"/system/prompts/plan", "/system/prompts/build?view=sample"} {
		code, body, _ := getPage(t, srv, p)
		if code != 200 {
			t.Fatalf("%s = %d", p, code)
		}
		if strings.Contains(body, `data-v="info">0 B`) {
			t.Errorf("%s has a viewer with content that says 0 B", p)
		}
	}
}

// TestFormsKeepTheirFieldsApart: forms were laid out with gap-1 (or only a horizontal gap), so a field and the
// button under it touched on a narrow window.
func TestFormsKeepTheirFieldsApart(t *testing.T) {
	forms, err := filepath.Glob(filepath.Join("..", "assets", "portal_ui", "templates", "*.html"))
	if err != nil || len(forms) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	form := regexp.MustCompile(`<form class="([^"]*)"`)
	for _, f := range forms {
		raw, _ := os.ReadFile(f)
		for _, m := range form.FindAllStringSubmatch(string(raw), -1) {
			classes := " " + m[1] + " "
			if !strings.Contains(classes, " gap-3 ") && !strings.Contains(classes, " gap-4 ") && !strings.Contains(classes, " gap-5 ") && !strings.Contains(classes, " gap-6 ") {
				t.Errorf("%s: form class %q leaves fields and buttons (almost) touching: use gap-3 or more", filepath.Base(f), m[1])
			}
		}
	}
}
