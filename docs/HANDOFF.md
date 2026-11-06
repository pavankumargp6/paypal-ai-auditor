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
- `eee9873 fix: make amount migration atomic and observable` — made the migration SQL run in the runner-owned transaction, inserted the `schema_migrations` row in that same transaction, redacted the DSN in database errors, reported and checked row counts/totals, and applied the migration once to Render after approval.
- `d3274a2 feat: preserve typed PayPal errors` — parses PayPal `name`, `message`, `details[].issue`, descriptions, and `debug_id` on non-2xx token/create/verify/capture/reconcile responses; empty/non-JSON responses retain the HTTP status and use `UNCLASSIFIED`. Tests cover structured errors and the empty 403 capture response.
- `6c07a82 ci: require postgres tests and show verbose output` — PostgreSQL integration tests now fail with `CI=true` when their dedicated DB URL is missing, and the CI test command uses `-v` so test names appear in logs.
- `c0f6f8b test: lock dashboard to minor-unit API fields` — added a test that ties the revenue card and grid to `total_captured_minor` / `amount_minor`, BigInt-based formatting, and the `$175.00` output for 17,500 USD minor units.
- `5904848 feat: add phase 1.2 config and insight cache` — added `APP_BASE_URL` redirects and documented `DATABASE_URL`, cached Groq insight by summary for 60 seconds, used PayPal capture timestamps, corrected the model name, and documented sandbox/auth/synthetic-data limitations.
- `69ff126 feat: record payment attempts and decline rate` — added best-effort attempt logging for create/approve/capture/reconcile, the additive attempts migration 002, and a real-only capture decline-rate query/UI with “No data yet” when unavailable.
- Current frontend/diagnostic and attempts-schema updates are included in the implementation commit; the hash will be recorded in the immediate handoff follow-up: forced AG Grid Source values to text; added Groq status/error-code-only failure diagnostics; extended attempt records with PayPal order, capture, and debug IDs; revised additive migration 002 and the disposable CI migration setup. Migration 002 remains unapplied.
- Sandbox evidence is in `docs/sandbox-findings.md`. Supported negative-test mock responses returned specific `details[].issue` values. `INSUFFICIENT_FUNDS` as a mock code returned HTTP 403 with an empty body and is not listed in PayPal's Orders v2 error reference.

## In progress / next step

