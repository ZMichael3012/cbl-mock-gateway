# CBL-3.6. Mock gateway: sandbox provider with signed webhooks

| Field | Value |
| :-- | :-- |
| Epic | E3, Gateway Connector |
| Repository | `cbl-mock-gateway` |
| Size | M |
| Wave and parallel group | W1-CONNECTOR |
| Depends on | 1.7 (CF-1), 1.6 (HMAC), 3.5 (webhook format) |
| Contract version | `cbl-protos@cf-1` (events `billing/events/v1`) |
| Status | done (review closed, commit CBL-3.6) |

## 1. Context and goal

Before the real provider is fixed (KI-01), the platform needs a sandbox gateway: adapter 3.2,
e2e of payment flows (CBL-002, section 7, GE-1) and resilience checks 3.3 run against a
mock provider. The service emulates the gateway API, sends HMAC-signed webhooks under the
sandbox provider contract (CBL-3.5) to Webhook Ingress, and can reproduce failures:
unavailability, delays, duplicates, an invalid signature, abnormal statuses.

## 2. Boundaries

In scope: provider HTTP API (invoices and statuses), sending signed webhooks, failure
scenarios (fail_next, latency, duplicate, delay, signature mode), delivery log, e2e check
of the webhook contract, compose and smoke.

Out of scope: real crypto operations and addresses, adapter 3.2 (the client of this API), the
normalization pipeline 3.5, client resilience 3.3, prod use.

## 3. Functional requirements

FR-1. Provider API (no authentication, trusted-network stand):
- `POST /api/v1/invoices`: body `{payment_id: uuid, amount: Money, network, expires_at}`;
  creates an invoice with `gateway_id` (UUIDv7), a deterministic mock address, and status
  `pending`; the `Idempotency-Key` header makes a repeat return the same invoice
  (`replayed: true`), with no duplicate; response `201`/`200`;
- `GET /api/v1/invoices/{gateway_id}`: the current invoice state, including the latest
  `status`, `received_amount`, `confirmations`, `txid`, `event_ts`; `404` for an unknown one;
- validation: `payment_id` is a UUID, `amount.minor_units > 0`, asset 2 - 16, decimals up to 18,
  `network` from the code charset, `expires_at` non-empty; errors are `400 problem+json`.

FR-2. Webhooks (sandbox provider contract from CBL-3.5):
- `POST /api/v1/simulate`: body `{gateway_id, status, received_amount?, confirmations?,
  txid?, event_ts?, delay_ms?, duplicate?, signature?}`; status from the canonical set of
  3.1 (detected, confirmed, failed, expired, underpaid, overpaid; plus the aliases
  pending/completed/error); simulate updates the invoice state and sends a webhook;
- the webhook body is the sandbox contract JSON (`payment_id`, `gateway_id`, `status`,
  `received_amount`, `confirmations`, `txid`, `network`, `event_ts`);
- signature: kit `hmac.Sign` over the raw body, header `t=<unix>,v1=<hex>`
  (`hmac.BuildHeader`); the names of the signature and event id headers are configurable
  (`X-Billing-Signature`, `X-Event-Id` by default) and match the Webhook Ingress
  configuration;
- `signature`: `valid` (default), `invalid` (a corrupted signature), `missing` (the header
  is absent) - to check Ingress rejection;
- `duplicate` (default 1, up to 10): resending the same event (the same `event_id`
  and body) - checks Ingress deduplication; `delay_ms` (up to 5000) postpones the sending
  (race, ordering, expiration scenarios); delivery is detached from the request context
  and completes even if the client disconnects; `event_ts` overrides the event time
  (out-of-order events);
- result: `200` with the list of deliveries `{http_status, error, mode}` (delivery errors do
  not change the simulate response code); each delivery is written to the log; recipient
  redirects are not followed (a 3xx counts as non-delivery).

FR-3. Provider API failure scenarios:
- `POST /api/v1/scenarios {fail_next, latency_ms}`: the next `fail_next` provider API calls
  respond `503 problem+json` (checks retries and the circuit breaker of 3.3), `latency_ms`
  (up to 5000) adds a delay to the response (checks timeouts);
