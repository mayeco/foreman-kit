package portal

import (
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var (
	// hrefRe finds where the links of a page lead inside the portal.
	hrefRe = regexp.MustCompile(`href="(/[^"#]*)`)
	// getFormRe finds the forms of a page, and formActionRe the address a form asks.
	getFormRe    = regexp.MustCompile(`<form\b[^>]*>`)
	formActionRe = regexp.MustCompile(`\baction="(/[^"#?]*)`)
)

// TestEveryPageIsReachedFromDashboard: a person who starts at Dashboard and follows the links and
// the search forms of each page reaches every page the portal declares. The menu changes with each
// step of the redesign: a page that only an address typed by hand opens is a page nobody finds, and
// this test names it.
func TestEveryPageIsReachedFromDashboard(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)

	// The pages nothing leads to, each with why.
	unlinked := map[string]string{
		"/":                        "the answer to an address that does not exist",
		"/healthz":                 "a probe for programs, not a page",
		"/visual/sessions/{stamp}": "the seed leaves no file of a visual QA session to open",
		"/system/recommendations":  "the tab exists once a dispatcher has run for a day, which the seed has not",
	}

	patternOf := func(address string) string {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4567"+address, nil)
		_, pattern := srv.router().Handler(req)
		return strings.TrimPrefix(pattern, "GET ")
	}
	inside := func(address string) bool {
		return address != "" && !strings.HasPrefix(address, "/api/") && !strings.HasPrefix(address, "/assets/")
	}

	reached := map[string]bool{}
	seen := map[string]bool{"/": true}
	// A page with filters links to itself once for each of them: a few addresses of one page are
	// enough to find where it leads.
	const addressesPerPage = 8
	perPage := map[string]int{}
	queue := []string{"/"}
	asked := 0
	for len(queue) > 0 {
		address := queue[0]
		queue = queue[1:]
		reached[patternOf(address)] = true
		code, body, header := getPage(t, srv, address)
		asked++
		if code >= 500 {
			t.Errorf("GET %s = %d, reached by following links from Dashboard", address, code)
		}
		var leads []string
		for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
			leads = append(leads, html.UnescapeString(m[1]))
		}
		if code >= 300 && code < 400 {
			leads = append(leads, header.Get("Location"))
		}
		for _, to := range leads {
			if !inside(to) || seen[to] {
				continue
			}
			page, _, _ := strings.Cut(to, "?")
			if perPage[page] >= addressesPerPage {
				continue
			}
			perPage[page]++
			seen[to] = true
			queue = append(queue, to)
		}
		// A form that asks with GET leads to its address too: the search box, a filter.
		for _, form := range getFormRe.FindAllString(body, -1) {
			if !strings.Contains(strings.ToLower(form), `method="get"`) {
				continue
			}
			if m := formActionRe.FindStringSubmatch(form); m != nil && inside(m[1]) {
				reached[patternOf(m[1])] = true
			}
		}
		if asked > 5000 {
			t.Fatalf("following links from Dashboard did not end after %d pages", asked)
		}
	}

	pages := 0
	declared := map[string]bool{}
	for _, pattern := range declaredGETRoutes(t) {
		declared[pattern] = true
		if !inside(pattern) {
			continue
		}
		pages++
		reason, known := unlinked[pattern]
		switch {
		case !reached[pattern] && !known:
			t.Errorf("nothing leads to %s from Dashboard: a person cannot find that page", pattern)
		case reached[pattern] && known:
			t.Errorf("something leads to %s now: take it off the pages nothing leads to (%s)", pattern, reason)
		}
	}
	for pattern := range unlinked {
		if !declared[pattern] {
			t.Errorf("%s is listed as a page nothing leads to, and no route declares it any more", pattern)
		}
	}
	t.Logf("followed links through %d addresses, over %d pages declared", asked, pages)
}
