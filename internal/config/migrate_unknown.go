package config

import (
	"fmt"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// unknownKeyNotes names, for the file f as the application wrote it, each key it holds that no
// setting of the catalog reads and that is no former key nor content of the file (a typo): the
// rewrite writes only the keys of today, so the value would be lost without a word. values holds
// what the rewrite writes for each setting, to say what stands where the key was meant.
func (c *configData) unknownKeyNotes(f settings.File, values map[settings.Key]any) []string {
	current, present := c.data[f.Name]
	if !present {
		return nil
	}
	var notes []string
	r := (&settings.Reader{Root: c.root, FilesAlone: true}).Use(f.Name, current)
	for _, p := range r.Problems(f.Name) {
		if !p.Unknown {
			continue
		}
		note := fmt.Sprintf("  %s: REVIEW: %s is not a setting of this file and was dropped", p.Key.Path(), settings.Inline(digMap(current, p.Key.Parts()...)))
		if s, ok := closestSetting(f, p.Key, current); ok {
			written, set := values[s.Key]
			if !set {
				written = s.Shipped()
			}
			if sameSetting(s, written, s.Shipped()) {
				note += fmt.Sprintf("; did you mean %s? the kit's value %s was written", s.Key.Path(), settings.Inline(s.Shipped()))
			} else {
				note += fmt.Sprintf("; did you mean %s? it holds %s in the file written", s.Key.Path(), settings.Inline(written))
			}
		}
		notes = append(notes, note)
	}
	return notes
}

// closestSetting returns the setting of the file f whose path is nearest to the one of key, when it
// is near enough to be a typo of it and the application did not set it already.
func closestSetting(f settings.File, key settings.Key, current map[string]any) (settings.Setting, bool) {
	typed := key.Path()
	limit := len(typed) / 3
	if limit < 1 {
		limit = 1
	}
	if limit > 3 {
		limit = 3
	}
	var best settings.Setting
	found, bestDistance := false, limit+1
	for _, s := range settings.In(f.Name) {
		if s.Key == settings.AgentCommands || s.Key == settings.ExplorationStrategies || digMap(current, s.Key.Parts()...) != nil {
			continue
		}
		if d := editDistance(typed, s.Key.Path()); d < bestDistance {
			best, found, bestDistance = s, true, d
		}
	}
	return best, found
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
