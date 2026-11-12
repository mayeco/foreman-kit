package cycle

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// An application owns its prompts, its guides and its settings files: `foreman-kit update` never
// replaces one, it proposes the new template beside it. The sentences that told agents where the
// memory is are the exception, because an agent that follows the old ones looks for files that are
// gone: update rewrites those sentences where it finds them word for word, and leaves the rest of
// the file as the application wrote it.

// memoryMentions lists, for each file an application owns, the sentences that named MEMORY.md and
// memory/ and what each one says now. It is every difference between the seeds of the kit that kept
// the memory in those files and the seeds of today, so a file nobody edited comes out as today's seed.
var memoryMentions = []struct {
	file  string
	pairs [][2]string
}{
	{".foreman-kit/prompts/plan.md", [][2]string{
		{"  {{memory}}           the lesson files of memory/ (one repository or capability each), those of the\n                       capabilities this change modifies first; empty while there are none\n",
			"  {{memory}}           the notes of one repository or capability each (.agent-memory/modules/), those\n                       of the capabilities this change modifies first; empty while there are none\n"},
		{"Read MEMORY.md first: it holds this workspace's conventions and the lessons of earlier changes.{{memory}}",
			"Run `agent-memory fetch` first: it prints this workspace's conventions and the index of its memory. Then run\n`agent-memory fetch \"<what this change is about>\"` for the lessons and decisions of earlier changes that match.{{memory}}"},
	}},
	{".foreman-kit/prompts/build.md", [][2]string{
		{"  {{memory}}           the lesson files of memory/ (one repository or capability each), those of the\n                       capabilities this change modifies first; empty while there are none\n",
			"  {{memory}}           the notes of one repository or capability each (.agent-memory/modules/), those\n                       of the capabilities this change modifies first; empty while there are none\n"},
		{"Read MEMORY.md first: it holds this workspace's conventions and the lessons of earlier changes.{{memory}}",
			"Run `agent-memory fetch` first: it prints this workspace's conventions and the index of its memory. Then run\n`agent-memory fetch \"<what this change is about>\"` for the lessons and decisions of earlier changes that match.{{memory}}"},
		{"Do not edit MEMORY.md or memory/: the\n  scheduled cycle curates them from handoff.md.",
			"Do not write to the memory (no\n  `agent-memory propose`, no edit under .agent-memory/): the scheduled cycle curates it from handoff.md."},
	}},
	{".foreman-kit/prompts/cycle.md", [][2]string{
		{"- You MAY write only `MEMORY.md`, files under `memory/`, `.foreman-kit/agent_backlog.json`, `.foreman-kit/agent_proposals.json`, files under `log/cycle/`, and `log/PAUSE` (one line saying why, only for a systemic failure).\n- Any other write pauses the whole system for a human, and nothing you did is recorded.\n- Never commit, push, pull or switch branches; never start or stop the dispatcher or the merger.",
			"- You MAY write only the memory (`.agent-memory/`, with `agent-memory propose`), `.foreman-kit/agent_backlog.json`, `.foreman-kit/agent_proposals.json`, files under `log/cycle/`, and `log/PAUSE` (one line saying why, only for a systemic failure).\n- Any other write pauses the whole system for a human, and nothing you did is recorded.\n- Never commit, push, pull or switch branches; never start or stop the dispatcher or the merger.\n- Run any reproduction that writes state (a visual QA request, a backlog row, an execution, a commit, a push, an agent run) only in a disposable instance under `$TMPDIR` with its own `FOREMANKIT_STATE_DIR`, `FOREMANKIT_DB_PATH` and a portal port of its own (`foreman-kit portal --port <port>`), or in a `foreman-kit sim` stack (`foreman-kit sim init \"$TMPDIR/<dir>\"`), never against this application's own store, portal or remotes; read-only probes (`foreman-kit status`, `foreman-kit progress`, a portal `GET`) may use the real instance."},
	}},
	{".foreman-kit/cycle.yaml", [][2]string{
		{"  # MEMORY.md holds what applies to every change, and every agent reads it on every\n  # call. memory/repos/<name>.md and memory/specs/<capability>.md hold what applies\n  # to one child repository or capability; the PLAN and BUILD prompts point to\n  # them, so only the changes that touch that scope read them. Over a cap, the next\n  # cycle and visual session are told to trim that file first.\n",
			"  # The memory is .agent-memory/, kept with agent-memory: agents read it with\n  # `agent-memory fetch`, and the cycle and the visual session write it with\n  # `agent-memory propose`. conventions.md, pitfalls.md and decisions.md hold what\n  # applies to every change. modules/repo-<name>.md and modules/spec-<capability>.md\n  # hold what applies to one child repository or capability; the PLAN and BUILD\n  # prompts point to them, so only the changes that touch that scope read them.\n  # Over a cap, the next cycle and visual session are told to trim that file first.\n"},
		{" # MEMORY.md\n",
			" # each of conventions.md, pitfalls.md and decisions.md\n"},
		{" # each file under memory/\n",
			" # each file under modules/\n"},
	}},
	{"AGENTS.md", [][2]string{
		{"Read `MEMORY.md` next: it holds the conventions and lessons learned here; `memory/` holds those of one repository or capability.",
			"Run `agent-memory fetch` next: it prints the conventions of this repository and the index of its memory (`.agent-memory/`: conventions, lessons, decisions, and the notes of one repository or capability each), and `agent-memory fetch \"<topic>\"` searches it."},
		{"├── MEMORY.md                long-term memory: what holds for every change\n├── memory/                  what holds for one repository (repos/<name>.md) or capability (specs/<capability>.md)\n",
			"├── .agent-memory/           long-term memory, kept with agent-memory: conventions.md, pitfalls.md and decisions.md hold\n│                            for every change; modules/repo-<name>.md and modules/spec-<capability>.md for one\n│                            repository or capability\n"},
		{"and curates `MEMORY.md` and `memory/` using 3-way conflict-free merges.",
			"and curates the memory (`.agent-memory/`, with `agent-memory propose`) using 3-way conflict-free merges."},
		{"in that change's `handoff.md`, not in `MEMORY.md` or `memory/`.",
			"in that change's `handoff.md`, not in the memory (`.agent-memory/`)."},
		{"- The cycle and the visual QA session write only `MEMORY.md`, `memory/`, the backlog and the proposals.",
			"- The cycle and the visual QA session write only the memory (`.agent-memory/`, with `agent-memory propose`), the backlog and the proposals."},
		{"├── config/                  settings the binary reads, one file per reader: pipeline.yaml, agents.yaml",
			"├── .foreman-kit/            settings the binary reads, one file per reader: pipeline.yaml, agents.yaml"},
		{"The settings live in seven files under `config/`, each key written in its file.",
			"The settings live in seven files under `.foreman-kit/`, each key written in its file."},
	}},
	{"README.md", [][2]string{
		{"queues new work and curates `MEMORY.md`, the project's long-term memory;",
			"queues new work and curates the project's long-term memory (`.agent-memory/`, kept with [agent-memory](https://github.com/xChuCx/agent-memory));"},
		{"The settings live in seven files under `config/`, one per reader:",
			"The settings live in seven files under `.foreman-kit/`, one per reader:"},
	}},
}

