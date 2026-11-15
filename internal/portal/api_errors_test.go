package portal

import (
	"net/http"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/mayeco/foreman-kit/internal/config"
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
		{"a single-word key that is not a setting", http.MethodPost, "/api/system/settings",
			map[string]any{"key": "probe", "value": "1"}, http.StatusBadRequest, "invalid_input", "probe is not a setting"},
		{"a single-word key of a dry run", http.MethodPost, "/api/system/settings",
			map[string]any{"key": "workers", "value": "1", "dry_run": true}, http.StatusBadRequest, "invalid_input", "workers is not a setting"},
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

// A refusal of config that opens with a word of its own still reads as a sentence, and one that
// opens with the key keeps it as config wrote it.
func TestARefusalOfConfigReadsAsASentenceUnlessItOpensWithTheKey(t *testing.T) {
	for _, tc := range []struct {
		refusal *config.Refusal
		want    string
	}{
		{&config.Refusal{Reason: "probe is not a setting; foreman-kit config show lists them", OpensWithValue: true}, "probe is not a setting; foreman-kit config show lists them."},
		{&config.Refusal{Reason: "cannot set pipeline.dispatch.workers in .foreman-kit/pipeline.yaml (x); edit it by hand"}, "Cannot set pipeline.dispatch.workers in .foreman-kit/pipeline.yaml (x); edit it by hand."},
	} {
		a := answerOf(refusalProblem(tc.refusal))
		if a.status != http.StatusBadRequest || a.code != "invalid_input" || a.message != tc.want {
			t.Errorf("answerOf(%+v) = %+v, want %q", tc.refusal, a, tc.want)
		}
	}
}
