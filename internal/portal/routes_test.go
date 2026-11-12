package portal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// TestEveryRouteIsDeclaredWithItsMethod: the API is routed by the same mux as the pages, so a route
// answers the methods it declares and no other, an address nobody declared is not found, and the
// name in an address reaches its handler whole.
func TestEveryRouteIsDeclaredWithItsMethod(t *testing.T) {
	srv, _, _ := setupTestPortal(t)

	// A route of the API with a method it does not take: 405, with the methods it does take.
	code, body, _, hdr := doReq(t, srv, http.MethodPost, "/api/dashboard", map[string]any{})
	if c, msg := errorOf(body); code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "GET" || c != "method_not_allowed" || !strings.Contains(msg, "GET") {
		t.Errorf("POST /api/dashboard = %d, Allow %q, %v; want 405 with Allow: GET", code, hdr.Get("Allow"), body)
	}
	code, _, _, hdr = doReq(t, srv, http.MethodGet, "/api/workspace/pause", nil)
	if code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "POST" {
		t.Errorf("GET /api/workspace/pause = %d, Allow %q; want 405 with Allow: POST", code, hdr.Get("Allow"))
	}
	code, _, _, hdr = doReq(t, srv, http.MethodDelete, "/api/backlog", nil)
	if code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "GET, POST" {
		t.Errorf("DELETE /api/backlog = %d, Allow %q; want 405 with Allow: GET, POST", code, hdr.Get("Allow"))
	}
	// A method the API has no route for at all still answers through apiNoRoute: JSON 405 with the
	// exact Allow header on a known address, and JSON 404 without Allow on an unknown one.
	code, body, _, hdr = doReq(t, srv, http.MethodPut, "/api/dashboard", map[string]any{})
	if c, msg := errorOf(body); code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "GET" || c != "method_not_allowed" || !strings.Contains(msg, "GET") || !strings.Contains(hdr.Get("Content-Type"), "application/json") {
		t.Errorf("PUT /api/dashboard = %d, Allow %q, %v (%s); want JSON 405 with Allow: GET", code, hdr.Get("Allow"), body, hdr.Get("Content-Type"))
	}
	for _, tc := range []struct {
		path      string
		wantCode  int
		wantErr   string
		wantAllow string
	}{
		{path: "/api/dashboard", wantCode: http.StatusMethodNotAllowed, wantErr: "method_not_allowed", wantAllow: "GET"},
		{path: "/api/system/settings", wantCode: http.StatusMethodNotAllowed, wantErr: "method_not_allowed", wantAllow: "GET, POST"},
		{path: "/api/no-such-thing", wantCode: http.StatusNotFound, wantErr: "not_found", wantAllow: ""},
	} {
		for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodOptions} {
			var payload any
			if method != http.MethodOptions {
				payload = map[string]any{}
			}
			code, body, _, hdr := doReq(t, srv, method, tc.path, payload)
			c, _ := errorOf(body)
			if code != tc.wantCode || c != tc.wantErr || hdr.Get("Allow") != tc.wantAllow || !strings.Contains(hdr.Get("Content-Type"), "application/json") {
				t.Errorf("%s %s = %d, code %q, Allow %q, Content-Type %q; want %d, code %q, Allow %q, application/json",
					method, tc.path, code, c, hdr.Get("Allow"), hdr.Get("Content-Type"), tc.wantCode, tc.wantErr, tc.wantAllow)
			}
			if tc.wantAllow == "" && len(hdr.Values("Allow")) > 0 {
				t.Errorf("%s %s set an empty Allow header %v; want no Allow header", method, tc.path, hdr.Values("Allow"))
			}
		}
	}

	// An address of the API nobody declared: 404, as JSON, with any method.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		var payload any
		if method != http.MethodGet {
			payload = map[string]any{}
		}
		for _, path := range []string{"/api", "/api/no-such-thing", "/api/changes/a/b/c", "/api/proposals/x", "/api/proposals/some-change/maybe",
			"/api/no-such/thing", "/api/nothing"} {
			code, body, _, hdr := doReq(t, srv, method, path, payload)
			if c, msg := errorOf(body); code != http.StatusNotFound || c != "not_found" || !strings.Contains(msg, path) || !strings.Contains(hdr.Get("Content-Type"), "application/json") {
				t.Errorf("%s %s = %d %v (%s); want 404 as JSON", method, path, code, body, hdr.Get("Content-Type"))
			}
		}
	}

	// The API takes its addresses with a trailing slash too; a page redirects to its one address.
	if code, body, _, _ := doReq(t, srv, http.MethodGet, "/api/dashboard/", nil); code != http.StatusOK || body["counts"] == nil {
		t.Errorf("GET /api/dashboard/ = %d %v; want what the kit is doing", code, body)
	}
	if code, _, _, hdr := doReq(t, srv, http.MethodGet, "/changes/", nil); code != http.StatusMovedPermanently || hdr.Get("Location") != "/changes" {
		t.Errorf("GET /changes/ = %d to %q; want 301 to /changes", code, hdr.Get("Location"))
	}

	// The name in the address reaches the handler as one segment, decoded.
	code, body, _, _ = doReq(t, srv, http.MethodGet, "/api/changes/no-such-change", nil)
	if c, msg := errorOf(body); code != http.StatusNotFound || c != "not_found" || !strings.Contains(msg, "no-such-change") {
		t.Errorf("GET /api/changes/no-such-change = %d %v", code, body)
	}
	code, body, _, _ = doReq(t, srv, http.MethodGet, "/api/changes/a%2Fb", nil)
	if _, msg := errorOf(body); code != http.StatusNotFound || !strings.Contains(msg, "a/b") {
		t.Errorf("GET /api/changes/a%%2Fb = %d %v; want the name a/b, one segment, decoded", code, body)
	}

	// Assets: what the portal has, and nothing else. An asset that does not exist is not a page.
	if code, _, raw, _ := doReq(t, srv, http.MethodGet, "/assets/no-such-file.js", nil); code != http.StatusNotFound || strings.Contains(raw, "<html") {
		t.Errorf("GET /assets/no-such-file.js = %d; want a plain 404", code)
	}
	if code, _, _, _ := doReq(t, srv, http.MethodGet, "/assets/fonts/x.ttf", nil); code != http.StatusNotFound {
		t.Errorf("GET /assets/fonts/x.ttf = %d, want 404", code)
	}
	if code, _, _, _ := doReq(t, srv, http.MethodGet, "/assets/agents/..%2F..%2Fserver.go", nil); code != http.StatusNotFound {
		t.Errorf("an agent logo outside the logos = %d, want 404", code)
	}

	// Every answer says its type is not to be guessed.
	for _, path := range []string{"/", "/api/dashboard", "/assets/portal.css", "/no-such-page", "/api/no-such-thing"} {
		if _, _, _, hdr := doReq(t, srv, http.MethodGet, path, nil); hdr.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s has no X-Content-Type-Options: nosniff", path)
		}
	}
}

