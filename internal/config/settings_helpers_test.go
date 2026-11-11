package config

import (
	"fmt"
	"io"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// pipelineKey returns the catalog key of a setting of .foreman-kit/pipeline.yaml named as the file writes it.
func pipelineKey(path string) settings.Key {
	return settings.Key(settings.BaseBranch.File() + "." + path)
}

// pipelineValue returns the value in effect for a key of .foreman-kit/pipeline.yaml, with the notes about
// unusable values on stderr.
func pipelineValue(root, path string, stderr io.Writer) (any, error) {
	key := pipelineKey(path)
	if _, ok := settings.Lookup(key); !ok {
		return nil, fmt.Errorf("unknown setting %s", path)
	}
	return (&settings.Reader{Root: root, Stderr: stderr}).Value(key), nil
}

func pipelineString(root, path string) string { return settings.String(root, pipelineKey(path)) }
func pipelineInt(root, path string) int       { return settings.Int(root, pipelineKey(path)) }
