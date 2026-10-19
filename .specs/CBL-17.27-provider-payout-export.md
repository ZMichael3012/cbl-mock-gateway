# CBL-17.27 (cbl-mock-gateway part). Provider payout, refund and export emulation

## 1. Context and goal

The 17.26 increment D global E2E run (`test/e2e/.runs/20261019t130356z/` in the umbrella
repository) proved the mock-gateway gap: GE-2/GE-3/GE-4 (payout and refund saga) are blocked
because the mock emulates invoice create/get only, and GE-5 is blocked because the connector
export (`ListGatewayPayments`) calls the BTCPay store invoice list, which the mock does not
serve (stand evidence: `NotFound: GET /api/v1/stores/mock-store/invoices?endDate=...&skip=0&startDate=...&take=10`).

Goal: extend the BTCPay profile of the mock-gateway with the payout and refund wire shapes
(the pull payment plus payout flow verified against the real BTCPay regtest stand in 17.9),
the store invoice list used by the connector export, and a silent simulate control that
models a lost webhook and injects provider-side amount divergence for GE-5. This unblocks
GE-2/GE-3/GE-4/GE-5 (the Kafka half) on the global stand.

## 2. Scope

In scope:

- BTCPay profile payout emulation: pull payment create, payout create, payout read, the
  stand payout state control (simulate);
- refund emulation: the connector refund for BTCPay uses the same two provider calls
  (pull payment plus payout, `CreateRefund` in `internal/providers/btcpay/adapter.go`), so
  payout support is refund support; the refund read aliases the payout read;
- the store invoice list (export) endpoint for the connector export walk;
- the silent simulate flag (state change without webhook deliveries);
- scenario injection (`fail_next`/`fail_status`/`retry_after_s`) on the payout calls;
- unit, httptest and in-process e2e coverage in this repository.

Out of scope:

- payout/refund emulation for the other profiles (CoinGate/BitPay/NOWPayments): the global
  stand uses the BTCPay mock; the shapes stay additive for a future stand need;
- payout webhooks: the connector resolves payout states by polling (`GetPayout`); the real
  BTCPay payout webhooks are not emulated;
- any fund movement: the stand never moves funds; payout states are operator-controlled
  (T-MOCK-08);
- platform contracts: none change; the mock emulates the provider wire only.

## 3. Architecture sketch

### 3.1. Packages and layers

- `internal/domain/profile_api.go`: the payout wire parse and render functions
  (`ParsePullPaymentCreate`, `ParsePayoutCreate`, `PayoutResponse`, `PullPaymentResponse`)
  and the invoice list response reuse (`CreateInvoiceResponse` bodies are the list rows);
- `internal/domain/mock.go` (or a new `payout.go`): the `PullPayment` and `Payout` models,
  the payout state vocabulary and validation, `ErrPayoutNotFound`, `ErrPullPaymentNotFound`;
- `internal/usecase/service.go`: the in-memory stores (`pullPayments`, `pullByName`,
  `payouts`), `CreatePullPayment`, `CreatePayout`, `GetPayout`, `SimulatePayout`,
  `ExportInvoices`; the existing `applyScenario` is called by the payout calls and the
  export;
- `internal/transport/profiles.go`: the optional payout route table in `profileRoutes`
  (`Payout *payoutRoutes`), mounted by `registerProfileRoutes` with the profile auth;
- `internal/transport/server.go`: the payout simulate control route and the `silent` flag
  in the simulate request; the new error mappings.

### 3.2. Wire formats (profile contract, from the connector adapter and the 17.9 regtest run)

All provider routes require `Authorization: token <token>` (the profile auth rule).

1. `POST /api/v1/stores/:store_id/pull-payments`
   body `{"name":"<platform payout id or refund-<id>>","amount":"0.00100000","currency":"BTC","payoutMethods":["BTC"]}`
   -> 201 `{"id":"<32 hex>","name":...,"amount":...,"currency":...,"payoutMethods":[...],"viewLink":"..."}`;
   a repeat with the same name returns the original pull payment (200, stand aid T-MOCK-09).
2. `POST /api/v1/pull-payments/:pull_payment_id/payouts`
   body `{"destination":"<address>","amount":"0.00100000","payoutMethodId":"BTC-CHAIN"}`
   -> 200 `{"id":"<32 hex>","state":"AwaitingApproval","pullPaymentId":"<id>"}`.
3. `GET /api/v1/stores/:store_id/payouts/:payout_id`
   -> 200 `{"id":...,"state":"<provider state>","pullPaymentId":...}`.
