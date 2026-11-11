package gitx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestOpenWorktreeAndInMemoryObjects(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	mainDir := filepath.Join(tmp, "umbrella")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := RunGit(ctx, mainDir, "init", "-b", "master"); err != nil {
		t.Fatal(err)
	}
	_, _ = RunGit(ctx, mainDir, "config", "user.email", "test@example.com")
	_, _ = RunGit(ctx, mainDir, "config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(mainDir, "MEMORY.md"), []byte("# Memory v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = RunGit(ctx, mainDir, "add", "MEMORY.md")
	_, _ = RunGit(ctx, mainDir, "commit", "-m", "chore: initial\n\nOpenSpec-Change: init-setup")

	repo, err := Open(mainDir)
	if err != nil {
		t.Fatal(err)
	}
	baseSHA, err := repo.RevParse("HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// Create a worktree (.worktrees/worker-test)
	wtDir := filepath.Join(mainDir, ".worktrees", "worker-test")
	if err := repo.AddWorktree(ctx, wtDir, "feat/spec-worker-test", "HEAD", false); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wtDir, "MEMORY.md"), []byte("# Memory v2 from worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = RunGit(ctx, wtDir, "add", "MEMORY.md")
	_, _ = RunGit(ctx, wtDir, "commit", "-m", "feat: update memory\n\nOpenSpec-Change: worker-test")

	// Open the worktree with go-git (EnableDotGitCommonDir: true)
	wtRepo, err := Open(wtDir)
	if err != nil {
		t.Fatalf("Open worktree with go-git failed: %v", err)
	}

	wtHead, err := wtRepo.RevParse("HEAD")
	if err != nil {
		t.Fatalf("RevParse HEAD in worktree failed: %v", err)
	}
	if wtHead == baseSHA {
		t.Fatalf("expected worktree HEAD != baseSHA")
	}

	mb, err := wtRepo.MergeBase("HEAD", baseSHA)
	if err != nil {
		t.Fatalf("MergeBase in worktree failed: %v", err)
	}
	if mb != baseSHA {
		t.Fatalf("expected MergeBase %s, got %s", baseSHA, mb)
	}

	blobV1, err := wtRepo.ReadBlob(baseSHA, "MEMORY.md")
	if err != nil || string(blobV1) != "# Memory v1\n" {
		t.Fatalf("unexpected blob v1: %q (err=%v)", string(blobV1), err)
	}
	blobV2, err := wtRepo.ReadBlob("HEAD", "MEMORY.md")
	if err != nil || string(blobV2) != "# Memory v2 from worktree\n" {
		t.Fatalf("unexpected blob v2: %q (err=%v)", string(blobV2), err)
	}

	changed, err := wtRepo.ChangedFiles(baseSHA, "HEAD")
	if err != nil || len(changed) != 1 || changed[0] != "MEMORY.md" {
		t.Fatalf("unexpected changed files: %v (err=%v)", changed, err)
	}

	hasTrailer, err := wtRepo.HasCommitWithTrailer("HEAD", "worker-test")
	if err != nil || !hasTrailer {
		t.Fatalf("expected HasCommitWithTrailer(worker-test)=true, got %v (err=%v)", hasTrailer, err)
	}

	// Test SaveUnpublishedCommit in-memory without touching git index
	if err := os.WriteFile(filepath.Join(wtDir, "MEMORY.md"), []byte("# Memory v3 unpublished\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unpubSHA, err := wtRepo.SaveUnpublishedCommit("refs/openspec/unpublished/test-1", "chore: unpublished record", []string{"MEMORY.md"})
	if err != nil {
		t.Fatalf("SaveUnpublishedCommit failed: %v", err)
	}
	blobV3, err := wtRepo.ReadBlob(unpubSHA, "MEMORY.md")
	if err != nil || string(blobV3) != "# Memory v3 unpublished\n" {
		t.Fatalf("unexpected unpublished blob: %q (err=%v)", string(blobV3), err)
	}
}

func TestSSHConfigAndAgentResolution(t *testing.T) {
	// Use a short socket path in /tmp because macOS Unix domain sockets have a 104-byte path limit.
	sockDir, err := os.MkdirTemp("/tmp", "oskit-ssh-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)

	// 1. Start an in-process SSH agent on a Unix socket with an ed25519 key
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = pub
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv, Comment: "test-ed25519"}); err != nil {
		t.Fatal(err)
	}

	sockPath := filepath.Join(sockDir, "a.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
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

	// 2. Write a custom ~/.ssh/config with an alias
	sshCfgFile := filepath.Join(sockDir, "ssh_config")
	cfgContent := fmt.Sprintf(`Host github-corp
  HostName 127.0.0.1
  User git-custom
  Port 2222
  StrictHostKeyChecking no
`)
	if err := os.WriteFile(sshCfgFile, []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREMANKIT_SSH_CONFIG", sshCfgFile)
	t.Setenv("SSH_AUTH_SOCK", sockPath)

	auth, hostCfg, err := BuildSSHAuth("git@github-corp:acme/repo.git")
	if err != nil {
		t.Fatalf("BuildSSHAuth failed: %v", err)
	}
	if hostCfg == nil {
		t.Fatal("expected non-nil hostCfg")
	}
	if hostCfg.HostName != "127.0.0.1" || hostCfg.Port != "2222" {
		t.Fatalf("unexpected resolved SSH config: %+v", hostCfg)
	}

	signers, err := collectSigners(*hostCfg)
	if err != nil || len(signers) == 0 {
		t.Fatalf("expected ssh-agent signer to be collected, got %d (err=%v)", len(signers), err)
	}
	_ = auth
	_ = pem.EncodeToMemory
	_ = git.PlainOpen
	_ = config.NewModules
	_ = gossh.PublicKeys
	if !strings.Contains(rewriteRemoteHost("git@github-corp:acme/repo.git", hostCfg), "127.0.0.1:2222") {
		t.Fatalf("expected rewriteRemoteHost to expand alias and port")
	}
}

func TestSSHHostKeyCallbackVerification(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// The machine's global known_hosts (present on CI runners) must not satisfy the checks below.
	prevSystem := systemKnownHostsFile
	systemKnownHostsFile = filepath.Join(tmp, "no_system_known_hosts")
	t.Cleanup(func() { systemKnownHostsFile = prevSystem })

	// 1. Missing known_hosts with StrictHostKeyChecking=accept-new must fail (never return InsecureIgnoreHostKey)
	cfgMissing := filepath.Join(tmp, "ssh_config_missing")
	if err := os.WriteFile(cfgMissing, []byte("Host corp\n  HostName 127.0.0.1\n  StrictHostKeyChecking accept-new\n  UserKnownHostsFile "+filepath.Join(tmp, "nonexistent_known_hosts")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREMANKIT_SSH_CONFIG", cfgMissing)
	if _, _, err := BuildSSHAuth("git@corp:acme/repo.git"); err == nil {
		t.Fatal("expected BuildSSHAuth to return error when known_hosts is missing and StrictHostKeyChecking=accept-new")
	}

	// 2. Unparseable known_hosts via UserKnownHostsFile must fail
	badKH := filepath.Join(tmp, "bad_known_hosts")
	if err := os.WriteFile(badKH, []byte("not-a-valid-known-hosts-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgBad := filepath.Join(tmp, "ssh_config_bad")
	if err := os.WriteFile(cfgBad, []byte("Host corp\n  HostName 127.0.0.1\n  UserKnownHostsFile "+badKH+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREMANKIT_SSH_CONFIG", cfgBad)
	if _, _, err := BuildSSHAuth("git@corp:acme/repo.git"); err == nil {
		t.Fatal("expected BuildSSHAuth to return error when UserKnownHostsFile is unparseable")
	}

	// 3. Valid UserKnownHostsFile from ~/.ssh/config must be loaded and verify host keys strictly
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherSSHPub, err := gossh.NewPublicKey(otherPub)
	if err != nil {
		t.Fatal(err)
	}

	validKH := filepath.Join(tmp, "custom_known_hosts")
	khLine := fmt.Sprintf("127.0.0.1 %s", gossh.MarshalAuthorizedKey(sshPub))
	if err := os.WriteFile(validKH, []byte(khLine), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgValid := filepath.Join(tmp, "ssh_config_valid")
	if err := os.WriteFile(cfgValid, []byte("Host corp\n  HostName 127.0.0.1\n  StrictHostKeyChecking accept-new\n  UserKnownHostsFile "+validKH+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREMANKIT_SSH_CONFIG", cfgValid)
	_, hostCfg, err := BuildSSHAuth("git@corp:acme/repo.git")
	if err != nil {
		t.Fatalf("expected BuildSSHAuth to succeed with valid UserKnownHostsFile, got: %v", err)
	}
	cb, err := buildHostKeyCallback(*hostCfg)
	if err != nil {
		t.Fatalf("buildHostKeyCallback failed: %v", err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	if err := cb("127.0.0.1:22", addr, sshPub); err != nil {
		t.Fatalf("expected matching host key to pass verification, got: %v", err)
	}
	if err := cb("127.0.0.1:22", addr, otherSSHPub); err == nil {
		t.Fatal("expected mismatched host key to be rejected when StrictHostKeyChecking=accept-new")
	}
}

func TestRunGitStdoutStderrSeparationAndSanitizeEnv(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	if _, err := RunGit(ctx, tmp, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}

	// 1. Verify stderr warnings on exit 0 do not pollute stdout
	out, err := RunGit(ctx, tmp, "-c", "alias.warnok=!sh -c 'printf \"clean-stdout\\n\"; printf \"warning: noisy stderr\\n\" >&2'", "warnok")
	if err != nil {
		t.Fatalf("expected exit 0 from git warnok, got error: %v", err)
	}
	if out != "clean-stdout\n" {
		t.Fatalf("expected stdout without stderr pollution, got %q", out)
	}

	// 2. Verify non-zero exit combines stdout and stderr for full diagnostics
	failOut, err := RunGit(ctx, tmp, "-c", "alias.warnfail=!sh -c 'printf \"out-msg\\n\"; printf \"err-msg\\n\" >&2; exit 1'", "warnfail")
	if err == nil {
		t.Fatal("expected error from git warnfail")
	}
	if !strings.Contains(failOut, "out-msg") || !strings.Contains(failOut, "err-msg") {
		t.Fatalf("expected combined stdout and stderr on error, got %q", failOut)
	}
	if !strings.Contains(err.Error(), "out-msg") || !strings.Contains(err.Error(), "err-msg") {
		t.Fatalf("expected error message to include combined stdout and stderr, got %v", err)
	}

	// 3. Verify FOREMANKIT_SANITIZE_GIT_ENV=1 prevents SSH_AUTH_SOCK discovery/injection
	xdgDir := filepath.Join(tmp, "xdg")
	if err := os.MkdirAll(xdgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeSock := filepath.Join(xdgDir, "ssh-agent.socket")
	if err := os.WriteFile(fakeSock, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", xdgDir)
	t.Setenv("SSH_AUTH_SOCK", "")

	t.Setenv("FOREMANKIT_SANITIZE_GIT_ENV", "0")
	sockOut, err := RunGit(ctx, tmp, "-c", "alias.printsock=!sh -c 'printf \"%s\" \"$SSH_AUTH_SOCK\"'", "printsock")
	if err != nil {
		t.Fatalf("printsock failed: %v", err)
	}
	if sockOut != fakeSock {
		t.Fatalf("expected DiscoverSSHAuthSock to inject %q when FOREMANKIT_SANITIZE_GIT_ENV!=1, got %q", fakeSock, sockOut)
	}

	t.Setenv("FOREMANKIT_SANITIZE_GIT_ENV", "1")
	sanitizedOut, err := RunGit(ctx, tmp, "-c", "alias.printsock=!sh -c 'printf \"%s\" \"$SSH_AUTH_SOCK\"'", "printsock")
	if err != nil {
		t.Fatalf("printsock with sanitize=1 failed: %v", err)
	}
	if sanitizedOut != "" {
		t.Fatalf("expected empty SSH_AUTH_SOCK when FOREMANKIT_SANITIZE_GIT_ENV=1, got %q", sanitizedOut)
	}
}