- Phase 1.0 migration was approved and applied once to the user's Render Postgres database on 2026-11-06. The runner now executes the migration and records `schema_migrations` in one transaction, redacts `DATABASE_URL` from errors, and checks/reports row counts and total minor units before and after.
- Post-migration read verification: the locally started app on port 18080 returned HTTP 200 from `/api/transactions` with 10 rows and HTTP 200 plus valid JSON from `/api/spend-intelligence`. No capture endpoint was called. Port 8080 rejected an earlier bind attempt; a later listener check found no process bound to it, so the earlier owner could not be identified. Nothing was stopped or killed.
- Current local checks: `gofmt -l .` returned no files, `go vet ./...` exited 0, and `go test -race -count=1 ./...` passed with `ok paypal-ai-auditor 2.393s`; `cmd/migrate` compiled. The three PostgreSQL integration cases skip because `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL` is unset locally; in CI, a missing URL is now fatal.
- GitHub Actions run [Go CI #42208728526](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42208728526) for the attempts step completed successfully. The verbose log shows all three PostgreSQL tests passed: `TestPostgresAdvisoryLockSerializesSameOrder`, `TestPostgresLegacyMinorFallback`, and `TestPostgresLockPersistenceFailureRetryReconciles`.
- Phase 1.2 and attempts-table implementation are committed and pass local tests. Migration 002 is ready for review but has not been run. Stop for explicit approval before executing it against Render. Webhooks are not started. Phase 2 decline classification still requires checking PayPal documentation for buyer-recoverable semantics before final wording.

## Decisions and reasons

- Parse PayPal amount strings directly into `int64` minor units. JPY, HUF, and TWD use exponent 0; other currencies default to 2. API minor-unit fields are JSON strings to avoid JavaScript precision loss.
- The live-record query uses `COALESCE(amount_minor, ROUND(amount::numeric * currency_factor)::BIGINT)` because the currently deployed app may continue inserting rows with `amount_minor = NULL`.
- `initDB` includes `amount_minor` in `CREATE TABLE IF NOT EXISTS` for fresh databases. It does not alter an existing table. Until the migration adds the column, an existing database remains unchanged; the new read query will fail with `column amount_minor does not exist`, so database-backed dashboard and capture operations cannot work with the new app until the migration is applied.
- Advisory locking uses a transaction-scoped PostgreSQL lock. Capture persistence receives that transaction through context and writes on the same connection, avoiding a second pool connection while concurrent requests wait for the lock. Pool limits are 8 open / 4 idle, with 5-minute maximum lifetime and 1-minute idle lifetime.
- PostgreSQL integration tests use only `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL`, never `DATABASE_URL`, so local test runs do not accidentally target Render. GitHub Actions now supplies a PostgreSQL service and that dedicated URL.
- PayPal return/cancel URLs are built from `APP_BASE_URL` (default `http://localhost:8080`). Groq insights are cached in memory for 60 seconds by the complete summary payload, including a per-key in-flight request guard; the cache is bounded to 128 summaries. Capture rows use PayPal `create_time`, falling back to `update_time`.
- Payment attempt writes run asynchronously through a separate pooled database operation and only log a fixed failure message if insertion fails; they do not share or abort the capture advisory-lock transaction. The decline rate counts capture-stage, non-synthetic attempts; known sandbox decline issues increment the numerator. Until migration 002 is applied, the dashboard shows “No data yet” and attempt inserts fail best-effort without breaking payment flow.
- `psql` is not installed in the current environment. `cmd/migrate` is a one-shot Go runner using existing PostgreSQL and dotenv dependencies. It creates `schema_migrations` if needed, then executes the migration, verifies/reports totals, and records the migration filename in one transaction. A rerun after a successful record skips it; an interrupted unrecorded migration can safely be retried because the SQL is idempotent.
- The legacy `amount` column remains `NOT NULL`; new capture inserts keep it populated from minor units in SQL for compatibility. Reads, validation, totals, API output, and frontend formatting use minor units.

## Known issues and unverified items

- The migration output was `rows before: 7`, `total minor units before: 17500`, `rows after: 7`, `total minor units after: 17500`, `migration applied: 001_amount_minor.sql`. The legacy `amount` column remains present and the migration is recorded in `schema_migrations`.
- Migration 002 at `migrations/002_attempts.sql` has not been run. No Render database writes have been made for attempts. After the user explicitly approves it, the runner command is `go run ./cmd/migrate ./migrations/002_attempts.sql`.
- The current CI workflow applies migrations 001 and 002 to its disposable PostgreSQL service before tests. Successful attempt persistence and the database-backed decline-rate query will be exercised there after the workflow push; migration 002 remains unapplied to Render.
- The dedicated PostgreSQL test URL is unset locally; the advisory-lock, legacy-fallback, and actual-lock persistence-retry tests skip locally. They ran against the PostgreSQL 16 service in successful CI run #42208728526. The current CI workflow migration changes still need a new Actions run.
- The transaction-aware retry path is covered locally with mocked PayPal and a simulated persistence failure. The env-gated integration test exercises it with a real PostgreSQL advisory-lock transaction and mocked PayPal when the dedicated test DB is available.
- PayPal negative-test mock responses are not actual buyer/card decline transactions. `INSUFFICIENT_FUNDS` classification remains unverified.
- The app has no authentication. Live payments are out of scope; use only PayPal Sandbox credentials. The Groq diagnostic probe returned HTTP 401 with error code `invalid_api_key`; the key and response body were not printed. Groq lists `openai/gpt-oss-20b` as a supported production model in its current model docs: https://console.groq.com/docs/models and https://console.groq.com/docs/model/openai/gpt-oss-20b.
- Before finalizing decline labels, verify from PayPal docs that `buyer_recoverable` means the buyer can retry with another funding source; do not imply recovery is guaranteed. Seeded failures must be labeled simulated.

## Commands (run from the repository root in PowerShell)

Tests and static checks:

```powershell
$goExe = 'C:\Program Files\Go\bin\go.exe'
& $goExe version
& $goExe vet ./...
& $goExe test -race -count=1 -v ./...
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

`psql` is not available here. The approved migration has already been applied once. Its invocation was:

```powershell
& 'C:\Program Files\Go\bin\go.exe' run ./cmd/migrate ./migrations/001_amount_minor.sql
```

The user approved and the migration has already been run once. `cmd/migrate` reads `DATABASE_URL` without printing its value; it creates `schema_migrations` if needed, and applies the migration plus tracking record in the same transaction. Do not rerun it as part of this handoff unless the user requests it.
