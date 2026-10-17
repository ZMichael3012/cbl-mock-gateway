# CBL-17.5 (cbl-mock-gateway part). Provider emulation profiles

| Field | Value |
| :-- | :-- |
| Epic | E17 (real provider integration) |
| Repository | `cbl-mock-gateway` (this spec) and `cbl-connector` (companion spec `.specs/CBL-17.5-conformance-suite.md`) |
| Size | M |
| Wave and parallel group | W5-PROVIDER |
| Depends on | CBL-17.2 (SPI v1 and manifest schema), CBL-17.3 (kit primitives), CBL-17.4 (ingress strategies) |
| Contract version | none (test stand; the wire shapes below are the profile contract) |
| Status | done |

## 1. Context and goal

CBL-3.6 emulates a single sandbox provider with one wire format (canonical JSON plus
the kit HMAC header). E17 needs contract tests of the four real adapters (17.6 - 17.9)
without network access: each provider must be emulated with its own fields, statuses,
signature scheme, failure modes and documented limits. This task replaces the
"sandbox decoder" stand with provider profiles and, together with the connector
companion spec, delivers the generalized contract suite that was task 3.7.

The profile wire shapes are this stand's contract: the real adapter tasks (17.6 -
17.9) test against them in integration, and the sandbox runs (17.16) later validate
the same shapes against the live provider APIs. Where a real detail is not yet
verified (for example BitPay IPN event codes), the approximation is marked and
owned by the adapter task.

## 2. Scope

In scope: provider profiles in `cbl-mock-gateway` (wire shapes, statuses, signing,
auth presence checks, failure injections), the scenario extension, the delivery log
extension, profile e2e scenarios and the compose smoke additions.

Out of scope: real adapters (`cbl-connector`, 17.6 - 17.9); the conformance suite
(companion spec); ECDSA request signature verification of BitPay requests (no
secp256k1 dependency in the stand, T-MOCK-06); implicit rate counters (T-MOCK-05);
Webhook Ingress configuration (17.16 wires the ensure stand).

## 3. Architecture sketch

### 3.1. Packages and layers

Layers stay `internal/transport` -> `internal/usecase` -> `internal/domain`; the
webhook adapter lives in `internal/webhook`; profiles are a new domain-level data
package used by transport and webhook (no new layer, no import cycles: domain does
not import usecase).

New and changed units:

```go
// internal/domain/profiles.go
type ProfileID string
const (
    ProfileMock        ProfileID = "mock"
    ProfileNOWPayments ProfileID = "nowpayments"
    ProfileCoinGate    ProfileID = "coingate"
    ProfileBitPay      ProfileID = "bitpay"
    ProfileBTCPay      ProfileID = "btcpay"
)

// SignatureKind selects the webhook signing scheme of a profile.
type SignatureKind string
const (
    SignatureLegacyHMAC       SignatureKind = "legacy_hmac"        // kit hmac t/v1 (mock)
    SignatureRawHMAC          SignatureKind = "raw_hmac"           // BitPay base64, BTCPay hex with prefix
    SignatureCanonicalJSONHMAC SignatureKind = "canonical_json_hmac" // NOWPayments SHA-512
    SignatureTokenEcho        SignatureKind = "token_echo"         // CoinGate
)

// SignatureSpec describes how a profile signs its webhooks.
type SignatureSpec struct {
    Kind      SignatureKind
    Header    string // raw_hmac, canonical_json_hmac, legacy_hmac
    Encoding  string // "hex" or "base64" for raw_hmac
    Prefix    string // optional digest prefix ("sha256=" of BTCPay)
    Field     string // token_echo body field ("token")
}

// Profile describes one emulated provider.
type Profile struct {
    ID                ProfileID
    DisplayName       string
    AuthHeader        string   // required non-empty request header
    AuthPrefix        string   // optional value prefix ("Bearer ", "token ")
    StatusMap         map[Status]string // canonical -> raw provider status
    RawStatuses       []string // documented raw statuses of the provider
    Signature         SignatureSpec
}

// ProfileByID returns the profile by its identifier.
func ProfileByID(id string) (Profile, bool)

// AllProfiles returns all profiles in a stable order.
func AllProfiles() []Profile

// RawStatus maps a canonical simulate status to the provider raw status.
func (p Profile) RawStatus(canonical Status) (string, error)

// HasRawStatus reports whether the raw status is in the documented list.
func (p Profile) HasRawStatus(raw string) bool
```

