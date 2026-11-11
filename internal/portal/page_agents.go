package portal

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Agents: the engines the kit runs its prompts on. /agents draws them and /api/agents returns
// them. For each one: whether this machine has it and this application uses it, and for which
// roles; its health as the last ping or run left it; who runs it now; and how it worked since a
// date, measured in attempts: how they ended, how long they took, how many times the idle watchdog
// stopped it and the longest pause it came back from, which is what the idle limit has to stay
// above. The registry and the attempts are read from the database in the snapshot of the page;
// what is installed and what the application uses comes from the machine and from its files.

// agentsPeriod is the period of the page when its address names none.
const agentsPeriod = "7d"

// agentRow is one engine.
type agentRow struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	// Known says the kit ships a way to run it. An engine it does not ship (one an older kit ran,
	// the stand-in of a simulation) is listed while the registry has it on record.
	Known bool `json:"known"`
	// Installed says its command is on PATH; Selected, that this application uses it; Roles, the
	// roles of the application that try it.
	Installed bool     `json:"installed"`
	Selected  bool     `json:"selected"`
	Roles     []string `json:"roles"`

	// Health is a value of engines.health_status: "unknown" until a ping or a run checks the engine.
	Health              string `json:"health"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	LastLatencyMS       int64  `json:"last_latency_ms,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	LastCheckedAt       string `json:"last_checked_at,omitempty"`
	// CooldownUntil is until when the kit leaves it for last, "" when it does not.
	CooldownUntil string `json:"cooldown_until,omitempty"`

	// InUse are the processes that run it now; Idle, its attempts that show no activity now.
	InUse []store.EngineUse `json:"in_use"`
	Idle  []store.IdleAgent `json:"idle"`
	// Runs are the same processes as the page draws them, each with its attempt when that one shows
	// no activity; Quiet, the attempts that show none and whose process claimed no engine.
	Runs  []agentRun        `json:"-"`
	Quiet []store.IdleAgent `json:"-"`
	// Stat is how it worked since the date of the page, nil when it made no attempt since then.
	Stat *store.EngineStat `json:"stat,omitempty"`
}

// agentRun is a process that runs an engine now, with its attempt while that one shows no activity.
type agentRun struct {
	store.EngineUse
	Idle *store.IdleAgent
}

// OnRecord says the registry has something about the engine: a check of its health, a process that
// runs it, or an attempt since the date of the page.
func (a agentRow) OnRecord() bool {
	return a.LastCheckedAt != "" || len(a.InUse) > 0 || a.Stat != nil
}

// Ended counts its attempts of the period that ended, and Good, the ones that ended well.
func (a agentRow) Ended() int {
	if a.Stat == nil {
		return 0
	}
	return a.Stat.Attempts - a.Stat.Running
}

func (a agentRow) Good() int {
	if a.Stat == nil {
		return 0
	}
	return a.Stat.ByOutcome["ok"]
}

// WorkSeconds is how long its attempts of the period that ended took in all.
func (a agentRow) WorkSeconds() int {
	if a.Stat == nil {
		return 0
	}
	return int(a.Stat.WorkSeconds)
}

