package portal

import "testing"

// A cycle whose judgment failed is shown as an error, like the other outcomes that verified nothing.
func TestCycleToneOfAFailedJudgment(t *testing.T) {
	for outcome, want := range map[string]string{
		"judgment failed": "error",
		"scope violation": "error",
		"done":            "success",
		"paused":          "warning",
	} {
		if got := cycleTone(outcome); got != want {
			t.Errorf("cycleTone(%q) = %q, want %q", outcome, got, want)
		}
	}
}
