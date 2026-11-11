package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"time"
)

// QuarantineRecord is the quarantine of a change as the orchestrator reads it. It is derived: a
// block is a row of change_blocks from the moment it happens until something resolves it, a rebuild
// the merger asked for is a row of rebuild_requests until a worker consumes it, and a risk approval
// is a row of risk_approvals until it is revoked. Nothing here is overwritten; what the record says
// is what those rows say now.
type QuarantineRecord struct {
	ChangeName      string `json:"change_name"`
	Status          string `json:"status"` // "blocked", "rebuild_requested", "approved", "cleared"
	Reason          string `json:"reason,omitempty"`
	Details         string `json:"details,omitempty"`
	Stage           string `json:"stage,omitempty"`
	Worktree        string `json:"worktree,omitempty"`
	Branch          string `json:"branch,omitempty"`
	Host            string `json:"host,omitempty"`
	Retries         int    `json:"retries"`
	LastRetryJSON   string `json:"last_retry_json,omitempty"`
	RetryTold       bool   `json:"retry_told"`
	Rebuilds        int    `json:"rebuilds"`
	ConflictDetails string `json:"conflict_details,omitempty"`
	ApprovedRisk    bool   `json:"approved_risk"`
	BlockedAt       string `json:"blocked_at,omitempty"`
	UpdatedAt       string `json:"updated_at"`
}

// Resolutions of a block.
const (
	BlockAutoRetry  = "auto_retry" // the dispatcher retried the change by itself
	BlockCleared    = "cleared"    // lifted, the change's counters kept
	BlockUnblocked  = "unblocked"  // lifted by an operator, the counters started over
	BlockApproved   = "approved"   // a person accepted the risk it was blocked for
	BlockSuperseded = "superseded" // another block or a rebuild request took its place
	BlockIntegrated = "integrated" // the change was integrated
	BlockAbandoned  = "abandoned"  // the change was abandoned
)

// A resolution that starts the change's counters over: what happened before it no longer counts
// towards its retries or its rebuilds.
func forgets(resolution string) bool {
	return resolution == BlockUnblocked || resolution == BlockIntegrated || resolution == BlockAbandoned
}

type blockRow struct {
	id         int64
	reason     string
	details    string
	stage      string
	worktree   string
	branch     string
	host       string
	retryNum   int
	retryTold  bool
	blockedAt  int64
	resolvedAt int64 // 0 while open
	resolution string
}

type rebuildRow struct {
	id          int64
	branch      string
	details     string
	num         int
	requestedAt int64
	consumedAt  int64 // 0 while pending
}

// quarantineState is what counts of a change's quarantine now: the blocks and rebuild requests since
// its counters last started over, and its risk approval in force.
type quarantineState struct {
	change     string
	chain      []blockRow
	reqs       []rebuildRow
	approvalID int64
	approvedAt int64
}

func (q *quarantineState) open() *blockRow {
	for i := range q.chain {
		if q.chain[i].resolvedAt == 0 {
			return &q.chain[i]
		}
	}
	return nil
}

func (q *quarantineState) last() *blockRow {
	if len(q.chain) == 0 {
		return nil
	}
	return &q.chain[len(q.chain)-1]
}

// lastRetried is the latest block the dispatcher retried by itself: what the next agent is told.
func (q *quarantineState) lastRetried() *blockRow {
	for i := len(q.chain) - 1; i >= 0; i-- {
		if q.chain[i].resolution == BlockAutoRetry {
			return &q.chain[i]
		}
	}
	return nil
}

func (q *quarantineState) pendingRequest() *rebuildRow {
	for i := len(q.reqs) - 1; i >= 0; i-- {
		if q.reqs[i].consumedAt == 0 {
			return &q.reqs[i]
		}
	}
	return nil
}

func (q *quarantineState) latestRequest() *rebuildRow {
	if len(q.reqs) == 0 {
		return nil
	}
	return &q.reqs[len(q.reqs)-1]
}

// retries is how many times the change was retried by itself since its counters started over.
func (q *quarantineState) retries() int {
	if o := q.open(); o != nil {
		return o.retryNum
	}
	if l := q.last(); l != nil {
		if l.resolution == BlockAutoRetry {
			return l.retryNum + 1
		}
		return l.retryNum
	}
	return 0
}

