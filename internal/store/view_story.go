package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// Kinds of episode in the story of a change (the kinds of v_change_episodes).
const (
	EpisodeRun        = "run"        // a worker run, with what it carried out
	EpisodeMerge      = "merge"      // an attempt of the merger
	EpisodeManifest   = "manifest"   // the change was handed to the merger
	EpisodeBlock      = "block"      // the change was blocked
	EpisodeRebuild    = "rebuild"    // the merger asked for the change to be built again
	EpisodeApproval   = "approval"   // a person accepted the risk the change was blocked for
	EpisodeDeferral   = "deferral"   // no engine could work on the change, outside a run
	EpisodeGates      = "gates"      // gates run on the change outside a run and a merge attempt
	EpisodeEvaluation = "evaluation" // a question put to the evaluator outside a run and a merge attempt
	EpisodeExecution  = "execution"  // an execution outside a run: a prompt sent from the portal
)

// Episode is one thing that happened to a change. Kind names the one field that is set.
type Episode struct {
	Kind string `json:"kind"`
	// At is when the episode began.
	At string `json:"at"`

	Run        *RunEpisode      `json:"run,omitempty"`
	Merge      *MergeEpisode    `json:"merge,omitempty"`
	Manifest   *ManifestRecord  `json:"manifest,omitempty"`
	Block      *BlockRecord     `json:"block,omitempty"`
	Rebuild    *RebuildRequest  `json:"rebuild,omitempty"`
	Approval   *RiskApproval    `json:"approval,omitempty"`
	Deferral   *Deferral        `json:"deferral,omitempty"`
	Gates      *GateRoundDetail `json:"gates,omitempty"`
	Evaluation *Evaluation      `json:"evaluation,omitempty"`
	Execution  *ExecutionDetail `json:"execution,omitempty"`
}

// RunEpisode is a worker run with everything that happened inside it, each list in the order it
// happened.
type RunEpisode struct {
	ChangeRun
	Executions  []ExecutionDetail   `json:"executions"`
	GateRounds  []GateRoundDetail   `json:"gate_rounds"`
	Repairs     []RepairRoundRecord `json:"repairs"`
	Evaluations []Evaluation        `json:"evaluations"`
	Deferrals   []Deferral          `json:"deferrals"`
}

// MergeEpisode is a merge attempt with the gates it ran, the questions it put to the evaluator and
// the repositories it pushed.
type MergeEpisode struct {
	MergeAttempt
	GateRounds  []GateRoundDetail `json:"gate_rounds"`
	Evaluations []Evaluation      `json:"evaluations"`
	Pushes      []ManifestPush    `json:"pushes"`
}

// ExecutionDetail is an execution with its attempts. In a list its PromptText holds only the
// beginning of the prompt.
type ExecutionDetail struct {
	Execution
	Attempts []AttemptDetail `json:"attempts"`
	// OwnerGone says the execution is recorded as running and the process that ran it, on this host,
	// is dead: it ended without recording how.
	OwnerGone bool `json:"owner_gone,omitempty"`
}

// executionDetail returns e as a list or a story shows it, before its attempts are read.
func (v *View) executionDetail(e Execution) *ExecutionDetail {
	return &ExecutionDetail{
		Execution: e,
		Attempts:  []AttemptDetail{},
		OwnerGone: e.Status == "running" && e.PID > 0 && (e.Host == "" || e.Host == v.s.host) && !IsPIDAlive(e.PID),
	}
}

// AttemptDetail is one try of one engine with the stretches in which it showed no activity.
type AttemptDetail struct {
	ExecutionAttempt
	IdlePeriods []IdlePeriod `json:"idle_periods"`
}

// GateRoundDetail is a round of gates with the result of each gate.
type GateRoundDetail struct {
	GateRound
	Runs []GateRunRecord `json:"runs"`
}

