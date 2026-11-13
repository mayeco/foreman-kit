package portal

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mayeco/foreman-kit/internal/store"
)

// answer is what an error gets over HTTP: a status, and a code a client can act on without reading
// the message.
type answer struct {
	status  int
	code    string
	message string
}

// answerOf returns the answer of an error: 404, 400, 409, 403, 405 or 429 for a problem, 400 for a
// page cursor that no page handed out, and 500 for anything else.
func answerOf(err error) answer {
	var p *problem
	switch {
	case errors.As(err, &p):
		switch p.kind {
		case notFoundProblem:
			return answer{http.StatusNotFound, "not_found", sentence(p.message, p.keepFirst)}
		case conflictProblem:
			return answer{http.StatusConflict, "conflict", sentence(p.message, p.keepFirst)}
		case forbiddenProblem:
			return answer{http.StatusForbidden, "forbidden", sentence(p.message, p.keepFirst)}
		case methodProblem:
			return answer{http.StatusMethodNotAllowed, "method_not_allowed", sentence(p.message, p.keepFirst)}
		case rateLimitedProblem:
			return answer{http.StatusTooManyRequests, "rate_limited", sentence(p.message, p.keepFirst)}
		}
		return answer{http.StatusBadRequest, "invalid_input", sentence(p.message, p.keepFirst)}
	case errors.Is(err, store.ErrBadCursor):
		return answer{http.StatusBadRequest, "invalid_input", "The page cursor is not valid."}
	}
	return answer{http.StatusInternalServerError, "internal", err.Error()}
}

// sentence returns a problem as a person reads it: a service words it the way Go words an error,
// without a capital or a full stop. The first rune is upper-cased only when the first word is made
// of letters (ignoring one trailing ",", ":" or ";") and keepFirst does not say it is an
// interpolated value: setting keys, change names, paths and other identifiers keep their case.
func sentence(message string, keepFirst bool) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return message
	}
	if !keepFirst && plainWord(message) {
		first, size := utf8.DecodeRuneInString(message)
		message = string(unicode.ToUpper(first)) + message[size:]
	}
	if last, _ := utf8.DecodeLastRuneInString(message); !strings.ContainsRune(".!?:", last) {
		message += "."
	}
	return message
}

// plainWord says whether the first word of a message holds only letters, ignoring one trailing
// ",", ":" or ";".
func plainWord(message string) bool {
	word := message
	if i := strings.IndexFunc(message, unicode.IsSpace); i >= 0 {
		word = message[:i]
	}
	if n := len(word); n > 1 && strings.ContainsRune(",:;", rune(word[n-1])) {
		word = word[:n-1]
	}
	for _, r := range word {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return word != ""
}

// fail answers a request of the API with an error: {"error": {"code", "message"}}. A failure of the
// portal itself (500) also goes to the log.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	a := answerOf(err)
	if a.status >= http.StatusInternalServerError {
		log.Printf("portal: %s %s: %v", r.Method, r.URL.Path, err)
	}
	writeJSON(w, a.status, map[string]any{"error": map[string]string{"code": a.code, "message": a.message}})
}

// decodeInput reads the body of an action into T. The body is JSON, declared as such, with the
// fields of T and no other; an action that takes no field may come with no body at all. Nothing an
// action takes is read from the address: what a form of another site can send without asking
// (a query, a body that is not declared as JSON) never reaches it.
func decodeInput[T any](r *http.Request) (T, error) {
	var in T
	if r.Body == nil {
		return in, nil
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		if errors.Is(err, io.EOF) {
			return in, nil
		}
		var zero T
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return zero, errInvalid("the request body is larger than %d bytes", tooLarge.Limit)
		}
		return zero, errInvalid("the request body is not the JSON this action takes: %v", err)
	}
	var zero T
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return zero, errInvalid("the request body must be declared as Content-Type: application/json")
	}
	if dec.More() {
		return zero, errInvalid("the request body holds more than one JSON value")
	}
	return in, nil
}

// started is the answer of an action that launched an execution: 202, with the execution it created
// and the address of its page. What the execution does is read there, not in this answer.
type started struct {
	ExecID    string          `json:"exec_id"`
	Href      string          `json:"href"`
	Execution store.Execution `json:"execution"`
}

func writeStarted(w http.ResponseWriter, ex store.Execution) {
	writeJSON(w, http.StatusAccepted, started{ExecID: ex.ExecID, Href: executionHref(ex.ExecID), Execution: ex})
}

// executionHref is the address of the page of an execution.
func executionHref(execID string) string { return "/executions/" + url.PathEscape(execID) }
