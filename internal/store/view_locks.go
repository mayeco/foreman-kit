package store

import (
	"context"
	"database/sql"
)

// HeldLock is a lock or a lease on record, with whether what took it still holds it.
type HeldLock struct {
	Key       string `json:"key"`
	OwnerType string `json:"owner_type"`
	PID       int    `json:"pid"`
	Host      string `json:"host"`
	Slot      int    `json:"slot,omitempty"`
	// Stamp names what the lock is held for, when its holder said it: the cycle, the session.
	Stamp string `json:"stamp,omitempty"`
	// ChangeName is the change a worker holds the lease of, "" for every other lock.
	ChangeName  string `json:"change_name,omitempty"`
	AcquiredAt  string `json:"acquired_at"`
	HeartbeatAt string `json:"heartbeat_at"`
	// ExpiresAt is when the lock stops counting if its holder says nothing more.
	ExpiresAt string `json:"expires_at"`
	// Stale says nothing holds the lock any more: its time ran out, or its process is gone. The
	// next process that wants it takes it.
	Stale bool `json:"stale"`
}

// ListLocks returns the locks and the leases on record, the one taken first first. It releases
// none: a lock nothing holds any more is returned as stale.
func (v *View) ListLocks(ctx context.Context) ([]HeldLock, error) {
	out := []HeldLock{}
	nowUnix := v.now / 1000
	err := v.each(ctx, func(scan func(dest ...any) error) error {
		var rec LockRecord
		var slot sql.NullInt64
		var stamp sql.NullString
		var acquired, heartbeat int64
		if err := scan(&rec.LockKey, &rec.OwnerType, &rec.PID, &rec.Host, &slot, &stamp, &acquired, &heartbeat, &rec.TTLSeconds); err != nil {
			return err
		}
		rec.Slot, rec.Stamp = int(slot.Int64), stamp.String
		rec.AcquiredAt, rec.HeartbeatAt = acquired/1000, heartbeat/1000
		held := HeldLock{
			Key: rec.LockKey, OwnerType: rec.OwnerType, PID: rec.PID, Host: rec.Host, Slot: rec.Slot, Stamp: rec.Stamp,
			AcquiredAt:  msToRFC3339(acquired),
			HeartbeatAt: msToRFC3339(heartbeat),
			ExpiresAt:   msToRFC3339(heartbeat + int64(rec.TTLSeconds)*1000),
			Stale:       IsLockStale(rec, v.s.host, nowUnix),
		}
		if change, lease := WorkerLeaseChange(rec.LockKey); lease {
			held.ChangeName = change
		}
		out = append(out, held)
		return nil
	}, `SELECT lock_key, owner_type, pid, host, slot, stamp, acquired_at, heartbeat_at, ttl_seconds
	    FROM locks ORDER BY acquired_at, lock_key`)
	return out, err
}
