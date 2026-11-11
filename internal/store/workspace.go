package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var nonSlugChars = regexp.MustCompile(`[^a-z0-9_-]+`)

// AppRoot locates the application checkout or worktree containing dir.
func AppRoot(dir string) string {
	startDir := dir
	if startDir == "" {
		if wd, err := os.Getwd(); err == nil {
			startDir = wd
		}
	}
	var absStart string
	if startDir != "" {
		if abs, err := filepath.Abs(startDir); err == nil {
			absStart = abs
		} else {
			absStart = filepath.Clean(startDir)
		}
	}

	if env := strings.TrimSpace(os.Getenv("FOREMANKIT_APP_ROOT")); env != "" {
		if absEnv, err := filepath.Abs(env); err == nil {
			if absStart != "" {
				if wt := worktreeWithin(absStart, absEnv); wt != "" {
					return wt
				}
			}
			return absEnv
		}
	}

	if absStart == "" {
		return "."
	}

	cur := absStart
	for {
		if fileExists(filepath.Join(cur, ".foreman-kit/kit.lock")) ||
			// An application installed before the kit was called Foreman, until update moves it.
			fileExists(filepath.Join(cur, LegacyLockFile)) ||
			fileExists(filepath.Join(cur, ".foreman-kit", "pipeline.yaml")) ||
			fileExists(filepath.Join(cur, ".foreman-kit", "agents.yaml")) ||
			dirExists(filepath.Join(cur, "openspec")) {
			return cur
		}
		parent := filepath.Dir(cur)
		parentBase := filepath.Base(parent)
		if parentBase == ".worktrees" && hasUmbrellaMarker(filepath.Dir(parent)) {
			return cur
		}
		if exists(filepath.Join(cur, ".git")) {
			if (parentBase != "repos" && parentBase != ".worktrees") || !isUmbrellaOrWorktree(filepath.Dir(parent)) {
				break
			}
		}
		if parent == cur {
			break
		}
		cur = parent
	}
	return absStart
}

// MainRoot resolves the main umbrella checkout even if root is inside .worktrees/<name>,
// a git worktree (via .git file gitdir pointer), or repos/<child>.
func MainRoot(root string) string {
	if root == "" {
		root = AppRoot("")
	}
	abs, err := filepath.Abs(root)
	if err == nil {
		root = abs
	}

	root = findUmbrellaRoot(root)

	// Check if .git is a gitfile pointing to <main>/.git/worktrees/<name> or <main>/.git/modules/<name>
	dotGit := filepath.Join(root, ".git")
	if info, err := os.Stat(dotGit); err == nil && !info.IsDir() {
		if raw, err := os.ReadFile(dotGit); err == nil {
			line := strings.TrimSpace(string(raw))
			if strings.HasPrefix(line, "gitdir:") {
				gitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(root, gitDir)
				}
				gitDir = filepath.Clean(gitDir)
				// e.g. /path/to/app/.git/worktrees/worker-foo -> /path/to/app
				if idx := strings.Index(gitDir, string(filepath.Separator)+".git"+string(filepath.Separator)); idx > 0 {
					candidate := gitDir[:idx]
					if isUmbrellaOrWorktree(candidate) {
						root = findUmbrellaRoot(candidate)
					}
				}
			}
		}
	}

	return filepath.Clean(root)
}

func hasUmbrellaMarker(dir string) bool {
	if dir == "" {
		return false
	}
	if fileExists(filepath.Join(dir, ".foreman-kit/kit.lock")) ||
		fileExists(filepath.Join(dir, LegacyLockFile)) ||
		fileExists(filepath.Join(dir, ".foreman-kit", "pipeline.yaml")) ||
		fileExists(filepath.Join(dir, ".foreman-kit", "agents.yaml")) ||
		fileExists(filepath.Join(dir, ".gitmodules")) ||
		dirExists(filepath.Join(dir, "openspec")) {
		return true
	}
	if env := strings.TrimSpace(os.Getenv("FOREMANKIT_APP_ROOT")); env != "" {
		if absEnv, err := filepath.Abs(env); err == nil && samePath(dir, absEnv) {
			return true
		}
	}
	return false
}

