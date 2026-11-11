package portal

import (
	"fmt"
	"os"
	"strings"
)

// Small reads the services share: whether a path is a file or a directory, and a value or a text
// written the way a message says it.

// isFile reports whether p is a file.
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// isDir reports whether p is a directory.
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// asStr writes a value as text: "" for none, and a text as it is.
func asStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// firstNonEmptyLine returns the first line of s that says something, cut to what fits in a message,
// or fallback when s says nothing.
func firstNonEmptyLine(s, fallback string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 110 {
				return line[:110]
			}
			return line
		}
	}
	return fallback
}
