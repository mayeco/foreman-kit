---
name: foreman-kit-config
description: Explains and changes the settings of an application built with the Foreman. Covers how a change travels to the base branch (.foreman-kit/pipeline.yaml), the agents, their roles, commands and limits (.foreman-kit/agents.yaml), what a cycle does (.foreman-kit/cycle.yaml), when it runs and the portal (.foreman-kit/daemon.yaml) and the browser session (.foreman-kit/visual_qa.yaml). Also covers the exploration strategies (.foreman-kit/exploration.yaml), the evaluator and its gates (.foreman-kit/evaluator.yaml), each child repository's repo.config.yaml, the agents' prompts (.foreman-kit/prompts/), a value given to one run with --set, and what the environment still carries. Use when asked what a setting does, how the system is configured or why it behaves a certain way. Also use to change a setting, an agent's role, command or prompt, a strategy, a gate, the approval mode or the visual session.
---

# OpenSpec kit configuration: understand it, change it safely

Paths are relative to the application's root. `foreman-kit config` knows each setting: what it does, the value the kit ships, and which process reads it and when. Answer from its output, never from memory.

Seven files, each read by one process: `pipeline.yaml` (the dispatcher and the merger), `agents.yaml` (every agent call), `cycle.yaml` (the cycle), `daemon.yaml` (the daemon, when it starts), `visual_qa.yaml` (the browser session), `exploration.yaml` (the cycle and the browser session) and `evaluator.yaml` (every gate of the evaluator). Three rules hold for all of them:

- **Every key is written in its file.** One that is missing or invalid is an error of `foreman-kit config check`, never read as some other value: `foreman-kit update` writes the ones a file lacks. Until the file is mended, whatever reads it starts nothing new (the dispatcher and the merger, the cycle, the visual session, a daemon that is starting); what is already being built goes on. `foreman-kit status` says so on its `CONFIG:` line.
- **Times carry their unit**: `30s`, `15m`, `2h`, `1h30m`; `never` where a key says so. A bare number is refused.
- **A key of an older kit is an error, not an alias.** `check` says where its value went; `foreman-kit update` rewrites the files of an application written for an older kit, once.

## Understand

1. Show the settings. Add a topic for one area, or `--json` for exact values:
   ```bash
   foreman-kit config show                 # the seven files, then repos, prompts, env
   foreman-kit config show agents
   ```
2. Check them. It reports keys that are missing or invalid, keys nothing reads (a typo, or a key of an older kit), values that do not fit together, and what would stop the next cycle:
   ```bash
   foreman-kit config check                # ends with config: OK or config: FAIL
   ```
3. Ask what the records argue for changing. From what the kit recorded (agents and gates a limit cut, retries that helped or did not, workers with nothing to start), it lists the settings another value would have served better, with the value now, the one it recommends, why, and what would have been gained. It needs a day of dispatching before it says anything, changes nothing itself, and uses no agent:
   ```bash
   foreman-kit config recommend            # the last 24 hours; --window 7d, 15d or 30d for longer
   foreman-kit config recommend --evidence # also what each rule saw, even with nothing to recommend
   ```
4. Explain in the person's language, organized by their question rather than by file. For each setting, say:
   - what it changes in the pipeline;
   - its value, and whether that value is the one the kit ships;
   - when a change takes effect;
   - what changing it would cause.