func isUmbrellaOrWorktree(dir string) bool {
	if hasUmbrellaMarker(dir) {
		return true
	}
	parent := filepath.Dir(dir)
	if filepath.Base(parent) == ".worktrees" && hasUmbrellaMarker(filepath.Dir(parent)) {
		return true
	}
	return false
}

func findUmbrellaRoot(root string) string {
	cur := filepath.Clean(root)
	result := cur
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		parentBase := filepath.Base(parent)
		if parentBase == ".worktrees" || parentBase == "repos" {
			grandparent := filepath.Dir(parent)
			if grandparent != parent && isUmbrellaOrWorktree(grandparent) {
				result = grandparent
				cur = grandparent
				continue
			}
		}
		cur = parent
	}
	return result
}

func worktreeWithin(startDir, envRoot string) string {
	umbrella := findUmbrellaRoot(envRoot)
	cur := filepath.Clean(startDir)
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		if filepath.Base(parent) == ".worktrees" {
			gp := filepath.Dir(parent)
			if samePath(gp, envRoot) {
				return filepath.Join(envRoot, ".worktrees", filepath.Base(cur))
			}
			if samePath(gp, umbrella) {
				return filepath.Join(umbrella, ".worktrees", filepath.Base(cur))
			}
			if hasUmbrellaMarker(gp) {
				return cur
			}
		}
		cur = parent
	}
	return ""
}

func samePath(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	ra := evalSymlinksBestEffort(ca)
	rb := evalSymlinksBestEffort(cb)
	return ra != "" && ra == rb
}

// evalSymlinksBestEffort resolves symlinks on p. If p does not exist yet, it walks up
// to the deepest existing ancestor, runs filepath.EvalSymlinks on that ancestor, and
// joins the remaining suffix so paths like /tmp/ws and /private/tmp/ws always match.
func evalSymlinksBestEffort(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	} else {
		p = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved)
	}
	cur := p
	var suffix []string
	for {
		parent := filepath.Dir(cur)
		base := filepath.Base(cur)
		if parent == cur {
			break
		}
		suffix = append(suffix, base)
		cur = parent
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved)
		}
	}
	return filepath.Clean(p)
}

// WorkspaceID returns <slug>-<hash12> for the canonical root of the project.
func WorkspaceID(dir string) (workspaceID string, canonicalRoot string) {
	canonicalRoot = evalSymlinksBestEffort(MainRoot(evalSymlinksBestEffort(AppRoot(dir))))
	base := strings.ToLower(filepath.Base(canonicalRoot))
	slug := nonSlugChars.ReplaceAllString(base, "-")
	slug = strings.Trim(slug, "-_")
	if slug == "" {
		slug = "workspace"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	sum := sha256.Sum256([]byte(canonicalRoot))
	hash12 := hex.EncodeToString(sum[:])[:12]
	return slug + "-" + hash12, canonicalRoot
}

// StateDBPath returns the SQLite database path for the workspace containing dir.
func StateDBPath(dir string) string {
	dbPath, _, _, _ := ResolveDBPath(dir)
	return dbPath
}

// ResolveDBPath returns the SQLite database path for the workspace containing dir:
// $FOREMANKIT_DB_PATH if set, otherwise ~/.foreman-kit/state/<slug>-<hash12>.db
// (or $FOREMANKIT_STATE_DIR/<slug>-<hash12>.db). The application never writes under
// the umbrella's log/ directory: a legacy log/openspec.db found there (with no state
// database yet) is moved into the state directory on first resolution.
func ResolveDBPath(dir string) (dbPath string, workspaceID string, canonicalRoot string, err error) {
	workspaceID, canonicalRoot = WorkspaceID(dir)
	if envPath := strings.TrimSpace(os.Getenv("FOREMANKIT_DB_PATH")); envPath != "" {
		abs, err := filepath.Abs(envPath)
		if err != nil {
			return "", "", "", err
		}
		return abs, workspaceID, canonicalRoot, nil
	}

	stateDir, err := resolveStateDir()
	if err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", "", "", err
	}
	dbPath = filepath.Join(stateDir, workspaceID+".db")
	if !fileExists(dbPath) && strings.TrimSpace(os.Getenv("FOREMANKIT_STATE_DIR")) == "" {
		mainRoot := evalSymlinksBestEffort(MainRoot(dir))
		for _, root := range []string{mainRoot, canonicalRoot} {
			if root == "" {
				continue
			}
			legacy := filepath.Join(root, "log", "openspec.db")
			if !fileExists(legacy) {
				continue
			}
			if migrateLegacyDB(legacy, dbPath) {
				break
			}
			// The move failed (e.g. cross-device); keep using the legacy file rather than losing state.
			return evalSymlinksBestEffort(legacy), workspaceID, canonicalRoot, nil
		}
	}
	return dbPath, workspaceID, canonicalRoot, nil
}

