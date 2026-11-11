package portal

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Assessments: which gates fail and how long they take, and how the evaluator answers, since a
// date. /quality draws it and /api/quality returns it. Per repository and gate: how many times the
// gate ran, how many of those failed and how long they took. Per gate of the evaluator: how many
// questions it was asked, how it answered them, what the orchestrator did about the answers and
// what they cost. The database adds the numbers up from the rows of gate runs and of evaluations:
// nothing here is counted from a log or from a report in a file.

// sinceEverything is the period that starts with the first row on record.
const sinceEverything = "all"

// qualityPeriod is the period of the page when its address names none.
const qualityPeriod = "7d"

// qualityPeriods are the periods the page offers: how the address names each, and its words.
var qualityPeriods = [][2]string{
	{"24h", "24 hours"}, {"7d", "7 days"}, {"30d", "30 days"}, {sinceEverything, "Everything"},
}

// sinceOf reads the instant an aggregate starts from, as the address gives it in since: a period
// back from now ("90m", "24h", "7d"), an instant, or "all" for everything on record, which is the
// zero time. An address that says nothing gets the period fallback. It returns since as it was
// asked for, too: that is what a page marks among the periods it offers.
func sinceOf(q url.Values, fallback string, now time.Time) (string, time.Time, error) {
	asked := strings.TrimSpace(q.Get("since"))
	if asked == "" {
		asked = fallback
	}
	if asked == sinceEverything {
		return asked, time.Time{}, nil
	}
	since, err := cycle.ParseSince(asked, now)
	if err != nil {
		return asked, time.Time{}, errInvalid("since is a period back from now (24h, 7d), an instant (2026-11-05T09:00:00Z) or %q for everything on record, and %q is none of them",
			sinceEverything, asked)
	}
	return asked, since, nil
}

// gateSum is what the gates of the page add up to.
type gateSum struct {
	Repos, Gates, Runs, Failures int
	TotalMS                      int64
}

// evaluationSum is what the evaluations of the page add up to.
type evaluationSum struct {
	Gates, Evaluations, Passed, Cached int
	CostUSD                            float64
}

// qualityPage is what the gates found and how the evaluator answered since a date.
type qualityPage struct {
	// Since is the instant the numbers start from, "" when they cover everything on record.
	Since string `json:"since,omitempty"`
	// Gates holds one entry for each gate of each repository that ran, by repository and gate.
	Gates []store.GateStat `json:"gates"`
	// Evaluations holds one entry for each gate of the evaluator that was asked, by gate.
	Evaluations []store.EvaluationStat `json:"evaluations"`

	// Periods are the periods the page offers, with the one it shows marked.
	Periods []stateTab    `json:"-"`
	GateSum gateSum       `json:"-"`
	EvalSum evaluationSum `json:"-"`
}

func (p *qualityPage) head(v *pageView) {
	v.Title, v.Section = "Assessments", "/quality"
	v.Crumbs = []crumb{{Label: "Assessments"}}
}

func (s *Server) readQuality(ctx context.Context, db *store.View, r *http.Request) (*qualityPage, error) {
	asked, since, err := sinceOf(r.URL.Query(), qualityPeriod, db.At())
	if err != nil {
		return nil, err
	}
	p := &qualityPage{Gates: []store.GateStat{}, Evaluations: []store.EvaluationStat{}}
	if !since.IsZero() {
		p.Since = since.UTC().Format(time.RFC3339)
	}
	gates, err := db.GateStats(ctx, since)
	if err != nil {
		return nil, err
	}
	evaluations, err := db.EvaluationStats(ctx, since)
	if err != nil {
		return nil, err
	}

	repos := map[string]bool{}
	for _, g := range gates {
		p.Gates = append(p.Gates, g)
		repos[g.RepoPath] = true
		p.GateSum.Runs += g.Runs
		p.GateSum.Failures += g.Failures
		p.GateSum.TotalMS += g.TotalMS
	}
	p.GateSum.Repos, p.GateSum.Gates = len(repos), len(gates)
	for _, e := range evaluations {
		p.Evaluations = append(p.Evaluations, e)
		p.EvalSum.Evaluations += e.Evaluations
		p.EvalSum.Passed += e.Passed
		p.EvalSum.Cached += e.Cached
		p.EvalSum.CostUSD += e.CostUSD
	}
	p.EvalSum.Gates = len(evaluations)

	for _, period := range qualityPeriods {
		value := period[0]
		if value == qualityPeriod {
			// The period of the page is its address with no period in it.
			value = ""
		}
		p.Periods = append(p.Periods, stateTab{State: period[0], Label: period[1],
			Href: hrefWith(r, "since", value), Active: period[0] == asked})
	}
	return p, nil
}
