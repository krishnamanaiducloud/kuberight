-- Removing this trigger permits destructive audit-history changes. Export and
-- protect retained history before intentionally rolling back this migration.
DROP TRIGGER IF EXISTS audit_events_append_only ON audit_events;
DROP FUNCTION IF EXISTS kuberight_reject_audit_mutation();
