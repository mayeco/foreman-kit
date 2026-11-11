package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Two workers whose changes share a capability must never both enter BUILD.
func TestAuditClaimCapabilitiesIsAtomic(t *testing.T) {
	root := t.TempDir()
	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-claim-audit", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	for round := 0; round < 50; round++ {
		d := &Dispatcher{
			store:    st,
			building: make(map[string][]string),
			held:     make(map[string][]string),
		}
		const workers = 8
		var winners atomic.Int32
		var start, wg sync.WaitGroup
		start.Add(1)
		// The main loop holds stateMu across SQLite I/O (computeWaitedHours, pauseFirst,
		// requeueRequestedRebuilds); waits over 1ms put sync.Mutex into starvation (FIFO hand-off) mode.
		stopHolder := make(chan struct{})
		holderDone := make(chan struct{})
		go func() {
			defer close(holderDone)
			for {
				select {
				case <-stopHolder:
					return
				default:
				}
				d.stateMu.Lock()
				time.Sleep(2 * time.Millisecond)
				d.stateMu.Unlock()
			}
		}()
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				start.Wait()
				if d.claimCapabilities(ctx, fmt.Sprintf("change-%d", w), []string{"auth"}) {
					winners.Add(1)
				}
			}(w)
		}
		start.Done()
		wg.Wait()
		close(stopHolder)
		<-holderDone
		if n := winners.Load(); n != 1 {
			t.Fatalf("round %d: %d workers claimed capability auth at the same time", round, n)
		}
	}
}
