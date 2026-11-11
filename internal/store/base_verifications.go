package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// Statuses of a base verification (merge.outside_commits: verify).
const (
	// BaseVerdictPassed: the check passed on the base alone.
	BaseVerdictPassed = "passed"
	// BaseVerdictFailed: the check failed on the base alone, so the base is broken.
	BaseVerdictFailed = "failed"
	// BaseVerdictAccepted: the check failed on the base alone and a human accepted that base.
	BaseVerdictAccepted = "accepted"
)

// BaseVerification is the verdict of one check of a child repository's base alone: the commit the
// merger rebases a change onto, when it holds commits pushed outside the pipeline (a hotfix). The check
// is "regression" (the repository's whole regression set) or "gate:<key>" (one fast gate). A verdict is
// computed at most once per (RepoPath, BaseSHA, CheckName).
type BaseVerification struct {
	RepoPath       string `json:"repo"`
	BaseSHA        string `json:"sha"`
	CheckName      string `json:"check"`
	Status         string `json:"status"`
	ExitCode       int    `json:"exit_code"`
	Excerpt        string `json:"excerpt,omitempty"`
	PointerSHA     string `json:"pointer,omitempty"`
	OutsideCommits int    `json:"outside_commits"`
	CreatedAt      string `json:"created_at"`
	AcceptedBy     string `json:"accepted_by,omitempty"`
	AcceptedAt     string `json:"accepted_at,omitempty"`
	// ResolvedAt is set on a failed verdict once the repository's base moved off its sha.
	ResolvedAt string `json:"resolved_at,omitempty"`
}

// BrokenBase is a child repository whose current base fails a check on its own: one or more failed,
// unaccepted, unresolved verdicts for the same sha.
type BrokenBase struct {
	Repo           string   `json:"repo"`
	SHA            string   `json:"sha"`
	Since          string   `json:"since"`
	FailedChecks   []string `json:"failed_checks"`
	Pointer        string   `json:"pointer,omitempty"`
	OutsideCommits int      `json:"outside_commits"`
	Excerpt        string   `json:"excerpt,omitempty"`
	// LastFailedAt is when the newest of its failed verdicts was computed.
	LastFailedAt string `json:"last_failed_at"`
	// RecheckedAt is when this base had its one automatic recheck (RecheckBrokenBase), "" before it.
	RecheckedAt string `json:"rechecked_at,omitempty"`
}

// BaseCheckLabel names a check for people: "regression" stays, "gate:test" becomes "test gate".
func BaseCheckLabel(check string) string {
	if key, ok := strings.CutPrefix(check, "gate:"); ok {
		return key + " gate"
	}
	return check
}

