package gitx

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/kevinburke/ssh_config"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

var scpLikeRemoteRe = regexp.MustCompile(`^(?:([^@/\s]+)@)?([A-Za-z0-9._-]+):(.+)$`)

// SSHHostConfig holds resolved SSH parameters from ~/.ssh/config and /etc/ssh/ssh_config.
type SSHHostConfig struct {
	Alias                 string
	HostName              string
	User                  string
	Port                  string
	IdentityFiles         []string
	UserKnownHostsFiles   []string
	IdentitiesOnly        bool
	StrictHostKeyChecking string
	ProxyJump             string
	ProxyCommand          string
	IdentityAgent         string
}

// ResolveSSHConfig resolves alias, user, hostname, port, and IdentityFiles from ~/.ssh/config.
func ResolveSSHConfig(alias, defaultUser string) SSHHostConfig {
	cfg := SSHHostConfig{
		Alias:    alias,
		HostName: alias,
		User:     defaultUser,
		Port:     "22",
	}
	if cfg.User == "" {
		cfg.User = "git"
	}

	userCfg := loadUserSSHConfig()
	get := func(key string) string {
		if userCfg != nil {
			if v, err := userCfg.Get(alias, key); err == nil && v != "" {
				return v
			}
		}
		return ssh_config.Get(alias, key)
	}
	getAll := func(key string) []string {
		if userCfg != nil {
			if vals, err := userCfg.GetAll(alias, key); err == nil && len(vals) > 0 {
				return vals
			}
		}
		return ssh_config.GetAll(alias, key)
	}

	if hn := strings.TrimSpace(get("HostName")); hn != "" {
		cfg.HostName = hn
	}
	if defaultUser == "" {
		if u := strings.TrimSpace(get("User")); u != "" {
			cfg.User = u
		}
	}
	if p := strings.TrimSpace(get("Port")); p != "" {
		cfg.Port = p
	}
	if strings.EqualFold(strings.TrimSpace(get("IdentitiesOnly")), "yes") {
		cfg.IdentitiesOnly = true
	}
	cfg.StrictHostKeyChecking = strings.ToLower(strings.TrimSpace(get("StrictHostKeyChecking")))
	cfg.ProxyJump = strings.TrimSpace(get("ProxyJump"))
	cfg.ProxyCommand = strings.TrimSpace(get("ProxyCommand"))
	cfg.IdentityAgent = expandHome(strings.TrimSpace(get("IdentityAgent")))

	seen := make(map[string]bool)
	for _, idFile := range getAll("IdentityFile") {
		expanded := expandHome(strings.TrimSpace(idFile))
		if expanded != "" && !seen[expanded] {
			seen[expanded] = true
			cfg.IdentityFiles = append(cfg.IdentityFiles, expanded)
		}
	}

	seenKH := make(map[string]bool)
	for _, khEntry := range getAll("UserKnownHostsFile") {
		for _, khFile := range strings.Fields(khEntry) {
			if strings.EqualFold(khFile, "none") {
				continue
			}
			expanded := expandHome(strings.TrimSpace(khFile))
			if expanded != "" && !seenKH[expanded] {
				seenKH[expanded] = true
				cfg.UserKnownHostsFiles = append(cfg.UserKnownHostsFiles, expanded)
			}
		}
	}
	return cfg
}

// DiscoverSSHAuthSock returns SSH_AUTH_SOCK from environment, XDG_RUNTIME_DIR / systemd on Linux,
// 1Password agent sockets, or launchctl on macOS headless/cron runs.
func DiscoverSSHAuthSock(customAgent string) string {
	if customAgent != "" && !strings.EqualFold(customAgent, "SSH_AUTH_SOCK") && !strings.EqualFold(customAgent, "none") {
		if _, err := os.Stat(customAgent); err == nil {
			return customAgent
		}
	}
	if sock := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK")); sock != "" {
		if _, err := os.Stat(sock); err == nil {
			return sock
		}
	}

	home, _ := os.UserHomeDir()
	xdgRuntime := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if xdgRuntime == "" && runtime.GOOS == "linux" {
		xdgRuntime = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}

	var candidates []string
	if xdgRuntime != "" {
		candidates = append(candidates,
			filepath.Join(xdgRuntime, "ssh-agent.socket"),
			filepath.Join(xdgRuntime, "keyring", "ssh"),
			filepath.Join(xdgRuntime, "gcr", "ssh"),
			filepath.Join(xdgRuntime, "gnupg", "S.gpg-agent.ssh"),
		)
	}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".1password", "agent.sock"),
			filepath.Join(home, ".ssh", "agent.sock"),
		)
		if runtime.GOOS == "darwin" {
			candidates = append(candidates,
				filepath.Join(home, "Library", "Group Containers", "2BUA8C4S2C.com.1password", "t", "agent.sock"),
			)
		}
	}
	for _, cand := range candidates {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}

	if sock := serviceManagerAuthSock(); sock != "" {
		if _, err := os.Stat(sock); err == nil {
			return sock
		}
	}
	return ""
}

