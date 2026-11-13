# AGENTS.md

Developer guide for AI coding agents working on the **`foreman-kit` Go repository** itself (whether driven directly in this repository or mounted at `repos/foreman-kit` inside a dogfooding OpenSpec umbrella).

---

## 1. What This Repository Is

This repository is the **source code of the `foreman-kit` orchestrator binary** (`module github.com/mayeco/foreman-kit`), NOT an application umbrella workspace.

- It compiles into a single static Go executable (`CGO_ENABLED=0`) for **Linux and macOS** (`amd64` and `arm64`).
- All files that `foreman-kit init` / `foreman-kit update` install into a target umbrella workspace live inside **`internal/assets/`** (`seeds/`, `templates/`, `skills/`, `portal_ui/`) and are embedded via `//go:embed`.
- **Never duplicate `.foreman-kit/` or `skills/` at the root of this repository.** If you are changing default umbrella configurations, prompts, templates, or agent skills, edit them under `internal/assets/`.

---

## 2. Package Architecture

| Package | Responsibility |
| :--- | :--- |
| `cmd/foreman-kit` | CLI root flags (`-d`/`--daemon`, `-w`/`--web`, `--port`, `--bind`) and Cobra subcommands. |
| `internal/assets` | `//go:embed` filesystem exposing `seeds/`, `templates/`, `skills/`, and `portal_ui/`. |
| `internal/store` | Pure-Go SQLite (`modernc.org/sqlite`) store at `~/.foreman-kit/state/<slug>-<hash12>.db` in WAL mode (runtime files — daemon/cycle/worker logs, scratch, PAUSE — in the sibling `~/.foreman-kit/state/<slug>-<hash12>/`; nothing is written under the umbrella `log/`) with separate `writeDB` (`MaxOpenConns(1)`) and `readDB` (`MaxOpenConns(8)`) pools. It owns the relational schema: numbered migrations in `internal/store/migrations/` applied by `PRAGMA user_version`, `STRICT` tables related by foreign keys (`changes` -> `change_runs` -> `executions` -> `execution_attempts` -> `log_chunks` and `attempt_idle_periods`; `gate_rounds` -> `gate_runs`; `repair_rounds`; `change_blocks`, `rebuild_requests`, `risk_approvals`; `ready_manifests` -> `merge_attempts`; `events` pointing at all of them), process locks, and the Git -> SQLite mirror of the backlog. No other package runs SQL: callers use the store's functions and carry the row they work under (`store.Scope`) in the context. |
| `internal/settings` | The settings catalog: the one definition of every key of the seven `.foreman-kit/` files (its file, what it accepts, the value the kit ships, what it decides, who reads it), the one loader of their values (the environment variable that overrides the key, else its file), what keeps a file from being used (`Reader.Problems`), and the keys an older kit read with where each value lives now (`former.go`). It imports only `internal/store`, so every package reads its settings from it. |
| `internal/config` | `foreman-kit config show`, `check` and `set` over the settings catalog; the supported agents and `agents.yaml` (including the `commands:` that run each agent); the rewrite of an application's files from their seeds (`rewrite.go`) and of the files an older kit wrote (`migrate.go`, run by `foreman-kit update`); the prompts. |
| `internal/memory` | The application's long-term memory, `.agent-memory/`, kept with [agent-memory](https://github.com/xChuCx/agent-memory), a program of its own that is run and never contained: the one package that runs it (`init`, `propose`, the section merge, `rebuild-index`), with how to install it when it is missing; which files the memory holds, their caps and the fingerprint recorded with every agent call; and the import of what an application kept in `MEMORY.md` and `memory/` (`legacy.go`). |
| `internal/orchestrator` | `Dispatcher` (`PLAN` -> native + strict OpenSpec validation -> `BUILD` -> gates -> the merge queue, `ready_manifests`) and `Merger` (rebase onto `origin/<base_branch>`, base-diff gates, `openspec archive`, idempotent `PUSHING_CHILDREN` / `PUSHING_ROOT`). |
| `internal/cycle` | Autonomous `RunCycle`, per-project resident daemon (`daemon.go`) with the rule that starts its cycle before the tick (`ondemand.go`), the cap on how many changes may be open at once (`queue.go`), the settings its own records argue for changing (`recommend.go`, over `store.SettingEvidence`), project initialization (`setup.go`), proposals, memory 3-way merge (`memory.go`), and visual QA filtering (`visual_qa.go`). |
| `internal/proc` | Process group execution (`Setpgid`), Linux `/proc/<pid>/stat` and macOS `ps` process-tree inspection, idle watchdog (CPU at the rate of work + worktree + `$TMPDIR` + output; an idle agent is recorded while it happens), slot isolation (`FOREMANKIT_SLOT`), and gate execution. |
| `internal/evaluator` | The evaluator, the model that answers the gates' calibrated questions: native HTTP client of the Jev API (`github.com/shanehull/go-jev`), inline question/criteria loading from `.foreman-kit/evaluator.yaml`, 60 KB state budget clipping (`ClipState`), and SQLite `evaluation_cache`. |
| `internal/llm` | Optional structured LLM diagnostics (`github.com/zendev-sh/goai` `GenerateObject[T]` + `go-playground/validator/v10`) for gate failure diagnosis, explanation of a rejection by the evaluator, semantic plan audit, and rebase conflict summaries. Application code is NEVER written by `internal/llm`; coding agents (`claude`, `opencode`, `hermes`, etc.) always write code. |
| `internal/gitx` | Git repository operations (`go-git/v5` + `git` CLI), worktree lifecycle, submodule `--reference` initialization, topological `CompletedSince`, and cross-platform `SSH_AUTH_SOCK` discovery. |
| `internal/portal` | Embedded HTTP + SSE server: the server-rendered multi-page portal (`html/template` + daisyUI, one URL per page) and its JSON API. A page and its API endpoint share one typed read over a snapshot of the store (`page_*.go`, with the routes in `api_*.go`: `/` and `/api/dashboard`, `/inbox` and `/api/inbox`, `/changes/<name>` and `/api/changes/<name>`, `/executions/<id>`, `/gates/<id>`, etc.); what acts on a worktree or a process is a service (`svc_*.go`), and `layers_test.go` holds each file to the imports of its layer. A page that shows what the kit says of the application rather than rows (the settings, the setup, the prompts) asks the kit before it takes its snapshot (`pageAsking`): a read of the kit that needs a connection of the store must not wait for one while the page holds another, and `readonly_test.go` asks every `GET` route with a single connection to find one that does. An error of the API is always `{"error": {"code", "message"}}`. |
| `internal/sim` | Self-contained 14-scenario simulation laboratory (`foreman-kit sim`) with 13 simulated agent binaries (`sim-agent-*`) verifying the full pipeline end to end without external API calls. |

