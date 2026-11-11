-- foreman-kit state database, schema version 3: the evaluator is no longer named after its provider.
--
-- The model that answers the gates' calibrated questions is "the evaluator" in the configuration, in
-- the commands and in the events; Jev is the model that implements it today. The two tables that hold
-- what it was asked take that name, and so do the events already written, so that one name reads the
-- whole history.

ALTER TABLE jev_evaluations RENAME TO evaluations;
DROP INDEX idx_jev_evaluations_change;
CREATE INDEX idx_evaluations_change ON evaluations(change_name, id);

ALTER TABLE jev_cache RENAME TO evaluation_cache;

-- jev.answer, jev.route and jev.unavailable become evaluator.answer, evaluator.route and
-- evaluator.unavailable.
UPDATE events SET event = 'evaluator.' || substr(event, 5) WHERE event LIKE 'jev.%';
