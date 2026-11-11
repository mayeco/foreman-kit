package cycle

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Concurrent visual QA requests must all be queued.
func TestAuditConcurrentVisualQARequestsAreAllQueued(t *testing.T) {
	root := t.TempDir()
	st, err := store.OpenAt(filepath.Join(root, "log", "openspec.db"), "ws-vqa-audit", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := VisualQARequest(ctx, root, st, fmt.Sprintf("check page %d", i), "", io.Discard); err != nil {
				t.Errorf("request %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	items, err := st.ListVisualQA(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != n {
		t.Fatalf("%d concurrent requests reported success but only %d were queued", n, len(items))
	}
}
