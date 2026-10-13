# Lifecycle: lease

## In short

A lease (`Lease`) is the right of one holder to work on one unit of work until a deadline. One mechanism serves four kinds of work that P1 `claim` hands out: a task attempt (`attempt`), a tool call (`toolcall`), a delivery (`delivery`), and a step of an erasure or search request at an edge store (`erasure`, schema `spec/model/erasure-work.schema.json`). Every `claim` creates a new lease with a number `lease_no` one greater than the previous one and a fencing token `(epoch, work_id, lease_no, worker_id)`: the number in the token grows with every claim, and the store rejects a changing call with an old number (FEN-1, FEN-2). `lease_no` is not a counter of spent attempts: spent attempts are counted by `attempts_spent`, runtime runs with a model answer by `runs_counted` (the table in `docs/tasks-and-delivery.md`); an `action.<a>` task has no lease (CNT-7). The holder extends the lease with `heartbeat` and counts its deadline by its own monotonic clock from `lease_remaining_s` (TIM-1); if it did not extend in time, it stops the work itself and writes nothing more (FEN-6). Fencing protects only writes to the farm store; writes to the outside world are guarded by APR-7 and the work id in the external record (FEN-7).

## States

| State | Meaning | Terminal |
|---|---|---|
| `active` | The holder works; the token is current; the attempt handles (SEC-3) are valid | no |
| `closed` | The holder submitted the outcome `complete` or `fail` with the current token | yes |
| `released` | The holder returned the work with `release` (worker stop); the attempt is not spent | yes |
| `expired` | The `lease_until` deadline passed without an extension; the kind of the unit of work decides its outcome | yes |
| `revoked` | The core revoked the lease: a cancellation, worker revocation, revoked rights, a hold with the effect `cancel_running`, a change of the seat holder, an increment of the store epoch. The handles are invalid; a `heartbeat` with this token answers `cancel_requested`, any other changing call gets `fenced` | yes |
| `preempted` | Only an attempt: the core preempted it with the control operation `preempt`; the attempt is not spent, the task returns to the queue | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `active` | P1 `claim` with the kinds `attempt`, `toolcall`, `delivery` or `erasure` | the work passed the dispatcher filter and the holds; a tool call is handed only to a toolhost whose data recorded at `enroll` matched the call's `target`, a delivery only to the worker registered for the delivery's `target`; `lease_no` is one greater than the previous one; `lease_until` is by the store clock; the answer carries `lease_remaining_s` | FEN-1, TEN-3, TEN-3a, DLV-6, DSP-1, HLD-1, TIM-1 |
| `active` | `active` | P1 `heartbeat` with the current token | extends the deadline no further than the ceiling; carries a progress stage from the type's `enum` and a completion share; the answer carries `lease_remaining_s` | FEN-2, FEN-3, DLV-11 |
| `active` | `active` | C1 `lease.extend` when the core leaves `not_ready` | the lease was `active` at the start of the downtime window (from the last successful tick of the background loop until readiness); one operation under a lock extends the deadline to "the moment of readiness plus the lease period"; a second copy of the core changes nothing if the deadline is already later | FEN-5, OBS-8 |
| `active` | `closed` | P1 `complete` or P1 `fail` with the current token | the outcome is accepted; a repeat of the same call gives the same answer | FEN-2, IDM-3 |
| `active` | `released` | P1 `release` | the holder lets the work go when stopping; the reason `worker_drain`, the attempt is not spent | FEN-2 |
| `active` | `expired` | the `reaper` loop | `lease_until` has passed. An attempt: `lease_expired`, `attempts_spent` one greater. A read call: `error(lease_expired)`, the model sees the error. A write call: `exec_unknown`, no retry. A delivery: back to `planned` with a pause | FEN-1, FEN-3, DLV-1, APR-7 |
| `active` | `expired` | the `reaper` loop after a downtime window | a lease extended by `lease.extend` at core readiness is still not extended by the holder: the reason `core_unavailable`, the attempt is not spent and does not count toward `runs_counted`, only `lease_no` grows | FEN-1, FEN-5, GW-13 |
| `active` | `revoked` | P1 `cancel`; P2 `task.cancel`; cancellation of the parent | the task is canceled; the holder learns it in the `heartbeat` answer and stops the work before the next one | FEN-4 |
| `active` | `revoked` | P2 `worker.revoke` | the worker's leases and handles are revoked in the same transaction; the work returns to the queue without spending an attempt | FEN-2, IDN-4 |
| `active` | `revoked` | the core in the transaction of the cause: P2 `principal.off`, P2 `ungrant` | the initiator is disabled or lost the right; the task is `canceled` | PRM-3 |
| `active` | `revoked` | P2 `hold.set` with the effect `cancel_running` | the hold's scope covers the work; the attempt ends with the reason `held` without spending | HLD-3 |
| `active` | `revoked` | P2 `seat.assign`, P2 `seat.vacate`, a seat hold | an attempt of a seat task of the previous epoch; the reason `seat_changed`, without spending | IDN-10, MGR-1 |
| `active` | `revoked` | C1 `epoch.advance` on restore from backup | the store epoch grew; tokens and handles of the previous epoch get `fenced`; the attempt is not spent; write calls in `queued` and `leased` become `exec_unknown` | RET-6, SEC-3 |
| `active` | `preempted` | the control operation `preempt` | only an attempt; at most 2 preemptions per task, the third gives `canceled(preempt_limit)` | MGR-11 |
| `active` | `released` | the worker's `drain_deadline` in `draining` | v0.2; the worker lets unfinished work go, the attempt is not spent | LCY-4 |
| any except `active` | (no transition) | a changing call with an old token, with a token of the previous store epoch or with the previous seat epoch | the answer `fenced`, not a single write | FEN-2 |

