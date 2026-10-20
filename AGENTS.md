# AGENTS.md - cbl-mock-gateway

Mock provider for tests and stands: emulation of the gateway API and signed webhooks.

Mandatory global rules: `../../AGENTS.md`, `../../.specs/CBL-001-development-process.md`,
`../../.specs/CBL-002-repos-and-docker.md`, contracts `../../.specs/CBL-003-contracts.md`.
The em dash (U+2014) is forbidden, only the hyphen is used.

## 1. Boundaries

In scope: invoice creation, statuses, signed webhooks, failure scenarios, provider
emulation profiles (CBL-17.5) for contract tests without network access; since CBL-17.27
the BTCPay profile also emulates the payout/refund wire (pull payment plus payout, the
stand state control) and the store invoice list used by the connector export, and
`POST /api/v1/simulate` gained the `silent` flag.

Out of scope: other platform services; interaction only through `cbl-protos` contracts
(gRPC), Kafka and the perimeter REST (see `CBL-000`, section 2); no fund movement (the
stand never pays out, the operator advances payout states).

## 2. Contracts

- REST: `POST /api/v1/invoices`, `GET /api/v1/invoices/{gateway_id}` - emulation of the
  sandbox provider (mock profile, unchanged since 3.6); `POST /api/v1/simulate` - sending
  signed webhooks; `GET /api/v1/webhooks` - delivery log; `POST|GET /api/v1/scenarios`,
  `POST /api/v1/scenarios/reset` - failure scenarios; webhooks go to Webhook Ingress under
  the sandbox provider contract (CBL-3.5)
- REST provider profiles (CBL-17.5): NOWPayments `POST /v1/invoice`,
  `GET /v1/payment/{id}` (`x-api-key`); CoinGate `POST /v2/orders`,
  `GET /v2/orders/{id}` (`Authorization: Bearer`); BitPay `POST /invoices`,
  `GET /invoices/{id}` (`X-Identity`/`X-Signature` presence; decimal major-unit
  `price`/`amountPaid`, `token` in the create body, the `merchant/invoice` facade
  envelope); BTCPay
  `POST /api/v1/stores/{store_id}/invoices`, `GET /api/v1/stores/{store_id}/invoices/{id}`
  (`Authorization: token`); the wire shapes, statuses and signatures are the profile
  contract (spec section 3.2)
- REST provider payout/export emulation (CBL-17.27, BTCPay only): `POST
  /api/v1/stores/{store_id}/pull-payments` (same-name replay),
  `POST /api/v1/pull-payments/{pull_payment_id}/payouts`,
  `GET /api/v1/stores/{store_id}/payouts/{payout_id}`,
  `POST /api/v1/payouts/{payout_id}/simulate` (stand control),
  `GET /api/v1/stores/{store_id}/invoices?startDate&endDate&skip&take` (the export
  walk) and `?orderId=` (the connector lookup); `POST /api/v1/simulate` gains
  `silent` (state change without webhooks)
- gRPC: none
- Kafka produces: none
- Kafka consumes: none
- Storage: in-memory (invoices, scenarios, log of the last 100 deliveries)

Contracts are frozen in `cbl-protos` (CF-1 for gRPC, CF-2/CF-3 for events), see `CBL-004`.

## 3. Architecture

Layers: `internal/transport` -> `internal/usecase` -> `internal/domain`; sending signed
webhooks - the `internal/webhook` adapter through the `usecase.WebhookSender` port.
`internal/storage` is not used: the stand state is in-memory (T-MOCK-01).
Provider profiles (17.5): `internal/domain/profiles.go` and `profile_api.go` hold the
profile catalogue (status tables, auth rules, signatures, wire shapes), the transport
mounts the provider routes from the profile route table (`routesOf`) with generic
handlers, `internal/webhook/profiles.go` builds profile bodies and signatures (kit
`webhooksig` canonical serializer, stdlib HMAC, fail closed) and the `webhook.Switch`
routes events to per-profile senders; profile targets derive from `WEBHOOK_TARGET_URL`
by replacing `/hooks/mock/`.
Payout, refund and export emulation (17.27): `internal/domain/payout.go` holds the
`PullPayment`/`Payout` models, the state vocabulary of the connector payoutStates table,
the request `Validate()` rules, `PullPayment.AllowsPayout` (method and amount
consistency) and the deterministic store-list ordering (`SortInvoicesByCreated`,
`FilterInvoices` at wire (unix second) granularity); `internal/usecase/service.go` keeps
the in-memory pull payment and payout stores (caps, name replay, the scenario injection)
and the store list/lookup (`ExportInvoices`, `LookupInvoicesByOrder`); the transport
mounts the optional payout routes from `profileRoutes.payout` (BTCPay only) and the
stand payout state control; the `silent` simulate flag updates an invoice without
webhooks. The wire shapes mirror the real Greenfield flow verified in the 17.9 regtest
run and the connector adapter (pull payment, payout, `AwaitingApproval`/`Completed`
states, the invoice list body).
Libraries: Gin, zap, cbl-kit (hmac, webhooksig, ginmw, money), google/uuid; GORM, franz-go,
go-redis, goose and PostgreSQL are not needed by the mock service - the deviation from the
CBL-002 set is recorded by trade-off T-MOCK-01.