```go
// internal/webhook/profiles.go
// ProfileWebhook builds the provider-shaped webhook body and the signature
// header of a profile; it fails closed (an error, never an unsigned send).
func ProfileWebhook(profile domain.Profile, secret []byte, event usecase.WebhookEvent) ([]byte, string, string, error)
```

Transport additions:

```go
// internal/transport/profiles.go
// registerProfileRoutes mounts the provider API routes of all real profiles
// from the profile route table of the transport (routesOf); the mock profile
// keeps its legacy sandbox routes.
func (s *Server) registerProfileRoutes(router gin.IRouter)

// requireProfileAuth rejects requests without the profile auth header (401 problem+json).
func requireProfileAuth(profile domain.Profile) gin.HandlerFunc
```

`usecase.Service` changes (signature-compatible extensions):

```go
// CreateInvoiceRequest gains the provider and order reference:
type Request... // domain.CreateInvoiceRequest: + Provider ProfileID; OrderID string (idempotency key source)

// SimulateRequest gains the profile and the raw override:
//   Provider  ProfileID (default mock)
//   RawStatus string    (must be empty or documented in the profile)

// Output deliveries gain the provider:
//   domain.WebhookDelivery.Provider
//   usecase.Delivery.Provider
```

Scenario extension (`internal/domain/mock.go`):

```go
type Scenario struct {
    FailNext     int    // existing: number of injected failures
    LatencyMS    int    // existing
    FailStatus   int    // 0 = 503; allowed 429, 500, 502, 503
    RetryAfterS  int    // Retry-After header value when FailStatus == 429 (0..600)
}
```

### 3.2. Wire formats (profile contract)

All provider routes answer `application/json`; errors use problem+json
(`application/problem+json` via `ginmw.AbortWithProblem`). The auth check is header
presence plus prefix (the stand keeps no per-provider credentials except the webhook
signing secrets).

Amount rule: requests carry amounts as decimal strings or JSON numbers (never
float in code: parsed with `money.ParseAny`/`money.ParseJSONNumber` from kit 17.3);
the decimals table of the stand: USDT 6, USDC 6, BTC 8, ETH 18, TRX 6, LTC 8,
default 2. Responses re-emit amounts in the provider's documented form without
float arithmetic (strings of minor units scaled, or integer minor units).

Common mapping of a profile invoice: `asset = UPPER(price_currency/currency)`,
`network` as stated per profile below; `gateway_id` per profile:

| Profile | Create route | Get route | gateway_id form |
| :-- | :-- | :-- | :-- |
| mock (existing) | `POST /api/v1/invoices` | `GET /api/v1/invoices/{id}` | UUIDv7 string |
| nowpayments | `POST /v1/invoice` | `GET /v1/payment/{payment_id}` | UUIDv7 string (mock stand-in for the numeric id) |
| coingate | `POST /v2/orders` | `GET /v2/orders/{id}` | monotonic integer as string (mock stand-in for the real integer id) |
| bitpay | `POST /invoices` | `GET /invoices/{id}` | 22 alphanumeric characters (derived from a UUID) |
| btcpay | `POST /api/v1/stores/{store_id}/invoices` | `GET /api/v1/stores/{store_id}/invoices/{id}` | 32 hex characters |

Auth requirements:

| Profile | Header | Rule |
| :-- | :-- | :-- |
| nowpayments | `x-api-key` | non-empty |
| coingate | `Authorization` | prefix `Bearer ` plus non-empty token |
| bitpay | `X-Identity` and `X-Signature` | both non-empty (ECDSA is not verified, T-MOCK-06) |
| btcpay | `Authorization` | prefix `token ` plus non-empty key |

