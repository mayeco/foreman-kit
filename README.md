# foreman-kit

`foreman-kit` is a single static Go binary (`CGO_ENABLED=0`) that orchestrates AI coding agents to specify, build, verify, and integrate multi-repository software projects using [OpenSpec](https://openspec.dev) and an embedded SQLite WAL state engine (`log/openspec.db`).

All umbrella seeds, templates, skills, and the real-time HTTP/SSE Web Portal are compiled directly into the binary via `//go:embed` (`internal/assets/`). No Ruby, Bundler, Sinatra, external database daemon, or `crontab` is required.

---

## 1. Repository Layout

```text
.
├── cmd/foreman-kit/        CLI entrypoint, flags (-d/--daemon, -w/--web), and subcommands
├── internal/
│   ├── assets/              Embedded files (//go:embed):
│   │   ├── seeds/           Umbrella project seeds (AGENTS.md, README.md, ROADMAP.md, config/*)
│   │   ├── templates/       Templates for child repos, OpenSpec rules and a new memory (authoring_rules.md, repo.yaml, repo_env.sh, memory_conventions.md)
│   │   ├── skills/          Agent skills installed into target umbrellas (foreman-kit-*)
│   │   └── portal_ui/       Embedded Web Portal: page templates, compiled daisyUI stylesheet, icons, fonts, portal.js
│   ├── config/              config show/check/set over the settings catalog, the supported agents and prompts
│   ├── cycle/               Autonomous cycle runner, per-project background daemon, setup/init/update, visual QA
│   ├── evaluator/           The evaluator: native Jev client (shanehull/go-jev), policy and questions, SQLite cache
│   ├── gitx/                Pure-Go (go-git/v5) and git CLI helpers, worktrees, submodules, SSH agent discovery
│   ├── llm/                 Structured LLM diagnostics (zendev-sh/goai + validator/v10) for triage/audits
│   ├── memory/              The application's memory (.agent-memory/), kept with the external agent-memory CLI
│   ├── orchestrator/        Dispatcher (PLAN -> BUILD workers) and Merger (rebase -> gates -> archive -> push)
│   ├── portal/              Embedded HTTP + SSE Web Portal server and REST API
│   ├── proc/                Process group runner, CPU/tree/TMPDIR watchdog, slot allocator, gate runner
│   ├── settings/            The settings catalog: every key of .foreman-kit/*.yaml defined once, and the one loader of their values
│   ├── sim/                 Repeatable 14-scenario end-to-end simulation stack (foreman-kit sim)
│   └── store/               Pure-Go SQLite WAL store (modernc.org/sqlite) with separate writeDB/readDB pools
├── web/portal/              Build tooling of the portal stylesheet (Tailwind CSS 4 + daisyUI 5), icon sprite and fonts
├── .github/workflows/       release.yml (verify, GoReleaser, npm) and npm-publish.yml
├── .goreleaser.yaml         How a release is built and uploaded to GitHub
├── npm/                     The npm package: package.json (private), the Node launcher, its README
├── scripts/                 build.sh (static binaries per platform), npm-pack.sh, npm-install.sh
└── install.sh               Bootstrap script to build or download ~/.local/bin/foreman-kit
```

---

## 2. Build and Install

### Requirements
- **Linux or macOS** (`amd64` or `arm64`)
- **Go >= 1.25** (to build from source)
- **git** and **openspec** CLI (`npm install -g @fission-ai/openspec` or `brew install openspec`)
- **agent-memory** CLI (`go install github.com/xChuCx/agent-memory/cmd/agent-memory@latest`, or `npm install -g @xchucx/agent-memory`, or a binary of its [releases](https://github.com/xChuCx/agent-memory/releases/latest) on `PATH`). The application's long-term memory is kept with it. `foreman-kit` runs it as a program of its own and does not contain it: `init`, `update`, the preflight and the tests of this repository fail, saying how to install it, on a machine that lacks it. Install it where it stays on `PATH` for every process that runs an agent (a directory of one version of a toolchain manager goes away with that version); `foreman-kit preflight` prints where it finds it.
- At least one coding agent CLI authenticated on `PATH` (`claude`, `codex`, `copilot`, `hermes`, `opencode`, `agy`, `kilo`, `cline`, `pi`, `omp`, `crush`, `cursor-agent`, `zcode`, or any custom binary configured under `commands:` in `.foreman-kit/agents.yaml`).

### Build from source
```bash
CGO_ENABLED=0 go build -o ~/.local/bin/foreman-kit ./cmd/foreman-kit
# or run the bootstrap script:
./install.sh
```

### Build for several platforms, and install with npm

`scripts/build.sh` builds the static binary for this platform, or for each one given
(`--target darwin/arm64`, or `--all` for macOS and Linux on amd64 and arm64), into
`dist/bin/<os>-<arch>/foreman-kit`, with the version, the commit and the build time in it
(`foreman-kit version`).

`scripts/npm-pack.sh` (same options) lays out the npm package from `npm/` with those binaries
under `vendor/<os>-<arch>/` and a Node launcher that runs the one of the machine, and packs it into
`dist/mayeco-foreman-kit-<version>.tgz`. `scripts/npm-install.sh` packs it and installs that file
globally with `npm install -g`, or under a directory with `--prefix <dir>`:

```bash
NPM=/opt/homebrew/bin/npm scripts/npm-install.sh
```

`NPM` chooses the npm. The global prefix of an npm a version manager provides (mise, nvm, fnm)
belongs to one version of Node, and an agent or a daemon started with another one does not find
`foreman-kit` there; the script says so, and says whether `PATH` finds another `foreman-kit`
first. Nothing is published: the package is marked private, and npm refuses to publish it. A
package of this platform alone is about 10 MB; with `--all`, about 40 MB.

### Releases (GoReleaser and GitHub Actions)

A release is a tag `v<semver>` pushed to GitHub (`v4.1.0`; `v4.1.0-rc.1` is a prerelease). It runs
`.github/workflows/release.yml`:

1. **verify**: `go vet`, the build, every test, the evaluator's contract, the 14 simulations and the
   23 fault scenarios, with `openspec` and `agent-memory` installed from npm (the tests run them).
2. **GitHub release**: [GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`) builds the static
   binary for macOS and Linux on amd64 and arm64 with the tag's version in it, and uploads each one
   as `foreman-kit-<os>-<arch>` (what `install.sh` downloads) with `checksums.txt`.
3. **npm package**: `.github/workflows/npm-publish.yml` downloads those binaries, checks their
   checksums, packs `@mayeco/foreman-kit` with `scripts/npm-pack.sh --prebuilt --public`, checks
   that the installed launcher runs, and publishes it with provenance, under the dist-tag `next`
   for a prerelease and `latest` otherwise. It needs the repository secret `NPM_TOKEN` (an npm
   automation token that may publish the `@mayeco` scope). Run by hand with a tag, it publishes
   the npm package of a release that already exists, or only packs it with `dry-run`.

Run by hand (Actions > release > Run workflow), the release workflow is a rehearsal: the same
verification, a GoReleaser snapshot and the npm package packed, kept as the run's artifacts, and
nothing published. Locally, `goreleaser check` validates the configuration and
`goreleaser release --snapshot --clean` builds the release into `dist/release/` without uploading
anything (`mise run release:snapshot`).

---

## 3. Quick Start: Creating and Running an Umbrella Workspace

`foreman-kit` operates on an **umbrella workspace** that holds `openspec/` specifications, `config/`, and one or more child repositories under `repos/<name>/`.

```bash
# 1. Initialize a new umbrella workspace: seeds and skills, then `openspec init` (openspec/) and
#    `agent-memory init` (.agent-memory/). Without either program it says how to install it and stops.
foreman-kit init ../my-platform --agents claude,hermes --plan claude --build claude --cycle hermes
cd ../my-platform

# 2. Add child repositories as git submodules under repos/<name>
foreman-kit setup add-repo git@github.com:org/backend.git --name backend

# 3. Validate configuration, environment, and agent health
foreman-kit config check
foreman-kit preflight

# 4. Start the per-project background daemon + embedded Web Portal (no crontab needed)
foreman-kit -d -w
foreman-kit daemon status
```

### Configuration (`.foreman-kit/*.yaml`)
An umbrella workspace holds seven settings files, one per reader. Every key is written in its file: one that is missing, unknown or invalid is an error of `foreman-kit config check`, and nothing is derived from another key. A process starts no new work on a file it cannot use: it says which key keeps it (in its log, in `foreman-kit status` as `CONFIG:`, and in `foreman-kit preflight`) and goes on when the file is mended, while the changes already being built continue.

| File | Decides | Read by |
| :--- | :--- | :--- |
| `.foreman-kit/pipeline.yaml` | How a change travels from the backlog to the base branch | The dispatcher and the merger |
| `.foreman-kit/agents.yaml` | Which agent CLIs the application uses, who does what, the limits of one call and the command that runs each agent | Every agent call and the preflight |
| `.foreman-kit/cycle.yaml` | What one cycle does: how long it may take and what it may queue | The cycle |
| `.foreman-kit/daemon.yaml` | When the cycle runs and whether the Web Portal is served | The daemon, when it starts |
| `.foreman-kit/visual_qa.yaml` | The browser session an agent app runs as its own scheduled task | The visual session |
| `.foreman-kit/exploration.yaml` | What the cycle and the visual session look for | Both |
| `.foreman-kit/evaluator.yaml` | How the evaluator is called, what each gate asks and what the kit does with the answers | Every gate of the evaluator |

Times carry their unit (`30s`, `15m`, `2h`), and `never` turns off a limit that can be off. `foreman-kit config show` explains every key with its value, `foreman-kit config set <file>.<key> <value>` changes one in place, and `foreman-kit config recommend` lists the ones the kit's own records argue for changing. The command that runs each agent is written whole under `commands:` in `.foreman-kit/agents.yaml` (`run`, `restricted`, `ping`) and runs exactly as written; with `permissions: restricted` an agent without a `restricted` command cannot be used. `foreman-kit update` rewrites the files of a workspace installed with an older kit: each value moves to the key that holds it now, and a key the kit no longer reads is an error from then on. A change that was being built keeps the former files in its worktree: what runs there reads the files of the main checkout, so it goes on with the settings the workspace has now.

### Child repositories (`repos/<name>/repo.config.yaml`)
Each child repository describes itself in a file at its own root, committed in that repository: its base branch, how it runs and the commands that check a change to it. `foreman-kit setup repo <name>` writes it from `.foreman-kit/templates/repo.yaml`.

| Key | Holds | Read by |
| :--- | :--- | :--- |
| `base_branch`, `kind`, `env_script` | The branch the merger pushes to, `web` or `cli`, and the script sourced before each command of the repository | Everything that runs a command of the repository |
| `service` | The dev command, the port and the health endpoint | The visual session |
| `visual_qa.prepare` | What readies the clone the visual session runs the application from | The visual session |
| `required_services` | The services that must answer before anything starts, as `name: command` | The preflight and the visual session |
| `setup` | What prepares the environment of a worker slot. It is not a gate: it runs before the gates and before the regression | The gates |
| `gates` | The commands that run after every BUILD and again at merge | The dispatcher and the merger |
| `regression.commands` | The commands of the cycle's sweep, usually the full suite. Empty: the sweep runs nothing in this repository | The cycle, and the gates of a change that touches a path of `regression.when` |
| `regression.when` | The paths whose change runs those commands after the gates | The dispatcher and the merger |

`foreman-kit update` rewrites the file of a repository installed with an older kit, whose keys `gates.setup`, `service.prepare`, `requires`, `regression` and `regression_when` are no longer read, and prints the command that commits it in each repository. A repository whose regression had no command of its own ran its gates: the update writes the gates that run as its `regression.commands`, so that it goes on checking the same. Until the rewritten file reaches the base branch of the repository, a checkout that still holds the former keys reads the file of the main checkout, so no change that was being built starts again.

### A value for one run (`--set`)
A setting has one value, the one its file writes. To run a command once with another value, give it before the command, named with its file as `foreman-kit config show` lists it and written as the file would write it:

```bash
foreman-kit --set pipeline.dispatch.workers=4 dispatch
foreman-kit --set agents.roles.build.agents=codex,claude --set evaluator.enabled=false dispatch
foreman-kit -d -w --set daemon.cycle.every_when_out_of_work=never
```

The value holds for that run and for everything it starts (the daemon, the gates an agent runs in its worktree), and no file is written. `--set` takes the keys `foreman-kit config set` takes: one value, or the agents of a role. A list, the arguments of a role and the commands of the agents are edited in their file. A key the catalog does not have, a key that is not one value, a value the setting does not accept, or one this application cannot use (an agent it does not list, a slot one of its workers takes), stops the run before it starts, with the words `config set` has for the same value. The agents of a role are written as a person types them (`codex,claude`) or as their file holds them (`[codex, claude]`), to `--set` and to `config set` alike. `foreman-kit config show` lists what a run was given, and each key says what replaced it. A daemon started with `--set` keeps those values until it stops: `foreman-kit status` and `foreman-kit daemon status` name them, and so does its log.

### Memory (`.agent-memory/`)

What the application learns is kept in `.agent-memory/` at its root, with [agent-memory](https://github.com/xChuCx/agent-memory): Markdown files git tracks, read and written through that program's own commands. `foreman-kit` runs the `agent-memory` on `PATH` and contains none of it.

| File | Holds | Read by |
| :--- | :--- | :--- |
| `conventions.md` | rules that apply to every change | every agent: `agent-memory fetch` prints it |
| `pitfalls.md` | lessons and environment quirks that apply to every change | a search: `agent-memory fetch "<topic>"` |
| `decisions.md` | decisions not visible in the code, each with its date, status and reason | a search |
| `modules/repo-<name>.md`, `modules/spec-<capability>.md` | what applies to one child repository or one capability | the changes that touch it: the PLAN and BUILD prompts name the file (`{{memory}}`) |

- **Who writes**: the cycle's judgment and a visual QA session, with `agent-memory propose` (the `foreman-kit-memory` skill has the command for each kind of entry); plan and build agents leave their lessons in `handoff.md`, and a write of theirs under `.agent-memory/` sends the change back. A person may write at any time.
- **How it is recorded**: the cycle commits `.agent-memory/` with the backlog and the proposals. Every such commit is made after `agent-memory rebuild-index`, so a checkout that receives it is left unchanged by any search an agent runs there. When two sessions recorded at once, a file both changed is merged section by section with `agent-memory merge-driver`, and line by line where both added to one section; `index.md`, which agent-memory derives, is written again instead of merged.
- **What the kit reads by itself**: only which files the memory holds and how long they are. `foreman-kit memory status` shows each against its cap (`memory.max_chars`, `memory.max_chars_per_scope` in `.foreman-kit/cycle.yaml`); `foreman-kit memory fingerprint` prints the version recorded with every agent call, which `foreman-kit prompt-report --by memory` reports on.
- **An application installed before this** kept its memory in `MEMORY.md` and `memory/`: `foreman-kit update` creates `.agent-memory/`, takes each part in through `agent-memory propose` (what agent-memory refuses stops the update with the reason and the old files in place), removes the two, and rewrites the sentences of the application's prompts and guides that named them. Until then the preflight fails and `foreman-kit config check` says why.

### Environment Variables (`.env`)
No environment variable changes a setting. The environment carries what is not a setting: the secrets, where things are on this machine, and what the kit hands to the gates and the agents it runs.

On startup, `foreman-kit` loads a dotenv-format `.env` file from the directory it runs in (via [`godotenv`](https://github.com/joho/godotenv)). Variables already set in the environment take precedence, so the file only provides defaults. A missing file is ignored; a malformed one prints a warning and is skipped. `.env` is git-ignored, so keep secrets (API keys, tokens) there. Copy [`.env.example`](.env.example) to get started.

| Variable | Purpose |
| :--- | :--- |
| `TYPESAFE_API_KEY` | The key of the evaluator, which today is the Jev model. It is the only variable the key is read from\* |
| `FOREMANKIT_DIAGNOSTICS_API_KEY` | The key of the diagnostics assistant, checked before the provider's own variable |
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY`, `OPENROUTER_API_KEY` | The provider's own variable for that key |
| `FOREMANKIT_PORTAL_TOKEN` | The token of the Web Portal's API, to call it from outside this machine |
| `FOREMANKIT_HOME` | Where the state of the workspaces of this machine lives (`~/.foreman-kit` when unset) |
| `FOREMANKIT_STATE_DIR`, `FOREMANKIT_DB_PATH` | The directory of that state in place of `$FOREMANKIT_HOME/state`, and the database of this application in place of the one found there |
| `FOREMANKIT_APP_ROOT` | The application a command works on, whatever directory it runs in (`foreman-kit <directory> <command>` says the same). The kit sets it for each agent it runs, to the worktree of the agent |
| `OPENSPEC_SLOT`, `OPENSPEC_CHANGED_FILES`, `OPENSPEC_MAIN_ROOT` | The same as `FOREMANKIT_SLOT`, `FOREMANKIT_CHANGED_FILES` and `FOREMANKIT_MAIN_ROOT`, under the names the kit gave them before it was called Foreman, for the scripts of a repository written then. Any `OPENSPEC_<name>` set in the environment is read as `FOREMANKIT_<name>` when that one is not set |
| `FOREMANKIT_PATH_PREFIX` | Prepended to `PATH` for what the kit runs: where the tools are on this machine |
| `FOREMANKIT_SSH_CONFIG`, `FOREMANKIT_SANITIZE_GIT_ENV` | The SSH configuration to read in place of `~/.ssh/config`, and `1` to keep the kit from looking for an SSH agent when `SSH_AUTH_SOCK` is not set |
| `FOREMANKIT_SET` | What `foreman-kit --set` gave this run, for the processes the run starts. `--set` writes it and nobody exports it: a process that finds in it what `--set` does not write does not start |
| `FOREMANKIT_CHANGE`, `FOREMANKIT_PHASE`, `FOREMANKIT_SLOT` | Set by the kit for the agents and the gates it runs, and read back by the commands they run: the change and the phase in work, and the slot of the worker |
| `FOREMANKIT_ROLE`, `FOREMANKIT_ENGINE`, `FOREMANKIT_PROMPT_FILE`, `FOREMANKIT_WORKDIR`, `FOREMANKIT_MAIN_ROOT`, `TMPDIR` | Set by the kit for each agent it runs: its role, the agent it is, the file of its prompt, its worktree, the main checkout, and for the agents of a worker a scratch directory of their own |
| `TEST_ENV_NUMBER`, `PORT`, `FOREMANKIT_CHANGED_FILES`, `CI`, `NONINTERACTIVE`, `GIT_TERMINAL_PROMPT`, `GIT_MERGE_AUTOEDIT`, `LANG`, `LC_ALL` | Set by the kit for the gates and the commands of a repository: the number and the port of the slot (3000 plus 10 for each slot), the files the change touched, and what keeps a command from waiting for a person or writing in another encoding |
| `CYCLE_STAMP` | The name of a cycle, set by the kit for the process that runs it |
| `SSH_AUTH_SOCK`, `GH_TOKEN`, `GITHUB_TOKEN`, `GITLAB_TOKEN`, `GIT_ASKPASS` | Taken out of the environment of the agents a worker runs: what reaches a remote is the merger's to push |
| `AGENT_STUB`, `FOREMANKIT_FORCE_BUILTIN`, `FOREMANKIT_SIM_STRICT` | For tests and simulations: the script the agent named `stub` runs, `1` to check a change without the `openspec` CLI, and `1` to make `foreman-kit sim` fail on a known gap |

\* Or a credentials file. When the variable is not set, the key is read from the first existing file among `$XDG_CONFIG_HOME/jev/credentials.json`, `~/.config/jev/credentials.json`, and `~/.jev/credentials.json` (JSON field `api_key`, `apiKey`, `token`, or `key`).

The evaluator is active when `enabled` is not `false` in `.foreman-kit/evaluator.yaml` and an API key resolves; its model, its endpoint, and the modes, questions and thresholds of each gate are written there. The diagnostics assistant explains a failure in words (a failing gate, a refusal by the evaluator, a plan audit, a rebase conflict) and never writes application code. It is off as shipped, and is written in the `diagnostics:` section of `.foreman-kit/pipeline.yaml` (`enabled`, `provider`, `model`, `base_url`, `timeout`, `retries`); it works when it is enabled **and** a key resolves.

API key lookup of the diagnostics assistant per provider (first non-empty wins). A key is never taken from another vendor's variable:

| Provider | Variables checked |
| :--- | :--- |
| `openai` | `FOREMANKIT_DIAGNOSTICS_API_KEY`, `OPENAI_API_KEY` |
| `anthropic` | `FOREMANKIT_DIAGNOSTICS_API_KEY`, `ANTHROPIC_API_KEY` |
| `google` / `gemini` | `FOREMANKIT_DIAGNOSTICS_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY` |
| `openrouter` | `FOREMANKIT_DIAGNOSTICS_API_KEY`, `OPENROUTER_API_KEY` |
| `compat` | `FOREMANKIT_DIAGNOSTICS_API_KEY` only |
| `ollama` | `FOREMANKIT_DIAGNOSTICS_API_KEY` (optional; no key needed) |

#### Variables an older foreman-kit read
`OPENSPEC_WORKERS`, `OPENSPEC_BASE_BRANCH`, `AGENT_ENGINE` and its three per-role forms, `AGENT_PERMISSIONS`, `JEV_DISABLED`, `JEV_MODEL`, `OPENSPEC_LLM_ENABLED` and the others that overrode a key are no longer read. Whoever still exports one expects it to hold, so it is not ignored in silence: `foreman-kit config check` names each one that is set with what does its job now, and what read its file starts no new work until it is out of the environment: unset in the shell that exports it, or deleted from the `.env` file that writes it. `OPENSPEC_LLM_API_KEY` is `FOREMANKIT_DIAGNOSTICS_API_KEY`, and `JEV_API_KEY` is `TYPESAFE_API_KEY`. The same holds for `CLAUDE_ALLOWED_TOOLS`, `CLAUDE_DISALLOWED_TOOLS` and `COPILOT_DENIED_TOOLS`: the tools an agent may use are written in its own command, under `commands` in `.foreman-kit/agents.yaml`, and a list exported in the environment restricts nothing.

### Dogfooding `foreman-kit`
To develop `foreman-kit` itself using the autonomous pipeline, create a parent umbrella repository and mount this repository as a child submodule (`kind: cli`):

```bash
foreman-kit init ../openspec-umbrella --agents claude,hermes --plan claude --build claude --cycle hermes
cd ../openspec-umbrella
foreman-kit setup add-repo /path/to/foreman-kit --name foreman-kit
```

The parent umbrella receives the seed `AGENTS.md`, `README.md`, `ROADMAP.md`, and `config/` from `internal/assets/seeds/`, and its `.agent-memory/` from `agent-memory init`, while `repos/foreman-kit` remains this clean Go repository.

### Web Portal

`foreman-kit -w` serves a server-rendered, multi-page admin portal: every page (dashboard, changes, queue, cycles, runs, jobs, system, ...) has its own URL, links and browser history, and is rendered from `internal/assets/portal_ui/templates` with Go `html/template`. A small `portal.js` only enhances pages (actions posted to the JSON API, live refresh of running pages, file viewers). The UI uses [daisyUI](https://daisyui.com) with the kit's own `openspec` theme plus every built-in daisyUI theme; pick one from the navbar or **System → Appearance** (remembered per browser in a cookie).

The compiled stylesheet, icon sprite and fonts are committed, so `go build` needs no Node.js. After changing templates or `portal.js`, rebuild them:

```bash
cd web/portal && npm ci && npm run build   # writes internal/assets/portal_ui/{portal.css,icons.svg,favicon.svg,fonts/}
```

---

## 4. CLI Reference

| Command | Description |
| :--- | :--- |
| `foreman-kit -d` (`--daemon`) | Start the per-project background daemon (continuous merger and dispatcher + the cycle's judgment every `cycle.every` of `.foreman-kit/daemon.yaml`, or `cycle.every_when_out_of_work` after the last one once the workers run out of work and fewer than `queue.max_open` changes are open) |
| `foreman-kit -w` (`--web`) | Start the embedded Web Portal (`http://127.0.0.1:4567`); combine as `foreman-kit -d -w` |
| `foreman-kit daemon <status\|stop\|restart\|run>` | Inspect, stop, or restart the current project's background daemon |
| `foreman-kit init [dir] --agents ...` | Initialize an OpenSpec umbrella workspace with embedded seeds and skills, and run `openspec init` and `agent-memory init` in it (both must be installed) |
| `foreman-kit update [dir]` | Update embedded skills and lockfile in an existing umbrella workspace, rewrite `.foreman-kit/*.yaml` written for an older kit (each value moves to the key that holds it now), and give an application installed before agent-memory its `.agent-memory/`, taking in what `MEMORY.md` and `memory/` held |
| `foreman-kit setup <status\|add-repo\|repo>` | Inspect umbrella setup status or configure child repositories under `repos/` |
| `foreman-kit config <show\|check\|set\|recommend>` | Inspect, validate, or modify `.foreman-kit/*.yaml` settings, or list the ones the kit's own records argue for changing |
| `foreman-kit preflight` | Verify git, OpenSpec, agent-memory and the application's memory, kit checksums, live agent pings, and the canary of the evaluator |
| `foreman-kit dispatch [--dry-run]` | Plan and build queued backlog items across parallel isolated worktrees |
| `foreman-kit merge [--batch \| --watch]` | Rebase, run gates, archive specs, and push ready changes to `origin` |
| `foreman-kit cycle [--detach]` | Run one autonomous verification, exploration, proposal, and dispatch cycle |
| `foreman-kit gates [--repo <name>]` | Run quality gates for changed or specified child repositories |
| `foreman-kit exec <repo> <cmd...>` | Execute a command inside `<repo>` with its `env_script` and slot environment |
| `foreman-kit status` / `progress` | Show active locks, workers, queues, blocked items, and throughput metrics |
| `foreman-kit proposals <list\|show\|approve\|reject\|review>` | Manage and review agent and roadmap proposals |
| `foreman-kit migrate-from-files [dir]` | Idempotently import legacy v3.x JSON/log state into `log/openspec.db` |
| `foreman-kit sim <init\|run\|reset\|status> [dir]` | Run the 14-scenario repeatable simulation stack with simulated agent binaries |

---

## 5. Testing and Simulation Stack

```bash
# Run all unit and integration tests (they install applications: agent-memory must be on PATH)
CGO_ENABLED=0 go test ./...

# Run the full 14-scenario end-to-end simulation laboratory
./bin/foreman-kit sim run /tmp/sim_stack all

# Run the 23 fault-injection scenarios (or one by name; `everything` runs both groups)
./bin/foreman-kit sim run /tmp/sim_stack faults
./bin/foreman-kit sim run /tmp/sim_stack fault-git-base-force-pushed
```

### Fault-injection scenarios
`internal/sim/faults.go` runs each scenario on a real umbrella stack (bare remotes, a submodule child, SQLite state), injects one production failure, and checks that the pipeline degrades safely and recovers once the fault is gone. `go test ./internal/sim/` runs them (skipped with `-short`; `FOREMANKIT_SIM_FAULT=<name>` runs one).

| Area | Scenarios |
| :--- | :--- |
| Coding agents | crash by SIGKILL mid-change (fallback continues from the half-done worktree), hang caught by the idle watchdog, orchestrator killed mid-build, 429 throttling (deferred, not quarantined), garbage/noisy output with invalid UTF-8, writes outside allowed paths, self-commits and branch switching |
| Git | remote rejects pushes (pre-receive hook), umbrella and child remotes unreachable, stale `index.lock` files, force-pushed base, submodule gitlink to an unpushed commit |
| Routing gates of the evaluator | an orchestrator error that only one change hits (blocked alone, sibling integrates, no pause); stray `.DS_Store` and logs reverted by the orchestrator (no repair round); an agent that gives up (sent to a human, not deferred) vs. a full disk on the machine (pause); a clean rebase that contradicts a teammate's stated invariant (advisory: flagged; enforce: sent back, quarantined, approved) |
| Concurrent work loss | a rebase conflict "resolved" by an agent that drops a teammate's production hotfix (sent back, then quarantined, then integrated by a human approval); an agent that restores the hotfix when told; a trivial `chore: fix new line` loss accepted automatically |
| Disk | unwritable `.worktrees/`, disk full during gates (real write failures via a 512-byte file size limit) |
| Evaluator / LLM | Jev 500, 429, hang past timeout, and garbage body (advisory and enforce + `when_unavailable: block`); LLM diagnostics provider 500, garbage body, schema-invalid object, and hang |
| Backlog | empty, path-traversal and invalid names, duplicates, dependency cycles, missing dependencies, unicode and 200 KB descriptions, non-object entries |

### Concurrent work loss (evaluator gate `concurrent_work_loss`)
After rebasing a change's child repositories onto their base, the merger looks for lines the change removes that commits landed on the base **after the change started** (by `git blame` on the base and the change's first `change.start`). That is a teammate's concurrent work, typically dropped while an agent resolved a rebase conflict, and gates pass when nothing tests it. The merger asks the evaluator, giving each such commit's full message, author and the removed lines:

- `importance` is judged by the commit message first and confirmed by the lines: `critical` (hotfix, production down, outage, security, rollback, urgent tone), `significant` (deliberate feature, fix, route, config, test), `trivial` (whitespace, trailing newline, formatting, a typo, as a chore/style message says), or `cannot_tell`. Losing `chore: fix new line` is not the same as losing `HOTFIX: production is down`.
- `superseded_by_change`: the change deliberately replaces that work (`allow_when` overrides `block_when`).

In `enforce` mode (the default) a blocking answer sends the change back to the BUILD agent with the lost commits and lines to restore (up to `merge.conflict_rebuilds`), then quarantines it as `concurrent_work_loss`, which waits for a person: `foreman-kit approve <change>` integrates it as it is. A trivial loss integrates and is recorded (`merge.work_loss_detected`, `evaluator.route`). Thresholds live in `.foreman-kit/evaluator.yaml`.

### Routing gates of the evaluator (state → question → action)
Where the orchestrator used to react one fixed way (pause the workspace, wait for a person, retry blindly), it asks the evaluator a calibrated question about the state and takes the action the gate's `actions:` rules select in `.foreman-kit/evaluator.yaml` (`{ question, choice, min_confidence, action }`). When the gate is off or advisory, the evaluator is unavailable, or no rule matches, the fallback is the behavior from before the gate, so a gate can only improve on it. Every decision is recorded as a `evaluator.route` event (`action`, `suggested`, `rule`, `answers`).

| Gate | State | Question | Actions (fallback first) |
| :--- | :--- | :--- | :--- |
| `system_error_triage` (enforce) | an `orchestrator_error`, its stage, other changes failing the same way | `scope`: change_specific / transient / workspace_wide | `pause` · `block_change` (only that change) · `retry_later` (`transient_error`) |
| `policy_violation_triage` (enforce) | files a BUILD left outside its paths, with previews (protected paths never reach the evaluator) | `nature`: stray_artifact / tampering | `repair` · `revert_and_continue` · `quarantine` |
| `failure_triage` (enforce) | gate output after one re-run | `cause`: environment_or_flaky / … | `repair` · `hold_environment` (`environment_hold`, no repair rounds) |
| `agent_outcome` (enforce) | output of engines that all failed without changes (exit 75) and matched no quota/auth pattern | `outcome`: provider_outage / quota_or_rate_limit / auth / agent_gave_up / environment / crash | `defer` · `needs_human` (`agent_gave_up`) · `pause` (`environment_unavailable`) |
| `semantic_merge_conflict` (advisory) | the change's diff and concurrent commits (messages, diffs) on the same files | `relation`: compatible / contradicts / duplicates | `integrate` · `send_back` (then `semantic_conflict`) · `needs_human` |
| `push_rejection` (enforce) | the remote's rejection output | `kind`: non_fast_forward / transient / policy | `retry` · `needs_human` (`push_rejected`) |

`foreman-kit approve <change>` integrates a `semantic_conflict` (or `concurrent_work_loss`) change as it is. `foreman-kit evaluator-contract --live` checks that each gate routes its calibration fixtures to the expected action.

A property the pipeline does not guarantee yet is checked as a **known gap**: a violation is printed as a `KNOWN GAP` note and the scenario still passes. Set `FOREMANKIT_SIM_STRICT=1` to make known gaps fail.
