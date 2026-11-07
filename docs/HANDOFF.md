# Revenue Guardian Handoff

Updated: 2026-11-07
Repository: `C:\dev\paypal-ai-auditor`
Branch: `feat/revenue-guardian`

## Current authorized fixes (2026-11-07)

This section supersedes earlier verification and gap statements below.

- Ran `go run ./cmd/migrate .\migrations\004_findings_actions.sql` from the repository root using its `.env` configuration. Runner reported `migration already applied: 004_findings_actions.sql`; no DDL was newly applied. A read-only schema query confirmed the `findings` and `actions` tables and their columns in the configured Render database. Migration 005 was not run.
- Refused action approval with HTTP 409 and a clear message when the stored action audit contains `money_moved=true`; added unit coverage and retained/updated the PostgreSQL integration test to assert the unsafe row remains unchanged.
- Added `ADMIN_TOKEN` authentication using `crypto/subtle` for finding detection/seeding, action proposals, and approvals. Checkout creation/capture do not require the admin token; PayPal Sandbox buyer approval remains part of checkout. An unset admin token fails closed for findings/action writes. The browser asks for it per protected operation and neither persists nor logs it. `.env.example` and README document the setting and judge token handling.
- Repeated-persisted-capture findings resolve an available PayPal order ID through related attempts; unit and PostgreSQL integration coverage assert the evidence.
- Added `.gitattributes` (`*.go text eol=lf`) and normalized tracked Go files.
- Added additive migration 005, `migrations/005_action_money_moved_check.sql`, and included it in disposable CI migrations. The check rejects `money_moved=true` anywhere in an action result, including proposed status. `NOT VALID` leaves historical violating rows in place and does not scan them when adding the constraint, but PostgreSQL enforces it for new rows and updated rows. Migration 005 has not been applied to production. Migration 004 is already present in the configured Render database.
- README documents token-protected findings/action writes, unauthenticated reads and checkout, the correct evidence claims, all five migrations and exact PowerShell runner commands, the 005 SQL and NOT VALID behavior, and private token provision for judges.
- Local checks: `gofmt -l .` printed no files; `go vet ./...` exited successfully; `go test -race -count=1 -v ./...` passed. The nine PostgreSQL integration tests skipped locally because `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL` was intentionally unset; no database was accessed.
- CI run [#48](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42370483910) passed for commit `f7019f7014475b839505973fe58d2ab0f5d45688`: migration application, gofmt, vet, and race tests all succeeded. All eight PostgreSQL integration tests reported PASS, including the unsafe-audit test with the tightened 005 constraint; none skipped. The 005 migration ran only against the disposable CI database, not production.
- Added a public read-only `GET /api/webhook-activity` summary endpoint and dashboard panel. It reports received/processed counts, the count of events with a stored processing error, and last event type/time only; it deliberately omits IDs, error detail, payloads, and payer data. Unit coverage checks GET-only behavior, and PostgreSQL integration coverage verifies summary counts and non-disclosure against a payload fixture.
- CI run [#52](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42406087202) passed for commit `5917c8823aa14776b82456cc64b2f92a2af5269e`: migration application, gofmt, vet, and race tests succeeded. All nine PostgreSQL integration tests reported PASS, including `TestPostgresWebhookActivitySummaryOmitsPayload`; no integration tests skipped. This handoff update is a follow-up documentation commit and must also pass CI.

### Pull request description

**Title:** Secure PayPal Sandbox revenue-leakage review and add action audit guard

**Summary:** Refuse approval of unsafe stored action audits, require an administrator token for findings/action writes, include available order IDs in repeated-capture evidence, and add an additive database check. Update the UI, migration workflow, and documentation. No refunds or other money movement are performed by action approval.

**Deployment notes:** Migration 004 was confirmed present in the configured Render database by the runner and a read-only schema query. Migration 005 has not been applied to production; the tightened database-level audit safeguard still requires separate approval. Set `APP_BASE_URL` and `PAYPAL_WEBHOOK_ID` in Render. Checkout endpoints do not require `ADMIN_TOKEN`; findings/action writes do. Judges must receive the admin token privately to approve the demo finding; the public repo contains no real token.

## Pre-submission verification (2026-11-07)

- The initial worktree was clean at `86bfe14` but one commit behind `origin/feat/revenue-guardian`. It was fast-forwarded to `54aa758` (`Revamp spend intelligence dashboard UI`); that source commit matched the remote and the worktree was clean before documentation edits.
- No application code was changed during this verification. Local checks did not access a database, Render settings, PayPal, or Groq. The pushed CI workflow uses its own disposable PostgreSQL service; no production/application database or Render settings were accessed. The requester reports migrations 001–003 are already applied in production; this was not independently checked. Earlier handoff notes say migration 004 had not been applied to Render; its current production status was not checked.
- Local checks on the source commit, with `DATABASE_URL` and `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL` removed from the test process:
  - `gofmt -l .` printed `actions.go` and `main.go`. The Windows worktree has CRLF line endings for these two files (`git ls-files --eol`: index LF, worktree CRLF), and local Go is 1.27.3 while `go.mod` and CI use Go 1.26.8. The CI gofmt step passed for this source commit.
  - `go vet ./...` exited successfully with no output.
  - `go test -race -count=1 -v ./...` exited successfully: `PASS`, `ok paypal-ai-auditor 3.961s`, and `? paypal-ai-auditor/cmd/migrate [no test files]`. All eight PostgreSQL integration tests skipped because the dedicated test URL was intentionally unset; no database was contacted.
- The latest GitHub Actions run for source commit `54aa758` is [run 42267185103](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42267185103), and it failed. Migration application, gofmt, and vet steps passed. Seven PostgreSQL integration tests passed; `TestPostgresDetectorsAndActionAudit` failed because the approval endpoint returned HTTP 200 for a pre-existing proposed action whose stored JSON result had `money_moved=true`. This issue was intentionally not fixed during this verification; CI is not green and this is a merge blocker.
- The code uses integer minor-unit fields for payment/finding values and current calculations, but the legacy `captures.amount` column remains `DOUBLE PRECISION`. Real finding evidence contains capture IDs and order IDs where available, not invariably both. Groq generates spend insights, not explanations for deterministic findings. The API has no authentication; a dashboard click is not an authenticated human identity. These limits are called out in the rewritten README.
- Secret-pattern scanning found credential-shaped data in the ignored local `.env`; values were not displayed. Tracked matches were limited to example placeholders, workflow test configuration, and redacted handoff samples. No secret values were printed or added to documentation.

## Proposed pull request description

**Title:** Revamp PayPal Sandbox spend intelligence and revenue-leakage dashboard

### Summary

- Add a responsive spend-intelligence dashboard with PayPal Sandbox transaction history, AI spend insights, leakage totals, synthetic demo data, and deterministic duplicate-capture/fee-mismatch findings.
- Record finding actions and audit events without calling a refund API or moving money.
- Verify PayPal webhook signatures and deduplicate receipts by event ID.

### Verification

- `go vet ./...` and local `go test -race -count=1 -v ./...` pass; local PostgreSQL integration tests skip without the dedicated test database.
- The latest branch CI run is currently failing `TestPostgresDetectorsAndActionAudit` on approval of an unsafe pre-existing action record (`money_moved=true`). Do not merge until this is fixed and a new CI run is green.

### Deployment notes

- Per requester, production migrations 001–003 are already applied; this has not been independently verified here.
- Migration 004 defines findings and actions and must be applied to the intended production database before relying on those endpoints. Database state was not checked during this task.
- Set `APP_BASE_URL` to the deployed app URL and configure `PAYPAL_WEBHOOK_ID` in Render. The webhook endpoint fails closed when the webhook ID is absent.
- This change does not modify Render settings or run database migrations.

## Goal and scope

Build a sandbox-only PayPal revenue-leakage assistant while preserving the existing create, approve, capture, reconcile, and persistence-pending flow. Money in Go and API/UI calculations is integer minor units. Keep the legacy `captures.amount` column until the user approves its removal.

## Done

- `0685125 feat: add simulated leakage demo endpoint` — added exact findings leakage-score route aliases with bodyless GET/POST support; seeded a deterministic, idempotent Synthetic Demo duplicate-capture finding; split simulated score buckets from live totals; added route/integration coverage and README instructions. Local `go vet ./...` and `go test -race -count=1 ./...` passed (`ok paypal-ai-auditor 2.414s`; PostgreSQL tests skipped locally because the dedicated test URL is unset). CI run #36 was cancelled when the following handoff commit triggered a newer run; run #37 passed all checks and all eight PostgreSQL integration tests, including `TestPostgresDemoSeedAndLeakageScoreRoutes` (`ok paypal-ai-auditor 1.598s`). No Render endpoint or database was called.
- `934906a feat: add deterministic findings and action tracking` — added additive migration 004; deterministic duplicate-capture and fee-mismatch detectors; findings scan/list, leakage-score, propose-action and approve-action APIs; JSONB audit results with `money_moved=false`; README and unit/PostgreSQL tests; CI now migrates 004. Local `go vet ./...` and `go test -race -count=1 ./...` passed (`ok paypal-ai-auditor 2.527s`); all seven PostgreSQL integration tests skipped locally because the dedicated test URL is unset. Migration 004 has not been run against Render.
- `9902c35 fix: type action proposal json parameters` — cast reused SQL parameters to text so PostgreSQL infers types in the action proposal JSONB expression. CI run #42245888814 then passed migration application, gofmt, and vet; it uncovered a JSONB whitespace-sensitive assertion in the integration test.
- `45d7234 test: compare action audit json semantically` — decoded the action JSONB audit and checks money_moved fields semantically across PostgreSQL JSON formatting. Local `go vet ./...` and `go test -race -count=1 ./...` passed (`ok paypal-ai-auditor 2.478s`; database tests skipped locally).
- CI run [#42246294855](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42246294855) passed for `45d7234`. CI applied migrations 001–004; gofmt, vet, and race tests passed. All seven PostgreSQL tests passed, including `TestPostgresDetectorsAndActionAudit`, `TestPostgresConcurrentDuplicateWebhookPostsWriteOneAttempt`, and the advisory-lock tests (`ok paypal-ai-auditor 1.598s`).
- `1f4d0fd docs: record webhook concurrency test` — recorded the PostgreSQL concurrency test and its local skip status.
- CI run [#42237267272](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42237267272) passed for commit `1f4d0fd`. Migrations 001–003 and the verbose race-test step succeeded with the configured PostgreSQL service; all six integration tests, including `TestPostgresConcurrentDuplicateWebhookPostsWriteOneAttempt`, ran rather than skipped.
- `01ab71e test: serialize concurrent duplicate webhook deliveries` — added the env-gated PostgreSQL test that sends two simultaneous POSTs with the same event ID, observes the second session blocked on `SELECT ... FOR UPDATE`, and asserts exactly one attempt insert and row. Local vet and race tests passed; PostgreSQL case skips locally without the dedicated DB URL.
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

- The screenshot showed `/api/findings/leakage-score` falling through to the finding-action ID parser; the score handler existed only at `/api/leakage-score` and accepted GET only. The score now supports bodyless GET/POST at both paths. Added `POST /api/findings/seed-demo`, idempotently creating an open synthetic $25 duplicate-capture finding with synthetic order/capture IDs. Live score buckets exclude synthetic findings; separate `simulated_*_minor` buckets include them. A PostgreSQL integration test covers both score methods, idempotent seeding, live/simulated bucket separation, and propose/approve transitions. No Render endpoint or database has been called.
- Migration 004, deterministic detectors, leakage score SQL, and record-only action endpoints are implemented locally. Local vet/race tests pass; the seven PostgreSQL integration tests skip because `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL` is unset. CI now applies migration 004 before running them.
- Migration 004 is additive and idempotent. It creates `findings` and `actions`, both with `BIGSERIAL` IDs, JSONB evidence/result, the requested status checks, and indexes. It has not been run against Render. Show `migrations/004_findings_actions.sql` and get explicit approval before running `go run ./cmd/migrate ./migrations/004_findings_actions.sql` against Render.
- The Go detectors are independent of Groq: duplicate capture evidence cites capture IDs; fee mismatch evidence cites expected and actual fee minor units and flags deltas greater than one minor unit. The fee estimate is the requested 2.9% plus 30 minor units heuristic, not PayPal's full merchant-specific fee schedule. `POST /api/findings/detect` runs them; findings can be read with `GET /api/findings`.
- Action routes are `POST /api/findings/{id}/action` and `POST /api/actions/{id}/approve`. Approval records `proposed` → `approved` → `executed` audit events in the JSONB result and changes the finding to `actioned`; `money_moved` is always false, and no PayPal money operation is called. The requested “recovered” score bucket maps to `actioned`, not proof of funds returned.
- Migration 004, detectors, leakage score, and record-only action endpoints are implemented and green in CI. Wait for explicit approval before applying migration 004 to Render. Do not start the agent loop until requested.
- Historical CI run [#42235597732](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42235597732) passed after applying migrations 001–003. The concurrent duplicate-delivery test was subsequently confirmed green in run #42237267272.
- Migration 003 was revised with additive `resource_id`, `processed_at`, and `process_error` columns plus a `resource_id` index. The approved command was `go run ./cmd/migrate ./migrations/003_webhook_events.sql`; it was applied to the user's Render database. The runner reported 0 rows and 0 minor units before and after. Read-only schema inspection showed `event_id`, `event_type`, `resource_id`, `payload`, `received_at`, `processed_at`, and `process_error`; `schema_migrations` contains 001, 002, and 003.
- Decline rate now uses only `INSTRUMENT_DECLINED`, `PAYER_CANNOT_PAY`, and `PAYMENT_DENIED` as numerator codes, among real capture-stage attempts. README states these codes come from sandbox negative testing, defines recoverable as retrying with another funding source without guaranteeing recovery, and warns that webhook payloads may contain payer data (sandbox only).
- Current local checks for findings/actions: `go vet ./...` passed; `go test -race -count=1 -v ./...` passed. All seven PostgreSQL integration tests skipped because `PAYPAL_AI_AUDITOR_TEST_DATABASE_URL` is unset locally.
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
- Findings are generated only by deterministic Go code from non-synthetic attempts and PayPal's completed-capture fee data. Findings are idempotent by a stable SHA-256 fingerprint; rescans refresh evidence only while status remains open, preserving acknowledged/actioned/dismissed states. Fee estimate is stored in minor units without float arithmetic.
- An action's `executed` state means record-only approval in this app. Audit events say `money_moved: false`; score's `recovered_minor` is the specified sum of `actioned` findings and must not be represented as a confirmed PayPal refund.
- `psql` is not installed in the current environment. `cmd/migrate` is a one-shot Go runner using existing PostgreSQL and dotenv dependencies. It creates `schema_migrations` if needed, then executes the migration, verifies/reports totals, and records the migration filename in one transaction. A rerun after a successful record skips it; an interrupted unrecorded migration can safely be retried because the SQL is idempotent.
- The legacy `amount` column remains `NOT NULL`; new capture inserts keep it populated from minor units in SQL for compatibility. Reads, validation, totals, API output, and frontend formatting use minor units.

## Known issues and unverified items

- Historical note: migration 001 was previously applied to the former database, which the user has since deleted; that run reported 7 rows and 17,500 minor units before and after. Current replacement-database results are recorded below.
- On the replacement empty database, read-only inspection confirmed `captures.amount_minor` exists and `captures` had 0 rows; `schema_migrations` initially contained only migration 001. Migrations 001, 002, and approved 003 were applied with `cmd/migrate`; each reported 0 rows and 0 minor units before and after. Attempts columns were verified after 002; webhook columns were verified after 003. No credentials were printed.
- Historical CI run [#42221126619](https://github.com/pavankumargp6/paypal-ai-auditor/actions/runs/42221126619) passed with migrations 001–003 applied to disposable PostgreSQL. The updated webhook retry integration test is pending a new CI run.
- The dedicated PostgreSQL test URL is unset locally, so all seven PostgreSQL integration tests skip locally. Migration 004 and `TestPostgresDetectorsAndActionAudit` await CI verification after this task's push.
- The transaction-aware retry path is covered locally with mocked PayPal and a simulated persistence failure. The env-gated integration test exercises it with a real PostgreSQL advisory-lock transaction and mocked PayPal when the dedicated test DB is available.
- Local app verification on port 18080 against the replacement database passed: `/api/spend-intelligence` returned HTTP 200, `total_captured_minor=0`, `decline_rate_available=false`, and `ai_insight=Not enough data yet`; `/` returned HTTP 200 and the page contains the “No data yet” decline fallback, Source labels, and citation rendering. No PayPal capture was triggered. The app process was stopped after the check.
- Webhook handling stores payloads that may contain payer data; do not log payloads and use this endpoint for Sandbox only. Supported capture events write webhook-stage attempts; unknown event types are persisted and marked processed without an attempt.
- Migration 004 has not been applied to Render. Until then, findings and actions routes return database errors on that database; CI applies it to its disposable PostgreSQL service.
- PayPal negative-test mock responses are not actual buyer/card decline transactions. `INSUFFICIENT_FUNDS` classification remains unverified.
- The app has no authentication. Live payments are out of scope; use only PayPal Sandbox credentials. The Groq diagnostic probe returned HTTP 401 with error code `invalid_api_key`; the key and response body were not printed. Groq lists `openai/gpt-oss-20b` as a supported production model in its current model docs: https://console.groq.com/docs/models and https://console.groq.com/docs/model/openai/gpt-oss-20b.
- Before finalizing decline labels, verify from PayPal docs that `buyer_recoverable` means the buyer can retry with another funding source; do not imply recovery is guaranteed. Seeded failures must be labeled simulated.

## Commands (run from the repository root in PowerShell)

Tests and static checks:

```powershell
$goExe = 'C:\Program Files\Go\bin\go.exe'
$gofmtExe = 'C:\Program Files\Go\bin\gofmt.exe'
& $gofmtExe -w main.go webhook.go actions.go detectors.go main_test.go main_integration_test.go
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

`psql` is not available here. Migrations 001–003 have already been applied. Migration 004 has not been applied to Render; the command for after explicit approval is:

```powershell
& 'C:\Program Files\Go\bin\go.exe' run ./cmd/migrate ./migrations/004_findings_actions.sql
```

`cmd/migrate` reads `DATABASE_URL` without printing its value; it creates `schema_migrations` if needed, and applies the migration plus tracking record in the same transaction. Do not run migration 004 against Render before explicit approval.
