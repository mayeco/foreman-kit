package config

import (
	"encoding/json"
	"os"
	"strings"
)

// BacklogProblem returns why the backlog file at path cannot be used ("" when it is valid JSON or
// missing), for status and dispatch --dry-run.
func BacklogProblem(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		return err.Error()
	}
	if strings.TrimSpace(string(raw)) == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err.Error()
	}
	return ""
}