4. `POST /api/v1/payouts/:payout_id/simulate` (stand control, no provider equivalent)
   body `{"state":"InProgress"|"Completed"|"Cancelled"|"AwaitingApproval"|"AwaitingPayment"}`
   -> 200 the payout body; an unknown state word is 400.
5. `GET /api/v1/stores/:store_id/invoices?startDate=<unix>&endDate=<unix>&skip=<n>&take=<n>`
   -> 200 a JSON array of the BTCPay invoice bodies (the same shape as `GET .../invoices/:id`),
   filtered by `createdTime` in `[startDate, endDate]` (inclusive, compared at the wire
   granularity of unix seconds), sorted deterministically by (`createdTime`, `id`) ascending,
   paged by `skip`/`take` (`take` default 100, clamped to 500); only BTCPay invoices are
   listed; no rows answer `[]`. An `orderId=<reference>` query filters by the platform
   payment without a window (the real API supports it and the connector `LookupByOrderID`
   recovery relies on the route): the answer is the matching array or `[]`, so an unknown
   order keeps the connector not-found classification.
6. `POST /api/v1/simulate` gains `"silent": true`: the invoice state is updated and no
   webhook is sent; the response is `{"event_id":"","deliveries":[]}` (models a lost
   webhook and injects provider-side divergence for GE-5).

### 3.3. Data flows

- GE-2: payout saga -> `payout.commands` -> connector processor -> pull payment plus payout
  -> `submitted` -> the status worker polls `GET .../payouts/:id` -> the operator (the E2E
  step) advances the state with the simulate control -> `confirmed` -> the saga commits the
  reserve and posts.
- GE-3: `POST /api/v1/scenarios {"fail_next":1,"fail_status":400}` -> the pull payment call
  answers the injected 400 -> the connector classifies a rejection -> `failed` -> the saga
  releases the hold (`compensated`).
- GE-4: a refund payout (the `refund_invoice_id` marker) -> the connector resolves the
  original invoice provider and calls the same pull payment plus payout with the refund
  name; the read aliases the payout read.
- GE-5: the connector export lists the store invoices; the reconciler compares the provider
  row with the payment; a silent simulate with a different `received_amount` after the
  platform already recorded the amount produces the `amount_mismatch` alert.

### 3.4. Storage and migrations

No migrations: the state stays in memory (T-MOCK-01). New caps: at most 1000 pull payments
and 1000 payouts (503 `store_full`), mirroring `maxInvoices`.

### 3.5. Error taxonomy

| Case | Status | Code |
| :-- | :-- | :-- |
| Missing or malformed profile auth | 401 | `unauthorized` |
| Malformed JSON body (bind) | 400 | `invalid_json` |
| Field validation of a decoded body | 400 | `invalid_request` |
| Unknown payout | 404 | `payout_not_found` |
| Unknown pull payment | 404 | `pull_payment_not_found` |
| Unknown invoice | 404 | `invoice_not_found` |
| Store cap reached | 503 | `store_full` |
| Injected scenario failure | the configured status | `provider_unavailable` |
| Missing/odd export window parameters | 400 | `invalid_request` |

### 3.6. Decisions and rejected alternatives

