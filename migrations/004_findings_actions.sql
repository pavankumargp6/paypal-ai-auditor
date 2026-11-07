-- Additive deterministic findings and action audit storage.
CREATE TABLE IF NOT EXISTS findings (
    id BIGSERIAL PRIMARY KEY,
    fingerprint TEXT NOT NULL UNIQUE,
    detector TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open',
    amount_minor BIGINT NOT NULL,
    currency TEXT NOT NULL,
    evidence JSONB NOT NULL,
    explanation TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT findings_status_check CHECK (status IN ('open', 'acknowledged', 'actioned', 'dismissed')),
    CONSTRAINT findings_amount_minor_check CHECK (amount_minor >= 0),
    CONSTRAINT findings_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT findings_detector_check CHECK (detector IN ('duplicate_capture', 'fee_mismatch'))
);

CREATE INDEX IF NOT EXISTS findings_status_created_at_idx
    ON findings (status, created_at DESC);

CREATE TABLE IF NOT EXISTS actions (
    id BIGSERIAL PRIMARY KEY,
    finding_id BIGINT NOT NULL REFERENCES findings(id),
    action_type TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'proposed',
    result JSONB NOT NULL DEFAULT '{"events":[],"money_moved":false}'::JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT actions_status_check CHECK (status IN ('proposed', 'approved', 'executed', 'failed', 'rejected')),
    CONSTRAINT actions_type_nonempty CHECK (length(action_type) > 0)
);

CREATE INDEX IF NOT EXISTS actions_finding_created_at_idx
    ON actions (finding_id, created_at DESC);