Request and response shapes:

- nowpayments create: `{order_id, price_amount, price_currency, pay_currency,
  ipn_callback_url?, order_description?, success_url?, cancel_url?}`; response
  `{id, order_id, price_amount, price_currency, pay_currency, invoice_url,
  created_at, expiration_estimate_date}`; `price_amount` is a JSON number.
  `network = LOWER(pay_currency)`. Repeat `order_id` replays the invoice
  (mock extension, documented by T-MOCK-07). Get: `{payment_id, invoice_id,
  order_id, payment_status, price_amount, actually_paid, pay_currency, updated_at}`,
  `updated_at` RFC 3339.
- coingate create: `{order_id, price_amount, price_currency, pay_currency,
  callback_url?, cancel_url?}`; response `{id, status, order_id, price_amount,
  price_currency, pay_amount, pay_currency, payment_address, created_at}`;
  amounts are decimal strings with 6 fraction digits; `status` is the raw provider
  status; `network = LOWER(pay_currency)`. Get answers the same shape with the
  current status.
- bitpay create: `{price, currency, token, orderId, notificationURL?, redirectURL?,
  posData?}`; per the official API `price` is a decimal number in major units of
  the pricing currency (the platform sends the crypto amount), `token` is the
  pairing token, `posData` carries the requested network; response is the facade
  envelope `{facade: "merchant/invoice", data: {id, url, status, price, currency,
  orderId, amountPaid, confirmations, invoiceTime, expirationTime, bitcoinAddress}}`
  with decimal major-unit `price`/`amountPaid` and Unix millisecond times;
  `network = LOWER(posData or currency)`. `X-Identity`/`X-Signature` must be
  non-empty (the ECDSA scheme and its hex encoding are not reproduced, T-MOCK-06).
  Get answers the same envelope with the current status.
  Correction 17.10.2026 (17.8): the initial 17.5 shape used integer minor-unit
  amounts, a flat body and implied a base64 request signature; the official
  BitPay documentation and SDK key utilities (hex DER signature, compressed
  public key identity) disproved that approximation during the 17.8 security
  review, and the profile was realigned to the documented contract.
- btcpay create: `{amount, currency, metadata: {orderId, itemDesc?, orderNetwork?},
  checkout: {redirectURL?}}`; response `{id, storeId, amount, currency, status,
  checkoutLink, createdTime, expirationTime, metadata}`; `amount` is a decimal
  string with 6 fraction digits; times are Unix seconds;
  `network = LOWER(metadata.orderNetwork or currency)`.

Status tables (canonical simulate -> raw provider status; `RawStatuses` is the
documented list, a `raw_status` override must come from it):

| Canonical | nowpayments | coingate | bitpay | btcpay |
| :-- | :-- | :-- | :-- | :-- |
| pending | waiting | new | new | New |
| detected | confirming | confirming | paid | Processing |
| confirmed | finished | paid | complete | Settled |
| failed | failed | invalid | invalid | Invalid |
| expired | expired | expired | expired | Expired |
| underpaid | partially_paid | invalid | paid | Processing |
| overpaid | finished | paid | paid | Settled |

Full raw lists: nowpayments `waiting, confirming, confirmed, sending,
partially_paid, finished, failed, refunded, expired`; coingate `new, pending,
confirming, paid, invalid, expired, canceled, refunded`; bitpay `new, paid,
confirmed, complete, expired, invalid`; btcpay `New, Processing, Settled, Expired,
Invalid`. The mock profile keeps the canonical statuses themselves.

Webhook bodies (simulate) and signing:

