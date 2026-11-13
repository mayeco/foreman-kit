package sim

import (
	"os"
	"path/filepath"
)

// SimulatedAgents lists the fake agent binaries installed into <stack>/bin/
// and configurable in .foreman-kit/agents.yaml like any real agent CLI.
var SimulatedAgents = []string{
	"sim-agent-clean",
	"sim-agent-quota",
	"sim-agent-auth-broken",
	"sim-agent-bad-spec",
	"sim-agent-overlong-spec",
	"sim-agent-flaky-code",
	"sim-agent-unchecked-task",
	"sim-agent-conflict",
	"sim-agent-rogue",
	"sim-agent-needs-human",
	"sim-agent-always-fail",
	"sim-agent-cycle",
	"sim-agent-router",
	// Fault-injection personas (see faults.go).
	"sim-agent-crash-midway",
	"sim-agent-hang",
	"sim-agent-rate-limit",
	"sim-agent-garbage",
	"sim-agent-noisy",
	"sim-agent-self-commit",
	"sim-agent-branch-switch",
	"sim-agent-escape",
	"sim-agent-clobber",
	"sim-agent-clobber-learns",
	"sim-agent-stray",
	"sim-agent-gives-up",
	"sim-agent-enospc",
	"sim-agent-reorder",
	// Detached personas: the client of a gateway whose work goes on in a service outside the call's
	// process group (see fault-agent-detached-gateway).
	"sim-agent-detached",
	"sim-agent-detached-hang",
}

