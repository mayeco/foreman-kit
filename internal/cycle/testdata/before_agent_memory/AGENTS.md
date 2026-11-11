# AGENTS.md

Guide for any AI agent working in this repository: Claude Code, Codex, GitHub Copilot, Hermes, OpenCode, Google Antigravity, Kilo Code, Cline, pi, oh-my-pi, Crush, Cursor Agent, ZCode (or any custom CLI declared under `commands:` in `config/agents.yaml`), whether a person drives it or the pipeline runs it. Read `MEMORY.md` next: it holds the conventions and lessons learned here; `memory/` holds those of one repository or capability. `README.md` describes the product and `ROADMAP.md` what comes next.

## 1. What this repository is

The umbrella of the application, built specification-first with [OpenSpec](https://openspec.dev) and the `openspec-kit` single-binary Go orchestrator (`CGO_ENABLED=0` + embedded SQLite WAL) that lets AI coding agents specify, build, verify and integrate it.

```text
.
├── openspec/specs/          canonical behavior: what the application does today
├── openspec/changes/        changes in progress, and archive/ once integrated
├── repos/<name>/            the application's code, one git submodule per repository
│   └── repo.config.yaml     its base branch, gates, regression suite and when it runs, dev command, env_script, required services
├── config/                  settings the binary reads, one file per reader: pipeline.yaml, agents.yaml
│                            (including the commands that run each agent), cycle.yaml, daemon.yaml,
│                            visual_qa.yaml, exploration.yaml, evaluator.yaml;
│                            the backlog (agent_backlog.json), history (agent_history.json), and proposals (agent_proposals.json);
│                            prompts/: the agents' prompts (plan.md, build.md, cycle.md)
├── .agents/skills/          the kit's skills (openspec-kit-*) and OpenSpec's
├── MEMORY.md                long-term memory: what holds for every change
├── memory/                  what holds for one repository (repos/<name>.md) or capability (specs/<capability>.md)
└── log/  .worktrees/        runtime state (git-ignored; SQLite WAL database at log/openspec.db)
```

## 2. How the application changes

1. Work is queued in `config/agent_backlog.json` (`change_name`, `priority`, `description`, optional `depends_on` and `locks`): by a person, by the scheduled cycle when it finds a defect, or from an approved proposal (`config/agent_proposals.json`).
2. `openspec-kit dispatch` takes each item into its own isolated worktree (`.worktrees/worker-<change>`, branch `feat/spec-<change>`), initializes submodules with local `--reference` object sharing, and links git-ignored local configs. A **plan** coding agent writes the OpenSpec change (`proposal.md`, `specs/`, `design.md`, `tasks.md`) until `openspec validate --strict` and the native OpenSpec validator pass. A **build** coding agent then implements the tasks in `repos/`, with automated tests, until every task `- [x]` is checked and the repository gates pass. Several changes build in parallel across worker slots (`OPENSPEC_SLOT`, `TEST_ENV_NUMBER`, `PORT`), coordinated by SQLite locks (`locks`) and per-engine cooldown circuit breakers (`engines`).
3. `openspec-kit merge` (or `openspec-kit merge --batch`) integrates each finished change from the merge queue: it rebases touched child submodules and the umbrella branch onto `origin/<base_branch>`, re-runs the gates against the integration base diff, archives the specification with `openspec archive` (the only way `openspec/specs/` changes), and pushes idempotently to `origin`.
4. Every hour the cycle (`openspec-kit cycle`) verifies what was integrated, explores the application, queues defects, proposes improvements and roadmap features, and curates `MEMORY.md` and `memory/` using 3-way conflict-free merges. It queues only what still fits under `queue.max_open` (`config/cycle.yaml`), the number of changes that may be open at once (queued, building, ready, merging, blocked or waiting to be rebuilt): with the queue full it verifies and reports, and an approved proposal waits for room. A visual QA session (`openspec-kit visual-qa`) in an agent app tests UI-affecting changes in a browser.
5. `openspec-kit portal` serves the embedded real-time HTTP + SSE Web Portal (`http://127.0.0.1:4567`) backed by `log/openspec.db`.
6. The per-project daemon (`openspec-kit -d`, add `-w` for the portal) runs steps 2 and 3 continuously: its merger integrates each change as soon as it enters the merge queue, and its dispatcher starts each backlog item as soon as it becomes eligible (a new item, a dependency integrated, an unblock, a retry due) and a worker slot is free. Only the cycle's judgment (step 4) is periodic, every `cycle.every` (`config/daemon.yaml`), and it never waits for a dispatch, nor a merge for it. The daemon also starts it `cycle.every_when_out_of_work` after the last cycle began (`never`: never before `cycle.every`), once a worker is free with nothing to start, the queue has room (`queue.max_open`) and a change was built, blocked or integrated in either of the last two cycles; `openspec-kit daemon status` says when the next cycle runs and what it waits for. When every agent of a role is out (a usage limit, a lost login), the daemon defers the work and tries again after a cooldown that grows from 1 to 15 minutes; it never pauses on its own for that. Without the daemon, a scheduled `openspec-kit cycle --detach` merges, judges and dispatches once per run.

The settings live in seven files under `config/`, each key written in its file. A file that lacks a key, holds a value the key does not accept or writes a key nothing reads stops whatever reads it from starting new work until it is mended; `openspec-kit status` says which (`CONFIG:`) and `openspec-kit config check` lists every problem.

Anything that fails is quarantined in `blocked_changes` (`log/blocked/<change>.json`) with a structured reason and waits for a person; the rest keeps going. Transient reasons (such as a failing gate, a rejected push, or a stale manifest) are retried automatically after `blocked_changes.retry_after` up to `blocked_changes.retries` times (`config/pipeline.yaml`); `openspec-kit status` shows when.

## 3. Rules for every agent

- **Specification first.** Behavior is described in `openspec/specs/`. A change to behavior is an OpenSpec change under `openspec/changes/<change>/`; never edit `openspec/specs/` by hand.
- **OpenSpec authoring constraints.** Every scenario header uses four hashes (`#### Scenario: <Name>`), every `### Requirement:` descriptive paragraph must be at most 500 characters, and every new capability (`openspec/changes/<change>/specs/<cap>/spec.md`) must include a `## Purpose` section of at least 50 characters without `TBD`.
- **Tests prove behavior.** Every behavior change comes with automated tests that assert what its scenarios name.
- **Run a repository's commands in its environment**: `openspec-kit exec <name> <command>` (one test file, a linter, a console, the application). It loads the toolchain the repository pins, enforces a clean UTF-8 non-interactive environment, and assigns an isolated test database and port per worker slot. `openspec-kit gates` runs the gates of what you changed.
- **Never edit the kit**: the `openspec-kit-*` skills are replaced on upgrade, and `openspec-kit verify` / `preflight` fail while they differ.
- **Never write secrets** into the repository, the backlog, the memory, the reports or a prompt.
- **English** for all code, comments, specifications, commits and documentation, unless the project's conventions below say otherwise.

### When the pipeline runs you (plan, build, cycle, visual QA)

Your prompt says which role you have and what you may write. In short:
- Never commit, push, pull, rebase, reset or switch branches: `openspec-kit` does.
- Never edit `repos/*/repo.config.yaml`, a repository's `env_script` or CI configuration: the merger blocks it as `policy_violation`.
- When you resolve a rebase conflict, keep what teammates landed on the base: the merger asks the evaluator (`concurrent_work_loss`) about every line your change removes from commits that arrived after it started, judging their commit messages, and sends the change back (then quarantines it) when that work matters.
- A plan or build agent writes only in `openspec/changes/<its change>/` and `repos/`; it leaves lessons, each with its evidence, in that change's `handoff.md`, not in `MEMORY.md` or `memory/`. The dispatcher records each repair round beside it in `repairs.md`.
- The cycle and the visual QA session write only `MEMORY.md`, `memory/`, the backlog and the proposals. Anything else uncommitted pauses the system.
- When a decision needs a person, write the question to `openspec/changes/<change>/.needs-human` and stop.

### When a person drives you

Use the skills; they hold the procedures and call `openspec-kit`:

| The person asks | Skill | Command |
| :--- | :--- | :--- |
| To set the application up, or add a repository | `openspec-kit-init` | `openspec-kit setup status`, `add-repo`, `repo` |
| What a setting does, to change one, or what the records argue for changing | `openspec-kit-config` | `openspec-kit config show`, `check`, `set`, `recommend` |
| How it is going, what it did, what needs them | `openspec-kit-status` | `openspec-kit progress`, `openspec-kit status` |
| To unblock, approve, pause or run the cycle | `openspec-kit-cycle` ("Operating the cycle") | `openspec-kit cycle`, `openspec-kit proposals` |
| To start/stop the per-project daemon + Web Portal | `openspec-kit-status` | `openspec-kit -d -w`, `openspec-kit daemon status`, `openspec-kit daemon stop` |
| To launch the real-time web dashboard | `openspec-kit-status` | `openspec-kit -w` / `openspec-kit portal` |
| To migrate legacy v3.x file state into SQLite | `openspec-kit-init` | `openspec-kit migrate-from-files` |
| To specify or build a change by hand | `openspec-propose`, `openspec-apply-change` | `openspec` |

Commit, push, approve a proposal or a risky change, or change a setting only when the person asks. A setting must be committed and pushed right away: the next cycle pauses on uncommitted files outside its scope.

## 4. Project conventions

{{PROJECT_CONVENTIONS}}
