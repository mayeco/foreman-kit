package settings

import (
	"fmt"
	"strings"
	"time"
)

// Former is a key an application's files held before the seven files of today: where its value
// went, or what replaced it. Nothing reads a former key. `foreman-kit update` moves its value to
// the keys that took it over, and `foreman-kit config check` says, for one still written, where
// it went.
type Former struct {
	// Key is the key as it was written, with the name of the file it lived in
	// ("pipeline.limits.gate_seconds").
	Key Key
	// To lists the keys that take its value.
	To []Key
	// Convert gives the value the shape of the key to: seconds become a time with its unit, a
	// renamed word becomes the new one. old reads another former key of the same application, for
	// the few values that came out of two keys. Nil: the value moves as it is.
	Convert func(raw any, to Key, old func(Key) any) (any, bool)
	// Gone says, for a key nothing took over, what does its job now.
	Gone string
}

// NoLongerRead says where the value of the former key went, for the message about a file that
// still writes it.
func (f Former) NoLongerRead() string {
	if len(f.To) == 0 {
		return "no longer read: " + f.Gone
	}
	places := make([]string, len(f.To))
	for i, to := range f.To {
		places[i] = to.Where()
	}
	return fmt.Sprintf("no longer read: it is %s (foreman-kit update rewrites the file)", oneOfThem(places))
}

// oneOfThem joins places as a sentence does: "a and b", "a, b and c".
func oneOfThem(places []string) string {
	if len(places) < 2 {
		return strings.Join(places, "")
	}
	return strings.Join(places[:len(places)-1], ", ") + " and " + places[len(places)-1]
}

// inUnit returns a Convert that reads a whole number of unit and writes it as a time: 7200 seconds
// are 2h. zero is what 0 becomes ("never", "0s"), or "" when 0 was not a value of the key.
func inUnit(unit time.Duration, zero string) func(any, Key, func(Key) any) (any, bool) {
	return func(raw any, _ Key, _ func(Key) any) (any, bool) {
		n, ok := wholeNumber(raw)
		if !ok || n < 0 {
			return nil, false
		}
		if n == 0 {
			return zero, zero != ""
		}
		return FormatDuration(time.Duration(n) * unit), true
	}
}

// plus returns a Convert like inUnit that adds extra to the time: the margin the kit used to add on
// its own to the value it read.
func plus(unit, extra time.Duration) func(any, Key, func(Key) any) (any, bool) {
	return func(raw any, _ Key, _ func(Key) any) (any, bool) {
		n, ok := wholeNumber(raw)
		if !ok || n <= 0 {
			return nil, false
		}
		return FormatDuration(time.Duration(n)*unit + extra), true
	}
}

// renamed returns a Convert that replaces each word of a fixed set by the one that says the same
// today.
func renamed(words map[string]string) func(any, Key, func(Key) any) (any, bool) {
	return func(raw any, _ Key, _ func(Key) any) (any, bool) {
		text, ok := raw.(string)
		if !ok {
			return nil, false
		}
		word, ok := words[strings.TrimSpace(text)]
		return word, ok
	}
}

// positive is a Convert for a count whose 0 meant "the value the kit ships": a number above 0 moves
// as it is, and 0 becomes that value, written.
func positive(raw any, to Key, _ func(Key) any) (any, bool) {
	n, ok := wholeNumber(raw)
	if !ok || n < 0 {
		return nil, false
	}
	if n == 0 {
		shipped := Of(to).Default
		return shipped, shipped != nil
	}
	return n, true
}

// The words that were renamed, old to new.
var (
	formerSelection      = map[string]string{"fallback": SelectionInOrder, "random": SelectionRandom, "spread": SelectionLeastBusy}
	formerPermissions    = map[string]string{"all": PermissionsFull, "restricted": PermissionsRestricted}
	formerOutsideCommits = map[string]string{"verify": OutsideCommitsVerify, "change": OutsideCommitsIgnore, "integrated": OutsideCommitsInclude}
	formerApproval       = map[string]string{"evaluator": ApprovalEvaluator, "jev": ApprovalEvaluator, "human": ApprovalHuman, "auto": ApprovalNobody}
	formerPromptApproval = map[string]string{"human": ApprovalHuman, "auto": ApprovalNobody}
	formerExploration    = map[string]string{"rotation": ExplorationInOrder, "random": ExplorationRandom}
	formerUnavailable    = map[string]string{"skip": UnavailablePass, "block": UnavailableBlock}
	formerProvider       = map[string]string{"openai": "openai", "anthropic": "anthropic", "google": "google", "gemini": "google", "openrouter": "openrouter", "ollama": "ollama", "compat": "compat"}
)

