package store

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MigrationReport summarizes counts imported by MigrateFromFiles (Section 10).
type MigrationReport struct {
	WorkspaceDir   string `json:"workspace_dir"`
	DBPath         string `json:"db_path"`
	HistoryItems   int    `json:"history_items"`
	BacklogItems   int    `json:"backlog_items"`
	Proposals      int    `json:"proposals"`
	ReadyManifests int    `json:"ready_manifests"`
	BlockedChanges int    `json:"blocked_changes"`
	BlockHistory   int    `json:"block_history"`
	EventsImported int    `json:"events_imported"`
	RepairRounds   int    `json:"repair_rounds"`
	Cycles         int    `json:"cycles"`
	VisualQAItems  int    `json:"visual_qa_items"`
	PortalRunsJobs int    `json:"portal_runs_jobs"`
}

var repairHeaderRe = regexp.MustCompile(`(?m)^##\s+([^\s,]+),\s*([^\s,]+),\s*(.+)$`)

// MigrateFromFiles imports legacy file-based state from <appDir>/config/ and <appDir>/log/
// into the workspace's SQLite database idempotently (Section 10).
func (s *Store) MigrateFromFiles(ctx context.Context, appDir string, out io.Writer) (*MigrationReport, error) {
	if out == nil {
		out = io.Discard
	}
	if appDir == "" {
		appDir = s.canonicalRoot
	}
	absDir, err := filepath.Abs(appDir)
	if err == nil {
		appDir = absDir
	}

	rep := &MigrationReport{
		WorkspaceDir: appDir,
		DBPath:       s.dbPath,
	}

	// 1 & 2 & 4. Import config/agent_{history,backlog,proposals}.json (including rotated history files)
	// The report counts what was put on record now: an item the registry already holds is not counted.
	if rep.HistoryItems, rep.BacklogItems, rep.Proposals, err = s.countNewRegistryItems(ctx, appDir); err != nil {
		return rep, err
	}
	if err := s.SyncJSONToSQLite(ctx, appDir); err != nil {
		return rep, err
	}

	// 2b. Import log/ready_to_merge/*.json
	readyFiles, _ := filepath.Glob(filepath.Join(appDir, "log", "ready_to_merge", "*.json"))
	for _, rf := range readyFiles {
		m := readJSONSingleMap(rf)
		if len(m) == 0 {
			continue
		}
		chg := defaultStr(m["change"], strings.TrimSuffix(filepath.Base(rf), ".json"))
		branch := defaultStr(m["branch"], "feat/spec-"+chg)
		wt := defaultStr(m["worktree"], filepath.Join(appDir, ".worktrees", "worker-"+chg))
		var caps []string
		if rawCaps, ok := m["capabilities"].([]any); ok {
			for _, c := range rawCaps {
				caps = append(caps, fmt.Sprint(c))
			}
		}
		assessment, _ := m["jev"].(map[string]any)
		itemMap, _ := m["item"].(map[string]any)
		// A manifest names the commit to merge: one without it cannot be queued. The manifest of a
		// commit already on record, open or closed, is not queued again.
		if known, err := s.rowExists(ctx, `SELECT 1 FROM ready_manifests WHERE change_name = ? AND head_sha = ?`, chg, defaultStr(m["head"], "")); err != nil {
			return rep, err
		} else if known {
			continue
		}
		if err := s.EnqueueReady(ctx, ReadyManifest{
			ChangeName:   chg,
			Branch:       branch,
			Worktree:     wt,
			HeadSHA:      defaultStr(m["head"], ""),
			Capabilities: caps,
			Assessment:   assessment,
			Item:         itemMap,
			EnqueuedAt:   defaultStr(m["enqueued_at"], ""),
		}); err == nil {
			rep.ReadyManifests++
		}
	}

	// 3. Import log/blocked/history/*.json: the blocks a change was retried from, oldest first, so the
	// block it has open now (below) continues their count. A file names no retry number of its own:
	// the blocks of one change are numbered in the order they happened (blocked_at, then file order),
	// whatever other changes have in the same directory.
	histFiles, _ := filepath.Glob(filepath.Join(appDir, "log", "blocked", "history", "*.json"))
	type historyBlock struct {
		m         map[string]any
		blockedAt int64
		retry     int
		file      int
	}
	histByChange := make(map[string][]historyBlock)
	var histOrder []string
	for idx, hf := range histFiles {
		m := readJSONSingleMap(hf)
		if len(m) == 0 {
			continue
		}
		chg := defaultStr(m["change"], strings.TrimSuffix(filepath.Base(hf), ".json"))
		h := historyBlock{m: m, blockedAt: rfc3339ToMS(defaultStr(m["blocked_at"], "")), file: idx}
		if r, ok := m["retry"].(float64); ok && r > 0 {
			h.retry = int(r)
		}
		if _, seen := histByChange[chg]; !seen {
			histOrder = append(histOrder, chg)
		}
		histByChange[chg] = append(histByChange[chg], h)
	}
	for _, chg := range histOrder {
		blocks := histByChange[chg]
		sort.SliceStable(blocks, func(i, j int) bool {
			a, b := blocks[i], blocks[j]
			if a.blockedAt != b.blockedAt {
				return a.blockedAt < b.blockedAt
			}
			if a.retry != b.retry {
				return a.retry < b.retry
			}
			return a.file < b.file
		})
		retryNum := 0
		for _, h := range blocks {
			retryNum++
			if h.retry > 0 {
				retryNum = h.retry
			}
			// The block a change was retried from at that retry number is on record once imported.
			if known, err := s.rowExists(ctx, `
				SELECT 1 FROM change_blocks
				WHERE change_name = ? AND host = 'imported' AND resolution = ? AND retry_num = ?
			`, chg, BlockAutoRetry, retryNum-1); err != nil {
				return rep, err
			} else if known {
				continue
			}
			m, num := h.m, retryNum
			blockedAt := s.msOrNow(defaultStr(m["blocked_at"], ""))
			err := s.inTx(ctx, func(tx *sql.Tx) error {
				if err := s.ensureChangeTx(ctx, tx, chg, s.nowMS()); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `
					INSERT INTO change_blocks (change_name, reason, details, host, retry_num, retry_told, blocked_at, resolved_at, resolution)
					VALUES (?, ?, ?, 'imported', ?, 1, ?, ?, ?)
				`, chg, defaultStr(m["reason"], "gate_failure"), nullStr(defaultStr(m["details"], "")), num-1, blockedAt, s.nowMS(), BlockAutoRetry)
				return err
			})
			if err == nil {
				rep.BlockHistory++
			}
		}
	}

	// 2c. Import log/blocked/*.json and log/approved/*. A block the file describes that the change
	// already has on record (the same reason, details and stage, at the same instant when the file
	// gives one) is not recorded again: BlockChange would close the open one as superseded.
	blockedFiles, _ := filepath.Glob(filepath.Join(appDir, "log", "blocked", "*.json"))
	for _, bf := range blockedFiles {
		m := readJSONSingleMap(bf)
		if len(m) == 0 {
			continue
		}
		chg := defaultStr(m["change"], strings.TrimSuffix(filepath.Base(bf), ".json"))
		reason := defaultStr(m["reason"], "gate_failure")
		blockedAt := rfc3339ToMS(defaultStr(m["blocked_at"], ""))
		if known, err := s.rowExists(ctx, `
			SELECT 1 FROM change_blocks
			WHERE change_name = ? AND reason = ? AND COALESCE(details, '') = ? AND COALESCE(stage, '') = ?
			  AND COALESCE(host, '') <> 'imported' AND (? = 0 OR blocked_at = ?)
		`, chg, reason, defaultStr(m["details"], ""), defaultStr(m["stage"], ""), blockedAt, blockedAt); err != nil {
			return rep, err
		} else if known {
			continue
		}
		if err := s.BlockChange(ctx, QuarantineRecord{
			ChangeName: chg,
			Reason:     reason,
			Details:    defaultStr(m["details"], ""),
			Stage:      defaultStr(m["stage"], ""),
			Worktree:   defaultStr(m["worktree"], ""),
			Branch:     defaultStr(m["branch"], ""),
			Host:       defaultStr(m["host"], ""),
			BlockedAt:  defaultStr(m["blocked_at"], ""),
		}); err == nil {
			rep.BlockedChanges++
		}
	}

	approvedFiles, _ := filepath.Glob(filepath.Join(appDir, "log", "approved", "*"))
	for _, af := range approvedFiles {
		info, err := os.Stat(af)
		if err != nil || info.IsDir() {
			continue
		}
		// A change that has had an approval on record, revoked or not, is not approved again: that
		// would accept the risk of a block it has met since.
		chg := filepath.Base(af)
		if known, err := s.rowExists(ctx, `SELECT 1 FROM risk_approvals WHERE change_name = ?`, chg); err != nil {
			return rep, err
		} else if known {
			continue
		}
		_ = s.ApproveChangeRiskBy(ctx, chg, "imported")
	}

	// 5. Import log/events.jsonl
	eventsFile := filepath.Join(appDir, "log", "events.jsonl")
	if f, err := os.Open(eventsFile); err == nil {
		fileCount := make(map[string]int)
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			evName := defaultStr(ev["event"], "")
			if evName == "" {
				continue
			}
			in := EventInput{
				Event:  evName,
				Change: defaultStr(ev["change"], ""),
				Engine: defaultStr(ev["engine"], ""),
				Phase:  defaultStr(ev["phase"], ""),
			}
			if t, err := time.Parse(time.RFC3339, defaultStr(ev["ts"], "")); err == nil {
				in.TS = t
			}
			if p, ok := ev["pid"].(float64); ok {
				in.PID = int(p)
			}
			// What has a column leaves the payload; the rest of the line is the payload.
			for _, k := range []string{"ts", "event", "pid", "change", "engine", "phase"} {
				delete(ev, k)
			}
			in.Payload = ev
			// A line already on record (a run before imported it) is skipped. Lines that are the
			// same event are told apart by how many the file holds: the n-th needs n rows on record.
			key, recorded, err := s.eventsOnRecord(ctx, in)
			if err != nil {
				_ = f.Close()
				return rep, err
			}
			fileCount[key]++
			if recorded >= fileCount[key] {
				continue
			}
			if s.EmitEvent(context.WithValue(ctx, scopeKey{}, Scope{}), in) == nil {
				rep.EventsImported++
			}
		}
		_ = f.Close()
	}

	// 6. Import repairs.md from openspec/changes/archive/*/repairs.md and openspec/changes/*/repairs.md.
	// A repair round belongs to a worker run and names the execution that repaired: the file-based kit
	// recorded neither, so each change gets one run, and each round one execution, marked as imported.
	var repairFiles []string
	if m1, err := filepath.Glob(filepath.Join(appDir, "openspec", "changes", "archive", "*", "repairs.md")); err == nil {
		repairFiles = append(repairFiles, m1...)
	}
	if m2, err := filepath.Glob(filepath.Join(appDir, "openspec", "changes", "*", "repairs.md")); err == nil {
		repairFiles = append(repairFiles, m2...)
	}
	datePref := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-`)
	for _, rf := range repairFiles {
		raw, err := os.ReadFile(rf)
		if err != nil {
			continue
		}
		parentName := filepath.Base(filepath.Dir(rf))
		if parentName == "archive" {
			continue
		}
		chg := datePref.ReplaceAllString(parentName, "")
		matches := repairHeaderRe.FindAllStringSubmatchIndex(string(raw), -1)
		if len(matches) == 0 {
			continue
		}
		var runID int64
		for i, loc := range matches {
			phase := string(raw[loc[2]:loc[3]])
			ts := string(raw[loc[4]:loc[5]])
			eng := normalizeEngine(string(raw[loc[6]:loc[7]]))
			end := len(raw)
			if i+1 < len(matches) {
				end = matches[i+1][0]
			}
			body := string(raw[loc[1]:end])
			passed := strings.Contains(body, "Result: passed.")
			at := s.msOrNow(ts)
			execID := fmt.Sprintf("imported-%s-%s-%d", chg, phase, i+1)
			// A round whose execution an earlier run recorded is on record: it is not recorded again.
			if known, err := s.rowExists(ctx, `SELECT 1 FROM repair_rounds WHERE exec_id = ?`, execID); err != nil {
				return rep, err
			} else if known {
				continue
			}
			err := s.inTx(ctx, func(tx *sql.Tx) error {
				if err := s.ensureChangeTx(ctx, tx, chg, s.nowMS()); err != nil {
					return err
				}
				if err := s.ensureEngineTx(ctx, tx, eng); err != nil {
					return err
				}
				if runID == 0 {
					// The run an earlier import made for the change stands for this one too.
					err := tx.QueryRowContext(ctx, `
						SELECT id FROM change_runs WHERE change_name = ? AND host = 'imported' AND pid = 0 ORDER BY id LIMIT 1
					`, chg).Scan(&runID)
					if err != nil && err != sql.ErrNoRows {
						return err
					}
				}
				if runID == 0 {
					res, err := tx.ExecContext(ctx, `
						INSERT INTO change_runs (change_name, pid, host, slot, worktree, branch, origin, result, started_at, finished_at)
						VALUES (?, 0, 'imported', 0, '', ?, 'backlog', 'stopped', ?, ?)
					`, chg, "feat/spec-"+chg, at, at)
					if err != nil {
						return err
					}
					if runID, err = res.LastInsertId(); err != nil {
						return err
					}
				}
				status := "failed"
				if passed {
					status = "succeeded"
				}
				_, err := tx.ExecContext(ctx, `
					INSERT INTO executions (exec_id, kind, change_name, change_run_id, phase, engines_requested, engine_used,
					                        status, exit_code, host, started_at, finished_at)
					VALUES (?, 'worker', ?, ?, ?, ?, ?, ?, 0, 'imported', ?, ?)
					ON CONFLICT(exec_id) DO NOTHING
				`, execID, chg, runID, phase, eng, nullStr(eng), status, at, at)
				return err
			})
			if err != nil {
				continue
			}
			if s.RecordRepairRound(ctx, RepairRoundRecord{
				ChangeName:   chg,
				ChangeRunID:  runID,
				Phase:        phase,
				ExecID:       execID,
				AskedReason:  "repair",
				AskedDetails: strings.TrimSpace(body),
				ResultPassed: passed,
				CreatedAt:    ts,
			}) == nil {
				rep.RepairRounds++
			}
		}
	}

	// 7. Import log/cycle/state.json, log/cycle/visual_queue.json, log/cycle/cycle-*.log
	// The cycle state.json names as the last one is finished by the file only when no earlier import
	// or run of the kit recorded it: what is on record is not overwritten.
	var lastCycleKnown bool
	if stBytes, err := os.ReadFile(filepath.Join(appDir, "log", "cycle", "state.json")); err == nil && len(stBytes) > 0 {
		var state map[string]any
		if json.Unmarshal(stBytes, &state) == nil {
			if last, ok := state["last_cycle"].(map[string]any); ok {
				if stamp := defaultStr(last["stamp"], ""); stamp != "" {
					if lastCycleKnown, err = s.rowExists(ctx, `SELECT 1 FROM cycles WHERE stamp = ?`, stamp); err != nil {
						return rep, err
					}
				}
			}
		}
	}
	cycleLogs, _ := filepath.Glob(filepath.Join(appDir, "log", "cycle", "cycle-*.log"))
	for _, cl := range cycleLogs {
		stamp := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(cl), "cycle-"), ".log")
		at := cycleStampMS(stamp)
		if at == 0 {
			if info, err := os.Stat(cl); err == nil {
				at = info.ModTime().UnixMilli()
			} else {
				at = s.nowMS()
			}
		}
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO cycles (stamp, host, outcome, log_path, started_at, finished_at)
			VALUES (?, 'imported', 'done', ?, ?, ?)
			ON CONFLICT(stamp) DO NOTHING
		`, stamp, cl, at, at)
		if err == nil {
			// Only a row the insert wrote counts: a cycle already on record is left as it is.
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				rep.Cycles++
			}
		}
	}
	if stBytes, err := os.ReadFile(filepath.Join(appDir, "log", "cycle", "state.json")); err == nil && len(stBytes) > 0 {
		var state map[string]any
		if json.Unmarshal(stBytes, &state) == nil {
			// The workspace remembers the last commit visual QA covered once: an earlier import, or the
			// kit itself, already set it, and the file's older one would queue everything since again.
			if sha := defaultStr(state["visual_qa_last_sha"], ""); sha != "" {
				var current sql.NullString
				if err := s.db.QueryRowContext(ctx, `SELECT visual_qa_last_sha FROM workspace WHERE id = 1`).Scan(&current); err == nil && current.String == "" {
					_ = s.SetVisualQALastSHA(ctx, sha)
				}
			}
			if last, ok := state["last_cycle"].(map[string]any); ok {
				if stamp := defaultStr(last["stamp"], ""); stamp != "" && !lastCycleKnown {
					_ = s.FinishCycle(ctx, stamp, defaultStr(last["outcome"], "done"), defaultStr(last["finished_at"], ""))
				}
			}
		}
	}
	vqaPath := filepath.Join(appDir, "log", "cycle", "visual_queue.json")
	if vqaBytes, err := os.ReadFile(vqaPath); err == nil && len(vqaBytes) > 0 {
		var vqaObj struct {
			Items []map[string]any `json:"items"`
		}
		var vqaList []map[string]any
		if json.Unmarshal(vqaBytes, &vqaObj) == nil && len(vqaObj.Items) > 0 {
			vqaList = vqaObj.Items
		} else {
			_ = json.Unmarshal(vqaBytes, &vqaList)
		}
		for idx, it := range vqaList {
			idStr := defaultStr(it["id"], strconv.Itoa(idx+1))
			chg := defaultStr(it["change"], defaultStr(it["change_name"], ""))
			what := defaultStr(it["detail"], defaultStr(it["what"], chg))
			st := defaultStr(it["status"], "pending")
			note := defaultStr(it["note"], defaultStr(it["verdict_note"], ""))
			// An item on record is skipped: by its id when it carries a number (the id the queue file
			// and the table share), else by the change, the text and the creation time it names.
			var known bool
			var err error
			if id, perr := strconv.ParseInt(idStr, 10, 64); perr == nil && id > 0 {
				known, err = s.rowExists(ctx, `SELECT 1 FROM visual_qa_items WHERE id = ?`, id)
			} else {
				created := rfc3339ToMS(defaultStr(it["created_at"], ""))
				known, err = s.rowExists(ctx, `
					SELECT 1 FROM visual_qa_items
					WHERE change_name IS ? AND what = ? AND (? = 0 OR created_at = ?)
				`, nullStr(chg), what, created, created)
			}
			if err != nil {
				return rep, err
			}
			if known {
				continue
			}
			if s.EnqueueVisualQA(ctx, VisualQAItem{
				ItemID:      idStr,
				ChangeName:  chg,
				What:        what,
				Status:      st,
				VerdictNote: note,
				CreatedAt:   defaultStr(it["created_at"], ""),
			}) == nil {
				rep.VisualQAItems++
			}
		}
	}

	// 8. Import log/portal/runs/*.json and log/portal/jobs/*.json
	for _, kind := range []string{"runs", "jobs"} {
		files, _ := filepath.Glob(filepath.Join(appDir, "log", "portal", kind, "*.json"))
		for _, pf := range files {
			m := readJSONSingleMap(pf)
			if len(m) == 0 {
				continue
			}
			id := defaultStr(m["id"], strings.TrimSuffix(filepath.Base(pf), ".json"))
			if existing, _ := s.GetExecution(ctx, id); existing != nil {
				continue
			}
			started := defaultStr(m["started_at"], "")
			status := normalizeExecStatus(defaultStr(m["status"], "finished"), 0)
			if status == "running" {
				// Nothing of the file-based kit still runs.
				status = "stopped"
			}
			if s.CreateExecution(ctx, Execution{
				ExecID:     id,
				Kind:       kind,
				Agent:      defaultStr(m["agent"], ""),
				Phase:      defaultStr(m["kind"], ""),
				Status:     status,
				PromptText: defaultStr(m["prompt"], defaultStr(m["command"], "")),
				Workdir:    defaultStr(m["workdir"], ""),
				Summary:    defaultStr(m["summary"], ""),
				Host:       "imported",
				StartedAt:  started,
				FinishedAt: defaultStr(m["finished_at"], started),
			}) == nil {
				rep.PortalRunsJobs++
			}
		}
	}

	fmt.Fprintf(out, "migrate-from-files: completed for %s -> %s\n", rep.WorkspaceDir, rep.DBPath)
	fmt.Fprintf(out, "  history_items:    %d\n", rep.HistoryItems)
	fmt.Fprintf(out, "  backlog_items:    %d\n", rep.BacklogItems)
	fmt.Fprintf(out, "  proposals:        %d\n", rep.Proposals)
	fmt.Fprintf(out, "  ready_manifests:  %d\n", rep.ReadyManifests)
	fmt.Fprintf(out, "  blocked_changes:  %d\n", rep.BlockedChanges)
	fmt.Fprintf(out, "  block_history:    %d\n", rep.BlockHistory)
	fmt.Fprintf(out, "  events_imported:  %d\n", rep.EventsImported)
	fmt.Fprintf(out, "  repair_rounds:    %d\n", rep.RepairRounds)
	fmt.Fprintf(out, "  cycles:           %d\n", rep.Cycles)
	fmt.Fprintf(out, "  visual_qa_items:  %d\n", rep.VisualQAItems)
	fmt.Fprintf(out, "  portal_runs_jobs: %d\n", rep.PortalRunsJobs)
	return rep, nil
}

