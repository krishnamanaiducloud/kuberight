-- Rollback discards review decisions and provenance. Export them first.
DROP INDEX IF EXISTS recommendations_subject_latest_idx;
DROP INDEX IF EXISTS recommendations_review_status_idx;
ALTER TABLE recommendations
    DROP COLUMN IF EXISTS review_comment,
    DROP COLUMN IF EXISTS reviewed_at,
    DROP COLUMN IF EXISTS reviewed_by,
    DROP COLUMN IF EXISTS review_status,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS created_by;
