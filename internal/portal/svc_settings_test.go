package portal

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestFreshSharesARunAmongThoseWhoAskAtOnceAndKeepsNothing: a read that is costly to run many times
// at once runs once for the callers that ask together, and what each gets was read after it asked:
// never the read that was already running when it arrived, and never one kept from before.
func TestFreshSharesARunAmongThoseWhoAskAtOnceAndKeepsNothing(t *testing.T) {
	var f fresh[int64]
	var runs atomic.Int64
	read := func() int64 { return runs.Add(1) }

	// One after another, each asks after the read before it returned: each gets a read of its own.
	for want := int64(1); want <= 3; want++ {
		if got := f.get(read); got != want {
			t.Fatalf("the caller %d got the read %d: nothing is kept for one that asks later", want, got)
		}
	}

	// While a read runs, the callers that arrive wait for the next one, and share it.
	begun, release := make(chan struct{}), make(chan struct{})
	first := make(chan int64, 1)
	go func() {
		first <- f.get(func() int64 {
			close(begun)
			<-release
			return runs.Add(1)
		})
	}()
	<-begun
	const callers = 20
	got := make(chan int64, callers)
	for i := 0; i < callers; i++ {
		go func() { got <- f.get(read) }()
	}
	// Long enough for them to be waiting; one that is late only gets a later read.
	time.Sleep(100 * time.Millisecond)
	close(release)
	if v := <-first; v != 4 {
		t.Fatalf("the read that was running returned %d, want 4", v)
	}
	for i := 0; i < callers; i++ {
		if v := <-got; v < 5 {
			t.Errorf("a caller that arrived while the read 4 ran got the read %d: it began before the caller asked", v)
		}
	}
	if total := runs.Load(); total >= 4+callers {
		t.Errorf("%d callers that asked at once ran %d reads: none was shared", callers, total-4)
	}
}
