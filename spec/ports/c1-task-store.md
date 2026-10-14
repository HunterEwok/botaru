# C1 TaskStore

How the core is built, not a contract: only the core touches the store, so this page describes the construction and has no request and response tables (C10, D-114).

## In short

C1 is the farm store: tasks and the queue, leases, tool calls, approvals, deliveries, events, usage, audit, holds, files, erasure requests, messages, proposals, campaigns, scripts, seats, incidents and bot identities. A transition is written in one transaction together with its event, the deliveries and messages it produced, its continuations and its usage lines (DLV-2). The queue is the farm pool: the dispatcher decides what `claim` returns, and the store makes sure that exactly one holder holds a work unit (FEN-1). The reference is PostgreSQL (D-03); the store clock is the master clock for every deadline (TIM-1).

## How it works

- **Only the core** has the credentials and the network path to C1. Workers, channels, runtimes and plugins talk only through P1, P3 and P4; `farmctl` on the core machine with the core credentials counts as part of the core, so the emergency hold `farm` is written into C1 even when the core process is not running (HLD-1).
- **Tasks and leases.** `enqueue` writes the task record, the budget reserve and the idempotency key `(tenant, idempotency_key)`; a repeat with the same initiator and body returns the task, another body gets `idempotency_conflict`, and after a deletion a tombstone keeps the key, so a repeat gets `gone` (IDM-1, RET-3). `claim` creates a lease with `lease_no` one higher and the store epoch. `heartbeat`, `complete`, `fail`, `release` and `cancel` change a record only with the current token; a stale token gets `fenced` (FEN-2). `lease.extend` gives every lease that was active when the core went down a new term once the core is ready again, once even with two core copies (FEN-5); `reap` ends expired leases (FEN-3, APR-7).
- **Approvals and grants.** `approval.create` and `approval.decide` keep a snapshot of the arguments and decide once (APR-4, APR-19); `service_grant` writes the permission record of a service or script call without a card (APR-18).
- **Money.** `meter.reserve`, `meter.commit` and `meter.release` reserve, charge and release by every applicable limit (BUD-1..3).
- **Events and audit.** `event.append` and `event.read` hold the CloudEvents log, the core sets `source` and `id` (IDM-4); `audit.append` writes the audit, which is append-only and holds only metadata (AUD-2, AUD-3).
- **Messages.** `msg.stage` stages a message inside an attempt, `msg.publish` publishes it in the `complete` transaction, and `msg.lease`, `msg.ack`, `msg.nack`, `msg.expire` and `state.put` serve the inbox (MSG-9, MSG-12).
- **Files and erasure.** `file.put`, `file.get` and `file.stat` keep file records, bytes only by reference (ART-1). `file.purge` and `task.purge` delete content by the scope `(tenant)`, `(tenant, subject)` or a list of ids and leave a tombstone; `erasure.*` keeps erasure requests, written to the erasure log before the first byte is deleted (RET-4, RET-7). `epoch.advance` grows the store epoch on restore and replays the erasure log (RET-6).
- **Holds, campaigns, scripts, the manager.** `hold.set` and `hold.release` (HLD-2); `campaign.create`, `campaign.release`, `campaign.item_close`, `campaign.control` (ORC-1); `script.create`, `script.release_step`, `script.step_close`, `script.control` (SCR-8); `seat.assign`, `seat.vacate`, `seat.resume`; `control.apply` (MGR-9); `incident.*`; `bot.create`, `bot.bind`, `bot.retire` (IDN-11); from v0.2 `case.open`, `case.advance`, `case.close`, `attribution.create` and `attribution.decide` (GOV-12, GOV-13). Each is one transaction with its event.

## What it does not do

- It makes no permission decision: C2 computes it in the same transaction.
- It does not call the model and does not push work: work is taken through P1.
- It keeps no secrets: program identities are `api:<key_id>`, tokens are hashes.

## Where the details are

Record schemas in `spec/model/` (`task-record`, `approval`, `delivery`, `spend`, `audit-record`, `message`, `file-record`), each with its retention class in `x-botaru-retention` (RET-1); lifecycles in `spec/lifecycles/`; the rows `C1.*` of `spec/ports/operations.yaml`. Behaviour is checked through the core: rows of the `core-blackbox` suite with the test-control port, fixtures `state-machine/`, `lease/`, `approval/`, `delivery/`, `restore/`, and Go tests of the reference store.