// Others are the ways its attempts of the period ended other than well, by name.
func (a agentRow) Others() []outcomeCount {
	if a.Stat == nil {
		return nil
	}
	var out []outcomeCount
	for outcome, n := range a.Stat.ByOutcome {
		if outcome != "ok" && n > 0 {
			out = append(out, outcomeCount{Outcome: outcome, Count: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Outcome < out[j].Outcome })
	return out
}

// outcomeCount is how many attempts ended one way.
type outcomeCount struct {
	Outcome string
	Count   int
}

// agentsPage is the engines of the kit.
type agentsPage struct {
	// Since is the instant the attempts are counted from, "" when they are all of them.
	Since  string     `json:"since,omitempty"`
	Agents []agentRow `json:"agents"`
	// Hidden counts the engines the kit can run that this machine does not have, that this
	// application does not use and that the registry knows nothing of: the page lists them only
	// when its address asks for all (show=all).
	Hidden int `json:"hidden"`
	// Setup is how this application runs its agents.
	Setup AgentSetup `json:"setup"`

	Periods []stateTab `json:"-"`
	// ShowAll says the page lists every engine; Toggle is its address the other way.
	ShowAll bool   `json:"-"`
	Toggle  string `json:"-"`
	// Busy says a process runs an engine now: the page keeps itself up to date.
	Busy bool `json:"-"`
}

// Installed and Selected count the engines this machine has, and the ones this application uses.
func (p *agentsPage) Installed() int {
	n := 0
	for _, a := range p.Agents {
		if a.Installed {
			n++
		}
	}
	return n
}

func (p *agentsPage) Selected() int {
	n := 0
	for _, a := range p.Agents {
		if a.Selected {
			n++
		}
	}
	return n
}

func (p *agentsPage) head(v *pageView) {
	v.Title, v.Section = "Agents", "/agents"
	v.Crumbs = []crumb{{Label: "Agents"}}
	if p.Busy {
		v.Live = 10
	}
}

func (s *Server) readAgents(ctx context.Context, db *store.View, r *http.Request) (*agentsPage, error) {
	q := r.URL.Query()
	asked, since, err := sinceOf(q, agentsPeriod, db.At())
	if err != nil {
		return nil, err
	}
	show := q.Get("show")
	if show != "" && show != "all" {
		return nil, errInvalid("show is %q to list every engine the kit can run, and %q is not that", "all", show)
	}
	records, err := db.ListEngines(ctx)
	if err != nil {
		return nil, err
	}
	stats, err := db.EngineStats(ctx, since)
	if err != nil {
		return nil, err
	}
	idle, err := db.ListIdleAgents(ctx)
	if err != nil {
		return nil, err
	}

	p := &agentsPage{Agents: []agentRow{}, Setup: s.exec.AgentSetup(), ShowAll: show == "all"}
	if !since.IsZero() {
		p.Since = since.UTC().Format(time.RFC3339)
	}
	// What the kit ships, in the order it names them, and then what only the registry knows.
	rows := map[string]*agentRow{}
	var names []string
	row := func(name string) *agentRow {
		if rows[name] == nil {
			rows[name] = &agentRow{Name: name, Roles: []string{}, Health: "unknown", InUse: []store.EngineUse{}, Idle: []store.IdleAgent{}}
			names = append(names, name)
		}
		return rows[name]
	}
	for _, e := range s.exec.Engines() {
		a := row(e.Name)
		a.Label, a.Known, a.Installed, a.Selected, a.Roles = e.Label, true, e.Installed, e.Selected, e.Roles
	}
	for _, rec := range records {
		a := row(rec.Engine)
		a.Health, a.ConsecutiveFailures, a.LastLatencyMS = rec.Health, rec.ConsecutiveFailures, rec.LastLatencyMS
		a.LastError, a.LastCheckedAt, a.CooldownUntil = rec.LastError, rec.LastCheckedAt, rec.CooldownUntil
		a.InUse = rec.InUse
		p.Busy = p.Busy || len(rec.InUse) > 0
	}
	for i := range stats {
		row(stats[i].Engine).Stat = &stats[i]
	}
	for _, agent := range idle {
		a := row(agent.Engine)
		a.Idle = append(a.Idle, agent)
	}

	// The engines this application uses come first, then the ones this machine has or the registry
	// knows, each group in the order the kit names its engines; the rest only when asked for.
	rank := func(a *agentRow) int {
		switch {
		case a.Selected:
			return 0
		case a.Installed || a.OnRecord():
			return 1
		}
		return 2
	}
	sort.SliceStable(names, func(i, j int) bool { return rank(rows[names[i]]) < rank(rows[names[j]]) })
	for _, name := range names {
		a := rows[name]
		if rank(a) == 2 && !p.ShowAll {
			p.Hidden++
			continue
		}
		// The attempt that shows no activity goes with the process that runs it.
		quiet := map[string]*store.IdleAgent{}
		for i := range a.Idle {
			quiet[a.Idle[i].ExecID] = &a.Idle[i]
		}
		for _, use := range a.InUse {
			run := agentRun{EngineUse: use}
			if use.ExecID != "" {
				run.Idle = quiet[use.ExecID]
				delete(quiet, use.ExecID)
			}
			a.Runs = append(a.Runs, run)
		}
		for i := range a.Idle {
			if quiet[a.Idle[i].ExecID] != nil {
				a.Quiet = append(a.Quiet, a.Idle[i])
			}
		}
		p.Agents = append(p.Agents, *a)
	}

	for _, period := range qualityPeriods {
		value := period[0]
		if value == agentsPeriod {
			value = ""
		}
		p.Periods = append(p.Periods, stateTab{State: period[0], Label: period[1], Href: hrefWith(r, "since", value), Active: period[0] == asked})
	}
	if p.ShowAll {
		p.Toggle = hrefWith(r, "show", "")
	} else {
		p.Toggle = hrefWith(r, "show", "all")
	}
	return p, nil
}