// The former keys other former keys are read with.
const (
	formerBlockedRetries = Key("pipeline.dispatch.blocked_retries")
	formerMergeInterval  = Key("pipeline.dispatch.merge_interval_seconds")
)

// baseRecheckFloor is the least the merger waited before it checked a broken base again, whatever
// the keys said.
const baseRecheckFloor = 5 * time.Minute

// formerBaseRecheck gives merge.base_recheck_after the wait the merger used to work out of three
// keys: dispatch.blocked_retry_hours, never less than five minutes or the merger's own interval,
// and never at all with dispatch.blocked_retries at 0.
func formerBaseRecheck(raw any, _ Key, old func(Key) any) (any, bool) {
	hours, ok := wholeNumber(raw)
	if !ok || hours < 0 {
		return nil, false
	}
	if retries, set := wholeNumber(old(formerBlockedRetries)); set && retries <= 0 {
		return Never, true
	}
	wait := time.Duration(hours) * time.Hour
	floor := baseRecheckFloor
	if seconds, set := wholeNumber(old(formerMergeInterval)); set && time.Duration(seconds)*time.Second > floor {
		floor = time.Duration(seconds) * time.Second
	}
	if wait < floor {
		wait = floor
	}
	return FormatDuration(wait), true
}

// formerRoleAgents moves the agents of a role, which were the role's whole value, under its agents
// key. A role already written as a mapping is not a former value.
func formerRoleAgents(raw any, _ Key, _ func(Key) any) (any, bool) {
	switch raw.(type) {
	case string, []any, []string:
		return raw, true
	}
	return nil, false
}

// formerExtraArgs gives a role the arguments extra_args held for it: an agent's own value, or, when
// that was a mapping per role, the entry of the role or else its default.
func formerExtraArgs(role string) func(any, Key, func(Key) any) (any, bool) {
	return func(raw any, _ Key, _ func(Key) any) (any, bool) {
		byAgent, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		out := map[string]any{}
		for agent, args := range byAgent {
			if perRole, isMap := args.(map[string]any); isMap {
				args = perRole[role]
				if args == nil {
					args = perRole["default"]
				}
			}
			if args == nil {
				continue
			}
			if text, isText := args.(string); isText && strings.TrimSpace(text) == "" {
				continue
			}
			out[agent] = args
		}
		return out, true
	}
}

// formerSlot reads the slot of the visual session, which said false for none.
func formerSlot(raw any, _ Key, _ func(Key) any) (any, bool) {
	if raw == false {
		return SlotNone, true
	}
	if text, ok := raw.(string); ok && (strings.TrimSpace(text) == "" || strings.EqualFold(strings.TrimSpace(text), "false")) {
		return SlotNone, true
	}
	n, ok := ParseSlot(raw)
	if !ok {
		return nil, false
	}
	if n == 0 {
		return SlotNone, true
	}
	return n, true
}

// formers lists the former keys, in the order of the files they lived in.
var formers = buildFormers()

