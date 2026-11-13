package sim

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// detachedAgentEntry is what fault-agent-detached-gateway writes in commands.<name> for a detached
// persona: its run command with the state directory of its service, and the keys of a detached agent.
func detachedAgentEntry(name, state string, stop bool, activity bool) map[string]any {
	run := name + " --state " + state + " {{args}} -p {{prompt}}"
	entry := map[string]any{"run": run, "restricted": run, "ping": run, "detached": true}
	if stop {
		entry["stop"] = []any{"bash", "-c", "kill $(cat " + filepath.Join(state, "service.pid") + ") 2>/dev/null; true"}
	}
	if activity {
		entry["activity"] = []any{filepath.Join(state, "activity")}
	}
	return entry
}

// giveDetachedCommands writes those entries into the workspace's .foreman-kit/agents.yaml and commits it.
func (fe *faultEnv) giveDetachedCommands(entries map[string]map[string]any) {
	path := filepath.Join(fe.ws(), ".foreman-kit", "agents.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		fe.note("read agents.yaml: %v", err)
		return
	}
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		fe.note("parse agents.yaml: %v", err)
		return
	}
	commands, _ := data["commands"].(map[string]any)
	if commands == nil {
		commands = map[string]any{}
	}
	for name, entry := range entries {
		commands[name] = entry
	}
	data["commands"] = commands
	out, _ := yaml.Marshal(data)
	_ = os.WriteFile(path, out, 0o644)
	commitWorkspaceConfig(fe.ctx, fe.ws(), "configure detached agents")
}

// A detached agent hands each call to a service outside the process the kit starts (the hermes
// gateway). Without a stop command it holds no role: the dispatcher starts nothing. With one, a call
// that stalls is stopped with it, and no other call starts over the service it left in the worktree;
// a call whose service records progress in its activity file is not stopped as idle.
func (fe *faultEnv) agentDetachedGateway() error {
	fe.pipeline(pipelineOpts{agentSeconds: 90, idleSeconds: 3})
	state := filepath.Join(fe.st.RootDir, "detached-state")
	hangState := filepath.Join(state, "hang")
	slowState := filepath.Join(state, "slow")
	_ = os.MkdirAll(hangState, 0o755)
	_ = os.MkdirAll(slowState, 0o755)
	defer func() {
		if b, err := os.ReadFile(filepath.Join(hangState, "service.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}()

	// 1. Detached, with no stop command, in the build role: nothing starts, and check says why.
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-detached-hang", "sim-agent-clean"})
	fe.giveDetachedCommands(map[string]map[string]any{
		"sim-agent-detached-hang": detachedAgentEntry("sim-agent-detached-hang", hangState, false, false),
	})
	fe.backlog("detached-stopped")
	_ = fe.dispatch(1, time.Time{})
	execs, _ := fe.db.ListExecutions(fe.ctx, "worker", "detached-stopped", 0)
	why := settings.Unusable(fe.ws(), settings.DispatcherFiles...)
	if err := fe.check("a detached agent without a stop command in the build role: the dispatcher starts nothing and says why",
		len(execs) == 0 && strings.Contains(why, "sim-agent-detached-hang is detached")); err != nil {
		return err
	}
	checkText := strings.Join(config.Check(fe.ws())[config.AgentsFile].Errors, "\n")
	if err := fe.check("config check names the role and the agent that cannot hold it",
		strings.Contains(checkText, "roles.build.agents: sim-agent-detached-hang is detached")); err != nil {
		return err
	}

	// 2. With a stop command: the stalled call is stopped with it, its service ends, and the next agent
	// of the role builds the change in the same worktree.
	fe.giveDetachedCommands(map[string]map[string]any{
		"sim-agent-detached-hang": detachedAgentEntry("sim-agent-detached-hang", hangState, true, false),
	})
	if err := fe.dispatchMerge(1); err != nil {
		fe.note("detached-stopped pass: %v", err)
	}
	if err := fe.check("the change is integrated by the next agent once the stalled detached call was stopped", fe.integrated("detached-stopped")); err != nil {
		return err
	}
	stalled, cleanAfter := false, false
	execs, _ = fe.db.ListExecutions(fe.ctx, "worker", "detached-stopped", 0)
	for _, ex := range execs {
		attempts, _ := fe.db.ListAttempts(fe.ctx, ex.ExecID)
		for _, a := range attempts {
			if a.Engine == "sim-agent-detached-hang" && a.Outcome == store.AttemptIdleStalled {
				stalled = true
			}
			if stalled && a.Engine == "sim-agent-clean" && a.Outcome == store.AttemptOK {
				cleanAfter = true
			}
		}
	}
	if err := fe.check("the detached call stalled and the next agent built after it", stalled && cleanAfter); err != nil {
		return err
	}
	if gone, checked := waitNoProcess("sleep 86398", 5*time.Second); checked {
		if err := fe.check("the stop command ended the service the detached call left in the worktree", gone); err != nil {
			return err
		}
	} else {
		fe.note("pgrep unavailable: leftover-service check skipped")
	}

	// 3. A detached call whose service records progress in its activity file outlives the idle limit.
	fe.agents([]string{"sim-agent-clean"}, []string{"sim-agent-detached"})
	fe.giveDetachedCommands(map[string]map[string]any{
		"sim-agent-detached": detachedAgentEntry("sim-agent-detached", slowState, true, true),
	})
	fe.backlog("detached-slow")
	if err := fe.dispatchMerge(1); err != nil {
		fe.note("detached-slow pass: %v", err)
	}
	okDetached := false
	execs, _ = fe.db.ListExecutions(fe.ctx, "worker", "detached-slow", 0)
	for _, ex := range execs {
		attempts, _ := fe.db.ListAttempts(fe.ctx, ex.ExecID)
		for _, a := range attempts {
			if a.Engine == "sim-agent-detached" && a.Outcome == store.AttemptOK {
				okDetached = true
			}
		}
	}
	return fe.check("a detached call busy only through its activity file for longer than the idle limit is not stopped, and its change integrates",
		okDetached && fe.integrated("detached-slow"))
}
