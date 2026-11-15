package portal

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// runPortalCLIWithin runs RunPortalCLI on args with a context nothing cancels while it runs, and
// fails the test when the command has not returned within the given time: a command line the
// portal refuses must end on its own, not wait for a signal. A command that hangs is stopped when
// the test ends.
func runPortalCLIWithin(t *testing.T, root string, st *store.Store, args []string, within time.Duration) (code int, stdout, stderr string) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	var out, errOut lockedBuffer
	done := make(chan int, 1)
	go func() { done <- RunPortalCLI(ctx, root, st, &fakeKit{st: st}, args, &out, &errOut) }()
	select {
	case code = <-done:
		return code, out.String(), errOut.String()
	case <-time.After(within):
		t.Fatalf("portal %q did not return within %s: it said %q %q", args, within, out.String(), errOut.String())
		return 0, "", ""
	}
}

// freePort returns a port of host that nothing listens on as it returns.
func freePort(t *testing.T, host string) int {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// servePortalCLI runs RunPortalCLI on args until the test cancels the context it returns, and
// waits until its stdout says it listens at address. It returns what ends the run: cancel, then
// read the exit code from the channel.
func servePortalCLI(t *testing.T, root string, st *store.Store, args []string, address string) (context.CancelFunc, <-chan int, *lockedBuffer, *lockedBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out, errOut := &lockedBuffer{}, &lockedBuffer{}
	done := make(chan int, 1)
	go func() { done <- RunPortalCLI(ctx, root, st, &fakeKit{st: st}, args, out, errOut) }()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		select {
		case code := <-done:
			t.Fatalf("portal %q ended with %d before it listened: %q %q", args, code, out.String(), errOut.String())
		default:
		}
		if strings.Contains(out.String(), "portal: listening on "+address+"\n") {
			return cancel, done, out, errOut
		}
	}
	t.Fatalf("portal %q never said it listens on %s: %q %q", args, address, out.String(), errOut.String())
	return nil, nil, nil, nil
}