func buildFormers() []Former {
	out := []Former{
		// ---- .foreman-kit/pipeline.yaml
		{Key: "pipeline.base_branch", To: []Key{BaseBranch}},
		{Key: "pipeline.dispatch.wait_seconds", Gone: "a one-pass dispatch takes its wait from the command line (foreman-kit dispatch --wait 30m)"},
		{Key: formerMergeInterval, Gone: "a watching merger takes its interval from the command line (foreman-kit merge --watch --interval 10s), and a broken base is checked again " + BaseRecheckAfter.Where() + " after it failed"},
		{Key: "pipeline.dispatch.conflict_rebuilds", To: []Key{ConflictRebuilds}},
		{Key: "pipeline.dispatch.aging_hours", To: []Key{PriorityBumpEvery}, Convert: inUnit(time.Hour, Never)},
		{Key: formerBlockedRetries, To: []Key{BlockedRetries}},
		{Key: "pipeline.dispatch.blocked_retry_hours", To: []Key{BlockedRetryAfter}, Convert: inUnit(time.Hour, "0s")},
		{Key: "pipeline.dispatch.blocked_retry_hours", To: []Key{BaseRecheckAfter}, Convert: formerBaseRecheck},
		{Key: "pipeline.limits.agent_seconds", To: []Key{RoleTimeout("plan"), RoleTimeout("build")}, Convert: inUnit(time.Second, "")},
		{Key: "pipeline.limits.agent_idle_seconds", To: []Key{IdleTimeout}, Convert: inUnit(time.Second, Never)},
		{Key: "pipeline.limits.agent_idle_cpu_percent", To: []Key{IdleCPUPercent}},
		{Key: "pipeline.limits.agent_retries", To: []Key{AgentRetries}},
		{Key: "pipeline.limits.agent_retry_delay_seconds", To: []Key{AgentRetryAfter}, Convert: inUnit(time.Second, "0s")},
		{Key: "pipeline.limits.agent_budget_usd", Gone: "a spending cap is an option of the agent's own command (--max-budget-usd in " + AgentCommands.Path() + ".claude of " + AgentCommands.FilePath() + ")"},
		{Key: "pipeline.limits.gate_seconds", To: []Key{GateTimeout}, Convert: inUnit(time.Second, "")},
		{Key: "pipeline.limits.lease_seconds", To: []Key{LocksStaleAfter}, Convert: inUnit(time.Second, "")},
		{Key: "pipeline.gates.merge_changed_files", To: []Key{OutsideCommits}, Convert: renamed(formerOutsideCommits)},
		{Key: "pipeline.llm.enabled", To: []Key{DiagnosticsEnabled}},
		{Key: "pipeline.llm.provider", To: []Key{DiagnosticsProvider}, Convert: renamed(formerProvider)},
		{Key: "pipeline.llm.model", To: []Key{DiagnosticsModel}},
		{Key: "pipeline.llm.base_url", To: []Key{DiagnosticsBaseURL}},
		{Key: "pipeline.llm.timeout_seconds", To: []Key{DiagnosticsTimeout}, Convert: inUnit(time.Second, "")},
		{Key: "pipeline.llm.max_retries", To: []Key{DiagnosticsRetries}},

		// ---- .foreman-kit/agents.yaml
		{Key: "agents.order", To: []Key{AgentSelection}, Convert: renamed(formerSelection)},
		{Key: AgentPermissions, To: []Key{AgentPermissions}, Convert: renamed(formerPermissions)},
		{Key: "agents.extra_args", To: []Key{RoleArgs("plan")}, Convert: formerExtraArgs("plan")},
		{Key: "agents.extra_args", To: []Key{RoleArgs("build")}, Convert: formerExtraArgs("build")},
		{Key: "agents.extra_args", To: []Key{RoleArgs("cycle")}, Convert: formerExtraArgs("cycle")},

		// ---- .foreman-kit/cycle.yaml
		{Key: "cycle.qa_time_budget_minutes", To: []Key{CycleBudget}, Convert: inUnit(time.Minute, "")},
		// The judgment's hard limit was its budget plus a margin of fifteen minutes.
		{Key: "cycle.qa_time_budget_minutes", To: []Key{RoleTimeout("cycle")}, Convert: plus(time.Minute, 15*time.Minute)},
		{Key: "cycle.max_consecutive_failures", Gone: QueueMaxOpen.Path() + " stops the cycle from queuing while too many changes are open"},
		{Key: "cycle.exploration.minutes", To: []Key{CycleExploring}, Convert: inUnit(time.Minute, "")},
		{Key: "cycle.exploration.max_new_backlog_items_per_cycle", To: []Key{QueueMaxFindings, VisualQAMaxFindings}},
		{Key: "cycle.exploration.areas", Gone: "the strategies are in " + ExplorationStrategies.FilePath() + ", one entry each"},
		{Key: "cycle.roadmap.max_new_per_cycle", To: []Key{RoadmapMaxProposals}},
		{Key: "cycle.proposals.approval", To: []Key{ProposalsApprovedBy}, Convert: renamed(formerApproval)},
		{Key: "cycle.proposals.prompt_approval", To: []Key{PromptEditsApprovedBy}, Convert: renamed(formerPromptApproval)},
		{Key: "cycle.visual_qa.enabled", To: []Key{VisualQAEnabled}},
		{Key: "cycle.visual_qa.minutes", To: []Key{VisualQATimeBudget}, Convert: inUnit(time.Minute, "")},
		// The session's lock expired thirty minutes after its budget.
		{Key: "cycle.visual_qa.minutes", To: []Key{VisualQAExpiresAfter}, Convert: plus(time.Minute, 30*time.Minute)},
		{Key: "cycle.visual_qa.max_items_per_session", To: []Key{VisualQAMaxChecks}},
		{Key: "cycle.visual_qa.slot", To: []Key{VisualQASlot}, Convert: formerSlot},
		{Key: "cycle.qa.skills", To: []Key{CycleSkills, VisualQASkills}},
		{Key: MemoryMaxChars, To: []Key{MemoryMaxChars}, Convert: positive},
		{Key: "cycle.memory.scoped_max_chars", To: []Key{MemoryMaxCharsPerScope}, Convert: positive},
		{Key: "cycle.daemon.web", To: []Key{DaemonPortal}},
		{Key: "cycle.daemon.bind", To: []Key{DaemonBind}},
		{Key: "cycle.daemon.port", To: []Key{DaemonPort}},
		{Key: "cycle.daemon.interval_minutes", To: []Key{CycleEvery}, Convert: inUnit(time.Minute, "")},
		{Key: "cycle.daemon.idle_delay_minutes", To: []Key{CycleEveryWhenOutOfWork}, Convert: inUnit(time.Minute, Never)},

		// ---- .foreman-kit/exploration.yaml
		{Key: "exploration.order", To: []Key{ExplorationSelection}, Convert: renamed(formerExploration)},

		// ---- .foreman-kit/evaluator.yaml
		{Key: "evaluator.max_retries", To: []Key{EvaluatorRetries}},
		{Key: "evaluator.max_state_chars", To: []Key{EvaluatorStateMaxChars}},
		{Key: "evaluator.include_diffs", To: []Key{EvaluatorIncludeDiffs}},
		{Key: "evaluator.on_unavailable", To: []Key{EvaluatorWhenUnavailable}, Convert: renamed(formerUnavailable)},
	}
	for _, role := range Roles {
		out = append(out, Former{Key: Key("agents.roles." + role), To: []Key{RoleAgents(role)}, Convert: formerRoleAgents})
	}
	return out
}

