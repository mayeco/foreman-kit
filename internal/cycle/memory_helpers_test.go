package cycle

import (
	"os"
	"path/filepath"

	"github.com/mayeco/foreman-kit/internal/memory"
)

// memRel is the file of the memory the tests of the record write lessons to, relative to a checkout.
const memRel = memory.PitfallsFile

// memPath returns where memRel is in the checkout at dir, with its directory in place.
func memPath(dir string) string {
	full := filepath.Join(dir, filepath.FromSlash(memRel))
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	return full
}
