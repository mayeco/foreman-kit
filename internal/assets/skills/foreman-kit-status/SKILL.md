---
name: foreman-kit-status
description: How the autonomous pipeline of the Foreman is going, read-only. Reports what runs now (a cycle, a dispatch, a visual QA session, changes in flight), what the cycles did in a period (changes integrated or blocked, proposals, visual checks, agent time), what is pending, how far the roadmap got, the system's health and what needs a person. Use when asked how the cycle, the pipeline or the project is going, what it did or is doing, whether it is running, or what is waiting for a human.
---

# OpenSpec kit status: how is it going?

Read-only. Never approve, unblock, pause, resume, edit or start anything here; operating the cycle is the `foreman-kit-cycle` skill's job ("Operating the cycle"). Paths are relative to the application's root.

## Gather

1. Run the report for the period the person asks about (24 hours when they do not say):
   ```bash
   foreman-kit progress --since 24h        # or 90m, 7d, 2026-10-01
   ```
   Add `--json` when you need exact figures or lists. Every number you give comes from this output; never estimate one.
2. When something looks stuck, add the instant snapshot: `foreman-kit status` (locks, processes, worktrees, child repositories). An `AGENT IDLE` line names an agent that has shown no activity for a while (no file changed, nothing printed, no command of its own using CPU), for how long, and when the idle watchdog stops it (`limits.idle_timeout` in `.foreman-kit/agents.yaml`): it may be reading or waiting for its model, so say it is idle, not that it failed.
   When the person asks what comes next, or why an item has not started: `foreman-kit dispatch --dry-run` (`--json` for exact fields). It is read-only and works while a dispatch runs: every pending item in the order it would start, its priority after aging, how long it waited, and what holds it (building, waiting for the merger or held by a broken base, blocked and its retry, a dependency and that dependency's state, a capability in use, or already ended by the running dispatch, so it starts in the next one).
3. For the judgment's own account of the last cycle (what it verified, found and proposed), read the newest file in `log/cycle/reports/` (visual sessions: `visual-*.md`).
4. For one change: inspect its blocked `reason` and `details` via `foreman-kit status` or `foreman-kit progress --json`, its archived proposal in `openspec/changes/archive/`, and its `repairs.md` (what each repair round was asked to fix and whether it then passed).
5. When the person asks whether an edit of the agents' prompts or of the memory helped: `foreman-kit prompt-report --since 30d` (`--by memory` for the memory). Say how many changes each version started; a version with a few says little.

## Answer

Short, in the person's language, in four blocks:

1. **Dashboard**: running or idle; what runs (a cycle judging, a dispatch and its changes in flight, a visual session); paused and why.
2. **Done** in the period: the changes integrated, by name; how many cycles ran and how they ended; visual checks and their verdicts; proposals made, approved, integrated.
3. **Pending**: the backlog by state, how many changes are open against `queue.max_open`, proposals waiting, the visual queue, roadmap progress.
4. **Needs you**: one line per item, with the command that resolves it:
   - a broken base (`BASE BROKEN` in `foreman-kit status`, `needs_human.broken_bases` in `foreman-kit progress --json`, details in `foreman-kit base status`): one item, however many changes it holds. A commit pushed straight to that child repository outside the pipeline (a hotfix) fails its regression or a gate on its own, so the merger holds the repository's ready changes in the queue instead of quarantining them, and every other repository keeps integrating. They resume by themselves once someone fixes the repository's base branch, or when the merger's one automatic recheck of that base (`merge.base_recheck_after` after it failed; the line says when) passes, in case what failed has healed; `foreman-kit base accept <repo> <sha>` (the line gives it) integrates them anyway: the tests failing on that base stay accepted, for the changes built on it later too, and any other failing test is the change's own gate_failure. A worker can meet a test that fails on the base before the merger does, with no broken base listed yet: `foreman-kit base accept <repo>` then checks the base the umbrella records on its own first (every gate and the regression, in a worktree of its own) and accepts what fails there, or says the base passes and the failure is the change's;
   - a blocked change: read its reason; for `risk_review`, review the diff, then run `foreman-kit approve <change>` (or `foreman-kit unblock <change>` after fixing the cause, or `foreman-kit abandon <change>` to discard it). One the report shows with an automatic retry still to come (`retry 1 of 1 at <time>`) needs nobody yet: say when it runs again;
   - a proposal waiting: `foreman-kit proposals approve <change>` or `reject <change>`; for a prompt proposal, read it first with `foreman-kit proposals show <change>`;
   - a pause: fix the reason, then `rm log/PAUSE`;
   - a visual check that could not verify: what was missing, from its note.

Add one line on health only when something is off: consecutive failures near the limit, a failing preflight, cycles that stop before the end, no cycle in a period when the cycle is scheduled hourly, or a full queue (`QUEUE FULL` in `foreman-kit status`, `pending.queue.full` in `foreman-kit progress --json`): while as many changes are open as `queue.max_open` allows, the cycle verifies and reports but queues nothing, approved proposals wait (`pending.proposals_approved`), and the daemon starts no cycle before its interval. It clears when a change is integrated or abandoned, so say which blocked or held changes fill it.

## Reading the signals

| Cycle outcome | Means |
| :--- | :--- |
| `done` | Judged, recorded and dispatched (under the daemon: handed to its continuous dispatcher) |
| `judged; an earlier dispatch was still running` | Normal: it verified and queued, and left building to the dispatch in progress |
| `dispatched only (visual QA session running)` | Normal: the visual session held the judgment that hour |
| `paused` | `log/PAUSE` exists; nothing runs until a person removes it |
| `scope violation` | The judgment wrote outside its scope; the system paused itself and recorded nothing |
| `preflight failed` | An agent CLI was not logged in, a kit file was edited, or a tool was missing; see the cycle's log |
| `stopped before the end` | The process died (killed, the computer slept); the next cycle continues. Repeated: read `log/cycle/cycle-<stamp>.log` |
| `running (<step>)` | The newest cycle is still at that step |

- `CONFIG:` followed by a file and a key, under `== Guards`: that configuration file lacks a key, holds a value the key does not accept or writes a key nothing reads, and whatever reads it starts nothing new until it is mended. `foreman-kit config check` lists every problem; the `foreman-kit-config` skill mends them.
- No cycle at all in the last hours: the daemon is not running (`foreman-kit daemon status`; `foreman-kit -d -w` starts it), or the computer was asleep.

## The per-project daemon

`foreman-kit daemon status` says whether it runs (`foreman-kit -d -w` starts it with the portal, `foreman-kit daemon stop` stops it); its log is `log/daemon.log`. While it runs, `the daemon's continuous dispatcher` and `the merger` always show as running in `foreman-kit progress`: that is normal, not a stuck dispatch. They merge each change as soon as it is ready and start each eligible item as soon as a worker is free; only the cycle's judgment runs every `cycle.every`, or `cycle.every_when_out_of_work` after the last one began (both in `.foreman-kit/daemon.yaml`; `never`: never sooner) once a worker is free with nothing to start and fewer than `queue.max_open` changes are open: `daemon status` then says when the next cycle runs at the latest and what keeps it from running sooner, and the daemon log says why a cycle ran before its tick. Each wake-up is logged (`merger: woke (ready)`, `dispatcher: woke (backlog)`); `daemon: merger waits` means a cron cycle or a CLI holds that lock, and the daemon takes over when it ends. A change deferred because every agent is out is retried after a cooldown of a minute, doubling up to `outage.max_wait` (`.foreman-kit/agents.yaml`) (`deferred: every engine is out; no worker starts before <time>` in the log): nobody needs to act unless it ends blocked `engine_unavailable`.
- Changes integrated but the roadmap does not move: its items are not proposed yet, or the proposals wait for approval.
- Many changes blocked for the same reason: a systemic problem; say so first.
