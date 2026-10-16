# CBL-16.10. cbl-mock-gateway: English comments and documentation; e2e via E2E.md

| Field | Value |
| :-- | :-- |
| Epic | E16, `Convention alignments` |
| Repository | `cbl-mock-gateway` |
| Size | S |
| Wave and parallel group | W0-CORR |
| Depends on | `CBL-16.1` (convention), `CBL-16.2` (pilot pattern) |
| Contract version | not applicable (no contract changes) |
| Status | done, review closed |

## 1. Context and goal

Two cross-cutting customer corrections (16.10.2026): (1) comments and documentation are
written in English in all files (code, tests, specs, AGENTS.md, README, configurations;
log and error messages stay unchanged as executable contracts); (2) e2e runs are not
PowerShell scripts but an agent instruction `test/e2e/E2E.md` with run artifacts in
`test/e2e/.runs/`. The convention is `CBL-002` section 7.

## 2. Scope

In scope: English translation of all comments, docstrings, the spec (CBL-3.6), AGENTS.md
and README.md; `test/e2e/E2E.md` (scenario `E2E_MockGateway_Smoke`: stand up, healthz,
invoice create/replay/read, simulate with duplicate=2 and the delivery acceptance rule,
the webhook log); removal of `test/e2e/smoke.ps1` and the `make smoke` target;
Makefile/.gitignore updates.

Out of scope: changing behavior and runtime messages (problem+json `detail` texts, zap
messages quoted in specs), the tag-e2e webhook contract scenario (`make test-e2e`), other
repositories (16.11 - 16.16).

## 3. Requirements

FR-1. Language: no Russian comments or docstrings remain; Russian string literals remain
only as runtime problem+json details; test failure descriptions are English; godoc
docstrings keep the entity-name first line and end with a period; AGENTS.md development
rules state the English documentation rule.

FR-2. E2E: `test/e2e/E2E.md` contains preconditions, isolation (compose project per
lowercase UTC mark, fixed host ports), the run order, the smoke check list (healthz,
create + idempotent replay + read pending, simulate event_id/two deliveries with the
0-or-2xx rule, webhook log >= 2), mandatory artifacts and cleanup; there are no
PowerShell run scripts; AGENTS.md references the instruction.

FR-3. Auxiliary files live in `test/e2e/.additionals/` (summary template, inventory);
`.runs/` is gitignored.

FR-4. Validation run: the scenario executed per `E2E.md`, artifacts in
`test/e2e/.runs/<mark>/`, `summary.md` filled with a PASS verdict.

## 4. Plan

Comment and documentation translation (parallel by file groups: cmd/domain/usecase/
webhook/transport, spec, AGENTS.md/README/e2e test) -> E2E.md and `.additionals` ->
removal of the ps1 script and the smoke target -> validation run -> 4 reviews -> commit
`CBL-16.10: English comments and docs; e2e via E2E.md`.

## 5. Reviews and runs

Verified: unit tests green (no production code changes), lint 0, the smoke scenario run
per `E2E.md` with run artifacts, em-dash check clean, comment language verified by a
Cyrillic scan (remaining Cyrillic: problem+json detail literals in
`internal/transport/server.go`; none are comments).
