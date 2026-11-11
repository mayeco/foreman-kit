package store

import (
	"context"
	"encoding/json"
	"strings"
)

// CountOpenChanges returns how many changes the pipeline has not finished with: the ones a worker
// builds, the ones ready for the merger or being merged, the blocked ones and the ones waiting to be
// rebuilt, together with those of queued, the names the backlog lists, that are neither integrated
// nor discarded. A change in both is counted once. It is what a cap on the queue is measured against:
// the cycle that would add to the queue and the daemon that would start that cycle count the same way.
//
// The caller reads the backlog because Git is its source. The registry mirrors it, and keeps an item
// as queued after it left the file without being integrated or abandoned; and a change the merger
// just integrated may still be in the file.
func (s *Store) CountOpenChanges(ctx context.Context, queued []string) (int, error) {
	names := make([]string, 0, len(queued))
	for _, name := range queued {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	list, err := json.Marshal(names)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.ReadDB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT change_name FROM v_change_state
			WHERE state IN (?, ?, ?, ?, ?)
			UNION
			SELECT q.value FROM json_each(?) q
			WHERE NOT EXISTS (
				SELECT 1 FROM changes c
				WHERE c.change_name = q.value AND c.backlog_status IN ('integrated', 'discarded'))
		)
	`, ChangeBuilding, ChangeReady, ChangeMerging, ChangeBlocked, ChangeRebuildRequested, string(list)).Scan(&n)
	return n, err
}
