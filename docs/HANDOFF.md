# Revenue Guardian Handoff

Updated: 2026-11-06
Repository: `C:\dev\paypal-ai-auditor`
Branch: `feat/revenue-guardian`

## Goal and scope

Build a sandbox-only PayPal revenue-leakage assistant while preserving the existing create, approve, capture, reconcile, and persistence-pending flow. Money in Go and API/UI calculations is integer minor units. Keep the legacy `captures.amount` column until the user approves its removal.

## Done

- `ccfdac0 feat: process PayPal capture webhooks idempotently` — revised migration 003 with additive resource/processing fields and an index; process completed, denied, and refunded capture webhooks into attempts with transactional deduplication and retryable failures; preserve raw signed JSON bytes and require exact PayPal `SUCCESS`; tightened decline-rate codes, documented payer-data and decline semantics, and added unit plus PostgreSQL retry coverage. Local vet and race suite passed; all five PostgreSQL integration tests skipped locally because the dedicated test URL is unset.
- `ee356e2 docs: record webhook processing handoff` — recorded the webhook implementation and next verification step.
- `9da3f2a docs: record pushed webhook branch status` — recorded the pushed ref and CI watch location.
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
- Groq verification: current official docs list `openai/gpt-oss-20b`; the local request returned HTTP 401 / `invalid_api_key`, so the model is listed as valid but the configured credential was rejected.
- `85e27ab feat: allow webhook attempt stage` — added `webhook` to the attempts stage CHECK and updated its migration regression test. `go vet ./...` and `go test -race -count=1 ./...` pass locally (`ok paypal-ai-auditor 2.374s`).
- `0d6b1b7 feat: verify and deduplicate PayPal webhooks` — added `POST /webhooks/paypal`, raw-body verification through PayPal Sandbox, invalid-signature 401, idempotent event_id receipt storage, additive migration 003, `PAYPAL_WEBHOOK_ID`, and mocked/PostgreSQL tests. Webhook events are persisted only; no payment side effects.
- `b467ba1 feat: ground insights in cited real captures` — filters LLM evidence to real completed captures joined to non-synthetic attempts, requires strict JSON Schema output at temperature 0, validates cited order/capture pairs against evidence, caches by evidence hash, returns “Not enough data yet” with no evidence, and displays citations in the dashboard. `gofmt -l .` returned no files, `go vet ./...` passed, and `go test -race -count=1 ./...` passed (`ok paypal-ai-auditor 2.404s`).
- `c66e589 fix: clarify source and prepare attempt schema` — fixed AG Grid Source rendering, added status/error-code-only Groq diagnostics, extended attempts with PayPal order/capture/debug IDs, revised additive migration 002, excluded reconciles and already-captured outcomes from decline-rate input, and configured disposable CI migrations.
- Sandbox evidence is in `docs/sandbox-findings.md`. Supported negative-test mock responses returned specific `details[].issue` values. `INSUFFICIENT_FUNDS` as a mock code returned HTTP 403 with an empty body and is not listed in PayPal's Orders v2 error reference.

## In progress / next step

