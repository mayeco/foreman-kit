package portal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A ping whose command exists but cannot be started (a script whose interpreter is missing) has no
// output to show: the portal reported it as "failed" with an exit code and nothing else.
func TestKitPingSaysWhyAnEngineCouldNotBeStarted(t *testing.T) {
	_, st, root := setupTestPortal(t)
	binDir := t.TempDir()
	broken := filepath.Join(binDir, "claude")
	if err := os.WriteFile(broken, []byte("#!/nonexistent/interpreter\necho pong\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")

	res := NewKit(st, "unused").PingEngine(context.Background(), root, "claude")
	if res.Status != "failed" {
		t.Fatalf("expected the ping of an engine that cannot be started to fail, got %+v", res)
	}
	if !strings.Contains(res.Detail, broken) {
		t.Fatalf("the ping does not say why the engine could not be started: %+v", res)
	}
}
