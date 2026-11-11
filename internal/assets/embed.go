package assets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:seeds all:templates all:skills all:portal_ui
var FS embed.FS

// Version and SourceCommit can be injected at build time via -ldflags.
var (
	Version      = "4.0.0-go"
	SourceCommit = "HEAD"
	BuiltAt      = "2026-11-08T00:00:00Z"
)

// ReadTemplate reads a file under .foreman-kit/templates/<rel> from root if present,
// falling back to the embedded templates/<rel>.
func ReadTemplate(root, rel string) ([]byte, error) {
	rel = strings.TrimPrefix(rel, ".foreman-kit/templates/")
	rel = strings.TrimPrefix(rel, "templates/")
	if root != "" {
		local := filepath.Join(root, ".foreman-kit", "templates", filepath.FromSlash(rel))
		if data, err := os.ReadFile(local); err == nil {
			return data, nil
		}
	}
	return FS.ReadFile("templates/" + strings.TrimPrefix(filepath.ToSlash(rel), "/"))
}

// ReadSeed reads an embedded seed file (e.g. "README.md", "gitignore", ".foreman-kit/pipeline.yaml").
func ReadSeed(rel string) ([]byte, error) {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
	if rel == ".gitignore" {
		rel = "gitignore"
	}
	return FS.ReadFile("seeds/" + rel)
}

// SHA256Bytes returns the lowercase hex SHA-256 of b.
func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// WalkEmbedded walks a subtree of the embedded FS ("seeds", "templates", "skills", "portal_ui")
// and returns a map of relative path -> file bytes.
func WalkEmbedded(prefix string) (map[string][]byte, error) {
	out := make(map[string][]byte)
	err := fs.WalkDir(FS, prefix, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := FS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, prefix+"/")
		out[rel] = b
		return nil
	})
	return out, err
}