- Added `TestPostgresConcurrentDuplicateWebhookPostsWriteOneAttempt`: it sends two real concurrent handler POSTs with the same event ID to mocked PayPal verification, holds the winner at its attempt insert, confirms the other PostgreSQL session is waiting on the receipt row's `SELECT ... FOR UPDATE`, then releases the winner and asserts one attempt insert call and exactly one `webhook` attempt row. The local race suite passes; the env-gated Postgres test skips locally and must be confirmed in CI after push.
- Next: commit and push this integration test, confirm the new test passes in CI, then pause. No agent loop work is authorized yet.
- At the previous pushed revision, `HEAD` and the remote ref matched at `9da3f2afa8942546b6194e59ca0051998ab50eed`. GitHub Actions run [#42235597732](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42235597732) passed after applying migrations 001–003. The workflow sets `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL`, so the five PostgreSQL integration tests in that revision ran rather than skipped. The new concurrent duplicate-delivery test awaits CI.
- Migration 003 was revised with additive `resource_id`, `processed_at`, and `process_error` columns plus a `resource_id` index. The approved command was `go run ./cmd/migrate ./migrations/003_webhook_events.sql`; it was applied to the user's Render database. The runner reported 0 rows and 0 minor units before and after. Read-only schema inspection showed `event_id`, `event_type`, `resource_id`, `payload`, `received_at`, `processed_at`, and `process_error`; `schema_migrations` contains 001, 002, and 003.
- Decline rate now uses only `INSTRUMENT_DECLINED`, `PAYER_CANNOT_PAY`, and `PAYMENT_DENIED` as numerator codes, among real capture-stage attempts. README states these codes come from sandbox negative testing, defines recoverable as retrying with another funding source without guaranteeing recovery, and warns that webhook payloads may contain payer data (sandbox only).
- Current local checks: `go vet ./...` passed; `go test -race -count=1 -v ./...` passed unit tests. All six PostgreSQL tests, including the new concurrency test, skipped because `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL` is unset locally.
- Phase 1.0 migration was approved and applied once to the user's Render Postgres database on 2026-11-06. The runner now executes the migration and records `schema_migrations` in one transaction, redacts `DATABASE_URL` from errors, and checks/reports row counts and total minor units before and after.
- Post-migration read verification: the locally started app on port 18080 returned HTTP 200 from `/api/transactions` with 10 rows and HTTP 200 plus valid JSON from `/api/spend-intelligence`. No capture endpoint was called. Port 8080 rejected an earlier bind attempt; a later listener check found no process bound to it, so the earlier owner could not be identified. Nothing was stopped or killed.
- Current local checks: `gofmt -l .` returned no files, `go vet ./...` exited 0, and `go test -race -count=1 ./...` passed with `ok paypal-ai-auditor 2.552s`; `cmd/migrate` compiled. The three PostgreSQL integration cases skip because `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL` is unset locally; in CI, a missing URL is now fatal.
- GitHub Actions run [Go CI #42208728526](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42208728526) for the attempts step completed successfully. The verbose log shows all three PostgreSQL tests passed: `TestPostgresAdvisoryLockSerializesSameOrder`, `TestPostgresLegacyMinorFallback`, and `TestPostgresLockPersistenceFailureRetryReconciles`.
- Phase 1.2, attempts, webhooks, and strict evidence-based AI are implemented. No PayPal capture side effects are performed by webhook handling. Pause before the planned agent loop.

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

- Historical note: migration 001 was previously applied to the former database, which the user has since deleted; that run reported 7 rows and 17,500 minor units before and after. Current replacement-database results are recorded below.
- On the replacement empty database, read-only inspection confirmed `captures.amount_minor` exists and `captures` had 0 rows; `schema_migrations` initially contained only migration 001. Migrations 001, 002, and approved 003 were applied with `cmd/migrate`; each reported 0 rows and 0 minor units before and after. Attempts columns were verified after 002; webhook columns were verified after 003. No credentials were printed.
- Historical CI run [#42221126619](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42221126619) passed with migrations 001–003 applied to disposable PostgreSQL. The updated webhook retry integration test is pending a new CI run.
- The dedicated PostgreSQL test URL is unset locally, so all five database integration tests skip locally. The new webhook retry integration test must be confirmed in CI after this branch push.
- The transaction-aware retry path is covered locally with mocked PayPal and a simulated persistence failure. The env-gated integration test exercises it with a real PostgreSQL advisory-lock transaction and mocked PayPal when the dedicated test DB is available.
- Local app verification on port 18080 against the replacement database passed: `/api/spend-intelligence` returned HTTP 200, `total_captured_minor=0`, `decline_rate_available=false`, and `ai_insight=Not enough data yet`; `/` returned HTTP 200 and the page contains the “No data yet” decline fallback, Source labels, and citation rendering. No PayPal capture was triggered. The app process was stopped after the check.
- Webhook handling stores payloads that may contain payer data; do not log payloads and use this endpoint for Sandbox only. Supported capture events write webhook-stage attempts; unknown event types are persisted and marked processed without an attempt.
- PayPal negative-test mock responses are not actual buyer/card decline transactions. `INSUFFICIENT_FUNDS` classification remains unverified.
- The app has no authentication. Live payments are out of scope; use only PayPal Sandbox credentials. The Groq diagnostic probe returned HTTP 401 with error code `invalid_api_key`; the key and response body were not printed. Groq lists `openai/gpt-oss-20b` as a supported production model in its current model docs: https://console.groq.com/docs/models and https://console.groq.com/docs/model/openai/gpt-oss-20b.
- Before finalizing decline labels, verify from PayPal docs that `buyer_recoverable` means the buyer can retry with another funding source; do not imply recovery is guaranteed. Seeded failures must be labeled simulated.

## Commands (run from the repository root in PowerShell)

Tests and static checks:

```powershell
$goExe = 'C:\Program Files\Go\bin\go.exe'
$gofmtExe = 'C:\Program Files\Go\bin\gofmt.exe'
& $gofmtExe -w main.go webhook.go main_test.go main_integration_test.go
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

`psql` is not available here. The approved migrations have already been applied. The most recent invocation was:

```powershell
& 'C:\Program Files\Go\bin\go.exe' run ./cmd/migrate ./migrations/003_webhook_events.sql
```

The user approved and migrations 001–003 have already been run. `cmd/migrate` reads `DATABASE_URL` without printing its value; it creates `schema_migrations` if needed, and applies the migration plus tracking record in the same transaction. Do not rerun them as part of this handoff unless the user requests it.