// Formers returns the former keys.
func Formers() []Former {
	return append([]Former(nil), formers...)
}

// formerAt returns the former key that key is, or is written under (an entry of extra_args), with
// the text of that former key. It reports "" for a key that never was one, and for a key of today
// that only held other words before.
func formerAt(key Key) (Former, string) {
	for _, f := range formers {
		if _, current := index[f.Key]; current {
			continue
		}
		if key == f.Key {
			return f, string(f.Key)
		}
		// What a former mapping held is its key's, unless keys of today live under the same name
		// (roles.plan was a list and is now a mapping of settings).
		if strings.HasPrefix(string(key), string(f.Key)+".") && !holdsCurrentKeys(f.Key) {
			return f, string(f.Key)
		}
	}
	return Former{}, ""
}

// holdsCurrentKeys reports whether a key of the catalog is written under key.
func holdsCurrentKeys(key Key) bool {
	for _, s := range all {
		if strings.HasPrefix(string(s.Key), string(key)+".") {
			return true
		}
	}
	return false
}

// Move is what an application's former keys give a key of today.
type Move struct {
	// Value is the value for the key, in the shape it takes.
	Value any
	// From is the former key the value came from.
	From Key
	// Was is what that former key held.
	Was any
	// Lost names a former key that is written but whose value the key cannot take (a word the kit
	// never knew): nothing moved, and a person has to look at it. "" when nothing was lost.
	Lost Key
}

// Moved returns the value an application's former keys give key, already in the shape key takes:
// old reads one former key as its file held it. It reports false when no former key of key is
// written, or none holds something that converts; Move.Lost then says whether one was written.
func Moved(key Key, old func(Key) any) (Move, bool) {
	s, known := Lookup(key)
	if !known {
		return Move{}, false
	}
	var lost Move
	for _, f := range formers {
		takes := false
		for _, to := range f.To {
			if to == key {
				takes = true
				break
			}
		}
		if !takes {
			continue
		}
		raw := old(f.Key)
		if raw == nil {
			continue
		}
		if f.Key == key && s.Problem(raw) == "" {
			// The key kept its name and already holds a value of today.
			continue
		}
		value := raw
		if f.Convert != nil {
			converted, ok := f.Convert(raw, key, old)
			if !ok {
				lost = Move{Lost: f.Key, Was: raw}
				continue
			}
			value = converted
		}
		if s.Problem(value) != "" {
			lost = Move{Lost: f.Key, Was: raw}
			continue
		}
		return Move{Value: value, From: f.Key, Was: raw}, true
	}
	return lost, false
}

// HasFormer reports whether the file called name, as data holds it, writes a former key or a
// former value: it was written for an older foreman-kit and `foreman-kit update` rewrites it.
func HasFormer(name string, data map[string]any) bool {
	for _, f := range formers {
		if f.Key.File() != name {
			continue
		}
		raw := dig(data, f.Key.Parts()...)
		if raw == nil {
			continue
		}
		s, current := Lookup(f.Key)
		if !current {
			// A role that already is a mapping is not the former list of its agents.
			if _, isMap := raw.(map[string]any); isMap && holdsCurrentKeys(f.Key) {
				continue
			}
			return true
		}
		// The key is one of today's: it is former only while it holds a word of before.
		if s.Problem(raw) != "" && f.Convert != nil {
			if _, ok := f.Convert(raw, f.Key, func(Key) any { return nil }); ok {
				return true
			}
		}
	}
	return false
}