// InstallSimulatedAgentBinaries writes all fake agent executables into binDir.
// Every binary is a standalone executable that responds to preflight pings ("pong")
// and directly writes OpenSpec artifacts or Go code in the worktree according to its persona
// (or according to the change name when using sim-agent-router).
func InstallSimulatedAgentBinaries(binDir string) error {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	for _, name := range SimulatedAgents {
		script := buildAgentScript(name)
		p := filepath.Join(binDir, name)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func buildAgentScript(persona string) string {
	return `#!/usr/bin/env bash
set -euo pipefail

PERSONA="` + persona + `"
PROMPT_TEXT=""
if [ -n "${FOREMANKIT_PROMPT_FILE:-}" ] && [ -f "${FOREMANKIT_PROMPT_FILE}" ]; then
  PROMPT_TEXT="$(cat "${FOREMANKIT_PROMPT_FILE}")"
else
  for arg in "$@"; do
    PROMPT_TEXT="$PROMPT_TEXT $arg"
  done
fi

# 1. Preflight ping check
case "$PROMPT_TEXT" in
  *"Reply with ONLY the word: pong"*|*"ONLY the word: pong"*)
    if [ "$PERSONA" = "sim-agent-auth-broken" ]; then
      echo "Error: 401 Unauthorized - not logged in" >&2
      exit 1
    fi
    echo "pong"
    exit 0
    ;;
esac

WORKDIR="${FOREMANKIT_WORKDIR:-$(pwd)}"
cd "$WORKDIR"

CHANGE="${FOREMANKIT_CHANGE:-}"
if [ -z "$CHANGE" ]; then
  if [ -d "openspec/changes" ]; then
    for d in openspec/changes/*; do
      [ -d "$d" ] || continue
      base="$(basename "$d")"
      [ "$base" = "archive" ] && continue
      CHANGE="$base"
      break
    done
  fi
fi

PHASE="${FOREMANKIT_PHASE:-}"
ROLE="${FOREMANKIT_ROLE:-}"
if [ -z "$ROLE" ]; then
  if [ -n "$CHANGE" ] && [ ! -f "openspec/changes/$CHANGE/tasks.md" ]; then
    ROLE="plan"
  elif [ -n "$CHANGE" ]; then
    ROLE="build"
  else
    ROLE="cycle"
  fi
fi

# Dynamic routing when using sim-agent-router or when persona is overridden by change prefix
EFFECTIVE="$PERSONA"
if [ "$PERSONA" = "sim-agent-router" ]; then
  case "$CHANGE" in
    bad-spec-*|repair-spec-*)   EFFECTIVE="sim-agent-bad-spec" ;;
    overlong-*)                 EFFECTIVE="sim-agent-overlong-spec" ;;
    flaky-*|repair-code-*)      EFFECTIVE="sim-agent-flaky-code" ;;
    unchecked-*)                EFFECTIVE="sim-agent-unchecked-task" ;;
    conflict-*)                 EFFECTIVE="sim-agent-conflict" ;;
    rogue-*|tamper-*)           EFFECTIVE="sim-agent-rogue" ;;
    ambiguous-*|needs-human-*)  EFFECTIVE="sim-agent-needs-human" ;;
    broken-*|always-fail-*)     EFFECTIVE="sim-agent-always-fail" ;;
    garbage-*)                  EFFECTIVE="sim-agent-garbage" ;;
    noisy-*)                    EFFECTIVE="sim-agent-noisy" ;;
    escape-*)                   EFFECTIVE="sim-agent-escape" ;;
    self-commit-*)              EFFECTIVE="sim-agent-self-commit" ;;
    stray-*)                    EFFECTIVE="sim-agent-stray" ;;
    branch-switch-*)            EFFECTIVE="sim-agent-branch-switch" ;;
    *)                          EFFECTIVE="sim-agent-clean" ;;
  esac
fi

# Prints a few MB of noise: long lines, ANSI escapes and bytes that are not valid UTF-8.
emit_noise() {
  local i=0
  local pad
  pad="$(printf 'z%.0s' {1..700})"
  while [ $i -lt 4000 ]; do
    printf '\033[31mNOISE %05d\033[0m \xff\xfe\xc3\x28 %s\n' "$i" "$pad"
    i=$((i + 1))
  done
}

# GARBAGE persona: says a lot, changes nothing, claims success.
if [ "$EFFECTIVE" = "sim-agent-garbage" ]; then
  emit_noise
  echo "sim-agent-garbage: done (wrote nothing)"
  exit 0
fi

# RATE-LIMIT persona: provider throttling with a long Retry-After; changes nothing.
if [ "$EFFECTIVE" = "sim-agent-rate-limit" ]; then
  echo "Error: 429 Too Many Requests: rate limit reached for requests. Please try again in 90s." >&2
  exit 1
fi

# NOISY persona: floods stdout, then behaves like the clean persona.
if [ "$EFFECTIVE" = "sim-agent-noisy" ]; then
  emit_noise
fi

write_valid_plan() {
  local chg="$1"
  local cap="feature-$(printf '%s' "$chg" | tr '_' '-')"
  mkdir -p "openspec/changes/$chg/specs/$cap"
  cat > "openspec/changes/$chg/proposal.md" <<EOF
# Proposal: $chg

## Why
Users need deterministic support for $chg in the backend service with automated verification.

## What Changes
- Add handler and business logic for $chg in repos/backend.
- Add automated unit tests verifying $chg scenarios.
EOF

  cat > "openspec/changes/$chg/design.md" <<EOF
# Design: $chg

## Architecture
Implement the $chg module inside repos/backend with a pure Go function and unit test suite.
EOF

  cat > "openspec/changes/$chg/tasks.md" <<EOF
# Tasks: $chg

- [ ] 1.1 Implement $chg logic in repos/backend
- [ ] 1.2 Add automated tests for $chg in repos/backend
EOF

  cat > "openspec/changes/$chg/specs/$cap/spec.md" <<EOF
# $cap Specification

## Purpose
Governs the $cap capability so callers receive deterministic responses for $chg in production.

## ADDED Requirements

### Requirement: Deterministic $chg execution
The backend MUST execute $chg and return a verified status string.

#### Scenario: Standard $chg invocation
- **GIVEN** the backend service is initialized
- **WHEN** the caller invokes the $chg operation
- **THEN** the response status equals "ok:$chg"
EOF
}

write_valid_build() {
  local chg="$1"
  local safe_id
  safe_id="$(printf '%s' "$chg" | tr -c 'a-zA-Z0-9' '_')"
  mkdir -p repos/backend
  cat > "repos/backend/feature_${safe_id}.go" <<EOF
package backend

func Feature_${safe_id}() string {
	return "ok:${chg}"
}
EOF

  cat > "repos/backend/feature_${safe_id}_test.go" <<EOF
package backend

import "testing"

func TestFeature_${safe_id}(t *testing.T) {
	if got := Feature_${safe_id}(); got != "ok:${chg}" {
		t.Fatalf("expected ok:${chg}, got %s", got)
	}
}
EOF

  if [ -f "openspec/changes/$chg/tasks.md" ]; then
    sed 's/- \[ \]/- [x]/g; s/\* \[ \]/* [x]/g; s/+ \[ \]/+ [x]/g; s/1\. \[ \]/1. [x]/g; s/2\. \[ \]/2. [x]/g' \
      "openspec/changes/$chg/tasks.md" > "openspec/changes/$chg/tasks.md.tmp"
    mv "openspec/changes/$chg/tasks.md.tmp" "openspec/changes/$chg/tasks.md"
  fi

  cat > "openspec/changes/$chg/handoff.md" <<EOF
- cue: when working on $chg
  lesson: keep feature functions pure and covered by table tests
  evidence: repos/backend/feature_${safe_id}_test.go
  confidence: high
EOF
}

# Handle CYCLE role
if [ "$ROLE" = "cycle" ] || [ "$EFFECTIVE" = "sim-agent-cycle" ]; then
  # The judgment writes the memory the way a real one does: through agent-memory, in this worktree.
  if [ -f ".agent-memory/pitfalls.md" ]; then
    if ! grep -q "Simulated cycle lesson" .agent-memory/pitfalls.md; then
      agent-memory propose --intent add_pitfall --op append_to_section \
        --path pitfalls.md --section-id pitfalls \
        --content "- **Simulated cycle lesson**: verified integrated changes and updated long-term memory." >/dev/null || exit 1
    fi
  fi
  if [ -f ".foreman-kit/agent_proposals.json" ]; then
    cat > ".foreman-kit/agent_proposals.json" <<'EOF'
[
  {
    "change_name": "add-audit-trail",
    "priority": "normal",
    "description": "Add structured audit trail endpoint and unit tests in repos/backend",
    "source": "roadmap",
    "status": "proposed"
  }
]
EOF
  fi
  echo "sim-agent-cycle: completed cycle judgment"
  exit 0
fi

# Handle AUTH-BROKEN persona (triggers 60m auth_failed cooldown in the engines table)
if [ "$EFFECTIVE" = "sim-agent-auth-broken" ]; then
  echo "Error: 401 Unauthorized - authentication failed, not logged in" >&2
  exit 1
fi

# Handle QUOTA persona: leaves partial work and exits 1 with quota exhaustion message (30m quota_exceeded cooldown)
if [ "$EFFECTIVE" = "sim-agent-quota" ]; then
  if [ -n "$CHANGE" ]; then
    mkdir -p "openspec/changes/$CHANGE"
    echo "partial progress by sim-agent-quota" > "openspec/changes/$CHANGE/partial_attempt.txt"
  fi
  echo "Error: You have hit your session limit, resets 9pm" >&2
  exit 1
fi

# Handle NEEDS-HUMAN persona
if [ "$EFFECTIVE" = "sim-agent-needs-human" ]; then
  write_valid_plan "$CHANGE"
  cat > "openspec/changes/$CHANGE/.needs-human" <<EOF
Ambiguous requirement in $CHANGE: should the endpoint require mTLS or Bearer JWT tokens?
EOF
  echo "sim-agent-needs-human: wrote .needs-human question"
  exit 0
fi

# Handle BAD-SPEC persona (fails first PLAN, repairs on plan-repair-1)
if [ "$EFFECTIVE" = "sim-agent-bad-spec" ] && [ "$ROLE" = "plan" ]; then
  case "$PHASE" in
    plan-repair*)
      rm -rf "openspec/changes/$CHANGE/specs/broken-cap"
      write_valid_plan "$CHANGE"
      echo "sim-agent-bad-spec: repaired spec on $PHASE"
      exit 0
      ;;
    *)
      mkdir -p "openspec/changes/$CHANGE/specs/broken-cap"
      echo "# Proposal" > "openspec/changes/$CHANGE/proposal.md"
      echo "# Design" > "openspec/changes/$CHANGE/design.md"
      printf "# Tasks\n- [ ] 1.1 Do work\n" > "openspec/changes/$CHANGE/tasks.md"
      printf "# Broken Spec\n\nSome invalid spec text without Purpose or Scenario.\n" > "openspec/changes/$CHANGE/specs/broken-cap/spec.md"
      echo "sim-agent-bad-spec: wrote intentionally invalid spec on first attempt"
      exit 0
      ;;
  esac
fi

# Handle OVERLONG-SPEC persona (writes >500-char requirement paragraph on attempt 1, repairs on plan-repair-1)
if [ "$EFFECTIVE" = "sim-agent-overlong-spec" ] && [ "$ROLE" = "plan" ]; then
  case "$PHASE" in
    plan-repair*)
      rm -rf "openspec/changes/$CHANGE/specs"
      write_valid_plan "$CHANGE"
      echo "sim-agent-overlong-spec: shortened requirement paragraph to <=500 chars on $PHASE"
      exit 0
      ;;
    *)
      write_valid_plan "$CHANGE"
      cap="feature-$(printf '%s' "$CHANGE" | tr '_' '-')"
      long_line="$(printf 'X%.0s' {1..520})"
      cat > "openspec/changes/$CHANGE/specs/$cap/spec.md" <<EOF
# $cap Specification

## Purpose
Governs the $cap capability so callers receive deterministic responses for $CHANGE in production.

## ADDED Requirements

### Requirement: Overlong $CHANGE requirement
The system SHALL enforce this overlong requirement paragraph: ${long_line}

#### Scenario: Standard $CHANGE invocation
- **WHEN** the caller invokes $CHANGE
- **THEN** the response status equals "ok:$CHANGE"
EOF
      echo "sim-agent-overlong-spec: wrote >500-char requirement paragraph on initial plan"
      exit 0
      ;;
  esac
fi

# Standard PLAN for all other personas
if [ "$ROLE" = "plan" ]; then
  write_valid_plan "$CHANGE"
  echo "$EFFECTIVE: completed PLAN for $CHANGE"
  exit 0
fi

# The state directory of a detached persona's service: --state <dir> in its command.
detached_state() {
  local prev=""
  for arg in "$@"; do
    if [ "$prev" = "--state" ]; then printf '%s' "$arg"; return; fi
    prev="$arg"
  done
}

# start_service <state> <script>: runs script in a session of its own, out of the call's process
# group, with its current directory in the work tree (what a gateway's shell commands do).
start_service() {
  if command -v perl >/dev/null 2>&1; then
    perl -MPOSIX -e 'POSIX::setsid(); exec "bash", "-c", $ARGV[0]' "$2" </dev/null >"$1/service.log" 2>&1 &
  else
    nohup bash -c "$2" </dev/null >"$1/service.log" 2>&1 &
  fi
}

# BUILD phase personas
case "$EFFECTIVE" in
  sim-agent-detached)
    # A gateway's client: the work goes on in a service of its own session, which records progress in
    # an activity file outside the work tree for six seconds, then builds like sim-agent-clean. The
    # client prints nothing and waits.
    STATE="$(detached_state "$@")"
    mkdir -p "$STATE"
    rm -f "$STATE/done"
    start_service "$STATE" "for i in 1 2 3 4 5 6 7 8 9 10 11 12; do echo progress \$i >> '$STATE/activity'; sleep 0.5; done; sim-agent-clean; echo \$? > '$STATE/done'"
    while [ ! -s "$STATE/done" ]; do sleep 1; done
    rc="$(cat "$STATE/done")"
    rm -f "$STATE/done"
    exit "$rc"
    ;;

  sim-agent-detached-hang)
    # A gateway's client whose service hangs in the work tree: killing the client stops nothing; only
    # the agent's stop command ends the service.
    STATE="$(detached_state "$@")"
    mkdir -p "$STATE"
    start_service "$STATE" "echo \$\$ > '$STATE/service.pid'; exec sleep 86398"
    exec sleep 86397
    ;;

  sim-agent-crash-midway)
    # Writes half of the feature (code, no test, tasks unchecked) and dies by SIGKILL.
    safe_id="$(printf '%s' "$CHANGE" | tr -c 'a-zA-Z0-9' '_')"
    mkdir -p repos/backend
    cat > "repos/backend/feature_${safe_id}.go" <<EOF
package backend

func Feature_${safe_id}() string {
	return "ok:${CHANGE}"
}
EOF
    echo "sim-agent-crash-midway: wrote half the feature, crashing now" >&2
    kill -9 $$
    ;;

  sim-agent-hang)
    # Hangs without output, CPU, or file changes (an idle agent the watchdog must stop).
    exec sleep 86399
    ;;

  sim-agent-self-commit)
    # Commits its own work in the child repository and the umbrella, like many real agents do.
    write_valid_build "$CHANGE"
    git -C repos/backend add -A
    git -C repos/backend -c user.name=agent -c user.email=agent@example.com commit -q -m "agent: implement $CHANGE" || true
    git add -A
    git -c user.name=agent -c user.email=agent@example.com commit -q -m "agent: implement $CHANGE (umbrella)" || true
    echo "sim-agent-self-commit: committed its own work for $CHANGE"
    exit 0
    ;;

  sim-agent-branch-switch)
    # Commits and then leaves both the umbrella and the child on branches it made up.
    write_valid_build "$CHANGE"
    git -C repos/backend checkout -q -B "agent-child-$CHANGE"
    git -C repos/backend add -A
    git -C repos/backend -c user.name=agent -c user.email=agent@example.com commit -q -m "agent: implement $CHANGE" || true
    git checkout -q -B "agent-side-$CHANGE"
    git add -A
    git -c user.name=agent -c user.email=agent@example.com commit -q -m "agent: implement $CHANGE on a side branch" || true
    echo "sim-agent-branch-switch: left umbrella and child on made-up branches"
    exit 0
    ;;

  sim-agent-escape)
    # Builds correctly but also writes outside its worktree.
    write_valid_build "$CHANGE"
    if [ -n "${FOREMANKIT_APP_ROOT:-}" ]; then
      echo "escaped by $CHANGE" > "${FOREMANKIT_APP_ROOT}/ESCAPED_BY_AGENT_${CHANGE}.txt" || true
    fi
    echo "escaped by $CHANGE" > "../ESCAPED_SIBLING_${CHANGE}.txt" || true
    echo "sim-agent-escape: wrote outside the worktree"
    exit 0
    ;;

  sim-agent-stray)
    # Builds correctly but leaves an OS file and a debug log at the umbrella root.
    write_valid_build "$CHANGE"
    printf '\0\0\0\1Bud1' > .DS_Store
    printf '2026-11-09 12:00:01 DEBUG starting build of %s\n2026-11-09 12:00:04 DEBUG tests passed\n' "$CHANGE" > debug-agent.log
    echo "sim-agent-stray: built $CHANGE and left .DS_Store and debug-agent.log behind"
    exit 0
    ;;

  sim-agent-gives-up)
    # Runs, decides the task cannot be done, and stops without changing anything.
    echo "I cannot implement $CHANGE: the requirement asks for the endpoint to be both public and admin-only, which contradicts itself. Giving up without changes."
    exit 1
    ;;

  sim-agent-enospc)
    # The machine's disk is full: the agent cannot write anything.
    echo "fatal: write error: No space left on device" >&2
    exit 1
    ;;

  sim-agent-reorder)
    # Puts its route first, before "baseline" (idempotent): textually clean against a teammate's commit at
    # the end of the file, semantically against an invariant that commit states.
    write_valid_build "$CHANGE"
    if ! grep -q "route-from-${CHANGE}" repos/backend/routes_shared.go; then
      awk -v r="route-from-${CHANGE}" '/"baseline",/ { printf "\t\"%s\",\n", r } { print }' \
        repos/backend/routes_shared.go > repos/backend/routes_shared.go.tmp
      mv repos/backend/routes_shared.go.tmp repos/backend/routes_shared.go
    fi
    echo "sim-agent-reorder: put route-from-${CHANGE} before baseline"
    exit 0
    ;;

  sim-agent-clobber-learns)
    # Clobbers like sim-agent-clobber, but when the merger reports lost concurrent work it rebuilds the
    # file from the child's base and keeps its own route.
    write_valid_build "$CHANGE"
    if printf '%s' "$PROMPT_TEXT" | grep -q "## Concurrent work lost" && \
       git -C repos/backend show origin/main:routes_shared.go > repos/backend/routes_shared.go.base 2>/dev/null; then
      if ! grep -q "route-from-${CHANGE}" repos/backend/routes_shared.go.base; then
        awk -v r="route-from-${CHANGE}" '/^}/ { printf "\t\"%s\",\n}\n", r; next } { print }' \
          repos/backend/routes_shared.go.base > repos/backend/routes_shared.go
      else
        cp repos/backend/routes_shared.go.base repos/backend/routes_shared.go
      fi
      rm -f repos/backend/routes_shared.go.base
      echo "sim-agent-clobber-learns: restored the teammate's lines and kept route-from-${CHANGE}"
      exit 0
    fi
    rm -f repos/backend/routes_shared.go.base
    cat > "repos/backend/routes_shared.go" <<EOF
package backend

var RouteRegistry = []string{
	"route-from-${CHANGE}",
}
EOF
    echo "sim-agent-clobber-learns: rewrote routes_shared.go for $CHANGE"
    exit 0
    ;;

  sim-agent-clobber)
    # Always rewrites routes_shared.go with only its own route, dropping whatever else is there
    # (including a teammate's route after a rebase conflict).
    write_valid_build "$CHANGE"
    cat > "repos/backend/routes_shared.go" <<EOF
package backend

var RouteRegistry = []string{
	"route-from-${CHANGE}",
}
EOF
    echo "sim-agent-clobber: rewrote routes_shared.go for $CHANGE"
    exit 0
    ;;

  sim-agent-flaky-code)
    case "$PHASE" in
      repair*)
        write_valid_build "$CHANGE"
        echo "sim-agent-flaky-code: fixed failing test on $PHASE"
        exit 0
        ;;
      *)
        write_valid_build "$CHANGE"
        safe_id="$(printf '%s' "$CHANGE" | tr -c 'a-zA-Z0-9' '_')"
        cat > "repos/backend/feature_${safe_id}_test.go" <<EOF
package backend

import "testing"

func TestFeature_${safe_id}(t *testing.T) {
	t.Fatalf("simulated flaky bug in first build attempt for ${CHANGE}")
}
EOF
        echo "sim-agent-flaky-code: introduced failing test on initial build"
        exit 0
        ;;
    esac
    ;;

  sim-agent-unchecked-task)
    case "$PHASE" in
      repair*)
        write_valid_build "$CHANGE"
        echo "sim-agent-unchecked-task: checked remaining task in tasks.md on $PHASE"
        exit 0
        ;;
      *)
        write_valid_build "$CHANGE"
        # Leave task 1.2 unchecked (- [ ]) on initial apply
        cat > "openspec/changes/$CHANGE/tasks.md" <<EOF
# Tasks: $CHANGE

- [x] 1.1 Implement $CHANGE logic in repos/backend
- [ ] 1.2 Add automated tests for $CHANGE in repos/backend
EOF
        echo "sim-agent-unchecked-task: left Line 4 unchecked in tasks.md on initial build"
        exit 0
        ;;
    esac
    ;;

  sim-agent-always-fail)
    write_valid_build "$CHANGE"
    safe_id="$(printf '%s' "$CHANGE" | tr -c 'a-zA-Z0-9' '_')"
    cat > "repos/backend/feature_${safe_id}_test.go" <<EOF
package backend

import "testing"

func TestFeature_${safe_id}(t *testing.T) {
	t.Fatalf("persistent gate failure in ${CHANGE}")
}
EOF
    echo "sim-agent-always-fail: wrote persistently failing test"
    exit 0
    ;;

  sim-agent-rogue)
    write_valid_build "$CHANGE"
    printf "\n# rogue modification by sim-agent-rogue\n" >> repos/backend/repo.config.yaml
    echo "sim-agent-rogue: tampered with repos/backend/repo.config.yaml"
    exit 0
    ;;

  sim-agent-conflict)
    write_valid_build "$CHANGE"
    if [ -f "repos/backend/routes_shared.go" ] && grep -q "route-from-" repos/backend/routes_shared.go; then
      # Pure POSIX awk (no python3 dependency on Linux/macOS)
      if ! grep -q "route-from-${CHANGE}" repos/backend/routes_shared.go; then
        awk -v r="route-from-${CHANGE}" '
          /^}/ { printf "\t\"%s\",\n}\n", r; next }
          { print }
        ' repos/backend/routes_shared.go > repos/backend/routes_shared.go.tmp
        mv repos/backend/routes_shared.go.tmp repos/backend/routes_shared.go
      fi
      echo "sim-agent-conflict: resolved conflict cleanly on rebuild for $CHANGE"
    else
      cat > "repos/backend/routes_shared.go" <<EOF
package backend

var RouteRegistry = []string{
	"route-from-${CHANGE}",
}
EOF
      echo "sim-agent-conflict: wrote conflicting routes_shared.go for $CHANGE"
    fi
    exit 0
    ;;

  *)
    write_valid_build "$CHANGE"
    if printf '%s' "$PROMPT_TEXT" | grep -q "A previous attempt stopped before finishing"; then
      echo "continued_after_previous_attempt=true" > "openspec/changes/$CHANGE/continued.txt"
    fi
    echo "$EFFECTIVE: completed BUILD for $CHANGE"
    exit 0
    ;;
esac
`
}
