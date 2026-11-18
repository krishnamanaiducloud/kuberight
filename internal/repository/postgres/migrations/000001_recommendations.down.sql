-- Rollback is intentionally explicit and ordered by dependency. Operators must
-- back up retained audit history before applying this destructive migration.
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS recommendation_idempotency;
DROP TABLE IF EXISTS recommendations;