| Profile | Body highlights | Signature |
| :-- | :-- | :-- |
| mock (existing) | sandbox contract unchanged | kit hmac header `X-Billing-Signature` |
| nowpayments | `{payment_id, order_id, payment_status, price_amount, actually_paid, pay_currency, updated_at}`; `payment_id` is the mock gateway id, `order_id` is our payment id | canonical JSON HMAC-SHA512 hex, header `x-nowpayments-sig` |
| coingate | `{id, order_id, status, price_amount, price_currency, pay_amount, pay_currency, token, created_at}` | token echo: `token` equals the profile secret |
| bitpay | `{event: {code, name}, data: {id, orderId, status, price, currency, amountPaid, confirmations}}` | raw HMAC-SHA256 base64 over the body, header `x-signature` |
| btcpay | `{deliveryId, webhookId, type, invoiceId, storeId, metadata}` | raw HMAC-SHA256 hex with prefix `sha256=`, header `BTCPay-Sig` |

BitPay `event.name` mapping (mock approximation, owned by 17.8): pending ->
`invoice_created` (1000), detected -> `invoice_paidInFull` (1001), confirmed ->
`invoice_completed` (1003), expired -> `invoice_expired` (1004), failed ->
`invoice_failedToConfirm` (1005), underpaid/overpaid are carried by
`invoice_paidInFull` with `amountPaid` differing from `price`. BTCPay `type`
mapping: pending -> `InvoiceCreated` (mock extension, BTCPay emits no created
event), detected/underpaid -> `InvoiceProcessing` (or `InvoiceReceivedPayment`
when confirmations are below the profile threshold, simplified), confirmed ->
`InvoiceSettled`, expired -> `InvoiceExpired`, failed -> `InvoiceInvalid`.

The event id header (`X-Event-Id` by default) is added to every profile delivery
as a stand-only aid for sinks and e2e; it is not part of the provider contract.

Signature modes: `valid` (correct), `invalid` (corrupted digest; for token echo a
non-matching token), `missing` (no header or the token field removed). Deliveries
are logged with the provider.

### 3.3. Data flows

Provider API call (any profile): request -> transport profile route -> auth
presence check (401) -> strict JSON bind (400/413) -> profile request mapping
(400 on invalid amount/currency) -> scenario injection (`FailStatus`,
`Retry-After`, latency) -> `usecase.CreateInvoice`/`GetInvoice` -> profile response
mapping. Invoice storage keys `provider + ":" + order_id` for the replay map, so
ordering collisions across profiles are impossible.

Simulate flow: request (`gateway_id`, canonical `status`, optional `raw_status`,
`provider`, amount/confirmations/txid/event time, delay/duplicate, signature mode)
-> profile resolution (400 unknown) -> raw status resolution (400 unsupported
canonical or unknown raw override) -> invoice state update -> webhook body of the
profile -> target URL resolution (per-profile env or derived, 400 when missing) ->
signed send with retries of duplicates -> delivery log with the provider.

Failure branches: unknown profile -> 400 `unknown_provider`; unsupported canonical
status for the profile -> 400 `unsupported_status`; unknown raw override -> 400
`unknown_raw_status`; missing auth header -> 401 `unauthorized`; injected failure
-> `FailStatus` (default 503) with `Retry-After` when 429; store full -> 503
`store_full` (unchanged); unknown invoice -> 404 (unchanged).

### 3.4. Storage and migrations

No storage changes: the stand state is in-memory (T-MOCK-01). `domain.Invoice`
gains `Provider` and `RawStatus` fields; the delivery log gains `Provider`.

### 3.5. Error taxonomy

Existing classes stay: `ErrInvalidRequest` -> 400, `ErrInvoiceNotFound` -> 404,
`ErrInjectedFailure` -> scenario status (503 by default), `ErrStoreFull` -> 503.
New wrapped uses of `ErrInvalidRequest`: unknown provider profile, unsupported
canonical status, unknown raw status, missing webhook target for a profile.

### 3.6. Decisions and rejected alternatives

- D1. Profiles are data (status tables, signature specs) plus small mapping
  functions; profile-specific parse/shape code is contained in the profile
  module of the domain, and the transport only consults the profile route table
  (`routesOf`); adding a profile touches the profile data, the profile API
  shaping and the webhook body builder, never the generic handlers (mirror of
  the connector rule 14.5.2 at the stand level).
- D2. Limits are emulated by injection (`fail_status=429` plus `retry_after_s`),
  not by implicit counters: deterministic tests matter more than a faithful leaky
  bucket in a stand (T-MOCK-05).
