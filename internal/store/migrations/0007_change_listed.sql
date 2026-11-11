-- foreman-kit state database, schema version 7: when the main checkout first listed each change.
--
-- A change a cycle queued is on record, unknown, until the backlog of the main checkout lists it
-- (changes.cycle_stamp). A change somebody took out of the backlog is unknown too, and keeps the cycle
-- that queued it: the two could not be told apart. listed_at is the hour the mirror of the main
-- checkout first found the change in its backlog, and it stays when the change leaves the backlog.
-- NULL: the main checkout has never listed the change.
--
-- For the changes already on record it is the hour they were queued, or first seen. A change that is
-- queued, integrated or discarded was in the backlog, and so was one that has a run, because only a
-- queued change is built. An unknown change with no run stays NULL: nothing on record says the
-- backlog ever listed it.

ALTER TABLE changes ADD COLUMN listed_at INTEGER;

UPDATE changes SET listed_at = COALESCE(queued_at, first_seen_at)
WHERE backlog_status IN ('queued', 'integrated', 'discarded')
   OR EXISTS (SELECT 1 FROM change_runs r WHERE r.change_name = changes.change_name);