// ManifestRecord is a change handed to the merger: a row of ready_manifests, open until the merger
// integrates the change, sends it back or blocks it.
type ManifestRecord struct {
	ID               int64    `json:"id"`
	ChangeName       string   `json:"change_name"`
	ChangeRunID      int64    `json:"change_run_id,omitempty"`
	Branch           string   `json:"branch"`
	Worktree         string   `json:"worktree"`
	HeadSHA          string   `json:"head_sha"`
	RiskEvaluationID int64    `json:"risk_evaluation_id,omitempty"`
	Capabilities     []string `json:"capabilities"`
	EnqueuedAt       string   `json:"enqueued_at"`
	ClosedAt         string   `json:"closed_at,omitempty"`
	Outcome          string   `json:"outcome,omitempty"`
}

// BlockRecord is a block of a change: a row of change_blocks, open until something resolves it.
type BlockRecord struct {
	ID             int64  `json:"id"`
	ChangeName     string `json:"change_name"`
	ChangeRunID    int64  `json:"change_run_id,omitempty"`
	MergeAttemptID int64  `json:"merge_attempt_id,omitempty"`
	Reason         string `json:"reason"`
	Details        string `json:"details,omitempty"`
	Stage          string `json:"stage,omitempty"`
	Worktree       string `json:"worktree,omitempty"`
	Branch         string `json:"branch,omitempty"`
	Host           string `json:"host,omitempty"`
	// RetryNum is how many times the change had been retried by itself when it was blocked.
	RetryNum   int    `json:"retry_num"`
	BlockedAt  string `json:"blocked_at"`
	ResolvedAt string `json:"resolved_at,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	ResolvedBy string `json:"resolved_by,omitempty"`
}

// RebuildRequest is the merger asking for a change to be built again on the current base.
type RebuildRequest struct {
	ID              int64  `json:"id"`
	ChangeName      string `json:"change_name"`
	MergeAttemptID  int64  `json:"merge_attempt_id,omitempty"`
	Branch          string `json:"branch"`
	Reason          string `json:"reason,omitempty"`
	ConflictDetails string `json:"conflict_details,omitempty"`
	RebuildNum      int    `json:"rebuild_num"`
	RequestedAt     string `json:"requested_at"`
	// ConsumedAt is when a worker took the request, "" while none has.
	ConsumedAt string `json:"consumed_at,omitempty"`
}

// RiskApproval is a person accepting the risk a change was blocked for.
type RiskApproval struct {
	ID           int64  `json:"id"`
	ChangeName   string `json:"change_name"`
	BlockID      int64  `json:"block_id,omitempty"`
	ApprovedBy   string `json:"approved_by"`
	ApprovedAt   string `json:"approved_at"`
	RevokedAt    string `json:"revoked_at,omitempty"`
	RevokeReason string `json:"revoke_reason,omitempty"`
}

// Deferral is a change put off because every engine of a role was out.
type Deferral struct {
	ID          int64  `json:"id"`
	ChangeName  string `json:"change_name"`
	ChangeRunID int64  `json:"change_run_id,omitempty"`
	Role        string `json:"role"`
	// Counted is false for a deferral inside the cooldown of one already counted.
	Counted    bool   `json:"counted"`
	RetryAt    string `json:"retry_at,omitempty"`
	OutputTail string `json:"output_tail,omitempty"`
	DeferredAt string `json:"deferred_at"`
	ClearedAt  string `json:"cleared_at,omitempty"`
}

// ManifestPush is a child repository pushed for a manifest.
type ManifestPush struct {
	ManifestID     int64  `json:"manifest_id"`
	RepoPath       string `json:"repo_path"`
	PushedSHA      string `json:"pushed_sha"`
	MergeAttemptID int64  `json:"merge_attempt_id"`
	PushedAt       string `json:"pushed_at"`
}

// StoryPage is one page of the story of a change, the latest episode first.
type StoryPage struct {
	Episodes []Episode `json:"items"`
	// Next is the cursor of the page that follows, "" when this one is the last.
	Next string `json:"next,omitempty"`
}

// executionListColumns is executionColumns with the prompt cut short: a list says what an execution
// was asked in a line, and only the execution's own page shows all of it.
var executionListColumns = strings.Replace(executionColumns, "e.prompt_text", "substr(e.prompt_text, 1, 400)", 1)

// inChunk is how many keys a statement names at once: well under the limit of bound parameters.
const inChunk = 400

// chunked calls fn with keys in groups of at most inChunk.
func chunked(keys []any, fn func(part []any) error) error {
	for len(keys) > 0 {
		n := min(len(keys), inChunk)
		if err := fn(keys[:n]); err != nil {
			return err
		}
		keys = keys[n:]
	}
	return nil
}

type episodeKey struct {
	kind string
	id   int64
	at   int64
}

func episodeCursor(cursor string) (at int64, kind string, id int64, err error) {
	at, rest, err := timeCursor(cursor)
	if err != nil || rest == "" {
		return at, "", 0, err
	}
	kind, rawID, ok := strings.Cut(rest, ".")
	id, perr := strconv.ParseInt(rawID, 10, 64)
	if !ok || perr != nil || kind == "" {
		return 0, "", 0, ErrBadCursor
	}
	return at, kind, id, nil
}

// ChangeStory returns one page of the story of a change, the latest episode first: what happened to
// the change, read from the rows of each fact and not from events. before is the cursor a page gave
// as Next ("" for the first page); limit, how many episodes the page holds. Only the episodes of the
// page are loaded with what they carry, so a change with thousands of executions does not load them
// all.
func (v *View) ChangeStory(ctx context.Context, change, before string, limit int) (StoryPage, error) {
	var page StoryPage
	at, kind, id, err := episodeCursor(before)
	if err != nil {
		return page, err
	}
	limit = pageSize(limit)

	var keys []episodeKey
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var k episodeKey
		if err := scan(&k.kind, &k.id, &k.at); err != nil {
			return err
		}
		keys = append(keys, k)
		return nil
	}, `
		SELECT kind, episode_id, at FROM v_change_episodes
		WHERE change_name = ? AND (at, kind, episode_id) < (?, ?, ?)
		ORDER BY at DESC, kind DESC, episode_id DESC
		LIMIT ?
	`, change, at, kind, id, limit+1)
	if err != nil {
		return page, err
	}
	if len(keys) > limit {
		keys = keys[:limit]
		last := keys[limit-1]
		page.Next = formatTimeCursor(last.at, last.kind+"."+strconv.FormatInt(last.id, 10))
	}

	ids := map[string][]any{}
	for _, k := range keys {
		ids[k.kind] = append(ids[k.kind], k.id)
	}
	l := &storyLoader{v: v, ctx: ctx, change: change}
	if err := l.load(ids); err != nil {
		return page, err
	}
	page.Episodes = make([]Episode, 0, len(keys))
	for _, k := range keys {
		if ep, ok := l.episode(k); ok {
			page.Episodes = append(page.Episodes, ep)
		}
	}
	return page, nil
}

// storyLoader reads what the episodes of one page carry, table by table.
type storyLoader struct {
	v      *View
	ctx    context.Context
	change string

	runs       map[int64]*RunEpisode
	merges     map[int64]*MergeEpisode
	manifests  map[int64]*ManifestRecord
	blocks     map[int64]*BlockRecord
	rebuilds   map[int64]*RebuildRequest
	approvals  map[int64]*RiskApproval
	deferrals  map[int64]*Deferral
	rounds     map[int64]*GateRoundDetail
	evals      map[int64]*Evaluation
	executions map[int64]*ExecutionDetail // by rowid
}

func (l *storyLoader) episode(k episodeKey) (Episode, bool) {
	ep := Episode{Kind: k.kind, At: msToRFC3339(k.at)}
	ok := false
	switch k.kind {
	case EpisodeRun:
		ep.Run, ok = l.runs[k.id]
	case EpisodeMerge:
		ep.Merge, ok = l.merges[k.id]
	case EpisodeManifest:
		ep.Manifest, ok = l.manifests[k.id]
	case EpisodeBlock:
		ep.Block, ok = l.blocks[k.id]
	case EpisodeRebuild:
		ep.Rebuild, ok = l.rebuilds[k.id]
	case EpisodeApproval:
		ep.Approval, ok = l.approvals[k.id]
	case EpisodeDeferral:
		ep.Deferral, ok = l.deferrals[k.id]
	case EpisodeGates:
		ep.Gates, ok = l.rounds[k.id]
	case EpisodeEvaluation:
		ep.Evaluation, ok = l.evals[k.id]
	case EpisodeExecution:
		ep.Execution, ok = l.executions[k.id]
	}
	return ep, ok
}

func (l *storyLoader) load(ids map[string][]any) error {
	steps := []func(map[string][]any) error{
		l.loadRuns, l.loadMerges, l.loadManifests, l.loadBlocks, l.loadRebuilds, l.loadApprovals,
		l.loadDeferrals, l.loadEvaluations, l.loadGateRounds, l.loadExecutions,
	}
	for _, step := range steps {
		if err := step(ids); err != nil {
			return err
		}
	}
	return nil
}

func (l *storyLoader) loadRuns(ids map[string][]any) error {
	l.runs = map[int64]*RunEpisode{}
	return chunked(ids[EpisodeRun], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			r, err := scanChangeRun(scan)
			if err == nil {
				l.runs[r.ID] = &RunEpisode{
					ChangeRun: r, Executions: []ExecutionDetail{}, GateRounds: []GateRoundDetail{},
					Repairs: []RepairRoundRecord{}, Evaluations: []Evaluation{}, Deferrals: []Deferral{},
				}
			}
			return err
		}, `SELECT `+changeRunColumns+` FROM change_runs r WHERE r.id IN (`+placeholders(len(part))+`)`, part...)
	})
}

func (l *storyLoader) loadMerges(ids map[string][]any) error {
	l.merges = map[int64]*MergeEpisode{}
	var manifestIDs []any
	seen := map[int64]bool{}
	err := chunked(ids[EpisodeMerge], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			a, err := scanMergeAttempt(scan)
			if err != nil {
				return err
			}
			l.merges[a.ID] = &MergeEpisode{MergeAttempt: a, GateRounds: []GateRoundDetail{}, Evaluations: []Evaluation{}, Pushes: []ManifestPush{}}
			if !seen[a.ManifestID] {
				seen[a.ManifestID] = true
				manifestIDs = append(manifestIDs, a.ManifestID)
			}
			return nil
		}, `SELECT `+mergeAttemptColumns+` FROM merge_attempts m JOIN ready_manifests r ON r.id = m.manifest_id
		    WHERE m.id IN (`+placeholders(len(part))+`)`, part...)
	})
	if err != nil {
		return err
	}
	// The pushes of those attempts, found through their manifests.
	return chunked(manifestIDs, func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			var p ManifestPush
			var at int64
			if err := scan(&p.ManifestID, &p.RepoPath, &p.PushedSHA, &p.MergeAttemptID, &at); err != nil {
				return err
			}
			p.PushedAt = msToRFC3339(at)
			if m := l.merges[p.MergeAttemptID]; m != nil {
				m.Pushes = append(m.Pushes, p)
			}
			return nil
		}, `SELECT manifest_id, repo_path, pushed_sha, merge_attempt_id, pushed_at FROM manifest_pushes
		    WHERE manifest_id IN (`+placeholders(len(part))+`) ORDER BY pushed_at, repo_path`, part...)
	})
}

const manifestRecordColumns = `id, change_name, change_run_id, branch, worktree, head_sha, risk_evaluation_id, enqueued_at, closed_at, outcome`

func scanManifestRecord(scan func(dest ...any) error) (ManifestRecord, error) {
	var m ManifestRecord
	var runID, evalID, closed sql.NullInt64
	var outcome sql.NullString
	var enqueued int64
	err := scan(&m.ID, &m.ChangeName, &runID, &m.Branch, &m.Worktree, &m.HeadSHA, &evalID, &enqueued, &closed, &outcome)
	m.ChangeRunID, m.RiskEvaluationID = runID.Int64, evalID.Int64
	m.EnqueuedAt = msToRFC3339(enqueued)
	m.ClosedAt = nullMSToRFC3339(closed)
	m.Outcome = outcome.String
	m.Capabilities = []string{}
	return m, err
}

// loadManifestRecords reads the manifests whose ids are given, with the capabilities each claims.
func (v *View) loadManifestRecords(ctx context.Context, ids []any) (map[int64]*ManifestRecord, error) {
	out := map[int64]*ManifestRecord{}
	err := chunked(ids, func(part []any) error {
		if err := v.each(ctx, func(scan func(dest ...any) error) error {
			m, err := scanManifestRecord(scan)
			if err == nil {
				out[m.ID] = &m
			}
			return err
		}, `SELECT `+manifestRecordColumns+` FROM ready_manifests WHERE id IN (`+placeholders(len(part))+`)`, part...); err != nil {
			return err
		}
		return v.each(ctx, func(scan func(dest ...any) error) error {
			var id int64
			var capability string
			if err := scan(&id, &capability); err != nil {
				return err
			}
			if m := out[id]; m != nil {
				m.Capabilities = append(m.Capabilities, capability)
			}
			return nil
		}, `SELECT manifest_id, capability FROM manifest_capabilities
		    WHERE manifest_id IN (`+placeholders(len(part))+`) ORDER BY manifest_id, capability`, part...)
	})
	return out, err
}

func (l *storyLoader) loadManifests(ids map[string][]any) error {
	var err error
	l.manifests, err = l.v.loadManifestRecords(l.ctx, ids[EpisodeManifest])
	return err
}

const blockRecordColumns = `id, change_name, change_run_id, merge_attempt_id, reason, details, stage, worktree, branch, host,
	retry_num, blocked_at, resolved_at, resolution, resolved_by`

func scanBlockRecord(scan func(dest ...any) error) (BlockRecord, error) {
	var b BlockRecord
	var runID, mergeID, resolved sql.NullInt64
	var details, stage, worktree, branch, host, resolution, by sql.NullString
	var blocked int64
	err := scan(&b.ID, &b.ChangeName, &runID, &mergeID, &b.Reason, &details, &stage, &worktree, &branch, &host,
		&b.RetryNum, &blocked, &resolved, &resolution, &by)
	b.ChangeRunID, b.MergeAttemptID = runID.Int64, mergeID.Int64
	b.Details, b.Stage, b.Worktree, b.Branch, b.Host = details.String, stage.String, worktree.String, branch.String, host.String
	b.BlockedAt = msToRFC3339(blocked)
	b.ResolvedAt = nullMSToRFC3339(resolved)
	b.Resolution, b.ResolvedBy = resolution.String, by.String
	return b, err
}

func (l *storyLoader) loadBlocks(ids map[string][]any) error {
	l.blocks = map[int64]*BlockRecord{}
	return chunked(ids[EpisodeBlock], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			b, err := scanBlockRecord(scan)
			if err == nil {
				l.blocks[b.ID] = &b
			}
			return err
		}, `SELECT `+blockRecordColumns+` FROM change_blocks WHERE id IN (`+placeholders(len(part))+`)`, part...)
	})
}

const rebuildRequestColumns = `id, change_name, merge_attempt_id, branch, reason, conflict_details, rebuild_num, requested_at, consumed_at`

func scanRebuildRequest(scan func(dest ...any) error) (RebuildRequest, error) {
	var r RebuildRequest
	var mergeID, consumed sql.NullInt64
	var reason, details sql.NullString
	var requested int64
	if err := scan(&r.ID, &r.ChangeName, &mergeID, &r.Branch, &reason, &details, &r.RebuildNum, &requested, &consumed); err != nil {
		return r, err
	}
	r.MergeAttemptID = mergeID.Int64
	r.Reason, r.ConflictDetails = reason.String, details.String
	r.RequestedAt = msToRFC3339(requested)
	r.ConsumedAt = nullMSToRFC3339(consumed)
	return r, nil
}

func (l *storyLoader) loadRebuilds(ids map[string][]any) error {
	l.rebuilds = map[int64]*RebuildRequest{}
	return chunked(ids[EpisodeRebuild], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			r, err := scanRebuildRequest(scan)
			if err == nil {
				l.rebuilds[r.ID] = &r
			}
			return err
		}, `SELECT `+rebuildRequestColumns+` FROM rebuild_requests WHERE id IN (`+placeholders(len(part))+`)`, part...)
	})
}

func (l *storyLoader) loadApprovals(ids map[string][]any) error {
	l.approvals = map[int64]*RiskApproval{}
	return chunked(ids[EpisodeApproval], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			var a RiskApproval
			var blockID, revoked sql.NullInt64
			var reason sql.NullString
			var approved int64
			if err := scan(&a.ID, &a.ChangeName, &blockID, &a.ApprovedBy, &approved, &revoked, &reason); err != nil {
				return err
			}
			a.BlockID = blockID.Int64
			a.ApprovedAt = msToRFC3339(approved)
			a.RevokedAt = nullMSToRFC3339(revoked)
			a.RevokeReason = reason.String
			l.approvals[a.ID] = &a
			return nil
		}, `SELECT id, change_name, block_id, approved_by, approved_at, revoked_at, revoke_reason
		    FROM risk_approvals WHERE id IN (`+placeholders(len(part))+`)`, part...)
	})
}

const deferralColumns = `d.id, d.change_name, d.change_run_id, d.role, d.counted, d.retry_at, d.output_tail, d.deferred_at, d.cleared_at`

func scanDeferral(scan func(dest ...any) error) (Deferral, error) {
	var d Deferral
	var runID, retry, cleared sql.NullInt64
	var tail sql.NullString
	var counted int
	var deferred int64
	err := scan(&d.ID, &d.ChangeName, &runID, &d.Role, &counted, &retry, &tail, &deferred, &cleared)
	d.ChangeRunID = runID.Int64
	d.Counted = counted != 0
	d.RetryAt = nullMSToRFC3339(retry)
	d.OutputTail = tail.String
	d.DeferredAt = msToRFC3339(deferred)
	d.ClearedAt = nullMSToRFC3339(cleared)
	return d, err
}

// loadDeferrals reads the deferrals that are episodes of their own and those of the runs of the page.
func (l *storyLoader) loadDeferrals(ids map[string][]any) error {
	l.deferrals = map[int64]*Deferral{}
	err := chunked(ids[EpisodeDeferral], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			d, err := scanDeferral(scan)
			if err == nil {
				l.deferrals[d.ID] = &d
			}
			return err
		}, `SELECT `+deferralColumns+` FROM change_deferrals d WHERE d.id IN (`+placeholders(len(part))+`)`, part...)
	})
	if err != nil {
		return err
	}
	return chunked(ids[EpisodeRun], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			d, err := scanDeferral(scan)
			if err != nil {
				return err
			}
			if r := l.runs[d.ChangeRunID]; r != nil {
				r.Deferrals = append(r.Deferrals, d)
			}
			return nil
		}, `SELECT `+deferralColumns+` FROM change_deferrals d
		    WHERE d.change_name = ? AND d.change_run_id IN (`+placeholders(len(part))+`) ORDER BY d.id`,
			append([]any{l.change}, part...)...)
	})
}

// loadEvaluations reads the evaluations that are episodes of their own, those of the runs and those
// of the merge attempts of the page.
func (l *storyLoader) loadEvaluations(ids map[string][]any) error {
	l.evals = map[int64]*Evaluation{}
	err := chunked(ids[EpisodeEvaluation], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			ev, err := scanEvaluation(scan)
			if err == nil {
				l.evals[ev.ID] = &ev
			}
			return err
		}, `SELECT `+evaluationColumns+` FROM evaluations v WHERE v.id IN (`+placeholders(len(part))+`)`, part...)
	})
	if err != nil {
		return err
	}
	err = chunked(ids[EpisodeRun], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			ev, err := scanEvaluation(scan)
			if err != nil {
				return err
			}
			if r := l.runs[ev.ChangeRunID]; r != nil {
				r.Evaluations = append(r.Evaluations, ev)
			}
			return nil
		}, `SELECT `+evaluationColumns+` FROM evaluations v
		    WHERE v.change_run_id IN (`+placeholders(len(part))+`) ORDER BY v.id`, part...)
	})
	if err != nil {
		return err
	}
	return chunked(ids[EpisodeMerge], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			ev, err := scanEvaluation(scan)
			if err != nil {
				return err
			}
			if m := l.merges[ev.MergeAttemptID]; m != nil {
				m.Evaluations = append(m.Evaluations, ev)
			}
			return nil
		}, `SELECT `+evaluationColumns+` FROM evaluations v
		    WHERE v.change_name = ? AND v.merge_attempt_id IN (`+placeholders(len(part))+`) ORDER BY v.id`,
			append([]any{l.change}, part...)...)
	})
}

// loadGateRounds reads the gate rounds that are episodes of their own, those of the runs and those
// of the merge attempts of the page, each with the result of its gates.
func (l *storyLoader) loadGateRounds(ids map[string][]any) error {
	l.rounds = map[int64]*GateRoundDetail{}
	var order []*GateRoundDetail
	read := func(where string, keys []any) error {
		return chunked(keys, func(part []any) error {
			return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
				r, err := scanGateRound(scan)
				if err != nil {
					return err
				}
				if l.rounds[r.ID] == nil {
					d := &GateRoundDetail{GateRound: r, Runs: []GateRunRecord{}}
					l.rounds[r.ID] = d
					order = append(order, d)
				}
				return nil
			}, `SELECT `+gateRoundColumns+` FROM gate_rounds r WHERE `+where+` IN (`+placeholders(len(part))+`) ORDER BY r.id`, part...)
		})
	}
	if err := read(`r.id`, ids[EpisodeGates]); err != nil {
		return err
	}
	if err := read(`r.change_run_id`, ids[EpisodeRun]); err != nil {
		return err
	}
	if err := read(`r.merge_attempt_id`, ids[EpisodeMerge]); err != nil {
		return err
	}

	roundIDs := make([]any, 0, len(order))
	for _, d := range order {
		roundIDs = append(roundIDs, d.ID)
	}
	if err := chunked(roundIDs, func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			g, err := scanGateRun(scan)
			if err != nil {
				return err
			}
			if d := l.rounds[g.GateRoundID]; d != nil {
				d.Runs = append(d.Runs, g)
			}
			return nil
		}, `SELECT `+gateRunColumns+gateRunTables+` WHERE g.gate_round_id IN (`+placeholders(len(part))+`) ORDER BY g.id`, part...)
	}); err != nil {
		return err
	}
	// The rounds were read in the order they ran; hand each to the run or the merge attempt it is of.
	for _, d := range order {
		if r := l.runs[d.ChangeRunID]; r != nil && d.ChangeRunID != 0 {
			r.GateRounds = append(r.GateRounds, *d)
		}
		if m := l.merges[d.MergeAttemptID]; m != nil && d.MergeAttemptID != 0 {
			m.GateRounds = append(m.GateRounds, *d)
		}
	}
	return nil
}

// loadExecutions reads the executions that are episodes of their own and those of the runs of the
// page, each with its attempts and their idle periods, and the repairs of those runs.
func (l *storyLoader) loadExecutions(ids map[string][]any) error {
	l.executions = map[int64]*ExecutionDetail{}
	var all []*ExecutionDetail
	byRun := map[int64][]*ExecutionDetail{}
	read := func(where, order string, keys []any) error {
		return chunked(keys, func(part []any) error {
			return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
				var rowid int64
				e, err := scanExecution(func(dest ...any) error { return scan(append(dest, &rowid)...) })
				if err != nil {
					return err
				}
				d := l.v.executionDetail(e)
				all = append(all, d)
				if e.ChangeRunID != 0 {
					byRun[e.ChangeRunID] = append(byRun[e.ChangeRunID], d)
				} else {
					l.executions[rowid] = d
				}
				return nil
			}, `SELECT `+executionListColumns+`, e.rowid FROM executions e WHERE `+where+` IN (`+placeholders(len(part))+`)`+order, part...)
		})
	}
	if err := read(`e.rowid`, ``, ids[EpisodeExecution]); err != nil {
		return err
	}
	if err := read(`e.change_run_id`, ` ORDER BY e.started_at, e.rowid`, ids[EpisodeRun]); err != nil {
		return err
	}
	byExec := make(map[string]*ExecutionDetail, len(all))
	execIDs := make([]any, 0, len(all))
	for _, d := range all {
		byExec[d.ExecID] = d
		execIDs = append(execIDs, d.ExecID)
	}
	if err := l.v.loadAttempts(l.ctx, execIDs, byExec); err != nil {
		return err
	}
	for runID, list := range byRun {
		if r := l.runs[runID]; r != nil {
			for _, d := range list {
				r.Executions = append(r.Executions, *d)
			}
		}
	}

	return chunked(ids[EpisodeRun], func(part []any) error {
		return l.v.each(l.ctx, func(scan func(dest ...any) error) error {
			rr, err := scanRepairRound(scan)
			if err != nil {
				return err
			}
			if r := l.runs[rr.ChangeRunID]; r != nil {
				r.Repairs = append(r.Repairs, rr)
			}
			return nil
		}, `SELECT `+repairRoundColumns+repairRoundTables+` WHERE rr.change_run_id IN (`+placeholders(len(part))+`) ORDER BY rr.id`, part...)
	})
}

// loadAttempts fills the attempts, with their idle periods, of the executions in byExec.
func (v *View) loadAttempts(ctx context.Context, execIDs []any, byExec map[string]*ExecutionDetail) error {
	type slot struct {
		exec *ExecutionDetail
		at   int
	}
	byAttempt := map[int64]slot{}
	var attemptIDs []any
	err := chunked(execIDs, func(part []any) error {
		return v.each(ctx, func(scan func(dest ...any) error) error {
			a, err := scanAttempt(scan)
			if err != nil {
				return err
			}
			if d := byExec[a.ExecID]; d != nil {
				d.Attempts = append(d.Attempts, AttemptDetail{ExecutionAttempt: a, IdlePeriods: []IdlePeriod{}})
				byAttempt[a.ID] = slot{exec: d, at: len(d.Attempts) - 1}
				attemptIDs = append(attemptIDs, a.ID)
			}
			return nil
		}, `SELECT `+attemptColumns+` FROM execution_attempts a
		    WHERE a.exec_id IN (`+placeholders(len(part))+`) ORDER BY a.exec_id, a.attempt_no`, part...)
	})
	if err != nil {
		return err
	}
	return chunked(attemptIDs, func(part []any) error {
		return v.each(ctx, func(scan func(dest ...any) error) error {
			p, err := scanIdlePeriod(scan, v.now)
			if err != nil {
				return err
			}
			if s, ok := byAttempt[p.AttemptID]; ok {
				s.exec.Attempts[s.at].IdlePeriods = append(s.exec.Attempts[s.at].IdlePeriods, p)
			}
			return nil
		}, `SELECT `+idlePeriodColumns+`
		    FROM attempt_idle_periods p JOIN execution_attempts a ON a.id = p.attempt_id
		    WHERE p.attempt_id IN (`+placeholders(len(part))+`) ORDER BY p.id`, part...)
	})
}