// rowExists reports whether the query (which selects one column) returns a row.
func (s *Store) rowExists(ctx context.Context, query string, args ...any) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, query+` LIMIT 1`, args...).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// eventsOnRecord says how many events already on record are the line of events.jsonl that in was read
// from: the same event of the same change, engine and phase with the same payload, at the same
// instant and from the same process when the line names them. It also returns the line's identity,
// which tells apart the lines of one file that are the same event.
func (s *Store) eventsOnRecord(ctx context.Context, in EventInput) (string, int, error) {
	payload := in.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", 0, err
	}
	var ts int64
	if !in.TS.IsZero() {
		ts = in.TS.UTC().UnixMilli()
	}
	pid := in.PID
	if pid < 0 {
		pid = 0
	}
	engine := normalizeEngine(in.Engine)
	key := fmt.Sprintf("%d|%d|%s|%s|%s|%s|%s", ts, pid, in.Event, in.Change, engine, in.Phase, b)
	var n int
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM events
		WHERE event = ? AND change_name IS ? AND engine IS ? AND phase IS ? AND payload_json = ?
		  AND (? = 0 OR ts = ?) AND (? = 0 OR pid = ?)
	`, in.Event, nullStr(in.Change), nullStr(engine), nullStr(in.Phase), string(b), ts, ts, pid, pid).Scan(&n)
	return key, n, err
}

// countNewRegistryItems counts the items of the history, the backlog and the proposals files that the
// registry does not hold yet: a change not yet integrated or discarded for the history, one not yet
// queued (or past it) for the backlog, a proposal with no row for the proposals.
func (s *Store) countNewRegistryItems(ctx context.Context, appDir string) (history, backlog, proposals int, err error) {
	seen := make(map[string]bool)
	for _, item := range readAllHistoryMapSlice(appDir) {
		name := itemName(item, "change_name")
		if name == "" || seen["h:"+name] {
			continue
		}
		seen["h:"+name] = true
		status := "integrated"
		if defaultStr(item["status"], "") == "discarded" {
			status = "discarded"
		}
		known, err := s.rowExists(ctx, `SELECT 1 FROM changes WHERE change_name = ? AND backlog_status = ?`, name, status)
		if err != nil {
			return 0, 0, 0, err
		}
		if !known {
			history++
		}
	}
	for _, item := range readJSONMapSlice(filepath.Join(appDir, ".foreman-kit", "agent_backlog.json")) {
		name := itemName(item, "change_name")
		if name == "" || seen["b:"+name] {
			continue
		}
		seen["b:"+name] = true
		known, err := s.rowExists(ctx, `
			SELECT 1 FROM changes WHERE change_name = ? AND backlog_status IN ('queued', 'integrated', 'discarded')
		`, name)
		if err != nil {
			return 0, 0, 0, err
		}
		if !known {
			backlog++
		}
	}
	for _, item := range readJSONMapSlice(filepath.Join(appDir, ".foreman-kit", "agent_proposals.json")) {
		id := proposalID(item)
		if id == "" || seen["p:"+id] {
			continue
		}
		seen["p:"+id] = true
		known, err := s.rowExists(ctx, `SELECT 1 FROM proposals WHERE proposal_id = ?`, id)
		if err != nil {
			return 0, 0, 0, err
		}
		if !known {
			proposals++
		}
	}
	return history, backlog, proposals, nil
}

func readJSONSingleMap(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	return nil
}

// cycleStampMS reads the instant a cycle stamp names (for example 20261107T100000Z), 0 when it names none.
func cycleStampMS(stamp string) int64 {
	for _, layout := range []string{"20060102T150405Z", "20060102-150405", "20060102150405", time.RFC3339} {
		if t, err := time.Parse(layout, stamp); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}
