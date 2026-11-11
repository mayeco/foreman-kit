package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrLockHeld indicates the lock or lease is currently held by another active owner.
var ErrLockHeld = errors.New("store: lock is already held")

// WorkerLeasePrefix prefixes the lock key of the lease a dispatcher worker holds on the change it builds.
const WorkerLeasePrefix = "change:"

// WorkerLeaseKey returns the lock key of the worker lease on change.
func WorkerLeaseKey(change string) string { return WorkerLeasePrefix + change }

// WorkerLeaseChange returns the change a worker lease key belongs to. ok is false for every other
// lock (cycle, dispatch, dispatcher, merger, daemon, record, visual_qa, ...).
func WorkerLeaseChange(lockKey string) (change string, ok bool) {
	change, ok = strings.CutPrefix(lockKey, WorkerLeasePrefix)
	return change, ok && change != ""
}

// LockRecord represents a row in the locks table.
type LockRecord struct {
	LockKey      string `json:"lock_key"`
	OwnerType    string `json:"owner_type"`
	PID          int    `json:"pid"`
	Host         string `json:"host"`
	Slot         int    `json:"slot,omitempty"`
	Stamp        string `json:"stamp,omitempty"`
	AcquiredAt   int64  `json:"acquired_at"`
	HeartbeatAt  int64  `json:"heartbeat_at"`
	TTLSeconds   int    `json:"ttl_seconds"`
	MetadataJSON string `json:"metadata_json,omitempty"`
}

// LockOptions configures a lock or lease acquisition.
type LockOptions struct {
	OwnerType string
	PID       int
	Host      string
	Slot      int
	Stamp     string
	TTL       time.Duration
	Metadata  map[string]any
}

// IsPIDAlive checks whether a local process ID owned by the current user is alive
// using kill(pid, 0). A zombie, which still answers kill(pid, 0) until its parent reaps it, is not alive:
// on Linux /proc/<pid>/stat reports it as 'Z' (or 'X', dead), and on macOS and the BSDs getpgid(pid)
// no longer finds it.
func IsPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	if runtime.GOOS == "linux" {
		if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			s := string(raw)
			if closeParen := strings.LastIndexByte(s, ')'); closeParen >= 0 && closeParen+2 < len(s) {
				state := s[closeParen+2]
				if state == 'Z' || state == 'X' {
					return false
				}
			}
		}
		return true
	}
	// There is no /proc to read here, and spawning ps for every liveness check is too slow for the
	// callers that poll. The kernel answers kill for an exited process until it is reaped, but looks
	// only live processes up for getpgid.
	if _, err := syscall.Getpgid(pid); errors.Is(err, syscall.ESRCH) {
		return false
	}
	return true
}

// selfStartUnix approximates when this process started. A lock row that carries this process's PID but
// was written before that belongs to an earlier process that had the same PID (a restarted container
// reuses PID 1, for example).
var selfStartUnix = time.Now().UTC().Unix()

// linuxProcessStartUnix derives a process start time from /proc/<pid>/stat field 22 (starttime, in clock
// ticks since boot) and the btime line of /proc/stat, so PID-reuse detection does not depend on a ps binary
// (absent from debian-slim, distroless and scratch images; busybox ps has no lstart).
func linuxProcessStartUnix(pid int) int64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(raw)
	closeParen := strings.LastIndexByte(s, ')')
	if closeParen < 0 || closeParen+2 >= len(s) {
		return 0
	}
	// fields[0] is field 3 (state), so field 22 (starttime) is fields[19].
	fields := strings.Fields(s[closeParen+2:])
	if len(fields) < 20 {
		return 0
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil || ticks < 0 {
		return 0
	}
	statRaw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(statRaw), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			btime, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil || btime <= 0 {
				return 0
			}
			// USER_HZ is 100 for /proc on every Linux architecture this binary targets (amd64, arm64).
			return btime + ticks/100
		}
	}
	return 0
}

