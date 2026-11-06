# PayPal Spend Intelligence Agent

An AI-powered dashboard for merchants to view verified PayPal Sandbox payments, track live revenue, and receive actionable insights from Groq (`openai/gpt-oss-20b`).

## Architecture
- **Backend:** Go (standard library only, no heavy frameworks).
- **Database:** PostgreSQL (hosted on Render).
- **Frontend:** HTML/JS with AG Grid.
- **AI:** Groq API.
- **Payments:** PayPal Orders v2 API (Sandbox).

## Setup & Environment Variables
Required variables:
- `DATABASE_URL` (PostgreSQL connection string; see `.env.example`)
- `APP_BASE_URL` (application base URL used for PayPal return/cancel redirects; defaults to `http://localhost:8080`)
- `PAYPAL_CLIENT_ID`
- `PAYPAL_CLIENT_SECRET`
- `GROQ_API_KEY`

## PayPal Flow
1. User clicks "PayPal" -> `/api/create-order` creates a $25.00 USD order.
2. User is redirected to the PayPal `approveURL`.
3. User logs in and approves the payment.
4. User is redirected back to the app.
5. Frontend calls `/api/capture-order`.
6. Backend verifies status is `APPROVED`, captures funds, validates amount/currency, and saves to PostgreSQL.

## Error Handling
- **Reconciliation:** If an order is already `COMPLETED`, the backend safely reconciles instead of failing.
- **Persistence Failures:** If PayPal succeeds but DB insert fails, the API returns `capture_succeeded_persistence_pending`.

## Findings and Actions
- Deterministic Go detectors create `duplicate_capture` and `fee_mismatch` findings; Groq does not decide leak status or amounts. Groq is limited to plain-English insight/explanation text.
- Duplicate-capture evidence includes the capture IDs. Fee evidence records expected fee, PayPal's reported `seller_receivable_breakdown.paypal_fee`, and the delta in minor units ([PayPal webhook payload](https://developer.paypal.com/api/rest/webhooks/rest/)). The fee detector applies the requested `round(amount_minor * 0.029) + 30` heuristic and flags differences greater than one minor unit; this is a heuristic, not PayPal's full merchant-specific fee schedule.
- Leakage score SQL groups open findings as lost, open/acknowledged findings as recoverable, and actioned findings as recovered. Here, “recovered” follows the requested actioned status mapping; it is not proof that funds were returned.
- Approving an action records an executed status and audit events in JSONB; `money_moved` remains false. No action endpoint moves funds.

## Limitations
- **Sandbox only:** Payments use PayPal Sandbox; do not use this app for live transactions.
- **No authentication:** API endpoints do not authenticate users and must not be exposed as a production merchant system.
- **Sandbox reason codes:** Reason codes come from sandbox negative testing and do not establish production decline behavior.
- **Synthetic data:** Synthetic records are labeled in the UI and excluded from live metrics.
- **Decline definition:** Only `INSTRUMENT_DECLINED`, `PAYER_CANNOT_PAY`, and `PAYMENT_DENIED` count as declines. These reason codes come from sandbox negative testing. PayPal's [payment failure guide](https://developer.paypal.com/api/handle-payment-failures/) suggests trying a different payment method for these conditions. “Recoverable” means the buyer can retry with another funding source; it does not guarantee recovery.
- **Webhook data:** Stored webhook payloads may contain payer data. This application is sandbox only; do not send production payer payloads to it.
- **Decline Rate:** The rate counts only the three decline codes above in real (non-synthetic) capture attempts; reconciles and other failure codes are excluded. It is unavailable until capture attempts exist.

## Testing
Run the unit tests (which mock HTTP and do not hit real PayPal):
```bash
gofmt -w main.go main_test.go
go test ./...
go test -race ./...
go vet ./...

