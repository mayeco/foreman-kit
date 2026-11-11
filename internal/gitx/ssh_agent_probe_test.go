package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sshProbeEnv leaves the process with no agent socket to find (no SSH_AUTH_SOCK, a HOME and an
// XDG_RUNTIME_DIR without one) and puts a systemctl and a launchctl that run script first on PATH. It
// returns the file in which those stand-ins write one line per call.
func sshProbeEnv(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	body := "#!/bin/sh\necho called >> \"" + calls + "\"\n" + script + "\n"
	for _, name := range []string{"systemctl", "launchctl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("SSH_AUTH_SOCK", "")

	forget := func() {
		serviceManagerProbe.mu.Lock()
		serviceManagerProbe.asked, serviceManagerProbe.sock = time.Time{}, ""
		serviceManagerProbe.mu.Unlock()
	}
	forget()
	t.Cleanup(forget)
	return calls
}

func sshProbeCalls(t *testing.T, calls string) int {
	t.Helper()
	raw, err := os.ReadFile(calls)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}

// A process started without an agent socket (a cron job, a service) and finding none in the usual places
// asked systemctl or launchctl for it before every git command it ran: one more process per command, for
// an answer that does not change from one command to the next.
func TestDiscoverSSHAuthSockAsksTheServiceManagerOnceInAWhile(t *testing.T) {
	// The stand-in answers both as "systemctl --user show-environment" and as "launchctl getenv".
	calls := sshProbeEnv(t, `if [ "$1" = "--user" ]; then echo "SSH_AUTH_SOCK=$SSH_PROBE_SOCK"; else echo "$SSH_PROBE_SOCK"; fi`)
	sock := filepath.Join(t.TempDir(), "service-manager.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_PROBE_SOCK", sock)

	for i := 0; i < 5; i++ {
		if got := DiscoverSSHAuthSock(""); got != sock {
			t.Fatalf("call %d: DiscoverSSHAuthSock = %q, want the service manager's %q", i+1, got, sock)
		}
	}
	if n := sshProbeCalls(t, calls); n != 1 {
		t.Fatalf("the service manager was asked %d times for 5 lookups in a row, want once", n)
	}

	// The socket is checked on every lookup all the same: one that is gone is not handed out.
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverSSHAuthSock(""); got != "" {
		t.Fatalf("DiscoverSSHAuthSock = %q for a socket that no longer exists", got)
	}
}

// A service manager that does not answer (a "systemctl --user" without its bus) used to hang the lookup,
// and with it every git command, for as long as it took.
func TestDiscoverSSHAuthSockGivesUpOnAServiceManagerThatDoesNotAnswer(t *testing.T) {
	// The shell is killed at the timeout; the sleep it started keeps the output pipe open after it.
	calls := sshProbeEnv(t, "sleep 5")
	prev := serviceManagerProbeTimeout
	serviceManagerProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { serviceManagerProbeTimeout = prev })

	started := time.Now()
	if got := DiscoverSSHAuthSock(""); got != "" {
		t.Fatalf("DiscoverSSHAuthSock = %q from a service manager that never answered", got)
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the lookup waited %s for a service manager that does not answer", took.Round(time.Millisecond))
	}
	// The next lookups do not wait for it again.
	started = time.Now()
	for i := 0; i < 3; i++ {
		_ = DiscoverSSHAuthSock("")
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("three more lookups took %s: the unanswered question was put again", took.Round(time.Millisecond))
	}
	if n := sshProbeCalls(t, calls); n != 1 {
		t.Fatalf("the service manager was asked %d times, want once", n)
	}
}