- `GET /api/v1/scenarios` - the current state, `POST /api/v1/scenarios/reset` - reset.

FR-4. Delivery log: `GET /api/v1/webhooks` returns the last 100 deliveries
(`event_id`, `gateway_id`, `status`, `signature mode`, `sent_at`, `http_status`, `error`);
the log is in-memory with a size limit.

FR-5. Storage is in-memory with a mutex (up to 1000 invoice records, scenarios, a log of the
last 100 deliveries); state is lost on restart: the mock service is deterministically
recreated in tests. Invoice overflow - `503 store_full`. Money is int64 minimal units
plus decimals; float is forbidden.

FR-6. Configuration: `HTTP_ADDR` (:8120), `METRICS_ADDR` (:9320), `WEBHOOK_TARGET_URL`
(default `http://localhost:8060/hooks/mock/ep1`), `WEBHOOK_SECRET` (default `mock-secret`),
`WEBHOOK_SIGNATURE_HEADER`, `WEBHOOK_EVENT_ID_HEADER`, `WEBHOOK_TIMEOUT` (default 5s).
The secret is not logged.

FR-7. E2E: a Go test (tag `e2e`) starts mock-gateway and a local sink: the sink verifies the
signature with kit `hmac.Verify`, deduplicates by `event_id`, and parses the body as the
sandbox contract; scenarios: create -> simulate confirmed with a duplicate -> two deliveries
of one event_id -> the signature is valid -> the repeated delivery is rejected by the sink
deduplication; an invalid/missing signature is rejected by the sink; delay/out-of-order and
fail_next are checked at the API level. Compose smoke: healthz, invoice creation, simulate,
delivery log; the live integration with Webhook Ingress is the global GE-1.

## 4. Contracts

- REST (provider emulation): see FR-1 - FR-4; errors `application/problem+json` via
  ginmw. The webhook body is the sandbox provider contract CBL-3.5 (JSON), the signature is
  kit hmac (compatible with Webhook Ingress 5.1). `cbl-protos@cf-1` does not change.

## 5. Data model and migrations

No migrations (in-memory). Structures: `Invoice`, `WebhookDelivery`, `Scenario` in domain.

## 6. Finite state machines and rules

The invoice status changes only through simulate (the mock does not check monotonicity - that
is the Payment 4.3 area); `pending` is the initial status, simulate transitions it to the
requested canonical status. A repeated simulate is allowed: it models repeated/duplicate
provider webhooks.

## 7. Edge cases

A repeated create with the same `Idempotency-Key` (the same invoice), simulate of an unknown
invoice (404), `received_amount` of an asset different from the invoice (accepted - the
underpaid/overpaid scenario with a different amount), zero confirmations, an empty txid,
`delay_ms` greater than the client timeout (delivery completes with a detached context), an
unreachable `WEBHOOK_TARGET_URL` (an error in the log, simulate responds 200), a recipient
redirect (3xx is non-delivery, the body does not go to another host), `fail_next`
exhaustion, concurrent simulate calls, log overflow (100), invoice overflow (503 store_full),
a body larger than 64 KB (413), unknown JSON fields (400).

## 8. Non-functional requirements

- The service is a test service: no authentication, it is not deployed to prod
  (KI-mock-gateway-02); it listens on the trusted compose network; secrets come only from env.
- The API response budget is local milliseconds (excluding the scenario delay);
  `CreateInvoice` for adapter 3.2 fits within p99 < 1 s (CBL-002).
- Request bodies are limited to 64 KB (`MaxBytesReader`, 413), JSON is strict
  (`DisallowUnknownFields`, trailing data is forbidden); `WriteTimeout` (75 s) covers the
  worst scenario of latency 5 s + delay 5 s + 10 deliveries at `WEBHOOK_TIMEOUT`.
- The simulate delivery is detached from the request context (90 s budget): a client
  disconnect does not cancel the webhook and does not lose the log entry.
- Concurrency - a mutex, races are excluded; unit+e2e coverage is not lower than 80%;
  money values are int64/decimals.

## 9. Test plan

