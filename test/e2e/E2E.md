# E2E cbl-mock-gateway: provider API and signed webhooks on the compose stand

Run instruction for the local e2e scenario of the service. A run is performed by an
agent platform-independently: docker and a command shell. There are no PowerShell run
scripts (customer decision of 16.10.2026, `CBL-002` section 7).

Repository: `cbl-mock-gateway`. Stand: `deploy/docker-compose.yml` (mock-gateway in the
`app` profile). The stand smoke exercises the provider API (create, idempotent replay,
read) and the signed webhook simulation; the webhook contract end-to-end scenario runs
separately on the `e2e` tag (`make test-e2e`, no Docker).

## 1. Scenarios

| Scenario | Checks | Contracts |
| :-- | :-- | :-- |
| `E2E_MockGateway_Smoke` | compose stand up, healthz, invoice creation returns a gateway_id, the same Idempotency-Key replays to the same gateway_id, the invoice reads back as pending, simulate with duplicate=2 returns an event_id and two deliveries (each either accepted or a recorded network error with http_status 0), the webhook log has at least two entries | provider REST contract, webhook contract equal to the CBL-3.5 sandbox provider (T-MOCK-03) |

## 2. Isolation and parallelism

- Run mark: UTC lowercase `yyyymmddthhmmssz` (uppercase is rejected by compose project
  naming rules).
- Artifact directory: `test/e2e/.runs/<mark>/` (create before the start).
- Compose project: `cbl-mock-gateway-e2e-<mark>` (the compose file declares
  `name: cbl-mock-gateway`; the `-p` flag overrides it).
- Host ports 8120 and 9320 are published; do not run two mock-gateway stands in parallel
  and stop other stands first.
- In the smoke the webhook target (Webhook Ingress) may be unavailable: delivery must be
  recorded either as a success or as a network error (`http_status` 0); both are PASS.
- All commands run from the repository root.

## 3. Common run order

1. `<MARK>` = current UTC time in lowercase; create `test/e2e/.runs/<MARK>/`.
2. Stop the previous stand: `docker compose -p cbl-mock-gateway-e2e-<MARK> -f deploy/docker-compose.yml --profile app down -v`
   (output to `compose-down.log`; the first run has nothing to stop).
3. Start the stand: `docker compose -p cbl-mock-gateway-e2e-<MARK> -f deploy/docker-compose.yml --profile app up -d --build --wait`
   (`compose-up.log`); `docker compose ps` into `compose-ps.txt`.
4. Wait for `GET http://localhost:8120/healthz` = 200 (up to 60 s, 2 s interval;
   `curl.exe -s -o NUL -w "%{http_code}"`; redirect native output to files).
5. Run the scenario checks (section 4), recording every OK/FAIL line in
   `smoke-checks.txt` and raw responses in `smoke.log`.
6. Capture logs: `docker compose -p ... logs --no-color > compose-logs.txt`.
7. `summary.md` by the `.additionals/summary.template.md` template; stop the stand:
   `down -v` (append to `compose-down.log`).

Utility notes: POST bodies should be sent from files (`curl.exe -d @<file> -H "Content-Type: application/json"`)
to avoid shell quoting problems; parse JSON with the shell's JSON features and record the
raw body in the log; note the test invoice idempotency key `smoke-1` is stand-scoped, a
restarted stand forgets it (in-memory state, T-MOCK-01).

## 4. E2E_MockGateway_Smoke

1. Common run order, steps 1-4.
2. Create an invoice: `POST /api/v1/invoices` with header `Idempotency-Key: smoke-1` and
   body `{ "payment_id": "<fresh UUID>", "amount": { "minor_units": 100000, "asset": "USDT", "decimals": 2 }, "network": "trc20", "expires_at": "<now+15m RFC3339 UTC>" }`;
   the response must carry a non-empty `gateway_id`.
3. Idempotent replay: repeat the identical request with the same header; the response
   `gateway_id` must equal the first one.
4. Read back: `GET /api/v1/invoices/{gateway_id}` returns `status` = `pending`.
5. Simulate webhooks: `POST /api/v1/simulate` with
   `{ "gateway_id": "<gateway_id>", "status": "confirmed", "duplicate": 2 }`; the
   response must carry a non-empty `event_id` and `deliveries` with exactly 2 entries;
   for each entry `http_status` is 0 (recorded network error) or in the 2xx range.
6. Webhook log: `GET /api/v1/webhooks` returns at least 2 entries.
7. Checks go to `smoke-checks.txt`.

## 5. Mandatory run artifacts

- `summary.md` (template `.additionals/summary.template.md`; PASS/FAIL verdict, steps,
  key values, artifact list, time);
- `compose-ps.txt`, `compose-up.log`, `compose-down.log`, `compose-logs.txt`;
- `smoke.log` (raw HTTP transcripts) and `smoke-checks.txt`;
- on failure: container states, the mock-gateway log fragments, the failing request and
  its raw response.

## 6. Auxiliary files (.additionals)

- `summary.template.md` - the `summary.md` template;
- `inventory.md` - the list of auxiliary files and their purpose.

## 7. Cleanup

`docker compose -p cbl-mock-gateway-e2e-<MARK> -f deploy/docker-compose.yml --profile app down -v`
at the end; `.runs/` is not committed; a repeated run must start from a clean stand.