// serviceManagerProbeTimeout bounds the question serviceManagerAuthSock puts to the service manager, and
// serviceManagerProbeEvery is how long its answer is kept.
var (
	serviceManagerProbeTimeout = 2 * time.Second
	serviceManagerProbeEvery   = 30 * time.Second
)

// serviceManagerProbe is the last answer of the service manager and when it was given.
var serviceManagerProbe struct {
	mu    sync.Mutex
	asked time.Time
	sock  string
}

// serviceManagerAuthSock asks the user's service manager for SSH_AUTH_SOCK: systemd on Linux, launchd on
// macOS. It returns "" when there is none to ask or it names no socket.
//
// RunGit comes here before every git command of a process that was started without an agent socket and
// finds none in the usual places (a cron job, a service, a container). The answer is therefore kept for
// serviceManagerProbeEvery instead of starting systemctl or launchctl each time, and the question is
// abandoned after serviceManagerProbeTimeout: a "systemctl --user" that cannot reach its bus may not
// answer at all, and every git command, the idle watchdog that runs them and the work behind a lock
// heartbeat would hang with it.
func serviceManagerAuthSock() string {
	var name string
	var args []string
	switch runtime.GOOS {
	case "linux":
		name, args = "systemctl", []string{"--user", "show-environment"}
	case "darwin":
		name, args = "launchctl", []string{"getenv", "SSH_AUTH_SOCK"}
	default:
		return ""
	}

	p := &serviceManagerProbe
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.asked.IsZero() && time.Since(p.asked) < serviceManagerProbeEvery {
		return p.sock
	}

	ctx, cancel := context.WithTimeout(context.Background(), serviceManagerProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// A helper the command left behind must not keep Output waiting on the pipe after the timeout.
	cmd.WaitDelay = time.Second
	sock := ""
	if out, err := cmd.Output(); err == nil {
		if runtime.GOOS == "linux" {
			for _, line := range strings.Split(string(out), "\n") {
				if v, ok := strings.CutPrefix(line, "SSH_AUTH_SOCK="); ok && strings.TrimSpace(v) != "" {
					sock = strings.TrimSpace(v)
				}
			}
		} else {
			sock = strings.TrimSpace(string(out))
		}
	}
	p.asked, p.sock = time.Now(), sock
	return sock
}

// BuildSSHAuth builds a go-git transport.AuthMethod for an SSH remote URL,
// consulting ~/.ssh/config, ssh-agent, ~/.ssh/id_* keys, and known_hosts.
func BuildSSHAuth(remoteURL string) (transport.AuthMethod, *SSHHostConfig, error) {
	alias, user, isSSH := parseSSHRemoteHostUser(remoteURL)
	if !isSSH {
		return nil, nil, nil
	}
	hostCfg := ResolveSSHConfig(alias, user)

	hostKeyCallback, err := buildHostKeyCallback(hostCfg)
	if err != nil {
		return nil, &hostCfg, err
	}

	auth := &gitssh.PublicKeysCallback{
		User: hostCfg.User,
		Callback: func() ([]gossh.Signer, error) {
			return collectSigners(hostCfg)
		},
	}
	auth.HostKeyCallback = hostKeyCallback
	return auth, &hostCfg, nil
}

// NeedsSystemSSH reports whether the remote or SSH configuration requires falling back to OpenSSH CLI
// (e.g. ProxyJump, ProxyCommand, or GIT_SSH_COMMAND).
func NeedsSystemSSH(hostCfg *SSHHostConfig) bool {
	if strings.TrimSpace(os.Getenv("GIT_SSH_COMMAND")) != "" {
		return true
	}
	if hostCfg == nil {
		return false
	}
	return hostCfg.ProxyJump != "" || hostCfg.ProxyCommand != ""
}

func collectSigners(hostCfg SSHHostConfig) ([]gossh.Signer, error) {
	var signers []gossh.Signer

	// 1. Agent signers (unless IdentitiesOnly is set without IdentityFile)
	if !hostCfg.IdentitiesOnly {
		if sock := DiscoverSSHAuthSock(hostCfg.IdentityAgent); sock != "" {
			if conn, err := net.Dial("unix", sock); err == nil {
				ag := agent.NewClient(conn)
				if agentSigners, err := ag.Signers(); err == nil {
					signers = append(signers, agentSigners...)
				}
				_ = conn.Close()
			}
		}
	}

	// 2. IdentityFiles configured in ~/.ssh/config + standard ~/.ssh/id_* keys
	var candidateFiles []string
	candidateFiles = append(candidateFiles, hostCfg.IdentityFiles...)
	if !hostCfg.IdentitiesOnly || len(candidateFiles) == 0 {
		home, _ := os.UserHomeDir()
		if home != "" {
			for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
				candidateFiles = append(candidateFiles, filepath.Join(home, ".ssh", name))
			}
		}
	}

	seen := make(map[string]bool)
	for _, f := range candidateFiles {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		pemBytes, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		signer, err := gossh.ParsePrivateKey(pemBytes)
		if err == nil {
			signers = append(signers, signer)
		}
	}

	if len(signers) == 0 {
		return nil, fmt.Errorf("gitx: no usable SSH keys or ssh-agent signers found for host %s", hostCfg.Alias)
	}
	return signers, nil
}