// TestNoPatternOfTheRouterConflicts builds the router: http.ServeMux panics when two patterns
// could take the same request and neither is the more specific one.
func TestNoPatternOfTheRouterConflicts(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the router does not build: %v", r)
		}
	}()
	if srv.router() == nil {
		t.Fatal("no router")
	}
}

// TestEveryAPIRouteUsesAnAPIMethod: apiNoRoute probes only apiMethods and ServeHTTP sends any
// other method on /api/ to apiNoRoute before the mux, so a route registered under /api/ with a
// method outside apiMethods would be unreachable and missing from Allow.
func TestEveryAPIRouteUsesAnAPIMethod(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	unlisted, total := unlistedAPIRoutes(t, srv.router())
	if total == 0 {
		t.Fatal("no /api/ routes were read from the router")
	}
	for _, pat := range unlisted {
		t.Errorf("route %q is under /api/ with a method not in apiMethods (%v)", pat, apiMethods)
	}

	// Verify the check catches a route registered with a method outside apiMethods.
	tmp := http.NewServeMux()
	tmp.HandleFunc("GET /api/ok", func(http.ResponseWriter, *http.Request) {})
	tmp.HandleFunc("PUT /api/x", func(http.ResponseWriter, *http.Request) {})
	got, _ := unlistedAPIRoutes(t, tmp)
	if len(got) != 1 || got[0] != "PUT /api/x" {
		t.Errorf("unlistedAPIRoutes with PUT /api/x = %v, want [PUT /api/x]", got)
	}
}

