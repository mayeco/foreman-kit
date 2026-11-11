package gitx

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// BlobAt returns the contents of relPath as the commit rev of the repository at dir records it.
// found is false, with no error, only when git says that the commit has no file there. A revision
// that does not exist, an object that cannot be loaded or a git that could not run is an error, never
// "it has no such file": a caller that reconciles files would take it for a file somebody deleted.
//
// It asks git and not go-git, whose Tree.File answers "file not found" for an object it cannot load
// as it does for a path the tree does not have.
func BlobAt(ctx context.Context, dir, rev, relPath string) (content []byte, found bool, err error) {
	rel := filepath.ToSlash(strings.TrimPrefix(relPath, "/"))
	listed, err := RunGit(ctx, dir, "--literal-pathspecs", "ls-tree", "-z", "--full-tree", rev, "--", rel)
	if err != nil {
		return nil, false, err
	}
	if listed == "" {
		return nil, false, nil
	}
	// One entry: "<mode> <type> <object>\t<path>\0".
	meta, _, ok := strings.Cut(strings.TrimSuffix(listed, "\x00"), "\t")
	fields := strings.Fields(meta)
	if !ok || len(fields) != 3 {
		return nil, false, fmt.Errorf("git ls-tree %s -- %s: cannot read its answer %q", rev, rel, listed)
	}
	if fields[1] != "blob" {
		// A directory or a child repository stands there: the commit has no file at this path.
		return nil, false, nil
	}
	text, err := RunGit(ctx, dir, "cat-file", "blob", fields[2])
	if err != nil {
		return nil, false, err
	}
	return []byte(text), true, nil
}