- D3. BitPay request ECDSA is not verified (T-MOCK-06): presence of the identity and
  signature headers only; the signature encoding follows the official SDK key
  utilities (hex DER over url plus body, compressed public key identity, corrected
  in the 17.8 review), and real signing is exercised by 17.8 unit proofs plus the
  BitPay test stand in 17.16.
- D4. Wire shapes are the stand contract now and are re-validated against live
  APIs by 17.16; approximations are marked (BitPay event codes, BTCPay types).
- Rejected: separate mock service per provider (plan 14.3 explicitly forbids it);
  profile selection by header instead of path (adapters use real provider paths,
  so the paths must differ).

## 4. Functional requirements

FR-1. Five profiles exist (`mock` plus the four providers) with the wire shapes,
auth rules, status tables and signatures of section 3.2.

FR-2. The mock profile behavior is fully backward compatible: routes, bodies,
signatures and statuses of CBL-3.6 do not change (existing tests stay green).

FR-3. Provider routes perform the profile auth presence check (401 problem+json)
before any state change.

FR-4. `POST /api/v1/simulate` accepts `provider` (default `mock`) and
`raw_status` (optional; must be a documented raw status of the profile) and sends
the provider-shaped body signed per the profile to the profile target URL.

FR-5. The scenario gains `fail_status` (0, 429, 500, 502, 503) and `retry_after_s`
(0 - 600; `retry_after_s > 0` requires `fail_status` 429 and sets `Retry-After`).

FR-6. The delivery log and the simulate response carry the provider of the send.

FR-7. Webhook targets: `WEBHOOK_TARGET_URL_<PROFILE>` wins; otherwise the target
is derived from `WEBHOOK_TARGET_URL` by replacing the `/hooks/mock/` path segment
with `/hooks/<profile>/`; when neither yields a target, simulate answers 400
`missing_webhook_target`.

FR-8. Secrets: `WEBHOOK_SECRET_<PROFILE>` with stand defaults
(`mock-nowpayments-secret`, `mock-coingate-token`, `mock-bitpay-secret`,
`mock-btcpay-secret`); the mock profile keeps `WEBHOOK_SECRET`.

FR-9. Amount parsing uses the kit money parser (no float); the decimals table of
section 3.2 applies; amounts in responses are emitted without float arithmetic.

## 5. Contracts

REST additions (stand API, not part of `cbl-protos`): the profile routes of
section 3.2 and the extended `/api/v1/simulate`, `/api/v1/scenarios` request and
response fields (provider, raw_status, fail_status, retry_after_s). The connector
companion spec mirrors the wire shapes as conformance fixtures.

## 6. Data model and migrations

Not applicable (in-memory, T-MOCK-01). Field additions only (section 3.4).

## 7. State machines and rules

The canonical simulate statuses and aliases of CBL-3.6 stay the platform-side
input; the profile status tables of section 3.2 translate them to raw provider
statuses. A `raw_status` override changes only the raw value carried in the body
and the stored invoice; the canonical state of the sent event remains the
`status` field of the request (so adapters can exercise every raw status while the
platform-side model stays deterministic).

## 8. Edge cases

E-1. Unknown provider in simulate -> 400.
E-2. Canonical status unsupported by the profile (none in the current tables,
kept for future profiles) -> 400.
E-3. Raw override not in the documented list -> 400.
E-4. Missing auth header -> 401 with problem+json, no state change.
E-5. Repeat `order_id` within a profile replays the invoice; the same order id in
another profile creates a separate invoice.
E-6. Missing webhook target for a profile -> 400 `missing_webhook_target`.
E-7. `fail_status` outside the allowed set or `retry_after_s` outside 0 - 600,
or `retry_after_s > 0` without `fail_status` 429 -> 400 (scenario validation).
E-8. A BitPay request without `X-Identity` or `X-Signature` -> 401 (presence check;
the stand does not reproduce the ECDSA scheme).
E-9. Amounts with more fraction digits than the decimals table -> 400
(`invalid_amount`).
E-10. BTCPay store id mismatch between create and get does not matter (the stand
serves any store id); documented stand simplification.
E-11. A profile route that addresses an invoice of another provider (for
example a CoinGate id through the NOWPayments get route) answers 404
`invoice_not_found`: the provider of an invoice is fixed at creation, and a
cross-profile read must not render a provider-shaped body for a foreign id.
Simulate with a `provider` that does not match the invoice provider answers 400
`provider_mismatch`.
E-12. Problem message texts of the legacy mock routes were translated to English
in this task; the error codes and statuses are unchanged (the message text is
not part of the stand contract).