## 4. Commands

```powershell
make build        # go build ./...
make test         # go test ./...
make test-e2e     # end-to-end webhook contract scenario (tag e2e)
make vet          # go vet ./...
make lint         # golangci-lint in Docker
make up           # docker compose -f deploy/docker-compose.yml --profile app up -d
make down         # docker compose -f deploy/docker-compose.yml down -v
```

Compose stand smoke runs follow the `test/e2e/E2E.md` instruction (scenario
E2E_MockGateway_Smoke; artifacts in `test/e2e/.runs/<mark>/`); there are no PowerShell run
scripts.

## 5. Ports and environment

HTTP `8120`, gRPC `-`, metrics `9320`.
Variables (applicable to the service): `HTTP_ADDR`, `METRICS_ADDR`, `WEBHOOK_TARGET_URL`
(default `http://localhost:8060/hooks/mock/ep1`), `WEBHOOK_SECRET` (default `mock-secret`),
`WEBHOOK_SIGNATURE_HEADER` (default `X-Billing-Signature`), `WEBHOOK_EVENT_ID_HEADER`
(default `X-Event-Id`), `WEBHOOK_TIMEOUT` (default 5s).
Profile variables (17.5): `WEBHOOK_TARGET_URL_<PROVIDER>` (NOWPAYMENTS, COINGATE, BITPAY,
BTCPAY) override the derived target; `WEBHOOK_SECRET_NOWPAYMENTS`,
`WEBHOOK_SECRET_COINGATE`, `WEBHOOK_SECRET_BITPAY`, `WEBHOOK_SECRET_BTCPAY` set the
webhook signing secrets (stand defaults `mock-nowpayments-secret`, `mock-coingate-token`,
`mock-bitpay-secret`, `mock-btcpay-secret`).

## 6. Development rules

- Pipeline (TDD-first, CBL-001): an SDD spec with the architecture sketch and the full test
  plan (unit and integration matrices, e2e scenario descriptions) -> contracts -> red tests
  with recorded evidence -> implementation -> 4-agent review on the staged diff (no-context,
  architecture, with-context, security - blocking) -> AGENTS.md -> final runs (unit +
  integration + local e2e) -> linters -> commit.
- Unit+integration coverage is at least 80%; money scenarios only on real PostgreSQL and
  Kafka (testcontainers); local e2e in docker-compose is mandatory.
- Money is int64 in minor units plus decimals only; float is forbidden.
- Godoc docstrings are mandatory in English: the first line starts with the entity name and ends with a period; comments explain why; errors are wrapped with `%w`; panics in business code are forbidden.
- Commit: `CBL-<epic>.<number>: <short message>`, the spec with the same prefix in the same commit.

Adding a provider profile (CBL-17.22 checklist):

1. Add the `ProfileID` constant and the profile row in `internal/domain/profiles.go`:
   status tables, auth rule, signature kind and the create/payment wire shape.
2. Add the route table entry and the parse/response functions; keep the generic handlers
   and the `raw_status` echo.
3. Add the signing mode in `internal/webhook/profiles.go` when the provider signs
   differently; the signature must be computed over the exact provider body.
4. Add the profile fixture and tests (parse, response shaping, signature, status matrix),
   then update `TestConnectorProvidersAreEmulated` and the connector `CONNECTOR_<PROVIDER>_*`
   stand variables.
5. Update the connector `docs/provider-onboarding.md` and run the profile e2e scenario on
   the compose stand.

