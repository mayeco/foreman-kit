package gitx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"net"
	"os"
	"path/filepath"
	"testing"

	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const printSockAlias = "alias.printsock=!sh -c 'printf \"%s\" \"$SSH_AUTH_SOCK\"'"

// shortSockDir returns a directory short enough for a unix socket path (macOS allows 104 bytes).
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "oskit-sock-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serveAgent serves keyring on a new unix socket at path and stops when the test ends.
func serveAgent(t *testing.T, keyring agent.Agent, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = agent.ServeAgent(keyring, c)
				_ = c.Close()
			}(conn)
		}
	}()
}

// authSockSeenByGit runs a git command that prints the SSH_AUTH_SOCK it received.
func authSockSeenByGit(t *testing.T) string {
	t.Helper()
	out, err := RunGit(context.Background(), t.TempDir(), "-c", printSockAlias, "printsock")
	if err != nil {
		t.Fatalf("printsock failed: %v", err)
	}
	return out
}

// A daemon started in tmux keeps the agent path of an SSH login that ended: git got it, although a live
// agent was discoverable, and every fetch and push through the CLI failed with publickey.
func TestRunGitReplacesAStaleSSHAuthSockWithALiveOne(t *testing.T) {
	sockDir := shortSockDir(t)
	live := filepath.Join(sockDir, "ssh-agent.socket")
	serveAgent(t, agent.NewKeyring(), live)
	sshProbeEnv(t, "exit 1")
	t.Setenv("XDG_RUNTIME_DIR", sockDir)
	t.Setenv("FOREMANKIT_SANITIZE_GIT_ENV", "0")
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(sockDir, "gone.sock"))

	if got := authSockSeenByGit(t); got != live {
		t.Fatalf("git received SSH_AUTH_SOCK=%q, want the live socket %q", got, live)
	}
}

func TestRunGitKeepsALiveSSHAuthSock(t *testing.T) {
	sockDir := shortSockDir(t)
	inherited := filepath.Join(sockDir, "inherited.sock")
	serveAgent(t, agent.NewKeyring(), inherited)
	serveAgent(t, agent.NewKeyring(), filepath.Join(sockDir, "ssh-agent.socket"))
	sshProbeEnv(t, "exit 1")
	t.Setenv("XDG_RUNTIME_DIR", sockDir)
	t.Setenv("FOREMANKIT_SANITIZE_GIT_ENV", "0")
	t.Setenv("SSH_AUTH_SOCK", inherited)

	if got := authSockSeenByGit(t); got != inherited {
		t.Fatalf("git received SSH_AUTH_SOCK=%q, want the inherited live socket %q", got, inherited)
	}
}

func TestRunGitLeavesAStaleSSHAuthSockWhenNothingIsDiscoverable(t *testing.T) {
	sockDir := shortSockDir(t)
	stale := filepath.Join(sockDir, "gone.sock")
	sshProbeEnv(t, "exit 1")
	t.Setenv("FOREMANKIT_SANITIZE_GIT_ENV", "0")
	t.Setenv("SSH_AUTH_SOCK", stale)

	if got := authSockSeenByGit(t); got != stale {
		t.Fatalf("git received SSH_AUTH_SOCK=%q, want the inherited value %q", got, stale)
	}
}

func TestRunGitSanitizedEnvironmentKeepsAStaleSSHAuthSock(t *testing.T) {
	sockDir := shortSockDir(t)
	stale := filepath.Join(sockDir, "gone.sock")
	serveAgent(t, agent.NewKeyring(), filepath.Join(sockDir, "ssh-agent.socket"))
	sshProbeEnv(t, "exit 1")
	t.Setenv("XDG_RUNTIME_DIR", sockDir)
	t.Setenv("FOREMANKIT_SANITIZE_GIT_ENV", "1")
	t.Setenv("SSH_AUTH_SOCK", stale)

	if got := authSockSeenByGit(t); got != stale {
		t.Fatalf("git received SSH_AUTH_SOCK=%q, want the inherited value %q under FOREMANKIT_SANITIZE_GIT_ENV=1", got, stale)
	}
}

// agentWithKeys serves a keyring holding the given private keys on SSH_AUTH_SOCK, with a HOME that has no
// key files, so the agent is the only source of signers.
func agentWithKeys(t *testing.T, keys ...any) {
	t.Helper()
	keyring := agent.NewKeyring()
	for _, k := range keys {
		if err := keyring.Add(agent.AddedKey{PrivateKey: k, Comment: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	sockDir := shortSockDir(t)
	sock := filepath.Join(sockDir, "a.sock")
	serveAgent(t, keyring, sock)
	t.Setenv("HOME", t.TempDir())
	cfg := filepath.Join(sockDir, "ssh_config")
	if err := os.WriteFile(cfg, []byte("Host *\n  StrictHostKeyChecking no\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREMANKIT_SSH_CONFIG", cfg)
	t.Setenv("SSH_AUTH_SOCK", sock)
}

func verifySignature(t *testing.T, pub gossh.PublicKey, data []byte, sig *gossh.Signature) {
	t.Helper()
	if err := pub.Verify(data, sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

// go-git signs during the handshake, after the callback returned: the signer used to fail with
// "use of closed network connection".
func TestAgentSignerSignsAfterTheKeysWereListed(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	agentWithKeys(t, priv)

	auth, _, err := BuildSSHAuth("git@example.invalid:acme/repo.git")
	if err != nil {
		t.Fatalf("BuildSSHAuth: %v", err)
	}
	signers, err := auth.(*gitssh.PublicKeysCallback).Callback()
	if err != nil || len(signers) != 1 {
		t.Fatalf("BuildSSHAuth callback = %d signers, err=%v; want the agent's one key", len(signers), err)
	}
	data := []byte("data to sign")
	sig, err := signers[0].Sign(rand.Reader, data)
	if err != nil {
		t.Fatalf("agent signer of BuildSSHAuth failed to sign: %v", err)
	}
	verifySignature(t, signers[0].PublicKey(), data, sig)

	direct, err := collectSigners(SSHHostConfig{Alias: "example.invalid"})
	if err != nil || len(direct) != 1 {
		t.Fatalf("collectSigners = %d signers, err=%v; want the agent's one key", len(direct), err)
	}
	sig, err = direct[0].Sign(rand.Reader, data)
	if err != nil {
		t.Fatalf("collectSigners signer failed to sign: %v", err)
	}
	verifySignature(t, direct[0].PublicKey(), data, sig)
}

func TestAgentSignerSignsWithAnRSAAlgorithm(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	agentWithKeys(t, priv)

	signers, err := collectSigners(SSHHostConfig{Alias: "example.invalid"})
	if err != nil || len(signers) != 1 {
		t.Fatalf("collectSigners = %d signers, err=%v; want the agent's one key", len(signers), err)
	}
	as, ok := signers[0].(gossh.AlgorithmSigner)
	if !ok {
		t.Fatalf("agent signer %T does not implement ssh.AlgorithmSigner", signers[0])
	}
	data := []byte("data to sign")
	sig, err := as.SignWithAlgorithm(rand.Reader, data, gossh.KeyAlgoRSASHA256)
	if err != nil {
		t.Fatalf("SignWithAlgorithm: %v", err)
	}
	if sig.Format != gossh.KeyAlgoRSASHA256 {
		t.Fatalf("signature format = %q, want %q", sig.Format, gossh.KeyAlgoRSASHA256)
	}
	verifySignature(t, signers[0].PublicKey(), data, sig)
}
