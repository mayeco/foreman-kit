# {{PROJECT_NAME}}

{{PROJECT_DESCRIPTION}}

## Repositories

| Path                  | Purpose          | Base branch       |
| :-------------------- | :--------------- | :---------------- |
| `repos/{{REPO_NAME}}` | {{REPO_PURPOSE}} | `{{BASE_BRANCH}}` |

## Getting started

{{GETTING_STARTED}}

## How this project is developed

Changes are specified with [OpenSpec](https://openspec.dev) and built by AI coding agents orchestrated by `openspec-kit` (a single static Go binary backed by SQLite WAL at `log/openspec.db`):

- Behavior lives in `openspec/specs/`. Each change is proposed, specified, built and archived under `openspec/changes/`.
- Work is queued in `config/agent_backlog.json`. `openspec-kit dispatch` plans and builds each item in an isolated worktree, and `openspec-kit merge` (or `openspec-kit merge --batch`) integrates it once the quality gates pass. Each repository declares its gates in `repos/<name>/repo.config.yaml`, and optionally an `env_script` that prepares its toolchain; `openspec-kit gates` runs the gates, and `openspec-kit exec repos/<name> <command>` runs any other command of a repository in its isolated slot environment.
- `config/agents.yaml` lists the AI coding agents the project uses, the commands that run each one (`commands:`, run exactly as written: the kit adds nothing to them), and, in order of preference, the ones that run each role (the next runs when one fails): `plan` (specifies each item), `build` (implements it) and `cycle` (reviews the work). With `selection: random` each call starts with any healthy agent of its role instead, and with `selection: least_busy` with the one the fewest running calls are using (skipping engines on quota or auth cooldown in the `engines` table), so the work spreads evenly across quotas.
- `ROADMAP.md` lists the features to build, in priority order; the cycle proposes the next ones as it goes.
- An hourly cycle (`openspec-kit cycle`, or the per-project daemon `openspec-kit -d -w`, which also merges and dispatches continuously in between) verifies what was integrated, looks for defects and improvements, queues new work and curates `MEMORY.md`, the project's long-term memory; a visual QA session (`openspec-kit visual-qa`) in the Claude or Hermes app tests UI-affecting changes in a browser.
- `openspec-kit status` shows the state of the pipeline at any time, `openspec-kit progress --since 24h` reports throughput and health, and `openspec-kit -w` (`openspec-kit portal`) or `openspec-kit -d -w` launches the embedded real-time HTTP/SSE web dashboard (`http://127.0.0.1:4567`); or ask your agent, which uses the `openspec-kit-status` skill.
- The settings live in seven files under `config/`, one per reader: `pipeline.yaml` (the dispatcher and the merger), `agents.yaml` (every agent call), `cycle.yaml` (the cycle), `daemon.yaml` (the daemon: when the cycle runs, the portal), `visual_qa.yaml` (the visual session), `exploration.yaml` (what the cycle and the visual session look for) and `evaluator.yaml` (every gate the evaluator answers). Every key is written in its file, with times in their unit (`30s`, `15m`, `2h`, `never`). `openspec-kit config show` explains every setting, `openspec-kit config check` validates them, `openspec-kit config set <file>.<key> <value>` changes one, `openspec-kit config recommend` lists the ones the kit's own records argue for changing, and the `openspec-kit-config` skill does it with you; `openspec-kit setup status` lists what is left to set up, and the `openspec-kit-init` skill adds a repository.

AI agents read [AGENTS.md](AGENTS.md) first (Claude Code through `CLAUDE.md`): how this repository works, the rules they follow and the project's conventions. The orchestration kit runs through the `openspec-kit` binary (`~/.local/bin/openspec-kit`) and installs its embedded `openspec-kit-*` skills into `.agents/skills/` (copied into the other agents' skills directories); `.openspec-kit.lock` records its version and SHA-256 checksums. Do not edit those skill files here: an upgrade replaces them.
