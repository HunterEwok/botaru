# T TestControl

How the core is built for its checks, not a contract with foreign code (C10, D-114).

## In short

The test-control port gives the check runner what P1 and P2 cannot: move the store clock, run one tick of a background loop, stop a process, inject a store fault, take a store snapshot and restore from it, submit an external event, query the permission table and call a pure core function. With one way to do these, the same fixtures mean the same thing on every core. The port exists only on a test bench (APR-9); the initial state of a fixture is seeded through the ordinary paths, P2 `apply` and P1 and P2 calls.

## How it works

- The core serves the port on a separate listener (`spec/bindings/http/README.md`, path `/v1/test/<operation>`) with the test bench key from the deployment settings; a worker token, a person's credentials and the `health` key are not accepted there. Only the `conformance/` runner calls it.
- Operations: `clock.set` and `clock.advance` (the store clock only goes forward, deadlines follow it, loops do not start by themselves; TIM-1); `loop.tick` (one tick of a loop of `background-loops.md`, with `copies: 2` two at the same time to check the lock); `process.stop` (a worker or the core, `kill` or `graceful`); `store.fault` (`before_commit`, `unavailable`, `low_space`, `clear`; OBS-8, CAP-5); `store.snapshot` and `store.restore` (the real backup and restore path, the epoch grows; RET-5, RET-6); `authorize.eval` (one row of the permission table without a write; PRM-2); `function.eval` (one pure core function by name: `commission.accuracy`, `digest.sample`, `reporter.number_check`, `schedule.check`; COM-17, DIG-5, REP-4, TIM-5); `event.inject` (an incoming `Activity` by the same path as `submit_activity`; IDN-3).
- The port does not bypass the rules: a tick does what the loop would do on its timer, an injected event takes the ordinary path. It changes only time, order and environment failures, and it creates no tasks, decisions, subjects or manifests directly. Every action writes an audit record with the subject `system` (AUD-1).

## When the port exists

- Only if (1) the test bench key is set, (2) the store was created as a test bench store at the first start of the core, a flag that never changes, and (3) the store never had a tenant other than `kind: test`, including a retired one whose data is still in the store (APR-9). The service tenant `farm` of a test bench is `kind: test` too.
- A core that sees the test bench key with any other store does not start and writes the reason into its startup log (`stand/prod-sees-eval` in `docs/quality.md`); `apply` of a production tenant in a test bench store gets `forbidden`.

## Where the details are

The rows `T.*` of `spec/ports/operations.yaml`. The request schemas that fixtures check through their stand steps are core test schemas, not a port contract: `spec/model/test-clock-request.schema.json`, `test-loop-tick-request`, `test-process-stop-request`, `test-store-snapshot-request`, `test-store-restore-request`, `test-function-eval-request`; `authorize.eval` takes the `authz-request` of a table row. The stand table of `conformance/runner.md` says how the runner builds the bodies.