// ProcessStartUnix returns when the process with this ID started, in Unix seconds, or 0 when that cannot
// be read (no such process, or neither /proc nor a ps with lstart). A PID alone does not identify a
// process: the system gives it to another one once the first has exited.
func ProcessStartUnix(pid int) int64 { return processStartTimeUnix(pid) }

// processStartProbeTimeout bounds the ps that reads a process's start time. On macOS, and on Linux
// without /proc, it runs while AcquireLock decides inside its transaction whether a lock is stale: a ps
// that does not answer would hold the single writer connection, and with it the database's write lock
// against every other process, for as long as it hangs. An unanswered question reads as "unknown", which
// leaves the lock with its owner.
var processStartProbeTimeout = 2 * time.Second

func processStartTimeUnix(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	if runtime.GOOS == "linux" {
		if t := linuxProcessStartUnix(pid); t > 0 {
			return t
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), processStartProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return 0
	}
	for _, layout := range []string{
		"Mon Jan _2 15:04:05 2006",
		"Mon Jan 2 15:04:05 2006",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// IsLockStale reports whether an existing lock record has expired or belongs to a dead or reused local PID.
func IsLockStale(rec LockRecord, currentHost string, nowUnix int64) bool {
	if currentHost == "" {
		currentHost, _ = os.Hostname()
	}
	hb := rec.HeartbeatAt
	if hb == 0 {
		hb = rec.AcquiredAt
	}
	if rec.TTLSeconds > 0 && (nowUnix-hb) > int64(rec.TTLSeconds) {
		return true
	}

	// visual-qa begin is a short-lived CLI command whose PID exits immediately
	// while the visual QA session stays active until its TTL expires.
	if rec.LockKey == "visual_qa" || rec.OwnerType == "visual_qa" {
		return false
	}

	// For process locks on the same host, check PID liveness and PID reuse via process start time.
	if rec.PID > 0 && (rec.Host == "" || rec.Host == currentHost) {
		if rec.PID == os.Getpid() {
			// Our own PID is always alive: the row is ours only if it was written after we started.
			return rec.AcquiredAt > 0 && rec.AcquiredAt+1 < selfStartUnix
		}
		if !IsPIDAlive(rec.PID) {
			return true
		}
		if rec.AcquiredAt > 0 || rec.HeartbeatAt > 0 {
			if lockOwnerReplaced(rec, processStartTimeUnix(rec.PID), nowUnix) {
				return true
			}
		}
	}

	return false
}

// lockOwnerReplaced reports whether the live process that now has rec.PID, started at procStart, is a
// later process than the one that wrote rec: it started after the row was last written.
//
// The row's last write is its latest heartbeat, not its acquisition. Both are wall-clock readings, and so
// is procStart, which Linux derives from the current boot time: after the clock is stepped forward (a VM
// or WSL2 guest resumed after its host slept, the first time sync of a board without a clock) every
// running process appears to have started that much later. Compared with the acquisition, a daemon's
// locks then looked stale to every other process for as long as it held them, and were taken from it.
// Its next heartbeat is written with the new clock, which a process that merely got the same PID never
// does. A start time in the future means the clock was set back after the process started (macOS keeps
// the start time it gave) and says nothing about PID reuse.
func lockOwnerReplaced(rec LockRecord, procStart, nowUnix int64) bool {
	// Start times and lock rows have a resolution of one second, and Linux rounds the boot time.
	const slack = 5
	if procStart <= 0 || procStart > nowUnix+slack {
		return false
	}
	lastWrite := max(rec.AcquiredAt, rec.HeartbeatAt)
	return lastWrite > 0 && procStart > lastWrite+slack
}

// AcquireLock attempts to atomically acquire a lock/lease in SQLite.
// If an existing lock is stale, it reclaims it and emits a lease.stale_reclaimed event.
func (s *Store) AcquireLock(ctx context.Context, key string, opts LockOptions) (*LockRecord, error) {
	if opts.PID <= 0 {
		opts.PID = os.Getpid()
	}
	if opts.Host == "" {
		opts.Host, _ = os.Hostname()
	}
	if opts.OwnerType == "" {
		opts.OwnerType = "worker"
	}
	ttlSec := int(opts.TTL.Seconds())
	if ttlSec <= 0 {
		ttlSec = 7200
	}
	metaStr := "{}"
	if len(opts.Metadata) > 0 {
		if b, err := json.Marshal(opts.Metadata); err == nil {
			metaStr = string(b)
		}
	}

	// Rows hold milliseconds; a LockRecord, like IsLockStale, speaks in seconds.
	nowMS := time.Now().UTC().UnixMilli()
	now := nowMS / 1000
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Not a bare BEGIN: one reported as canceled may have opened the transaction all the same, and
	// returning without ending it would leave the single writer connection inside it (beginRawTx).
	if err := beginRawTx(ctx, conn); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			rollbackRawTx(ctx, conn)
		}
	}()

	var existing LockRecord
	var slot sql.NullInt64
	var stamp, meta sql.NullString
	err = conn.QueryRowContext(ctx, `
		SELECT lock_key, owner_type, pid, host, slot, stamp, acquired_at / 1000, heartbeat_at / 1000, ttl_seconds, metadata_json
		FROM locks WHERE lock_key = ?
	`, key).Scan(
		&existing.LockKey, &existing.OwnerType, &existing.PID, &existing.Host,
		&slot, &stamp, &existing.AcquiredAt, &existing.HeartbeatAt, &existing.TTLSeconds, &meta,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	var reclaimed *LockRecord
	if err == nil {
		existing.Slot = int(slot.Int64)
		existing.Stamp = stamp.String
		existing.MetadataJSON = meta.String
		if !IsLockStale(existing, opts.Host, now) {
			return nil, ErrLockHeld
		}
		reclaimed = &existing
		if _, err := conn.ExecContext(ctx, "DELETE FROM locks WHERE lock_key = ?", key); err != nil {
			return nil, err
		}
	}

	_, err = conn.ExecContext(ctx, `
		INSERT INTO locks (lock_key, owner_type, pid, host, slot, stamp, acquired_at, heartbeat_at, ttl_seconds, metadata_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, key, opts.OwnerType, opts.PID, opts.Host, opts.Slot, opts.Stamp, nowMS, nowMS, ttlSec, metaStr)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The lock row is written: COMMIT runs detached from cancellation. A COMMIT interrupted by a
	// cancellation can still have committed and is then reported as ctx.Err() by the driver, which
	// would leak this lock under a live PID until its TTL; every caller pairs success with ReleaseLock.
	if _, err := conn.ExecContext(context.WithoutCancel(ctx), "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	_ = conn.Close()

	if reclaimed != nil {
		_ = s.EmitEvent(ctx, EventInput{
			Event: "lease.stale_reclaimed",
			PID:   opts.PID,
			Payload: map[string]any{
				"lock_key":      key,
				"previous_pid":  reclaimed.PID,
				"previous_host": reclaimed.Host,
			},
		})
	}

	return &LockRecord{
		LockKey:      key,
		OwnerType:    opts.OwnerType,
		PID:          opts.PID,
		Host:         opts.Host,
		Slot:         opts.Slot,
		Stamp:        opts.Stamp,
		AcquiredAt:   now,
		HeartbeatAt:  now,
		TTLSeconds:   ttlSec,
		MetadataJSON: metaStr,
	}, nil
}

// HeartbeatLock refreshes heartbeat_at (and optionally ttl_seconds) for an owned lock.
func (s *Store) HeartbeatLock(ctx context.Context, key string, pid int, ttl ...time.Duration) error {
	if pid <= 0 {
		pid = os.Getpid()
	}
	now := time.Now().UTC().UnixMilli()
	var (
		res sql.Result
		err error
	)
	if len(ttl) > 0 && int(ttl[0].Seconds()) > 0 {
		res, err = s.db.ExecContext(ctx, `
			UPDATE locks SET heartbeat_at = ?, ttl_seconds = ? WHERE lock_key = ? AND pid = ?
		`, now, int(ttl[0].Seconds()), key, pid)
	} else {
		res, err = s.db.ExecContext(ctx, `
			UPDATE locks SET heartbeat_at = ? WHERE lock_key = ? AND pid = ?
		`, now, key, pid)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrLockHeld
	}
	return nil
}

// StartLockHeartbeat spawns a background goroutine that refreshes lockName for pid
// every min(ttl/3, 15*time.Second) until the returned stop function is called or ctx is canceled.
func (s *Store) StartLockHeartbeat(ctx context.Context, lockName string, pid int, ttl time.Duration) func() {
	if s == nil {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if pid <= 0 {
		pid = os.Getpid()
	}
	interval := 15 * time.Second
	if ttl > 0 && ttl/3 < interval {
		interval = ttl / 3
	}
	if interval <= 0 {
		interval = time.Millisecond
	}

	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				_ = s.HeartbeatLock(hbCtx, lockName, pid, ttl)
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// ReleaseLock deletes a lock owned by pid (or unconditionally if pid == 0).
func (s *Store) ReleaseLock(ctx context.Context, key string, pid int) error {
	if pid <= 0 {
		_, err := s.db.ExecContext(ctx, "DELETE FROM locks WHERE lock_key = ?", key)
		return err
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM locks WHERE lock_key = ? AND pid = ?", key, pid)
	return err
}

// LiveLock returns the lock row for key only while its owner still holds it: rows whose TTL expired,
// whose PID is dead, or whose PID was reused by a later process (IsLockStale) are reported as absent.
func (s *Store) LiveLock(ctx context.Context, key string) (*LockRecord, error) {
	rec, err := s.GetLock(ctx, key)
	if err != nil || rec == nil {
		return nil, err
	}
	if IsLockStale(*rec, "", time.Now().UTC().Unix()) {
		return nil, nil
	}
	return rec, nil
}

// GetLock returns the lock record for key, stale or not (use LiveLock to ignore stale rows).
func (s *Store) GetLock(ctx context.Context, key string) (*LockRecord, error) {
	var rec LockRecord
	var slot sql.NullInt64
	var stamp, meta sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT lock_key, owner_type, pid, host, slot, stamp, acquired_at / 1000, heartbeat_at / 1000, ttl_seconds, metadata_json
		FROM locks WHERE lock_key = ?
	`, key).Scan(
		&rec.LockKey, &rec.OwnerType, &rec.PID, &rec.Host,
		&slot, &stamp, &rec.AcquiredAt, &rec.HeartbeatAt, &rec.TTLSeconds, &meta,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.Slot = int(slot.Int64)
	rec.Stamp = stamp.String
	rec.MetadataJSON = meta.String
	return &rec, nil
}

// ListLocks returns all rows in the locks table.
func (s *Store) ListLocks(ctx context.Context) ([]LockRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT lock_key, owner_type, pid, host, slot, stamp, acquired_at / 1000, heartbeat_at / 1000, ttl_seconds, metadata_json
		FROM locks ORDER BY acquired_at ASC, lock_key ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LockRecord
	for rows.Next() {
		var rec LockRecord
		var slot sql.NullInt64
		var stamp, meta sql.NullString
		if err := rows.Scan(
			&rec.LockKey, &rec.OwnerType, &rec.PID, &rec.Host,
			&slot, &stamp, &rec.AcquiredAt, &rec.HeartbeatAt, &rec.TTLSeconds, &meta,
		); err != nil {
			return nil, err
		}
		rec.Slot = int(slot.Int64)
		rec.Stamp = stamp.String
		rec.MetadataJSON = meta.String
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SetLockMetadata replaces the metadata of a lock this process holds (what a dispatcher says about
// itself while it runs). It changes nothing when pid does not hold key.
func (s *Store) SetLockMetadata(ctx context.Context, key string, pid int, metadata map[string]any) error {
	if pid <= 0 {
		pid = os.Getpid()
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	b, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE locks SET metadata_json = ? WHERE lock_key = ? AND pid = ?`, string(b), key, pid)
	return err
}
