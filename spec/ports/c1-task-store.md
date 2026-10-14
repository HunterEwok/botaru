# C1 TaskStore

Sheet version: 0.1.0.

## In short

C1 is the farm store with atomic transitions: tasks and the queue, leases, tool calls, approvals, deliveries, events, usage, audit, holds, files, erasure requests, message inboxes and proposals. Each transition is written in one transaction together with its event, the deliveries it produced and the usage lines. The C1 task queue is the farm pool: the dispatcher decides what `claim` returns, and the store guarantees that exactly one holder holds a work unit. The reference implementation is PostgreSQL (D-03); the SQL binding grows together with the reference and is not part of the sheet. The store clock is the master clock for all deadlines.

## Who calls and who implements

- **Called by** the core only: the protocols P1-P4, the background loops, `farmctl` through P2.
- **Implemented by** the store adapter inside the core. Only core processes have the credentials and the network path to C1: pipelines, channels, runtimes and plugins talk only through ports.

## Operations

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| `enqueue` | core (P1, P2, `schedule` loops, routes) | Creates a task with a record written by the core; budget reserve; check of the idempotency key and tombstones | `idempotency_conflict`, `gone`, `rejected{reason}` | v0.1 | IDM-1, IDM-2, RET-3, BUD-1, TEN-1 |
| `claim` | core (P1) | Chooses work by the dispatcher's filter and order and creates a lease with `lease_no` one higher and the store epoch; for a personal assistant task writes `owner` into the lease | empty | v0.1 | FEN-1, DSP-1, DSP-2, DSP-10, TEN-3 |
| `heartbeat`, `complete`, `fail`, `release`, `cancel` | core (P1, P2) | Lease and task transitions by token; a stale token changes nothing | `fenced` | v0.1 | FEN-2, IDM-3, DLV-2 |
| `lease.extend` | core on leaving `not_ready` | Under a lock extends each lease that was `active` at the start of the core downtime window to "ready moment plus lease period"; a second core copy changes nothing with the same operation if the lease is already extended to a later deadline; a lease that the holder did not renew even after that ends with `core_unavailable` without spending the attempt | | v0.1 | FEN-5, OBS-8 |
| `reap` | `reaper` loop | Expired leases: attempt `lease_expired`, read call `error(lease_expired)`, write call `exec_unknown`; a lease that expired while the core was not running does not spend the attempt | | v0.1 | FEN-3, APR-7 |
| `approval.create`, `approval.decide` | core (built-in tool `request_action`, P1 `decide`, `/run`) | An approval record with a snapshot of the arguments and a `context` line from closed fields; a one-time decision transition; on approval, in the same transaction `approved -> executing`, the task `action.<action>` goes straight to `working` and one write call; an initiator copy of the card and a reminder at half the period (APR-19) | `already_decided`, `expired` | v0.1 | APR-4, APR-7, APR-19, CNT-7 |
| `service_grant` | core (P3 `service/control` and `monitor/script_run`, P2 `service.control`, `/run` of a `routine` class action) | A permission audit record without a card: the bot or the human author, role, task, service and verb or host and script, reason code; the number is in the write call as the `grant_id` field, one execution per number. Without a card only `restart` from `services[].no_card` and scripts from the role's list (D-74); a `routine` class action (writes only into its own directories, idempotent) has the initiator as author and the limits `runs_per_hour`, `runs_per_day` (D-78) | `service_rate_limited` | v0.1 | APR-17, APR-18, MON-8, MON-10 |
| `meter.reserve`, `meter.commit`, `meter.release` | core (P4, P3, `enqueue`) | Reserve, charge and release by all applicable limits | `budget_exhausted` | v0.1 | BUD-1, BUD-2, BUD-3 |
| `event.append`, `event.read` | core | The CloudEvents event log; the core sets `source` and `id` | | v0.1 | IDM-4, AUD-2 |
| `audit.append` | core | An audit record, append-only | | v0.1 | AUD-1, AUD-3 |
| `msg.stage`, `msg.publish`, `msg.lease`, `msg.ack`, `msg.nack`, `msg.expire`, `state.put` | core (P1, P3, `inbox` loop) | Messages and inboxes: staging in an attempt, publishing in the `complete` transaction, entry lease, acknowledgement, deadline | `inbox_full`, `sender_quota` | v0.1 | MSG-9, MSG-15, MSG-12 |
| `hold.set`, `hold.release` | core (P2); `farmctl hold farm` on the core machine directly when the core process is not running | A hold of a scope with effects and a deadline. A hold of the `farm` and `seat` scopes is a record of the service tenant `farm`, a hold of another scope is a record of the tenant of that scope. A hold ends by `until` or is removed by a person with `hold.release`; the core does not remove a hold by itself, the holder of the farm manager seat removes only its own holds | | v0.1 | HLD-1, HLD-2, HLD-3, TEN-1 |
| proposal and review transitions; change trial record | core (P1 `complete` and `decide`, `proposal` loop, P2 `proposal.link_commit`) | Creating a proposal on `complete`, lens reviews, the verdict by code, the people's decision by card buttons (P1 `decide`, subject `proposal`), the trial record on "Accept", the reverse edit on `realized`; from v0.2 the monthly audit sample by the recorded seed with the `inclusion_prob` of each item | | v0.1 | PRP-1, PRP-4, PRP-20, PRP-21, COM-4 |
| rollups of observation series | `observe` loop | Window, function from a closed list, class not below the series | | v0.1 | OBV-4 |
| `file.put`, `file.get`, `file.stat` | core (P1) | The core writes the file record, bytes only by reference. A file in `purged` gives `gone`; from v0.2 also `expired` by its period | `not_found`, `gone` | v0.1 | ART-1, ART-3 |
| `file.purge` | `erasure` loop on an erasure request; `retention` loop by `expires_at` | Deletes file bytes by the scope `(tenant)`, `(tenant, subject)` or by a list of ids together with the fragments and embeddings of corpora, the file record moves to `purged` and remains as a tombstone; the result `done`, `failed` or `not_applicable` goes into the erasure request. Deletion on a person's request is part of v0.1 (D-76); deletion by the `expires_at` period and corpus fragments from v0.2 | | v0.1 | RET-4, RET-7, RET-2, KNW-5 |
| `task.purge` | `erasure` loop on an erasure request | Deletes task content by the scope `(tenant, subject)` (tasks where the subject is the initiator or is in `personal_subjects`) or by a list of ids: `body`, `result`, the bodies of deliveries and events with content. A tombstone `(tenant, idempotency key hash, id, state, times)` remains, so a repeated key gives `gone`, not a new task (RET-3). The result `done` or `not_applicable` with `basis` goes into the erasure request | `store_unavailable` | v0.1 | RET-3, RET-4, RET-7, SEC-11 |
| `erasure.*` | core (P2, `erasure` and `identity` loops) | An erasure request: the scope `(tenant)`, `(tenant, subject)` or a list of object ids, a reason from a closed list and a result for each store; the request is written to the erasure log before the first byte is deleted, the log is backed up separately | | v0.1 | RET-7, RET-5 |
| `epoch.advance` | core on restore | Growth of the store epoch; pending approvals (`pending`) move to `canceled(restored)`; write calls in `queued` and `leased` and their approvals in `executing` move to `exec_unknown`; the erasure log is replayed | | v0.1 | RET-6 |
| `campaign.create`, `campaign.release`, `campaign.item_close`, `campaign.control` | core | Atomic transitions of a campaign and its items with the event and deliveries in one transaction; each item is executed at most once | | v0.1 | ORC-1, ORC-3, ORC-4, ORC-8, ORC-11, DLV-2 |
| `script.create`, `script.release_step`, `script.step_close`, `script.control` | core | A script run and its steps: the run record with steps in `pending` and a `script_confirm` delivery; release of a step as a task with the key `system:script:<run>:<step>`, a campaign or an approval; closing a step and a run; the transitions `running`, `paused`, `canceled`, `rejected`. Each operation in one transaction with its event | | v0.1 | SCR-5, SCR-8, SCR-10, SCR-11, DLV-2 |
| `seat.assign`, `seat.vacate`, `seat.resume`, seat hold | core (P2, incident) | Seat holder, epoch + 1, the stop reason set | | v0.1 | IDN-10, MGR-1, GOV-22 |
| `control.apply` | core (P3 of the manager, P2 of people) | A control operation from a closed list: audit, event and a notice to people in one transaction | `fenced`, `held`, `target_not_implicated`, `target_excluded`, `protected_target`, `control_rate_limited` | v0.1 | MGR-1, MGR-9, MGR-13, MGR-14, MGR-32 |
| `incident.*` | core (`incident` loop, P2) | Open by alarm and class, `resolved` by code, close or declare false by a person (`dismissed` with a reason code); from v0.2 declaring an incident false moves its attributions to changes with another person's "Yes" answer to `disputed` instead of canceling them (D-79) | | v0.1 | MGR-8, GOV-13 |
| `bot.create`, `bot.bind`, `bot.retire` | core | A bot identity, binding to a worker or a seat with an epoch, retirement by a trigger from a closed list | | v0.1 | IDN-8, IDN-11 |
| `case.open`, `case.advance`, `case.close`; ballot record | core (`governance` loop) | A governance case; the ballot value is taken from the P4 answer of this attempt | | v0.2 | GOV-7, GOV-12 |
| `attribution.create`, `attribution.decide`; grants; escalations | core | Incident attribution (states `pending`, `yes`, `no`, `disputed`, `canceled`; a dispute is resolved by the answer of the author of "Yes" or by silence for 7 days), model grant, escalation | | v0.2 | GOV-13, MGR-5, MGR-18 |

