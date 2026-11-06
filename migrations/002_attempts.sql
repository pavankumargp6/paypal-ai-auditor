-- Additive attempts history for PayPal create, approval, capture, and reconcile outcomes.
-- Decline rate denominator is real capture outcomes (succeeded, declined, failed); it excludes synthetic rows, reconciles, and ORDER_ALREADY_CAPTURED.
-- CI runs both additive migrations against a disposable PostgreSQL service database.
CREATE TABLE IF NOT EXISTS attempts (
    id BIGSERIAL PRIMARY KEY,
    paypal_order_id TEXT,
    capture_id TEXT,
    debug_id TEXT,
    stage TEXT NOT NULL,
    outcome TEXT NOT NULL,
    reason_code TEXT,
    reason_detail TEXT,
    http_status INTEGER,
    amount_minor BIGINT NOT NULL,
    currency TEXT NOT NULL,
    is_synthetic BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT attempts_stage_check CHECK (stage IN ('create', 'approve', 'capture', 'reconcile', 'webhook')),
    CONSTRAINT attempts_outcome_check CHECK (outcome IN ('succeeded', 'failed', 'declined', 'reconciled')),
    CONSTRAINT attempts_http_status_check CHECK (http_status IS NULL OR http_status BETWEEN 100 AND 599),
    CONSTRAINT attempts_amount_minor_check CHECK (amount_minor >= 0),
    CONSTRAINT attempts_currency_check CHECK (currency ~ '^[A-Z]{3}$')
);

CREATE INDEX IF NOT EXISTS attempts_paypal_order_created_at_idx
    ON attempts (paypal_order_id, created_at DESC)
    WHERE paypal_order_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS attempts_real_capture_created_at_idx
    ON attempts (created_at DESC)
    WHERE stage = 'capture'
      AND is_synthetic = FALSE
      AND outcome IN ('succeeded', 'declined', 'failed')
      AND COALESCE(reason_code, '') <> 'ORDER_ALREADY_CAPTURED';
