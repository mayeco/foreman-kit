# {{PROJECT_NAME}}

{{PROJECT_DESCRIPTION}}

## Repositories

| Path                  | Purpose          | Base branch       |
| :-------------------- | :--------------- | :---------------- |
| `repos/{{REPO_NAME}}` | {{REPO_PURPOSE}} | `{{BASE_BRANCH}}` |

## Getting started

{{GETTING_STARTED}}

## How this project is developed

Changes are specified with [OpenSpec](https://openspec.dev) and built by AI coding agents orchestrated by `foreman-kit` (a single static Go binary backed by SQLite WAL at `log/openspec.db`):

- Behavior lives in `openspec/specs/`. Each change is proposed, specified, built and archived under `openspec/changes/`.
- Work is queued in `.foreman-kit/agent_backlog.json`. `foreman-kit dispatch` plans and builds each item in an isolated worktree, and `foreman-kit merge` (or `foreman-kit merge --batch`) integrates it once the quality gates pass. Each repository declares its gates in `repos/<name>/repo.config.yaml`, and optionally an `env_script` that prepares its toolchain; `foreman-kit gates` runs the gates, and `foreman-kit exec repos/<name> <command>` runs any other command of a repository in its isolated slot environment.
- `.foreman-kit/agents.yaml` lists the AI coding agents the project uses, the commands that run each one (`commands:`, run exactly as written: the kit adds nothing to them), and, in order of preference, the ones that run each role (the next runs when one fails): `plan` (specifies each item), `build` (implements it) and `cycle` (reviews the work). With `selection: random` each call starts with any healthy agent of its role instead, and with `selection: least_busy` with the one the fewest running calls are using (skipping engines on quota or auth cooldown in the `engines` table), so the work spreads evenly across quotas.
- `ROADMAP.md` lists the features to build, in priority order; the cycle proposes the next ones as it goes.
- An hourly cycle (`foreman-kit cycle`, or the per-project daemon `foreman-kit -d -w`, which also merges and dispatches continuously in between) verifies what was integrated, looks for defects and improvements, queues new work and curates the project's long-term memory (`.agent-memory/`, kept with [agent-memory](https://github.com/xChuCx/agent-memory)); a visual QA session (`foreman-kit visual-qa`) in the Claude or Hermes app tests UI-affecting changes in a browser.
- `foreman-kit status` shows the state of the pipeline at any time, `foreman-kit progress --since 24h` reports throughput and health, and `foreman-kit -w` (`foreman-kit portal`) or `foreman-kit -d -w` launches the embedded real-time HTTP/SSE web dashboard (`http://127.0.0.1:4567`); or ask your agent, which uses the `foreman-kit-status` skill.
- The settings live in seven files under `.foreman-kit/`, one per reader: `pipeline.yaml` (the dispatcher and the merger), `agents.yaml` (every agent call), `cycle.yaml` (the cycle), `daemon.yaml` (the daemon: when the cycle runs, the portal), `visual_qa.yaml` (the visual session), `exploration.yaml` (what the cycle and the visual session look for) and `evaluator.yaml` (every gate the evaluator answers). Every key is written in its file, with times in their unit (`30s`, `15m`, `2h`, `never`). `foreman-kit config show` explains every setting, `foreman-kit config check` validates them, `foreman-kit config set <file>.<key> <value>` changes one, `foreman-kit config recommend` lists the ones the kit's own records argue for changing, and the `foreman-kit-config` skill does it with you; `foreman-kit setup status` lists what is left to set up, and the `foreman-kit-init` skill adds a repository.

AI agents read [AGENTS.md](AGENTS.md) first (Claude Code through `CLAUDE.md`): how this repository works, the rules they follow and the project's conventions. The orchestration kit runs through the `foreman-kit` binary (`~/.local/bin/foreman-kit`) and installs its embedded `foreman-kit-*` skills into `.agents/skills/` (copied into the other agents' skills directories); `.foreman-kit/kit.lock` records its version and SHA-256 checksums. Do not edit those skill files here: an upgrade replaces them.
