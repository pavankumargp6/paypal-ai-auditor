# PayPal Spend Intelligence Agent

An AI-powered dashboard for merchants to view verified PayPal Sandbox payments, track live revenue, and receive actionable insights from Groq (Llama 3).

## Architecture
- **Backend:** Go (standard library only, no heavy frameworks).
- **Database:** PostgreSQL (hosted on Render).
- **Frontend:** HTML/JS with AG Grid.
- **AI:** Groq API.
- **Payments:** PayPal Orders v2 API (Sandbox).

## Setup & Environment Variables
Required variables:
- `DATABASE_URL` (PostgreSQL connection string)
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

## Error Handling & Limitations
- **Reconciliation:** If an order is already `COMPLETED`, the backend safely reconciles instead of failing.
- **Persistence Failures:** If PayPal succeeds but DB insert fails, the API returns `capture_succeeded_persistence_pending`.
- **Decline Rate:** We only persist successful captures. A true decline rate requires a separate `attempts` table (not implemented).
- **Demo Data:** Synthetic records are clearly labeled in the UI and excluded from live revenue calculations.

## Testing
Run the unit tests (which mock HTTP and do not hit real PayPal):
```bash
gofmt -w main.go main_test.go
go test ./...
go test -race ./...
go vet ./...

