package settings

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BranchRe matches valid git branch names.
var BranchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// durationRe matches a time written as whole hours, minutes and seconds, largest unit first.
var durationRe = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

// Never is how a duration, or a count, says "it never happens".
const Never = "never"

// SlotNone is how a slot says "none: the ports and databases of the main checkout".
const SlotNone = "none"

// ParseDuration reads a time written with its unit: 30s, 15m, 2h, or several of them from the
// largest down (1h30m). It accepts whole numbers only and nothing shorter than a second; a bare
// number has no unit and is refused, and so is zero.
func ParseDuration(text string) (time.Duration, bool) {
	text = strings.TrimSpace(text)
	m := durationRe.FindStringSubmatch(text)
	if text == "" || m == nil {
		return 0, false
	}
	var total time.Duration
	for i, unit := range []time.Duration{time.Hour, time.Minute, time.Second} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil || n > math.MaxInt64/int64(time.Hour) {
			return 0, false
		}
		total += time.Duration(n) * unit
	}
	if total <= 0 {
		return 0, false
	}
	return total, true
}

// FormatDuration writes a time the way ParseDuration reads it, with the largest units that leave no
// remainder: 2h, 45m, 1h30m, 90s as 1m30s. Zero is 0s, and a part shorter than a second is dropped.
func FormatDuration(d time.Duration) string {
	seconds := int64(d / time.Second)
	if seconds <= 0 {
		return "0s"
	}
	var b strings.Builder
	if h := seconds / 3600; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m := seconds % 3600 / 60; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s := seconds % 60; s > 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}

// Parse validates a value read from a file, or given with --set, and returns it with the type of
// the setting: an int for KindInt (0 for "never"), a bool for KindBool, a time.Duration for
// KindDuration (0 for "never" and for 0s), an int for KindSlot (0 for "none"), and the trimmed text
// for the kinds written as one word. Lists and mappings are returned as they came when they have
// the right shape; what they hold is checked by whoever reads them. It reports false for a value the
// setting does not accept.
func (s Setting) Parse(raw any) (any, bool) {
	if raw == nil {
		return nil, false
	}
	switch s.Kind {
	case KindInt:
		if text, isText := raw.(string); isText && s.Never && strings.TrimSpace(text) == Never {
			return 0, true
		}
		n, ok := wholeNumber(raw)
		if !ok || n < s.Min {
			return nil, false
		}
		return n, true
	case KindBool:
		b, ok := raw.(bool)
		return b, ok
	case KindText, KindPath:
		text, ok := raw.(string)
		if !ok {
			return nil, false
		}
		text = strings.TrimSpace(text)
		if text == "" && !s.Empty {
			return nil, false
		}
		return text, true
	case KindEnum:
		text, ok := raw.(string)
		if !ok {
			return nil, false
		}
		text = strings.TrimSpace(text)
		for _, v := range s.Values {
			if text == v {
				return text, true
			}
		}
		return nil, false
	case KindBranch:
		if _, isBool := raw.(bool); isBool {
			return nil, false
		}
		text := strings.TrimSpace(fmt.Sprint(raw))
		if !BranchRe.MatchString(text) {
			return nil, false
		}
		return text, true
	case KindDuration:
		text, ok := raw.(string)
		if !ok {
			return nil, false
		}
		text = strings.TrimSpace(text)
		if (s.Never && text == Never) || (s.Zero && text == "0s") {
			return time.Duration(0), true
		}
		d, ok := ParseDuration(text)
		if !ok || d < s.MinDuration {
			return nil, false
		}
		return d, true
	case KindSlot:
		n, ok := ParseSlot(raw)
		if !ok {
			return nil, false
		}
		return n, true
	case KindList:
		switch raw.(type) {
		case []any, []string, []float64, []int:
			return raw, true
		}
		return nil, false
	case KindAgents:
		switch raw.(type) {
		case string, []any, []string:
			return raw, true
		}
		return nil, false
	case KindCommands, KindArgs:
		if _, ok := raw.(map[string]any); ok {
			return raw, true
		}
		return nil, false
	}
	return nil, false
}

// Expected says, in words, what the setting accepts.
func (s Setting) Expected() string {
	switch s.Kind {
	case KindInt:
		var expected string
		switch {
		case s.Min == 0:
			expected = "a whole number, 0 or more"
		case s.Min == 1:
			expected = "a whole number above 0"
		default:
			expected = fmt.Sprintf("a whole number, %d or more", s.Min)
		}
		if s.Never {
			expected += ", or never"
		}
		return expected
	case KindBool:
		return "true or false"
	case KindText, KindPath:
		if s.Empty {
			return `text, or "" for none`
		}
		return "text"
	case KindEnum:
		return oneOf(s.Values)
	case KindBranch:
		return "a branch name"
	case KindDuration:
		expected := "a time with its unit (30s, 15m, 2h)"
		if s.MinDuration > time.Second {
			expected = fmt.Sprintf("a time with its unit (30s, 15m, 2h), %s or more", FormatDuration(s.MinDuration))
		}
		switch {
		case s.Never:
			expected += ", or never"
		case s.Zero:
			expected += ", or 0s"
		}
		return expected
	case KindSlot:
		return "none or a slot number"
	case KindList:
		return "a list"
	case KindAgents:
		return "a list of agents"
	case KindCommands:
		return "a mapping from each agent to its commands"
	case KindArgs:
		return "a mapping from each agent to its arguments"
	}
	return "a value"
}

// Problem says why raw is not a value of the setting, or "" when it is one.
func (s Setting) Problem(raw any) string {
	if _, ok := s.Parse(raw); ok {
		return ""
	}
	problem := "must be " + s.Expected()
	if _, isBool := raw.(bool); isBool && s.Kind == KindEnum {
		problem += ` (YAML reads off, on, yes and no as true or false: quote it, "off")`
	}
	return problem
}

// oneOf joins the words of a fixed set as a sentence does: "a or b", "a, b or c".
func oneOf(values []string) string {
	switch len(values) {
	case 0:
		return "nothing"
	case 1:
		return values[0]
	}
	return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
}

// positiveSlotRe matches a slot number written as text.
var positiveSlotRe = regexp.MustCompile(`^[1-9][0-9]*$`)

// ParseSlot reads the value of a KindSlot: the word none (0), or a positive number.
func ParseSlot(value any) (int, bool) {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if text == SlotNone {
			return 0, true
		}
		if positiveSlotRe.MatchString(text) {
			if n, err := strconv.Atoi(text); err == nil && n > 0 {
				return n, true
			}
		}
		return 0, false
	}
	if _, isBool := value.(bool); isBool {
		return 0, false
	}
	if n, ok := wholeNumber(value); ok && n > 0 {
		return n, true
	}
	return 0, false
}

// number converts a scalar read from YAML or given as text (an int, a float or the text of
// one) to a finite float64. A bool is not a number.
func number(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case uint64:
		f = float64(n)
	case float64:
		f = n
	case float32:
		f = float64(n)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// wholeNumber converts a scalar to an int when it is one: 3, 3.0 and "3" are, 3.5 and true are not.
func wholeNumber(v any) (int, bool) {
	if s, ok := v.(string); ok {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		return n, err == nil
	}
	f, ok := number(v)
	if !ok || f != math.Trunc(f) || math.Abs(f) > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}