## Guarantees

- MUST: a transition, its event, the deliveries, messages and usage it produced are written in one transaction (DLV-2).
- MUST: exactly one holder holds a work unit; the token is `(epoch, work_id, lease_no, worker_id)`; acceptance: 10 000 tasks on 8 competitors give 0 duplicates and 0 losses (FEN-1).
- MUST: `(tenant, idempotency_key)` is unique; after a task is deleted a tombstone remains, and a repeated key gives `gone`, not a new task; ids are not reused (IDM-1, RET-3).
- MUST: the audit and the transition log are append-only; they are deleted only in whole periods by their retention period (AUD-3).
- MUST: the audit, events, transition log and usage hold only metadata; content lives only in deletable stores with a period (AUD-2).
- MUST: each record belongs to exactly one tenant (TEN-1).
- MUST: the store clock is the master clock; times in records are UTC (TIM-1).
- MUST: only core processes have C1 credentials. `farmctl` on the core machine with the core credentials counts as part of the core: this way the emergency hold `farm` is written directly into C1 and works without the core process (HLD-1).
- SHOULD: at start the core itself notices that the external erasure log is ahead of the store and does not leave `not_ready` until reconciliation.

## Bindings

- SQL binding of the reference: signatures of store functions and their tests; grows together with the reference implementation.
- Record schemas: `spec/model/task-record.schema.json`, `approval`, `delivery`, `spend`, `audit-record`, `message`, `file-record`, `campaign-item` (campaign item), `inbox-entry` (inbox entry), `observation-point` (observation series point), `report-digest` (digest delivery), `review-input` (review task data); retention classes `x-botaru-retention` in each schema.
- Checks: the `conformance/suites/store` suite, fixtures `state-machine/`, `lease/`, `approval/`, `delivery/`, `restore/`.

## What the port does not do

- It does not make permission decisions: C2 computes them in the same transaction.
- It does not call the model and does not push work: work is taken through P1.
- It does not store secrets and key values: program identities are stored as `api:<key_id>`, tokens as hashes.
