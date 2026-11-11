-- foreman-kit state database, schema version 1.
--
-- Conventions: every *_at column (and cooldown_until) is a Unix time in milliseconds; booleans are 0 or 1;
-- every table is STRICT; no foreign key cascades, because history is never deleted. Current state is
-- derived by the views at the end: nothing stores it twice.

-- =====================================================================
-- 1. CATALOG: the things everything else points at
-- =====================================================================

CREATE TABLE proposals (
    proposal_id    TEXT PRIMARY KEY CHECK (length(proposal_id) > 0),
    kind           TEXT NOT NULL DEFAULT 'change',
    status         TEXT NOT NULL DEFAULT 'proposed',
    priority       TEXT NOT NULL DEFAULT 'normal',
    impact_score   REAL,
    source         TEXT,
    strategy       TEXT,
    description    TEXT NOT NULL DEFAULT '',
    evidence       TEXT,
    target_prompt  TEXT,
    edits_json     TEXT CHECK (edits_json   IS NULL OR json_valid(edits_json)),
    applied_json   TEXT CHECK (applied_json IS NULL OR json_valid(applied_json)),
    review_json    TEXT CHECK (review_json  IS NULL OR json_valid(review_json)),
    cycle_stamp    TEXT REFERENCES cycles(stamp),
    proposed_at    INTEGER NOT NULL,
    queued_at      INTEGER,
    integrated_at  INTEGER,
    extra_json     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(extra_json)),
    updated_at     INTEGER NOT NULL
) STRICT;

CREATE TABLE changes (
    change_name        TEXT PRIMARY KEY CHECK (length(change_name) > 0),
    backlog_status     TEXT NOT NULL DEFAULT 'unknown'
                       CHECK (backlog_status IN ('unknown','queued','integrated','discarded')),
    priority           TEXT NOT NULL DEFAULT 'normal',
    impact_score       REAL,
    kind               TEXT,
    source             TEXT,
    strategy           TEXT,
    description        TEXT NOT NULL DEFAULT '',
    proposal_id        TEXT REFERENCES proposals(proposal_id),
    first_seen_at      INTEGER NOT NULL,
    queued_at          INTEGER,
    integrated_at      INTEGER,
    integrated_commit  TEXT,
    discarded_at       INTEGER,
    extra_json         TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(extra_json)),
    updated_at         INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_changes_backlog ON changes(backlog_status, priority);

