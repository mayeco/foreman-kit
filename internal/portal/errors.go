package portal

import (
	"fmt"
	"strings"
)

// A problem is an error the one who asked can do something about: what was asked for does not exist,
// the request cannot be understood, the state of things does not allow it now, or the portal does
// not take it from who sent it or with the method it came with. A service returns one without
// knowing how it is answered; the handlers map it to a status (respond.go). An error that is not a
// problem is a failure of the portal itself.
type problem struct {
	kind    problemKind
	message string
	// keepFirst says that the message opens with a value the service interpolated (its format
	// starts with %s or %v and goes on): an identifier whose case must reach the client unchanged.
	keepFirst bool
}

type problemKind int

const (
	notFoundProblem problemKind = iota + 1
	invalidProblem
	conflictProblem
	forbiddenProblem
	methodProblem
	rateLimitedProblem
)

func (p *problem) Error() string { return p.message }

// newProblem formats a problem and remembers whether its message opens with an interpolated value.
// A format that is exactly "%s" or "%v" passes a whole message through and is not flagged.
func newProblem(kind problemKind, format string, args ...any) *problem {
	opensWithValue := len(format) > 2 && (strings.HasPrefix(format, "%s") || strings.HasPrefix(format, "%v"))
	return &problem{kind: kind, message: fmt.Sprintf(format, args...), keepFirst: opensWithValue}
}

// errNotFound says that what was asked for does not exist.
func errNotFound(format string, args ...any) error {
	return newProblem(notFoundProblem, format, args...)
}

// errInvalid says that the request cannot be understood, or asks for something that is not valid.
func errInvalid(format string, args ...any) error {
	return newProblem(invalidProblem, format, args...)
}

// errConflict says that the state of what was asked about does not allow the request now.
func errConflict(format string, args ...any) error {
	return newProblem(conflictProblem, format, args...)
}

// errForbidden says that the portal does not take the request from who sent it: another host,
// another site, or no token where one is needed.
func errForbidden(format string, args ...any) error {
	return newProblem(forbiddenProblem, format, args...)
}

// errMethod says that the address exists and does not take the method of the request.
func errMethod(format string, args ...any) error {
	return newProblem(methodProblem, format, args...)
}

// errRateLimited says that the caller has sent too many wrong tokens from its address in the window.
func errRateLimited(format string, args ...any) error {
	return newProblem(rateLimitedProblem, format, args...)
}
