package store

import (
	"context"
	"database/sql"
)

// EngineUse is a process that runs an engine now.
type EngineUse struct {
	ClaimID  int `json:"claim_id"`
	OwnerPID int `json:"owner_pid"`
	// Role is the role the engine was picked for; ChangeName and ExecID, the change and the
	// execution it works on. Each is "" when the process that claimed the engine did not say it.
	Role       string `json:"role,omitempty"`
	ChangeName string `json:"change_name,omitempty"`
	ExecID     string `json:"exec_id,omitempty"`
	ClaimedAt  string `json:"claimed_at"`
}

// EngineRecord is an engine the kit has on record: its health as the last check left it, and the
// processes that run it now.
type EngineRecord struct {
	Engine string `json:"engine"`
	// Health is one of the values of engines.health_status: "unknown" until a ping or a run of the
	// engine says how it is.
	Health              string `json:"health"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastLatencyMS       int64  `json:"last_latency_ms,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	LastCheckedAt       string `json:"last_checked_at,omitempty"`
	// CooldownUntil is until when the kit leaves the engine for last after it failed, "" when that
	// time has passed or the engine did not fail.
	CooldownUntil string `json:"cooldown_until,omitempty"`
	// InUse holds the processes that run the engine now, the one that began first first.
	InUse []EngineUse `json:"in_use"`
}

// ListEngines returns the engines on record, by name, each with its health and the processes that
// run it now. It removes nothing: the claim a dead process left is skipped here, and deleted by the
// next process that claims an engine.
func (v *View) ListEngines(ctx context.Context) ([]EngineRecord, error) {
	out := []EngineRecord{}
	at := map[string]int{}
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		rec := EngineRecord{InUse: []EngineUse{}}
		var lastError sql.NullString
		var checked sql.NullInt64
		var cooldown int64
		if err := scan(&rec.Engine, &rec.Health, &rec.ConsecutiveFailures, &rec.LastLatencyMS, &lastError, &checked, &cooldown); err != nil {
			return err
		}
		rec.LastError = lastError.String
		rec.LastCheckedAt = nullMSToRFC3339(checked)
		if cooldown > v.now {
			rec.CooldownUntil = msToRFC3339(cooldown)
		}
		at[rec.Engine] = len(out)
		out = append(out, rec)
		return nil
	}, `SELECT engine, health_status, consecutive_failures, last_latency_ms, last_error, last_checked_at, cooldown_until
	    FROM engines ORDER BY engine`)
	if err != nil {
		return nil, err
	}
	err = v.each(ctx, func(scan func(dest ...any) error) error {
		var use EngineUse
		var engine string
		var role, change, execID sql.NullString
		var claimed int64
		if err := scan(&use.ClaimID, &use.OwnerPID, &engine, &role, &change, &execID, &claimed); err != nil {
			return err
		}
		i, known := at[engine]
		if !known || !IsPIDAlive(use.OwnerPID) {
			return nil
		}
		use.Role, use.ChangeName, use.ExecID = role.String, change.String, execID.String
		use.ClaimedAt = msToRFC3339(claimed)
		out[i].InUse = append(out[i].InUse, use)
		return nil
	}, `SELECT claim_id, owner_pid, engine, role, change_name, exec_id, claimed_at
	    FROM engine_claims ORDER BY claimed_at, claim_id`)
	if err != nil {
		return nil, err
	}
	return out, nil
}
