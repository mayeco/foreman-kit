package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"
)

// EngineHealthRecord is the health and cooldown of an agent engine, as it was last recorded.
// CooldownUntil is in Unix seconds.
type EngineHealthRecord struct {
	Engine              string `json:"engine"`
	Installed           bool   `json:"installed"`
	Status              string `json:"status"` // "ok", "auth_failed", "quota_exceeded", "config_error", "not_installed", "timeout"
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastLatencyMS       int64  `json:"last_latency_ms"`
	LastError           string `json:"last_error,omitempty"`
	LastCheckedAt       string `json:"last_checked_at,omitempty"`
	CooldownUntil       int64  `json:"cooldown_until"`
}

func normalizeHealthStatus(status string) string {
	switch status {
	case "ok", "auth_failed", "quota_exceeded", "config_error", "not_installed", "timeout":
		return status
	}
	return "unknown"
}

// RecordEngineHealth records the health of an engine and, when it failed, until when it is left
// alone (CooldownUntil).
func (s *Store) RecordEngineHealth(ctx context.Context, rec EngineHealthRecord) error {
	engine := normalizeEngine(rec.Engine)
	if engine == "" {
		return nil
	}
	checked := rfc3339ToMS(rec.LastCheckedAt)
	if checked == 0 {
		checked = s.nowMS()
	}
	if rec.Status == "ok" {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO engines (engine, installed, health_status, consecutive_failures, last_latency_ms, last_error, last_checked_at, cooldown_until)
			VALUES (?, 1, 'ok', 0, ?, NULL, ?, 0)
			ON CONFLICT(engine) DO UPDATE SET
				installed = 1,
				health_status = 'ok',
				consecutive_failures = 0,
				last_latency_ms = excluded.last_latency_ms,
				last_error = NULL,
				last_checked_at = excluded.last_checked_at,
				cooldown_until = 0
		`, engine, rec.LastLatencyMS, checked)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO engines (engine, installed, health_status, consecutive_failures, last_latency_ms, last_error, last_checked_at, cooldown_until)
		VALUES (?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(engine) DO UPDATE SET
			installed = excluded.installed,
			health_status = excluded.health_status,
			consecutive_failures = engines.consecutive_failures + 1,
			last_latency_ms = excluded.last_latency_ms,
			last_error = excluded.last_error,
			last_checked_at = excluded.last_checked_at,
			cooldown_until = excluded.cooldown_until
	`, engine, boolToInt(rec.Installed), normalizeHealthStatus(rec.Status), rec.LastLatencyMS, nullStr(rec.LastError), checked, rec.CooldownUntil*1000)
	return err
}

