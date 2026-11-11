---
name: foreman-kit-init
description: Sets up an application right after the Foreman was installed into it. It describes the project (README.md, ROADMAP.md, openspec/config.yaml) and adds the application's code repositories as git submodules under repos/. It gives each repository its repo.config.yaml and environment script from the kit's templates, with the stack, base branch, dev command and gates it detects. Then it checks them, prepares the first commit, and leads to the preflight and the schedules. Use when asked to set up, initialize, onboard or configure a new application, to add or configure a child repository, or to say what is still missing before the cycle can run.
---

# OpenSpec kit init: from installed to ready to schedule

The kit is already installed: `.foreman-kit/kit.lock` exists, written by `foreman-kit init`. If it does not, follow `INSTALL.md` in `~/.foreman-kit` first. Paths are relative to the application's root.

Work with the person who owns the application. Facts about the project come from them and from the repositories themselves; never invent features, commands or constraints.

## Rules

- Never commit or push without the person's OK.
- Never edit the kit's `foreman-kit-*` skills.
- In a child repository, touch only `repo.config.yaml` and its environment script. Never write secrets in them: both are committed.
- A gate must pass on the repository's base branch today. A gate that always fails blocks every change, and one that is wrong lets broken changes through.

## 1. Where it stands

```bash
foreman-kit setup status        # --json for the exact items
```

It lists, in order, what is done (`ok`), what is left (`todo`) with the command or file that resolves it, and warnings (`warn`). It ends with the next step and the command that starts this application's daemon. Work through the `todo` items from the top, and run it again after each step.

## 2. Describe the project

Ask the person, read the repositories, then write:

- **`README.md`**: fill in every `{{...}}`: name, description, one row per repository (path, purpose, base branch), how to get started.
- **`ROADMAP.md`**: the features to build, in priority order, each with the outcome users get and how to tell it works. Take them from the person, never invent them. While `{{...}}` remain, the cycle proposes nothing from the roadmap.
- **`openspec/config.yaml`**: under `context:`, the constraints agents cannot infer from the code: the artifacts' language, platforms, compliance, domain rules. Keep it short.
- **`AGENTS.md`**, the guide every agent reads (`CLAUDE.md` points Claude Code at it): fill in its last section, the project's conventions, with what agents must follow here and cannot infer: naming, architecture rules, the language of user-facing text, libraries to use or avoid. Keep the rest as it is.
- Leave the memory, `.agent-memory/`, as it is: `foreman-kit init` created it with `agent-memory init` and wrote the kit's conventions in it, and the cycle curates it (the `foreman-kit-memory` skill). When it is missing, `agent-memory` was not installed: `foreman-kit setup status` and the preflight say so, with how to install it.

## 3. Add the repositories

For each code repository of the application:

```bash
foreman-kit setup add-repo <url> [--name backend] [--branch main]   # a new submodule under repos/
foreman-kit setup repo <name>                                       # one already under repos/
```

It delivers the kit's templates:

- **`repos/<name>/repo.config.yaml`**, from `.foreman-kit/templates/repo.yaml`, filled with what it detects. That is the stack and its `kind`: `web`, opened in a browser, or `cli`, a terminal program QA runs with arguments. Then the base branch, the dev command, the port and the health endpoint (empty for a cli), the gates (lint, typecheck, test, build) and the full regression suite. A Go module (`go.mod`) is a web server when it uses gin, echo, chi, fiber, gorilla/mux or `net/http`'s server, and a terminal program otherwise (cobra, urfave/cli, bubbletea). Its test gate runs only the packages of the changed files.
- **`script/env.sh`**, from `.foreman-kit/templates/repo_env.sh`, with `env_script` pointing at it, when the repository pins a toolchain (`mise.toml`, `.mise.toml`, `.tool-versions`, `.ruby-version`, `.nvmrc`, `.node-version`, `.python-version`, `.go-version` — loaded through `mise` first, with fallbacks to `asdf`, `rbenv`/`RVM`/`chruby`, `fnm`/`nvm`, `pyenv` and `goenv`).

An existing `repo.config.yaml` is kept unless `--force`. Then, for each repository:

