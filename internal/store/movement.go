package store

import (
	"context"
	"database/sql"
	"time"
)

// Movement is what the pipeline did to changes in a stretch of time: the facts that say it is getting
// somewhere. A run that ended held, deferred, paused, stopped or lost is not among them: it left its
// change where it was.
type Movement struct {
	// Built counts the change runs that ended ready: built, gates passed, handed to the merger.
	Built int `json:"built"`
	// Blocked counts the blocks opened, by a worker or by the merger.
	Blocked int `json:"blocked"`
	// Integrated counts the merge attempts that integrated their change.
	Integrated int `json:"integrated"`
}

// Any reports whether anything moved.
func (m Movement) Any() bool { return m.Built+m.Blocked+m.Integrated > 0 }

// MovementBetween counts what moved from the instant from, included, to the instant to, excluded.
// A zero to means until now.
func (s *Store) MovementBetween(ctx context.Context, from, to time.Time) (Movement, error) {
	return s.movementBetweenMS(ctx, from.UnixMilli(), untilMS(to))
}

// untilMS is the upper bound of a stretch as the queries take it: no bound for a zero instant.
func untilMS(to time.Time) int64 {
	if to.IsZero() {
		return 1<<63 - 1
	}
	return to.UnixMilli()
}

func (s *Store) movementBetweenMS(ctx context.Context, from, to int64) (Movement, error) {
	var m Movement
	err := s.ReadDB().QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM change_runs
			  WHERE result = 'ready' AND finished_at >= ?1 AND finished_at < ?2),
			(SELECT COUNT(*) FROM change_blocks
			  WHERE blocked_at >= ?1 AND blocked_at < ?2),
			(SELECT COUNT(*) FROM merge_attempts
			  WHERE result = 'integrated' AND finished_at >= ?1 AND finished_at < ?2)
	`, from, to).Scan(&m.Built, &m.Blocked, &m.Integrated)
	return m, err
}

// CycleMovement is a cycle on record with what the pipeline moved from the moment it began to the
// moment the next cycle began; for the latest cycle, to now.
type CycleMovement struct {
	Stamp     string    `json:"stamp"`
	StartedAt time.Time `json:"started_at"`
	// FinishedAt is zero while the cycle runs.
	FinishedAt time.Time `json:"finished_at"`
	Movement
}

// RecentCycleMovement returns the latest n cycles on record, the newest first, each with its
// movement. The daemon reads the latest two to decide whether the next cycle may start before its
// tick: when neither moved anything, another judgment would find the same queue.
func (s *Store) RecentCycleMovement(ctx context.Context, n int) ([]CycleMovement, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT stamp, started_at, finished_at FROM cycles ORDER BY started_at DESC, stamp DESC LIMIT ?
	`, n)
	if err != nil {
		return nil, err
	}
	var out []CycleMovement
	var starts []int64
	for rows.Next() {
		var c CycleMovement
		var started int64
		var finished sql.NullInt64
		if err := rows.Scan(&c.Stamp, &started, &finished); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.StartedAt = time.UnixMilli(started).UTC()
		if finished.Valid {
			c.FinishedAt = time.UnixMilli(finished.Int64).UTC()
		}
		out = append(out, c)
		starts = append(starts, started)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		until := untilMS(time.Time{})
		if i > 0 {
			until = starts[i-1]
		}
		m, err := s.movementBetweenMS(ctx, starts[i], until)
		if err != nil {
			return nil, err
		}
		out[i].Movement = m
	}
	return out, nil
}
