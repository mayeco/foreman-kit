package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// The schema lives in numbered migrations: migrations/NNNN_name.sql, applied in order inside one
// transaction each, with PRAGMA user_version recording the last one applied. No other package runs
// DDL or SQL against the state database.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrLegacyDatabaseInUse is returned by Open when the state database was written by a version of
// foreman-kit that predates schema versions and one of its processes still holds a live lock on it.
var ErrLegacyDatabaseInUse = errors.New("store: the state database of an earlier foreman-kit is still in use")

// Store wraps the per-workspace SQLite database with separated writer (max 1) and reader pools.
type Store struct {
	db            *sql.DB
	rdb           *sql.DB
	dbPath        string
	workspaceID   string
	canonicalRoot string
	host          string
	clock         atomic.Int64
}

// WorkspaceState is the workspace as the orchestrator reads it: the singleton workspace row, plus the
// state derived from the open pause, the open merge attempt and the latest finished cycle.
type WorkspaceState struct {
	WorkspaceID    string `json:"workspace_id"`
	RootPath       string `json:"root_path"`
	Paused         bool   `json:"paused"`
	PauseReason    string `json:"pause_reason,omitempty"`
	PausedAt       string `json:"paused_at,omitempty"`
	MergerState    string `json:"merger_state"`
	MergerChange   string `json:"merger_change,omitempty"`
	MergerStopPID  int    `json:"merger_stop_pid,omitempty"`
	CycleStateJSON string `json:"cycle_state_json"`
	LastHealthJSON string `json:"last_health_json,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

// Open resolves and opens the SQLite database for the workspace containing dir.
func Open(dir string) (*Store, error) {
	dbPath, workspaceID, canonicalRoot, err := ResolveDBPath(dir)
	if err != nil {
		return nil, err
	}
	return OpenAt(dbPath, workspaceID, canonicalRoot)
}

// OpenAt opens a SQLite database at an explicit path with a serialized writer pool (max 1) and a
// concurrent reader pool, and brings its schema to the current version. A database written before
// schema versions existed is set aside (see setAsideLegacyDB) and a new one is created.
func OpenAt(dbPath, workspaceID, canonicalRoot string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	s, err := openAt(dbPath, workspaceID, canonicalRoot)
	if errors.Is(err, errLegacySchema) {
		if err := setAsideLegacyDB(dbPath); err != nil {
			return nil, err
		}
		return openAt(dbPath, workspaceID, canonicalRoot)
	}
	return s, err
}

func openAt(dbPath, workspaceID, canonicalRoot string) (*Store, error) {
	pragmas := "_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-64000)"
	// Every transaction of the writer starts with BEGIN IMMEDIATE: one that reads and then writes
	// never has to upgrade its lock, which another process's write in between would refuse.
	writeDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?%s&_txlock=immediate", dbPath, pragmas))
	if err != nil {
		return nil, err
	}
	// Writer pool serialized to 1 connection to eliminate intra-process SQLITE_BUSY contention.
	writeDB.SetMaxOpenConns(1)

	readDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?%s", dbPath, pragmas))
	if err != nil {
		_ = writeDB.Close()
		return nil, err
	}
	readConns := runtime.NumCPU()
	if readConns < 4 {
		readConns = 4
	}
	readDB.SetMaxOpenConns(readConns)

	host, _ := os.Hostname()
	s := &Store{
		db:            writeDB,
		rdb:           readDB,
		dbPath:        dbPath,
		workspaceID:   workspaceID,
		canonicalRoot: canonicalRoot,
		host:          host,
	}
	if err := s.migrate(context.Background()); err != nil {
		_ = readDB.Close()
		_ = writeDB.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var firstErr error
	if s.rdb != nil {
		if err := s.rdb.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.db != nil {
		if err := s.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Store) DB() *sql.DB { return s.db }
func (s *Store) ReadDB() *sql.DB {
	if s.rdb != nil {
		return s.rdb
	}
	return s.db
}
func (s *Store) DBPath() string        { return s.dbPath }
func (s *Store) WorkspaceID() string   { return s.workspaceID }
func (s *Store) CanonicalRoot() string { return s.canonicalRoot }

// nowMS returns the current Unix time in milliseconds, strictly greater than every value it returned
// before in this process. Rows this store stamps are therefore ordered by their stamps, which the
// quarantine state relies on (a rebuild request against the block resolved after it).
func (s *Store) nowMS() int64 {
	for {
		now := time.Now().UnixMilli()
		last := s.clock.Load()
		if now <= last {
			now = last + 1
		}
		if s.clock.CompareAndSwap(last, now) {
			return now
		}
	}
}

// msToRFC3339 renders a stored instant for the API: "" for none.
func msToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func nullMSToRFC3339(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return msToRFC3339(v.Int64)
}

// rfc3339ToMS parses an instant given by a caller: 0 when it is empty or not a time.
func rfc3339ToMS(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UnixMilli()
	}
	return 0
}

// msOrNow is rfc3339ToMS, falling back to the store's clock.
func (s *Store) msOrNow(v string) int64 {
	if ms := rfc3339ToMS(v); ms > 0 {
		return ms
	}
	return s.nowMS()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// inTx runs fn inside one writer transaction. fn must use tx for every statement: the writer pool
// has a single connection, which the transaction holds.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

var errLegacySchema = errors.New("store: legacy schema")

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		var version int
		if _, err := fmt.Sscanf(name, "%04d_", &version); err != nil || version <= 0 {
			return nil, fmt.Errorf("store: migration %q is not named NNNN_name.sql", name)
		}
		raw, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: name, sql: string(raw)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migration %q is out of sequence (expected version %d)", m.name, i+1)
		}
	}
	return out, nil
}

// SchemaVersion is the schema version this binary reads and writes.
func SchemaVersion() int {
	migrations, _ := loadMigrations()
	return len(migrations)
}

func (s *Store) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	for {
		applied, err := s.applyNextMigration(ctx, migrations)
		if err != nil {
			return err
		}
		if !applied {
			break
		}
	}
	now := s.nowMS()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workspace (id, workspace_id, root_path, updated_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			workspace_id = excluded.workspace_id,
			root_path = CASE WHEN excluded.root_path != '' THEN excluded.root_path ELSE workspace.root_path END
	`, s.workspaceID, s.canonicalRoot, now)
	return err
}

