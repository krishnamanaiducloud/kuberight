-- Enforce the audit log's append-only contract in the database, not only in
-- repository code. The application role cannot silently rewrite, delete, or
-- truncate history even if a future code path accidentally issues such SQL.
CREATE FUNCTION kuberight_reject_audit_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only'
        USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER audit_events_append_only
BEFORE UPDATE OR DELETE OR TRUNCATE ON audit_events
FOR EACH STATEMENT
EXECUTE FUNCTION kuberight_reject_audit_mutation();