## Invariants

- Exactly one holder holds a unit of work; `lease_no` only grows, and `work_id` is common to all leases of one unit of work; one attempt lease corresponds to one runtime run with the identifier `<task id>:<lease_no>` (FEN-1).
- The lease period is at most 120 s, the `heartbeat` interval at most 30 s; a task type can only decrease them (FEN-3).
- Deadlines are counted by the store clock; the holder gets the remainder `lease_remaining_s`, and the absolute time in the answer is for reference (TIM-1).
- A holder outside the core stops the work by its own clock a margin (10 s by default) before the end of the lease and after that writes nothing, neither to the farm nor to the outside world, except `fail` or `release`; the toolhost sends the plugin an MCP cancellation and after `kill_after` ends its process. Checked by the fixture `adapter:lease/self-stop` (FEN-6).
- A lease that expired while the core was not working or the store was not answering does not spend the attempt; the core computes the downtime window from the last tick in the store and extends the leases with one `lease.extend` operation (FEN-5, OBS-8).
- The attempt handles expire together with the lease and are bound to the store epoch (SEC-3).
- Tool calls, child tasks, action requests and messages of an attempt without a live lease are rejected with `fenced`, and their results are given to nobody (FEN-4).
- A seat has no lease: the core writes the seat holder together with the seat epoch, and the seat epoch goes into the token of every seat attempt (IDN-10, MGR-1).
- Inbox entries that an attempt read are leased by it and are acknowledged only in the transaction of its `complete`. On `fail`, a lost lease and `msg.nack` they return to the inbox with the hand-out count one greater, and on cancellation, preemption and a lease that expired during core downtime (FEN-5), without counting a hand-out; their cycle is described in the message lifecycle (MSG-9).

## Versions

| Version | What appears |
|---|---|
| v0.1 | the states `active`, `closed`, `released`, `expired`, `revoked`; the kinds `attempt`, `toolcall`, `delivery`, `erasure`; `lease_no` in the token and the separate counters `attempts_spent`, `runs_counted`; the extension `lease.extend` at core readiness and the reason `core_unavailable`; holder self-stop; revocation on a store epoch increment and by a hold with `cancel_running`; after the core and the first bot (D-71, D-80): the state `preempted`; the seat epoch in the token of a seat attempt and revocation with the reason `seat_changed`; the fields `acting_bot` and `bot_epoch` in the attempt lease record |
| v0.2 | the `drain_deadline` by which a worker in `draining` lets work go |
