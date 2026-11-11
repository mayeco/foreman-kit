-- foreman-kit state database, schema version 6: which cycle queued each change.
--
-- A proposal already says which cycle proposed it (proposals.cycle_stamp). A change says it too, so
-- that a cycle's page lists what the cycle queued and a measurement does not have to infer it from
-- the hour. NULL: a change queued by a person, or before this column existed.

ALTER TABLE changes ADD COLUMN cycle_stamp TEXT REFERENCES cycles(stamp);
CREATE INDEX idx_changes_cycle ON changes(cycle_stamp) WHERE cycle_stamp IS NOT NULL;
