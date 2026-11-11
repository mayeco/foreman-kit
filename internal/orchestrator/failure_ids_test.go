package orchestrator

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/store"
)

// nodeSpecFailure is node --test's spec reporter output for one failing assertion on a whole HTML page
// (trimmed from a real run): the tail is the middle of the dumped page.
var nodeSpecFailure = `---- [gate] web:base:test: npm run test

> osk-web@0.1.0 test
> node --test test/

✔ health endpoint answers ok (8.1ms)
✖ home page is served (5.308167ms)
✔ unknown page is 404 (1.2ms)
ℹ tests 27
ℹ pass 26
ℹ fail 1

✖ failing tests:

test at test/app.test.js:25:1
✖ home page is served (5.308167ms)
  AssertionError [ERR_ASSERTION]: The input did not match the regular expression /<h1>OSK!<\/h1>/. Input:
  
  '<!doctype html>\n' +
` + strings.Repeat("    '<p>filler</p>\\n' +\n", 45) + `    '</html>\n'
  
      at TestContext.<anonymous> (file:///tmp/test/app.test.js:28:10)
  }
---- [FAIL] web:base:test (exit 1)
`

func TestFailingTestsNamesTheTestsOfCommonRunners(t *testing.T) {
	cases := map[string][]string{
		nodeSpecFailure: {"node:home page is served"},
		"--- FAIL: TestLoad (0.00s)\n    store_test.go:23: bad\n--- FAIL: TestOther (0.01s)\nFAIL": {"go:TestLoad", "go:TestOther"},
		"TAP version 13\nok 1 - fine\nnot ok 2 - home page is served\n  ---\n":                     {"tap:home page is served"},
		"FAILED tests/test_x.py::test_login - AssertionError: no\n":                                {"pytest:tests/test_x.py::test_login"},
		"rspec ./spec/models/user_spec.rb:12 # User validates\n":                                   {"rspec:./spec/models/user_spec.rb:12"},
		"  ✕ renders the header (12 ms)\n  ● Header › renders the header\n":                        {"jest:Header › renders the header", "jest:renders the header"},
		"panic: runtime error\nexit status 2\n":                                                    nil,
	}
	for out, want := range cases {
		if got := FailingTests(out); !reflect.DeepEqual(got, want) {
			t.Errorf("FailingTests(%.40q) = %q, want %q", out, got, want)
		}
	}
}

// TestFailureExcerptLeadsWithTheFailingTest: the alert of a broken base used to show the last 40 lines
// of the output, which for node --test were the middle of a dumped HTML page.
func TestFailureExcerptLeadsWithTheFailingTest(t *testing.T) {
	ex := FailureExcerpt(nodeSpecFailure)
	first := strings.SplitN(ex, "\n", 4)
	if !strings.HasPrefix(first[0], "✖ home page is served") || !strings.Contains(first[1], "AssertionError") {
		t.Fatalf("excerpt does not lead with the failing test and its message:\n%s", ex)
	}
	if !strings.Contains(ex, "---- [FAIL] web:base:test (exit 1)") {
		t.Fatalf("excerpt lost the failing gate:\n%s", ex)
	}
	if n := len(strings.Split(RedactExcerpt(ex, 40, 4000), "\n")); n > 40 {
		t.Fatalf("excerpt has %d lines", n)
	}
	if got := FailingTests(RedactExcerpt(ex, 40, 4000)); !reflect.DeepEqual(got, []string{"node:home page is served"}) {
		t.Fatalf("the stored excerpt no longer names the failing test: %q", got)
	}
	if plain := FailureExcerpt("some\nunknown\noutput\n"); plain != "some\nunknown\noutput" {
		t.Fatalf("an output naming no test should fall back to its tail, got %q", plain)
	}
}

// TestAcceptedCoversOnlyTheTestsTheBaseFailed: base accept used to accept a whole gate, so a change
// that broke another test of the same gate integrated too.
func TestAcceptedCoversOnlyTheTestsTheBaseFailed(t *testing.T) {
	v := &store.BaseVerification{BaseSHA: "11ed272df60e", Status: store.BaseVerdictAccepted, Excerpt: FailureExcerpt(nodeSpecFailure)}
	same := "---- [gate] web:test: npm run test\n✖ home page is served (3ms)\n---- [FAIL] web:test (exit 1)\n---- [gate] web:secrets: x\n"
	if ok, why := acceptedCovers(v, "web:test", same); !ok {
		t.Fatalf("the same failing test should be accepted: %s", why)
	}
	more := "---- [gate] web:test: npm run test\n✖ home page is served (3ms)\n✖ contact page links home (1ms)\n---- [FAIL] web:test (exit 1)\n"
	ok, why := acceptedCovers(v, "web:test", more)
	if ok || !strings.Contains(why, "node:contact page links home") {
		t.Fatalf("a test the base passed must not be accepted: ok=%v why=%q", ok, why)
	}
	crash := "---- [gate] web:test: npm run test\nSyntaxError: Unexpected token\n---- [FAIL] web:test (exit 1)\n"
	if ok, _ := acceptedCovers(v, "web:test", crash); ok {
		t.Fatal("a failure naming no test, on a base whose failure named one, must not be accepted")
	}
	unknown := &store.BaseVerification{BaseSHA: "abc", Status: store.BaseVerdictAccepted, Excerpt: "core test: failing"}
	if ok, _ := acceptedCovers(unknown, "core:test", crash); !ok {
		t.Fatal("when the accepted base named no test, the gate is accepted whole, as before")
	}
}