// systemKnownHostsFile is OpenSSH's global known_hosts, consulted after the user's files as ssh does.
// Tests point it elsewhere: a CI runner ships one (GitHub's carries github.com's keys).
var systemKnownHostsFile = "/etc/ssh/ssh_known_hosts"

func buildHostKeyCallback(hostCfg SSHHostConfig) (gossh.HostKeyCallback, error) {
	if hostCfg.StrictHostKeyChecking == "no" {
		return gossh.InsecureIgnoreHostKey(), nil
	}

	var candidates []string
	candidates = append(candidates, hostCfg.UserKnownHostsFiles...)
	if len(candidates) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, ".ssh", "known_hosts"))
		}
	}
	candidates = append(candidates, systemKnownHostsFile)

	var files []string
	seen := make(map[string]bool)
	for _, f := range candidates {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		if info, err := os.Stat(f); err == nil && !info.IsDir() {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("gitx: no known_hosts files found for host %s", hostCfg.Alias)
	}
	cb, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("gitx: invalid known_hosts files for host %s: %w", hostCfg.Alias, err)
	}
	return cb, nil
}

func parseSSHRemoteHostUser(remoteURL string) (host string, user string, ok bool) {
	u := strings.TrimSpace(remoteURL)
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "file://") || strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".") {
		return "", "", false
	}
	if strings.HasPrefix(u, "ssh://") {
		rest := strings.TrimPrefix(u, "ssh://")
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			rest = rest[:slash]
		}
		if at := strings.IndexByte(rest, '@'); at >= 0 {
			user = rest[:at]
			rest = rest[at+1:]
		}
		if colon := strings.IndexByte(rest, ':'); colon >= 0 {
			rest = rest[:colon]
		}
		return rest, user, rest != ""
	}
	if m := scpLikeRemoteRe.FindStringSubmatch(u); m != nil {
		return m[2], m[1], true
	}
	return "", "", false
}

func loadUserSSHConfig() *ssh_config.Config {
	if cfgPath := strings.TrimSpace(os.Getenv("FOREMANKIT_SSH_CONFIG")); cfgPath != "" {
		if f, err := os.Open(cfgPath); err == nil {
			defer f.Close()
			if cfg, err := ssh_config.Decode(f); err == nil {
				return cfg
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	f, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	defer f.Close()
	cfg, err := ssh_config.Decode(f)
	if err != nil {
		return nil
	}
	return cfg
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
