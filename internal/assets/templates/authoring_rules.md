## Framework rules

These rules come with the Foreman and apply on top of `openspec/config.yaml`, which belongs to this project.

Workspace:
- Application code lives in the child repositories under `repos/`; specifications live only in `openspec/`.
- Agents never commit, push, rebase, reset or switch branches. The dispatcher commits your output on `feat/spec-<change>` branches, and only the merger publishes base branches.
- Canonical specs (`openspec/specs/`) change only through `openspec archive`, which the merger runs. Never edit them.
- Every behavior change is proven by automated tests. `foreman-kit gates` runs the gates each touched repository declares in its `repo.config.yaml`.
- Run any other command of a repository (one test file, a linter, a console, the application) through `foreman-kit exec repos/<name> <command>`: it prepares the toolchain that repository pins. Never edit a repository's `repo.config.yaml` or its `env_script`.
- All code, comments, documentation, specs and commit messages are professional English.

Proposal:
- Keep it concise and self-contained, under 800 words, with Why, What Changes, Capabilities and Impact sections.
- Declare at least one New or Modified Capability, or set `skip_specs: true` in `.openspec.yaml`.
- Never invent capability names: check `openspec list --specs --json` first.
- Name the repositories under `repos/` the change touches and any shared hotspot (migrations, lockfiles, route registries).

Specs:
- Keep each requirement statement at or under 500 characters (`--strict` fails above that).
- Use RFC 2119 keywords for normative statements: SHALL, MUST, SHALL NOT.
- Every requirement has at least one scenario, with a header of exactly four hashtags: `#### Scenario: <Name>`.
- Scenarios use WHEN/THEN (and AND) bullet clauses, and every THEN names an observable result.
- A new capability starts with a `## Purpose` section of at least 50 characters.
- A MODIFIED requirement repeats the complete current block: the statement and every surviving scenario.

Design:
- Focus on architecture, patterns and decision rationale; state Goals and Non-Goals.
- Document trade-offs, security considerations, data migrations and the rollback strategy.

Tasks:
- Group tasks under numbered sections (`## 1. Setup`, `## 2. Core Implementation`).
- Every task is a checkbox, `- [ ] X.Y Description`, and states its verification (a test command or an observable behavior).
- Pair every code task with its automated test task.

Implementation:
- Run `foreman-kit gates` before marking the last task complete.
- If an unexpected data migration or schema change emerges, write the question to `.needs-human` in the change directory and stop.
- Record non-obvious lessons in `handoff.md` in the change directory.