// rebuilds is how many rebuilds the merger asked for since the counters started over, or since the
// change was retried after it ran out of them (a rebase_conflict block the dispatcher retried).
func (q *quarantineState) rebuilds() int {
	r := q.latestRequest()
	if r == nil {
		return 0
	}
	for _, b := range q.chain {
		if b.resolution == BlockAutoRetry && b.reason == "rebase_conflict" && b.resolvedAt > r.requestedAt {
			return 0
		}
	}
	return r.num
}

func (q *quarantineState) exists() bool {
	return len(q.chain) > 0 || len(q.reqs) > 0 || q.approvalID != 0
}

func (q *quarantineState) record() *QuarantineRecord {
	if !q.exists() {
		return nil
	}
	rec := &QuarantineRecord{
		ChangeName:   q.change,
		Retries:      q.retries(),
		Rebuilds:     q.rebuilds(),
		ApprovedRisk: q.approvalID != 0,
	}
	open, last, pending := q.open(), q.last(), q.pendingRequest()
	switch {
	case open != nil:
		rec.Status = "blocked"
	case pending != nil:
		rec.Status = "rebuild_requested"
	case rec.ApprovedRisk && (last == nil || last.resolution == BlockApproved):
		rec.Status = "approved"
	default:
		rec.Status = "cleared"
	}
	updated := q.approvedAt
	if src := last; src != nil {
		rec.Stage, rec.Worktree, rec.Branch, rec.Host = src.stage, src.worktree, src.branch, src.host
		rec.BlockedAt = msToRFC3339(src.blockedAt)
		if open != nil || src.resolution == BlockAutoRetry {
			rec.Reason, rec.Details = src.reason, src.details
		}
		updated = max(updated, src.blockedAt, src.resolvedAt)
	}
	if r := q.latestRequest(); r != nil {
		if pending != nil || rec.Rebuilds > 0 {
			rec.ConflictDetails = r.details
		}
		if rec.Branch == "" || pending != nil {
			rec.Branch = r.branch
		}
		updated = max(updated, r.requestedAt, r.consumedAt)
	}
	if lr := q.lastRetried(); lr != nil {
		rec.RetryTold = lr.retryTold
		snapshot := QuarantineRecord{
			ChangeName: q.change,
			Status:     "blocked",
			Reason:     lr.reason,
			Details:    lr.details,
			Stage:      lr.stage,
			Worktree:   lr.worktree,
			Branch:     lr.branch,
			Host:       lr.host,
			Retries:    lr.retryNum,
			BlockedAt:  msToRFC3339(lr.blockedAt),
			UpdatedAt:  msToRFC3339(lr.resolvedAt),
		}
		if b, err := json.Marshal(snapshot); err == nil {
			rec.LastRetryJSON = string(b)
		}
	}
	rec.UpdatedAt = msToRFC3339(updated)
	return rec
}

