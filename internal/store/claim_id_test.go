package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestAgentClaimIDRoundTripsEveryPID verifies that claim keys decode to their owner for every PID the
// kernel can assign (Linux allows up to 4194304), that plain PIDs decode to themselves, and that keys of
// different processes never collide.
func TestAgentClaimIDRoundTripsEveryPID(t *testing.T) {
	for _, pid := range []int{1, 99998, 999999, 1000000, 1234567, 4194303} {
		for _, n := range []int64{1, 2, 1 << 20} {
			if got := ClaimOwnerPID(AgentClaimID(n, pid)); got != pid {
				t.Fatalf("ClaimOwnerPID(AgentClaimID(%d, %d)) = %d", n, pid, got)
			}
		}
		if got := ClaimOwnerPID(pid); got != pid {
			t.Fatalf("a plain PID %d decoded to %d", pid, got)
		}
	}
	if AgentClaimID(1, 1234567) == AgentClaimID(2, 234567) {
		t.Fatal("claim keys of different processes collide")
	}
}

// TestAgentsInUseKeepsLiveClaimOfThisProcess verifies that a claim made under the encoded key is
// counted as live (it used to be decoded to the wrong PID once PIDs reached 1,000,000).
func TestAgentsInUseKeepsLiveClaimOfThisProcess(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenAt(filepath.Join(dir, "state.db"), "ws-claims", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.ClaimAgent(ctx, AgentClaimID(7, os.Getpid()), "claude", "build", "x"); err != nil {
		t.Fatal(err)
	}
	counts, err := st.AgentsInUse(ctx)
	if err != nil || counts["claude"] != 1 {
		t.Fatalf("expected the live claim to be counted, got %v (err=%v)", counts, err)
	}
}
