# Lifecycle: task

Kind: `task` (the kinds are listed in [the lifecycle index](README.md)).

## In short

A task (`Task`) is a unit of the farm's work: a body that the sender sent, and a task record that only the core writes (IDN-1). The states are named as in the A2A protocol: `submitted`, `working`, `completed`, `failed`, `canceled`, `rejected`. A task in a terminal state is not restarted: a repeat after the end is a new task with a `refs` link to the previous one, and a person's clarification is a new task in the same conversation (CNT-5). There are no waiting states: an attempt never waits for a person, another task, a timer or an external event, waiting lives only in core records (CNT-4). Every hand-out of a task to a worker is a new attempt under a lease with a fencing token (`spec/lifecycles/lease.md`). The task transition, its event, the deliveries, the continuations, the attempt's messages and the usage lines are written in one store transaction (DLV-2).

## States

| State | Meaning | Terminal |
|---|---|---|
| `submitted` | The task is recorded and waits to be handed out through `claim`. If it cannot be handed out now (the `max_running` ceiling, an empty bot group, an open provider circuit, the provider pool share, a retry pause, a hold), the record has a `waiting_reason` from a closed list (DSP-7) | no |
| `working` | An attempt is handed to a worker and holds a lease. An `action.<a>` task has no attempt and no lease: instead its only write call runs, and the task is created directly in this state (CNT-7) | no |
| `completed` | The holder submitted `complete`, and the result passed the type's full schema | yes |
| `failed` | The reason is non-retryable, or the attempts, the runs, the deadline or the budget ran out | yes |
| `canceled` | The task was canceled on request, together with its cause (a person was disabled, rights were revoked), or by the core by a rule (the preemption limit) | yes |
| `rejected` | The enqueue was rejected. There is no task record, the idempotency key is not taken, the refusal is written to the audit; this is an answer to the enqueue, not a stored state | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `submitted` | P1 `enqueue` (a worker with an attempt token, an external program with its own key); P1 `submit_activity` (a channel); P3 `enqueue_child`; the `schedule` loop; a tenant route (an event, `on: inbox`, `on: task_done`) | all enqueue checks passed in order: the core is ready, the schema of the request and the body, the initiator's rights `task:<type>` and `data:<mark>`, `may_enqueue` and the tree limits, the references, holds with `no_enqueue`, the budget, the queue ceiling, the key is free | IDM-1, IDM-2, PRM-3, PRM-6, TEN-7, HLD-3, BUD-1, CAP-1 |
| (none) | `working` | the core in the transaction of an approval | an `action.<a>` task with `approval_id` and the key `system:approval:<id>` is created directly in `working` together with its write call, without an attempt and a lease; P1 `enqueue` does not accept such types; the dispatcher filter, the `deadline` and `reaper` loops and the `start_by` check skip it | APR-5, APR-7, IDM-2, CNT-7 |
| (none) | `submitted` | the core in the transaction of a trigger: a continuation (`reply`, `campaign`, `script`), a campaign item (the `campaign` loop), a script step task (the `script` loop), a seat task (the `incident` loop, a human directive) | a key from the closed list; the initiator's rights are rechecked; on a refusal there is no task, and the person in `origin` gets a ready code text | CNT-1, CNT-2, CNT-3, ORC-3, SCR-8, MGR-4 |
| (none) | `submitted` | P2 `task.redrive` | the original work is dead and its body is not deleted yet; the type is not `action.*`; a new task with a `refs` link to the original and the key `system:redrive:<id>:<n>` | DLV-4 |
| (none) | `rejected` | any enqueue source above | the first failed check; the reason is from `reasons.yaml`, for example `input_schema_invalid`, `no_group_cap`, `no_data_class`, `ref_invalid`, `group_invalid`, `held`, `budget_exhausted`, `over_capacity`, `tree_too_large`, `memory_untrusted`; a refusal on rights carries the field `needs: [<right>]`, the code text names the group | IDM-1, PRM-8, DSP-4, DSP-7 |
| (key taken) | (no transition) | P1 `enqueue` with a key that the tenant already has | the same initiator and the same body: the answer `TaskRef{id, existing: true}`; otherwise `idempotency_conflict` without an id; the key of a deleted task gives `gone` | IDM-1, RET-3 |
| `submitted` | `working` | P1 `claim` | the dispatcher filter by the closed list of conditions `dispatch-predicates.yaml` (D1-D20): the tenant, the role's `claims`, `requires` by labels, `to_group`, a personal assistant task only to its worker, holds with `no_claim` and `cancel_running`, `max_running`, the retry pause `run_at`, the provider circuit and pool share; the initiator is active and has the rights at the recheck; a seat task has the current seat epoch. An attempt lease is created with `lease_no` one greater; the filter does not hand out `action.*` tasks | FEN-1, PRM-3, DSP-1, DSP-3, DSP-4, HLD-1, IDN-5, MGR-1, CNT-7 |
| `submitted` | `canceled` | P1 `claim` (the recheck at hand-out) | the initiator is disabled (`person_inactive`) or lost `task:<type>` or `data:<mark>` (`rights_revoked`); the task is not handed out | PRM-3 |
| `working` | `completed` | P1 `complete` with the current token | the result passed the type's full schema; in the same transaction the attempt's messages are published, the read inbox entries are acknowledged, the event, the deliveries, the continuation and the usage are written; a result with `need_input: true` opens the follow-up window `followup_until` (CNT-5) | FEN-2, DLV-2, MSG-9, OUT-1, CNT-5 |
| `working` | `completed` | the outcome `done` of the write call (P1 `complete` from the toolhost) | only an `action.<a>` task: the result from the closed fields of the plugin's outcome, the approval becomes `executed`, in the same transaction | CNT-7, APR-7 |
| `working` | `failed` | the outcome `error`, `denied(held)` or `exec_unknown` of the write call | only an `action.<a>` task: `failed(exec_failed)` on `error` (a plugin error, `timeout_s`, `no_executor`, `plugin_unavailable`) and `denied(held)`, `failed(exec_unknown)` on `exec_unknown`; there is no retry and no `task.redrive` | CNT-7, APR-7, DLV-4 |
| `working` | `submitted` | P1 `fail` with a retryable reason; the `reaper` loop; the `deadline` loop | the attempt is spent (`attempts_spent` one greater): `lease_expired`, `runtime_error`, `model_error`, `attempt_timeout`, `output_schema_invalid`, `stalled` (a stall is counted from the last finished call or progress change, a running call does not interrupt it); `attempts_spent` is less than `max_attempts`, `runs_counted` is less than `max_runs`, `deadline` has not passed; a retry after the `retry.backoff` pause | FEN-1, FEN-3, CNT-4 |
| `working` | `submitted` | P1 `release`; P1 `fail`; the `reaper` loop | the attempt is not spent (only `lease_no` grows): `worker_drain`; `core_unavailable` (the lease expired while the core was not working, and was not extended after `lease.extend`); `provider_unavailable` and `provider_rate_limited` with a pause until `deadline`; `provider_config` waits for a fix until `deadline`; `provider_quota` | FEN-1, FEN-5, CAP-2, GW-12, GW-13 |
| `working` | `submitted` | a hold with the effect `cancel_running` (P2 `hold.set`): the holder gets `cancel_requested` in the `heartbeat` answer | the attempt ends with the reason `held` and is not spent; while a hold with `cancel_running` or `no_claim` on the task's scope is in force, `claim` does not hand it out | HLD-3, FEN-4 |
| `working` | `submitted` | the control operation `preempt` | the attempt is `preempted`, not spent; at most 2 preemptions per task | MGR-11 |
| `working` | `submitted` | P2 `seat.assign`, P2 `seat.vacate`, a seat hold by an incident | an attempt of a seat task gets `cancel_requested` and ends with the reason `seat_changed` without spending; the task waits for a holder with the current epoch | MGR-1, IDN-10, FEN-2 |
| `working` | `submitted` | P1 `grant.request`; P2 `model_grant.revoke` | v0.2; the attempt ends with `awaiting_model_grant` or `grant_revoked` without spending; the task waits with `waiting_reason: model_grant_pending` or continues on the base model | MGR-17, MGR-18 |
| `working` | `failed` | P1 `fail` with a non-retryable reason; the `reaper` loop; the `deadline` loop; the core | `attempts_exhausted`, `runs_exhausted`, `deadline_exceeded`, `budget_exhausted` during work, `provider_spend_cap` (with an alarm by code), a reason from the type's `retry.non_retryable` | FEN-3, DLV-4, CAP-2, BUD-2 |
| `submitted` | `failed` | the `deadline` loop | `deadline` has passed (`deadline_exceeded`); the loop skips `action.*` tasks | DLV-4, CNT-7 |
| `submitted`, `working` | `canceled` | P1 `cancel` (the initiator, an external program for its own tasks); P2 `task.cancel` (the initiator with the `/cancel` command, a tenant admin, the operator); a cascading cancellation of the parent | the outcome `canceled(initiator)` if the initiator or the program canceled, and `canceled(admin)` if a tenant admin or the operator did; in a cascade the children get the parent's reason; the holder learns about the cancellation in the `heartbeat` answer | FEN-4, PRM-9, IDM-3 |
| `submitted`, `working` | `canceled` | the core in the transaction of the cause: P2 `principal.off`, P2 `ungrant`, a change of groups through `apply` | `person_inactive` or `rights_revoked` | PRM-3 |
| `submitted`, `working` | `canceled` | the control operation `cancel`; the third preemption; from v0.2 a takeover | `by_manager`, `preempt_limit`; v0.2: `superseded_by_takeover` | MGR-9, MGR-11, MGR-21 |

