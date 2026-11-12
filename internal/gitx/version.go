package gitx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// MinVersion is the oldest git version the kit supports.
const MinVersion = "2.17.0"

// MinWorktreeRepairVersion is the git version that added "git worktree repair".
const MinWorktreeRepairVersion = "2.29.0"

var gitVersionRe = regexp.MustCompile(`(?i)^\s*(?:git\s+version\s+)?(\d+)\.(\d+)(?:\.(\d+))?(?:[\s.(+-]|$)`)

// CurrentBranch returns the short name of the branch checked out in dir. It returns ("", false)
// when HEAD is detached, dir is not a git repository, or git fails.
func CurrentBranch(ctx context.Context, dir string) (string, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	out, err := RunGit(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", false
	}
	branch := strings.TrimSpace(out)
	if branch == "" || branch == "HEAD" {
		return "", false
	}
	return branch, true
}

// ParseVersion extracts the "X.Y.Z" version triple from `git --version` output or a raw version
// string, ignoring vendor suffixes such as "2.39.5 (Apple Git-154)" or "2.20.1.windows.1".
func ParseVersion(raw string) (string, bool) {
	ver, _, ok := parseVersionTuple(raw)
	return ver, ok
}

func parseVersionTuple(raw string) (string, [3]int, bool) {
	firstLine := strings.TrimSpace(strings.SplitN(raw, "\n", 2)[0])
	m := gitVersionRe.FindStringSubmatch(firstLine)
	if len(m) != 4 {
		return "", [3]int{}, false
	}
	maj, err1 := strconv.Atoi(m[1])
	min, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return "", [3]int{}, false
	}
	patch := 0
	if m[3] != "" {
		p, err3 := strconv.Atoi(m[3])
		if err3 != nil {
			return "", [3]int{}, false
		}
		patch = p
	}
	tuple := [3]int{maj, min, patch}
	return fmt.Sprintf("%d.%d.%d", maj, min, patch), tuple, true
}

// VersionAtLeast reports whether have is numerically greater than or equal to minimum.
func VersionAtLeast(have, minimum string) bool {
	_, h, okH := parseVersionTuple(have)
	_, m, okM := parseVersionTuple(minimum)
	if !okH || !okM {
		return false
	}
	if h[0] != m[0] {
		return h[0] > m[0]
	}
	if h[1] != m[1] {
		return h[1] > m[1]
	}
	return h[2] >= m[2]
}

var (
	versionCacheMu   sync.Mutex
	cachedGitPath    string
	cachedGitModNano int64
	cachedGitSize    int64
	cachedGitVer     string
)

// Version resolves `git` on PATH and parses its version triple. When `git` is found on PATH but its
// version output cannot be read or parsed, path is still returned alongside the error.
func Version(ctx context.Context) (version string, path string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path, err = exec.LookPath("git")
	if err != nil {
		return "", "", fmt.Errorf("git not found on PATH: %w", err)
	}
	var modNano, size int64
	if fi, statErr := os.Stat(path); statErr == nil {
		modNano = fi.ModTime().UnixNano()
		size = fi.Size()
		// Cache only real git binaries (>= 4 KiB); small wrapper scripts in tests always run fresh.
		if size >= 4096 {
			versionCacheMu.Lock()
			if path == cachedGitPath && modNano == cachedGitModNano && size == cachedGitSize && cachedGitVer != "" {
				ver := cachedGitVer
				versionCacheMu.Unlock()
				return ver, path, nil
			}
			versionCacheMu.Unlock()
		}
	}
	out, runErr := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	firstLine := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	ver, ok := ParseVersion(firstLine)
	if runErr != nil || !ok {
		if firstLine == "" && runErr != nil {
			firstLine = runErr.Error()
		}
		return "", path, fmt.Errorf("cannot read git version from %s (%q)", path, firstLine)
	}
	if size >= 4096 {
		versionCacheMu.Lock()
		cachedGitPath = path
		cachedGitModNano = modNano
		cachedGitSize = size
		cachedGitVer = ver
		versionCacheMu.Unlock()
	}
	return ver, path, nil
}

// CheckVersion verifies that the `git` on PATH is at least MinVersion. Its error names the version
// found (or that it could not be read), the minimum version, and the resolved path of the git binary.
func CheckVersion(ctx context.Context) error {
	ver, path, err := Version(ctx)
	if err != nil {
		if path == "" {
			return fmt.Errorf("git not found on PATH (minimum %s): %w", MinVersion, err)
		}
		return fmt.Errorf("git version at %s cannot be read (minimum %s): %w", path, MinVersion, err)
	}
	if !VersionAtLeast(ver, MinVersion) {
		return fmt.Errorf("git %s at %s is older than the minimum %s", ver, path, MinVersion)
	}
	return nil
}

// RepairWorktree runs `git worktree repair <wtPath>` in repoDir when the installed git is 2.29.0 or
// newer. On an older git it is skipped because `worktree repair` was added in git 2.29.
func RepairWorktree(ctx context.Context, repoDir, wtPath string) {
	if ver, _, err := Version(ctx); err == nil && VersionAtLeast(ver, MinWorktreeRepairVersion) {
		_, _ = RunGit(ctx, repoDir, "worktree", "repair", wtPath)
	}
}
