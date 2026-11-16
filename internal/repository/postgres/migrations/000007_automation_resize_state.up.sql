-- What automation remembers between passes about an in-place resize that
-- has not reached every pod: the retry backoff and the time a halt was
-- first seen. Kept here so a restart does not reset it and every API
-- replica reads the same value.
CREATE TABLE automation_resize_state (
    recommendation_id text PRIMARY KEY REFERENCES recommendations (id) ON DELETE CASCADE,
    failures integer NOT NULL DEFAULT 0 CHECK (failures >= 0),
    next_attempt_at timestamptz,
    halted_since timestamptz,
    updated_at timestamptz NOT NULL
);