1. **Review every value with the person.** Fill in each `{{...}}` it could not detect; the gates skip placeholders with a warning. Keep `gates.test` fast: it runs twice per change, and `FOREMANKIT_CHANGED_FILES` lists the changed files so it can run only the related tests. `regression.commands.test` keeps the full suite for the cycle's sweep; a repository with no command there has no sweep, because its gates do not stand in for it. When changes build in parallel, give each worker its own test database: `FOREMANKIT_SLOT` becomes `TEST_ENV_NUMBER` in `script/env.sh`, the database name must use it, and `setup` creates it (it is not a gate: it runs before the gates and before the regression). The template also has a `secrets` gate: keep it if `gitleaks` is installed, otherwise install it or remove the gate. In `script/env.sh`, export what the repository's commands need besides the toolchain (never secrets). Three optional keys: `required_services` names the services the gates need (`postgres: "pg_isready -h localhost -p 5432"`), which the preflight checks before each cycle; `regression.when` lists the paths whose change runs the full regression at BUILD and merge (models, initializers, migrations; an entry starting with `!` excludes paths, so `["config/**", "!config/locales/**"]` never reruns it for a translation fix; a commit pushed straight to the repository outside the pipeline, such as a hotfix, never counts as the next change's: with `merge.outside_commits: verify` in `.foreman-kit/pipeline.yaml`, as the kit ships it, the merger runs the regression once on the hotfix alone when its files match); `visual_qa.prepare` readies the visual session's clone (`pnpm install --frozen-lockfile`, a database to migrate and seed, local configuration linked from `$FOREMANKIT_MAIN_ROOT`).
2. **Prove it works:**
   ```bash
   foreman-kit exec <name> true          # the environment loads
   foreman-kit gates --repo <name>       # every gate passes
   ```
   When a gate fails on a clean checkout, fix the command, not the application's code, or ask the person.
3. **Commit it in the child repository and push to its base branch**, with the person's OK. `add-repo` and `repo` print the exact commands.

## 4. Review the settings

```bash
foreman-kit config check
```

With the person, and the `foreman-kit-config` skill to explain and change each one, go through:

- **Agents**: the agent of each role and the permissions (`.foreman-kit/agents.yaml`).
- **Pipeline**: the base branch, how many changes build at once, the limits of agent calls and gates (`.foreman-kit/pipeline.yaml`).
- **Approval**: who approves proposals (`proposals.approved_by` in `.foreman-kit/cycle.yaml`: `evaluator`, `human` or `nobody`).
- **Visual QA**: whether a visual QA session runs (`enabled` in `.foreman-kit/visual_qa.yaml`).
- **Evaluator**: its gates stay `advisory` until their answers have been read.
- **Strategies**: replace the generic ones in `.foreman-kit/exploration.yaml` with the application's own modules, screens and flows. `status` flags them while they are still the template's.

## 5. Commit the application

With the person's OK:

```bash
git add -A && git commit -m "chore: set up the OpenSpec kit" && git push origin <base-branch>
```

The application needs an `origin` remote: the merger and the cycle push to it. Its base branch is `repository.base_branch` in `.foreman-kit/pipeline.yaml`. The key has no value of the kit's own: the seed writes `main`, and nothing starts or integrates while the file names no branch; when the checkout is on another branch, `status` says so. Set it before this commit: `foreman-kit config set pipeline.repository.base_branch <branch>`.

## 6. Check and schedule

1. Run the preflight. It must end with `preflight: OK`; otherwise log in the agent CLI it names, or fix what it reports:
   ```bash
   foreman-kit preflight
   ```
2. Optionally, queue one small item in `.foreman-kit/agent_backlog.json` and run it by hand: `foreman-kit dispatch`, then `foreman-kit merge`.
3. Run one cycle and watch it: `foreman-kit cycle`.
4. Start the daemon, which builds, merges and runs the cycle: `foreman-kit -d -w` (`status` prints the command, and `foreman-kit daemon status` says whether it runs). Schedule the visual QA session in the Claude app or Hermes.

When `status` has no `todo` left, the setup is done. From then on, the `foreman-kit-status` skill tells how the pipeline is going.