// applyNextMigration applies the first migration the database lacks, in one transaction that also
// re-reads the version: of two processes opening the same new database, the second finds the
// migration applied. It reports whether it applied one.
func (s *Store) applyNextMigration(ctx context.Context, migrations []migration) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return false, err
	}
	if version > len(migrations) {
		return false, fmt.Errorf("store: %s has schema version %d, newer than this foreman-kit (%d): update foreman-kit", s.dbPath, version, len(migrations))
	}
	if version == 0 {
		var legacy int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'workspace_state'`).Scan(&legacy); err != nil {
			return false, err
		}
		if legacy > 0 {
			return false, errLegacySchema
		}
	}
	if version == len(migrations) {
		return false, nil
	}
	next := migrations[version]
	if _, err := tx.ExecContext(ctx, next.sql); err != nil {
		return false, fmt.Errorf("store: migration %s: %w", next.name, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, next.version)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// setAsideLegacyDB renames a state database written before schema versions existed to
// <path>.legacy-<UTC stamp> (with its WAL and SHM files), so that Open creates a new one. Nothing is
// deleted. It refuses, with ErrLegacyDatabaseInUse, while a process of the earlier version still
// holds a live lock in that database: two versions would otherwise run on two databases, with no
// lock between them.
func setAsideLegacyDB(dbPath string) error {
	if !fileExists(dbPath) {
		return nil
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(15000)", dbPath))
	if err != nil {
		return err
	}
	var version, legacy int
	_ = db.QueryRow(`PRAGMA user_version`).Scan(&version)
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'workspace_state'`).Scan(&legacy)
	if version != 0 || legacy == 0 {
		// Another process set it aside, or migrated it, since this one looked.
		_ = db.Close()
		return nil
	}
	host, _ := os.Hostname()
	now := time.Now().UTC().Unix()
	if rows, err := db.Query(`SELECT lock_key, owner_type, pid, host, acquired_at, heartbeat_at, ttl_seconds FROM locks`); err == nil {
		var held []string
		for rows.Next() {
			var rec LockRecord
			if rows.Scan(&rec.LockKey, &rec.OwnerType, &rec.PID, &rec.Host, &rec.AcquiredAt, &rec.HeartbeatAt, &rec.TTLSeconds) != nil {
				continue
			}
			if rec.PID != os.Getpid() && !IsLockStale(rec, host, now) {
				held = append(held, fmt.Sprintf("%s (pid %d)", rec.LockKey, rec.PID))
			}
		}
		_ = rows.Close()
		if len(held) > 0 {
			_ = db.Close()
			return fmt.Errorf("%w: %s holds %s; stop it (foreman-kit daemon stop) and run again", ErrLegacyDatabaseInUse, dbPath, strings.Join(held, ", "))
		}
	}
	// Fold the write-ahead log into the file before it moves.
	_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	_ = db.Close()

	aside := dbPath + ".legacy-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(dbPath, aside); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if fileExists(dbPath + suffix) {
			_ = os.Rename(dbPath+suffix, aside+suffix)
		}
	}
	fmt.Fprintf(os.Stderr, "foreman-kit: the state database had the schema of an earlier version; it was kept as %s and a new one was created\n", aside)
	return nil
}

