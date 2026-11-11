package store

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

// catalogTables are the tables that grow with the backlog or with the installation, not with use: the
// changes and what they declare, the engines, the repositories, the workspace and the locks held now.
// Every other table gains rows as the kit works, and no read of a View may walk one of those.
var catalogTables = map[string]bool{
	"changes": true, "change_dependencies": true, "change_locks": true, "engines": true, "repos": true,
	"workspace": true, "locks": true, "engine_claims": true,
}

var tableAliasRe = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+([a-z_][a-z0-9_]*)\s+(?:AS\s+)?([a-z_][a-z0-9_]*)`)

// aliasesIn returns, for each name a statement gives a table ("executions e", "gate_runs AS g"), the
// tables it gives it to.
func aliasesIn(sql string) map[string][]string {
	keywords := map[string]bool{
		"where": true, "on": true, "join": true, "left": true, "inner": true, "cross": true, "order": true, "group": true,
		"using": true, "limit": true, "union": true, "natural": true, "indexed": true, "not": true,
	}
	out := map[string][]string{}
	for _, m := range tableAliasRe.FindAllStringSubmatch(sql, -1) {
		table, alias := strings.ToLower(m[1]), strings.ToLower(m[2])
		if !keywords[alias] {
			out[alias] = append(out[alias], table)
		}
	}
	return out
}

// TestNoReadOfAViewWalksATableThatGrowsWithoutBound runs every read of a View with every filter it
// has and asks SQLite how it plans each statement. A statement that scans a table that grows with use
// (events, executions, gate_runs, log_chunks and every other table that is not a catalog) fails the
// test: a list is a search below its cursor, a filter is a search on its column, and what is open now
// is read from the partial index of the open rows. The one exception is the count of events by type
// with no filter, which reads the whole index of event names and never the table.
func TestNoReadOfAViewWalksATableThatGrowsWithoutBound(t *testing.T) {
	w := seedPortalWorld(t)
	ctx := context.Background()

	type statement struct {
		query string
		args  []any
	}
	var statements []statement
	seen := map[string]bool{}
	trace := func(query string, args []any) {
		if !seen[query] {
			seen[query] = true
			statements = append(statements, statement{query, args})
		}
	}
	err := w.st.view(ctx, trace, func(v *View) error {
		reads := []func() error{
			func() error { _, err := v.CountChangesByState(ctx, ""); return err },
			func() error { _, err := v.CountChangesByState(ctx, "api"); return err },
			func() error { _, err := v.ListChangeOverviews(ctx, ChangeFilter{}); return err },
			func() error {
				_, err := v.ListChangeOverviews(ctx, ChangeFilter{State: ChangeBuilding, Query: "api", Page: 2})
				return err
			},
			func() error { _, err := v.ChangeStory(ctx, "add-api", "", 0); return err },
			func() error { _, err := v.ChangeStory(ctx, "add-billing", "", 0); return err },
			func() error { _, err := v.ChangeStory(ctx, "add-report", "", 0); return err },
			func() error { _, err := v.ListExecutionsPage(ctx, ExecutionFilter{}); return err },
			func() error { _, err := v.ListExecutionsPage(ctx, ExecutionFilter{Kind: "worker"}); return err },
			func() error { _, err := v.ListExecutionsPage(ctx, ExecutionFilter{Status: "failed"}); return err },
			func() error { _, err := v.ListExecutionsPage(ctx, ExecutionFilter{Status: "running"}); return err },
			func() error { _, err := v.ListExecutionsPage(ctx, ExecutionFilter{Engine: "claude"}); return err },
			func() error { _, err := v.ListExecutionsPage(ctx, ExecutionFilter{Change: "add-api"}); return err },
			func() error {
				_, err := v.ListExecutionsPage(ctx, ExecutionFilter{Cycle: "20261105T090000Z"})
				return err
			},
			func() error {
				_, err := v.ListExecutionsPage(ctx, ExecutionFilter{Kind: "worker", Status: "succeeded", Engine: "claude", Change: "add-api"})
				return err
			},
			func() error { _, err := v.ListEventsPage(ctx, EventPageFilter{}); return err },
			func() error { _, err := v.ListEventsPage(ctx, EventPageFilter{Event: "agent.idle"}); return err },
			func() error { _, err := v.ListEventsPage(ctx, EventPageFilter{Change: "add-api"}); return err },
			func() error { _, err := v.ListEventsPage(ctx, EventPageFilter{ExecID: "api-apply"}); return err },
			func() error { _, err := v.ListEventsPage(ctx, EventPageFilter{ChangeRunID: w.apiRun}); return err },
			func() error {
				_, err := v.ListEventsPage(ctx, EventPageFilter{MergeAttemptID: w.firstMerge})
				return err
			},
			func() error { _, err := v.ListEventsPage(ctx, EventPageFilter{GateRoundID: w.failedRound}); return err },
			func() error {
				_, err := v.ListEventsPage(ctx, EventPageFilter{CycleStamp: "20261105T090000Z"})
				return err
			},
			func() error { _, err := v.CountEventsByType(ctx, EventPageFilter{}); return err },
			func() error { _, err := v.CountEventsByType(ctx, EventPageFilter{Change: "add-api"}); return err },
			func() error { _, err := v.CountEventsByType(ctx, EventPageFilter{ExecID: "api-apply"}); return err },
			func() error {
				_, err := v.CountEventsByType(ctx, EventPageFilter{CycleStamp: "20261105T090000Z"})
				return err
			},
			func() error { _, err := v.ListInbox(ctx); return err },
			func() error { _, err := v.ListProposals(ctx, "proposed", "", 0); return err },
			func() error { _, err := v.ListWaitFacts(ctx); return err },
			func() error { _, err := v.ListUnlistedQueued(ctx, 0); return err },
			func() error { _, err := v.ListLostRuns(ctx); return err },
			func() error { _, err := v.MergeQueue(ctx, "", 0); return err },
			func() error { _, err := v.GateStats(ctx, time.Now().Add(-24*time.Hour)); return err },
			func() error { _, err := v.EvaluationStats(ctx, time.Now().Add(-24*time.Hour)); return err },
			func() error { _, err := v.EngineStats(ctx, time.Now().Add(-24*time.Hour)); return err },
			func() error { _, err := v.ListPauses(ctx, "", 0); return err },
			func() error { _, err := v.OpenPause(ctx); return err },
			func() error { _, err := v.ListDispatcherRuns(ctx, "", 0); return err },
			func() error { _, err := v.ListIdleAgents(ctx); return err },
			func() error { _, err := v.ListEngineNames(ctx); return err },
			func() error { _, err := v.ListEngines(ctx); return err },
			func() error { _, err := v.ListVisualQAPage(ctx, "", "", 0); return err },
			func() error { _, err := v.ListVisualQAPage(ctx, "pending", "", 0); return err },
			func() error { _, err := v.CountVisualQAByStatus(ctx); return err },
			func() error { _, err := v.VisualSession(ctx); return err },
			func() error { _, err := v.VisualQACheck(ctx, "1"); return err },
			func() error { _, err := v.ListLocks(ctx); return err },
			func() error { _, err := v.LatestJob(ctx, "preflight", "preflight-services"); return err },
			func() error { _, err := v.Change(ctx, "add-api"); return err },
			func() error { _, err := v.Change(ctx, "add-ui"); return err },
			func() error { _, err := v.Change(ctx, "add-billing"); return err },
			func() error { _, err := v.Change(ctx, "add-export"); return err },
			func() error { _, err := v.Execution(ctx, "api-apply"); return err },
			func() error { _, err := v.Execution(ctx, "api-repair"); return err },
			func() error { _, err := v.Execution(ctx, "billing-prompt"); return err },
			func() error { _, err := v.Execution(ctx, "job-gates"); return err },
			func() error { _, _, err := v.ExecutionStatus(ctx, "api-apply"); return err },
			func() error { _, err := v.GateRound(ctx, w.failedRound); return err },
			func() error { _, err := v.EventsOfExecution(ctx, "api-apply"); return err },
			func() error { _, err := v.LogTail(ctx, "api-apply", 0); return err },
			func() error { _, err := v.LogTail(ctx, "api-apply", 8); return err },
			func() error { _, err := v.ReadLogChunks(ctx, "api-apply", "", 0); return err },
			// Every row of the world is in the stretch, so that each statement of the read runs: the
			// store stamps rows a little ahead of the clock when it writes many at once.
			func() error { _, err := v.SettingEvidence(ctx, time.Unix(0, 0), time.Now().Add(time.Hour)); return err },
			func() error { _, err := v.Dashboard(ctx, 0); return err },
			func() error { _, err := v.ListCyclesPage(ctx, "", 0); return err },
			func() error { _, err := v.Cycle(ctx, "20261105T090000Z"); return err },
			func() error { _, err := v.Event(ctx, 1); return err },
		}
		for _, read := range reads {
			if err := read(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) < 40 {
		t.Fatalf("the reads ran %d distinct statements: the trace is not seeing them", len(statements))
	}

	// The tables, the indexes that hold only the open rows of one, and what the views call the tables.
	tables, partial := map[string]bool{}, map[string]bool{}
	var views strings.Builder
	rows, err := w.st.DB().QueryContext(ctx, `SELECT type, name, COALESCE(sql, '') FROM sqlite_master`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var kind, name, sql string
		if err := rows.Scan(&kind, &name, &sql); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "table":
			tables[name] = true
		case "index":
			partial[name] = strings.Contains(strings.ToUpper(sql), " WHERE ")
		case "view":
			views.WriteString(sql + "\n")
		}
	}
	rows.Close()
	viewAliases := aliasesIn(views.String())

	const countOfEveryEvent = `SELECT event, COUNT(*) FROM events GROUP BY event ORDER BY event`
	usedIndexes := map[string]bool{}
	for _, st := range statements {
		ownAliases := aliasesIn(st.query)
		// tablesOf returns the tables a plan may mean by a name: the table of that name, or the ones the
		// statement gives the name to, or else the ones a view does.
		tablesOf := func(name string) []string {
			if tables[name] {
				return []string{name}
			}
			if own := ownAliases[name]; len(own) > 0 {
				return own
			}
			return viewAliases[name]
		}
		plan, err := w.st.DB().QueryContext(ctx, `EXPLAIN QUERY PLAN `+st.query, st.args...)
		if err != nil {
			t.Fatalf("EXPLAIN QUERY PLAN: %v\n%s", err, st.query)
		}
		var details []string
		for plan.Next() {
			var id, parent, unused int
			var detail string
			if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		plan.Close()
		if testing.Verbose() {
			t.Logf("%s\n  %s", strings.Join(strings.Fields(st.query), " "), strings.Join(details, "\n  "))
		}
		for _, detail := range details {
			if m := planIndexRe.FindStringSubmatch(detail); m != nil {
				usedIndexes[m[1]] = true
			}
			fields := strings.Fields(detail)
			if len(fields) < 2 || fields[0] != "SCAN" {
				continue
			}
			// "SCAN x USING [COVERING] INDEX name": the index is the last word.
			index := ""
			if strings.Contains(detail, " INDEX ") {
				index = fields[len(fields)-1]
			}
			if partial[index] {
				continue
			}
			if strings.Join(strings.Fields(st.query), " ") == countOfEveryEvent && strings.Contains(detail, "USING COVERING INDEX") {
				continue
			}
			for _, table := range tablesOf(strings.ToLower(fields[1])) {
				if tables[table] && !catalogTables[table] {
					t.Errorf("a read walks %s (%s):\n%s\nplan:\n  %s", table, detail, strings.TrimSpace(st.query), strings.Join(details, "\n  "))
					break
				}
			}
		}
	}

	// Every index the migration of the portal's reads adds holds a read, or waits for one that is named.
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var added []string
	for _, m := range migrations {
		if strings.HasSuffix(m.name, "_portal_reads.sql") {
			for _, idx := range createIndexRe.FindAllStringSubmatch(m.sql, -1) {
				added = append(added, idx[1])
			}
		}
	}
	if len(added) == 0 {
		t.Fatal("the migration of the portal's reads adds no index: was it renamed?")
	}
	for _, index := range added {
		reason, waits := indexesWaitingForARead[index]
		switch {
		case usedIndexes[index] && waits:
			t.Errorf("index %s holds a read now: take it off indexesWaitingForARead (%s)", index, reason)
		case !usedIndexes[index] && !waits:
			t.Errorf("index %s of the portal's reads holds no read of a View", index)
		}
	}
}

var (
	planIndexRe   = regexp.MustCompile(`\bINDEX ([a-z_][a-z0-9_]*)`)
	createIndexRe = regexp.MustCompile(`(?m)^CREATE (?:UNIQUE )?INDEX ([a-z_][a-z0-9_]*)`)
)

// indexesWaitingForARead are the indexes the migration of the portal's reads adds that no read of a
// View uses: those of a read that is not written yet, and those of a read made outside a View, with
// the read each one is for. One migration adds them all, so that the pages rewritten later and the
// daemon need no migration of their own. None waits now: every one of them holds a read.
var indexesWaitingForARead = map[string]string{}

// TestMovementIsCountedThroughItsIndexes: the daemon counts what moved after each pass of the
// dispatcher, with a read outside a View. The three counts are searches on the indexes the migration
// of the portal's reads adds for them.
func TestMovementIsCountedThroughItsIndexes(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for table, index := range map[string]string{
		"change_runs WHERE result = 'ready' AND finished_at >= ?1 AND finished_at < ?2":         "idx_change_runs_finished",
		"change_blocks WHERE blocked_at >= ?1 AND blocked_at < ?2":                              "idx_change_blocks_blocked",
		"merge_attempts WHERE result = 'integrated' AND finished_at >= ?1 AND finished_at < ?2": "idx_merge_attempts_finished",
	} {
		rows, err := st.DB().QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT COUNT(*) FROM `+table, 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if got := strings.Join(plan, "; "); !strings.HasPrefix(got, "SEARCH ") || !strings.Contains(got, index) {
			t.Errorf("the count over %s is planned as %q, want a search on %s", table, got, index)
		}
	}
	if _, err := st.MovementBetween(ctx, time.Now().Add(-time.Hour), time.Time{}); err != nil {
		t.Fatal(err)
	}
}

// TestAliasesInFindsTheNamesOfATable keeps the plan test honest: a table hidden behind a name it does
// not recognize would be walked unseen.
func TestAliasesInFindsTheNamesOfATable(t *testing.T) {
	got := aliasesIn(`SELECT 1 FROM executions e JOIN gate_runs AS g ON g.x = e.x LEFT JOIN executions cur ON 1
		WHERE EXISTS (SELECT 1 FROM executions WHERE x = 1) AND EXISTS (SELECT 1 FROM change_runs e)`)
	if strings.Join(got["e"], ",") != "executions,change_runs" || strings.Join(got["g"], ",") != "gate_runs" ||
		strings.Join(got["cur"], ",") != "executions" || len(got) != 3 {
		t.Errorf("aliases = %v", got)
	}
	if !strings.Contains(executionListColumns, "substr(e.prompt_text") || strings.Count(executionListColumns, ",") != strings.Count(executionColumns, ",")+2 {
		t.Errorf("executionListColumns no longer cuts the prompt of executionColumns:\n%s", executionListColumns)
	}
}
