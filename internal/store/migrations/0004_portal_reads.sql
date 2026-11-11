-- foreman-kit state database, schema version 4: what the portal reads.
--
-- Nothing here stores a fact. The views say how a page reads the tables, so the portal shows what the
-- rows say instead of working it out again from files and events; the indexes hold every filter and
-- every cursor of those reads, so no page walks a table that grows without bound.

-- =====================================================================
-- 1. VIEWS
-- =====================================================================

-- One row per change, for the list of changes and for the backlog: its state, how much work it took
-- and when something last happened to it. changes.updated_at is not part of that: the backlog mirror
-- rewrites it on every sync.
CREATE VIEW v_change_overview AS
SELECT c.change_name, c.description, c.priority, c.backlog_status, s.state,
       c.first_seen_at, c.queued_at, c.integrated_at, c.integrated_commit, c.discarded_at,
       (SELECT COUNT(*) FROM change_runs r WHERE r.change_name = c.change_name) AS runs,
       (SELECT COUNT(*) FROM executions e WHERE e.change_name = c.change_name) AS executions,
       (SELECT COUNT(*) FROM repair_rounds p JOIN change_runs r ON r.id = p.change_run_id
         WHERE r.change_name = c.change_name) AS repairs,
       (SELECT b.reason FROM change_blocks b
         WHERE b.change_name = c.change_name AND b.resolved_at IS NULL) AS open_block_reason,
       (SELECT COUNT(*) FROM change_deferrals d
         WHERE d.change_name = c.change_name AND d.cleared_at IS NULL) AS open_deferrals,
       (SELECT r.result FROM change_runs r
         WHERE r.change_name = c.change_name ORDER BY r.id DESC LIMIT 1) AS last_run_result,
       MAX(c.first_seen_at,
           COALESCE(c.queued_at, 0), COALESCE(c.integrated_at, 0), COALESCE(c.discarded_at, 0),
           COALESCE((SELECT MAX(COALESCE(r.finished_at, r.started_at)) FROM change_runs r
                      WHERE r.change_name = c.change_name), 0),
           COALESCE((SELECT MAX(COALESCE(m.finished_at, m.started_at)) FROM merge_attempts m
                      JOIN ready_manifests q ON q.id = m.manifest_id
                      WHERE q.change_name = c.change_name), 0),
           COALESCE((SELECT MAX(COALESCE(b.resolved_at, b.blocked_at)) FROM change_blocks b
                      WHERE b.change_name = c.change_name), 0)) AS last_activity_at
FROM changes c JOIN v_change_state s USING (change_name);

-- The episodes of a change's story, one row each: what happened to the change and when it began. A
-- worker run carries its executions, gate rounds, repairs, evaluations and deferrals, and a merge
-- attempt its gate rounds, evaluations and pushes; the rows of those tables that belong to neither
-- (a prompt sent from the portal, gates run by hand) are episodes of their own.
CREATE VIEW v_change_episodes AS
SELECT change_name, 'run' AS kind, id AS episode_id, started_at AS at FROM change_runs
UNION ALL
SELECT r.change_name, 'merge', m.id, m.started_at
FROM merge_attempts m JOIN ready_manifests r ON r.id = m.manifest_id
UNION ALL
SELECT change_name, 'manifest', id, enqueued_at FROM ready_manifests
UNION ALL
SELECT change_name, 'block', id, blocked_at FROM change_blocks
UNION ALL
SELECT change_name, 'rebuild', id, requested_at FROM rebuild_requests
UNION ALL
SELECT change_name, 'approval', id, approved_at FROM risk_approvals
UNION ALL
SELECT change_name, 'deferral', id, deferred_at FROM change_deferrals WHERE change_run_id IS NULL
UNION ALL
SELECT change_name, 'gates', id, started_at FROM gate_rounds
WHERE change_name IS NOT NULL AND change_run_id IS NULL AND merge_attempt_id IS NULL
UNION ALL
SELECT change_name, 'evaluation', id, created_at FROM evaluations
WHERE change_name IS NOT NULL AND change_run_id IS NULL AND merge_attempt_id IS NULL
UNION ALL
SELECT change_name, 'execution', rowid, started_at FROM executions
WHERE change_name IS NOT NULL AND change_run_id IS NULL;

-- The recorded facts that hold a queued change back. They say why a change waits, not which one the
-- dispatcher starts next: that also depends on its memory and on the worktrees, which are not here.
CREATE VIEW v_wait_facts AS
-- A change it depends on is not integrated.
SELECT d.change_name, 'dependency' AS kind, d.depends_on AS other_change, s.state AS other_state,
       NULL AS lock_name, NULL AS reason, NULL AS since_at, NULL AS until_at
