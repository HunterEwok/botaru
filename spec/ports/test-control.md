# T TestControl

Sheet version: 0.1.0.

## In short

The test-control port gives the check runner what cannot be done through P1 and P2: move the store clock, run one tick of a background loop, stop a process, inject a store fault, take a store snapshot and restore from it, submit an external event, query the permission table and call a pure core function. Without it every core implementation would invent its own way, and the same fixtures would mean different things (lead decision 4 of the 2026-10-12 review). The port exists only on a farm that was created as a test bench from its first start and that never had a tenant other than `kind: test` (APR-9); a production farm has no such port, and the core of a production farm with the port enabled does not start. The initial state is seeded through the ordinary paths: P2 `apply` of manifests and P1 and P2 operations, not through this port.

## Who calls and who implements

- **Implemented by** the core, on a separate listener (`spec/bindings/http/README.md`, path `/v1/test/<operation>`), with the test bench key from the test bench deployment settings. A worker token, a person's credentials and the `health` key are not accepted on this listener.
- **Called by** the `conformance/` check runner and only by it. A bot, a worker and a runtime do not know about the port.
- **Boundary.** The test-control port does not bypass the rules: a loop tick does the same as the loop would do on its timer, a submitted event takes the same path as an adapter's `submit_activity`. The port changes only the time, the order and environment failures.

## Operations

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| `clock.set` | runner | Sets the store clock to the instant `at`; the clock does not go back | `input_schema_invalid`, `forbidden` | v0.1 | TIM-1 |
| `clock.advance` | runner | Moves the store clock forward by `by_s` seconds; the deadlines of leases, approvals, schedules, holds and retention are computed from the new clock, but background loops do not start by themselves | `input_schema_invalid`, `forbidden` | v0.1 | TIM-1 |
| `loop.tick` | runner | One tick of a background loop by name from `background-loops.md`; with `copies: 2` two ticks at the same time, to check that the loop holds the lock and does not do the work twice. The answer comes after the end of the tick | `not_found`, `forbidden` | v0.1 | the loop rules in `background-loops.md` |
| `process.stop` | runner | Stops a test bench worker by `worker_id`, or the core: `kill` without shutdown, `graceful` as on a drain. The test bench starts the stopped process again by its own means | `not_found`, `forbidden` | v0.1 | FEN-1, FEN-2 |
| `store.fault` | runner | Injects a store fault at a closed point: `before_commit` (the next `count` transition transactions roll back entirely), `unavailable` (the store does not answer until `clear`), `low_space` (free space below the hard threshold), `clear` (remove the fault) | `input_schema_invalid`, `forbidden` | v0.1 | OBS-8, CAP-5, RET-6 |
| `store.snapshot` | runner | Takes a snapshot of the test bench store with the label `label` in the same way as the external backup process (RET-5) | `input_schema_invalid`, `forbidden` | v0.1 | RET-5, RET-6 |
| `store.restore` | runner | Restores the test bench store from the snapshot `label` by the real restore path: the store epoch grows, a restore record is written into the `farm` chain, the core answers `not_ready` until it is ready (RET-6). The answer comes when the core is ready again | `not_found`, `forbidden` | v0.1 | RET-6, AUD-6 |
| `authorize.eval` | runner | Queries the C2 permission table with one `authz-request` request and answers with `authz-decision` without a write and without side effects; for rows of the production kind of the table, the test bench passes the tenant kind in the request instead of creating a production tenant | `input_schema_invalid`, `forbidden` | v0.1 | PRM-2, APR-9 |
| `function.eval` | runner | Calls one pure core function by name with the `input` of a decision table case and answers with its `output`, without a write and without side effects: `governance.count` (GOV-8), `commission.calibration` (COM-11), `digest.sample` (DIG-5), `reporter.number_check` (REP-4), `schedule.check` (TIM-5). The core uses the same code for the function as on its ordinary path | `input_schema_invalid`, `not_found`, `forbidden` | v0.2 | APR-9, GOV-8, COM-11, DIG-5, REP-4, TIM-5 |
| `event.inject` | runner | Submits an incoming `Activity` the way the test bench channel or monitoring adapter named `via` would submit it; the core processes it by the same path as `submit_activity` of that process, with the same authenticity check by the process mark | `not_found`, `forbidden`, `submit_activity` refusals | v0.1 | IDN-3 |

Request and response schemas: the `T.*` rows of the map `spec/ports/operations.yaml`. The answer of a changing operation is `test-control-response` with the store clock after the action.

## Guarantees

- MUST: the port listener exists only if all of these hold: (1) the test bench key is set in the deployment settings; (2) the store was created as a test bench store: the flag is written at the first start of the core and never changes; (3) the store never had a tenant other than `kind: test`, including a retired one whose audit, erasure log or data within the retention period stayed in the store (APR-9). An empty store of a production farm does not become a test bench: the test bench flag is set only at the first start with the test bench key.
- MUST: on the test bench the service tenant `farm` is also `kind: test` by the definition of the test bench store.
- MUST: a core that sees the test bench key with a store without the test bench flag or with a tenant other than `kind: test` does not start and writes the reason into the startup log (`docs/quality.md`, `stand/prod-sees-eval`). The port cannot be enabled on a running farm: the test bench flag is not set after the first start.
- MUST: in a test bench store, `apply` of a production tenant or removal of `kind: test` is refused with `forbidden`.
- MUST: every action of the port writes an audit record with the subject `system` and the action name, so that a test bench run is visible in the log (AUD-1).
- MUST: the store clock only goes forward; `clock.set` into the past is refused with `input_schema_invalid`.
- MUST NOT: the port does not create or change records of tasks, decisions, subjects and manifests directly; P1 and P2 do all of that.
- Check of the `conformance/suites/core-blackbox` suite: a core with the test bench key on a store without the test bench flag does not start; a core with the test bench key on a test bench store with `farm`, `demo` and `demo2` (`kind: test`) answers `clock.advance` with `200`; `apply` of the production tenant `farm` in the test bench store gets `403 forbidden`.

## Bindings

- Transport: `spec/bindings/http/README.md`, path `/v1/test/<operation>`, a separate listener, the test bench key in `Authorization: Bearer`.
- Schemas: `spec/model/test-clock-request.schema.json`, `test-loop-tick-request`, `test-process-stop-request`, `test-store-fault-request`, `test-event-inject-request`, `test-store-snapshot-request`, `test-store-restore-request`, `test-control-response`; `authorize.eval` has the request `test-authorize-eval-request` and the response `authz-decision`; `function.eval` has `test-function-eval-request` and `test-function-eval-response`.
- Checks: the `conformance/suites/` suites that refer to test bench actions.

## What the port does not do

- It does not seed data: the initial state of a fixture is set by manifests and by P1 and P2 calls. `authorize.eval` and `function.eval` take their input in the request and keep nothing.
- It does not replace the model and the provider: for that there is the mock provider `spec/bindings/model-providers/mock.md` behind P4.
- It does not work on a production farm in any mode: there is no flag "open the port on production".
