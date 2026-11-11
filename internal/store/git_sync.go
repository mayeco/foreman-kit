package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SyncJSONToSQLite mirrors .foreman-kit/agent_backlog.json, .foreman-kit/agent_history.json (with its rotated
// files) and .foreman-kit/agent_proposals.json from dir into the changes and proposals tables. Git stays
// the source of the backlog; the tables are its registry, which every other table points at. The
// backlog status follows the hierarchy integrated / discarded > queued, and a queued change the
// backlog no longer lists goes back to unknown: the kit has records of it, and it is in no queue.
//
// dir is the main checkout: the queue is what that checkout lists, and so are the proposals that wait
// for a decision. Which cycle queued a change or made a proposal is not read from the files; the cycle
// says it (RecordCycleWork), and the mirror keeps it. What a cycle put on record before this checkout
// listed it (a change in no queue, a proposal that is ProposalUnknown) takes here the status its file
// gives it. changes.listed_at is the hour this checkout first listed a change: a change a cycle
// queued that has none was never in its backlog, and one somebody took out of the backlog keeps it.
func (s *Store) SyncJSONToSQLite(ctx context.Context, dir string) error {
	if dir == "" {
		dir = s.canonicalRoot
	}
	history := readAllHistoryMapSlice(dir)
	backlog, backlogWhole := readJSONMapSliceWhole(filepath.Join(dir, ".foreman-kit", "agent_backlog.json"))
	proposals := readJSONMapSlice(filepath.Join(dir, ".foreman-kit", "agent_proposals.json"))

	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMS()

		// 1. History: integrated or discarded, which nothing downgrades.
		for _, item := range history {
			name := itemName(item, "change_name")
			if name == "" {
				continue
			}
			status := "integrated"
			if defaultStr(item["status"], "") == "discarded" {
				status = "discarded"
			}
			endedAt := rfc3339ToMS(defaultStr(item["integrated_at"], defaultStr(item["archived_at"], "")))
			if endedAt == 0 {
				endedAt = now
			}
			var integratedAt, discardedAt any
			if status == "integrated" {
				integratedAt = endedAt
			} else {
				discardedAt = endedAt
			}
			extraJSON, _ := json.Marshal(item)
			_, _ = tx.ExecContext(ctx, `
				INSERT INTO changes (
					change_name, backlog_status, priority, impact_score, kind, source, strategy, description,
					first_seen_at, integrated_at, integrated_commit, discarded_at, extra_json, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(change_name) DO UPDATE SET
					backlog_status = excluded.backlog_status,
					integrated_at = COALESCE(excluded.integrated_at, changes.integrated_at),
					integrated_commit = COALESCE(excluded.integrated_commit, changes.integrated_commit),
					discarded_at = COALESCE(excluded.discarded_at, changes.discarded_at),
					extra_json = excluded.extra_json,
					updated_at = excluded.updated_at
			`, name, status, defaultStr(item["priority"], "normal"), numberOrNil(item["impact_score"]),
				nullStr(defaultStr(item["kind"], "")), nullStr(defaultStr(item["source"], "")), nullStr(defaultStr(item["strategy"], "")),
				defaultStr(item["description"], ""), now, integratedAt, nullStr(defaultStr(item["commit"], "")), discardedAt, string(extraJSON), now)
			s.syncChangeListsTx(ctx, tx, name, item, now)
		}

		// 2. Backlog: queued, unless the change is already integrated or discarded. The first time
		// this backlog lists a change is on record (listed_at), and stays when the change leaves it.
		for _, item := range backlog {
			name := itemName(item, "change_name")
			if name == "" {
				continue
			}
			var queuedAt any
			if q := rfc3339ToMS(defaultStr(item["queued_at"], "")); q > 0 {
				queuedAt = q
			} else {
				queuedAt = now
			}
			extraJSON, _ := json.Marshal(item)
			_, _ = tx.ExecContext(ctx, `
				INSERT INTO changes (
					change_name, backlog_status, priority, impact_score, kind, source, strategy, description,
					first_seen_at, queued_at, listed_at, extra_json, updated_at
				) VALUES (?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(change_name) DO UPDATE SET
					backlog_status = CASE WHEN changes.backlog_status IN ('integrated', 'discarded') THEN changes.backlog_status ELSE 'queued' END,
					listed_at = COALESCE(changes.listed_at, excluded.listed_at),
					priority = excluded.priority,
					impact_score = COALESCE(excluded.impact_score, changes.impact_score),
					kind = COALESCE(excluded.kind, changes.kind),
					source = COALESCE(excluded.source, changes.source),
					strategy = COALESCE(excluded.strategy, changes.strategy),
					description = CASE WHEN excluded.description != '' THEN excluded.description ELSE changes.description END,
					queued_at = COALESCE(changes.queued_at, excluded.queued_at),
					extra_json = excluded.extra_json,
					updated_at = excluded.updated_at
			`, name, defaultStr(item["priority"], "normal"), numberOrNil(item["impact_score"]),
				nullStr(defaultStr(item["kind"], "")), nullStr(defaultStr(item["source"], "")), nullStr(defaultStr(item["strategy"], "")),
				defaultStr(item["description"], defaultStr(item["why"], "")), now, queuedAt, now, string(extraJSON), now)
			s.syncChangeListsTx(ctx, tx, name, item, now)
		}

		// An item that left the backlog without being integrated or abandoned is not queued any more:
		// the registry follows the file, and keeps the change as one the backlog does not list, with
		// the hour the backlog first listed it. Only a file that was read whole takes a change out of
		// the queue.
		if backlogWhole {
			listed := make([]string, 0, len(backlog))
			for _, item := range backlog {
				if name := itemName(item, "change_name"); name != "" {
					listed = append(listed, name)
				}
			}
			names, _ := json.Marshal(listed)
			_, _ = tx.ExecContext(ctx, `
				UPDATE changes SET backlog_status = 'unknown', queued_at = NULL, updated_at = ?
				WHERE backlog_status = 'queued' AND change_name NOT IN (SELECT value FROM json_each(?))
			`, now, string(names))
		}

		// 3. Proposals, each with the status its file says.
		for _, item := range proposals {
			_ = s.mirrorProposalTx(ctx, tx, item, defaultStr(item["status"], "proposed"), now)
		}

		// A change that came from a proposal points at it.
		_, _ = tx.ExecContext(ctx, `
			UPDATE changes SET proposal_id = change_name
			WHERE proposal_id IS NULL AND EXISTS (SELECT 1 FROM proposals p WHERE p.proposal_id = changes.change_name)
		`)
		return nil
	})
}

