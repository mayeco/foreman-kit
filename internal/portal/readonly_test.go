package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// getPatternRe finds the GET routes the package declares: every one is a literal in its source.
var getPatternRe = regexp.MustCompile(`"GET (/[^"]*)"`)

// declaredGETRoutes reads the patterns of the GET routes from the source of the package.
func declaredGETRoutes(t *testing.T) []string {
	t.Helper()
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range getPatternRe.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]] = true
		}
	}
	var patterns []string
	for p := range seen {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	return patterns
}

// TestReadingAPageWritesNothing asks for every GET route the portal declares, over a database that
// holds something for each to show, and checks after each one that no row was written: a page or a
// read of the API shows what is on record and leaves it as it was. The mirror of the backlog is
// brought up to date by who edits the backlog, never by who looks at it.
//
// A route joins the table below when it is declared: one that is missing fails the test.
func TestReadingAPageWritesNothing(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seed := seedOverview(t, srv, st, root)
	ctx := context.Background()
	if _, err := st.Pause(ctx, "a pause to list", "portal", ""); err != nil {
		t.Fatal(err)
	}
	events, err := st.QueryEvents(ctx, store.EventFilter{})
	if err != nil || len(events) == 0 {
		t.Fatalf("the seed recorded no event to open: %v", err)
	}
	event := strconv.FormatInt(events[0].ID, 10)
	round := strconv.FormatInt(seed.failedRound, 10)

	// The address that asks each pattern for something that exists.
	addresses := map[string][]string{
		"/{$}":                                {"/"},
		"/":                                   {"/no-such-page"},
		"/go":                                 {"/go?q=add-api", "/go?q=nothing"},
		"/recent":                             {"/recent"},
		"/inbox":                              {"/inbox", "/inbox?kind=blocked"},
		"/board":                              {"/board", "/board?open=discarded,integrated"},
		"/changes":                            {"/changes", "/changes?state=building&q=add", "/changes?page=2"},
		"/changes/{name}":                     {"/changes/add-api", "/changes/add-billing", "/changes/add-api?limit=1"},
		"/changes/{name}/files":               {"/changes/add-search/files"},
		"/changes/{name}/files/{file...}":     {"/changes/add-search/files/specs/search/spec.md"},
		"/changes/{name}/events":              {"/changes/add-api/events"},
		"/changes/{name}/intervene":           {"/changes/add-search/intervene", "/changes/add-search/intervene?from=plan"},
		"/gates/{id}":                         {"/gates/" + round},
		"/executions":                         {"/executions", "/executions?status=running"},
		"/executions/new":                     {"/executions/new"},
		"/executions/{id}":                    {"/executions/build-1", "/executions/build-live", "/executions/run-1"},
		"/backlog":                            {"/backlog"},
		"/backlog/proposals":                  {"/backlog/proposals", "/backlog/proposals?status=rejected"},
		"/merge":                              {"/merge"},
		"/cycles":                             {"/cycles"},
		"/cycles/{stamp}":                     {"/cycles/20261108T120000Z"},
		"/visual":                             {"/visual", "/visual?status=pending"},
		"/visual/sessions":                    {"/visual/sessions"},
		"/visual/sessions/{stamp}":            {"/visual/sessions/20261108T120000Z"},
		"/events":                             {"/events", "/events?change=add-api"},
		"/events/{id}":                        {"/events/" + event},
		"/quality":                            {"/quality", "/quality?since=all"},
		"/agents":                             {"/agents", "/agents?show=all&since=24h"},
		"/system":                             {"/system"},
		"/system/health":                      {"/system/health"},
		"/system/settings":                    {"/system/settings", "/system/settings?file=evaluator", "/system/settings?edit=pipeline.dispatch.workers"},
		"/system/recommendations":             {"/system/recommendations", "/system/recommendations?window=30d"},
		"/system/setup":                       {"/system/setup"},
		"/system/prompts":                     {"/system/prompts"},
		"/system/prompts/{role}":              {"/system/prompts/plan", "/system/prompts/build?view=sample", "/system/prompts/cycle"},
		"/system/processes":                   {"/system/processes", "/system/processes?limit=2"},
		"/system/appearance":                  {"/system/appearance"},
		"/assets/":                            {"/assets/no-such-file"},
		"/assets/portal.css":                  {"/assets/portal.css"},
		"/assets/portal.js":                   {"/assets/portal.js"},
		"/assets/icons.svg":                   {"/assets/icons.svg"},
		"/assets/favicon.svg":                 {"/assets/favicon.svg"},
		"/assets/agents/{name}":               {"/assets/agents/claude.svg"},
		"/assets/fonts/{name}":                {"/assets/fonts/ibm-plex-sans-latin-400-normal.woff2"},
		"/api/dashboard":                      {"/api/dashboard"},
		"/api/inbox":                          {"/api/inbox"},
		"/api/board":                          {"/api/board"},
		"/api/changes":                        {"/api/changes"},
		"/api/changes/{name}":                 {"/api/changes/add-api"},
		"/api/changes/{name}/events":          {"/api/changes/add-api/events"},
		"/api/changes/{name}/files/{file...}": {"/api/changes/add-search/files/proposal.md"},
		"/api/gates/{id}":                     {"/api/gates/" + round},
		"/api/executions":                     {"/api/executions"},
		"/api/executions/{id}":                {"/api/executions/build-1"},
		"/api/executions/{id}/log":            {"/api/executions/build-1/log", "/api/executions/build-1/log?format=text"},
		"/api/executions/{id}/log/stream":     {"/api/executions/build-1/log/stream"},
		"/api/system/processes":               {"/api/system/processes"},
		"/api/agents":                         {"/api/agents", "/api/agents?since=all"},
		"/api/cycles":                         {"/api/cycles"},
		"/api/cycles/{stamp}":                 {"/api/cycles/20261108T120000Z"},
		"/api/events":                         {"/api/events", "/api/events?change=add-api"},
		"/api/events/{id}":                    {"/api/events/" + event},
		"/api/quality":                        {"/api/quality", "/api/quality?since=24h"},
		"/api/backlog":                        {"/api/backlog"},
		"/api/merge":                          {"/api/merge"},
		"/api/system/settings":                {"/api/system/settings", "/api/system/settings?file=agents", "/api/system/settings?edit=pipeline.dispatch.workers"},
		"/api/system/recommendations":         {"/api/system/recommendations", "/api/system/recommendations?window=7d"},
		"/api/system/setup":                   {"/api/system/setup"},
		"/api/visual":                         {"/api/visual", "/api/visual?status=verified"},
		"/api/visual/sessions":                {"/api/visual/sessions"},
		"/api/visual/sessions/{stamp}":        {"/api/visual/sessions/20261108T120000Z"},
		"/api/system/prompts":                 {"/api/system/prompts"},
		"/api/system/prompts/{role}":          {"/api/system/prompts/plan", "/api/system/prompts/cycle?view=sample"},
		"/api/system/health":                  {"/api/system/health"},
	}
	// The addresses that answer something other than 200 on purpose.
	answers := map[string]int{"/no-such-page": http.StatusNotFound, "/assets/no-such-file": http.StatusNotFound,
		"/go?q=add-api": http.StatusSeeOther, "/go?q=nothing": http.StatusSeeOther, "/system": http.StatusSeeOther,
		// No visual QA session left a file: the page of one says it is not found.
		"/visual/sessions/20261108T120000Z": http.StatusNotFound, "/api/visual/sessions/20261108T120000Z": http.StatusNotFound}
	// A page reads from one snapshot, which holds one connection of the store for as long as the page
	// is read. With a single connection to read with, a page that asks the store for something
	// outside its snapshot waits for the connection it is holding: a few such pages asked for at once
	// would wait for one another in a portal that serves. Here it is asked for alone, and does not
	// answer.
	st.ReadDB().SetMaxOpenConns(1)
	get := func(address string) int {
		// A stream stays open for as long as its reader does: this one leaves after a moment.
		stream := strings.Contains(address, "/stream")
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if stream {
			reqCtx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
			defer cancel()
		}
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4567"+address, nil).WithContext(reqCtx)
		req.Host = "127.0.0.1:4567"
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if !stream && reqCtx.Err() != nil {
			t.Errorf("GET %s did not answer in 5 seconds with one connection to read the database with: it asks the store for a second one while its snapshot holds the first", address)
		}
		return rec.Code
	}

	declared := declaredGETRoutes(t)
	if len(declared) < 60 {
		t.Fatalf("only %d GET routes were found in the source of the package: the test no longer reads them", len(declared))
	}
	asked := 0
	for _, pattern := range declared {
		list, ok := addresses[pattern]
		if !ok {
			t.Errorf("the GET route %s has no address in this test: add one that asks it for something that exists", pattern)
			continue
		}
		for _, address := range list {
			before, err := st.RowsWritten(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := http.StatusOK
			if code, ok := answers[address]; ok {
				want = code
			}
			if code := get(address); code != want {
				t.Errorf("GET %s = %d, want %d", address, code, want)
			}
			after, err := st.RowsWritten(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Errorf("GET %s wrote %d row(s) to the database", address, after-before)
			}
			asked++
		}
	}
	for pattern := range addresses {
		if i := sort.SearchStrings(declared, pattern); i >= len(declared) || declared[i] != pattern {
			t.Errorf("this test has an address for %s, which no GET route declares any more: take it off", pattern)
		}
	}
	t.Logf("asked for %d addresses over %d GET routes", asked, len(declared))
}
