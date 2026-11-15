package sim

import (
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

const planRoundOnly = "\n## plan-repair-1, 2026-11-15T10:00:00Z, sim-agent-clean\n\nAsked to fix (spec_invalid):\n\n```\nbad spec\n```\n"

const bothRounds = planRoundOnly + "\n## repair-1, 2026-11-15T10:05:00Z, sim-agent-clean\n\nAsked to fix (gate_failure):\n\n```\nbroken test\n```\n"

// TestRepairsHasRoundTellsAPlanRoundFromABuildRound pins the check of plan-build-repairs: the phase
// "plan-repair-1" contains "repair-1", so a repairs.md with only the plan round must not satisfy the
// build round, and the mention of a round in a body must not count as a round.
func TestRepairsHasRoundTellsAPlanRoundFromABuildRound(t *testing.T) {
	cases := []struct {
		name, repairs, phase string
		want                 bool
	}{
		{"plan round only has the plan round", planRoundOnly, "plan-repair-1", true},
		{"plan round only lacks the build round", planRoundOnly, "repair-1", false},
		{"both rounds have the plan round", bothRounds, "plan-repair-1", true},
		{"both rounds have the build round", bothRounds, "repair-1", true},
		{"build round only lacks the plan round", "\n## repair-1, 2026-11-15T10:05:00Z, sim-agent-clean\n", "plan-repair-1", false},
		{"a body that names the round is not a round", "\n## plan-repair-1, 2026-11-15T10:00:00Z, e\n\nrepair-1 is next\n## notes\n", "repair-1", false},
		{"another number is another round", bothRounds, "repair-2", false},
		{"empty file has no round", "", "repair-1", false},
	}
	for _, c := range cases {
		if got := repairsHasRound(c.repairs, c.phase); got != c.want {
			t.Errorf("%s: repairsHasRound(%q) = %v, want %v", c.name, c.phase, got, c.want)
		}
	}
}

// TestStillBlockedWithRetriesFailsWhenTheChangeMoved pins the check of auto-retry-quarantine: after a
// dispatch inside the backoff the change must still be blocked with the same retries.
func TestStillBlockedWithRetriesFailsWhenTheChangeMoved(t *testing.T) {
	cases := []struct {
		name string
		q    *store.QuarantineRecord
		want bool
	}{
		{"blocked and unchanged", &store.QuarantineRecord{Status: "blocked", Retries: 0}, true},
		{"retried at once", &store.QuarantineRecord{Status: "blocked", Retries: 1}, false},
		{"lifted", &store.QuarantineRecord{Status: "cleared", Retries: 0}, false},
		{"rebuild requested", &store.QuarantineRecord{Status: "rebuild_requested", Retries: 0}, false},
		{"no record", nil, false},
	}
	for _, c := range cases {
		if got := stillBlockedWithRetries(c.q, 0); got != c.want {
			t.Errorf("%s: stillBlockedWithRetries = %v, want %v", c.name, got, c.want)
		}
	}
}
