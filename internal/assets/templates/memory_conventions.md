<!--
The conventions a new application's memory starts with (.agent-memory/conventions.md): what the kit
establishes about every application it runs. `foreman-kit init` writes them through
`agent-memory propose`; from then on they belong to the application, like the rest of its memory.
This comment is not written.
-->
- **Topology**: application code lives in child repositories under `repos/`; specifications live only in this umbrella's `openspec/`.
- **Quality gates**: each child repo declares its gates in `repos/<name>/repo.config.yaml`; `foreman-kit gates` runs the gates of the repos a change touched, and `foreman-kit exec <name> <command>` runs any command in that repo's isolated slot environment (`FOREMANKIT_SLOT`, `TEST_ENV_NUMBER`, `PORT`).
- **Specification authoring**: every scenario header uses four hashtags (`#### Scenario: <Name>`), every `### Requirement:` descriptive paragraph is at most 500 characters, and every new capability includes a `## Purpose` section of at least 50 characters without `TBD`.
- **State model**: Git-tracked files (`.foreman-kit/agent_backlog.json`, `.foreman-kit/agent_history.json`, `.foreman-kit/agent_proposals.json`, `.agent-memory/`) carry cross-machine state; a SQLite database outside the repository (`~/.foreman-kit/state/`) holds local runtime queues, locks, gate runs, repair rounds, engine health and caches.
- **Memory**: `.agent-memory/` is this workspace's long-term memory, kept with `agent-memory`: `conventions.md`, `pitfalls.md` and `decisions.md` hold what applies to every change, `modules/repo-<name>.md` and `modules/spec-<capability>.md` what applies to one child repository or capability. Workers record lessons, each with its evidence, in `openspec/changes/<change>/handoff.md`; only the scheduled cycle and the visual QA session write the memory, with `agent-memory propose`. It holds durable, non-obvious facts: in-flight state, partial diffs and debug output do not belong in it.
- **Language**: all specifications, code, comments, documentation and commits are professional English.
