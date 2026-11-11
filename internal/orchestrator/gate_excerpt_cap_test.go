package orchestrator

import (
	"fmt"
	"strings"
	"testing"
)

// The RSpec excerpt keeps the Failures block and the head of "Failed examples:", a list with one line
// per failure. The list was meant to be cut at 25 lines and was not, so after a run with a hundred
// failures the gate output ended with nothing but "rspec ./spec/...:NN" lines: the last 120 lines, which
// are what the BUILD agent is told to fix, carried no error message at all.
func TestGateExcerptCapsTheFailedExamplesList(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat(".", 300) + "\n\nFailures:\n")
	for i := 1; i <= 150; i++ {
		fmt.Fprintf(&b, "\n  %d) Invoice %d totals\n     Failure/Error: expect(total).to eq(%d)\n", i, i, i)
	}
	b.WriteString("\nFinished in 12.3 seconds\n400 examples, 150 failures\n\nFailed examples:\n\n")
	for i := 1; i <= 150; i++ {
		fmt.Fprintf(&b, "rspec ./spec/invoice_spec.rb:%d # Invoice %d totals\n", i*10, i)
	}
	b.WriteString("\nRandomized with seed 4242\n")

	excerpt := ExtractGateFailureExcerpt("core:test", b.String(), 1)
	if !strings.Contains(excerpt, "150) Invoice 150 totals") || !strings.Contains(excerpt, "expect(total).to eq(150)") {
		t.Fatalf("the Failures block is not whole in the excerpt")
	}
	idx := strings.Index(excerpt, "Failed examples:")
	if idx < 0 {
		t.Fatalf("no Failed examples section in the excerpt")
	}
	if !strings.Contains(excerpt[idx:], "rspec ./spec/invoice_spec.rb:10 ") {
		t.Fatalf("the first failed example is missing from the excerpt")
	}
	if got := len(strings.Split(strings.TrimRight(excerpt[idx:], "\n"), "\n")); got > 25 {
		t.Errorf("the Failed examples section has %d lines, want at most 25", got)
	}
	// The dispatcher gives the BUILD agent the last 120 lines of the gate output, which ends with this
	// excerpt: they must still show why the examples fail.
	if tail := tailLines(excerpt, 120); !strings.Contains(tail, "Failure/Error:") {
		t.Errorf("the last 120 lines of the excerpt carry no failure message; they start with:\n%s", strings.Join(strings.SplitN(tail, "\n", 4)[:3], "\n"))
	}
}
