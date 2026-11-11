package portal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/store"
)

// fakeKit is the Kit of the tests: it starts no process. A test that cares what one of its methods
// does sets the function of that method; the others answer the way a healthy kit would.
type fakeKit struct {
	st *store.Store

	agent     func(ctx context.Context, workdir, engine, prompt string) (string, int, error)
	operation func(ctx context.Context, dir, operation string) (string, int, error)
	ping      func(ctx context.Context, root, engine string) PingResult
	simulate  func(ctx context.Context, root string) (*orchestrator.DryRunReport, string, error)
	recommend func(ctx context.Context, root, window string) (cycle.Recommendations, error)
}

// Recommend evaluates the settings from what the store of the test has on record, as the kit does.
func (k *fakeKit) Recommend(ctx context.Context, root, window string) (cycle.Recommendations, error) {
	if k.recommend != nil {
		return k.recommend(ctx, root, window)
	}
	return cycle.Recommend(ctx, root, k.st, window)
}

// SimulateDispatch answers as a dispatcher with two free workers and an empty backlog would.
func (k *fakeKit) SimulateDispatch(ctx context.Context, root string) (*orchestrator.DryRunReport, string, error) {
	if k.simulate != nil {
		return k.simulate(ctx, root)
	}
	return &orchestrator.DryRunReport{FreeWorkers: 2, Items: []orchestrator.DryRunItem{}},
		"dispatch: not running; the next cycle (or foreman-kit dispatch) starts one with 2 workers\nbacklog: nothing pending", nil
}

func (k *fakeKit) RunAgent(ctx context.Context, workdir, engine, prompt string) (string, int, error) {
	if k.agent != nil {
		return k.agent(ctx, workdir, engine, prompt)
	}
	return fmt.Sprintf("agent=%s prompt executed\n", engine), 0, nil
}

func (k *fakeKit) RunOperation(ctx context.Context, dir, operation string) (string, int, error) {
	if k.operation != nil {
		return k.operation(ctx, dir, operation)
	}
	return fmt.Sprintf("job %s completed\n", operation), 0, nil
}

func (k *fakeKit) PingEngine(ctx context.Context, root, engine string) PingResult {
	if k.ping != nil {
		return k.ping(ctx, root, engine)
	}
	if _, err := exec.LookPath(engine); err != nil {
		return PingResult{Engine: engine, Status: "not_installed", Detail: engine + " not found on PATH"}
	}
	return PingResult{Engine: engine, Status: "ok", LatencyMS: 5, Output: "pong"}
}

// StartCycle starts a real cycle of the test workspace without spawning the process that runs it.
func (k *fakeKit) StartCycle(ctx context.Context, root string) (string, error) {
	var out strings.Builder
	err := cycle.RunCycle(ctx, root, k.st, cycle.CycleOptions{
		Detach: true,
		DetachSpawnFunc: func(context.Context, string, string) (int, error) {
			return os.Getpid(), nil
		},
	}, &out)
	return out.String(), err
}

// newTestServer builds a Server on a fakeKit. The fake is returned so that a test can set what the
// kit does.
func newTestServer(t *testing.T, root string, st *store.Store, opts ...Option) (*Server, *fakeKit) {
	t.Helper()
	kit := &fakeKit{st: st}
	srv, err := New(root, st, kit, opts...)
	if err != nil {
		t.Fatalf("portal.New: %v", err)
	}
	return srv, kit
}

// TestNewNeedsAStoreAndAKit: a Server is not built without the state database or without the kit.
// There is no stand-in that answers as if the work had been done.
func TestNewNeedsAStoreAndAKit(t *testing.T) {
	_, st, root := setupTestPortal(t)
	if srv, err := New(root, nil, &fakeKit{}); err == nil || srv != nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("New without a store = %v, %v", srv, err)
	}
	if srv, err := New(root, st, nil); err == nil || srv != nil || !strings.Contains(err.Error(), "Kit") {
		t.Fatalf("New without a Kit = %v, %v", srv, err)
	}
	srv, err := New("", st, &fakeKit{st: st})
	if err != nil || srv.Root() != st.CanonicalRoot() {
		t.Fatalf("New takes the root of the store when none is given: %v, %v", srv, err)
	}
}