## 9. Non-functional requirements

- NFR-1: the added profile code keeps unit+integration coverage at least 80% for
  the repository (measured repository-wide).
- NFR-2: bodies stay bounded by the existing 64 KiB limit; time formats per
  provider do not produce unbounded output.
- NFR-3: no new dependencies (kit `money`, `webhooksig` canonical serializer,
  stdlib `hmac` are enough; no secp256k1).
- NFR-4: deterministic responses: no randomness except identifiers; no wall-clock
  dependence in shapes beyond provider time fields derived from the event time.

## 10. Test plan

### 10.1. Unit matrix (no Docker)

| ID | Case | Input | Expected |
| :-- | :-- | :-- | :-- |
| U-01 | profile lookup | `ProfileByID("coingate")` | profile with token_echo spec, `Bearer ` prefix |
| U-02 | profile lookup unknown | `ProfileByID("nope")` | `ok=false` |
| U-03 | raw status mapping | each canonical status x each profile | raw string of the table |
| U-04 | raw status lists | each profile | documented list matches section 3.2; canonical values unique |
| U-05 | raw override validation | `HasRawStatus("partially_paid")` on nowpayments / on mock | true / false |
| U-06 | decimals table | `MinorUnits("USDT","12.34")` / `"BTC","0.00000001"` | 12340000 / 1 |
| U-07 | decimals overflow | `"USDT","1.1234567"` | error `invalid_amount` |
| U-08 | scenario validation | `FailStatus 418`, `RetryAfterS 601`, valid 429/30 | error / error / ok |
| U-09 | NOW body | event fixture | JSON matches shape, `payment_id`=gateway, `order_id`=payment, RFC 3339 `updated_at` |
| U-10 | NOW signature | body + secret | `x-nowpayments-sig` verifies with `webhooksig.CanonicalJSONHMAC` SHA512 |
| U-11 | CoinGate body | event fixture | `token` equals secret, `id` numeric, amounts as strings |
| U-12 | BitPay body | event fixture | `event.name` per status table, `invoiceTime` ms, base64 `x-signature` verifies with RawHMAC SHA256 Base64 |
| U-13 | BTCPay body | event fixture | `type` per mapping, `BTCPay-Sig` = `sha256=<hex>` verifies with RawHMAC SHA256 Hex prefix |
| U-14 | invalid/missing modes | each profile x modes | corrupted digest / absent header or token |
| U-15 | profile request mapping | NOW create body -> CreateInvoiceRequest | asset/network/units per table |
| U-16 | CoinGate request mapping | decimal string amounts | minor units |
| U-17 | BTCPay request mapping | `metadata.orderNetwork` | network override |
| U-18 | auth middleware | each profile, missing/empty/wrong-prefix header | 401; correct prefix passes |
| U-19 | target derivation | base `/hooks/mock/ep1` | `/hooks/coingate/ep1` for coingate; explicit env wins; no match and no env -> error |
| U-20 | replay map per profile | same order id in two profiles | two invoices |
| U-21 | existing regression | all CBL-3.6 unit tests | green unchanged |

### 10.2. Integration matrix (httptest, no Docker)