// proposalID is the name a proposal goes by in the registry: its change_name, or its id.
func proposalID(item map[string]any) string {
	if id := itemName(item, "change_name"); id != "" {
		return id
	}
	return itemName(item, "id")
}

// mirrorProposalTx writes the proposal item under the status given: the one its file says for the
// mirror of the main checkout, ProposalUnknown for a cycle that records a proposal that checkout does
// not list. An integrated proposal stays integrated, and one that is queued is not taken back to
// proposed or approved.
func (s *Store) mirrorProposalTx(ctx context.Context, tx *sql.Tx, item map[string]any, status string, now int64) error {
	id := proposalID(item)
	if id == "" {
		return nil
	}
	proposedAt := rfc3339ToMS(defaultStr(item["proposed_at"], ""))
	if proposedAt == 0 {
		proposedAt = now
	}
	extraJSON, _ := json.Marshal(item)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO proposals (
			proposal_id, kind, status, priority, impact_score, source, strategy, description, evidence,
			target_prompt, edits_json, applied_json, review_json, proposed_at, queued_at, integrated_at,
			extra_json, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(proposal_id) DO UPDATE SET
			status = CASE
				WHEN proposals.status = 'integrated' THEN 'integrated'
				WHEN proposals.status = 'queued' AND excluded.status IN ('proposed', 'approved') THEN 'queued'
				ELSE excluded.status
			END,
			kind = excluded.kind,
			priority = excluded.priority,
			description = CASE WHEN excluded.description != '' THEN excluded.description ELSE proposals.description END,
			evidence = COALESCE(excluded.evidence, proposals.evidence),
			target_prompt = COALESCE(excluded.target_prompt, proposals.target_prompt),
			edits_json = COALESCE(excluded.edits_json, proposals.edits_json),
			applied_json = COALESCE(excluded.applied_json, proposals.applied_json),
			review_json = COALESCE(excluded.review_json, proposals.review_json),
			queued_at = COALESCE(excluded.queued_at, proposals.queued_at),
			integrated_at = COALESCE(excluded.integrated_at, proposals.integrated_at),
			extra_json = excluded.extra_json,
			updated_at = excluded.updated_at
	`, id, defaultStr(item["kind"], "change"), status, defaultStr(item["priority"], "normal"),
		numberOrNil(item["impact_score"]), nullStr(defaultStr(item["source"], "")), nullStr(defaultStr(item["strategy"], "")),
		defaultStr(item["description"], ""), nullStr(defaultStr(item["evidence"], "")),
		nullStr(defaultStr(item["target_prompt"], defaultStr(item["prompt"], ""))),
		jsonOrNil(item["edits"]), jsonOrNil(item["applied"]), jsonOrNil(item["review"]),
		proposedAt, msOrNil(item["queued_at"]), msOrNil(item["integrated_at"]), string(extraJSON), now)
	return err
}

// syncChangeListsTx replaces what a change depends on and the locks it declares with the lists of
// its backlog item. A change it depends on that the kit has not met yet is registered.
func (s *Store) syncChangeListsTx(ctx context.Context, tx *sql.Tx, change string, item map[string]any, now int64) {
	_, _ = tx.ExecContext(ctx, `DELETE FROM change_dependencies WHERE change_name = ?`, change)
	for _, dep := range itemStringList(item["depends_on"]) {
		if dep == change {
			continue
		}
		if s.ensureChangeTx(ctx, tx, dep, now) != nil {
			continue
		}
		_, _ = tx.ExecContext(ctx, `
			INSERT INTO change_dependencies (change_name, depends_on) VALUES (?, ?) ON CONFLICT DO NOTHING
		`, change, dep)
	}
	_, _ = tx.ExecContext(ctx, `DELETE FROM change_locks WHERE change_name = ?`, change)
	for _, lock := range itemStringList(item["locks"]) {
		_, _ = tx.ExecContext(ctx, `
			INSERT INTO change_locks (change_name, lock_name) VALUES (?, ?) ON CONFLICT DO NOTHING
		`, change, lock)
	}
}

func itemName(item map[string]any, key string) string {
	name := strings.TrimSpace(fmt.Sprint(item[key]))
	if name == "<nil>" {
		return ""
	}
	return name
}

// itemStringList reads a backlog list field written as a JSON list or as one comma-separated string.
func itemStringList(v any) []string {
	var parts []string
	switch t := v.(type) {
	case string:
		parts = strings.Split(t, ",")
	case []any:
		for _, it := range t {
			if it != nil {
				parts = append(parts, fmt.Sprint(it))
			}
		}
	case []string:
		parts = t
	}
	var out []string
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func numberOrNil(v any) any {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return nil
}

func jsonOrNil(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return nil
	}
	return string(b)
}

func msOrNil(v any) any {
	if ms := rfc3339ToMS(defaultStr(v, "")); ms > 0 {
		return ms
	}
	return nil
}

// MirroredFilesSignature returns a value that changes when one of the files SyncJSONToSQLite mirrors
// from dir is written, added or removed: the size and the time of modification of the backlog, of
// the proposals and of the history with its rotated files. It opens no file and no table, so a
// process can ask every few seconds and mirror only when the value has moved.
func (s *Store) MirroredFilesSignature(dir string) string {
	if dir == "" {
		dir = s.canonicalRoot
	}
	files := []string{
		filepath.Join(dir, ".foreman-kit", "agent_backlog.json"),
		filepath.Join(dir, ".foreman-kit", "agent_proposals.json"),
		filepath.Join(dir, ".foreman-kit", "agent_history.json"),
	}
	if rotated, err := filepath.Glob(filepath.Join(dir, ".foreman-kit", "agent_history.*.json")); err == nil {
		files = append(files, rotated...)
	}
	var b strings.Builder
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(&b, "%s -\n", filepath.Base(path))
			continue
		}
		fmt.Fprintf(&b, "%s %d %d\n", filepath.Base(path), info.Size(), info.ModTime().UnixNano())
	}
	return b.String()
}

func readAllHistoryMapSlice(dir string) []map[string]any {
	var out []map[string]any
	seen := make(map[string]bool)
	if matches, err := filepath.Glob(filepath.Join(dir, ".foreman-kit", "agent_history.*.json")); err == nil {
		for i := len(matches) - 1; i >= 0; i-- {
			for _, it := range readJSONMapSlice(matches[i]) {
				name := strings.TrimSpace(fmt.Sprint(it["change_name"]))
				if name != "" && !seen[name] {
					seen[name] = true
					out = append(out, it)
				}
			}
		}
	}
	for _, it := range readJSONMapSlice(filepath.Join(dir, ".foreman-kit", "agent_history.json")) {
		name := strings.TrimSpace(fmt.Sprint(it["change_name"]))
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, it)
		}
	}
	return out
}

func readJSONMapSlice(path string) []map[string]any {
	list, _ := readJSONMapSliceWhole(path)
	return list
}

// readJSONMapSliceWhole is readJSONMapSlice, and says whether the file is a JSON list that was read
// whole. A file that is missing, empty, or only read line by line (one caught half-written, or in
// the middle of a merge) is not: what it leaves out may still be there.
func readJSONMapSliceWhole(path string) ([]map[string]any, bool) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	var list []map[string]any
	if json.Unmarshal(raw, &list) == nil {
		return list, true
	}
	// Support one-object-per-line NDJSON format
	var linesOut []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if line == "" || line == "[" || line == "]" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			linesOut = append(linesOut, m)
		}
	}
	return linesOut, false
}

func defaultStr(v any, def string) string {
	if v == nil {
		return def
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "" || s == "<nil>" {
		return def
	}
	return s
}