The parity test fails when a connector provider has no profile, which is the intended
prompt to add one or record the reason (a sandbox-only provider) in the connector spec.

## 7. SLO and metrics

SLO is being specified in task 14.4. Base metrics: latency p95/p99, error rate, outbox lag,
consumer lag; dashboards after task 0.7.

## 8. Trade-offs

| ID | Decision | Pros | Cons and compensation |
| :-- | :-- | :-- | :-- |
| T-MOCK-01 | In-memory state (invoices, scenarios, log) | simple and deterministic stand, no dependencies | state is lost on restart, no scalability; the CBL-002 library set is not used | invoices are limited to 1000 records (503 store_full), log 100; tests recreate the service |
| T-MOCK-02 | `simulate` responds `200` for a valid request with the delivery list | tests distinguish non-delivery by `http_status`/log | non-delivery is not visible by the response code | the `GET /api/v1/webhooks` log, the `error` field and `mode` record the result; validation errors give 400/404/413/503 |
| T-MOCK-03 | The webhook format repeats contract CBL-3.5 (sandbox provider) | single source of the format for Connector and tests | the mock does not check the format on a live gateway | divergence only through updating both specs; real codes - 3.2 |
| T-MOCK-04 | Recipient redirects are not followed; simulate delivery is detached from the client context (90 s) | the body and signature do not go to a redirect; a client disconnect does not lose the webhook | the `delay` scenario occupies the handler until completion | scenario limits (5 s/10 duplicates) and `WriteTimeout` 75 s are derived together |
| T-MOCK-05 | Provider limits are emulated by error injection (`fail_status` 400/422 rejections, 429/500/502/503 unavailability plus `retry_after_s`), not by implicit counters | deterministic tests; the provider official limits are documented per profile for adapter limiter config | a burst of accepted calls never trips a real leaky bucket on the stand | real limiter behavior is tested on the connector side (kit ratelimit, 17.3); 429 handling paths are exercised deterministically; the injected 400/422 (17.27) drives the payout compensation scenario |
| T-MOCK-08 | A created payout starts in `AwaitingApproval`; the stand control `POST /api/v1/payouts/:id/simulate` advances the state (any allowed word may follow any other, the control ignores the failure scenario); the stand never moves funds | the polling path is exercised end to end (submitted -> confirmed) without fund movement; deterministic E2E steps | a test forgetting the simulate step sees a payout staying submitted (runbook); the control is stand-only like `/api/v1/scenarios` |
| T-MOCK-09 | Pull payments replay by `name` (the connector sends the platform payout id or `refund-<id>`); a payout create is not deduplicated | a connector retry after a lost response cannot orphan a second pull payment | the real API may not dedupe by name (stand aid, same class as T-MOCK-07) |
| T-MOCK-10 | The `silent` simulate flag updates an invoice without webhooks (empty event id, no deliveries) | models a lost webhook and injects provider-side divergence (amount_mismatch for GE-5) | a silent update is invisible to the platform by design |
| T-MOCK-11 | The export list renders the same body as the single-invoice read, windows by `createdTime` at unix-second granularity (inclusive) with a deterministic (time, id) sort and a clamped `take`; the same route answers the `orderId` lookup filter | one renderer; the offset cursor walk is stable; the lookup recovery keeps the connector not-found classification (empty array) | the real API clamps `take` differently; the stand clamps at the connector bound 500 |
| T-MOCK-12 | Payout/refund/export emulation is BTCPay-only; the store list and payouts filter by the caller-provided provider id | the global stand uses BTCPay; other profiles stay invoice-only (additive); a future profile cannot silently reuse the BTCPay wire | a future CoinGate/BitPay payout run needs the same work per profile |
| T-MOCK-13 | The failure scenario is a single global counter (`fail_next`): invoice calls, payout calls, the export and the payout polling consume it | one simple control (unchanged since 3.6) | on the global stand a reconciler export or a status poll can consume the injected failure before the intended call; the GE-3 step sets the scenario immediately before the payout command and pauses the reconciler export (runbook) |
| T-MOCK-14 | A payout must be consistent with its pull payment (offered method asset, amount within the held amount); the payout methods array is bounded (`maxPayoutMethods` 10) | the stand cannot hold records the real provider would reject; a body-sized methods array cannot amplify the in-memory store beyond the caps | the real provider validates more (method availability, limits); the stand check is the cheap subset |
| T-MOCK-06 | BitPay profile checks only the presence of `X-Identity`/`X-Signature`, not the ECDSA signature (the real scheme is a hex DER signature over url plus body with a compressed-key identity) | no secp256k1 dependency in the stand | a wrong request signature is not detected by the mock | real signing is validated offline by the connector unit suite (17.8) and against the BitPay test stand (17.16); the IPN `x-signature` side is fully verified |
| T-MOCK-07 | The replay map is scoped by provider plus order id | the same order id cannot collide across profiles; adapters can test replay per provider | the stand dedupes by order id even where the real provider does not | documented stand aid; the connector idempotency contract is tested by the conformance suite CONF-9 |