// GetWorkspaceState returns the workspace row with its derived state.
func (s *Store) GetWorkspaceState(ctx context.Context) (WorkspaceState, error) {
	var ws WorkspaceState
	var stopPID sql.NullInt64
	var lastSHA, lastHealth sql.NullString
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT workspace_id, root_path, merger_stop_pid, visual_qa_last_sha, last_health_json, updated_at
		FROM workspace WHERE id = 1
	`).Scan(&ws.WorkspaceID, &ws.RootPath, &stopPID, &lastSHA, &lastHealth, &updatedAt)
	if err != nil {
		return ws, err
	}
	ws.MergerStopPID = int(stopPID.Int64)
	ws.LastHealthJSON = lastHealth.String
	ws.UpdatedAt = msToRFC3339(updatedAt)

	var reason sql.NullString
	var pausedAt sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT reason, paused_at FROM v_workspace_paused LIMIT 1`).Scan(&reason, &pausedAt)
	if err == nil {
		ws.Paused = true
		ws.PauseReason = reason.String
		ws.PausedAt = nullMSToRFC3339(pausedAt)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ws, err
	}

	ws.MergerState = "IDLE"
	var state, change sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT state, change_name FROM v_merger LIMIT 1`).Scan(&state, &change)
	if err == nil {
		ws.MergerState = state.String
		ws.MergerChange = change.String
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ws, err
	}

	cycleState := map[string]any{}
	var stamp, outcome sql.NullString
	var finishedAt sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
		SELECT stamp, outcome, finished_at FROM cycles
		WHERE finished_at IS NOT NULL ORDER BY finished_at DESC, stamp DESC LIMIT 1
	`).Scan(&stamp, &outcome, &finishedAt)
	if err == nil {
		cycleState["last_cycle"] = map[string]any{
			"stamp":       stamp.String,
			"outcome":     outcome.String,
			"finished_at": nullMSToRFC3339(finishedAt),
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ws, err
	}
	if lastSHA.String != "" {
		cycleState["visual_qa_last_sha"] = lastSHA.String
	}
	b, _ := json.Marshal(cycleState)
	ws.CycleStateJSON = string(b)
	return ws, nil
}

// Pause opens a workspace pause recorded as put by `by` ("dispatcher", "merger", "portal" or "cli"),
// optionally over a change, and emits system.paused in the same transaction. A workspace that is
// already paused keeps the pause it has: it reports false and changes nothing.
func (s *Store) Pause(ctx context.Context, reason, by, change string) (bool, error) {
	paused := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspace_pauses WHERE resumed_at IS NULL`).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return nil
		}
		now := s.nowMS()
		if change != "" {
			if err := s.ensureChangeTx(ctx, tx, change, now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workspace_pauses (reason, paused_by, change_name, paused_at) VALUES (?, ?, ?, ?)
		`, reason, pauseActor(by), nullStr(change), now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workspace SET updated_at = ? WHERE id = 1`, now); err != nil {
			return err
		}
		paused = true
		return s.emitEventTx(ctx, tx, EventInput{
			Event:   "system.paused",
			Change:  change,
			Payload: map[string]any{"reason": reason, "by": pauseActor(by)},
		})
	})
	return paused, err
}

// Resume closes the open workspace pause as lifted by `by` and emits system.resumed in the same
// transaction. It reports whether the workspace was paused.
func (s *Store) Resume(ctx context.Context, by string) (bool, error) {
	resumed := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		res, err := tx.ExecContext(ctx, `
			UPDATE workspace_pauses SET resumed_at = ?, resumed_by = ? WHERE resumed_at IS NULL
		`, now, pauseActor(by))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workspace SET updated_at = ? WHERE id = 1`, now); err != nil {
			return err
		}
		resumed = true
		return s.emitEventTx(ctx, tx, EventInput{
			Event:   "system.resumed",
			Payload: map[string]any{"by": pauseActor(by)},
		})
	})
	return resumed, err
}

// SetPaused pauses or resumes the workspace on behalf of an operator at the command line. Callers
// that know who acts use Pause and Resume.
func (s *Store) SetPaused(ctx context.Context, paused bool, reason string) error {
	if paused {
		_, err := s.Pause(ctx, reason, "cli", "")
		return err
	}
	_, err := s.Resume(ctx, "cli")
	return err
}

func pauseActor(by string) string {
	switch by {
	case "dispatcher", "merger", "portal", "cli":
		return by
	}
	return "cli"
}

// SetMergerStopPID asks the merger running as pid to stop after the change it is integrating
// (0 withdraws the request).
func (s *Store) SetMergerStopPID(ctx context.Context, pid int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workspace SET merger_stop_pid = ?, updated_at = ? WHERE id = 1`, nullInt(int64(pid)), s.nowMS())
	return err
}

// SetVisualQALastSHA records the last integrated commit whose changes were queued for visual QA.
func (s *Store) SetVisualQALastSHA(ctx context.Context, sha string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workspace SET visual_qa_last_sha = ?, updated_at = ? WHERE id = 1`, nullStr(sha), s.nowMS())
	return err
}