// RecordEnginePing records how an engine answered a ping a person asked for: health is "ok", or the
// value of engines.health_status that says what the ping found wrong. A ping that answers clears
// the failures of the engine and the time it is left for last, as a run that works does. A ping
// that fails records how, and leaves that time as it is: how long an engine is left alone is
// decided by what happens to the runs of the kit, not by a ping.
func (s *Store) RecordEnginePing(ctx context.Context, engine, health string, latencyMS int64, lastError string) error {
	engine = normalizeEngine(engine)
	if engine == "" {
		return errors.New("store: a ping needs an engine")
	}
	if health == "ok" {
		return s.RecordEngineHealth(ctx, EngineHealthRecord{Engine: engine, Installed: true, Status: "ok", LastLatencyMS: latencyMS})
	}
	health = normalizeHealthStatus(health)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO engines (engine, installed, health_status, consecutive_failures, last_latency_ms, last_error, last_checked_at, cooldown_until)
		VALUES (?, ?, ?, 1, ?, ?, ?, 0)
		ON CONFLICT(engine) DO UPDATE SET
			installed = excluded.installed,
			health_status = excluded.health_status,
			consecutive_failures = engines.consecutive_failures + 1,
			last_latency_ms = excluded.last_latency_ms,
			last_error = excluded.last_error,
			last_checked_at = excluded.last_checked_at
	`, engine, boolToInt(health != "not_installed"), health, latencyMS, nullStr(lastError), s.nowMS())
	return err
}

// ListEngineHealth returns the health of every engine whose health was recorded, keyed by engine.
func (s *Store) ListEngineHealth(ctx context.Context) (map[string]EngineHealthRecord, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT engine, installed, health_status, consecutive_failures, last_latency_ms, last_error, last_checked_at, cooldown_until
		FROM engines WHERE last_checked_at IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]EngineHealthRecord)
	for rows.Next() {
		var r EngineHealthRecord
		var inst int
		var lastErr sql.NullString
		var lastChk sql.NullInt64
		var cooldown int64
		if err := rows.Scan(&r.Engine, &inst, &r.Status, &r.ConsecutiveFailures, &r.LastLatencyMS, &lastErr, &lastChk, &cooldown); err != nil {
			return nil, err
		}
		r.Installed = inst != 0
		r.LastError = lastErr.String
		r.LastCheckedAt = nullMSToRFC3339(lastChk)
		r.CooldownUntil = cooldown / 1000
		out[r.Engine] = r
	}
	return out, rows.Err()
}

// claimPIDBits is the number of low bits of an engine claim id that hold the owning process ID.
// proc.AgentExec registers its concurrent claims as AgentClaimID(n, os.Getpid()); a plain PID
// (`foreman-kit agents claim <agent> <pid>`) decodes to itself. Linux PIDs stay below 2^22 and
// macOS PIDs below 100000, so 32 bits always hold the PID.
const claimPIDBits = 32

// AgentClaimID returns the id of the n-th engine claim made by process pid.
func AgentClaimID(n int64, pid int) int {
	return int(n)<<claimPIDBits | pid
}

// ClaimOwnerPID returns the process ID that owns an engine claim id.
func ClaimOwnerPID(claimID int) int {
	return claimID & (1<<claimPIDBits - 1)
}

// pruneDeadClaims removes the claims whose owning process is gone.
func (s *Store) pruneDeadClaims(ctx context.Context, q querier) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, "SELECT claim_id, owner_pid, engine FROM engine_claims")
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	var dead []int64
	for rows.Next() {
		var claimID int64
		var owner int
		var engine string
		if err := rows.Scan(&claimID, &owner, &engine); err != nil {
			rows.Close()
			return nil, err
		}
		if !IsPIDAlive(owner) {
			dead = append(dead, claimID)
			continue
		}
		if engine != "" {
			counts[engine]++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range dead {
		_, _ = q.ExecContext(ctx, "DELETE FROM engine_claims WHERE claim_id = ?", id)
	}
	return counts, nil
}

func (s *Store) claimTx(ctx context.Context, q querier, claimID int, engine, role, change string) error {
	engine = normalizeEngine(engine)
	if engine == "" {
		return nil
	}
	now := s.nowMS()
	if err := s.ensureEngineTx(ctx, q, engine); err != nil {
		return err
	}
	if err := s.ensureChangeTx(ctx, q, change, now); err != nil {
		return err
	}
	execID := ScopeFrom(ctx).ExecID
	if execID != "" {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM executions WHERE exec_id = ?`, execID).Scan(&n); err != nil || n == 0 {
			execID = ""
		}
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO engine_claims (claim_id, owner_pid, host, engine, role, change_name, exec_id, claimed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(claim_id) DO UPDATE SET
			engine = excluded.engine,
			role = COALESCE(excluded.role, engine_claims.role),
			change_name = COALESCE(excluded.change_name, engine_claims.change_name),
			exec_id = COALESCE(excluded.exec_id, engine_claims.exec_id),
			claimed_at = excluded.claimed_at
	`, claimID, ClaimOwnerPID(claimID), s.host, engine, nullStr(role), nullStr(change), nullStr(execID), now)
	return err
}

// ClaimAgent registers that the process behind claimID (a plain PID, or an AgentClaimID) is running
// engine, for role and change when they are known.
func (s *Store) ClaimAgent(ctx context.Context, claimID int, engine, role, change string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.pruneDeadClaims(ctx, tx); err != nil {
			return err
		}
		return s.claimTx(ctx, tx, claimID, engine, role, change)
	})
}

// ReleaseAgent removes an engine claim.
func (s *Store) ReleaseAgent(ctx context.Context, claimID int) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM engine_claims WHERE claim_id = ?", claimID)
	return err
}

// AgentsInUse returns a map of engine -> active call count, pruning the claims of dead processes.
func (s *Store) AgentsInUse(ctx context.Context) (map[string]int, error) {
	var counts map[string]int
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		counts, err = s.pruneDeadClaims(ctx, tx)
		return err
	})
	return counts, err
}

// EngineClaim is an engine a live process is running now.
type EngineClaim struct {
	ClaimID    int    `json:"claim_id"`
	OwnerPID   int    `json:"owner_pid"`
	Engine     string `json:"engine"`
	Role       string `json:"role,omitempty"`
	ChangeName string `json:"change_name,omitempty"`
	ExecID     string `json:"exec_id,omitempty"`
	StartedAt  int64  `json:"started_at"` // Unix seconds
}

// ListEngineClaims returns the engine claims on record, oldest first.
func (s *Store) ListEngineClaims(ctx context.Context) ([]EngineClaim, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT claim_id, owner_pid, engine, role, change_name, exec_id, claimed_at FROM engine_claims ORDER BY claimed_at, claim_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EngineClaim
	for rows.Next() {
		var c EngineClaim
		var role, change, execID sql.NullString
		var at int64
		if err := rows.Scan(&c.ClaimID, &c.OwnerPID, &c.Engine, &role, &change, &execID, &at); err != nil {
			return nil, err
		}
		c.Role, c.ChangeName, c.ExecID = role.String, change.String, execID.String
		c.StartedAt = at / 1000
		out = append(out, c)
	}
	return out, rows.Err()
}

// ArrangeAgents orders candidate engines as selection says ("in_order", "random" or "least_busy")
// and atomically claims the chosen first engine for claimID if claimID > 0.
func (s *Store) ArrangeAgents(ctx context.Context, candidates []string, orderMode string, claimID int, pickRandom func(int) int) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if pickRandom == nil {
		pickRandom = func(n int) int { return 0 }
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := beginRawTx(ctx, conn); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			rollbackRawTx(ctx, conn)
		}
	}()

	counts, err := s.pruneDeadClaims(ctx, conn)
	if err != nil {
		return nil, err
	}

	cooldownSet := make(map[string]bool)
	if hRows, err := conn.QueryContext(ctx, "SELECT engine FROM engines WHERE cooldown_until > ?", time.Now().UTC().UnixMilli()); err == nil {
		for hRows.Next() {
			var eng string
			if hRows.Scan(&eng) == nil {
				cooldownSet[eng] = true
			}
		}
		hRows.Close()
	}

	var healthy, cooled []string
	for _, c := range candidates {
		if cooldownSet[c] {
			cooled = append(cooled, c)
		} else {
			healthy = append(healthy, c)
		}
	}
	pool := healthy
	if len(pool) == 0 {
		pool = candidates
	}

	var first string
	switch orderMode {
	case "random":
		first = pool[pickRandom(len(pool))%len(pool)]
	case "least_busy":
		minUse := -1
		var least []string
		for _, c := range pool {
			u := counts[c]
			if minUse == -1 || u < minUse {
				minUse = u
				least = []string{c}
			} else if u == minUse {
				least = append(least, c)
			}
		}
		sort.Strings(least)
		first = least[pickRandom(len(least))%len(least)]
	default:
		first = pool[0]
	}

	if claimID > 0 {
		if err := s.claimTx(ctx, conn, claimID, first, "", ""); err != nil {
			return nil, err
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// COMMIT runs detached from cancellation: an interrupted COMMIT can still commit and would
	// then be reported as a failure.
	if _, err := conn.ExecContext(context.WithoutCancel(ctx), "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	_ = conn.Close()

	result := make([]string, 0, len(candidates))
	result = append(result, first)
	for _, c := range healthy {
		if c != first {
			result = append(result, c)
		}
	}
	for _, c := range cooled {
		if c != first {
			result = append(result, c)
		}
	}
	return result, nil
}
