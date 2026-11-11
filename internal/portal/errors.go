package portal

import "fmt"

// A problem is an error the one who asked can do something about: what was asked for does not exist,
// the request cannot be understood, the state of things does not allow it now, or the portal does
// not take it from who sent it or with the method it came with. A service returns one without
// knowing how it is answered; the handlers map it to a status (respond.go). An error that is not a
// problem is a failure of the portal itself.
type problem struct {
	kind    problemKind
	message string
}

type problemKind int

const (
	notFoundProblem problemKind = iota + 1
	invalidProblem
	conflictProblem
	forbiddenProblem
	methodProblem
)

func (p *problem) Error() string { return p.message }

// errNotFound says that what was asked for does not exist.
func errNotFound(format string, args ...any) error {
	return &problem{notFoundProblem, fmt.Sprintf(format, args...)}
}

// errInvalid says that the request cannot be understood, or asks for something that is not valid.
func errInvalid(format string, args ...any) error {
	return &problem{invalidProblem, fmt.Sprintf(format, args...)}
}

// errConflict says that the state of what was asked about does not allow the request now.
func errConflict(format string, args ...any) error {
	return &problem{conflictProblem, fmt.Sprintf(format, args...)}
}

// errForbidden says that the portal does not take the request from who sent it: another host,
// another site, or no token where one is needed.
func errForbidden(format string, args ...any) error {
	return &problem{forbiddenProblem, fmt.Sprintf(format, args...)}
}

// errMethod says that the address exists and does not take the method of the request.
func errMethod(format string, args ...any) error {
	return &problem{methodProblem, fmt.Sprintf(format, args...)}
}
