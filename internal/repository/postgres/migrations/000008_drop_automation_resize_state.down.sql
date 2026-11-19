CREATE TABLE automation_resize_state (
    recommendation_id text PRIMARY KEY REFERENCES recommendations (id) ON DELETE CASCADE,
    failures integer NOT NULL DEFAULT 0 CHECK (failures >= 0),
    next_attempt_at timestamptz,
    halted_since timestamptz,
    updated_at timestamptz NOT NULL
);
