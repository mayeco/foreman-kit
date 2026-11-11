package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

// querier is what *sql.DB, *sql.Tx and *sql.Conn share.
type querier interface {
	reader
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// reader is the part of a querier that reads: what a read needs, and all a View gives.
type reader interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// The catalog tables (changes, engines, repos) are what every other table points at. A row is
// registered the first time its name appears anywhere, in the transaction that writes the row
// naming it, so a foreign key never fails for a name the kit is only now meeting.

func (s *Store) ensureChangeTx(ctx context.Context, q querier, change string, now int64) error {
	if change == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO changes (change_name, backlog_status, first_seen_at, updated_at)
		VALUES (?, 'unknown', ?, ?)
		ON CONFLICT(change_name) DO NOTHING
	`, change, now, now)
	return err
}

// normalizeEngine returns the single engine name in v: the first of a comma-separated list.
func normalizeEngine(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

func (s *Store) ensureEngineTx(ctx context.Context, q querier, engine string) error {
	if engine == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, `INSERT INTO engines (engine) VALUES (?) ON CONFLICT(engine) DO NOTHING`, engine)
	return err
}

// UmbrellaRepoPath is the repository path of the umbrella checkout itself, where the OpenSpec layer
// is validated. Its gates are labelled "openspec".
const UmbrellaRepoPath = "."

// ensureRepoTx registers a repository and returns its path, the key other tables use. A repository
// known only by its short name (the label of a gate) is looked up by it; one seen for the first
// time by name alone is registered with that name as its path.
func (s *Store) ensureRepoTx(ctx context.Context, q querier, path, name string) (string, error) {
	path = strings.TrimSpace(filepath.ToSlash(path))
	name = strings.TrimSpace(name)
	if path == "" && name == "" {
		return "", nil
	}
	if path == "" {
		if name == "openspec" {
			path = UmbrellaRepoPath
		} else {
			var found string
			err := q.QueryRowContext(ctx, `SELECT repo_path FROM repos WHERE name = ?`, name).Scan(&found)
			if err == nil {
				return found, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return "", err
			}
			path = name
		}
	}
	if name == "" {
		if path == UmbrellaRepoPath {
			name = "openspec"
		} else {
			name = filepath.Base(path)
		}
	}
	var existing string
	err := q.QueryRowContext(ctx, `SELECT repo_path FROM repos WHERE repo_path = ?`, path).Scan(&existing)
	if err == nil {
		return path, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	res, err := q.ExecContext(ctx, `INSERT INTO repos (repo_path, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, path, name)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Another repository already has this short name: this one is named by its path.
		if _, err := q.ExecContext(ctx, `INSERT INTO repos (repo_path, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, path, path); err != nil {
			return "", err
		}
	}
	return path, nil
}

// EnsureRepo registers a child repository by its path under the umbrella (for example "repos/api").
func (s *Store) EnsureRepo(ctx context.Context, path string) error {
	_, err := s.ensureRepoTx(ctx, s.db, path, "")
	return err
}

// SetChangeFirstSeen records when the dispatcher first saw change in the backlog.
func (s *Store) SetChangeFirstSeen(ctx context.Context, change string, at time.Time) error {
	if change == "" {
		return nil
	}
	ms := at.UTC().UnixMilli()
	if at.IsZero() {
		ms = s.nowMS()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO changes (change_name, backlog_status, first_seen_at, updated_at)
		VALUES (?, 'unknown', ?, ?)
		ON CONFLICT(change_name) DO UPDATE SET first_seen_at = excluded.first_seen_at
	`, change, ms, s.nowMS())
	return err
}

// ChangesFirstSeen returns, for every registered change, when it was first seen (RFC 3339).
func (s *Store) ChangesFirstSeen(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT change_name, first_seen_at FROM changes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[name] = msToRFC3339(at)
	}
	return out, rows.Err()
}

// MarkChangeIntegrated records that a change was integrated as commit (empty when the merger found it
// integrated already): the registry says so at once, without waiting for the next mirror of Git.
func (s *Store) MarkChangeIntegrated(ctx context.Context, change, commit string) error {
	if change == "" {
		return nil
	}
	now := s.nowMS()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO changes (change_name, backlog_status, first_seen_at, integrated_at, integrated_commit, updated_at)
		VALUES (?, 'integrated', ?, ?, ?, ?)
		ON CONFLICT(change_name) DO UPDATE SET
			backlog_status = 'integrated',
			integrated_at = COALESCE(changes.integrated_at, excluded.integrated_at),
			integrated_commit = COALESCE(excluded.integrated_commit, changes.integrated_commit),
			updated_at = excluded.updated_at
	`, change, now, now, nullStr(commit), now)
	return err
}

// MarkChangeDiscarded records that a change was abandoned: it leaves the backlog for good.
func (s *Store) MarkChangeDiscarded(ctx context.Context, change string) error {
	if change == "" {
		return nil
	}
	now := s.nowMS()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO changes (change_name, backlog_status, first_seen_at, discarded_at, updated_at)
		VALUES (?, 'discarded', ?, ?, ?)
		ON CONFLICT(change_name) DO UPDATE SET
			backlog_status = 'discarded',
			discarded_at = excluded.discarded_at,
			updated_at = excluded.updated_at
	`, change, now, now, now)
	return err
}

// ChangeState returns the derived state of a change (v_change_state): "" when it is not registered.
func (s *Store) ChangeState(ctx context.Context, change string) (string, error) {
	var state string
	err := s.ReadDB().QueryRowContext(ctx, `SELECT state FROM v_change_state WHERE change_name = ?`, change).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return state, err
}
