CREATE TABLE notification_outbox (
    id text PRIMARY KEY CHECK (id ~ '^ntf_[0-9a-f]{32}$'),
    event_type text NOT NULL CHECK (length(event_type) BETWEEN 1 AND 128),
    payload jsonb NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'delivered', 'dead')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    delivered_at timestamptz,
    last_error text NOT NULL DEFAULT '' CHECK (length(last_error) <= 1024),
    CONSTRAINT notification_outbox_delivered CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);

CREATE INDEX notification_outbox_due ON notification_outbox (next_attempt_at, id) WHERE status = 'pending';
CREATE INDEX notification_outbox_listing ON notification_outbox (status, created_at DESC, id);
