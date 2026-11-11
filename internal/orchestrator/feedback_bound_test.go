package orchestrator

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

// feedbackBoundMaxArg is the size at which Linux refuses one execve argument (MAX_ARG_STRLEN, 32 pages
// of 4 KiB). Every built-in engine command receives the whole prompt as one argument.
const feedbackBoundMaxArg = 128 << 10

// A gate that prints one very long line (a minified bundle or a base64 blob in a failed assertion) must
// not make the repair prompt larger than one command-line argument may be: the engine then cannot even
// start ("argument list too long"), is retried, and the role ends as an engine outage. The end of the
// output, which names the failure, is what the agent is given.
func TestRepairPromptStaysWithinOneArgumentAfterAHugeGateLine(t *testing.T) {
	root, _, _, st := setupTestWorkspaceWithSubmodule(t)
	const change = "huge-gate-line"
	writeBacklogJSON(t, root, map[string]any{"change_name": change, "description": "Its first build fails a gate that prints a huge line", "priority": "high"})

	const failure = "AssertionError: expected the bundle to be empty"
	var gates atomic.Int32
	gate := func(ctx context.Context, req GateRunRequest) (GateRunResult, error) {
		if gates.Add(1) > 1 {
			return GateRunResult{Output: "gates: PASS (1 executed)\n", OK: true}, nil
		}
		// 2 MiB on one line, in two-byte characters, then the lines that name the failure.
		out := "---- [gate] backend:test: npm test\n" + strings.Repeat("é", 1<<20) + "\n" + failure + "\n---- [FAIL] backend:test (exit 1)\ngates: FAIL (backend:test)\n"
		return GateRunResult{Output: out, ExitCode: 1, OK: false, Failures: []GateFailure{{Child: "repos/backend", Gate: "test", ExitCode: 1}}}, nil
	}
	var mu sync.Mutex
	var repairPrompts []string
	stub := continuousStubAgent()
	agent := func(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
		if strings.HasPrefix(req.Phase, "repair-") {
			mu.Lock()
			repairPrompts = append(repairPrompts, req.Prompt)
			mu.Unlock()
		}
		return stub(ctx, req)
	}
	disp, err := NewDispatcher(DispatcherOptions{Root: root, Store: st, MaxWorkers: 1, AgentRunner: agent, GateRunner: gate, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()
	if err := disp.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(repairPrompts) != 1 {
		t.Fatalf("want one repair round after the failed gate, got %d", len(repairPrompts))
	}
	prompt := repairPrompts[0]
	if len(prompt) >= feedbackBoundMaxArg {
		t.Errorf("the repair prompt is %d bytes: execve refuses an argument of %d bytes or more", len(prompt), feedbackBoundMaxArg)
	}
	if !strings.Contains(prompt, failure) || !strings.Contains(prompt, "gates: FAIL (backend:test)") {
		t.Error("the repair prompt lost the end of the gate output, which names the failure")
	}
	if !utf8.ValidString(prompt) {
		t.Error("the gate output was cut inside a character")
	}
}

func TestBoundedFeedbackKeepsTheEndAndWholeCharacters(t *testing.T) {
	if got := boundedFeedback("short"); got != "short" {
		t.Errorf("feedback within the bound must be quoted whole, got %q", got)
	}
	// Both parities of the cut position: one of them falls inside a two-byte character.
	for _, prefix := range []string{"a", "ab"} {
		long := prefix + strings.Repeat("é", maxFeedbackBytes) + "\nthe failure"
		got := boundedFeedback(long)
		if len(got) > maxFeedbackBytes+100 {
			t.Errorf("prefix %q: bounded feedback is %d bytes, want about %d", prefix, len(got), maxFeedbackBytes)
		}
		if !strings.HasSuffix(got, "\nthe failure") {
			t.Errorf("prefix %q: the end of the feedback was not kept", prefix)
		}
		if !utf8.ValidString(got) {
			t.Errorf("prefix %q: the feedback was cut inside a character", prefix)
		}
		if !strings.Contains(got, "of earlier output omitted") {
			t.Errorf("prefix %q: the cut is not announced: %q", prefix, got[:80])
		}
	}
}