func (s *Store) loadQuarantineTx(ctx context.Context, q reader, change string) (*quarantineState, error) {
	st := &quarantineState{change: change}
	// Since when the change's counters run: the latest block that started them over, or its integration.
	var since int64
	if err := q.QueryRowContext(ctx, `
		SELECT MAX(
			COALESCE((SELECT MAX(resolved_at) FROM change_blocks
			          WHERE change_name = ?1 AND resolution IN ('unblocked','integrated','abandoned')), 0),
			COALESCE((SELECT MAX(closed_at) FROM ready_manifests
			          WHERE change_name = ?1 AND outcome IN ('integrated','already_integrated')), 0))
	`, change).Scan(&since); err != nil {
		return nil, err
	}

	rows, err := q.QueryContext(ctx, `
		SELECT id, reason, details, stage, worktree, branch, host, retry_num, retry_told, blocked_at, resolved_at, resolution
		FROM change_blocks
		WHERE change_name = ? AND (resolved_at IS NULL OR resolved_at > ?)
		ORDER BY id
	`, change, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b blockRow
		var details, stage, worktree, branch, host, resolution sql.NullString
		var resolvedAt sql.NullInt64
		var told int
		if err := rows.Scan(&b.id, &b.reason, &details, &stage, &worktree, &branch, &host, &b.retryNum, &told, &b.blockedAt, &resolvedAt, &resolution); err != nil {
			rows.Close()
			return nil, err
		}
		b.details, b.stage, b.worktree, b.branch, b.host = details.String, stage.String, worktree.String, branch.String, host.String
		b.retryTold = told != 0
		b.resolvedAt = resolvedAt.Int64
		b.resolution = resolution.String
		st.chain = append(st.chain, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.QueryContext(ctx, `
		SELECT id, branch, conflict_details, rebuild_num, requested_at, consumed_at
		FROM rebuild_requests
		WHERE change_name = ? AND requested_at > ?
		ORDER BY id
	`, change, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r rebuildRow
		var details sql.NullString
		var consumedAt sql.NullInt64
		if err := rows.Scan(&r.id, &r.branch, &details, &r.num, &r.requestedAt, &consumedAt); err != nil {
			rows.Close()
			return nil, err
		}
		r.details = details.String
		r.consumedAt = consumedAt.Int64
		st.reqs = append(st.reqs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var approvalID, approvedAt sql.NullInt64
	err = q.QueryRowContext(ctx, `
		SELECT id, approved_at FROM risk_approvals WHERE change_name = ? AND revoked_at IS NULL
	`, change).Scan(&approvalID, &approvedAt)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	st.approvalID, st.approvedAt = approvalID.Int64, approvedAt.Int64
	return st, nil
}

// GetQuarantine returns the quarantine of a change, or nil when it has none in force: no open
// block, no rebuild request and no approval since its counters last started over.
func (s *Store) GetQuarantine(ctx context.Context, change string) (*QuarantineRecord, error) {
	st, err := s.loadQuarantineTx(ctx, s.db, change)
	if err != nil {
		return nil, err
	}
	return st.record(), nil
}

// ListQuarantine lists the quarantine of every change that has one, filtered by optional status
// ("" = all), latest change first.
func (s *Store) ListQuarantine(ctx context.Context, status string) ([]QuarantineRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT change_name FROM change_blocks
		UNION SELECT change_name FROM rebuild_requests
		UNION SELECT change_name FROM risk_approvals WHERE revoked_at IS NULL
	`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []QuarantineRecord
	for _, name := range names {
		st, err := s.loadQuarantineTx(ctx, s.db, name)
		if err != nil {
			return nil, err
		}
		rec := st.record()
		if rec == nil || (status != "" && rec.Status != status) {
			continue
		}
		out = append(out, *rec)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ChangeName < out[j].ChangeName
	})
	return out, nil
}

func (s *Store) resolveOpenBlockTx(ctx context.Context, tx *sql.Tx, change, resolution, by string, now int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE change_blocks SET resolved_at = ?, resolution = ?, resolved_by = ?
		WHERE change_name = ? AND resolved_at IS NULL
	`, now, resolution, nullStr(by), change)
	return err
}

func (s *Store) consumeRequestsTx(ctx context.Context, tx *sql.Tx, change string, now int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE rebuild_requests SET consumed_at = ? WHERE change_name = ? AND consumed_at IS NULL
	`, now, change)
	return err
}

// BlockChange records that a change is blocked: it needs a retry or a person before it runs again.
// A block the change already had open is closed as superseded, and a rebuild the merger had asked
// for is spent: the block is what the change waits on now. The worker run or merge attempt in the
// scope of ctx is recorded as where it happened.
func (s *Store) BlockChange(ctx context.Context, rec QuarantineRecord) error {
	sc := ScopeFrom(ctx)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		if err := s.ensureChangeTx(ctx, tx, rec.ChangeName, now); err != nil {
			return err
		}
		st, err := s.loadQuarantineTx(ctx, tx, rec.ChangeName)
		if err != nil {
			return err
		}
		retryNum := st.retries()
		if !st.exists() && rec.Retries > 0 {
			// A change met for the first time with a count of its own (a quarantine imported from files).
			retryNum = rec.Retries
		}
		if prev := st.last(); prev != nil {
			if rec.Worktree == "" {
				rec.Worktree = prev.worktree
			}
			if rec.Branch == "" {
				rec.Branch = prev.branch
			}
		}
		if err := s.resolveOpenBlockTx(ctx, tx, rec.ChangeName, BlockSuperseded, "", now); err != nil {
			return err
		}
		if err := s.consumeRequestsTx(ctx, tx, rec.ChangeName, now); err != nil {
			return err
		}
		if rec.Reason == "" {
			rec.Reason = "unknown"
		}
		host := rec.Host
		if host == "" {
			host = s.host
		}
		blockedAt := rfc3339ToMS(rec.BlockedAt)
		if blockedAt == 0 {
			blockedAt = now
		}
		runID := sc.runFor(rec.ChangeName)
		var mergeID int64
		if runID == 0 {
			mergeID = sc.mergeFor(rec.ChangeName)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO change_blocks (change_name, change_run_id, merge_attempt_id, reason, details, stage,
			                           worktree, branch, host, retry_num, blocked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, rec.ChangeName, nullInt(runID), nullInt(mergeID), rec.Reason, nullStr(rec.Details), nullStr(rec.Stage),
			nullStr(rec.Worktree), nullStr(rec.Branch), nullStr(host), retryNum, blockedAt)
		if err != nil {
			return err
		}
		if rec.ApprovedRisk && st.approvalID == 0 && !st.exists() {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO risk_approvals (change_name, approved_by, approved_at) VALUES (?, 'operator', ?)
			`, rec.ChangeName, now)
		}
		return err
	})
}

