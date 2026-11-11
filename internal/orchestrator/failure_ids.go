package orchestrator

import (
	"regexp"
	"sort"
	"strings"
)

// Failure lines of common test runners, each capturing the failing test's name.
var failureIDPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"go", regexp.MustCompile(`^\s*--- FAIL: (\S+)`)},
	{"node", regexp.MustCompile(`^\s*✖ (.+?)(?:\s+\([\d.]+m?s\))?$`)},
	{"tap", regexp.MustCompile(`^\s*not ok \d+ - (.+?)(?:\s+#.*)?$`)},
	{"pytest", regexp.MustCompile(`^(?:FAILED|ERROR) (\S+::\S+)`)},
	{"pytest", regexp.MustCompile(`^(\S+::\S+) (?:FAILED|ERROR)\b`)},
	{"rspec", regexp.MustCompile(`^rspec (\./\S+:\d+)`)},
	{"jest", regexp.MustCompile(`^\s*[✕×] (.+?)(?:\s+\(\d+\s*m?s\))?$`)},
	{"jest", regexp.MustCompile(`^\s*● (.+ › .+)$`)},
}

// failureIDLine returns the identity of the failing test a line of runner output names
// ("go:TestX", "node:home page is served"), or "".
func failureIDLine(line string) string {
	line = strings.TrimRight(line, "\r")
	for _, p := range failureIDPatterns {
		if m := p.re.FindStringSubmatch(line); len(m) == 2 {
			name := strings.TrimSpace(m[1])
			// Section headers of the runners' summaries, not tests.
			if name == "" || strings.HasSuffix(name, ":") || strings.HasPrefix(name, "failing tests") {
				return ""
			}
			return p.kind + ":" + name
		}
	}
	return ""
}

// FailingTests returns the sorted, distinct identities of the failing tests a gate's output names,
// or nil when it names none in a format it knows (a crash, a compile error, an unknown runner).
func FailingTests(output string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, line := range strings.Split(output, "\n") {
		if id := failureIDLine(line); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// FailureExcerpt condenses a failed check's output for an alert: the gates that failed and the
// failing tests, each with its first message line, then the end of the output. The end alone is
// often the middle of a dumped value (node's assertion diff of a whole HTML page), with the failing
// test's name far above it.
func FailureExcerpt(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	type entry struct {
		line, message string
		failLabel     bool
	}
	var entries []*entry
	byID := map[string]*entry{}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if strings.HasPrefix(line, "---- [FAIL]") {
			entries = append(entries, &entry{line: line, failLabel: true})
			continue
		}
		id := failureIDLine(line)
		if id == "" {
			continue
		}
		// The message right under it, indented deeper (node's "  AssertionError ...", Go's
		// "    file_test.go:12: ..."). A runner may name a test twice, in its progress and in its
		// summary; the summary's is the one with the message.
		msg := ""
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			next := strings.TrimRight(lines[j], "\r")
			if strings.TrimSpace(next) == "" {
				continue
			}
			if nIndent := len(next) - len(strings.TrimLeft(next, " \t")); nIndent > indent && failureIDLine(next) == "" {
				msg = "  " + strings.TrimSpace(next)
			}
			break
		}
		if e, ok := byID[id]; ok {
			if e.message == "" && msg != "" {
				e.line, e.message = line, msg
			}
			continue
		}
		e := &entry{line: line, message: msg}
		byID[id] = e
		entries = append(entries, e)
	}
	if len(byID) == 0 {
		return tailLines(output, 40)
	}
	var picked []string
	for _, e := range entries {
		if len(picked) >= 25 {
			break
		}
		picked = append(picked, e.line)
		if e.message != "" {
			picked = append(picked, e.message)
		}
	}
	return strings.Join(picked, "\n") + "\n...\n" + tailLines(output, 8)
}

// gateSegment returns the part of a gate run's output that one gate wrote: from its
// "---- [gate] <label>:" header to the next "---- [gate]" header (the whole output when absent).
func gateSegment(output, label string) string {
	start := strings.Index(output, "---- [gate] "+label+":")
	if start < 0 {
		return output
	}
	rest := output[start+len("---- [gate] "):]
	if end := strings.Index(rest, "\n---- [gate] "); end >= 0 {
		return output[start : start+len("---- [gate] ")+end]
	}
	return output[start:]
}
