package config

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Values of merge.outside_commits, as the settings catalog defines them.
const (
	OutsideCommitsVerify  = settings.OutsideCommitsVerify
	OutsideCommitsIgnore  = settings.OutsideCommitsIgnore
	OutsideCommitsInclude = settings.OutsideCommitsInclude
)

// AppRoot locates the application checkout or worktree containing dir.
func AppRoot(dir string) string {
	return store.AppRoot(dir)
}

// MainRoot resolves the main checkout when root is inside .worktrees/<name> or repos/<name>.
func MainRoot(root string) string {
	return store.MainRoot(root)
}

// KitFile returns filepath.Join(root, rel) if it exists on disk, otherwise empty string.
func KitFile(rel, root string) string {
	if root == "" {
		root = AppRoot("")
	}
	local := filepath.Join(root, filepath.FromSlash(rel))
	if fileExists(local) {
		return local
	}
	return local
}

// ReadKitFileBytes reads a file from root/rel if present, falling back to embedded templates or seeds.
func ReadKitFileBytes(root, rel string) ([]byte, error) {
	if root != "" {
		local := filepath.Join(root, filepath.FromSlash(rel))
		if b, err := os.ReadFile(local); err == nil {
			return b, nil
		}
	}
	if strings.HasPrefix(rel, ".foreman-kit/templates/") || strings.HasPrefix(rel, "templates/") {
		return assets.ReadTemplate(root, rel)
	}
	return assets.ReadSeed(rel)
}

// RunSettingsCLI implements `foreman-kit settings get <key>`: it prints the value in effect for one
// key of .foreman-kit/pipeline.yaml, named and written as the file does (dispatch.workers, gates.timeout).
func RunSettingsCLI(root string, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if root == "" {
		root = AppRoot("")
	}
	if len(args) == 2 && args[0] == "get" {
		pipeline := settings.BaseBranch.File()
		key := settings.Key(pipeline + "." + args[1])
		if s, ok := settings.Lookup(key); !ok || !s.Scalar() {
			var known []string
			for _, s := range settings.In(pipeline) {
				known = append(known, s.Key.Path())
			}
			fmt.Fprintf(stderr, "settings: unknown setting %s (%s)\n", args[1], strings.Join(known, ", "))
			return 2
		}
		r := &settings.Reader{Root: root, Stderr: stderr}
		fmt.Fprintln(stdout, r.Text(key))
		return 0
	}
	fmt.Fprintln(stderr, "Usage: foreman-kit settings get <key>")
	return 2
}

func digMap(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mp, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mp[k]
	}
	return cur
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == math.Trunc(n) {
			return int(n), true
		}
		return 0, false
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	default:
		return 0, false
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
