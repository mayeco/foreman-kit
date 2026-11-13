package portal

import (
	"net/http"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// TestAnErrorKeepsTheIdentifierItOpensWith: an API error that opens with a setting key or a change
// name carries it byte for byte, never with its first letter upper-cased, and still ends with a full
// stop.
func TestAnErrorKeepsTheIdentifierItOpensWith(t *testing.T) {
	srv, st, root := setupTestPortal(t)
	seedOverview(t, srv, st, root)

	if code, _, raw, _ := doReq(t, srv, http.MethodPost, "/api/backlog", map[string]any{"change_name": "probe", "description": "A probe"}); code != http.StatusCreated {
		t.Fatalf("adding probe = %d %s", code, raw)
	}

	for _, tc := range []struct {
		why, method, path string
		in                map[string]any
		status            int
		code, word        string
	}{
		{"a value the key does not accept", http.MethodPost, "/api/system/settings",
			map[string]any{"key": settings.QueueMaxOpen.Path(), "value": "0"}, http.StatusBadRequest, "invalid_input", settings.QueueMaxOpen.Path()},
		{"a key that is not a setting", http.MethodPost, "/api/system/settings",
			map[string]any{"key": "nope.x", "value": "1"}, http.StatusBadRequest, "invalid_input", "nope.x"},
		{"a change that is already queued", http.MethodPost, "/api/backlog",
			map[string]any{"change_name": "add-ui", "description": "Again"}, http.StatusConflict, "conflict", "add-ui"},
		{"a single-word change that is already queued", http.MethodPost, "/api/backlog",
			map[string]any{"change_name": "probe", "description": "Again"}, http.StatusConflict, "conflict", "probe"},
		{"a change the kit does not know", http.MethodGet, "/api/changes/no-such-change",
			nil, http.StatusNotFound, "not_found", "no-such-change"},
	} {
		var body any
		if tc.in != nil {
			body = tc.in
		}
		code, data, raw, _ := doReq(t, srv, tc.method, tc.path, body)
		c, msg := errorOf(data)
		if code != tc.status || c != tc.code {
			t.Errorf("%s: %s %s = %d %s; want %d %s", tc.why, tc.method, tc.path, code, raw, tc.status, tc.code)
			continue
		}
		first, size := utf8.DecodeRuneInString(tc.word)
		capitalized := string(unicode.ToUpper(first)) + tc.word[size:]
		if !strings.Contains(msg, tc.word) || strings.Contains(msg, capitalized) {
			t.Errorf("%s: the message %q does not carry %q verbatim", tc.why, msg, tc.word)
		}
		if !strings.HasSuffix(msg, ".") {
			t.Errorf("%s: the message %q does not end with a full stop", tc.why, msg)
		}
	}
}
