-- Additive migration: retain the legacy amount column for review and compatibility.
-- Run manually only after reviewing the SQL and its total-verification checks.
BEGIN;

ALTER TABLE captures ADD COLUMN IF NOT EXISTS amount_minor BIGINT;

UPDATE captures
SET amount_minor = ROUND(
    amount * CASE
        WHEN UPPER(currency) IN ('JPY', 'HUF', 'TWD') THEN 1
        ELSE 100
    END
)::BIGINT
WHERE amount_minor IS NULL;

DO $verify_amount_minor$
DECLARE
    mismatch_count BIGINT;
    legacy_total NUMERIC;
    minor_total NUMERIC;
BEGIN
    SELECT COUNT(*)
    INTO mismatch_count
    FROM captures
    WHERE amount_minor IS DISTINCT FROM ROUND(
        amount * CASE
            WHEN UPPER(currency) IN ('JPY', 'HUF', 'TWD') THEN 1
            ELSE 100
        END
    )::BIGINT;

    IF mismatch_count <> 0 THEN
        RAISE EXCEPTION 'amount_minor backfill verification failed for % rows', mismatch_count;
    END IF;

    SELECT COALESCE(SUM(ROUND(
        amount * CASE
            WHEN UPPER(currency) IN ('JPY', 'HUF', 'TWD') THEN 1
            ELSE 100
        END
    )::BIGINT), 0)
    INTO legacy_total
    FROM captures;

    SELECT COALESCE(SUM(amount_minor), 0)
    INTO minor_total
    FROM captures;

    IF legacy_total <> minor_total THEN
        RAISE EXCEPTION 'amount_minor total mismatch: legacy %, minor-unit %', legacy_total, minor_total;
    END IF;
END;
$verify_amount_minor$;

COMMIT;