| ID | Decision | Pros | Cons and compensation |
| :-- | :-- | :-- | :-- |
| T-MOCK-08 | A created payout starts in `AwaitingApproval`; the stand control `POST /api/v1/payouts/:id/simulate` advances the state (real providers move funds; the stand never does); any allowed state word may follow any other (the control is not a provider state machine) and the control deliberately ignores the failure scenario (it is not a provider API call and the connector never invokes it) | the polling path is exercised end to end (submitted -> confirmed) without fund movement; deterministic E2E steps | a test forgetting the simulate step sees a payout staying submitted (documented in the runbook); the control endpoint is stand-only, like `/api/v1/scenarios` |
| T-MOCK-09 | Pull payments replay by `name` (the connector sends the platform payout id or `refund-<id>` as the name): the same name returns the original pull payment; a payout create against the same pull payment is not deduplicated (only the provider-side reference dedupe of the connector matters) | a connector retry after a lost response cannot orphan a second pull payment; mirrors the provider-side dedupe reference of T-CONNECTOR-15 | the real API may not dedupe by name; the stand aid is documented (same class as T-MOCK-07) |
| T-MOCK-10 | The `silent` simulate flag updates the invoice without webhooks; the response carries an empty event id and no deliveries | one control surface models a lost webhook and injects provider-side divergence (amount_mismatch for GE-5) without touching the platform | a silent update is invisible to the platform by design; the caller must remember it is the injection |
| T-MOCK-11 | The export list renders the same body as the single-invoice read, windows by `createdTime` at the wire granularity (unix seconds, inclusive) with a deterministic (time, id) sort and a clamped `take`; the same route answers the `orderId` filter of the connector lookup | one renderer, no drift between get and list; the offset cursor walk is stable; the lookup recovery keeps its not-found classification (an empty array) | the window is inclusive on both ends; the real API clamps `take` differently, the stand clamps at the connector bound 500 |
| T-MOCK-12 | Payout/refund/export emulation lands for the BTCPay profile only; the store list and the payout records are filtered by the caller-provided provider id | the global stand uses BTCPay; the other profiles stay invoice-only until a stand needs them (additive); a future profile cannot silently reuse the BTCPay wire | a future CoinGate/BitPay payout run needs the same work per profile; the connector conformance fixtures already carry the shapes, so the adapter side is proven |
| T-MOCK-13 | The failure scenario is a single global counter (`fail_next`): invoice calls, payout calls, the export and the payout polling all consume it | one simple control for the whole stand (unchanged since 3.6) | on the global stand a reconciler export or a status poll can consume the injected failure before the intended call; the GE-3 step must set the scenario immediately before the payout command and pause the reconciler export (the run instruction records the sequencing) |
| T-MOCK-14 | The payout must be consistent with its pull payment: the method asset must be offered and the amount must not exceed the held amount; the payout methods array is bounded (maxPayoutMethods 10) | the stand cannot hold records the real provider would reject; a body-sized methods array cannot amplify the in-memory store beyond the record caps (security review finding) | the real provider validates more (method availability, limits); the stand check is the cheap subset |

## 4. Functional requirements

- FR-1: `POST /api/v1/stores/:store_id/pull-payments` creates a pull payment (auth, body
  validation: name non-empty bounded, amount positive decimal of the currency, currency a
  valid code, payoutMethods non-empty, bounded by `maxPayoutMethods` (10) and each a valid
  code); a same-name repeat replays.
- FR-2: `POST /api/v1/pull-payments/:id/payouts` creates a payout against an existing pull
  payment (destination non-empty bounded, amount positive, method id a valid code) in
  `AwaitingApproval`; the payout must be consistent with the pull payment (the method asset
  is offered and the amount does not exceed the held amount, T-MOCK-14).
- FR-3: `GET /api/v1/stores/:store_id/payouts/:id` returns the payout body.
- FR-4: `POST /api/v1/payouts/:id/simulate` sets an allowed provider state word
  (`AwaitingApproval`, `AwaitingPayment`, `InProgress`, `Completed`, `Cancelled`) and
  returns the body; other words are 400.
- FR-5: `GET /api/v1/stores/:store_id/invoices` lists BTCPay invoices by the window with
  the deterministic sort and skip/take paging; an `orderId` query serves the connector
  lookup without a window.
- FR-6: `POST /api/v1/simulate` with `silent` updates the invoice and sends nothing.
- FR-7: the payout calls and the export apply the failure scenario (latency, injected
  status, Retry-After); the payout state control is deliberately exempt (T-MOCK-08).
- FR-8: caps: 1000 pull payments, 1000 payouts, 503 `store_full` beyond.

## 5. Contracts

No platform contract changes. The provider wire shapes are the profile contract of this
spec (section 3.2), aligned with the connector adapter (verified in 17.9 against the real
BTCPay regtest stand) and the Greenfield API.

## 6. Data model and migrations

- `domain.PullPayment`: ID (32 hex), Name, Amount (money.Money), PayoutMethods []string,
  CreatedAt.
- `domain.Payout`: ID (32 hex), PullPaymentID, Destination, Amount, MethodID, State
  (provider word), CreatedAt.
- No migrations (in-memory, T-MOCK-01); caps per FR-8.

## 7. State machines and rules

- The payout state vocabulary is the connector `payoutStates` table: `AwaitingApproval`,
  `AwaitingPayment`, `InProgress` (submitted), `Completed` (confirmed), `Cancelled`
  (failed). The stand creates `AwaitingApproval`; only the simulate control changes it.
- The export window is inclusive: `startDate <= createdTime <= endDate` compared at the
  wire granularity (unix seconds); `skip >= 0`, `take >= 1` (missing or 0 applies the
  default 100, the value is clamped to 500); `startDate <= endDate` required; negative
  `skip`/`take` and non-integer values are 400. An `orderId` query bypasses the window
  requirement (the lookup filter).

