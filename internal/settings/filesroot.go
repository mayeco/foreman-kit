package settings

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/store"
)

// FilesRoot returns the checkout the settings of the application at root are read from: root, or
// the main checkout when root is one of its worktrees and was cut before `foreman-kit update`
// rewrote the files.
//
// Such a worktree, the one of a change that was being built, still holds the files an older
// foreman-kit read. None of their keys is read: the change goes on with the settings the
// application has now, in the main checkout, instead of the values the kit ships. A worktree whose
// files are of today reads its own, and so does one of an application nobody has updated yet: its
// main checkout holds the former files too, and what reads them says what rewrites them.
func FilesRoot(root string) string {
	if root == "" {
		root = store.AppRoot("")
	}
	main := store.MainRoot(root)
	if main == "" || sameDir(main, root) {
		return root
	}
	if !ofAnOlderKit(root) || ofAnOlderKit(main) {
		return root
	}
	return main
}

// ofAnOlderKit reports whether a settings file of the checkout at dir writes a key, or a value, an
// older foreman-kit read.
func ofAnOlderKit(dir string) bool {
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			continue
		}
		var data map[string]any
		if yaml.Unmarshal(raw, &data) != nil {
			continue
		}
		if HasFormer(f.Name, data) {
			return true
		}
	}
	return false
}

func sameDir(a, b string) bool {
	if absA, err := filepath.Abs(a); err == nil {
		a = absA
	}
	if absB, err := filepath.Abs(b); err == nil {
		b = absB
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