---

## 3. Engineering Rules and Invariants

1. **Static Binary (`CGO_ENABLED=0`)**: Never introduce Cgo dependencies. SQLite uses `modernc.org/sqlite`.
2. **Linux & macOS Parity**:
   - Process tree, zombie detection (`'Z'`), and CPU time must support `/proc/<pid>/stat` natively on Linux as well as `ps` on macOS/BSD (`internal/proc/watchdog.go`, `internal/store/locks.go`).
   - Default UTF-8 subprocess locale is `C.UTF-8` on Linux and `en_US.UTF-8` on macOS (`internal/proc/runner.go`).
   - Toolchain activation in `internal/assets/templates/repo_env.sh` and `FOREMANKIT_PATH_PREFIX` prioritizes `mise` (`~/.local/share/mise/shims` + `mise env -s bash`) first, falling back to `asdf`, `rbenv`/`RVM`/`chruby`, `fnm`/`nvm`, `pyenv`, and `goenv` only when `mise` is absent.
3. **Declarative Configuration Over Hardcoding**:
   - The command that runs each agent is written whole under `commands:` in `.foreman-kit/agents.yaml` (`run`, `restricted`, `ping`) and runs exactly as written: the binary expands its placeholders (`{{prompt}}`, `{{prompt_file}}`, `{{workdir}}`, `{{timeout}}`, `{{args}}`) and adds nothing. The command lines the kit ships live in `config.SUPPORTED`.
   - The evaluator's questions, instructions, criteria, and thresholds live together in `internal/assets/seeds/.foreman-kit/evaluator.yaml`.
   - A setting is defined once, in `internal/settings/catalog.go`, and its seed writes it: a test compares every seed with the catalog, key by key. Code names a setting by its constant (`settings.GateTimeout`), reads it with `settings.At(root)`, and builds any message that mentions it from `Key.Path()`, `Key.FilePath()` or `Key.Where()`. Never write a key name or a default value anywhere else.
   - Nothing is implicit: every key is written in its file, a time carries its unit (`30s`, `15m`, `2h`, `never`), and no value is derived from another. A key that is missing or invalid is an error of `foreman-kit config check`, and the process that reads its file starts no new work until it is mended (`settings.Reader.Unusable`, with the files of each process in `internal/settings/unusable.go`); a command that only shows a value reads the one the kit ships and says so. A test that runs a dispatcher, a merger, a cycle or a daemon writes the files whole with `settingstest.WriteFiles`. A key that is renamed or moved gets a row in `settings.Formers()`, which is what `foreman-kit update` and the message of `config check` read.
