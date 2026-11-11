package portal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// lockedBuffer is a buffer one goroutine writes while another reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestThePortalListensWhereItsSettingsSay: `foreman-kit portal` listens on the port and the address
// the settings of the daemon give, unless its command line names others. It reads nothing from the
// environment: a variable called PORT is what a gate gives the service it starts, not where the
// portal listens. A key the file lacks does not keep it from starting.
func TestThePortalListensWhereItsSettingsSay(t *testing.T) {
	_, st, root := setupTestPortal(t)
	t.Setenv("PORT", "1")
	t.Setenv("BIND", "203.0.113.9")

	// As the kit installs it: what the kit ships, and nothing to say.
	var said bytes.Buffer
	if port, bind := listenWhere(root, &said); port != DefaultPort || bind != DefaultBind || said.Len() != 0 {
		t.Errorf("an application as the kit installs it listens on %s:%d (%q), want %s:%d and nothing said", bind, port, said.String(), DefaultBind, DefaultPort)
	}
	// What its file says.
	for key, value := range map[settings.Key]string{settings.DaemonPort: "4599", settings.DaemonBind: "127.0.0.2"} {
		if err := config.Set(root, string(key), value, false, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if port, bind := listenWhere(root, &said); port != 4599 || bind != "127.0.0.2" || said.Len() != 0 {
		t.Errorf("with its settings changed the portal listens on %s:%d (%q), want 127.0.0.2:4599", bind, port, said.String())
	}
	// A key the file lacks: where the kit ships, and it says which key.
	dropKey(t, root, settings.DaemonPort)
	if port, bind := listenWhere(root, &said); port != DefaultPort || bind != "127.0.0.2" ||
		!strings.Contains(said.String(), settings.DaemonPort.Where()) || !strings.Contains(said.String(), strconv.Itoa(DefaultPort)) {
		t.Errorf("with the port missing from its file the portal listens on %s:%d and says %q", bind, port, said.String())
	}

	// The command line comes first, and the command says where it listens and answers there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- RunPortalCLI(ctx, root, st, &fakeKit{st: st}, []string{"--port", strconv.Itoa(free), "--bind", "127.0.0.1"}, &out, &errOut)
	}()
	address := fmt.Sprintf("http://127.0.0.1:%d", free)
	answered := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && !answered; time.Sleep(20 * time.Millisecond) {
		select {
		case code := <-done:
			t.Fatalf("the portal ended with %d before it answered: %s %s", code, out.String(), errOut.String())
		default:
		}
		if !strings.Contains(out.String(), "listening on "+address) {
			continue
		}
		if res, err := http.Get(address + "/api/dashboard"); err == nil {
			answered = res.StatusCode == http.StatusOK
			_ = res.Body.Close()
		}
	}
	if !answered {
		t.Fatalf("the portal did not answer at %s: it said %q %q", address, out.String(), errOut.String())
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("the portal ended with %d: %s", code, errOut.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the portal did not stop when it was told to")
	}
}
