-- foreman-kit state database, schema version 5: what started each cycle.
--
-- The daemon no longer starts the cycle's judgment only on its tick: with an idle delay set it also
-- starts it before the tick, once the pipeline has run out of work. A cycle therefore says what
-- started it: the daemon as it came up ('startup'), its tick ('interval'), a pipeline out of work
-- ('out_of_work'), or someone who asked for it from the command line, a scheduler or the portal
-- ('manual'). NULL: a cycle recorded before this column existed.

ALTER TABLE cycles ADD COLUMN trigger TEXT
    CHECK (trigger IS NULL OR trigger IN ('startup', 'interval', 'out_of_work', 'manual'));
