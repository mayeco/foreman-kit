package store

import (
	"context"
	"database/sql"
	"errors"
)

// ChangeDependency is a change another one declares it needs, or that declares it needs it, with the
// state it is in.
type ChangeDependency struct {
	ChangeName string `json:"change_name"`
	State      string `json:"state"`
}

// ChangeRecord is what is on record about one change now: the change as a list shows it, what it
// declares, and every row that is open for it. Its past is its story (ChangeStory).
type ChangeRecord struct {
	ChangeOverview
	// DependsOn are the changes it declares it needs; NeededBy, the ones that declare they need it.
	DependsOn []ChangeDependency `json:"depends_on"`
	NeededBy  []ChangeDependency `json:"needed_by"`
	// Locks are the locks it declares.
	Locks []string `json:"locks"`
	// Facts are the recorded facts that hold it back while it is queued.
	Facts []WaitFact `json:"facts"`
	// Block is the block in force, nil when the change is not blocked.
	Block *OpenBlock `json:"block,omitempty"`
	// Run is the worker run open now. Lost is that run when nothing holds the lease of the change:
	// the worker that was building it is gone.
	Run  *ChangeRun `json:"run,omitempty"`
	Lost *LostRun   `json:"lost,omitempty"`
	// Manifest is the manifest waiting for the merger; Merge, the merge attempt in progress.
	Manifest *ManifestRecord `json:"manifest,omitempty"`
	Merge    *MergeAttempt   `json:"merge,omitempty"`
	// Rebuild is the request to build the change again that no worker has taken yet.
	Rebuild *RebuildRequest `json:"rebuild,omitempty"`
	// Idle is the agent of its worker when it shows no activity now.
	Idle *IdleAgent `json:"idle,omitempty"`
}

// Change returns what is on record about the change name now, or nil when the kit has no record of a
// change of that name.
func (v *View) Change(ctx context.Context, name string) (*ChangeRecord, error) {
	overview, err := scanChangeOverview(v.q.QueryRowContext(ctx, `
		SELECT `+changeOverviewColumns+` FROM v_change_overview o WHERE o.change_name = ?
	`, name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec := &ChangeRecord{
		ChangeOverview: overview,
		DependsOn:      []ChangeDependency{},
		NeededBy:       []ChangeDependency{},
		Locks:          []string{},
		Facts:          []WaitFact{},
	}

	dependencies := func(into *[]ChangeDependency, query string) error {
		return v.each(ctx, func(scan func(dest ...any) error) error {
			var d ChangeDependency
			if err := scan(&d.ChangeName, &d.State); err != nil {
				return err
			}
			*into = append(*into, d)
			return nil
		}, query, name)
	}
	if err := dependencies(&rec.DependsOn, `
		SELECT d.depends_on, s.state FROM change_dependencies d JOIN v_change_state s ON s.change_name = d.depends_on
		WHERE d.change_name = ? ORDER BY d.depends_on
	`); err != nil {
		return nil, err
	}
	if err := dependencies(&rec.NeededBy, `
		SELECT d.change_name, s.state FROM change_dependencies d JOIN v_change_state s ON s.change_name = d.change_name
		WHERE d.depends_on = ? ORDER BY d.change_name
	`); err != nil {
		return nil, err
	}
	if err := v.each(ctx, func(scan func(dest ...any) error) error {
		var lock string
		if err := scan(&lock); err != nil {
			return err
		}
		rec.Locks = append(rec.Locks, lock)
		return nil
	}, `SELECT lock_name FROM change_locks WHERE change_name = ? ORDER BY lock_name`, name); err != nil {
		return nil, err
	}
	if err := v.each(ctx, func(scan func(dest ...any) error) error {
		var change string
		f, err := scanWaitFact(scan, &change)
		if err == nil {
			rec.Facts = append(rec.Facts, f)
		}
		return err
	}, `
		SELECT change_name, kind, other_change, other_state, lock_name, reason, since_at, until_at
		FROM v_wait_facts WHERE change_name = ? ORDER BY kind, other_change, lock_name, reason
	`, name); err != nil {
		return nil, err
	}

	block, err := scanBlockRecord(v.q.QueryRowContext(ctx, `
		SELECT `+blockRecordColumns+` FROM change_blocks WHERE change_name = ? AND resolved_at IS NULL
	`, name).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		open := OpenBlock{BlockRecord: block}
		st, err := v.s.loadQuarantineTx(ctx, v.q, name)
		if err != nil {
			return nil, err
		}
		if q := st.record(); q != nil {
			open.Quarantine = *q
		}
		rec.Block = &open
	}

	run, err := scanChangeRun(v.q.QueryRowContext(ctx, `
		SELECT `+changeRunColumns+` FROM change_runs r WHERE r.change_name = ? AND r.finished_at IS NULL ORDER BY r.id DESC LIMIT 1
	`, name).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		rec.Run = &run
		lost, err := v.lostRuns(ctx, name)
		if err != nil {
			return nil, err
		}
		for i := range lost {
			if lost[i].ID == run.ID {
				rec.Lost = &lost[i]
			}
		}
	}

	var manifestID sql.NullInt64
	err = v.q.QueryRowContext(ctx, `
		SELECT id FROM ready_manifests WHERE change_name = ? AND closed_at IS NULL ORDER BY id DESC LIMIT 1
	`, name).Scan(&manifestID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if manifestID.Valid {
		manifests, err := v.loadManifestRecords(ctx, []any{manifestID.Int64})
		if err != nil {
			return nil, err
		}
		rec.Manifest = manifests[manifestID.Int64]
	}

	merge, err := scanMergeAttempt(v.q.QueryRowContext(ctx, `
		SELECT `+mergeAttemptColumns+` FROM merge_attempts m JOIN ready_manifests r ON r.id = m.manifest_id
		WHERE r.change_name = ? AND m.finished_at IS NULL ORDER BY m.id DESC LIMIT 1
	`, name).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		rec.Merge = &merge
	}

	rebuild, err := scanRebuildRequest(v.q.QueryRowContext(ctx, `
		SELECT `+rebuildRequestColumns+` FROM rebuild_requests WHERE change_name = ? AND consumed_at IS NULL ORDER BY id DESC LIMIT 1
	`, name).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		rec.Rebuild = &rebuild
	}

	idle, err := v.ListIdleAgents(ctx)
	if err != nil {
		return nil, err
	}
	for i := range idle {
		if idle[i].Kind == "worker" && idle[i].ChangeName == name {
			rec.Idle = &idle[i]
			break
		}
	}
	return rec, nil
}
