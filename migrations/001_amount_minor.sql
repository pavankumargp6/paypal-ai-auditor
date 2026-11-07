-- Additive migration: retain the legacy amount column for review and compatibility.
-- Cast the legacy double to numeric before rounding, and reject values with sub-minor precision.
-- Run manually only after reviewing the SQL and its verification checks.
ALTER TABLE captures ADD COLUMN IF NOT EXISTS amount_minor BIGINT;

DO $verify_legacy_precision$
DECLARE
    sub_minor_count BIGINT;
BEGIN
    SELECT COUNT(*)
    INTO sub_minor_count
    FROM captures
    WHERE ABS(
        amount::numeric * CASE
            WHEN UPPER(currency) IN ('JPY', 'HUF', 'TWD') THEN 1::numeric
            ELSE 100::numeric
        END
        - ROUND(amount::numeric * CASE
            WHEN UPPER(currency) IN ('JPY', 'HUF', 'TWD') THEN 1::numeric
            ELSE 100::numeric
        END)
    ) > 0.0001::numeric;

    IF sub_minor_count <> 0 THEN
        RAISE EXCEPTION 'legacy amount has sub-minor-unit precision in % rows', sub_minor_count;
    END IF;
END;
$verify_legacy_precision$;

UPDATE captures
SET amount_minor = ROUND(amount::numeric * CASE
    WHEN UPPER(currency) IN ('JPY', 'HUF', 'TWD') THEN 1::numeric
    ELSE 100::numeric
END)::BIGINT
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
    WHERE amount_minor IS DISTINCT FROM ROUND(amount::numeric * CASE
        WHEN UPPER(currency) IN ('JPY', 'HUF', 'TWD') THEN 1::numeric
        ELSE 100::numeric
    END)::BIGINT;

    IF mismatch_count <> 0 THEN
        RAISE EXCEPTION 'amount_minor backfill verification failed for % rows', mismatch_count;
    END IF;

    SELECT COALESCE(SUM(ROUND(amount::numeric * CASE
        WHEN UPPER(currency) IN ('JPY', 'HUF', 'TWD') THEN 1::numeric
        ELSE 100::numeric
    END)::BIGINT), 0)
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
