package orchestrator

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// servingDispatcherWithClock returns a Dispatcher in Serve mode on a fake clock and the function that
// advances it.
func servingDispatcherWithClock(t *testing.T) (*Dispatcher, func(time.Duration)) {
	t.Helper()
	umbrellaDir, _, _, st := setupTestWorkspaceWithSubmodule(t)
	var mu sync.Mutex
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	d, err := NewDispatcher(DispatcherOptions{Root: umbrellaDir, Store: st, Out: io.Discard,
		NowFunc: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }})
	if err != nil {
		t.Fatal(err)
	}
	d.serving = true
	return d, func(by time.Duration) { mu.Lock(); now = now.Add(by); mu.Unlock() }
}

// Two workers that hit the same outage open one 60s window, not a 60s then a 120s one.
func TestOneOutageOpensOneCooldownWindow(t *testing.T) {
	d, advance := servingDispatcherWithClock(t)
	ctx := context.Background()
	if res := d.handleChangeError(ctx, "change-a", &EnginesDownError{Output: "quota"}, "", ""); res != "deferred" {
		t.Fatalf("change-a: %s", res)
	}
	advance(5 * time.Second)
	if res := d.handleChangeError(ctx, "change-b", &EnginesDownError{Output: "quota"}, "", ""); res != "deferred" {
		t.Fatalf("change-b: %s", res)
	}
	advance(56 * time.Second) // 61s after the outage began
	if d.isEnginesDown() {
		t.Fatal("one outage seen by two workers kept every worker from starting past its 60s window")
	}
}

// A build call that works says nothing about the plan engines: their cooldown stands.
func TestBuildSuccessKeepsThePlanCooldown(t *testing.T) {
	d, _ := servingDispatcherWithClock(t)
	if res := d.handleChangeError(context.Background(), "change-a", &EnginesDownError{Output: "quota", Role: "plan"}, "", ""); res != "deferred" {
		t.Fatalf("change-a: %s", res)
	}
	d.clearDeferrals("change-b", "build")
	if !d.isEnginesDown() {
		t.Fatal("a successful build call of another change ended the plan engines' cooldown")
	}
}
