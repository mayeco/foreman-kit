-- foreman-kit state database, schema version 2: what the idle watchdog sees while an agent runs.
--
-- An agent that goes idle is a fact with a beginning and an end, like a block or a pause. The row
-- opens once the watchdog has seen no activity from an attempt's processes for long enough to be
-- worth telling (no file changed, nothing printed, no command of its own using CPU), and closes
-- when the attempt works again, is stopped as stalled, or ends. The open rows are the agents that
-- `foreman-kit status` and the portal show as idle right now; the closed ones tell how long agents
-- that then went on working stayed idle, which is what limits.agent_idle_seconds has to stay above.

CREATE TABLE attempt_idle_periods (
    id             INTEGER PRIMARY KEY,
    attempt_id     INTEGER NOT NULL REFERENCES execution_attempts(id),
    -- The last activity the watchdog saw, which is earlier than the moment the row was written.
    started_at     INTEGER NOT NULL,
    -- The idle limit in force (limits.agent_idle_seconds): the attempt is stopped once idle this long.
    limit_seconds  INTEGER NOT NULL CHECK (limit_seconds > 0),
    ended_at       INTEGER,
    ended_by       TEXT CHECK (ended_by IN ('activity','stalled','exit')),
    CHECK ((ended_at IS NULL) = (ended_by IS NULL))
) STRICT;
CREATE UNIQUE INDEX ux_attempt_idle_periods_open ON attempt_idle_periods(attempt_id) WHERE ended_at IS NULL;
CREATE INDEX idx_attempt_idle_periods_attempt ON attempt_idle_periods(attempt_id, id);

-- The agents that are idle now: the open idle period of an attempt still running.
CREATE VIEW v_idle_agents AS
SELECT p.id AS idle_period_id, a.id AS attempt_id, a.engine,
       e.exec_id, e.kind, e.change_name, e.cycle_stamp, e.phase, e.pid, e.host,
       p.started_at AS idle_since, p.limit_seconds
FROM attempt_idle_periods p
JOIN execution_attempts a ON a.id = p.attempt_id
JOIN executions e ON e.exec_id = a.exec_id
WHERE p.ended_at IS NULL AND a.finished_at IS NULL AND e.status = 'running';
