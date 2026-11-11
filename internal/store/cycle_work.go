package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
)

// CycleWork is what a cycle added to the files it publishes: the backlog items it queued and the
// proposals it made, each as its file lists it.
type CycleWork struct {
	Queued   []map[string]any
	Proposed []map[string]any
}

// MirroredListing is what the backlog and the proposals under a checkout listed at one moment, by
// name. A list that was not read whole is not known, and nothing is said to have been added to it:
// what it left out may have been there.
type MirroredListing struct {
	backlog, proposals           map[string]bool
	backlogKnown, proposalsKnown bool
}

// ListMirrored reads what the backlog and the proposals under dir list now.
func ListMirrored(dir string) MirroredListing {
	var l MirroredListing
	l.backlog, l.backlogKnown = listedNames(mirroredBacklog(dir), func(item map[string]any) string { return itemName(item, "change_name") })
	l.proposals, l.proposalsKnown = listedNames(mirroredProposals(dir), proposalID)
	return l
}

// AddedSince returns what the backlog and the proposals under dir list now that l did not: what was
// added to them since l was read, each item once.
func (l MirroredListing) AddedSince(dir string) CycleWork {
	var work CycleWork
	if l.backlogKnown {
		items, _ := readJSONMapSliceWhole(mirroredBacklog(dir))
		work.Queued = itemsNotIn(items, l.backlog, func(item map[string]any) string { return itemName(item, "change_name") })
	}
	if l.proposalsKnown {
		items, _ := readJSONMapSliceWhole(mirroredProposals(dir))
		work.Proposed = itemsNotIn(items, l.proposals, proposalID)
	}
	return work
}

func mirroredBacklog(dir string) string {
	return filepath.Join(dir, ".foreman-kit", "agent_backlog.json")
}

func mirroredProposals(dir string) string {
	return filepath.Join(dir, ".foreman-kit", "agent_proposals.json")
}

// listedNames returns the names the JSON list at path gives its items, and whether they are all of
// them: a file that is not there lists nothing, and one that is not a whole JSON list is not known.
func listedNames(path string, name func(map[string]any) string) (map[string]bool, bool) {
	names := map[string]bool{}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return names, true
	}
	items, whole := readJSONMapSliceWhole(path)
	for _, item := range items {
		if n := name(item); n != "" {
			names[n] = true
		}
	}
	return names, whole
}

// itemsNotIn returns the items whose name before does not hold, the first of each name.
func itemsNotIn(items []map[string]any, before map[string]bool, name func(map[string]any) string) []map[string]any {
	var added []map[string]any
	seen := map[string]bool{}
	for _, item := range items {
		n := name(item)
		if n == "" || before[n] || seen[n] {
			continue
		}
		seen[n] = true
		added = append(added, item)
	}
	return added
}

// ProposalUnknown is the status of a proposal the kit has on record that the proposals file of the
// main checkout does not list: a cycle made it, and what the cycle recorded has not reached that
// checkout. It waits for no decision, because a decision is written in that file. It is to a proposal
// what "unknown" is to a change the backlog does not list.
const ProposalUnknown = "unknown"

// RecordCycleWork says in the registry that the cycle stamp queued those changes and made those
// proposals. It puts no change in the queue and no proposal before a person: the queue and the
// proposals that wait for a decision are what the files of the main checkout list (SyncJSONToSQLite),
// and an item that checkout does not list yet is on record, unknown, until it does: a change it has
// never listed has no changes.listed_at. A change that is integrated or discarded, and a proposal
// that is integrated, keep what they say: the item that names one again is not that work.
func (s *Store) RecordCycleWork(ctx context.Context, stamp string, work CycleWork) error {
	if stamp == "" || len(work.Queued)+len(work.Proposed) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()
		for _, item := range work.Queued {
			name := itemName(item, "change_name")
			if name == "" {
				continue
			}
			extraJSON, _ := json.Marshal(item)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO changes (
					change_name, backlog_status, priority, impact_score, kind, source, strategy, description,
					first_seen_at, cycle_stamp, extra_json, updated_at
				) VALUES (?, 'unknown', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(change_name) DO UPDATE SET
					cycle_stamp = CASE
						WHEN changes.backlog_status IN ('integrated', 'discarded') THEN changes.cycle_stamp
						ELSE excluded.cycle_stamp
					END,
					updated_at = excluded.updated_at
			`, name, defaultStr(item["priority"], "normal"), numberOrNil(item["impact_score"]),
				nullStr(defaultStr(item["kind"], "")), nullStr(defaultStr(item["source"], "")), nullStr(defaultStr(item["strategy"], "")),
				defaultStr(item["description"], defaultStr(item["why"], "")), now, stamp, string(extraJSON), now); err != nil {
				return err
			}
		}
		for _, item := range work.Proposed {
			id := proposalID(item)
			if id == "" {
				continue
			}
			// A proposal the registry has not met is one the main checkout does not list. One it has
			// met keeps its status: that is the mirror's to say.
			var met int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proposals WHERE proposal_id = ?`, id).Scan(&met); err != nil {
				return err
			}
			if met == 0 {
				if err := s.mirrorProposalTx(ctx, tx, item, ProposalUnknown, now); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE proposals SET cycle_stamp = ?, updated_at = ?
				WHERE proposal_id = ? AND status != 'integrated'
			`, stamp, now, id); err != nil {
				return err
			}
		}
		return nil
	})
}
