package store

import (
	"context"
	"encoding/json"
	"time"
)

// CycleSchedule is what the daemon that runs now says about its next cycle. It is a forecast, kept
// with the daemon's lock and rewritten as it changes. The facts are the cycles that ran, and each of
// those says what started it (Cycle.Trigger).
type CycleSchedule struct {
	// Interval is the daemon's interval (settings.CycleEvery): the longest it waits
	// between two cycles.
	Interval time.Duration
	// IdleDelay is the daemon's idle delay (settings.CycleEveryWhenOutOfWork): how long after a cycle
	// began the next one may start before its tick. 0: the cycle only runs on its tick.
	IdleDelay time.Duration
	// Latest is when the next cycle starts at the latest: the daemon's tick.
	Latest time.Time
	// Earliest is from when the next cycle may start before its tick. It is zero when IdleDelay is
	// 0, and while no cycle is on record to count the delay from.
	Earliest time.Time
	// Waits says what keeps the next cycle from starting before its tick, in the daemon's word for
	// it: "delay", "no_report", "busy", "queue_full", "no_movement", "no_cycle". It is "" when
	// IdleDelay is 0.
	Waits string
}

// cycleScheduleJSON is a CycleSchedule as the daemon's lock keeps it, under "next_cycle". A value
// that does not apply is null, so that merging it clears what an earlier schedule said.
type cycleScheduleJSON struct {
	Interval  string  `json:"interval"`
	IdleDelay *string `json:"idle_delay"`
	Latest    *string `json:"latest"`
	Earliest  *string `json:"earliest"`
	Waits     *string `json:"waits"`
}

func optionalInstant(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	v := t.UTC().Format(time.RFC3339)
	return &v
}

func instantOf(v *string) time.Time {
	if v == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, *v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// SetCycleSchedule records the schedule of the daemon pid with its lock, leaving the rest of what
// the lock says as it is. It changes nothing when pid does not hold the daemon lock.
func (s *Store) SetCycleSchedule(ctx context.Context, pid int, sched CycleSchedule) error {
	doc := cycleScheduleJSON{
		Interval: sched.Interval.String(),
		Latest:   optionalInstant(sched.Latest),
		Earliest: optionalInstant(sched.Earliest),
	}
	if sched.IdleDelay > 0 {
		delay := sched.IdleDelay.String()
		doc.IdleDelay = &delay
		if sched.Waits != "" {
			doc.Waits = &sched.Waits
		}
	}
	patch, err := json.Marshal(map[string]any{"next_cycle": doc})
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE locks SET metadata_json = json_patch(COALESCE(metadata_json, '{}'), ?)
		WHERE lock_key = 'daemon' AND pid = ?
	`, string(patch), pid)
	return err
}

// CycleSchedule returns what the daemon that runs now says about its next cycle. It is nil when no
// daemon runs, and when the one that does has said nothing yet.
func (s *Store) CycleSchedule(ctx context.Context) (*CycleSchedule, error) {
	lock, err := s.LiveLock(ctx, "daemon")
	if err != nil || lock == nil || lock.MetadataJSON == "" {
		return nil, err
	}
	var meta struct {
		NextCycle *cycleScheduleJSON `json:"next_cycle"`
	}
	if err := json.Unmarshal([]byte(lock.MetadataJSON), &meta); err != nil || meta.NextCycle == nil {
		return nil, nil
	}
	doc := meta.NextCycle
	sched := &CycleSchedule{Latest: instantOf(doc.Latest), Earliest: instantOf(doc.Earliest)}
	sched.Interval, _ = time.ParseDuration(doc.Interval)
	if doc.IdleDelay != nil {
		sched.IdleDelay, _ = time.ParseDuration(*doc.IdleDelay)
	}
	if doc.Waits != nil {
		sched.Waits = *doc.Waits
	}
	return sched, nil
}
