-- Additive attempts history for PayPal create, approval, capture, and reconcile outcomes.
-- The migration runner wraps this file and its schema_migrations record in one transaction.
CREATE TABLE IF NOT EXISTS attempts (
    id BIGSERIAL PRIMARY KEY,
    stage TEXT NOT NULL,
    outcome TEXT NOT NULL,
    reason_code TEXT NOT NULL,
    reason_detail TEXT NOT NULL,
    http_status INTEGER,
    amount_minor BIGINT NOT NULL,
    currency TEXT NOT NULL,
    is_synthetic BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS attempts_real_capture_created_at_idx
    ON attempts (created_at DESC)
    WHERE stage = 'capture' AND is_synthetic = FALSE;