| Level | Scenarios |
| :-- | :-- |
| Smoke | `go build ./...`; compose: healthz, create, simulate, log (run per the `test/e2e/E2E.md` instruction, scenario E2E_MockGateway_Smoke, compose stand) |
| Unit | domain (validation, statuses), usecase (create idempotency, simulate, scenarios, log, signature via a fake sender + kit hmac.Verify), transport (routes, problem+json, fail_next 503) |
| Integration | not applicable: in-memory state, no external databases |
| E2E | tag `e2e`: mock-gateway + sink with kit hmac.Verify and deduplication; duplicate, invalid/missing signature, delay, fail_next |
| Load | 14.1 (a burst of webhooks at Ingress) |

## 10. Work plan and commit

1. Spec, tests (unit + e2e), then implementation: domain, usecase, transport, webhook sender,
   main and env, compose, smoke.
2. Four review passes, AGENTS.md, linters, final runs.
3. Commit `CBL-3.6: sandbox gateway with signed webhooks and failure scenarios`; the spec is
   in the same commit.

## 11. Open questions and decisions

- ADR: the webhook format repeats the CBL-3.5 contract (sandbox provider) - a single source
  for the connector and tests; divergence only by updating both specs.
- ADR: simulate always responds `200` for a valid request: tests distinguish non-delivery
  by the log and `http_status`, not by the simulate code (validation errors give
  400/404/413/503).
- ADR: scenario limits (delay/latency up to 5 s, duplicate up to 10) are derived from the
  handler budget (`WriteTimeout` 75 s); delivery uses a detached context (90 s) so that the
  "delay greater than the client timeout" scenario can be reproduced.
- ADR: recipient redirects are not followed ("do not follow" mode): the env target must not
  forward the event body and signature to another host.
- Accepted with a limitation: the SSRF surface is limited by the `WEBHOOK_TARGET_URL` env
  (configured by the stand operator); in-memory invoices are limited to 1000 records; the
  service is not deployed to prod (KI-mock-gateway-02).
- Open question: the real codes/signatures of provider number 1 will appear in 3.2; the mock
  will remain a sandbox layer for tests.

## 12. Review (CBL-001, step 6)

Four independent passes (14.10.2026). The security blocker (an unbounded request body and
fields without limits in an unauthenticated service) was closed by the 64 KB limit and strict
JSON. All major and minor findings are closed or recorded.

| Role | Key findings | Closure |
| :-- | :-- | :-- |
| No-context | the simulate response has no `mode`; delay/latency exceed server timeouts; the body limit is not implemented; there are no unit tests for the webhook package; the concurrent test has no assert; smoke does not check delivery | `mode` added to deliveries; the limits are derived from `WriteTimeout` (75 s) and the detached context; `MaxBytesReader` 64 KB + strict JSON; sender tests and asserts added; smoke checks delivery statuses |
| Architecture | the scenario budget vs the timeouts; `delay_ms` was canceled by the client context; no invoice limit; redirects; the compose target localhost; the global compose without a link to ingress; the storage doc is misleading | a detached delivery context (90 s); a cap of 1000 invoices (503 store_full); redirects are forbidden; target `host.docker.internal`; global compose: explicit env + depends_on webhook-ingress; storage doc/comments fixed |
| With-context | NFR 64 KB/strict JSON is not met; `mode` is missing in the response; trade-off T-MOCK-02 is inaccurate; the journal was ahead of the review; cover.out is in the tree; latency/duplicate bounds | the NFR is implemented and covered by 413/400 tests; `mode` added; the wording is fixed; artifacts are cleaned up, `cover.out` in .gitignore; the bounds are updated in the spec |
| Security (BLOCK lifted) | an unbounded body (memory DoS); a delay up to 60 s vs WriteTimeout; unbounded invoices; redirects (SSRF); asset/txid without limits; the default secret | 64 KB + 413; scenarios <=5 s and a 75/90 s budget; cap 1000; redirects are not followed; charset/lengths of asset/txid in Validate; the default secret and the lack of authentication are an accepted stand risk (KI-mock-gateway-02) |