// stopPortalCLI cancels a portal servePortalCLI started and checks that it ends with 0.
func stopPortalCLI(t *testing.T, cancel context.CancelFunc, done <-chan int, errOut *lockedBuffer) {
	t.Helper()
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

// TestRunPortalCLIRefusesAPortOutsideTheRange: a port that is not a whole number from 1 to 65535
// is refused with exit 2 before anything listens, naming the value and the range.
func TestRunPortalCLIRefusesAPortOutsideTheRange(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	_, st, root := setupTestPortal(t)
	for _, tc := range []struct {
		args  []string
		value string
	}{
		{[]string{"--port", "abc"}, "abc"},
		{[]string{"--port", "0"}, "0"},
		{[]string{"--port", "-1"}, "-1"},
		{[]string{"--port", "65536"}, "65536"},
		{[]string{"--port=abc"}, "abc"},
		{[]string{"-p", "abc"}, "abc"},
		{[]string{"-p=0"}, "0"},
		{[]string{"--port", "4.5"}, "4.5"},
		{[]string{"--port="}, `""`},
		{[]string{"--bind", "127.0.0.1", "--port", "99999"}, "99999"},
	} {
		code, out, errOut := runPortalCLIWithin(t, root, st, tc.args, 5*time.Second)
		if code != 2 {
			t.Errorf("portal %q exited %d, want 2 (stderr %q)", tc.args, code, errOut)
		}
		if !strings.Contains(errOut, "--port "+tc.value+" ") || !strings.Contains(errOut, "1 to 65535") {
			t.Errorf("portal %q said %q on stderr, want the value %s and the range 1 to 65535", tc.args, errOut, tc.value)
		}
		if strings.Contains(out, "listening on") {
			t.Errorf("portal %q claimed to listen: %q", tc.args, out)
		}
	}
}

// TestRunPortalCLIRefusesMalformedArguments: an empty bind address, an unknown flag, a flag with
// no value and a positional argument are refused with exit 2 before anything listens; all but the
// empty address are answered with the usage line, which names both flags.
func TestRunPortalCLIRefusesMalformedArguments(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	_, st, root := setupTestPortal(t)
	for _, tc := range []struct {
		args []string
		says []string
	}{
		{[]string{"--bind="}, []string{"--bind is empty"}},
		{[]string{"--bind", ""}, []string{"--bind is empty"}},
		{[]string{"-o="}, []string{"--bind is empty"}},
		{[]string{"--verbose"}, []string{"--verbose", "--port", "--bind"}},
		{[]string{"--port"}, []string{"--port needs a value", "--bind"}},
		{[]string{"--port", "4567", "--bind"}, []string{"--bind needs a value", "--port"}},
		{[]string{"extra"}, []string{"extra", "--port", "--bind"}},
		{[]string{"--port", "4567", "extra"}, []string{"extra", "--port", "--bind"}},
	} {
		code, out, errOut := runPortalCLIWithin(t, root, st, tc.args, 5*time.Second)
		if code != 2 {
			t.Errorf("portal %q exited %d, want 2 (stderr %q)", tc.args, code, errOut)
		}
		for _, want := range tc.says {
			if !strings.Contains(errOut, want) {
				t.Errorf("portal %q said %q on stderr, want %q in it", tc.args, errOut, want)
			}
		}
		if strings.Contains(out, "listening on") {
			t.Errorf("portal %q claimed to listen: %q", tc.args, out)
		}
	}
}

// TestRunPortalCLIHelp: -h and --help print the help of the command on stdout and exit 0 without
// listening: both flags, the range of a port, and the settings each flag stands in for.
func TestRunPortalCLIHelp(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	_, st, root := setupTestPortal(t)
	for _, flag := range []string{"--help", "-h"} {
		code, out, errOut := runPortalCLIWithin(t, root, st, []string{flag}, 5*time.Second)
		if code != 0 {
			t.Errorf("portal %s exited %d, want 0 (stderr %q)", flag, code, errOut)
		}
		for _, want := range []string{"--port", "-p", "--bind", "-o", "1 to 65535", settings.DaemonPort.Path(), settings.DaemonBind.Path(), PortalUsage} {
			if !strings.Contains(out, want) {
				t.Errorf("portal %s printed %q, want %q in it", flag, out, want)
			}
		}
		if strings.Contains(out, "listening on") {
			t.Errorf("portal %s claimed to listen: %q", flag, out)
		}
	}
}

// TestRunPortalCLIAcceptsTheEqualsForm: --port=N --bind=ADDR set the port and the address as their
// separate forms do, and the portal answers there.
func TestRunPortalCLIAcceptsTheEqualsForm(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	_, st, root := setupTestPortal(t)
	port := freePort(t, "127.0.0.1")
	address := fmt.Sprintf("http://127.0.0.1:%d", port)
	cancel, done, _, errOut := servePortalCLI(t, root, st, []string{"--port=" + strconv.Itoa(port), "--bind=127.0.0.1"}, address)
	res, err := http.Get(address + "/api/dashboard")
	if err != nil {
		t.Fatalf("the portal said it listens on %s but does not answer: %v", address, err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET %s/api/dashboard answered %d, want 200", address, res.StatusCode)
	}
	stopPortalCLI(t, cancel, done, errOut)
}

// TestRunPortalCLIDoesNotClaimAPortInUse: when the port is taken the command exits 1 with the bind
// error on stderr, and never says it listens.
func TestRunPortalCLIDoesNotClaimAPortInUse(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	_, st, root := setupTestPortal(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := strconv.Itoa(held.Addr().(*net.TCPAddr).Port)
	code, out, errOut := runPortalCLIWithin(t, root, st, []string{"--port", port, "--bind", "127.0.0.1"}, 5*time.Second)
	if code != 1 {
		t.Errorf("portal on a port in use exited %d, want 1 (stderr %q)", code, errOut)
	}
	if !strings.HasPrefix(errOut, "portal: ") || !strings.Contains(errOut, port) {
		t.Errorf("portal on a port in use said %q on stderr, want the bind error after `portal: `", errOut)
	}
	if strings.Contains(out, "listening on") {
		t.Errorf("portal on a port in use claimed to listen: %q", out)
	}
}

// TestRunPortalCLIBracketsAnIPv6Address: an IPv6 bind address is written, and listened on, in
// brackets.
func TestRunPortalCLIBracketsAnIPv6Address(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	if ln, err := net.Listen("tcp", "[::1]:0"); err != nil {
		t.Skipf("this machine has no IPv6 loopback: %v", err)
	} else {
		_ = ln.Close()
	}
	_, st, root := setupTestPortal(t)
	port := freePort(t, "::1")
	address := fmt.Sprintf("http://[::1]:%d", port)
	cancel, done, _, errOut := servePortalCLI(t, root, st, []string{"--bind", "::1", "--port", strconv.Itoa(port)}, address)
	res, err := http.Get(address + "/api/dashboard")
	if err != nil {
		t.Fatalf("the portal said it listens on %s but does not answer: %v", address, err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET %s/api/dashboard answered %d, want 200", address, res.StatusCode)
	}
	stopPortalCLI(t, cancel, done, errOut)
}