// RequestRebuild records that the merger sent a change back to be rebuilt on the current base, and
// returns how many rebuilds it has asked for since the change's counters started over. A block the
// change had open and an earlier request still pending are closed: the new request stands for them.
func (s *Store) RequestRebuild(ctx context.Context, change, branch, conflictDetails string) (int, error) {
	return s.RequestRebuildFor(ctx, change, branch, "", conflictDetails)
}

// RequestRebuildFor is RequestRebuild with the reason the merger sent the change back for.
func (s *Store) RequestRebuildFor(ctx context.Context, change, branch, reason, conflictDetails string) (int, error) {
	sc := ScopeFrom(ctx)
	n := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		if err := s.ensureChangeTx(ctx, tx, change, now); err != nil {
			return err
		}
		st, err := s.loadQuarantineTx(ctx, tx, change)
		if err != nil {
			return err
		}
		n = st.rebuilds() + 1
		if branch == "" {
			if prev := st.last(); prev != nil {
				branch = prev.branch
			}
		}
		if err := s.resolveOpenBlockTx(ctx, tx, change, BlockSuperseded, "", now); err != nil {
			return err
		}
		if err := s.consumeRequestsTx(ctx, tx, change, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO rebuild_requests (change_name, merge_attempt_id, branch, reason, conflict_details, rebuild_num, requested_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, change, nullInt(sc.mergeFor(change)), branch, nullStr(reason), nullStr(conflictDetails), n, s.nowMS())
		return err
	})
	return n, err
}

// ApproveChangeRisk records that an operator accepts the risk a change was held for.
func (s *Store) ApproveChangeRisk(ctx context.Context, change string) error {
	return s.ApproveChangeRiskBy(ctx, change, "operator")
}

// ApproveChangeRiskBy records that `by` accepts the risk of a change. A block it had open for a
// reason a person can accept (risk_review, concurrent_work_loss, semantic_conflict) is resolved as
// approved; any other block stays, and the approval stands for when it is lifted.
func (s *Store) ApproveChangeRiskBy(ctx context.Context, change, by string) error {
	if by == "" {
		by = "operator"
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		if err := s.ensureChangeTx(ctx, tx, change, now); err != nil {
			return err
		}
		st, err := s.loadQuarantineTx(ctx, tx, change)
		if err != nil {
			return err
		}
		open := st.open()
		if st.approvalID == 0 {
			var blockID int64
			if open != nil {
				blockID = open.id
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO risk_approvals (change_name, block_id, approved_by, approved_at) VALUES (?, ?, ?, ?)
			`, change, nullInt(blockID), by, now); err != nil {
				return err
			}
		}
		if open != nil && RiskBlock(open.reason) {
			return s.resolveOpenBlockTx(ctx, tx, change, BlockApproved, by, s.nowMS())
		}
		return nil
	})
}

// RiskBlock reports whether a block for reason is one a person lifts by accepting the risk of the
// change: the review of its risk, a loss of concurrent work, a semantic conflict.
func RiskBlock(reason string) bool {
	switch reason {
	case "risk_review", "concurrent_work_loss", "semantic_conflict":
		return true
	}
	return false
}

// RevokeRiskApproval withdraws the risk approval of a change, for reason (for example
// "head_updated": commits nobody reviewed were added to it).
func (s *Store) RevokeRiskApproval(ctx context.Context, change, reason string) error {
	if reason == "" {
		reason = "revoked"
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE risk_approvals SET revoked_at = ?, revoke_reason = ? WHERE change_name = ? AND revoked_at IS NULL
	`, s.nowMS(), reason, change)
	return err
}

// AdvanceQuarantineRetry resolves the open block of a change as retried by the dispatcher itself,
// which counts one retry, and returns the change's quarantine after it. The block stays as the
// record of what the next agent is told (see MarkRetryTold).
func (s *Store) AdvanceQuarantineRetry(ctx context.Context, change string) (*QuarantineRecord, error) {
	var out *QuarantineRecord
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		st, err := s.loadQuarantineTx(ctx, tx, change)
		if err != nil {
			return err
		}
		if st.open() != nil {
			if _, err := tx.ExecContext(ctx, `
				UPDATE change_blocks SET resolved_at = ?, resolution = ?, retry_told = 0
				WHERE change_name = ? AND resolved_at IS NULL
			`, s.nowMS(), BlockAutoRetry, change); err != nil {
				return err
			}
			if st, err = s.loadQuarantineTx(ctx, tx, change); err != nil {
				return err
			}
		}
		out = st.record()
		return nil
	})
	return out, err
}

