package sim

import (
	"os"
	"testing"
)

// A simulation started from a build's gates must not see the build agent's own variables:
// they point at the real worktree and change.
func TestScrubInheritedAgentEnv(t *testing.T) {
	for _, k := range inheritedAgentEnv {
		t.Setenv(k, "outer-"+k)
	}
	restore := scrubInheritedAgentEnv()
	for _, k := range inheritedAgentEnv {
		if v, ok := os.LookupEnv(k); ok {
			t.Fatalf("%s still set inside the simulation: %q", k, v)
		}
	}
	restore()
	for _, k := range inheritedAgentEnv {
		if got := os.Getenv(k); got != "outer-"+k {
			t.Fatalf("%s not restored: %q", k, got)
		}
	}
}