The "nobody took it" deadline (`start_by`) is not a transition: the `deadline` loop raises the alarm `alarm.task.unclaimed`, and the task stays in `submitted` (OBS-6, DSP-8); this check skips `action.*` tasks (CNT-7). Every transition writes a `task.<state>` event with metadata without content; a retry after a spent attempt writes `task.retrying`.

## Invariants

- Only the core writes the fields of the task record (the tenant, the initiators, `origin`, the conversation, the parent and the depth, `max_tier`, `data_mark`, `untrusted_seen`, `approval_id`, `continues`, the campaign, script and seat keys, the priority class); the schema rejects a request with such a field (IDN-1).
- The pair `(tenant, idempotency_key)` is unique also after the task is deleted; only the core sets `system:` keys and only from a closed list (IDM-1, IDM-2).
- At most one attempt holds a task; a changing call with an old token gets `fenced` and changes nothing (FEN-1, FEN-2). The counters `lease_no`, `attempts_spent` and `runs_counted` are kept separately: the first grows at every `claim`, the second only on an outcome that spends, the third only on a runtime run with a model answer (FEN-1).
- An `action.<a>` task lives together with its write call: it is created in `working` and ends with the call's outcome; it has no attempt, lease, `claim`, `start_by` and `deadline_exceeded` (CNT-7).
- The inbox entries that an attempt read are acknowledged only in the `complete` transaction. On `fail`, a lost lease and `msg.nack` they return to the inbox with a hand-out count, and on cancellation, preemption and a lease that expired during core downtime, without the count (MSG-9, FEN-5).
- The reason an attempt ended is taken only from `reasons.yaml`; the registry decides whether it spends the attempt and whether the task is retried. An infrastructure failure and core downtime do not spend the attempt (FEN-5, CAP-2).
- There are no `input_required` and `auth_required` states; a waiting task does not hold a lease (CNT-4).
- A terminal state is final: no call moves a task from `completed`, `failed`, `canceled` back; `task.redrive` creates a new task (DLV-4).
- A delivery failure does not change the state of the task (DLV-3).
- Every task ends in a terminal state with an event; there are no silent losses (DLV-1).

## Versions

| Version | What appears |
|---|---|
| v0.1 | all states; the sources `enqueue`, `submit_activity`, `enqueue_child`, the schedule, a route by event, approval, `task.redrive`; the rights recheck at `claim`; the reasons from `reasons.yaml`, including `core_unavailable`, `stalled`, `held` from `cancel_running`, `provider_rate_limited`; the counters `lease_no`, `attempts_spent`, `runs_counted`; the `action.<a>` task in `working` with the outcomes `exec_failed` and `exec_unknown`; the follow-up window after `need_input`; the `needs` field in a refusal; the alarm on `start_by`; after the core and the first bot (D-71, D-80): routes by inbox and the auto-report, acknowledgement of read inbox entries in `complete` (MSG-9, D-71); continuations, campaign items, script step tasks, seat tasks with the epoch; `preempt` and `preempt_limit`, `by_manager`, `seat_changed`; the reason `provider_quota`; the fields `acting_bot` and `bot_epoch` in the attempt |
| v0.2 | waiting for a model grant (`awaiting_model_grant`, `model_grant_pending`, `grant_revoked`); takeover (`superseded_by_takeover`); continuations by `timer` and `await`; P1 `enqueue` with `mode: validate`, which checks and writes nothing |