4. **Language**: All source code, comments, tests, commit messages, and documentation must be written in clear professional English.
5. **Relational State**: Every row has a parent declared as a foreign key, and facts are inserted and closed, never overwritten or deleted (a block, a merge attempt, a manifest and a pause are rows with an opening and a closing time; current state is a view over the open ones). A schema change is a new file in `internal/store/migrations/`; never edit an applied migration, never run DDL or raw SQL from another package, and write an event in the same transaction as the fact it announces.
6. **Portal UI**: The portal is not a single-page app. Each page is a Go route rendering `internal/assets/portal_ui/templates`; `portal.js` only enhances pages and posts actions to the JSON API. A `GET` writes nothing: `readonly_test.go` asks every `GET` route of the package and fails when one leaves a row behind. Every page is found by following links from `/`: `reach_test.go` does it and names a page nothing leads to. Style markup with daisyUI 5 classes and Tailwind utilities only (no hand-written component CSS). After changing templates, `portal.js` or class names in the Go files of `internal/portal`, rebuild and commit the generated assets with `cd web/portal && npm ci && npm run build`, and run `npm test` there: it opens every page in a headless Chromium and checks that it loads without an error, passes the automatic accessibility check, is walked whole with the keyboard, draws no text smaller than 12 px and fits a phone and a tablet with controls 44 px tall (`web/portal/tests`). A control gets that size with the `touch:` variant of `web/portal/input.css` (a screen narrower than 640 px, or one a finger points at, whatever its width), never with `max-sm:`. It also draws every page in each theme of the menu and works out the contrast of its text (4.5:1), also on the buttons and badges the automatic check leaves undecided: a theme that fails is taken out of `portalThemes` and of the `themes:` list of `web/portal/input.css`, never excused. A new page joins the list in `internal/portal/browser_test.go`.
7. **Memory With an External Program**: An application's memory is `.agent-memory/`, kept with `agent-memory`, which foreman-kit runs from `PATH` and never embeds, vendors or reimplements; a missing program is an error that says how to install it (`memory.Require`). Only `internal/memory` runs it or knows its files. Two rules keep every checkout clean: a commit that changes `.agent-memory/` is made after `memory.Normalize` (the record of a session, a reconcile, `init`, `update`), so that no later search or index rebuild changes a tracked file anywhere; and a checkout an agent is about to read is given `memory.Refresh` first (before every agent call, and when a session worktree is prepared), because the search index is per checkout and does not follow what git brings. Reading for the portal or a prompt (`memory.PromptBlock`, `memory.Fingerprint`, `memory.Sizes`) reads files only and runs nothing.

---

## 4. Verification Commands

Always run these checks before finishing any change to this repository. Steps 3, 5 and 6 install applications, so they need the two programs an application is kept with on `PATH`: `agent-memory` (`go install github.com/xChuCx/agent-memory/cmd/agent-memory@latest`) and `openspec` (`npm install -g @fission-ai/openspec@latest`). The kit runs the real programs and ships no stand-in for either: a machine without one fails those steps with the same lines an application's `foreman-kit init` prints. A test that sets `FOREMANKIT_FORCE_BUILTIN=1` works without `openspec`; nothing works without `agent-memory`.

```bash
# 1. Static analysis
CGO_ENABLED=0 go vet ./...

# 2. Build the static binary
CGO_ENABLED=0 go build -o bin/foreman-kit ./cmd/foreman-kit

# 3. Run unit and integration test suites
CGO_ENABLED=0 go test -count=1 ./internal/...

# 4. Run the offline contract check of the evaluator
./bin/foreman-kit evaluator-contract

# 5. Run the 14-scenario end-to-end simulation stack when touching orchestrator/store/cycle/proc/evaluator
./bin/foreman-kit sim run /tmp/openspec-sim-stack all

# 6. Run the 24 fault-injection scenarios when touching failure handling (agents, git, merger, evaluator, LLM)
./bin/foreman-kit sim run /tmp/openspec-sim-stack faults

# 7. Open the portal's pages in a headless Chromium when touching its templates, portal.js or page handlers
#    (once per machine: npx playwright-core install chromium)
(cd web/portal && npm ci && npm test)
```