CREATE TABLE change_dependencies (
    change_name  TEXT NOT NULL REFERENCES changes(change_name),
    depends_on   TEXT NOT NULL REFERENCES changes(change_name),
    PRIMARY KEY (change_name, depends_on),
    CHECK (change_name <> depends_on)
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_change_dependencies_reverse ON change_dependencies(depends_on);

CREATE TABLE change_locks (
    change_name  TEXT NOT NULL REFERENCES changes(change_name),
    lock_name    TEXT NOT NULL CHECK (length(lock_name) > 0),
    PRIMARY KEY (change_name, lock_name)
) STRICT, WITHOUT ROWID;

CREATE TABLE engines (
    engine                TEXT PRIMARY KEY CHECK (length(engine) > 0),
    installed             INTEGER NOT NULL DEFAULT 1 CHECK (installed IN (0, 1)),
    health_status         TEXT NOT NULL DEFAULT 'unknown'
                          CHECK (health_status IN ('unknown','ok','auth_failed','quota_exceeded',
                                                   'config_error','not_installed','timeout')),
    consecutive_failures  INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    last_latency_ms       INTEGER NOT NULL DEFAULT 0,
    last_error            TEXT,
    last_checked_at       INTEGER,
    cooldown_until        INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE repos (
    repo_path  TEXT PRIMARY KEY CHECK (length(repo_path) > 0),
    name       TEXT NOT NULL UNIQUE
) STRICT;

-- =====================================================================
-- 2. CYCLE
-- =====================================================================

CREATE TABLE cycles (
    stamp           TEXT PRIMARY KEY CHECK (length(stamp) > 0),
    pid             INTEGER,
    host            TEXT,
    outcome         TEXT NOT NULL DEFAULT 'running',
    strategy        TEXT,
    judge_engine    TEXT REFERENCES engines(engine),
    umbrella_sha    TEXT,
    findings_count  INTEGER NOT NULL DEFAULT 0,
    report_path     TEXT,
    log_path        TEXT,
    started_at      INTEGER NOT NULL,
    finished_at     INTEGER
) STRICT;
CREATE INDEX idx_cycles_started ON cycles(started_at DESC);

-- =====================================================================
-- 3. WORK: dispatcher run -> change run -> execution -> attempt -> log
-- =====================================================================

CREATE TABLE dispatcher_runs (
    id            INTEGER PRIMARY KEY,
    pid           INTEGER NOT NULL,
    host          TEXT NOT NULL,
    mode          TEXT NOT NULL CHECK (mode IN ('once','continuous')),
    workers       INTEGER NOT NULL CHECK (workers >= 1),
    backlog_size  INTEGER NOT NULL DEFAULT 0,
    end_reason    TEXT,
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER
) STRICT;

CREATE TABLE change_runs (
    id                  INTEGER PRIMARY KEY,
    change_name         TEXT NOT NULL REFERENCES changes(change_name),
    dispatcher_run_id   INTEGER REFERENCES dispatcher_runs(id),
    pid                 INTEGER NOT NULL,
    host                TEXT NOT NULL,
    slot                INTEGER NOT NULL,
    worktree            TEXT NOT NULL,
    branch              TEXT NOT NULL,
    origin              TEXT NOT NULL DEFAULT 'backlog' CHECK (origin IN ('backlog','retry','rebuild')),
    retry_of_block_id   INTEGER REFERENCES change_blocks(id),
    rebuild_request_id  INTEGER REFERENCES rebuild_requests(id),
    result              TEXT CHECK (result IN ('ready','blocked','held','deferred','paused','stopped','lost')),
    started_at          INTEGER NOT NULL,
    finished_at         INTEGER,
    UNIQUE (id, change_name),
    CHECK ((result IS NULL) = (finished_at IS NULL))
) STRICT;
CREATE UNIQUE INDEX ux_change_runs_open ON change_runs(change_name) WHERE finished_at IS NULL;
CREATE INDEX idx_change_runs_change ON change_runs(change_name, id);

CREATE TABLE executions (
    exec_id            TEXT PRIMARY KEY CHECK (length(exec_id) > 0),
    kind               TEXT NOT NULL CHECK (kind IN ('worker','cycle','portal_run','portal_job')),
    change_name        TEXT REFERENCES changes(change_name),
    change_run_id      INTEGER,
    cycle_stamp        TEXT REFERENCES cycles(stamp),
    parent_exec_id     TEXT REFERENCES executions(exec_id),
    parent_relation    TEXT,
    phase              TEXT,
    role               TEXT,
    engines_requested  TEXT NOT NULL DEFAULT '',
    engine_used        TEXT REFERENCES engines(engine),
    status             TEXT NOT NULL DEFAULT 'running'
                       CHECK (status IN ('running','succeeded','failed','stopped','timed_out')),
    exit_code          INTEGER,
    pid                INTEGER,
    host               TEXT,
    workdir            TEXT,
    prompt_text        TEXT,
    prompt_version     TEXT,
    memory_version     TEXT,
    job_kind           TEXT,
    command_text       TEXT,
    summary            TEXT,
    log_pruned_at      INTEGER,
    started_at         INTEGER NOT NULL,
    finished_at        INTEGER,
    FOREIGN KEY (change_run_id, change_name) REFERENCES change_runs(id, change_name),
    CHECK ((status = 'running') = (finished_at IS NULL)),
    CHECK (status = 'running' OR exit_code IS NOT NULL),
    CHECK (kind <> 'worker' OR change_name IS NOT NULL),
    CHECK (kind <> 'cycle'  OR cycle_stamp IS NOT NULL),
    CHECK (change_run_id IS NULL OR change_name IS NOT NULL)
) STRICT;
CREATE INDEX idx_executions_change ON executions(change_name, started_at);
CREATE INDEX idx_executions_run    ON executions(change_run_id);
CREATE INDEX idx_executions_kind   ON executions(kind, started_at);
CREATE INDEX idx_executions_open   ON executions(status) WHERE status = 'running';

CREATE TABLE execution_attempts (
    id            INTEGER PRIMARY KEY,
    exec_id       TEXT NOT NULL REFERENCES executions(exec_id),
    attempt_no    INTEGER NOT NULL CHECK (attempt_no >= 1),
    engine        TEXT NOT NULL REFERENCES engines(engine),
    try_no        INTEGER NOT NULL DEFAULT 0 CHECK (try_no >= 0),
    exit_code     INTEGER,
    outcome       TEXT CHECK (outcome IN ('ok','error','timeout','idle_stalled','quota_exceeded',
                                          'auth_failed','not_installed','context_overflow',
                                          'too_large','cancelled')),
    idle_seconds  INTEGER,
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER,
    UNIQUE (exec_id, attempt_no),
    CHECK ((outcome IS NULL) = (finished_at IS NULL))
) STRICT;
CREATE INDEX idx_execution_attempts_engine ON execution_attempts(engine, started_at);

-- A process group an execution started (an agent CLI of one attempt, or the command of a portal job):
-- recorded so that a later orchestrator can stop it when the one that started it died.
CREATE TABLE execution_process_groups (
    exec_id      TEXT NOT NULL REFERENCES executions(exec_id),
    pgid         INTEGER NOT NULL CHECK (pgid > 0),
    attempt_id   INTEGER REFERENCES execution_attempts(id),
    recorded_at  INTEGER NOT NULL,
    PRIMARY KEY (exec_id, pgid)
) STRICT, WITHOUT ROWID;

CREATE TABLE log_chunks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    exec_id     TEXT NOT NULL REFERENCES executions(exec_id),
    attempt_id  INTEGER REFERENCES execution_attempts(id),
    ts          INTEGER NOT NULL,
    stream      TEXT NOT NULL DEFAULT 'combined' CHECK (stream IN ('combined','stdout','stderr','system')),
    content     TEXT NOT NULL
) STRICT;
CREATE INDEX idx_log_chunks_exec ON log_chunks(exec_id, id);

CREATE TABLE gate_rounds (
    id                INTEGER PRIMARY KEY,
    context           TEXT NOT NULL CHECK (context IN ('worker_verify','merger','base_verify','cli')),
    change_name       TEXT REFERENCES changes(change_name),
    change_run_id     INTEGER,
    merge_attempt_id  INTEGER REFERENCES merge_attempts(id),
    after_exec_id     TEXT REFERENCES executions(exec_id),
    slot              INTEGER NOT NULL DEFAULT 0,
    base_ref          TEXT,
    head_sha          TEXT,
    result            TEXT CHECK (result IN ('passed','failed','stopped')),
    started_at        INTEGER NOT NULL,
    finished_at       INTEGER,
    FOREIGN KEY (change_run_id, change_name) REFERENCES change_runs(id, change_name),
    CHECK ((result IS NULL) = (finished_at IS NULL)),
    CHECK (change_run_id IS NULL OR change_name IS NOT NULL),
    CHECK (context <> 'worker_verify' OR change_run_id IS NOT NULL),
    CHECK (context <> 'merger' OR merge_attempt_id IS NOT NULL)
) STRICT;
CREATE INDEX idx_gate_rounds_change ON gate_rounds(change_name, id);

CREATE TABLE gate_runs (
    id               INTEGER PRIMARY KEY,
    gate_round_id    INTEGER NOT NULL REFERENCES gate_rounds(id),
    repo_path        TEXT NOT NULL REFERENCES repos(repo_path),
    gate_name        TEXT NOT NULL,
    command          TEXT NOT NULL,
    exit_code        INTEGER NOT NULL,
    duration_ms      INTEGER NOT NULL CHECK (duration_ms >= 0),
    failure_excerpt  TEXT,
    output_path      TEXT,
    started_at       INTEGER NOT NULL,
    finished_at      INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_gate_runs_round ON gate_runs(gate_round_id, id);

CREATE TABLE repair_rounds (
    id                    INTEGER PRIMARY KEY,
    change_run_id         INTEGER NOT NULL REFERENCES change_runs(id),
    round_no              INTEGER NOT NULL CHECK (round_no >= 1),
    phase                 TEXT NOT NULL,
    exec_id               TEXT REFERENCES executions(exec_id),
    asked_reason          TEXT NOT NULL,
    asked_details         TEXT NOT NULL,
    asked_gate_round_id   INTEGER REFERENCES gate_rounds(id),
    changed_diffstat      TEXT,
    result_passed         INTEGER NOT NULL DEFAULT 0 CHECK (result_passed IN (0, 1)),
    result_next_reason    TEXT,
    result_gate_round_id  INTEGER REFERENCES gate_rounds(id),
    created_at            INTEGER NOT NULL,
    UNIQUE (change_run_id, phase, round_no)
) STRICT;

-- =====================================================================
-- 4. JUDGEMENTS (Jev) and LLM diagnostics
-- =====================================================================

CREATE TABLE jev_cache (
    cache_key      TEXT PRIMARY KEY,
    gate           TEXT NOT NULL,
    model          TEXT NOT NULL,
    state_json     TEXT NOT NULL,
    response_json  TEXT NOT NULL,
    created_at     INTEGER NOT NULL
) STRICT;

CREATE TABLE jev_evaluations (
    id                 INTEGER PRIMARY KEY,
    gate               TEXT NOT NULL,
    mode               TEXT,
    status             TEXT NOT NULL,
    model              TEXT,
    cached             INTEGER NOT NULL DEFAULT 0 CHECK (cached IN (0, 1)),
    cache_key          TEXT,
    cost_usd           REAL NOT NULL DEFAULT 0,
    answers_json       TEXT CHECK (answers_json IS NULL OR json_valid(answers_json)),
    route_action       TEXT,
    route_detail_json  TEXT CHECK (route_detail_json IS NULL OR json_valid(route_detail_json)),
    change_name        TEXT REFERENCES changes(change_name),
    change_run_id      INTEGER REFERENCES change_runs(id),
    merge_attempt_id   INTEGER REFERENCES merge_attempts(id),
    exec_id            TEXT REFERENCES executions(exec_id),
    created_at         INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_jev_evaluations_change ON jev_evaluations(change_name, id);

CREATE TABLE llm_cache (
    cache_key      TEXT PRIMARY KEY,
    task_kind      TEXT NOT NULL,
    model          TEXT NOT NULL,
    response_json  TEXT NOT NULL,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL
) STRICT;

-- =====================================================================
-- 5. QUARANTINE: one table per fact, history never overwritten
-- =====================================================================

CREATE TABLE change_blocks (
    id                INTEGER PRIMARY KEY,
    change_name       TEXT NOT NULL REFERENCES changes(change_name),
    change_run_id     INTEGER,
    merge_attempt_id  INTEGER REFERENCES merge_attempts(id),
    reason            TEXT NOT NULL CHECK (length(reason) > 0),
    details           TEXT,
    stage             TEXT,
    worktree          TEXT,
    branch            TEXT,
    host              TEXT,
    retry_num         INTEGER NOT NULL DEFAULT 0 CHECK (retry_num >= 0),
    retry_told        INTEGER NOT NULL DEFAULT 0 CHECK (retry_told IN (0, 1)),
    blocked_at        INTEGER NOT NULL,
    resolved_at       INTEGER,
    -- 'cleared' lifts the block and keeps the change's counters; 'unblocked' is an operator lifting it
    -- and starting the counters over, as 'integrated' and 'abandoned' also do.
    resolution        TEXT CHECK (resolution IN ('auto_retry','cleared','unblocked','approved','superseded',
                                                 'integrated','abandoned')),
    resolved_by       TEXT,
    FOREIGN KEY (change_run_id, change_name) REFERENCES change_runs(id, change_name),
    CHECK ((resolved_at IS NULL) = (resolution IS NULL))
) STRICT;
CREATE UNIQUE INDEX ux_change_blocks_open ON change_blocks(change_name) WHERE resolved_at IS NULL;
CREATE INDEX idx_change_blocks_change ON change_blocks(change_name, id);

CREATE TABLE rebuild_requests (
    id                INTEGER PRIMARY KEY,
    change_name       TEXT NOT NULL REFERENCES changes(change_name),
    merge_attempt_id  INTEGER REFERENCES merge_attempts(id),
    branch            TEXT NOT NULL,
    reason            TEXT,
    conflict_details  TEXT,
    rebuild_num       INTEGER NOT NULL CHECK (rebuild_num >= 1),
    requested_at      INTEGER NOT NULL,
    consumed_at       INTEGER
) STRICT;
CREATE UNIQUE INDEX ux_rebuild_requests_pending ON rebuild_requests(change_name) WHERE consumed_at IS NULL;

CREATE TABLE risk_approvals (
    id             INTEGER PRIMARY KEY,
    change_name    TEXT NOT NULL REFERENCES changes(change_name),
    block_id       INTEGER REFERENCES change_blocks(id),
    approved_by    TEXT NOT NULL,
    approved_at    INTEGER NOT NULL,
    revoked_at     INTEGER,
    revoke_reason  TEXT,
    CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL))
) STRICT;
CREATE UNIQUE INDEX ux_risk_approvals_active ON risk_approvals(change_name) WHERE revoked_at IS NULL;

CREATE TABLE change_deferrals (
    id             INTEGER PRIMARY KEY,
    change_name    TEXT NOT NULL REFERENCES changes(change_name),
    change_run_id  INTEGER REFERENCES change_runs(id),
    role           TEXT NOT NULL,
    counted        INTEGER NOT NULL DEFAULT 1 CHECK (counted IN (0, 1)),
    retry_at       INTEGER,
    output_tail    TEXT,
    deferred_at    INTEGER NOT NULL,
    cleared_at     INTEGER
) STRICT;
CREATE INDEX idx_change_deferrals_open ON change_deferrals(change_name) WHERE cleared_at IS NULL;

-- =====================================================================
-- 6. INTEGRATION: manifest -> merge attempt -> pushes
-- =====================================================================

CREATE TABLE ready_manifests (
    id                  INTEGER PRIMARY KEY,
    change_name         TEXT NOT NULL REFERENCES changes(change_name),
    change_run_id       INTEGER,
    branch              TEXT NOT NULL,
    worktree            TEXT NOT NULL,
    head_sha            TEXT NOT NULL CHECK (length(head_sha) > 0),
    risk_evaluation_id  INTEGER REFERENCES jev_evaluations(id),
    assessment_json     TEXT CHECK (assessment_json IS NULL OR json_valid(assessment_json)),
    item_json           TEXT CHECK (item_json IS NULL OR json_valid(item_json)),
    enqueued_at         INTEGER NOT NULL,
    closed_at           INTEGER,
    outcome             TEXT CHECK (outcome IN ('integrated','sent_back','blocked','removed',
                                                'invalid','already_integrated')),
    FOREIGN KEY (change_run_id, change_name) REFERENCES change_runs(id, change_name),
    CHECK ((closed_at IS NULL) = (outcome IS NULL))
) STRICT;
CREATE UNIQUE INDEX ux_ready_manifests_open ON ready_manifests(change_name) WHERE closed_at IS NULL;
CREATE INDEX idx_ready_manifests_queue ON ready_manifests(enqueued_at, id) WHERE closed_at IS NULL;

CREATE TABLE manifest_capabilities (
    manifest_id  INTEGER NOT NULL REFERENCES ready_manifests(id),
    capability   TEXT NOT NULL CHECK (length(capability) > 0),
    PRIMARY KEY (manifest_id, capability)
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_manifest_capabilities_cap ON manifest_capabilities(capability);

CREATE TABLE merge_attempts (
    id                 INTEGER PRIMARY KEY,
    manifest_id        INTEGER NOT NULL REFERENCES ready_manifests(id),
    pid                INTEGER NOT NULL,
    host               TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'CLAIMED'
                       CHECK (state IN ('CLAIMED','POLICY_CHECK','REBASING_CHILDREN','CHECKING_WORK_LOSS',
                                        'ASSEMBLING','AUDITING_TASKS','VERIFYING_BASE','RUNNING_GATES',
                                        'ARCHIVING','COMMITTING','PUSHING_CHILDREN','PUSHING_ROOT',
                                        'COMPLETED')),
    manifest_head_sha  TEXT NOT NULL,
    branch_tip_sha     TEXT,
    result             TEXT CHECK (result IN ('integrated','sent_back','blocked','push_rejected',
                                              'already_integrated','held','stopped')),
    result_reason      TEXT,
    integrated_commit  TEXT,
    specs_json         TEXT CHECK (specs_json IS NULL OR json_valid(specs_json)),
    resumed_from_id    INTEGER REFERENCES merge_attempts(id),
    started_at         INTEGER NOT NULL,
    finished_at        INTEGER,
    CHECK ((finished_at IS NULL) = (result IS NULL)),
    CHECK (result <> 'integrated' OR integrated_commit IS NOT NULL)
) STRICT;
CREATE UNIQUE INDEX ux_merge_attempts_open ON merge_attempts((finished_at IS NULL)) WHERE finished_at IS NULL;
CREATE INDEX idx_merge_attempts_manifest ON merge_attempts(manifest_id, id);

CREATE TABLE manifest_pushes (
    manifest_id       INTEGER NOT NULL REFERENCES ready_manifests(id),
    repo_path         TEXT NOT NULL REFERENCES repos(repo_path),
    pushed_sha        TEXT NOT NULL CHECK (length(pushed_sha) > 0),
    merge_attempt_id  INTEGER NOT NULL REFERENCES merge_attempts(id),
    pushed_at         INTEGER NOT NULL,
    PRIMARY KEY (manifest_id, repo_path)
) STRICT, WITHOUT ROWID;

CREATE TABLE base_verifications (
    id                INTEGER PRIMARY KEY,
    repo_path         TEXT NOT NULL REFERENCES repos(repo_path),
    base_sha          TEXT NOT NULL,
    check_name        TEXT NOT NULL,
    run_no            INTEGER NOT NULL DEFAULT 1 CHECK (run_no >= 1),
    origin            TEXT NOT NULL DEFAULT 'merge' CHECK (origin IN ('merge','recheck')),
    status            TEXT NOT NULL CHECK (status IN ('passed','failed','accepted')),
    exit_code         INTEGER NOT NULL DEFAULT 0,
    excerpt           TEXT,
    pointer_sha       TEXT,
    outside_commits   INTEGER NOT NULL DEFAULT 0,
    gate_round_id     INTEGER REFERENCES gate_rounds(id),
    merge_attempt_id  INTEGER REFERENCES merge_attempts(id),
    created_at        INTEGER NOT NULL,
    accepted_by       TEXT,
    accepted_at       INTEGER,
    resolved_at       INTEGER,
    superseded_at     INTEGER,
    UNIQUE (repo_path, base_sha, check_name, run_no),
    CHECK ((status = 'accepted') = (accepted_at IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX ux_base_verifications_current
    ON base_verifications(repo_path, base_sha, check_name) WHERE superseded_at IS NULL;

-- =====================================================================
-- 7. VISUAL QA
-- =====================================================================

CREATE TABLE visual_qa_items (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    change_name   TEXT REFERENCES changes(change_name),
    what          TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','in_progress','verified','regression','not-visual',
                                    'cannot-verify','dropped')),
    verdict_note  TEXT,
    requested_by  TEXT,
    cycle_stamp   TEXT REFERENCES cycles(stamp),
    exec_id       TEXT REFERENCES executions(exec_id),
    created_at    INTEGER NOT NULL,
    started_at    INTEGER,
    resolved_at   INTEGER
) STRICT;
CREATE INDEX idx_visual_qa_items_status ON visual_qa_items(status, id);

-- =====================================================================
-- 8. OPERATIONS: workspace, pauses, locks, claims, events
-- =====================================================================

CREATE TABLE workspace (
    id                  INTEGER PRIMARY KEY CHECK (id = 1),
    workspace_id        TEXT NOT NULL,
    root_path           TEXT NOT NULL,
    merger_stop_pid     INTEGER,
    visual_qa_last_sha  TEXT,
    last_health_json    TEXT CHECK (last_health_json IS NULL OR json_valid(last_health_json)),
    last_health_at      INTEGER,
    updated_at          INTEGER NOT NULL
) STRICT;

CREATE TABLE workspace_pauses (
    id           INTEGER PRIMARY KEY,
    reason       TEXT NOT NULL,
    paused_by    TEXT NOT NULL CHECK (paused_by IN ('dispatcher','merger','portal','cli')),
    change_name  TEXT REFERENCES changes(change_name),
    paused_at    INTEGER NOT NULL,
    resumed_at   INTEGER,
    resumed_by   TEXT,
    CHECK ((resumed_at IS NULL) = (resumed_by IS NULL))
) STRICT;
CREATE UNIQUE INDEX ux_workspace_pauses_open ON workspace_pauses((resumed_at IS NULL)) WHERE resumed_at IS NULL;

CREATE TABLE locks (
    lock_key       TEXT PRIMARY KEY,
    owner_type     TEXT NOT NULL,
    pid            INTEGER NOT NULL,
    host           TEXT NOT NULL,
    slot           INTEGER,
    stamp          TEXT,
    acquired_at    INTEGER NOT NULL,
    heartbeat_at   INTEGER NOT NULL,
    ttl_seconds    INTEGER NOT NULL CHECK (ttl_seconds > 0),
    metadata_json  TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json))
) STRICT;

CREATE TABLE engine_claims (
    claim_id     INTEGER PRIMARY KEY,
    owner_pid    INTEGER NOT NULL,
    host         TEXT NOT NULL,
    engine       TEXT NOT NULL REFERENCES engines(engine),
    role         TEXT,
    change_name  TEXT REFERENCES changes(change_name),
    exec_id      TEXT REFERENCES executions(exec_id),
    claimed_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE events (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                 INTEGER NOT NULL,
    event              TEXT NOT NULL CHECK (length(event) > 0),
    pid                INTEGER NOT NULL,
    host               TEXT,
    change_name        TEXT REFERENCES changes(change_name),
    exec_id            TEXT REFERENCES executions(exec_id),
    change_run_id      INTEGER REFERENCES change_runs(id),
    merge_attempt_id   INTEGER REFERENCES merge_attempts(id),
    dispatcher_run_id  INTEGER REFERENCES dispatcher_runs(id),
    cycle_stamp        TEXT REFERENCES cycles(stamp),
    gate_round_id      INTEGER REFERENCES gate_rounds(id),
    repo_path          TEXT REFERENCES repos(repo_path),
    engine             TEXT REFERENCES engines(engine),
    phase              TEXT,
    payload_json       TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload_json))
) STRICT;
CREATE INDEX idx_events_change ON events(change_name, id) WHERE change_name IS NOT NULL;
CREATE INDEX idx_events_exec   ON events(exec_id, id)     WHERE exec_id IS NOT NULL;
CREATE INDEX idx_events_run    ON events(change_run_id, id) WHERE change_run_id IS NOT NULL;
CREATE INDEX idx_events_event  ON events(event, id);
CREATE INDEX idx_events_ts     ON events(ts);