// unlistedAPIRoutes returns every pattern registered on mux under /api/ whose method is not in
// apiMethods, along with the total number of /api/ patterns inspected.
func unlistedAPIRoutes(t *testing.T, mux *http.ServeMux) (unlisted []string, total int) {
	t.Helper()
	allowed := map[string]bool{}
	for _, m := range apiMethods {
		allowed[m] = true
	}
	idx := reflect.ValueOf(mux).Elem().FieldByName("index")
	seen := map[string]bool{}
	collect := func(pat reflect.Value) {
		p := pat.Elem().FieldByName("str").String()
		seen[p] = true
	}
	multis := idx.FieldByName("multis")
	for i := 0; i < multis.Len(); i++ {
		collect(multis.Index(i))
	}
	segments := idx.FieldByName("segments")
	iter := segments.MapRange()
	for iter.Next() {
		list := iter.Value()
		for i := 0; i < list.Len(); i++ {
			collect(list.Index(i))
		}
	}
	var patterns []string
	for p := range seen {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	for _, p := range patterns {
		method, path, hasMethod := strings.Cut(p, " ")
		if !hasMethod {
			method, path = "", p
		}
		path = strings.TrimSpace(path)
		if path != "/api" && !strings.HasPrefix(path, "/api/") {
			continue
		}
		total++
		if !allowed[method] {
			unlisted = append(unlisted, p)
		}
	}
	return unlisted, total
}

// TestARequestBodyIsBounded: an action reads a few fields. A body larger than the limit is not read
// to the end, so the action runs without the fields it could not read and refuses the request.
func TestARequestBodyIsBounded(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	huge := `{"change_name":"` + strings.Repeat("a", maxRequestBody) + `"}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4567/api/backlog", strings.NewReader(huge))
	req.Host, req.RemoteAddr = "127.0.0.1:4567", "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a body over the limit = %d %s; want it refused", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_input") {
		t.Errorf("a body over the limit is refused without saying it is the request that is wrong: %s", rec.Body.String())
	}
	if items := backlogItems(t, srv.root); len(items) != 0 {
		t.Fatalf("a body over the limit added to the backlog: %v", items)
	}
}

// TestAnErrorSaysWhatToAnswer: an error carries its answer (404, 400, 409), a cursor no page handed
// out is a bad request, and anything else is a failure of the portal (500).
func TestAnErrorSaysWhatToAnswer(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{errNotFound("no change named %s", "x"), http.StatusNotFound, "not_found"},
		{errInvalid("the priority %q is not one", "urgent"), http.StatusBadRequest, "invalid_input"},
		{errConflict("%s is not blocked", "x"), http.StatusConflict, "conflict"},
		{fmt.Errorf("listing the story: %w", store.ErrBadCursor), http.StatusBadRequest, "invalid_input"},
		{fmt.Errorf("reading: %w", errNotFound("gone")), http.StatusNotFound, "not_found"},
		{errors.New("database is locked"), http.StatusInternalServerError, "internal"},
	} {
		got := answerOf(tc.err)
		if got.status != tc.status || got.code != tc.code || got.message == "" {
			t.Errorf("answerOf(%v) = %d %s %q; want %d %s", tc.err, got.status, got.code, got.message, tc.status, tc.code)
		}
	}
	if got := errNotFound("no change named %s", "x").Error(); got != "no change named x" {
		t.Errorf("the message of an error = %q", got)
	}

	// A page that asks for what does not exist answers 404 with the page of errors.
	srv, _, _ := setupTestPortal(t)
	code, _, raw, _ := doReq(t, srv, http.MethodGet, "/changes/no-such-change", nil)
	if code != http.StatusNotFound || !strings.Contains(raw, "<title>Not found") {
		t.Errorf("GET /changes/no-such-change = %d; want the page of errors with 404", code)
	}
}

// TestARefusedRequestSaysWhyInTheShapeOfTheAPI: what the portal does not take from who sent it (a
// host it does not answer to, an action from another site or another origin, an action whose body
// is a form) is answered 403 the way every other error of the API is: a code a client can act on,
// and a sentence that says why.
func TestARefusedRequestSaysWhyInTheShapeOfTheAPI(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	for name, tc := range map[string]struct {
		method, host, contentType string
		header                    map[string]string
	}{
		"a host the portal does not answer to": {method: http.MethodGet, host: "evil.example:4567"},
		"an action from another site":          {method: http.MethodPost, contentType: "application/json", header: map[string]string{"Sec-Fetch-Site": "cross-site"}},
		"an action from another origin":        {method: http.MethodPost, contentType: "application/json", header: map[string]string{"Origin": "http://evil.example"}},
		"an action from no origin at all":      {method: http.MethodPost, contentType: "application/json", header: map[string]string{"Origin": "null"}},
		"an action whose body is a form":       {method: http.MethodPost, contentType: "application/x-www-form-urlencoded"},
	} {
		address := "/api/dashboard"
		var body io.Reader
		if tc.method == http.MethodPost {
			address, body = "/api/workspace/pause", strings.NewReader(`{"reason":"from elsewhere"}`)
		}
		req := httptest.NewRequest(tc.method, "http://127.0.0.1:4567"+address, body)
		req.Host, req.RemoteAddr = "127.0.0.1:4567", "127.0.0.1:12345"
		if tc.host != "" {
			req.Host = tc.host
		}
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var answer struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusForbidden || answer.Error.Code != "forbidden" || answer.Error.Message == "" {
			t.Errorf("%s: %s %s = %d %s; want 403 with the code forbidden and why", name, tc.method, address, rec.Code, rec.Body.String())
		}
	}
	// Nothing was paused by a request the portal refused.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/dashboard", nil)
	if waiting, _ := data["waiting"].(map[string]any); code != http.StatusOK || waiting["paused"] != false {
		t.Errorf("a refused action paused the kit: %d %s", code, raw)
	}
}

// TestThePagesAboutThePortalItself: the search box leads to the change it names or to the search for
// it, Appearance sets the theme of the browser beside one the portal offers, and an address the
// portal does not have gets the page of an error with its status.
func TestThePagesAboutThePortalItself(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)

	for address, want := range map[string]string{
		"/go":                 "/changes",
		"/go?q=":              "/changes",
		"/go?q=add-api":       "/changes/add-api",
		"/go?q=no+such+thing": "/changes?q=no+such+thing",
		"/system":             "/system/health",
	} {
		if code, _, hdr := getPage(t, srv, address); code != http.StatusSeeOther || hdr.Get("Location") != want {
			t.Errorf("GET %s = %d to %q, want 303 to %q", address, code, hdr.Get("Location"), want)
		}
	}

	// The theme of the kit is set beside the light one, any other beside the kit's, and the address
	// names another to compare with.
	compared := func(body, theme string) bool {
		return strings.Contains(body, `aria-label="Portal preview in the `+theme+` theme"`)
	}
	_, body, _ := getPage(t, srv, "/system/appearance")
	if !compared(body, defaultTheme) || !compared(body, "light") {
		t.Errorf("Appearance does not set the theme of the kit beside the light one")
	}
	_, body, _ = getPage(t, srv, "/system/appearance", &http.Cookie{Name: themeCookie, Value: "light"})
	if !compared(body, "light") || !compared(body, defaultTheme) {
		t.Errorf("Appearance does not set a chosen theme beside the one of the kit")
	}
	_, body, _ = getPage(t, srv, "/system/appearance?compare=dracula")
	if !compared(body, "dracula") {
		t.Errorf("Appearance does not compare with the theme its address names")
	}
	if code, _, _ := getPage(t, srv, "/system/appearance?compare=no-such-theme"); code != http.StatusBadRequest {
		t.Errorf("GET /system/appearance?compare=no-such-theme = %d, want 400", code)
	}

	if code, body, _ := getPage(t, srv, "/recent"); code != http.StatusOK || !strings.Contains(body, "data-recent-list") {
		t.Errorf("GET /recent = %d, without the list the script fills", code)
	}

	// An address the portal does not have: the page of an error, with its status and the address.
	code, body, _ := getPage(t, srv, "/no-such-page")
	if text := textOf(body); code != http.StatusNotFound || !strings.Contains(text, "404") || !strings.Contains(text, "Signal lost") || !strings.Contains(text, "/no-such-page") {
		t.Errorf("GET /no-such-page = %d %s", code, text)
	}
}

// TestAnAnswerThatIsNotJSONIsAFailure: what cannot be written as JSON is answered 500 in the shape
// of every error of the API, not as an empty answer under the status of a success.
func TestAnAnswerThatIsNotJSONIsAFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"what": make(chan int)})
	var answer struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusInternalServerError || answer.Error.Code != "internal" || answer.Error.Message == "" {
		t.Errorf("an answer that is not JSON = %d %s", rec.Code, rec.Body.String())
	}
}