// agentsFile is where the commands of the agents are, relative to an application's root.
const agentsFile = ".foreman-kit/agents.yaml"

// allowsKitMemory finds, in the tools a command allows an agent, the kit's own memory command: an
// application that allows it and not agent-memory was written before agents read the memory with it.
var allowsKitMemory = regexp.MustCompile(`Bash\(foreman-kit\s+memory \*\)`)

const allowsAgentMemory = "Bash(agent-memory *)"

// memoryMentionsPending reports whether a file of target still names the memory as it was kept
// before agent-memory.
func memoryMentionsPending(target string) bool {
	for _, m := range memoryMentions {
		raw, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(m.file)))
		if err != nil {
			continue
		}
		for _, pair := range m.pairs {
			if strings.Contains(string(raw), pair[0]) {
				return true
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(agentsFile)))
	return err == nil && allowsKitMemory.Match(raw) && !strings.Contains(string(raw), allowsAgentMemory)
}

// rewriteMemoryMentions rewrites, in the files of target, the sentences that named the memory as it
// was kept before agent-memory, and lets the agents whose commands list the tools they may use run
// agent-memory. It returns the files it changed, relative to target.
func rewriteMemoryMentions(target string, out io.Writer) ([]string, error) {
	var changed []string
	for _, m := range memoryMentions {
		full := filepath.Join(target, filepath.FromSlash(m.file))
		raw, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		text := string(raw)
		for _, pair := range m.pairs {
			text = strings.ReplaceAll(text, pair[0], pair[1])
		}
		if text == string(raw) {
			continue
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, m.file)
		fmt.Fprintf(out, "  %s: it names the memory as it is kept now\n", m.file)
	}
	full := filepath.Join(target, filepath.FromSlash(agentsFile))
	if raw, err := os.ReadFile(full); err == nil && allowsKitMemory.Match(raw) && !strings.Contains(string(raw), allowsAgentMemory) {
		text := allowsKitMemory.ReplaceAllString(string(raw), "${0} "+allowsAgentMemory)
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, agentsFile)
		fmt.Fprintf(out, "  %s: the agents that are told which tools they may use may run agent-memory\n", agentsFile)
	}
	return changed, nil
}