## 8. Edge cases

- A same-name pull payment replay returns the original (FR-1) but a payout create against
  it still creates a new payout (the connector dedupes by the pull payment name only).
- An export request without `startDate`/`endDate` or with `startDate > endDate` is 400;
  an `orderId` query needs no window and answers the matching array (possibly `[]`).
- The window and the sort compare at the wire granularity: an invoice at `endDate + 900ms`
  still carries `createdTime == endDate` and is included.
- An export page past the end returns `[]` (not null).
- A silent simulate of a foreign-provider invoice is rejected like the normal simulate
  (provider mismatch, 400); a silent simulate without a webhook target is accepted (no
  delivery is attempted).
- A payout simulate with a raw word outside the vocabulary is 400; the state is unchanged.
- Cross-profile reads stay 404 (`GET .../invoices/:id` of a mock invoice through the btcpay
  route and vice versa); the list serves only BTCPay invoices.

## 9. Non-functional requirements

- Body limit unchanged (64 KiB); in-memory caps keep the unauthenticated stand bounded.
- The export walk is deterministic: stable sort (createdTime, id), no map iteration order.
- No new dependencies; the service keeps the CBL-002 deviation T-MOCK-01.

## 10. Test plan

### 10.1. Unit matrix (no Docker)

| ID | Case | Expected |
| :-- | :-- | :-- |
| U-01 | ParsePullPaymentCreate valid body | name/amount/currency/methods parsed, amount positive |
| U-02 | ParsePullPaymentCreate: empty name / negative amount / bad currency scale / short currency / empty methods / methods over the bound / invalid method chars | invalid_request |
| U-03 | ParsePayoutCreate valid body | destination/amount/method parsed |
| U-04 | ParsePayoutCreate: empty destination / bad method id (empty and invalid chars) | invalid_request |
| U-05 | PayoutResponse / PullPaymentResponse render (incl. viewLink) | the wire field names of section 3.2 |
| U-06 | Payout state validation | the five allowed words pass, others fail |
| U-07 | Export window filter and sort (incl. the sub-second boundary) | rows inside the window, sorted by (createdTime, id) |
| U-08 | Export skip/take paging and take clamp | pages correct, take clamped to 500 |
| U-09 | PullPaymentRequest/PayoutRequest.Validate bounds; PullPayment.AllowsPayout (foreign method, over amount, foreign asset) | invalid_request on each violation |

### 10.2. Integration matrix (httptest, no Docker)

| ID | Case | Expected |
| :-- | :-- | :-- |
| I-01 | Pull payment create with auth | 201 with id; no auth 401 |
| I-02 | Pull payment same-name replay | 200, the same id |
| I-03 | Payout create against the pull payment | 200, `AwaitingApproval`, the pull payment id |
| I-04 | Payout create against an unknown pull payment | 404 `pull_payment_not_found` |
| I-05 | Payout read | 200, the body; unknown id 404 `payout_not_found` |
| I-06 | Payout simulate Completed then read | the state changes; unknown word 400 |
| I-07 | Invoice list window: inside, outside, paging, empty | the exact rows; `[]` for an empty page |
| I-08 | Invoice list validation: missing/odd params; skip past the end | 400 `invalid_request`; `[]` |
| I-09 | Silent simulate | no delivery recorded, `event_id` empty, the invoice read reflects the new state; a foreign provider is 400 `provider_mismatch` |
| I-10 | Scenario injection on the pull payment create and the export | the injected status, the Retry-After header on 429, the latency delay; the export 503 |
| I-11 | Store caps (pull payments and payouts) | 503 `store_full` beyond the caps |
| I-12 | Cross-profile: mock invoice id through the btcpay payout routes | 404 |
| I-13 | Invoice list `orderId` lookup | the matching row; an unknown order answers `[]` |
| I-14 | Refund-name replay and distinct payouts | the `refund-<id>` pull payment replays by name; two payout creates produce two ids |

### 10.3. Smoke set

The existing `E2E_MockGateway_Smoke` stays green (no regression).

### 10.4. E2E scenario descriptions

- In-process scenario `E2E_MockGateway_ProviderPayoutExport` (tag e2e, existing pattern):
  create a BTCPay invoice through the profile route -> export the store list and find the
  invoice -> create a pull payment -> create a payout -> read it (`AwaitingApproval`) ->
  simulate `Completed` -> read it again -> silent-simulate the invoice with a different
  amount and verify the export reflects it while no webhook was sent.
