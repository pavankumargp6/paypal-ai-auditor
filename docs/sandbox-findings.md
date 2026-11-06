# PayPal Sandbox Failure Findings

Run date: 2026-11-06
Environment: PayPal Sandbox (`api-m.sandbox.paypal.com`) only. OAuth values were read from the ignored local `.env` into process memory and were not printed or recorded. No production credentials were used. No order was approved or captured; capture requests used PayPal's documented example order ID with the negative-testing header, and all returned mock errors.

## Baseline

- `go version`: `go version go1.27.3 windows/amd64`
- `go vet ./...`: exit code 0; no output.
- `go test -race ./...`: exit code 0; `ok paypal-ai-auditor (cached)`.

## Negative testing method

PayPal's [negative testing request headers documentation](https://developer.paypal.com/negative-testing/request-headers/) says to use the JSON request header `PayPal-Mock-Response: {"mock_application_codes":"<error_name>"}`. Orders v2 create and capture are listed among supported methods. Its [Orders v2 error reference](https://developer.paypal.com/api/orders/v2/error-messages) lists `INSTRUMENT_DECLINED`, `PAYER_CANNOT_PAY`, `PAYMENT_DENIED`, and `ORDER_ALREADY_CAPTURED` as issues for the relevant flow. It does not list `INSUFFICIENT_FUNDS`.

The following raw HTTP error response bodies were returned by the Sandbox. No secrets appeared in the bodies; response debug IDs and descriptions are retained as returned.

### Create order — requested `DUPLICATE_INVOICE_ID` — HTTP 422

```json
{
    "name": "UNPROCESSABLE_ENTITY",
    "details": [
        {
            "issue": "DUPLICATE_INVOICE_ID",
            "description": "Duplicate Invoice ID detected. To avoid a potential duplicate transaction your account setting requires that Invoice Id be unique for each transaction."
        }
    ],
    "message": "The requested action could not be completed, was semantically incorrect, or failed business validation.",
    "debug_id": "70c28ae654da",
    "links": [
        {
            "href": "https://developer.paypal.com/docs/api/orders/v2/#error-DUPLICATE_INVOICE_ID",
            "rel": "information_link",
            "method": "GET"
        }
    ]
}
```

### Create order — requested `INSTRUMENT_DECLINED` — HTTP 422

```json
{
    "name": "UNPROCESSABLE_ENTITY",
    "details": [
        {
            "issue": "INSTRUMENT_DECLINED",
            "description": "The instrument presented  was either declined by the processor or bank, or it can't be used for this payment."
        }
    ],
    "message": "The requested action could not be performed, semantically incorrect, or failed business validation.",
    "debug_id": "70c28ae654da",
    "links": [
        {
            "href": "https://developer.paypal.com/api/rest/reference/orders/v2/errors/#INSTRUMENT_DECLINED",
            "rel": "information_link",
            "method": "GET"
        }
    ]
}
```

### Capture order — requested `INSTRUMENT_DECLINED` — HTTP 422

```json
{
  "name": "UNPROCESSABLE_ENTITY",
  "details": [
    {
      "issue": "INSTRUMENT_DECLINED",
      "description": "The instrument presented  was either declined by the processor or bank, or it can't be used for this payment."
    }
  ],
  "message": "The requested action could not be performed, semantically incorrect, or failed business validation.",
  "debug_id": "3fc230ceac4f5",
  "links": [
    {
      "href": "https://developer.paypal.com/docs/api/orders/v2/#error-INSTRUMENT_DECLINED",
      "rel": "information_link",
      "method": "GET"
    }
  ]
}
```

### Capture order — requested `PAYER_CANNOT_PAY` — HTTP 422

```json
{
  "name": "UNPROCESSABLE_ENTITY",
  "details": [
    {
      "issue": "PAYER_CANNOT_PAY",
      "description": "Combination of payer and payee settings mean that this buyer cannot pay this seller."
    }
  ],
  "message": "The requested action could not be completed, was semantically incorrect, or failed business validation.",
  "debug_id": "70c28ae654da",
  "links": [
    {
      "href": "https://developer.paypal.com/docs/api/orders/v2/#error-PAYER_CANNOT_PAY",
      "rel": "information_link",
      "method": "GET"
    }
  ]
}
```

### Capture order — requested `INSUFFICIENT_FUNDS` — HTTP 403

Response body: empty.

This code is not documented as an Orders v2 error issue. The mock request did not return a reason body, so it provides no evidence that Orders v2 classifies an insufficient-funds failure under this code.

### Capture order — requested `PAYMENT_DENIED` — HTTP 422

```json
{
    "name": "UNPROCESSABLE_ENTITY",
    "details": [
        {
            "issue": "PAYMENT_DENIED",
            "description": "PayPal has declined to process this transaction."
        }
    ],
    "message": "The requested action could not be performed, semantically incorrect, or failed business validation.",
    "debug_id": "70c28ae654da",
    "links": [
        {
            "href": "https://developer.paypal.com/api/rest/reference/orders/v2/errors/#PAYMENT_DENIED",
            "rel": "information_link",
            "method": "GET"
        }
    ]
}
```

### Capture order — requested `ORDER_ALREADY_CAPTURED` — HTTP 422

```json
{
  "name": "UNPROCESSABLE_ENTITY",
  "details": [
    {
      "issue": "ORDER_ALREADY_CAPTURED",
      "description": "Order already captured.If 'intent=CAPTURE' only one capture per order is allowed."
    }
  ],
  "message": "The requested action could not be completed, was semantically incorrect, or failed business validation.",
  "debug_id": "70c28ae654da",
  "links": [
    {
      "href": "https://developer.paypal.com/docs/api/orders/v2/#error-ORDER_ALREADY_CAPTURED",
      "rel": "information_link",
      "method": "GET"
    }
  ]
}
```

## Finding

For supported negative-testing codes, the Orders v2 response includes a stable top-level error name and a specific `details[].issue` plus a human-readable description. This is sufficiently structured for failure logging and a reason-code breakdown. These are mock responses, not actual buyer/card decline transactions. The undocumented `INSUFFICIENT_FUNDS` mock code produced an empty HTTP 403 response, so insufficient-funds classification remains unverified.