-- =====================================================================
-- 9. VIEWS: current state is derived, never stored twice
-- =====================================================================

CREATE VIEW v_change_state AS
SELECT c.change_name,
       CASE
         WHEN c.backlog_status = 'integrated' THEN 'integrated'
         WHEN c.backlog_status = 'discarded'  THEN 'discarded'
         WHEN EXISTS (SELECT 1 FROM change_blocks b
                      WHERE b.change_name = c.change_name AND b.resolved_at IS NULL) THEN 'blocked'
         WHEN EXISTS (SELECT 1 FROM merge_attempts m JOIN ready_manifests r ON r.id = m.manifest_id
                      WHERE r.change_name = c.change_name AND m.finished_at IS NULL) THEN 'merging'
         WHEN EXISTS (SELECT 1 FROM ready_manifests r
                      WHERE r.change_name = c.change_name AND r.closed_at IS NULL) THEN 'ready'
         WHEN EXISTS (SELECT 1 FROM change_runs r
                      WHERE r.change_name = c.change_name AND r.finished_at IS NULL) THEN 'building'
         WHEN EXISTS (SELECT 1 FROM rebuild_requests q
                      WHERE q.change_name = c.change_name AND q.consumed_at IS NULL) THEN 'rebuild_requested'
         ELSE c.backlog_status
       END AS state
FROM changes c;

CREATE VIEW v_merger AS
SELECT m.id AS merge_attempt_id, r.change_name, m.state, m.pid, m.host, m.started_at
FROM merge_attempts m JOIN ready_manifests r ON r.id = m.manifest_id
WHERE m.finished_at IS NULL;

CREATE VIEW v_workspace_paused AS
SELECT id AS pause_id, reason, paused_by, change_name, paused_at
FROM workspace_pauses WHERE resumed_at IS NULL;

CREATE VIEW v_deferral_count AS
SELECT change_name, COUNT(*) AS deferrals, MAX(deferred_at) AS last_deferred_at
FROM change_deferrals WHERE cleared_at IS NULL AND counted = 1
GROUP BY change_name;
