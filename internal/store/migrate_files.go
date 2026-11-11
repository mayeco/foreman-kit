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
	rep.HistoryItems = len(readAllHistoryMapSlice(appDir))
	rep.BacklogItems = len(readJSONMapSlice(filepath.Join(appDir, ".foreman-kit", "agent_backlog.json")))
	rep.Proposals = len(readJSONMapSlice(filepath.Join(appDir, ".foreman-kit", "agent_proposals.json")))
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
		// A manifest names the commit to merge: one without it cannot be queued.
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
	// block it has open now (below) continues their count.
	histFiles, _ := filepath.Glob(filepath.Join(appDir, "log", "blocked", "history", "*.json"))
	for idx, hf := range histFiles {
		m := readJSONSingleMap(hf)
		if len(m) == 0 {
			continue
		}
		chg := defaultStr(m["change"], strings.TrimSuffix(filepath.Base(hf), ".json"))
		retryNum := idx + 1
		if r, ok := m["retry"].(float64); ok && r > 0 {
			retryNum = int(r)
		}
		blockedAt := s.msOrNow(defaultStr(m["blocked_at"], ""))
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			if err := s.ensureChangeTx(ctx, tx, chg, s.nowMS()); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO change_blocks (change_name, reason, details, host, retry_num, retry_told, blocked_at, resolved_at, resolution)
				VALUES (?, ?, ?, 'imported', ?, 1, ?, ?, ?)
			`, chg, defaultStr(m["reason"], "gate_failure"), nullStr(defaultStr(m["details"], "")), retryNum-1, blockedAt, s.nowMS(), BlockAutoRetry)
			return err
		})
		if err == nil {
			rep.BlockHistory++
		}
	}

	// 2c. Import log/blocked/*.json and log/approved/*
	blockedFiles, _ := filepath.Glob(filepath.Join(appDir, "log", "blocked", "*.json"))
	for _, bf := range blockedFiles {
		m := readJSONSingleMap(bf)
		if len(m) == 0 {
			continue
		}
		chg := defaultStr(m["change"], strings.TrimSuffix(filepath.Base(bf), ".json"))
		if err := s.BlockChange(ctx, QuarantineRecord{
			ChangeName: chg,
			Reason:     defaultStr(m["reason"], "gate_failure"),
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
		if err == nil && !info.IsDir() {
			_ = s.ApproveChangeRiskBy(ctx, filepath.Base(af), "imported")
		}
	}

	// 5. Import log/events.jsonl
	eventsFile := filepath.Join(appDir, "log", "events.jsonl")
	if f, err := os.Open(eventsFile); err == nil {
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
			err := s.inTx(ctx, func(tx *sql.Tx) error {
				if err := s.ensureChangeTx(ctx, tx, chg, s.nowMS()); err != nil {
					return err
				}
				if err := s.ensureEngineTx(ctx, tx, eng); err != nil {
					return err
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
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO cycles (stamp, host, outcome, log_path, started_at, finished_at)
			VALUES (?, 'imported', 'done', ?, ?, ?)
			ON CONFLICT(stamp) DO NOTHING
		`, stamp, cl, at, at); err == nil {
			rep.Cycles++
		}
	}
	if stBytes, err := os.ReadFile(filepath.Join(appDir, "log", "cycle", "state.json")); err == nil && len(stBytes) > 0 {
		var state map[string]any
		if json.Unmarshal(stBytes, &state) == nil {
			if sha := defaultStr(state["visual_qa_last_sha"], ""); sha != "" {
				_ = s.SetVisualQALastSHA(ctx, sha)
			}
			if last, ok := state["last_cycle"].(map[string]any); ok {
				if stamp := defaultStr(last["stamp"], ""); stamp != "" {
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
