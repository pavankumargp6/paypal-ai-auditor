# Revenue Guardian Handoff

Updated: 2026-11-06
Repository: `C:\dev\paypal-ai-auditor`
Branch: `feat/revenue-guardian`

## Goal and scope

Build a sandbox-only PayPal revenue-leakage assistant while preserving the existing create, approve, capture, reconcile, and persistence-pending flow. Money in Go and API/UI calculations is integer minor units. Keep the legacy `captures.amount` column until the user approves its removal.

## Done

- `2576ea8 docs: record sandbox findings and ignore executables` — added Sandbox negative-test findings, scanned the document for credential-shaped content, and ignored `*.exe`. The pre-existing executable is not tracked.
- `5f61a4a feat: store payment amounts in minor units` — added integer minor-unit parsing, string-encoded JSON fields, exact frontend formatting, and the additive `amount_minor` migration.
- `6f0c387 fix: serialize PayPal order captures` — added deterministic `PayPal-Request-Id`, PostgreSQL per-order advisory locking, already-captured reconciliation, and a 12-request mocked concurrency test.
- `0b43af7 fix: preserve legacy captures and verify lock recovery` — hardened the additive migration, added the legacy-row `COALESCE` fallback, set DB pool limits, wrote capture rows on the advisory-lock transaction, added env-gated PostgreSQL tests/CI, and added the migration runner.
- Sandbox evidence is in `docs/sandbox-findings.md`. Supported negative-test mock responses returned specific `details[].issue` values. `INSUFFICIENT_FUNDS` as a mock code returned HTTP 403 with an empty body and is not listed in PayPal's Orders v2 error reference.

## In progress / next step

- Phase 1.0 migration was approved and applied once to the user's Render Postgres database on 2026-11-06. The runner now executes the migration and records `schema_migrations` in one transaction, redacts `DATABASE_URL` from errors, and checks/reports row counts and total minor units before and after.
- Post-migration read verification: the locally started app on port 18080 returned HTTP 200 from `/api/transactions` with 10 rows and HTTP 200 plus valid JSON from `/api/spend-intelligence`. No capture endpoint was called. Port 8080 was already occupied, so the verification instance used `PORT=18080` and was stopped afterward.
- Current local checks: `gofmt -l .` returned no files, `go vet ./...` exited 0, and `go test -race -count=1 ./...` passed with `ok paypal-ai-auditor 2.270s`; `cmd/migrate` compiled. The three PostgreSQL integration cases skip because `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL` is unset locally.
- GitHub Actions run [Go CI #42201501029](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42201501029) for commit `3457d8a3fc6484847f0c577748e92c5becd30034` completed successfully on 2026-11-06. Its PostgreSQL 16 service and dedicated `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL` were configured for `go test -race -count=1 ./...`; all three PostgreSQL integration tests ran (they skip only when that variable is empty): `TestPostgresAdvisoryLockSerializesSameOrder`, `TestPostgresLegacyMinorFallback`, and `TestPostgresLockPersistenceFailureRetryReconciles`.
- After review/application as directed, continue Phase 1.1 typed PayPal errors, then attempts and webhooks. Phase 2 decline classification still requires checking PayPal documentation for buyer-recoverable semantics before final wording.

## Decisions and reasons

- Parse PayPal amount strings directly into `int64` minor units. JPY, HUF, and TWD use exponent 0; other currencies default to 2. API minor-unit fields are JSON strings to avoid JavaScript precision loss.
- The live-record query uses `COALESCE(amount_minor, ROUND(amount::numeric * currency_factor)::BIGINT)` because the currently deployed app may continue inserting rows with `amount_minor = NULL`.
- `initDB` includes `amount_minor` in `CREATE TABLE IF NOT EXISTS` for fresh databases. It does not alter an existing table. Until the migration adds the column, an existing database remains unchanged; the new read query will fail with `column amount_minor does not exist`, so database-backed dashboard and capture operations cannot work with the new app until the migration is applied.
- Advisory locking uses a transaction-scoped PostgreSQL lock. Capture persistence receives that transaction through context and writes on the same connection, avoiding a second pool connection while concurrent requests wait for the lock. Pool limits are 8 open / 4 idle, with 5-minute maximum lifetime and 1-minute idle lifetime.
- PostgreSQL integration tests use only `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL`, never `DATABASE_URL`, so local test runs do not accidentally target Render. GitHub Actions now supplies a PostgreSQL service and that dedicated URL.
- `psql` is not installed in the current environment. `cmd/migrate` is a one-shot Go runner using existing PostgreSQL and dotenv dependencies. It creates `schema_migrations`, applies the SQL file, and records the migration filename after success. A rerun after a successful record skips it; an interrupted unrecorded migration can safely be retried because the SQL is idempotent.
- The legacy `amount` column remains `NOT NULL`; new capture inserts keep it populated from minor units in SQL for compatibility. Reads, validation, totals, API output, and frontend formatting use minor units.

## Known issues and unverified items

- The migration output was `rows before: 7`, `total minor units before: 17500`, `rows after: 7`, `total minor units after: 17500`, `migration applied: 001_amount_minor.sql`. The legacy `amount` column remains present and the migration is recorded in `schema_migrations`.
- The dedicated PostgreSQL test URL is unset locally; the advisory-lock, legacy-fallback, and actual-lock persistence-retry tests skip locally. They ran against the PostgreSQL 16 service in successful CI run #42201501029.
- The transaction-aware retry path is covered locally with mocked PayPal and a simulated persistence failure. The env-gated integration test exercises it with a real PostgreSQL advisory-lock transaction and mocked PayPal when the dedicated test DB is available.
- PayPal negative-test mock responses are not actual buyer/card decline transactions. `INSUFFICIENT_FUNDS` classification remains unverified.
- The app has no authentication. Live payments are out of scope; use only PayPal Sandbox credentials.
- Before finalizing decline labels, verify from PayPal docs that `buyer_recoverable` means the buyer can retry with another funding source; do not imply recovery is guaranteed. Seeded failures must be labeled simulated.

## Commands (run from the repository root in PowerShell)

Tests and static checks:

```powershell
$goExe = 'C:\Program Files\Go\bin\go.exe'
& $goExe version
& $goExe vet ./...
& $goExe test -race -count=1 ./...
```

Start the app (requires `DATABASE_URL`, PayPal Sandbox credentials, and the correct database schema in `.env`):

```powershell
& 'C:\Program Files\Go\bin\go.exe' run .
```

Run PostgreSQL integration tests against a dedicated disposable local test database only:

```powershell
$env:PAYPAL_AI_AUDITOR_TEST_DATABASE_URL = 'postgres://test:test@localhost:5432/paypal_ai_auditor_test?sslmode=disable'
& 'C:\Program Files\Go\bin\go.exe' test -race -count=1 ./...
```

`psql` is not available here. After the user reviews and explicitly authorizes applying the migration to Render, run this from the repository root:

```powershell
& 'C:\Program Files\Go\bin\go.exe' run ./cmd/migrate ./migrations/001_amount_minor.sql
```

The user approved and the migration has already been run once. `cmd/migrate` reads `DATABASE_URL` without printing its value; it creates `schema_migrations` if needed, and applies the migration plus tracking record in the same transaction. Do not rerun it as part of this handoff unless the user requests it.