| ID | Case | Input | Expected |
| :-- | :-- | :-- | :-- |
| I-01 | NOW create + get round trip | auth header, create body | 201, response shape, get echoes status `waiting` -> after simulate raw status |
| I-02 | CoinGate create + get | `Bearer` token | 201, integer id, decimal string amounts |
| I-03 | BitPay create + get | identity/signature headers | 201, 22-char id, ms times |
| I-04 | BTCPay create + get | `token` header | 201, 32-hex id, unix seconds |
| I-05 | auth rejection matrix | no header per profile route | 401, no invoice created (get -> 404) |
| I-06 | injected failure | `fail_status=429, retry_after_s=30` then create | 429, `Retry-After: 30`, counter decremented |
| I-07 | injected failure default | `fail_status=0` | 503 |
| I-08 | simulate provider routing | simulate coingate | body signed with token, target `/hooks/coingate/ep1` derived |
| I-09 | simulate raw override | `status=confirmed, raw_status=partially_paid` on nowpayments | body `payment_status=partially_paid`, canonical event status `confirmed` |
| I-10 | simulate unknown raw | `raw_status=nope` | 400 `unknown_raw_status` |
| I-11 | simulate missing target | base without `/hooks/mock/`, no env | 400 `missing_webhook_target` |
| I-12 | delivery log | simulate with provider | entry carries provider |

### 10.3. Smoke set

`make build`, `make test`, `make vet`; repository coverage unchanged or higher.

### 10.4. E2E scenario descriptions

| Scenario | Description | Tag or stand |
| :-- | :-- | :-- |
| E2E_MockGateway_ProviderProfiles (in-process, tag `e2e`) | for each of the four real profiles: create via the provider route with the auth header (and 401 without); verify the response shape and replay by order id; simulate one canonical status per profile to a sink that verifies with the profile strategy from kit `webhooksig` (raw base64 / canonical SHA-512 / token echo / raw hex prefix) and checks the event id header; simulate an invalid signature and assert the sink rejects it; inject `fail_status=429` and assert the API answers 429 with `Retry-After` | `go test -tags e2e` |
| E2E_MockGateway_Profiles (compose) | smoke additions: create a CoinGate order with `Bearer`, 401 without the header, simulate `provider=coingate` and assert a delivery entry (network error allowed as in the existing smoke) | `test/e2e/E2E.md` |

Scheduling: the in-process scenario runs on every e2e invocation; the compose
scenario runs in this task's local e2e run and afterwards with the stand smoke.

### 10.5. Load plan

Not applicable (stand change of the mock service; the platform load profile and
ingress/connector loads are unchanged).

### 10.6. Test-first evidence

Red run recorded before implementation (repository `cbl-mock-gateway`,
17.10.2026): `go test ./...` failed to compile with
`undefined: domain.Profile`, `domain.ProfileByID`, `usecase.WithTargets`,
`unknown field Provider in struct literal of type usecase.WebhookEvent` and
`undefined: webhook.ProfileWebhook`; the existing `usecase` suite stayed green;
`go test -tags e2e ./test/e2e` failed with the same undefined symbols. Review
closure round: findings of the four reviewers closed (cross-profile invoice
reads and simulate reject 404/400, the invalid token echo mode derives from the
secret, `ProfileWebhook` fails closed on a signing error, English problem
details, U-03/I-05 and BitPay/BTCPay signature-mode test gaps filled,
documentation and sketch alignment).

## 11. Work plan and commit

Single commit in `cbl-mock-gateway`: spec, profiles, transport, webhook, tests,
e2e scenario, `E2E.md` additions, `AGENTS.md`, `README.md`. The companion commit
in `cbl-connector` carries the conformance suite.

## 12. Open questions and decisions

- The BitPay IPN `posData` echo is included in the body as an empty string
  (stand simplification, harmless for adapters).
- CoinGate `pay_amount` echoes the invoice amount (no partial amounts in the mock
  beyond the underpaid mapping; `invalid` carries the received amount).
- Decision D2 (injection instead of counters) is recorded as trade-off T-MOCK-05
  in `AGENTS.md`.
- Decision D3 (no ECDSA verification) is recorded as trade-off T-MOCK-06.
- Decision (replay map keyed by provider plus order id) is recorded as T-MOCK-07.