// migrateLegacyDB moves a legacy log/openspec.db (and its WAL/SHM side files) to dst.
func migrateLegacyDB(src, dst string) bool {
	if err := os.Rename(src, dst); err != nil {
		return false
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if fileExists(src + suffix) {
			_ = os.Rename(src+suffix, dst+suffix)
		}
	}
	return true
}

// LegacyLockFile is where an application installed before the kit was called Foreman keeps its lock;
// openspec-kit update moves it to .foreman-kit/kit.lock.
const LegacyLockFile = ".openspec-kit.lock"

// resolveStateDir returns $FOREMANKIT_STATE_DIR, or $FOREMANKIT_HOME/state (default ~/.foreman-kit/state).
// The first time, ~/.openspec-kit, the home of the kit before it was called Foreman, becomes
// ~/.foreman-kit, with the state of every application in it.
func resolveStateDir() (string, error) {
	if stateDir := strings.TrimSpace(os.Getenv("FOREMANKIT_STATE_DIR")); stateDir != "" {
		return stateDir, nil
	}
	kitHome := strings.TrimSpace(os.Getenv("FOREMANKIT_HOME"))
	if kitHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		kitHome = filepath.Join(home, ".foreman-kit")
		legacy := filepath.Join(home, ".openspec-kit")
		if !dirExists(kitHome) && dirExists(legacy) {
			_ = os.Rename(legacy, kitHome)
		}
	}
	return filepath.Join(kitHome, "state"), nil
}

// RuntimeDir returns the per-workspace directory for runtime files (daemon log and pid,
// cycle logs and reports, worker logs, scratch dirs, locks, PAUSE). It lives next to the
// workspace's SQLite database: <dir of db>/<slug>-<hash12>/. All worktrees of a workspace
// share it. The directory is created if missing. The umbrella's log/ is never used.
func RuntimeDir(dir string) string {
	workspaceID, _ := WorkspaceID(dir)
	var base string
	if envPath := strings.TrimSpace(os.Getenv("FOREMANKIT_DB_PATH")); envPath != "" {
		if abs, err := filepath.Abs(envPath); err == nil {
			base = filepath.Dir(abs)
		}
	}
	if base == "" {
		if sd, err := resolveStateDir(); err == nil {
			base = sd
		} else {
			base = filepath.Join(os.TempDir(), "foreman-state")
		}
	}
	rd := filepath.Join(base, workspaceID)
	_ = os.MkdirAll(rd, 0o755)
	return rd
}

// RuntimePath joins elem onto RuntimeDir(dir).
func RuntimePath(dir string, elem ...string) string {
	return filepath.Join(append([]string{RuntimeDir(dir)}, elem...)...)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// IsApplication reports whether dir holds a Foreman application: the lock `init` writes, the lock of
// an application installed before the kit was called Foreman, or the kit's own settings files.
func IsApplication(dir string) bool {
	if dir == "" {
		return false
	}
	return fileExists(filepath.Join(dir, ".foreman-kit", "kit.lock")) ||
		fileExists(filepath.Join(dir, LegacyLockFile)) ||
		fileExists(filepath.Join(dir, ".foreman-kit", "pipeline.yaml")) ||
		fileExists(filepath.Join(dir, ".foreman-kit", "agents.yaml"))
}
