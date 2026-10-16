# AGENTS.md - cbl-mock-gateway

Mock provider for tests and stands: emulation of the gateway API and signed webhooks.

Mandatory global rules: `../../AGENTS.md`, `../../.specs/CBL-001-development-process.md`,
`../../.specs/CBL-002-repos-and-docker.md`, contracts `../../.specs/CBL-003-contracts.md`.
The em dash (U+2014) is forbidden, only the hyphen is used.

## 1. Boundaries

In scope: invoice creation, statuses, signed webhooks, failure scenarios.

Out of scope: other platform services; interaction only through `cbl-protos` contracts
(gRPC), Kafka and the perimeter REST (see `CBL-000`, section 2).

## 2. Contracts

- REST: `POST /api/v1/invoices`, `GET /api/v1/invoices/{gateway_id}` - emulation of the
  provider API; `POST /api/v1/simulate` - sending signed webhooks; `GET /api/v1/webhooks`
  - delivery log; `POST|GET /api/v1/scenarios`, `POST /api/v1/scenarios/reset` - failure
  scenarios; webhooks go to Webhook Ingress under the sandbox provider contract (CBL-3.5)
- gRPC: none
- Kafka produces: none
- Kafka consumes: none
- Storage: in-memory (invoices, scenarios, log of the last 100 deliveries)

Contracts are frozen in `cbl-protos` (CF-1 for gRPC, CF-2/CF-3 for events), see `CBL-004`.

## 3. Architecture

Layers: `internal/transport` -> `internal/usecase` -> `internal/domain`; sending signed
webhooks - the `internal/webhook` adapter through the `usecase.WebhookSender` port.
`internal/storage` is not used: the stand state is in-memory (T-MOCK-01).
Libraries: Gin, zap, cbl-kit (hmac, ginmw, money), google/uuid; GORM, franz-go, go-redis,
goose and PostgreSQL are not needed by the mock service - the deviation from the CBL-002
set is recorded by trade-off T-MOCK-01.

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

## 6. Development rules

- Pipeline: SDD spec in `.specs/` -> architecture -> contracts -> tests first -> code ->
  review by 4 agents (no-context, architecture, with-context, security - blocking) ->
  AGENTS.md -> final runs -> linters -> commit.
- Unit+integration coverage is at least 80%; money scenarios only on real PostgreSQL and
  Kafka (testcontainers); local e2e in docker-compose is mandatory.
- Money is int64 in minor units plus decimals only; float is forbidden.
- Godoc docstrings are mandatory in English: the first line starts with the entity name and ends with a period; comments explain why; errors are wrapped with `%w`; panics in business code are forbidden.
- Commit: `CBL-<epic>.<number>: <short message>`, the spec with the same prefix in the same commit.

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

## 11. Runbook

Mock-gateway stand diagnostics:

1. `GET /api/v1/scenarios`: whether `fail_next`/`latency_ms` are enabled; reset - `/scenarios/reset`.
2. `GET /api/v1/webhooks`: the latest deliveries with `http_status` and `error`; code 0 - a
   network failure (check `WEBHOOK_TARGET_URL` and Webhook Ingress), 401 - the signature was
   rejected (`invalid`/`missing` mode or the secret does not match the Ingress provider).
3. Invoice not found - the in-memory state is lost on restart, recreate the invoice.
4. The secret and signature headers must match the Webhook Ingress configuration.
