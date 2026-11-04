CREATE TABLE policies (
    id text PRIMARY KEY CHECK (id ~ '^pol_[0-9a-f]{32}$'),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 128),
    description text NOT NULL DEFAULT '' CHECK (octet_length(description) <= 2048),
    scope_level text NOT NULL CHECK (scope_level IN ('global', 'namespace', 'workload', 'container')),
    cluster_id text NOT NULL DEFAULT '',
    namespace text NOT NULL DEFAULT '',
    workload_kind text NOT NULL DEFAULT '',
    workload_name text NOT NULL DEFAULT '',
    container_name text NOT NULL DEFAULT '',
    priority integer NOT NULL DEFAULT 0 CHECK (priority BETWEEN -1000 AND 1000),
    enabled boolean NOT NULL DEFAULT true,
    policy jsonb NOT NULL CHECK (jsonb_typeof(policy) = 'object'),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL,
    created_by text NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 512),
    updated_at timestamptz NOT NULL,
    updated_by text NOT NULL CHECK (char_length(updated_by) BETWEEN 1 AND 512),
    CHECK (
        (scope_level = 'global' AND namespace = '' AND workload_kind = '' AND workload_name = '' AND container_name = '') OR
        (scope_level = 'namespace' AND namespace <> '' AND workload_kind = '' AND workload_name = '' AND container_name = '') OR
        (scope_level = 'workload' AND namespace <> '' AND workload_kind <> '' AND workload_name <> '' AND container_name = '') OR
        (scope_level = 'container' AND namespace <> '' AND workload_kind <> '' AND workload_name <> '' AND container_name <> '')
    )
);

CREATE INDEX policies_resolution_idx
    ON policies (enabled, cluster_id, namespace, workload_kind, workload_name, container_name, priority DESC, updated_at DESC);

CREATE TABLE fleet_schedules (
    id text PRIMARY KEY CHECK (id ~ '^fsc_[0-9a-f]{32}$'),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 128),
    enabled boolean NOT NULL,
    interval_seconds bigint NOT NULL CHECK (interval_seconds BETWEEN 300 AND 2592000),
    scope jsonb NOT NULL CHECK (jsonb_typeof(scope) = 'object'),
    policy_id text NOT NULL DEFAULT '',
    window_seconds bigint NOT NULL CHECK (window_seconds BETWEEN 3600 AND 2592000),
    next_run_at timestamptz NOT NULL,
    last_run_at timestamptz,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL,
    created_by text NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 512),
    updated_at timestamptz NOT NULL,
    updated_by text NOT NULL CHECK (char_length(updated_by) BETWEEN 1 AND 512)
);

CREATE INDEX fleet_schedules_due_idx ON fleet_schedules (next_run_at, id) WHERE enabled;

CREATE TABLE fleet_scans (
    id text PRIMARY KEY CHECK (id ~ '^fsn_[0-9a-f]{32}$'),
    schedule_id text NOT NULL DEFAULT '',
    scheduled_for timestamptz NOT NULL,
	available_at timestamptz NOT NULL,
    scope jsonb NOT NULL CHECK (jsonb_typeof(scope) = 'object'),
    policy_id text NOT NULL DEFAULT '',
    window_seconds bigint NOT NULL CHECK (window_seconds BETWEEN 3600 AND 2592000),
    status text NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'failed', 'canceled')),
    expected integer NOT NULL DEFAULT 0 CHECK (expected >= 0),
    processed integer NOT NULL DEFAULT 0 CHECK (processed >= 0 AND processed <= expected),
    succeeded integer NOT NULL DEFAULT 0 CHECK (succeeded >= 0),
    failed integer NOT NULL DEFAULT 0 CHECK (failed >= 0),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    max_attempts integer NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
    lease_owner text NOT NULL DEFAULT '',
    lease_expires_at timestamptz,
    last_error text NOT NULL DEFAULT '' CHECK (octet_length(last_error) <= 4096),
	failure_details jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(failure_details) = 'array'),
    created_at timestamptz NOT NULL,
    created_by text NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 512),
    started_at timestamptz,
    completed_at timestamptz,
    cancellation_at timestamptz,
    idempotency_hash text UNIQUE,
	request_fingerprint text NOT NULL CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    CHECK (succeeded + failed <= processed),
    CHECK ((lease_owner = '') = (lease_expires_at IS NULL))
);

CREATE UNIQUE INDEX fleet_scans_schedule_slot_idx
    ON fleet_scans (schedule_id, scheduled_for) WHERE schedule_id <> '';
CREATE INDEX fleet_scans_claim_idx
    ON fleet_scans (available_at, id) WHERE status IN ('queued', 'running');
CREATE INDEX fleet_scans_history_idx ON fleet_scans (created_at DESC, id DESC);
