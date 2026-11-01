-- Adds recommendation provenance and single-assignment review state. Reviews
-- record a human decision only; KubeRight never applies a recommendation.
ALTER TABLE recommendations
    ADD COLUMN created_by text NOT NULL DEFAULT '',
    ADD COLUMN source text NOT NULL DEFAULT 'request',
    ADD COLUMN review_status text NOT NULL DEFAULT 'pending',
    ADD COLUMN reviewed_by text,
    ADD COLUMN reviewed_at timestamptz,
    ADD COLUMN review_comment text,
    ADD CONSTRAINT recommendations_created_by_length CHECK (octet_length(created_by) <= 512),
    ADD CONSTRAINT recommendations_source_value CHECK (source IN ('request', 'prometheus')),
    ADD CONSTRAINT recommendations_review_status_value CHECK (review_status IN ('pending', 'approved', 'rejected')),
    ADD CONSTRAINT recommendations_review_complete CHECK (
        (review_status = 'pending' AND reviewed_by IS NULL AND reviewed_at IS NULL AND review_comment IS NULL)
        OR (
            review_status <> 'pending'
            AND reviewed_by IS NOT NULL
            AND octet_length(reviewed_by) BETWEEN 1 AND 512
            AND reviewed_at IS NOT NULL
            AND (review_comment IS NULL OR octet_length(review_comment) BETWEEN 1 AND 1024)
        )
    );

CREATE INDEX recommendations_review_status_idx ON recommendations (review_status, created_at DESC, id DESC);

-- Supports DISTINCT ON queries that select the newest recommendation for each
-- container, which savings estimates and the approvals queue rely on.
CREATE INDEX recommendations_subject_latest_idx
    ON recommendations (cluster, namespace, workload_kind, workload, container, created_at DESC, id DESC);
