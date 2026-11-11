# Memory

Curated long-term memory for this workspace. Every agent reads it on every call, so it holds what applies to every change, within `memory.max_chars` of `config/cycle.yaml`. A lesson that holds for one child repository or one capability goes to `memory/repos/<name>.md` or `memory/specs/<capability>.md` instead: the PLAN and BUILD prompts point to those files, so only the changes that touch that scope read them. The scheduled cycle and the visual QA session are the only writers; workers record lessons, each with its evidence, in `openspec/changes/<change>/handoff.md`, and the dispatcher records every repair round beside it in `repairs.md`. Keep only durable, non-obvious facts: conventions, required flags, environment quirks and decisions with their reason. In-flight state, partial diffs and debug output do not belong here.

---

## 1. Project Conventions

- **Topology**: application code lives in child repositories under `repos/`; specifications live only in this umbrella's `openspec/`.
- **Quality gates**: each child repo declares its gates in `repos/<name>/repo.config.yaml`; `openspec-kit gates` runs the gates of the repos a change touched, and `openspec-kit exec <name> <command>` runs any command in that repo's isolated slot environment (`OPENSPEC_SLOT`, `TEST_ENV_NUMBER`, `PORT`).
- **Specification authoring**: every scenario header uses four hashtags (`#### Scenario: <Name>`), every `### Requirement:` descriptive paragraph is at most 500 characters, and every new capability includes a `## Purpose` section of at least 50 characters without `TBD`.
- **State model**: Git-tracked files (`config/agent_backlog.json`, `config/agent_history.json`, `config/agent_proposals.json`, `MEMORY.md`, `memory/`) carry cross-machine state; `log/openspec.db` (SQLite WAL) holds local runtime queues, locks, gate runs, repair rounds, engine health and caches.
- **Language**: all specifications, code, comments, documentation and commits are professional English.

---

## 2. Lessons and Environment Quirks

<!-- The scheduled cycle folds the handoff.md lessons of integrated changes here, or in memory/ when one holds for a single repository or capability: the situation it applies to, what to do, and the change whose evidence showed it. -->

---

## 3. Decisions

<!-- Decisions that are not visible in the code, each with its reason. -->
