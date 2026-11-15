package assets

import (
	"strings"
	"testing"
)

// Every shipped line that runs `foreman-kit evaluate eval` says what exit code 4 (the gate is off,
// nothing was asked) means, so an agent does not read it as a failure of the evaluator.
func TestSkillsSayWhatExitCode4OfEvaluateEvalMeans(t *testing.T) {
	for _, file := range []string{"skills/foreman-kit-qa/SKILL.md", "skills/foreman-kit-cycle/SKILL.md"} {
		raw, err := FS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		runs := 0
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "foreman-kit evaluate eval") {
				continue
			}
			runs++
			if !strings.Contains(line, "Exit code 4 means the gate is off and nothing was asked") &&
				!strings.Contains(line, "Exit code 4 means the gate is off (") {
				t.Errorf("%s: a line runs `foreman-kit evaluate eval` and does not say what exit code 4 means:\n%s", file, line)
			}
		}
		if runs == 0 {
			t.Errorf("%s: no line runs `foreman-kit evaluate eval`", file)
		}
	}
}
