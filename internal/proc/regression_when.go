package proc

import (
	"fmt"
	"strings"
)

// RegressionWhenMatch is how a repository's regression.when entries classify a set of changed files.
type RegressionWhenMatch struct {
	// Hit is the first file that triggers the regression: it matches a positive entry and no exclusion.
	// Empty when nothing triggers it.
	Hit string
	// Excluded is the first file that matched a positive entry but an exclusion removed, and Exclusion
	// is that exclusion as written (with its leading "!"). Both are empty when no exclusion applied.
	Excluded  string
	Exclusion string
}

// Triggered reports whether a file triggers the regression.
func (m RegressionWhenMatch) Triggered() bool { return m.Hit != "" }

// ExcludedNote is the one line the gate output gets when an exclusion is the only reason the
// regression does not run: a file matched a positive entry, an exclusion removed it, and no other
// file triggers the regression. It is empty otherwise.
func (m RegressionWhenMatch) ExcludedNote(repo string) string {
	if m.Hit != "" || m.Excluded == "" {
		return ""
	}
	return fmt.Sprintf("---- [info] %s: %s matches regression.when but %s excludes it", repo, m.Excluded, m.Exclusion)
}

// splitRegressionWhen separates regression.when entries into positive globs and exclusions (an entry
// starting with "!", kept with its "!"). Empty entries and the invalid "!" and "!!..." forms, which
// config check reports, are ignored.
func splitRegressionWhen(patterns []string) (positive, exclusions []string) {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
		case strings.HasPrefix(p, "!"):
			rest := strings.TrimSpace(p[1:])
			if rest == "" || strings.HasPrefix(rest, "!") {
				continue
			}
			exclusions = append(exclusions, "!"+rest)
		default:
			positive = append(positive, p)
		}
	}
	return positive, exclusions
}

// MatchRegressionWhen classifies files under a repository's regression.when entries. A file triggers
// the regression if and only if it matches at least one positive entry and no exclusion (an entry
// starting with "!"); unlike .gitignore, the order of the entries does not matter. Each glob uses the
// MatchRegressionGlob syntax.
func MatchRegressionWhen(patterns, files []string) RegressionWhenMatch {
	positive, exclusions := splitRegressionWhen(patterns)
	var m RegressionWhenMatch
	if len(positive) == 0 {
		return m
	}
	for _, f := range files {
		if strings.TrimSpace(f) == "" {
			continue
		}
		matched := false
		for _, g := range positive {
			if MatchRegressionGlob(g, f) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		excludedBy := ""
		for _, x := range exclusions {
			if MatchRegressionGlob(x[1:], f) {
				excludedBy = x
				break
			}
		}
		if excludedBy == "" {
			m.Hit = f
			return m
		}
		if m.Excluded == "" {
			m.Excluded, m.Exclusion = f, excludedBy
		}
	}
	return m
}

// RegressionHit reports whether any of files triggers the regression under the regression.when
// patterns (see MatchRegressionWhen), and the first file that does. Every place that evaluates
// regression.when uses it: foreman-kit gates, the dispatcher's BUILD verification and the merger.
func RegressionHit(patterns, files []string) (hit string, ok bool) {
	m := MatchRegressionWhen(patterns, files)
	return m.Hit, m.Triggered()
}