// MarkRetryTold records that the agent was told what stopped the change before its latest retry.
func (s *Store) MarkRetryTold(ctx context.Context, change string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		st, err := s.loadQuarantineTx(ctx, tx, change)
		if err != nil {
			return err
		}
		lr := st.lastRetried()
		if lr == nil {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE change_blocks SET retry_told = 1 WHERE id = ?`, lr.id)
		return err
	})
}

// QuarantineResolution says how a change's quarantine is lifted.
type QuarantineResolution struct {
	// Resolution is what resolves the open block (a Block* constant). Empty means BlockCleared, or
	// BlockUnblocked when Forget is set.
	Resolution string
	// By is who lifts it ("" when the kit does).
	By string
	// Forget starts the change's counters over: its retries and rebuilds no longer count.
	Forget bool
	// KeepApproval leaves a risk approval in force when Forget is set.
	KeepApproval bool
}

// ResolveQuarantine lifts the quarantine of a change: its open block is resolved, a rebuild request
// still pending is spent, and with Forget its counters start over and its risk approval is revoked.
func (s *Store) ResolveQuarantine(ctx context.Context, change string, r QuarantineResolution) error {
	if r.Resolution == "" {
		r.Resolution = BlockCleared
		if r.Forget {
			r.Resolution = BlockUnblocked
		}
	}
	if r.Forget && !forgets(r.Resolution) {
		r.Resolution = BlockUnblocked
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		st, err := s.loadQuarantineTx(ctx, tx, change)
		if err != nil {
			return err
		}
		if !st.exists() {
			return nil
		}
		if st.open() != nil {
			if err := s.resolveOpenBlockTx(ctx, tx, change, r.Resolution, r.By, now); err != nil {
				return err
			}
		} else if last := st.last(); r.Forget && last != nil {
			// Nothing is open to resolve, yet the counters must start over: the latest block is what
			// they are counted from, so it is the one marked as the point they start over at.
			if _, err := tx.ExecContext(ctx, `
				UPDATE change_blocks SET resolved_at = ?, resolution = ?, resolved_by = ? WHERE id = ?
			`, now, r.Resolution, nullStr(r.By), last.id); err != nil {
				return err
			}
		}
		if err := s.consumeRequestsTx(ctx, tx, change, now); err != nil {
			return err
		}
		if r.Forget && !r.KeepApproval {
			if _, err := tx.ExecContext(ctx, `
				UPDATE risk_approvals SET revoked_at = ?, revoke_reason = ? WHERE change_name = ? AND revoked_at IS NULL
			`, now, r.Resolution, change); err != nil {
				return err
			}
		}
		if r.Forget && len(st.chain) == 0 && len(st.reqs) > 0 {
			// Only rebuild requests count for this change: they are told apart from later ones by a
			// block resolved after them, so one is recorded as resolved at once.
			_, err := tx.ExecContext(ctx, `
				INSERT INTO change_blocks (change_name, reason, host, blocked_at, resolved_at, resolution, resolved_by)
				VALUES (?, 'rebuild_requested', ?, ?, ?, ?, ?)
			`, change, nullStr(s.host), now, now, r.Resolution, nullStr(r.By))
			return err
		}
		return nil
	})
}

// ClearQuarantine lifts the quarantine of a change. With forgetRetries its counters start over, as
// when an operator unblocks it; without, they are kept.
func (s *Store) ClearQuarantine(ctx context.Context, change string, forgetRetries bool) error {
	return s.ResolveQuarantine(ctx, change, QuarantineResolution{Forget: forgetRetries})
}

// DeferralCount is how many times a change was deferred because every engine of a role was out,
// since its deferrals were last cleared.
type DeferralCount struct {
	Count  int
	LastAt string
}

// RecordDeferral records that a change was deferred at `at` because every engine of role was out.
// counted is false for a deferral inside the cooldown of one already counted.
func (s *Store) RecordDeferral(ctx context.Context, change, role string, counted bool, at, retryAt time.Time, outputTail string) error {
	sc := ScopeFrom(ctx)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		if err := s.ensureChangeTx(ctx, tx, change, now); err != nil {
			return err
		}
		deferredAt := now
		if !at.IsZero() {
			deferredAt = at.UTC().UnixMilli()
		}
		var retry any
		if !retryAt.IsZero() {
			retry = retryAt.UTC().UnixMilli()
		}
		if role == "" {
			role = "unknown"
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO change_deferrals (change_name, change_run_id, role, counted, retry_at, output_tail, deferred_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, change, nullInt(sc.runFor(change)), role, boolToInt(counted), retry, nullStr(outputTail), deferredAt)
		return err
	})
}

// ClearDeferrals closes the deferrals of a change: an engine of the role works again, or the change
// reached the limit and was blocked.
func (s *Store) ClearDeferrals(ctx context.Context, change string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE change_deferrals SET cleared_at = ? WHERE change_name = ? AND cleared_at IS NULL
	`, s.nowMS(), change)
	return err
}

