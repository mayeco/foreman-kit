package store

import (
	"context"
	"errors"
	"fmt"
)

// Why a change cannot be abandoned right now.
const (
	// AbandonUnknown: the registry has no change of that name.
	AbandonUnknown = "unknown"
	// AbandonInFlight: a worker builds the change or the merger integrates it, and nothing stops
	// that process when the change is discarded under it.
	AbandonInFlight = "in_flight"
	// AbandonDone: the change is already integrated or discarded.
	AbandonDone = "done"
)

// AbandonRefusal says why a change is not abandoned. `foreman-kit abandon` and the portal's Abandon
// share it, so both refuse the same changes with the same words.
type AbandonRefusal struct {
	Change string
	// Kind is one of AbandonUnknown, AbandonInFlight and AbandonDone.
	Kind string
	// State is the state of the change when it was refused ("" when the registry does not know it).
	State   string
	message string
}

func (r *AbandonRefusal) Error() string { return r.message }

// AbandonRefusalFor returns why a change in state is not abandoned, or nil when it is: it waits,
// is ready, blocked or asked to be built again, and nothing runs on it. The state is the one
// ChangeState returns, so a change with an open change_run is building and one with an open merge
// attempt is merging.
func AbandonRefusalFor(change, state string) *AbandonRefusal {
	refuse := func(kind, format string, args ...any) *AbandonRefusal {
		return &AbandonRefusal{Change: change, Kind: kind, State: state, message: fmt.Sprintf(format, args...)}
	}
	switch state {
	case "":
		return refuse(AbandonUnknown, "the kit knows no change named %s", change)
	case ChangeBuilding:
		return refuse(AbandonInFlight, "a worker is building %s: it can be abandoned once its run ends", change)
	case ChangeMerging:
		return refuse(AbandonInFlight, "the merger is integrating %s: it can be abandoned once that attempt ends", change)
	case ChangeIntegrated:
		return refuse(AbandonDone, "%s is integrated: its commits are on the base branches", change)
	case ChangeDiscarded:
		return refuse(AbandonDone, "%s is already discarded", change)
	}
	return nil
}

// CheckAbandonable reads the state of a change and returns an *AbandonRefusal when it is not
// abandoned: unknown to the registry, being built, being integrated, integrated or discarded.
// It returns nil when the change may be abandoned, and any other error when the registry cannot be read.
func (s *Store) CheckAbandonable(ctx context.Context, change string) error {
	state, err := s.ChangeState(ctx, change)
	if err != nil {
		return err
	}
	if r := AbandonRefusalFor(change, state); r != nil {
		return r
	}
	return nil
}

// AsAbandonRefusal returns the refusal inside err, or nil when err is not one.
func AsAbandonRefusal(err error) *AbandonRefusal {
	var r *AbandonRefusal
	if errors.As(err, &r) {
		return r
	}
	return nil
}
