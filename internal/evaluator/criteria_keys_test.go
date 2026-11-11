package evaluator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The criteria of a noul are naturally written with unquoted true/false keys, and the options of a
// choice may be numbered. yaml.v3 decodes a mapping with such a key into map[any]any, which was rejected
// as "expected mapping" (for a choice: "requires at least 2 criteria options"), so the gate could not be
// asked at all.
func TestCriteriaKeysThatAreNotStrings(t *testing.T) {
	tf, err := ParseTemplateBytes([]byte(`questions:
  passed:
    type: noul
    instructions: Did every gate pass?
    criteria:
      true: No gate is marked FAIL.
      false: At least one gate failed.
  severity:
    type: choice
    instructions: How severe is the failure?
    criteria:
      1: Cosmetic.
      2: Functional.
      3: Data loss.
`))
	if err != nil {
		t.Fatalf("ParseTemplateBytes: %v", err)
	}
	var noul map[string]string
	if err := json.Unmarshal(tf.Questions["passed"].Criteria, &noul); err != nil {
		t.Fatal(err)
	}
	if len(noul) != 2 || noul["true"] != "No gate is marked FAIL." || noul["false"] != "At least one gate failed." {
		t.Fatalf("noul criteria sent to Jev: %v", noul)
	}
	var choice map[string]string
	if err := json.Unmarshal(tf.Questions["severity"].Criteria, &choice); err != nil {
		t.Fatal(err)
	}
	if len(choice) != 3 || choice["1"] != "Cosmetic." || choice["3"] != "Data loss." {
		t.Fatalf("choice options sent to Jev: %v", choice)
	}
	if got := tf.RawQuestions["passed"].ChoiceCriteria()["true"]; got != "No gate is marked FAIL." {
		t.Fatalf("ChoiceCriteria()[true] = %q", got)
	}
}

// The same through .foreman-kit/evaluator.yaml, whose questions are rebuilt into a template for every gate.
func TestCriteriaKeysThatAreNotStringsInThePolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".foreman-kit"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := `model: jev-1.14.0
gates:
  plan_review:
    mode: enforce
    assert:
      - "single_concern >= 0.6"
    questions:
      single_concern:
        type: noul
        instructions: Does the change pursue a single goal?
        criteria:
          true: Every part of the proposal serves the same goal.
          false: The change bundles unrelated goals.
`
	if err := os.WriteFile(filepath.Join(root, ".foreman-kit", "evaluator.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	pol, err := LoadPolicy(root)
	if err != nil {
		t.Fatal(err)
	}
	tf, _, err := LoadGateTemplate(root, "plan_review", pol)
	if err != nil {
		t.Fatalf("LoadGateTemplate: %v", err)
	}
	if !strings.Contains(string(tf.Questions["single_concern"].Criteria), `"true":"Every part of the proposal serves the same goal."`) {
		t.Fatalf("criteria sent to Jev: %s", tf.Questions["single_concern"].Criteria)
	}
}

// Two keys that read as one name (1 and 1.0, which YAML keeps apart) leave no single meaning for it,
// and a null key names nothing.
func TestCriteriaKeysThatCollideOrAreNullAreRejected(t *testing.T) {
	for criteria, want := range map[string]string{
		"1: Yes.\n      1.0: Also yes.": `key "1" is given twice`,
		"~: Nothing.\n      false: No.": "null key",
	} {
		_, err := ParseTemplateBytes([]byte("questions:\n  passed:\n    type: noul\n    instructions: Did every gate pass?\n    criteria:\n      " + criteria + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("criteria %q: got error %v, want one saying %q", criteria, err, want)
		}
	}
}
