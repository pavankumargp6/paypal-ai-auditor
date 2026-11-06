-- Additive storage for verified PayPal webhook receipts.
-- event_id is unique so PayPal retries can be acknowledged without duplicate receipts.
CREATE TABLE IF NOT EXISTS webhook_events (
    event_id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    resource_id TEXT,
    payload JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    process_error TEXT,
    CONSTRAINT webhook_events_event_id_nonempty CHECK (length(event_id) > 0),
    CONSTRAINT webhook_events_type_nonempty CHECK (length(event_type) > 0)
);

CREATE INDEX IF NOT EXISTS webhook_events_received_at_idx
    ON webhook_events (received_at DESC);

CREATE INDEX IF NOT EXISTS webhook_events_resource_id_idx
    ON webhook_events (resource_id);
