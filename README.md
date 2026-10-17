# cbl-mock-gateway

Mock provider for tests and stands: emulation of the gateway API and signed webhooks.

Service role and contracts: `AGENTS.md`. Global platform rules: `../../AGENTS.md`,
`../../.specs/CBL-000-system-description.md`, `../../.specs/CBL-003-contracts.md`.

## Provider profiles

Since CBL-17.5 the stand emulates five providers: the platform sandbox (`mock`) plus
NOWPayments, CoinGate, BitPay and BTCPay Server. Adapter and e2e tests reach any profile
without network access:

```text
POST /v1/invoice                             # nowpayments (x-api-key)
GET  /v1/payment/{id}
POST /v2/orders                              # coingate (Authorization: Bearer)
GET  /v2/orders/{id}
POST /invoices                               # bitpay (X-Identity + X-Signature)
GET  /invoices/{id}
POST /api/v1/stores/{store}/invoices         # btcpay (Authorization: token)
GET  /api/v1/stores/{store}/invoices/{id}
```

`POST /api/v1/simulate` carries `provider` and an optional `raw_status`; the scenario
endpoint injects failures (`fail_status`, `retry_after_s`). Webhook bodies and signatures
follow each provider scheme (canonical JSON HMAC-SHA512, token echo, raw HMAC base64, raw
HMAC hex with the `sha256=` prefix). Configure targets with `WEBHOOK_TARGET_URL_<PROVIDER>`
or rely on the derived `/hooks/<provider>/` path of `WEBHOOK_TARGET_URL`.

## Commands

```powershell
make build        # build
make test         # mocked unit tests
make vet          # go vet
make lint         # golangci-lint in Docker
make up           # dependencies + service in docker-compose
make down
```
