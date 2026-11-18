-- The caller applies this entire file in one PostgreSQL transaction and records
-- its SHA-256 checksum only after commit.
CREATE TABLE recommendations (
    id text PRIMARY KEY,
    cluster text NOT NULL DEFAULT '',
    namespace text NOT NULL,
    workload_kind text NOT NULL DEFAULT '',
    workload text NOT NULL,
    container text NOT NULL,
    confidence text NOT NULL,
    risk text NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),

    CONSTRAINT recommendations_id_format CHECK (id ~ '^rec_[0-9a-f]{32}$'),
    CONSTRAINT recommendations_cluster_length CHECK (octet_length(cluster) <= 253),
    CONSTRAINT recommendations_namespace_length CHECK (octet_length(namespace) BETWEEN 1 AND 253),
    CONSTRAINT recommendations_kind_length CHECK (octet_length(workload_kind) <= 128),
    CONSTRAINT recommendations_workload_length CHECK (octet_length(workload) BETWEEN 1 AND 253),
    CONSTRAINT recommendations_container_length CHECK (octet_length(container) BETWEEN 1 AND 253),
    CONSTRAINT recommendations_confidence_value CHECK (confidence IN ('low', 'medium', 'high')),
    CONSTRAINT recommendations_risk_value CHECK (risk IN ('low', 'medium', 'high')),
    CONSTRAINT recommendations_payload_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT recommendations_payload_id_matches CHECK ((payload ->> 'id') IS NOT DISTINCT FROM id),
    CONSTRAINT recommendations_payload_cluster_matches CHECK (COALESCE(payload ->> 'cluster', '') = cluster),
    CONSTRAINT recommendations_payload_namespace_matches CHECK ((payload ->> 'namespace') IS NOT DISTINCT FROM namespace),
    CONSTRAINT recommendations_payload_kind_matches CHECK (COALESCE(payload ->> 'kind', '') = workload_kind),
    CONSTRAINT recommendations_payload_workload_matches CHECK ((payload ->> 'workload') IS NOT DISTINCT FROM workload),
    CONSTRAINT recommendations_payload_container_matches CHECK ((payload ->> 'container') IS NOT DISTINCT FROM container),
    CONSTRAINT recommendations_payload_confidence_matches CHECK ((payload ->> 'confidence') IS NOT DISTINCT FROM confidence),
    CONSTRAINT recommendations_payload_risk_matches CHECK ((payload ->> 'risk') IS NOT DISTINCT FROM risk)
);

CREATE INDEX recommendations_created_at_idx ON recommendations (created_at DESC, id DESC);
CREATE INDEX recommendations_namespace_workload_idx ON recommendations (namespace, workload, created_at DESC, id DESC);
CREATE INDEX recommendations_confidence_idx ON recommendations (confidence, created_at DESC, id DESC);
CREATE INDEX recommendations_risk_idx ON recommendations (risk, created_at DESC, id DESC);

-- Insert the claim before the recommendation. The deferred foreign key permits
-- that order, so concurrent requests contend on the primary key before either
-- can create an orphan recommendation. The winning transaction must then write
-- the referenced recommendation before commit.
CREATE TABLE recommendation_idempotency (
    scope_digest bytea NOT NULL,
    idempotency_key text NOT NULL,
    request_fingerprint bytea NOT NULL,
    recommendation_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),

    CONSTRAINT recommendation_idempotency_pk PRIMARY KEY (scope_digest, idempotency_key),
    CONSTRAINT recommendation_idempotency_recommendation_unique UNIQUE (recommendation_id),
    CONSTRAINT recommendation_idempotency_scope_digest_length CHECK (octet_length(scope_digest) = 32),
    CONSTRAINT recommendation_idempotency_key_format CHECK (idempotency_key ~ '^[A-Za-z0-9._:-]{1,128}$'),
    CONSTRAINT recommendation_idempotency_fingerprint_length CHECK (octet_length(request_fingerprint) = 32),
    CONSTRAINT recommendation_idempotency_recommendation_fk
        FOREIGN KEY (recommendation_id)
        REFERENCES recommendations (id)
        ON DELETE CASCADE
        DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX recommendation_idempotency_created_at_idx ON recommendation_idempotency (created_at, scope_digest, idempotency_key);

-- Audit events are append-only for the application role. They intentionally do
-- not reference recommendations: retention of a recommendation must not erase
-- or mutate the historical security record that names it in resource.
CREATE TABLE audit_events (
    id text PRIMARY KEY,
    event_timestamp timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    actor text NOT NULL,
    action text NOT NULL,
    resource text NOT NULL,
    namespace text,
    outcome text NOT NULL,
    request_id text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,

    CONSTRAINT audit_events_id_format CHECK (id ~ '^evt_[0-9a-f]{32}$'),
    CONSTRAINT audit_events_actor_length CHECK (octet_length(actor) BETWEEN 1 AND 512),
    CONSTRAINT audit_events_action_format CHECK (action ~ '^[a-z][a-z0-9_.:-]{0,127}$'),
    CONSTRAINT audit_events_resource_length CHECK (octet_length(resource) BETWEEN 1 AND 512),
    CONSTRAINT audit_events_namespace_length CHECK (namespace IS NULL OR octet_length(namespace) BETWEEN 1 AND 253),
    CONSTRAINT audit_events_outcome_format CHECK (outcome ~ '^[a-z][a-z0-9_.:-]{0,63}$'),
    CONSTRAINT audit_events_request_id_length CHECK (octet_length(request_id) BETWEEN 1 AND 128),
    CONSTRAINT audit_events_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX audit_events_timestamp_idx ON audit_events (event_timestamp DESC, id DESC);
CREATE INDEX audit_events_actor_idx ON audit_events (actor, event_timestamp DESC, id DESC);
CREATE INDEX audit_events_namespace_idx ON audit_events (namespace, event_timestamp DESC, id DESC) WHERE namespace IS NOT NULL;
CREATE INDEX audit_events_request_id_idx ON audit_events (request_id);
