package portal

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// A list of values the schema allows in a column, read from the migrations: CHECK (column IN (...)),
// or CHECK (column IS NULL OR column IN (...)) for a column that may hold none of them.
var (
	sqlCommentRe = regexp.MustCompile(`--[^\n]*`)
	sqlTableRe   = regexp.MustCompile(`(?i)\b(?:CREATE\s+TABLE|ALTER\s+TABLE)\s+(\w+)`)
	sqlCheckInRe = regexp.MustCompile(`(?i)CHECK\s*\(\s*(?:\w+\s+IS\s+NULL\s+OR\s+)?(\w+)\s+IN\s*\(([^)]*)\)`)
	sqlQuotedRe  = regexp.MustCompile(`'([^']*)'`)
)

// checkLists returns the values the migrations allow in each column that has a list of them, by
// "table.column". A list of numbers (a flag that is 0 or 1) names no state and is left out.
func checkLists(t *testing.T) map[string][]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "store", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("the migrations of the store were not found: %v", err)
	}
	sort.Strings(files)
	lists := map[string][]string{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := sqlCommentRe.ReplaceAllString(string(raw), "")
		tables := sqlTableRe.FindAllStringSubmatchIndex(text, -1)
		for _, m := range sqlCheckInRe.FindAllStringSubmatchIndex(text, -1) {
			table := ""
			for _, tm := range tables {
				if tm[0] > m[0] {
					break
				}
				table = text[tm[2]:tm[3]]
			}
			var values []string
			for _, q := range sqlQuotedRe.FindAllStringSubmatch(text[m[4]:m[5]], -1) {
				values = append(values, q[1])
			}
			if table == "" || len(values) == 0 {
				continue
			}
			// A later migration that states the list of a column again replaces it.
			lists[table+"."+text[m[2]:m[3]]] = values
		}
	}
	return lists
}

// Lists of the schema that no page shows as a state.
var listsThatAreNotStates = map[string]string{
	"log_chunks.stream": "which stream of a process a chunk of output came from",
}

// Vocabularies whose values no CHECK of the schema lists.
var vocabulariesWithoutAList = map[string]string{
	vocChangeState:      "computed by v_change_state: its values are store.ChangeStates",
	vocEvaluationStatus: "evaluations.status is written by the evaluator, with no list in the schema",
	vocEvaluationMode:   "evaluations.mode is the mode of the gate when it was asked: its values are settings.GateModes",
	vocCycleWaits:       "store.CycleSchedule.Waits is the daemon's word, kept with its lock and in no column",
	vocProposalStatus:   "proposals.status is mirrored from the proposals file, with no list in the schema",
}

// TestEveryStateOfTheSchemaHasItsWords: a value the schema lets a column hold, and a state a change
// can be in, reads on a page through wordingOf. A value without words would show as it is recorded,
// in the vocabulary of the database and with no color: this test fails first.
func TestEveryStateOfTheSchemaHasItsWords(t *testing.T) {
	lists := checkLists(t)
	if len(lists) < 15 {
		t.Fatalf("only %d lists of values were read from the migrations: the test no longer reads the schema", len(lists))
	}
	for column, values := range lists {
		if _, skip := listsThatAreNotStates[column]; skip {
			continue
		}
		words, ok := vocabularies[column]
		if !ok {
			t.Errorf("the schema lists the values of %s and labels.go has no vocabulary for it (or name it in listsThatAreNotStates)", column)
			continue
		}
		allowed := map[string]bool{}
		for _, v := range values {
			allowed[v] = true
			if w := words[v]; w.Text == "" {
				t.Errorf("%s = %q has no words in labels.go", column, v)
			}
		}
		for v := range words {
			if !allowed[v] {
				t.Errorf("labels.go has words for %s = %q, which the schema does not allow", column, v)
			}
		}
	}
	for column := range listsThatAreNotStates {
		if _, ok := lists[column]; !ok {
			t.Errorf("%s is in listsThatAreNotStates and the schema has no such list", column)
		}
	}

	for _, state := range store.ChangeStates {
		w := vocabularies[vocChangeState][state]
		if w.Text == "" || w.Means == "" {
			t.Errorf("the state %q of a change needs its words and what it means", state)
		}
	}
	for _, mode := range settings.GateModes {
		if w := vocabularies[vocEvaluationMode][mode]; w.Text == "" || w.Means == "" {
			t.Errorf("the mode %q of a gate of the evaluator needs its words and what it means", mode)
		}
	}
	if len(vocabularies[vocEvaluationMode]) != len(settings.GateModes) {
		t.Errorf("labels.go has words for %d modes of a gate of the evaluator, and a gate has %d", len(vocabularies[vocEvaluationMode]), len(settings.GateModes))
	}
	if len(vocabularies[vocChangeState]) != len(store.ChangeStates) {
		t.Errorf("labels.go has words for %d states of a change, and a change has %d", len(vocabularies[vocChangeState]), len(store.ChangeStates))
	}

	for name, words := range vocabularies {
		_, listed := lists[name]
		_, declared := vocabulariesWithoutAList[name]
		switch {
		case !listed && !declared:
			t.Errorf("the vocabulary %s translates no list of the schema: remove it, or say in vocabulariesWithoutAList where its values come from", name)
		case listed && declared:
			t.Errorf("the schema now lists the values of %s: take it off vocabulariesWithoutAList", name)
		}
		for value, w := range words {
			switch w.Tone {
			case "", "info", "success", "warning", "error":
			default:
				t.Errorf("%s = %q has the tone %q, which is not one a badge has", name, value, w.Tone)
			}
			if strings.Contains(w.Text, "_") {
				t.Errorf("%s = %q reads %q: the words of a state are not the name of its value", name, value, w.Text)
			}
		}
	}
}

func TestAValueWithoutWordsShowsAsRecorded(t *testing.T) {
	if got := wordingOf(vocExecutionStatus, "timed_out"); got.Text != "timed out" || got.Tone != "error" {
		t.Errorf("wordingOf(executions.status, timed_out) = %+v", got)
	}
	if got := wordingOf(vocExecutionStatus, "half_done"); got.Text != "half done" || got.Tone != "" {
		t.Errorf("a value without words = %+v, want it as recorded with no tone", got)
	}
	if got := wordingOf("no.such_column", "x"); got.Text != "x" {
		t.Errorf("a vocabulary that does not exist = %+v", got)
	}
}