## 9. Known issues

| ID | Problem | Impact | Plan | Status |
| :-- | :-- | :-- | :-- | :-- |
| KI-mock-gateway-01 | CI and pre-commit are not configured | linters run manually | task 0.2 | open |
| KI-mock-gateway-02 | No authentication; simulate can generate any event; `WEBHOOK_TARGET_URL` is configured by the operator (SSRF surface, redirects are not followed); default secret `mock-secret` | the service is only suitable for a trusted-network stand | do not deploy to prod; real integrations - through the provider and mTLS (0.6) | open |

## 10. Task journal

| CBL | Date | Status | Spec | Commit |
| :-- | :-- | :-- | :-- | :-- |
| CBL-3.6 | 2026-10-14 | done, review closed (provider API, signed webhooks, failure scenarios, contract e2e, coverage 89.4%) | `.specs/CBL-3.6-mock-gateway.md` | `5cb4e7d` |
| CBL-16.10 | 2026-10-16 | done (English comments and documentation across the repository: code, tests, configs, the spec, AGENTS.md, README; e2e runs via `test/e2e/E2E.md` with scenario E2E_MockGateway_Smoke, `smoke.ps1` and `make smoke` removed; validation run PASS; coverage unchanged) | `.specs/CBL-16.10-conventions.md` | this commit |
| CBL-17.5 | 2026-10-17 | done, review closed (provider emulation profiles: five profiles with wire shapes, status tables, auth rules and signatures (canonical JSON HMAC-SHA512 of NOWPayments, token echo of CoinGate, raw HMAC base64 of BitPay, raw HMAC hex with the `sha256=` prefix of BTCPay); generic profile routes in the transport (route table `routesOf`); `webhook.ProfileSender`/`Switch` with `DeriveTarget`; simulate gained `provider`/`raw_status`, the scenario gained `fail_status`/`retry_after_s`; the delivery log carries the provider; in-process e2e scenario E2E_MockGateway_ProviderProfiles and the compose scenario E2E_MockGateway_Profiles; 4 review passes on the staged diff, findings closed: cross-profile reads answer 404 and a mismatched simulate answers 400 `provider_mismatch`, the invalid token echo derives from the secret, ProfileWebhook fails closed on a signing error, legacy problem details translated to English, signature-mode and status-matrix test gaps filled; unit coverage total 88.0%, e2e green, lint 0) | `.specs/CBL-17.5-provider-profiles.md` | this commit |
| CBL-17.8 | 2026-10-17 | done (BitPay profile wire correction found by the 17.8 security review: the official API and SDK key utilities specify decimal major-unit `price`/`amountPaid`, the `merchant/invoice` facade envelope, the required `token` body field, `posData` as the network carrier, a hex DER request signature and a compressed-key `X-Identity`; the profile parse, response shaping, auth presence check and all domain/transport tests were realigned; the connector adapter and fixtures follow in its 17.8 commit) | `.specs/CBL-17.5-provider-profiles.md` (correction section 3.2) | this commit |
| CBL-17.22 | 2026-10-20 | done (provider onboarding kit: profile checklist in the development rules and `TestConnectorProvidersAreEmulated` proving every connector real provider has a mock profile; spec in `../cbl-connector/.specs/CBL-17.22-provider-onboarding-kit.md`) | `../cbl-connector/.specs/CBL-17.22-provider-onboarding-kit.md` | this commit |
| CBL-17.26 (mock increment 2) | 2026-10-19 | done (BTCPay profile parity found by the M1 run: the invoice body now renders `amountPaid` from the invoice received amount, so the connector fetch-to-verify carries the received evidence; without it the canonical event had no amount and went to the DLQ; unit tests green) | `.specs/CBL-17.5-provider-profiles.md` (correction section 3.2) | this commit |
| CBL-17.26 (mock increment) | 2026-10-19 | done (CoinGate profile wire parity: the create request accepts the connector's `title`/`description`/`token` fields since 17.26, the real API does too; a parse test with the full connector body (fiat-priced stablecoin order, callback URL, IPN token); unit+vet+lint green) | `.specs/CBL-17.5-provider-profiles.md` (correction section 3.2) | this commit |
| CBL-17.27 | 2026-10-19 | done, review closed (provider payout, refund and export emulation for the BTCPay profile: the pull payment plus payout wire with the stand state control, the name replay, the refund reuse of the same calls, the store invoice list with the inclusive wire-granularity window, the deterministic paging and the `orderId` lookup of the connector recovery, the silent simulate flag (lost webhook and amount divergence injection), the scenario injection on the payout calls (400/422 rejections) and the payout consistency checks; 4 review passes, findings closed (the methods array bound, the sub-second window boundary, the orderId route shadowing, request `Validate()`, the taxonomy and exemption notes); unit+httptest+e2e green, coverage 88.7% (no regression), lint 0, em dash check green; the pre-existing BitPay e2e helper bug (the facade envelope unwrap, broken since 17.8) fixed; the global GE-2..GE-5 continuation runs follow in the umbrella 17.26 increment D) | `.specs/CBL-17.27-provider-payout-export.md` | this commit |

## 11. Runbook

Mock-gateway stand diagnostics:

1. `GET /api/v1/scenarios`: whether `fail_next`/`latency_ms`/`fail_status`/`retry_after_s`
   are enabled; reset - `/scenarios/reset`.
2. `GET /api/v1/webhooks`: the latest deliveries with `provider`, `http_status` and
   `error`; code 0 - a network failure (check `WEBHOOK_TARGET_URL` and Webhook Ingress),
   401 - the signature was rejected (`invalid`/`missing` mode or the secret does not match
   the Ingress provider).
3. Invoice not found - the in-memory state is lost on restart, recreate the invoice.
4. The secret and signature headers must match the Webhook Ingress configuration; for
   profiles check `WEBHOOK_SECRET_<PROVIDER>` and the derived or explicit target
   (`WEBHOOK_TARGET_URL_<PROVIDER>`).
5. Profile route 401: the expected header of the profile is missing or malformed
   (NOWPayments `x-api-key`, CoinGate `Bearer`, BTCPay `token`; BitPay requires a base64
   `X-Signature`).
6. Profile create answers 400: the provider-shaped body field or amount failed validation
   (the problem `detail` names the field); `400 missing_webhook_target` on simulate for a
   profile means the base URL has no `/hooks/mock/` segment and no explicit env target is
   set.

Payout, refund and export emulation (17.27):

1. A payout stays `submitted` on the platform until the operator advances the stand state:
   `POST /api/v1/payouts/{id}/simulate {"state":"Completed"}` (or `Cancelled` for the
   compensation path); the connector polls `GET /api/v1/stores/{store}/payouts/{id}` every
   `CONNECTOR_PAYOUT_POLL_INTERVAL`. An unknown state word is 400 and leaves the payout
   unchanged.
2. A same-name pull payment create replays the original (the connector sends the platform
   payout id or `refund-<id>`); a second payout create against the same pull payment is a
   new payout - only the connector-side reference dedupe prevents duplicates.
3. The payout must match the pull payment: the method asset must be offered and the amount
   must not exceed the held amount, otherwise 400 `invalid_request` (T-MOCK-14).
4. The export walk (`GET .../invoices?startDate&endDate&skip&take`) compares at unix-second
   granularity and answers `[]` past the end; the `orderId` filter without a window serves
   the connector lookup recovery (an unknown order answers `[]` = not found).
5. GE-3 sequencing (T-MOCK-13): `fail_next` is one global counter shared with the export
   and the payout polling; set the scenario immediately before the payout command and
   pause the reconciler export, otherwise the injected failure may be consumed by a poll.
6. The silent simulate (`POST /api/v1/simulate {"silent":true,...}`) changes the provider
   state without a webhook: use it to model a lost webhook or to inject a provider-side
   amount divergence for the reconciliation alert (GE-5).
