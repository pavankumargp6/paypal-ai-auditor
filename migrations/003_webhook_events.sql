-- Additive storage for verified PayPal webhook receipts.
-- event_id is unique so PayPal retries are acknowledged without duplicate receipts.
CREATE TABLE IF NOT EXISTS webhook_events (
    event_id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    payload JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT webhook_events_event_id_nonempty CHECK (length(event_id) > 0),
    CONSTRAINT webhook_events_type_nonempty CHECK (length(event_type) > 0)
);

CREATE INDEX IF NOT EXISTS webhook_events_received_at_idx
    ON webhook_events (received_at DESC);
