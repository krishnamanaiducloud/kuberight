CREATE TABLE recommendation_verification_runs (
    id text PRIMARY KEY CHECK (id ~ '^vfy_[0-9a-f]{32}$'),
    recommendation_id text NOT NULL REFERENCES recommendations (id) ON DELETE CASCADE,
    status text NOT NULL CHECK (status IN ('unavailable', 'inconclusive', 'not_applied', 'pending', 'verified', 'regressed')),
    configuration_observed boolean NOT NULL,
    first_observed_at timestamptz,
    observed_generation bigint NOT NULL DEFAULT 0 CHECK (observed_generation >= 0),
    observed_spec_fingerprint text NOT NULL DEFAULT '' CHECK (observed_spec_fingerprint = '' OR observed_spec_fingerprint ~ '^[0-9a-f]{64}$'),
    post_change_window_seconds bigint NOT NULL DEFAULT 0 CHECK (post_change_window_seconds >= 0),
    evidence_complete boolean NOT NULL,
    attribution text NOT NULL CHECK (attribution IN ('none', 'exact_pod_uid')),
    checks jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(checks) = 'array'),
    reasons jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(reasons) = 'array'),
    checked_at timestamptz NOT NULL,
    CONSTRAINT recommendation_verification_runs_observation CHECK (
        first_observed_at IS NULL OR (observed_generation > 0 AND observed_spec_fingerprint <> '')
    ),
    CONSTRAINT recommendation_verification_runs_complete CHECK (
        status NOT IN ('verified', 'regressed') OR
        (configuration_observed AND first_observed_at IS NOT NULL AND evidence_complete AND attribution = 'exact_pod_uid')
    ),
    UNIQUE (recommendation_id, id)
);

CREATE INDEX recommendation_verification_runs_history_idx
    ON recommendation_verification_runs (recommendation_id, checked_at DESC, id DESC);

CREATE TRIGGER recommendation_verification_runs_append_only
BEFORE UPDATE OR DELETE OR TRUNCATE ON recommendation_verification_runs
FOR EACH STATEMENT
EXECUTE FUNCTION kuberight_reject_audit_mutation();

CREATE TABLE recommendation_verification_outcomes (
    recommendation_id text PRIMARY KEY REFERENCES recommendations (id) ON DELETE CASCADE,
    latest_run_id text NOT NULL,
    status text NOT NULL CHECK (status IN ('unavailable', 'inconclusive', 'not_applied', 'pending', 'verified', 'regressed')),
    configuration_observed boolean NOT NULL,
    first_observed_at timestamptz,
    observed_generation bigint NOT NULL DEFAULT 0 CHECK (observed_generation >= 0),
    observed_spec_fingerprint text NOT NULL DEFAULT '' CHECK (observed_spec_fingerprint = '' OR observed_spec_fingerprint ~ '^[0-9a-f]{64}$'),
    post_change_window_seconds bigint NOT NULL DEFAULT 0 CHECK (post_change_window_seconds >= 0),
    evidence_complete boolean NOT NULL,
    attribution text NOT NULL CHECK (attribution IN ('none', 'exact_pod_uid')),
    checks jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(checks) = 'array'),
    reasons jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(reasons) = 'array'),
    checked_at timestamptz NOT NULL,
    regression_notified_at timestamptz,
    CONSTRAINT recommendation_verification_outcomes_run_fk
        FOREIGN KEY (recommendation_id, latest_run_id)
        REFERENCES recommendation_verification_runs (recommendation_id, id),
    CONSTRAINT recommendation_verification_outcomes_observation CHECK (
        first_observed_at IS NULL OR (observed_generation > 0 AND observed_spec_fingerprint <> '')
    ),
    CONSTRAINT recommendation_verification_outcomes_complete CHECK (
        status NOT IN ('verified', 'regressed') OR
        (configuration_observed AND first_observed_at IS NOT NULL AND evidence_complete AND attribution = 'exact_pod_uid')
    )
);

CREATE INDEX recommendation_verification_outcomes_checked_idx
    ON recommendation_verification_outcomes (checked_at DESC, recommendation_id DESC);
CREATE INDEX recommendation_verification_outcomes_status_idx
    ON recommendation_verification_outcomes (status, checked_at DESC, recommendation_id DESC);