| The question | The settings |
| :--- | :--- |
| Who plans, builds, judges? | `agents.roles.plan.agents`, `.build.agents`, `.cycle.agents`: each a list of agents in order; one that fails is tried again `agents.limits.retries` times, then the next one continues its work. A role without agents does not run. The agents themselves: `agents.agents` |
| Which agent starts? Can the work spread over several quotas? | `agents.selection`: `in_order` (always the role's first agent), `random` (any of them first, then the others in the order listed) or `least_busy` (the one with the fewest calls running, so concurrent changes go to different agents) |
| What exactly does an agent run? | `agents.commands.<agent>`: `run`, `restricted` and `ping`, each run exactly as written: the kit adds nothing. Placeholders: `{{prompt}}`, `{{prompt_file}}`, `{{workdir}}`, `{{timeout}}` (the role's timeout, in seconds) and `{{args}}` (the role's args). A spending cap or a tool list is an option written in the command. An agent whose CLI hands each call to a service outside the process the kit starts (the hermes gateway) is `detached: true`: it holds a role only with a `stop` command (`{{workdir}}`, `{{pid}}`), and its `activity` lists the files it writes as it works, which the idle watchdog watches. Edit `.foreman-kit/agents.yaml` by hand, then `check` |
| What may agents run? | `agents.permissions`: `full` (each agent's `run` command) or `restricted` (each agent's `restricted` command; an agent without one cannot be used, and `check` refuses it in a role) |
| Does an agent need a model, a provider or another option? | `agents.roles.<role>.args`: agent => its arguments in that role (a string split as a shell would, or a list). The preflight's ping uses those of the first role that lists the agent. Edit `.foreman-kit/agents.yaml` by hand, then `check` |
| How long may an agent call take? | `agents.roles.<role>.timeout` (one call of that role; the cycle's is the hard limit of its judgment), `agents.limits.idle_timeout` (an agent that shows no work that long has stalled and is stopped; `never`: not watched), `.idle_cpu_percent` (below this share of one core, the CPU its processes use is a waiting CLI, not work), `.retry_after`, `.ping_timeout` |
| What happens when every agent of a role is out? | `agents.outage.max_wait` (no worker starts for a minute, doubling up to this) and `agents.outage.give_up_after` (outages a change waits through before it is blocked; `never`) |
| How long does a cycle spend, and on what? | `cycle.budget.total`, `cycle.budget.exploring`, the strategies of `.foreman-kit/exploration.yaml` and their `selection`. `check` warns when the budget does not fit in `agents.roles.cycle.timeout` |
| How much new work per cycle? | `cycle.queue.max_findings`, `cycle.roadmap.max_proposals`, `cycle.roadmap.max_open` |
| How much work may be open at once? | `cycle.queue.max_open`: at that many changes queued, building, ready, merging, blocked or waiting to be rebuilt, the cycle queues nothing new, approved proposals wait, and the daemon starts no cycle early. `foreman-kit status` says how many are open (`open_changes`, or `QUEUE FULL`) |
| When does the daemon run the cycle? | `daemon.cycle.every` at the latest, and `daemon.cycle.every_when_out_of_work` after the last one began once a worker is free with nothing to start, the queue has room and something moved in the last two cycles (`never`: only `every`). `foreman-kit daemon status` says what the next cycle waits for. The daemon reads `.foreman-kit/daemon.yaml` when it starts: restart it after an edit |
| Is the portal served? | `daemon.portal.enabled`, `.bind`, `.port` |
| Who approves proposals? | `cycle.proposals.approved_by` (`evaluator`, `human`, `nobody`), and with `evaluator`, `evaluator.gates.proposal_review.mode`; a prompt edit: `cycle.proposals.prompt_edits_approved_by` |
| Is there a browser session? | `visual_qa.enabled`, `.time_budget`, `.expires_after` (when a session that never ended frees its lock), `.max_checks`, `.max_findings`, `.slot`, `.skills`. It runs the strategies whose `session` is `visual_qa` |
| How strict is the evaluator? | `evaluator.enabled`, `evaluator.gates.<gate>.mode` (`off`, `advisory`, `enforce`), the gate's thresholds, `evaluator.when_unavailable` (`pass`, `block`, or `stop`: like block, and the preflight fails). What each gate answered, and what `enforce` would have stopped: `foreman-kit evaluation-report` |
| What runs before a change merges? | The `gates` of each `repos/<name>/repo.config.yaml` (fast: after every BUILD and again at merge), within `pipeline.gates.timeout`, after its `setup`; its `regression.commands` run once per cycle, and after the gates when the change touches a path of `regression.when`. A repository without regression commands has no sweep |
| Which branch, which remote? | `pipeline.repository.base_branch` for the application (it has no value of the kit's own: without it nothing starts), `.remote`, `.worktrees_dir`; `base_branch` in each child repository's `repo.config.yaml` |
| How many changes at once? | `pipeline.dispatch.workers` |
| How often is a failed phase sent back to its agent? | `pipeline.build.repair_rounds` |
| What happens to conflicts, to hotfixes and to low items? | `pipeline.merge.conflict_rebuilds` (automatic rebuilds on the new base before `rebase_conflict`), `pipeline.merge.outside_commits` (`verify`, `ignore` or `include`: how commits pushed to a child repository outside the pipeline are tested), `pipeline.merge.base_recheck_after` (when a base that failed on its own is checked again), `pipeline.dispatch.priority_bump_every` (one priority up each time an item has waited that long) |
| Is a blocked change tried again? | `pipeline.blocked_changes.retries` (automatic retries of a change blocked for a reason that can heal on its own, such as `gate_failure` or `push_failed`; never `policy_violation`, `spec_ambiguity`, `risk_review`, `missing_child_commits` or a system failure) and `pipeline.blocked_changes.retry_after` (the wait before each). The `BLOCKED` lines of `foreman-kit status` say when the next retry comes, or why none will |
| What do the agents read? | `.foreman-kit/prompts/plan.md`, `build.md`, `cycle.md`: the prompts, this application's own (`show prompts`) |
| When does it run? | The schedules, not a setting |

## Change

1. **Confirm first.** Tell the person the setting, its old and new value, and the effect. Change it only after they agree. A recommendation of `foreman-kit config recommend` is advice like any other: each row gives its `config set` command, and a recommended value is at most double and at least half the value now, so that the next days say whether it was enough.
2. **A plain value** (a number, a time, true or false, a mode, the agents of a role, the base branch): use `set`. It keeps the file's comments, writes `"off"` quoted (YAML reads a bare `off` as false), and refuses a value the key does not take or a role for an agent that is not installed. It adds a key the file lacks, with the sections above it:
   ```bash
   foreman-kit config set cycle.proposals.approved_by human --dry-run
   foreman-kit config set cycle.proposals.approved_by human
   foreman-kit config set agents.limits.idle_timeout 30m
   ```
3. **A list, a mapping or a text**: edit the file with your edit tool, keeping its comments and style, then run `foreman-kit config check`. This covers the strategies, the evaluator's thresholds, the commands and the args of the agents, and the `skills` of each session. Never rewrite a YAML file through a YAML library: it drops the comments that document every setting.
   - **Strategy**: `slug` (kebab-case, unique), `name`, `kind` (`defects` go to the backlog; `improvements` and `features` become proposals), `directive` (name the application's own modules, screens, flows and paths, as concretely as you would brief a person), `session` (`cycle`, or `visual_qa` for the browser session: no strategy changes session on its own, so one of a session that is off does not run) and optional `enabled: false`.
   - **Agent command**: write the whole command line under `commands.<agent>.run` (and `.restricted`, `.ping`). It runs exactly as written, so every option the agent needs is in it; `{{args}}` is where the role's `args` go. A custom binary gets its own entry and is then listed in a role.
   - **Evaluator threshold or question**: decide it from `foreman-kit evaluation-report --gate <gate>` (each question's scores against its threshold, and what enforce would have stopped beside what happened). Edit `.foreman-kit/evaluator.yaml`, then run `foreman-kit config check` and `foreman-kit evaluator-contract`. Then run `foreman-kit evaluator-contract --live` to recalibrate against the real model; it costs about 11 calls.
4. **Commit and push it, with the person's OK**: `git add <file> && git commit -m "chore(config): <what>" && git push origin <base-branch>`.
   - The cycle's judgment and the visual session may only leave the memory (`.agent-memory/`), the backlog and the proposals uncommitted. When either one finds anything else uncommitted, its scope guard pauses the whole system.
   - So commit right after editing. Edit while `foreman-kit status` shows no cycle judging and no visual session.
   - If the system paused itself this way, commit the change, then `rm log/PAUSE`.
5. **Say when it takes effect**: `set` prints which process reads it. The dispatcher, the merger, the cycle and every agent call read their settings on each run, so the next one uses the new value. The daemon reads `.foreman-kit/daemon.yaml` when it starts: restart it.

## What is not changed here

| To change | Do |
| :--- | :--- |
| The agents an application uses | `foreman-kit update [<app>] --agents LIST [--plan A --build A --cycle A]`. It installs or removes their skills and writes the commands the kit ships for a new agent, on a branch for review |
| Files written for an older kit (`check` says `no longer read`, or a file is missing) | `foreman-kit update [<app>]`: it rewrites the seven files with the application's own values in the keys of today, says what it could not move (`REVIEW`), and commits on a branch for review |
| A child repository's gates, dev command, base branch or `env_script` | Edit `repos/<name>/repo.config.yaml` in the child repository, commit there and push to its base branch, then commit the new pointer in the application. The pipeline's agents may never change these files: the merger blocks it. The `foreman-kit-init` skill sets them up |
| An agent's prompt (`show prompts`) | Edit `.foreman-kit/prompts/plan.md`, `build.md` or `cycle.md` by hand: keep the `{{placeholders}}` its header comment names (`{{feedback}}` above all: without it a repair round never sees what failed), run `check`, show the result with `foreman-kit prompts render <name> change=demo ...`, then commit it like a setting |
| A value for one run | `foreman-kit --set <file>.<key>=<value> <command>`, with the key as `show` lists it: `foreman-kit --set pipeline.dispatch.workers=4 dispatch`. It takes the keys `set` takes (one value, or the agents of a role: `--set agents.roles.build.agents=codex,claude`), holds for that run and what it starts, writes no file, and is refused before the run when the key or the value is wrong, or when this application cannot use the value (an agent it does not list, a slot a worker takes): what `set` would refuse, `--set` refuses. Never in a scheduled prompt: what the application always wants is written in its file |
| An environment variable (`show env`) | None changes a setting: the environment carries the secrets (`TYPESAFE_API_KEY`), where things are on the machine (`FOREMANKIT_HOME`) and what the kit hands to what it runs. A variable an older kit read (`OPENSPEC_WORKERS`, `AGENT_PERMISSIONS`, `JEV_DISABLED`) is an error of `check` while it is set, with what does its job now, and what read its file starts nothing until it is unset |
| The `foreman-kit-*` skills | Never edit them: they are the kit's. An upgrade replaces them, and the preflight fails while they differ. A need there is a change to the framework |
| `<file>.new` next to a setting after an upgrade | The new template, kept out of git (`*.new` is ignored). Merge what you need into the file, then delete the `.new` |

## Common changes

| The person wants | Run |
| :--- | :--- |
| To approve every proposal themselves | `set cycle.proposals.approved_by human` |
| The evaluator to block risky changes | Read what it answered first, `foreman-kit evaluation-report --gate change_risk`, then `set evaluator.gates.change_risk.mode enforce` |
| No evaluator at all | `set evaluator.enabled false` |
| No browser session | `set visual_qa.enabled false` (its strategies then do not run: give them `session: cycle` to keep them) |
| Another installed agent to judge, with a second one behind it | `set agents.roles.cycle.agents codex,claude` |
| Agents held to their restricted commands | `set agents.permissions restricted` (refused while a role lists an agent without one) |
| hermes in a role is refused (`is detached ... has no commands.hermes.stop`) | Its calls run in the hermes gateway and cannot be stopped one by one: name another agent in the role (`set agents.roles.<role>.agents claude`), or write a `stop` command under `commands.hermes` if hermes offers one |
| Strategies in random order | `set exploration.selection random` |
| Shorter cycles | `set cycle.budget.total 30m` |
| The cycle only on its interval | `set daemon.cycle.every_when_out_of_work never`, then restart the daemon |
| The application works on `main` | `set pipeline.repository.base_branch main` |
| More changes built at once | `set pipeline.dispatch.workers 3` |
| Blocked changes wait for a person | `set pipeline.blocked_changes.retries 0` |