// ChecksText names the failed checks for people: "regression", "test gate", "regression and test gate".
func (b BrokenBase) ChecksText() string {
	labels := make([]string, 0, len(b.FailedChecks))
	for _, c := range b.FailedChecks {
		labels = append(labels, BaseCheckLabel(c))
	}
	switch len(labels) {
	case 0:
		return "a check"
	case 1:
		return labels[0]
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
}

// ChecksVerb is "fails" for one failed check and "fail" for several.
func (b BrokenBase) ChecksVerb() string {
	if len(b.FailedChecks) > 1 {
		return "fail"
	}
	return "fails"
}

const baseVerificationColumns = `repo_path, base_sha, check_name, status, exit_code, excerpt, pointer_sha, outside_commits, created_at, accepted_by, accepted_at, resolved_at`

// A verdict that was computed again (see RecheckBrokenBase, RecordBaseVerification) stays in the table
// with superseded_at set. Every reader below sees only the verdicts in force.
const baseVerificationCurrent = ` AND superseded_at IS NULL`

func scanBaseVerification(scan func(dest ...any) error) (BaseVerification, error) {
	var v BaseVerification
	var excerpt, pointer, acceptedBy sql.NullString
	var createdAt int64
	var acceptedAt, resolvedAt sql.NullInt64
	err := scan(&v.RepoPath, &v.BaseSHA, &v.CheckName, &v.Status, &v.ExitCode, &excerpt, &pointer, &v.OutsideCommits, &createdAt, &acceptedBy, &acceptedAt, &resolvedAt)
	v.Excerpt = excerpt.String
	v.PointerSHA = pointer.String
	v.CreatedAt = msToRFC3339(createdAt)
	v.AcceptedBy = acceptedBy.String
	v.AcceptedAt = nullMSToRFC3339(acceptedAt)
	v.ResolvedAt = nullMSToRFC3339(resolvedAt)
	return v, err
}

// GetBaseVerification returns the verdict in force for (repo, sha, check), or nil when there is none.
func (s *Store) GetBaseVerification(ctx context.Context, repo, sha, check string) (*BaseVerification, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+baseVerificationColumns+` FROM base_verifications WHERE repo_path = ? AND base_sha = ? AND check_name = ?`+baseVerificationCurrent, repo, sha, check)
	v, err := scanBaseVerification(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListBaseVerifications returns every verdict in force of repo on sha, oldest first (every verdict
// in force of repo when sha is empty).
func (s *Store) ListBaseVerifications(ctx context.Context, repo, sha string) ([]BaseVerification, error) {
	query := `SELECT ` + baseVerificationColumns + ` FROM base_verifications WHERE repo_path = ?` + baseVerificationCurrent
	args := []any{repo}
	if sha != "" {
		query += ` AND base_sha = ?`
		args = append(args, sha)
	}
	rows, err := s.ReadDB().QueryContext(ctx, query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BaseVerification
	for rows.Next() {
		v, err := scanBaseVerification(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RecordBaseVerification stores a verdict unless one is in force for its (repo, sha, check): a
// verdict is computed once, so a failed or accepted one stays as it is. The one exception is a passed
// verdict, which a failed one supersedes: a gate rerun with another change's files found the base
// broken. It reports whether the verdict was written. The gate round and the merge attempt in the
// scope of ctx are recorded as where it was computed.
func (s *Store) RecordBaseVerification(ctx context.Context, v BaseVerification) (bool, error) {
	sc := ScopeFrom(ctx)
	wrote := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		created := rfc3339ToMS(v.CreatedAt)
		if created == 0 {
			created = now
		}
		repo, err := s.ensureRepoTx(ctx, tx, v.RepoPath, "")
		if err != nil {
			return err
		}
		var currentID int64
		var currentStatus string
		err = tx.QueryRowContext(ctx, `
			SELECT id, status FROM base_verifications
			WHERE repo_path = ? AND base_sha = ? AND check_name = ?`+baseVerificationCurrent, repo, v.BaseSHA, v.CheckName).Scan(&currentID, &currentStatus)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if currentID != 0 {
			if currentStatus != BaseVerdictPassed || v.Status != BaseVerdictFailed {
				return nil
			}
			if _, err := tx.ExecContext(ctx, `UPDATE base_verifications SET superseded_at = ? WHERE id = ?`, now, currentID); err != nil {
				return err
			}
		}
		var lastRun, rechecked int
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(run_no), 0), COALESCE(SUM(status = 'failed' AND superseded_at IS NOT NULL), 0)
			FROM base_verifications WHERE repo_path = ? AND base_sha = ? AND check_name = ?
		`, repo, v.BaseSHA, v.CheckName).Scan(&lastRun, &rechecked); err != nil {
			return err
		}
		origin := "merge"
		if rechecked > 0 {
			origin = "recheck"
		}
		var roundID, mergeID any
		if sc.GateRoundID > 0 {
			roundID = sc.GateRoundID
		}
		if sc.MergeAttemptID > 0 {
			mergeID = sc.MergeAttemptID
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO base_verifications (repo_path, base_sha, check_name, run_no, origin, status, exit_code, excerpt,
			                                pointer_sha, outside_commits, gate_round_id, merge_attempt_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, repo, v.BaseSHA, v.CheckName, lastRun+1, origin, v.Status, v.ExitCode, nullStr(v.Excerpt),
			nullStr(v.PointerSHA), v.OutsideCommits, roundID, mergeID, created)
		if err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return wrote, err
}

// AcceptBaseVerifications marks every failed verdict of repo on sha accepted by `by` at `at`: the
// merger then integrates that repository's changes on that base with their own tests only. It
// returns how many verdicts it accepted.
func (s *Store) AcceptBaseVerifications(ctx context.Context, repo, sha, by string, at time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE base_verifications SET status = ?, accepted_by = ?, accepted_at = ?
		WHERE repo_path = ? AND base_sha = ? AND status = ?`+baseVerificationCurrent,
		BaseVerdictAccepted, by, at.UTC().UnixMilli(), repo, sha, BaseVerdictFailed)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ResolveBrokenBases marks resolved the failed verdicts of repo on every sha other than keepSHA: the
// repository's base moved off them. It returns the shas it resolved.
func (s *Store) ResolveBrokenBases(ctx context.Context, repo, keepSHA string, at time.Time) ([]string, error) {
	var shas []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT DISTINCT base_sha FROM base_verifications
			WHERE repo_path = ? AND base_sha != ? AND status = ? AND resolved_at IS NULL`+baseVerificationCurrent,
			repo, keepSHA, BaseVerdictFailed)
		if err != nil {
			return err
		}
		for rows.Next() {
			var sha string
			if err := rows.Scan(&sha); err != nil {
				rows.Close()
				return err
			}
			shas = append(shas, sha)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(shas) == 0 {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE base_verifications SET resolved_at = ?
			WHERE repo_path = ? AND base_sha != ? AND status = ? AND resolved_at IS NULL`+baseVerificationCurrent,
			at.UTC().UnixMilli(), repo, keepSHA, BaseVerdictFailed)
		return err
	})
	sort.Strings(shas)
	return shas, err
}

// ExpireAcceptedBases marks resolved the accepted verdicts of repo on every sha other than keepSHA:
// commits were pushed to the repository outside the pipeline since, so what a human accepted no longer
// describes its base and is not carried to it. It returns how many verdicts it expired.
func (s *Store) ExpireAcceptedBases(ctx context.Context, repo, keepSHA string, at time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE base_verifications SET resolved_at = ?
		WHERE repo_path = ? AND base_sha != ? AND status = ? AND resolved_at IS NULL`+baseVerificationCurrent,
		at.UTC().UnixMilli(), repo, keepSHA, BaseVerdictAccepted)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ReopenBrokenBase clears resolved_at on the failed verdicts of repo on sha: that base is current again.
func (s *Store) ReopenBrokenBase(ctx context.Context, repo, sha string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE base_verifications SET resolved_at = NULL
		WHERE repo_path = ? AND base_sha = ? AND status = ? AND resolved_at IS NOT NULL`+baseVerificationCurrent,
		repo, sha, BaseVerdictFailed)
	return err
}

// ListBrokenBases returns every repository whose base is broken now (failed verdicts neither accepted
// nor resolved), one entry per repository and sha, sorted by repository.
func (s *Store) ListBrokenBases(ctx context.Context) ([]BrokenBase, error) {
	return listBrokenBases(ctx, s.ReadDB())
}

func listBrokenBases(ctx context.Context, q reader) ([]BrokenBase, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+baseVerificationColumns+` FROM base_verifications
		WHERE status = ? AND resolved_at IS NULL`+baseVerificationCurrent+` ORDER BY repo_path, created_at, id`, BaseVerdictFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BrokenBase
	index := make(map[string]int)
	for rows.Next() {
		v, err := scanBaseVerification(rows.Scan)
		if err != nil {
			return nil, err
		}
		key := v.RepoPath + "\x00" + v.BaseSHA
		i, ok := index[key]
		if !ok {
			index[key] = len(out)
			out = append(out, BrokenBase{
				Repo:           v.RepoPath,
				SHA:            v.BaseSHA,
				Since:          v.CreatedAt,
				Pointer:        v.PointerSHA,
				OutsideCommits: v.OutsideCommits,
				Excerpt:        v.Excerpt,
			})
			i = len(out) - 1
		}
		out[i].FailedChecks = append(out[i].FailedChecks, v.CheckName)
		if v.CreatedAt > out[i].LastFailedAt {
			out[i].LastFailedAt = v.CreatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		at, err := baseRecheckedAt(ctx, q, out[i].Repo, out[i].SHA)
		if err != nil {
			return nil, err
		}
		out[i].RecheckedAt = at
	}
	return out, nil
}

// RecheckBrokenBase spends the one automatic recheck of repository repo's broken base sha: it marks
// the base's failed verdicts superseded, so the merger computes them again on its next attempt (a
// check can fail on the base alone for a reason that heals: a service that was down, a flaky test).
// The superseded verdicts stay as the record of what failed and of the recheck. It reports false,
// changing nothing, when that base was already rechecked or has no failed verdict in force. Passed
// and accepted verdicts stay.
func (s *Store) RecheckBrokenBase(ctx context.Context, repo, sha string, at time.Time) (bool, error) {
	rechecked := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var already int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM base_verifications
			WHERE repo_path = ? AND base_sha = ? AND status = ? AND superseded_at IS NOT NULL
		`, repo, sha, BaseVerdictFailed).Scan(&already); err != nil {
			return err
		}
		if already > 0 {
			return nil
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE base_verifications SET superseded_at = ?
			WHERE repo_path = ? AND base_sha = ? AND status = ?`+baseVerificationCurrent,
			at.UTC().UnixMilli(), repo, sha, BaseVerdictFailed)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		rechecked = n > 0
		return nil
	})
	return rechecked, err
}

// BaseRecheckedAt returns when repository repo's base sha had its automatic recheck, or "" when it has
// not had it.
func (s *Store) BaseRecheckedAt(ctx context.Context, repo, sha string) (string, error) {
	return baseRecheckedAt(ctx, s.ReadDB(), repo, sha)
}

func baseRecheckedAt(ctx context.Context, q reader, repo, sha string) (string, error) {
	var at sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT MAX(superseded_at) FROM base_verifications
		WHERE repo_path = ? AND base_sha = ? AND status = ? AND superseded_at IS NOT NULL
	`, repo, sha, BaseVerdictFailed).Scan(&at)
	if err != nil {
		return "", err
	}
	return nullMSToRFC3339(at), nil
}