// DeferralCounts returns the open, counted deferrals of every change that has any.
func (s *Store) DeferralCounts(ctx context.Context) (map[string]DeferralCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT change_name, deferrals, last_deferred_at FROM v_deferral_count`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]DeferralCount)
	for rows.Next() {
		var name string
		var c DeferralCount
		var last int64
		if err := rows.Scan(&name, &c.Count, &last); err != nil {
			return nil, err
		}
		c.LastAt = msToRFC3339(last)
		out[name] = c
	}
	return out, rows.Err()
}

// QuarantineHistoryItem is one resolved block of a change.
type QuarantineHistoryItem struct {
	ChangeName string `json:"change_name"`
	RetryNum   int    `json:"retry_num"`
	Reason     string `json:"reason"`
	Details    string `json:"details,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Resolution string `json:"resolution"`
	ResolvedBy string `json:"resolved_by,omitempty"`
	BlockedAt  string `json:"blocked_at"`
	ArchivedAt string `json:"archived_at"`
}

// ListQuarantineHistory returns the latest resolved blocks, newest first: of one change, or of every
// change when change is empty.
func (s *Store) ListQuarantineHistory(ctx context.Context, change string, limit int) ([]QuarantineHistoryItem, error) {
	query := `SELECT change_name, retry_num, reason, details, stage, resolution, resolved_by, blocked_at, resolved_at
	          FROM change_blocks WHERE resolved_at IS NOT NULL`
	var args []any
	if change != "" {
		query += ` AND change_name = ?`
		args = append(args, change)
	}
	query += ` ORDER BY id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.ReadDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuarantineHistoryItem
	for rows.Next() {
		var it QuarantineHistoryItem
		var details, stage, resolution, by sql.NullString
		var blockedAt, resolvedAt int64
		if err := rows.Scan(&it.ChangeName, &it.RetryNum, &it.Reason, &details, &stage, &resolution, &by, &blockedAt, &resolvedAt); err != nil {
			return nil, err
		}
		it.Details, it.Stage, it.Resolution, it.ResolvedBy = details.String, stage.String, resolution.String, by.String
		it.BlockedAt = msToRFC3339(blockedAt)
		it.ArchivedAt = msToRFC3339(resolvedAt)
		out = append(out, it)
	}
	return out, rows.Err()
}