- Global stand (umbrella, GE-2/GE-3/GE-4/GE-5): the connector payout pipeline against the
  mock; the saga reserve/submit/confirm/posting (GE-2), the injected 400 compensation
  (GE-3), the refund marker chain (GE-4), the export-driven amount_mismatch alert (GE-5).
  These runs belong to the 17.26 increment D continuation and are recorded in the umbrella
  run directory.

### 10.5. Load plan

Not applicable: the mock is a stand service (CBL-3.6 scope); no load changes.

### 10.6. Test-first evidence

Red run recorded before the implementation on the parent commit `6aa690c`: the domain package
fails to build (`ParsePullPaymentCreate`/`ParsePayoutCreate`/`PullPayment`/`Payout`/
`PayoutStateAwaitingApproval` undefined), 11 transport tests fail (`TestPayoutAuthAndPullPaymentCreate`,
`TestPullPaymentReplayByName`, `TestPayoutCreateAndRead`, `TestPayoutCreateUnknownPullPayment`,
`TestPayoutReadUnknown`, `TestPayoutSimulateStates`, `TestInvoiceListWindowAndPaging`,
`TestInvoiceListValidation`, `TestSilentSimulateSkipsWebhooks`, `TestPayoutScenarioInjection`,
`TestPayoutStoreCap`) and the e2e scenario fails at the first list call (expected 200, the
route did not exist). The transcript is kept in the task session (118 lines).

### 10.7. Review closure (19.10.2026)

Four review passes on the staged diff; findings closed before the commit:

- security (blocking): the unbounded `payoutMethods` array could amplify the in-memory
  store beyond the record caps - bounded by `maxPayoutMethods` (10) with a validation test
  and T-MOCK-14;
- the export window and the sort now compare at the wire granularity (unix seconds) so a
  sub-second part cannot fall outside the inclusive window (boundary test);
- the store list route answers the `orderId` filter (the connector `LookupByOrderID`
  recovery keeps its not-found classification; the route no longer shadows it with a 400);
- `PullPaymentRequest`/`PayoutRequest` gained `Validate()` called by the usecase (the
  domain validation pattern of the invoice path); the payout is cross-checked against the
  pull payment (T-MOCK-14);
- the malformed-JSON taxonomy is documented (`invalid_json` vs `invalid_request`);
- the payout simulate scenario exemption, the global `fail_next` counter scope (T-MOCK-13)
  and the stand-control transition freedom (T-MOCK-08) are recorded;
- test additions: 429/Retry-After and latency injection, the payout cap over HTTP, the
  `orderId` lookup, the refund-name replay, the silent provider mismatch, the unchanged
  state after a rejected word, skip-past-end, the sub-second boundary, the methods cap and
  the invalid-character method; the e2e reads the payout again after the simulate;
- the pre-existing BitPay e2e helper bug (the facade envelope was never unwrapped since
  17.8) is fixed and the BitPay fixture realigned to the current decimal-major-unit shape;
- coverage after the closure: domain 90.7%, transport 85.2%, usecase 91.4%, webhook 87.6%,
  total 88.7% (no regression against the 88.0% baseline).

## 11. Work plan and commit

1. This spec plus the CBL-004 task row (umbrella).
2. Red tests: the unit, httptest and e2e tests of section 10 against the parent code;
   record the failing run.
3. Implementation: the domain models and wire functions, the usecase stores and methods,
   the transport routes and handlers, the silent simulate flag.
4. Green runs: `go test ./...`, `go test -tags e2e ./test/e2e/...`, `go vet`, lint, the em
   dash check, coverage at least 80 percent in the changed packages (the repo total must
   not regress).
5. 4 review passes (no-context, architecture, with-context, security) on the staged diff;
   findings closed.
6. AGENTS.md (contracts, architecture, trade-offs T-MOCK-08..12, journal) and the commit
   `CBL-17.27: ...`.
7. Umbrella: the global compose wiring (payout `LEDGER_ADDR`, the stand
   `PAYOUT_APPROVAL_POLICY`, the connector payout allowlist pin, the reconciler
   `RECON_PROVIDERS: btcpay`) and the GE-2..GE-5 continuation runs.

## 12. Open questions and decisions

- None blocking. The scope is deliberately BTCPay-only (T-MOCK-12); extending the other
  profiles is additive and follows the same spec sections.