FROM changes c
JOIN change_dependencies d ON d.change_name = c.change_name
JOIN v_change_state s ON s.change_name = d.depends_on
WHERE c.backlog_status = 'queued' AND s.state <> 'integrated'
UNION ALL
-- It is blocked.
SELECT b.change_name, 'block', NULL, NULL, NULL, b.reason, b.blocked_at, NULL
FROM changes c JOIN change_blocks b ON b.change_name = c.change_name AND b.resolved_at IS NULL
WHERE c.backlog_status = 'queued'
UNION ALL
-- No engine of a role could work on it.
SELECT d.change_name, 'deferral', NULL, NULL, NULL, d.role, MAX(d.deferred_at), MAX(d.retry_at)
FROM changes c JOIN change_deferrals d ON d.change_name = c.change_name AND d.cleared_at IS NULL
WHERE c.backlog_status = 'queued'
GROUP BY d.change_name, d.role
UNION ALL
-- It declares a lock that a change in progress declares too.
SELECT l.change_name, 'lock', o.change_name, s.state, l.lock_name, NULL, NULL, NULL
FROM changes c
JOIN change_locks l ON l.change_name = c.change_name
JOIN change_locks o ON o.lock_name = l.lock_name AND o.change_name <> l.change_name
JOIN v_change_state s ON s.change_name = o.change_name
WHERE c.backlog_status = 'queued' AND s.state IN ('building', 'ready', 'merging');

-- =====================================================================
-- 2. INDEXES
-- =====================================================================

-- Events, filtered by what they are about.
CREATE INDEX idx_events_merge           ON events(merge_attempt_id, id)   WHERE merge_attempt_id IS NOT NULL;
CREATE INDEX idx_events_gate_round      ON events(gate_round_id, id)      WHERE gate_round_id IS NOT NULL;
CREATE INDEX idx_events_cycle           ON events(cycle_stamp, id)        WHERE cycle_stamp IS NOT NULL;

-- Executions: the cursor of the list, the executions of a cycle, and the ones that continue another.
CREATE INDEX idx_executions_started     ON executions(started_at, exec_id);
CREATE INDEX idx_executions_cycle       ON executions(cycle_stamp)        WHERE cycle_stamp IS NOT NULL;
CREATE INDEX idx_executions_parent      ON executions(parent_exec_id)     WHERE parent_exec_id IS NOT NULL;

-- A change's story: what a worker run and a merge attempt carry, and the episodes of the change.
CREATE INDEX idx_gate_rounds_run        ON gate_rounds(change_run_id)     WHERE change_run_id IS NOT NULL;
CREATE INDEX idx_gate_rounds_merge      ON gate_rounds(merge_attempt_id)  WHERE merge_attempt_id IS NOT NULL;
CREATE INDEX idx_evaluations_run        ON evaluations(change_run_id)     WHERE change_run_id IS NOT NULL;
CREATE INDEX idx_ready_manifests_change ON ready_manifests(change_name, id);
CREATE INDEX idx_rebuild_requests_change ON rebuild_requests(change_name, id);
CREATE INDEX idx_risk_approvals_change  ON risk_approvals(change_name, id);
CREATE INDEX idx_change_deferrals_change ON change_deferrals(change_name, id);

-- Aggregates since a date: gates, evaluations, attempts and worker runs.
CREATE INDEX idx_gate_runs_started      ON gate_runs(started_at);
CREATE INDEX idx_evaluations_created    ON evaluations(created_at);
CREATE INDEX idx_execution_attempts_started ON execution_attempts(started_at);
CREATE INDEX idx_change_runs_started    ON change_runs(started_at);

-- What a cycle produced, and the cycles running now.
CREATE INDEX idx_proposals_cycle        ON proposals(cycle_stamp)         WHERE cycle_stamp IS NOT NULL;
CREATE INDEX idx_visual_qa_items_cycle  ON visual_qa_items(cycle_stamp)   WHERE cycle_stamp IS NOT NULL;
CREATE INDEX idx_cycles_open            ON cycles(started_at)             WHERE finished_at IS NULL;

-- The proposals waiting for a decision, the changes that declare a lock, and the runs of a dispatch.
CREATE INDEX idx_proposals_status       ON proposals(status, proposed_at);
CREATE INDEX idx_change_locks_lock      ON change_locks(lock_name, change_name);
CREATE INDEX idx_change_runs_dispatcher ON change_runs(dispatcher_run_id) WHERE dispatcher_run_id IS NOT NULL;

-- What ended in a stretch of time: the runs, the blocks and the merge attempts the daemon counts to
-- tell whether the pipeline moved (Store.MovementBetween), and the latest results a page shows.
CREATE INDEX idx_change_runs_finished   ON change_runs(finished_at);
CREATE INDEX idx_change_blocks_blocked  ON change_blocks(blocked_at);
CREATE INDEX idx_merge_attempts_finished ON merge_attempts(finished_at);
