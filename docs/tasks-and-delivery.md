# Tasks and delivery

## In short

This document describes how a task lives after it has been submitted: which record the core writes, how enqueue works, how the idempotency key prevents a second task, how an attempt holds a lease and why a late worker cannot spoil anything, which reasons spend an attempt, where waiting lives, and how the answer reaches people. The main idea: everything that permissions, the address and retries depend on is decided by core code from store records, not by the model and not by the sender. An attempt never waits; waiting lives in core records, and the core enqueues the continuation (D-41). A delivery is handed out at least once, the address is taken only from a closed list, and a group chat gets only a summary (D-48). The task language (body, catalog, types, manifests) is described in `docs/task-language.md`, permissions and approvals in `docs/permissions.md`, and the machine transition tables in `spec/lifecycles/task.md`, `lease.md`, `toolcall.md`, `delivery.md`.

## Task record

A task consists of a body (the sender submitted it) and a task record (only the core writes it). The schema rejects a request that contains at least one record field (IDN-1).

| Record field | Meaning | Version |
|---|---|---|
| `id`, `tenant`, `type` | task id (never reused), the tenant from the credentials, the type from the body | v0.1 |
| `initiator` | the principal on whose behalf the task runs: a person, an external program (`service`) or `system` | v0.1 |
| initiator set | the initiator and the initiators of everything the attempt has read (messages, inbox entries); the attempt's permissions are intersected with all of them (PRM-6) | v0.1 |
| `enqueued_by` | who enqueued it: a channel, a worker with an attempt, a schedule, a route, the core; for a seat task, a person with a directive or a core loop | v0.1 |
| `origin` | where to answer: the channel and chat, signed by the core on intake | v0.1 |
| `conversation` | the conversation: an opaque thread id computed from `origin` | v0.1 |
| `parent`, `root`, `depth`, `correlationid` | the task tree: parent, root, depth (at most 5), tree root for limits | v0.1 |
| `refs`, `continues` | references to tenant objects; the core sets `continues` on a reply-to answer (CNT-5) | v0.1 |
| `max_tier`, `data_mark` | the task tier and data mark; the mark is fixed at enqueue (PRM-4, PRM-5) | v0.1 |
| `untrusted_seen` | the distrust flag with its source; it is only set, never removed (INJ-13). A goal from an external program sets it at enqueue with the source `goal:service` unless the type declared `goal_trust: authored`. A route task for a monitoring system problem of its own tenant with a trigger from `monitor_triggers` does not get the flag, and the record shows `monitor` as the task's source (for audit and cards, D-82) | v0.1 |
| `personal_subjects` | principals whose personal data is in the body, attachments or result (the initiator, authors of forwarded messages, participants of a group chat); the core sets it, and erasure `erase(tenant, subject)` works by it (SEC) | v0.1 |
| `approval_id` | the approval number of an `action.<a>` task | v0.1 |
| `to_group`, `priority_class`, `fairness_key`, `scope` | bot group address, priority class, the dispatcher's fairness key, resource scope | v0.1 |
| `state`, `lease_no`, `attempts_spent`, `runs_counted`, `waiting_reason`, `run_at` | state; number of the last lease (grows on every `claim`); number of spent attempts (compared with `max_attempts`); number of runtime runs with a model answer (compared with `max_runs`); why the task waits; the time before which it is not dispatched (retry pause). The counter table is in the section "Attempts, lease and fencing" | v0.1 |
| `followup_until` | end of the follow-up window after a result with `need_input: true` (CNT-5) | v0.1 |
| `campaign`, `campaign_item`, `script_run`, `script_step` | link to a campaign and a script run | v0.1 |
| `acting_bot`, `bot_epoch`, `seat_epoch`, `incident` | the bot identity on whose behalf the attempt runs, and the seat epoch | v0.1 |
| `grant`, `takeover_of`, `trace` | model grant, takeover, trace | v0.2 |

The body and the result are content: they lie in a deletable store with the expiry of their retention class, and their schemas declare the class `content` in `x-botaru-retention` (RET-1). The event, the audit and the transition journal carry only metadata (AUD-2). After a task is deleted, a tombstone of class `tombstone` remains (RET-3).

## Enqueue

### Where tasks come from

| Source | Operation | Initiator | Idempotency key |
|---|---|---|---|
| a person in a channel | P1 `submit_activity` from the channel | the person, by verified identity | `system:activity:<channel>:<update id>` (IDM-5) |
| external program (D-47) | P1 `enqueue` with its own key | the program | the program's own key |
| `job` worker | P1 `enqueue` with the attempt token | the parent's initiator | the worker's own key |
| model | P3 `enqueue_child` | the parent's initiator | `system:child:<parent>:<body hash>` |
| schedule | the `schedule` loop | `system` | `system:schedule:<schedule>:<slot>` |
| event route | the core in the event's transaction | `system` | `system:event:<route>:<source>:<id>` |
| inbox route, auto-report | the `inbox` loop, the core in the task end transaction | `system` | `system:wake:<route>:<window>`, `system:report:<task>:<route>` |
| approval of an approval request | the core in the decision transaction | the approval's initiator | `system:approval:<id>` |
| dead work | P2 `task.redrive` | the initiator of the original task | `system:redrive:<id>:<n>` |
| continuation, campaign item, script step, incident | the core | by the rules of its part | keys from the closed list of IDM-2 |

### Order of checks

The core checks an enqueue in order, and the first failure stops it. A refusal creates no task record and does not take the key; the refusal is written to the audit, a person gets a ready code text, and the model a short "no permission" without details (PRM-8). A permission refusal (`no_group_cap`, `no_data_class`, `forbidden`) carries the field `needs: [<permission>]`, which code fills in: which permissions are missing; the code text for a person names the tenant group that grants this permission and who grants it (the tenant's `delegated_groups` are granted by the admin, the rest by the operator). Code adds the same `needs` field to the result of a task that ended with a refusal because of tool permissions: it is taken from the refusal records of this attempt, not from model text.

1. **The core is ready.** While the store is unavailable, a restore from backup or a core drain is in progress, the answer is `not_ready` with `retry_after_s` (OBS-8, RET-6).
2. **Tenant and principal from the credentials.** The tenant in the request, if the adapter passed one, equals the tenant of the credentials; otherwise `tenant_mismatch` and an alarm (TEN-2).
3. **Request and body schema.** Record fields, a "this worker" field, a `system:` key from anyone but the core, an `action.*` type and an unknown type are rejected by the schema: `input_schema_invalid` (IDN-1, DSP-4, APR-5).
4. **Key.** If the pair `(tenant, idempotency_key)` already exists: the same initiator and the same body (by the SHA-256 hash of the canonical JSON form per RFC 8785, `spec/hashing.md`) give the existing task `TaskRef{id, existing: true}` without further checks; otherwise `idempotency_conflict` without an id; the key of a deleted task gives `gone` (IDM-1, RET-3).
5. **The initiator is active:** otherwise `person_inactive`.
6. **Holds before permissions.** A hold with the effect `no_enqueue` on the task's scope gives `rejected(held)`, except for core tasks for erasure and export (HLD-3, PRM-2).
7. **Permissions.** Every initiator has `task:<type>` with the full type name and version `.vN`, or a permission on the family `task:<name>.*` (`no_group_cap`, PRM-7). If a worker enqueues: the type is in the role's `may_enqueue`, the depth is at most 5, children at most `max_children`, tree tasks at most `max_tree_tasks`, the tier no higher than the parent's (`depth_exceeded`, `tree_too_large`, `tier_exceeded`); `to.group` is among the allowed ones (`group_invalid`) (PRM-6, DSP-4).
8. **Mark.** `data_mark` is the maximum of the type's class and the parent's mark; the initiator has `data:<data_mark>` (`no_data_class`). The distrust flag is set according to INJ-13, and for a task from an external program also according to the type's `goal_trust`: without `goal_trust: authored` the program's goal is untrusted text (`untrusted_seen` with the source `goal:service`); a type that writes memory gets `memory_untrusted` when the flag is set (PRM-5, MEM-3).
9. **References.** `file_ref` and `refs` only to own objects; for foreign and nonexistent ones the same `ref_invalid` (TEN-7).
10. **Parameters.** The core fills in default values and checks `params` against the type's `params_schema`: `input_schema_invalid` (INJ-1).
11. **Tenant ceilings.** `max_queued` and `max_enqueue_per_hour`: `rejected(over_capacity)` with `retry_after_s` (CAP-1, DSP-7).
12. **Budget.** The core reserves the estimate in all applicable limits (tenant, principal, tree): `budget_exhausted` (BUD-1).
13. **Write.** The task record, the event `task.submitted`, the reserve and the key are written in one transaction (DLV-2).

From v0.2 a request with `mode: validate` goes through the same checks and writes nothing (PRM-11).

### Idempotency keys

The retry key is needed so that a network failure, a channel retry or a second copy of a loop does not produce a second task. The key is unique within the pair `(tenant, idempotency_key)` (D-43); after a task is deleted, a tombstone `(tenant, key hash, task id)` remains, and the key stays taken while the tenant lives (IDM-1, RET-3).

Keys with the prefix `system:` are set only by the core and only from a closed list; a new core key appears only by editing this list (IDM-2). A trigger that fires twice gives one task, while two routes on one event give two tasks, because the route name is part of the key.

| Key | What sets it | Version |
|---|---|---|
| `system:schedule:<schedule>:<slot>` | the `schedule` loop; the slot in the `schedule_slot` format (TIM-3) | v0.1 |
| `system:approval:<id>` | an action task on approval | v0.1 |
| `system:event:<route>:<source>:<id>` | an event route | v0.1 |
| `system:activity:<channel>:<update id>` | a channel incoming message | v0.1 |
| `system:redrive:<id>:<n>` | a redrive of dead work, not for `action.*` | v0.1 |
| `system:child:<parent>:<body hash>` | a child task from `enqueue_child`; a retry of the parent's attempt does not give a second child | v0.1 |
| `system:msg:<attempt>:<n>` | a bot message, published in the `complete` transaction (MSG-9) | v0.1 |
| `system:wake:<route>:<window>`, `system:report:<task>:<route>` | an inbox wake-up and an auto-report (MSG-16) | v0.1 |
| `system:review:<proposal>:<lens>` | a commission lens review task | v0.1 |
| `system:cont:<trigger>:<source id>` | a continuation, including `system:cont:reply:<child task>` and `system:cont:campaign:<campaign>` | v0.1 |
| `system:script:<run>:<step>` | a script step task, which is also a continuation with the trigger `script` (SCR-8) | v0.1 |
| `system:campaign:<campaign>:<n>` | a campaign item (ORC-3) | v0.1 |
| `system:incident:<class>:<subject>:<window>`, `system:control:<epoch>:<number>` | an incident task and a control operation | v0.1 |
| `system:incident:<incident>:close` | the closing pass of an incident that moved to `resolved` | v0.1 |
| `system:inspect:<basis>:<n>` | a data read by the manager seat (`manager.inspect.v1`) from the `inspect` field of an incident class or from `inspect_request` of the decision role | v0.1 |
| `system:daily:<tenant>:<day>` | the daily review of the manager seat, the task `manager.review.v1` on the core schedule (MGR-41) | v0.1 |
| `system:handover:<fact offer>` | fact selection by the successor, the task `memory.pick.v1` with the purpose `handover` (MEM-8) | v0.1 |
| `system:ingress:<source>:<delivery id>`, `system:escalation:<attempt>`, `system:takeover:<task>` | an external event, an escalation to the manager, a takeover | v0.2 |
| `system:grants:<tenant>:<window>` | a batch of waiting model grant requests, the task `manager.grants.v1` every 10 minutes | v0.2 |
| `system:governance:<case>:<round>:<bot>` | a bot's ballot on a case: a voter has one task per round | v0.2 |
| `system:review:<proposal>:<lens>:shadow` | a task of a lens's shadow reviewer; it does not merge with the seat's task (COM-10) | v0.2 |
| `system:necessity:<escalation>` | review of an escalation to the manager by the necessity lens in the source tenant (COM-13) | v0.2 |

The core computes the key of a channel incoming message itself from (channel, update id), so one update submitted twice gives one task (IDM-5); the channel moves the external system's offset only after the core answers (DLV-5).

Example. The external program `report-client` of tenant `demo` enqueues a nightly report with the key `nightly-2026-10-05`. The answer is lost in the network, the program repeats the request with the same body and gets the same `TaskRef` with `existing: true`. Had it sent a different body with the same key, the answer would have been `idempotency_conflict` without an id. Half a year later the task body has been deleted by expiry, and a repeat of the same key gets `gone`, not a new task.

## Task lifecycle

The states are named as in A2A: `submitted`, `working`, `completed`, `failed`, `canceled`, `rejected` (the machine table is in `spec/lifecycles/task.md`). A task in a final state is not restarted; a retry after the end is a new task with a `refs` link. There are no `input_required` and `auth_required` states (CNT-4): if a bot needs something from a person, it answers with a question and ends the attempt, and the answer becomes a new task (section "Waiting and continuations").

Only the dispatcher filter, by the closed list of conditions `dispatch-predicates.yaml`, dispatches a task from `submitted` (DSP-3). A task that cannot be dispatched right now carries a `waiting_reason` from a closed list (DSP-7):

| `waiting_reason` | Meaning | Version |
|---|---|---|
| `no_member`, `no_member_ready` | the task's bot group is empty, or none of its members is ready | v0.1 |
| `type_quota` | `max_running` per (tenant, type) is exhausted | v0.1 |
| `held` | a hold with `no_claim` covers the task | v0.1 |
| `run_at` | the retry pause has not ended yet | v0.1 |
| `provider_unavailable` | the circuit of the needed (workspace, model pool) pair is open, including because of a provider configuration error (GW-11, GW-13) | v0.1 |
| `resource_busy`, `not_released` | the resource is taken by another released write call (ORC-12); the campaign item has not been released yet | v0.1 |
| `provider_quota` | the tenant has no free share of the needed model pool (GW-10) | v0.1 |
| `group_quota`, `model_grant_pending` | the bot group's `max_running` or the rule's `max_share` is exhausted; the task waits for a decision on a grant of a stronger model (MGR-17) | v0.2 |

Under pressure, work is not dropped and an attempt is not spent. The `start_by` deadline is not a transition: the `deadline` loop raises the alarm `alarm.task.unclaimed` by code (DSP-8).

## Attempts, lease and fencing

An **attempt** is one dispatch of a task to a worker. P1 `claim` creates a lease with a `lease_no` 1 greater than the previous one and the token `(epoch, work_id, lease_no, worker_id)`: `epoch` is the store epoch, and `work_id` is shared by all leases of the task (FEN-1). The holder extends the lease with `heartbeat`, hands in the result with `complete` or `fail`, and on stopping returns the work with `release`. The same mechanism serves a tool call and a delivery (`spec/lifecycles/lease.md`). One attempt lease corresponds to one runtime run, and its identifier for E4 is `<task id>:<lease_no>`.

**Three counters.** The core keeps them separately, and each is compared with its own ceiling (FEN-1, FEN-3):

| Counter | What increments it | What does not increment it | What it is compared with |
|---|---|---|---|
| `lease_no` | every `claim`, including after `held`, `core_unavailable`, `worker_drain`, `provider_unavailable`, `provider_rate_limited`, `preempted`, `seat_changed` | nothing resets it | nothing; it is part of the token and of `run_id` |
| `attempts_spent` | an outcome that spends an attempt according to the registry: `lease_expired`, `runtime_error`, `model_error`, `attempt_timeout`, `output_schema_invalid`, `stalled` | outcomes without spending (`held`, `core_unavailable`, `worker_drain`, provider, preemption, seat change) | `retry.max_attempts`, ceiling 3 |
| `runs_counted` | a runtime run in which the model answered at least once | a run that ended before the first model answer (`provider_unavailable`, `provider_rate_limited`, `provider_quota`, `provider_config`, `core_unavailable`); `job` tasks | `retry.max_runs`, ceiling 6 |

Example: a task got two `provider_unavailable`, one `held` and three `lease_expired`: `lease_no` is 6, `attempts_spent` is 3, and it ends with `failed(attempts_exhausted)`.

**Fencing.** A process can "fall asleep" (a garbage collection pause, a network outage) for longer than the lease and wake up without knowing it. Therefore every mutating call of the holder carries the token, and the store rejects a call with an old number, with a previous store epoch or with a previous seat epoch: the answer is `fenced`, and nothing is written (FEN-2). Acceptance: 10,000 tasks on 8 competing workers give 0 duplicates and 0 losses; `kill -9` of a worker in the middle of a task, the second attempt finishes the work, and the late `complete` of the first gets `fenced`.

**Deadlines and ceilings.** The task type sets the deadlines, and the farm sets the ceilings: lease 120 s, `heartbeat` interval 30 s, at most 3 attempts, at most 6 runtime runs with a model answer (FEN-3). A tenant dispatcher rule in `Tenant.spec.dispatch` can, for its type, raise the attempt deadline `timeouts.attempt` by at most a factor of two and not past the task's `deadline`, and lower `retry.max_attempts` to no less than 1; it does not exceed the farm ceilings (DSP-3).

```yaml
timeouts:
  start_by: 10m      # nobody took it: alarm alarm.task.unclaimed
  lease: 120s        # farm ceiling
  heartbeat: 30s     # farm ceiling
  attempt: 5m        # one attempt
  stall_after: 3m    # no progress: the attempt ends with stalled
  deadline: 30m      # the whole task with all attempts
retry:
  max_attempts: 3    # farm ceiling; 1 for a non-idempotent action
  max_runs: 6        # farm ceiling; only runs with a model answer count
  backoff: {initial: 5s, factor: 2, max: 60s}
  non_retryable: []  # in addition to reasons.yaml
```

**Cancellation.** The holder learns about a cancellation in the `heartbeat` answer (`cancel_requested`) and stops working before the next `heartbeat`. Tool calls, child tasks, action requests and messages of an attempt that has lost its lease or been canceled are rejected with `fenced`, and their results are given to nobody (FEN-4).

**Inbox entries read.** Entries that an attempt has read are acknowledged only in the transaction of its `complete`. On `fail`, loss of the lease and `msg.nack` they return to the inbox with the delivery count one higher, and after the third delivery the entry ends as `dead(delivery_exhausted)`. On cancellation, preemption and a lease that expired while the core was not running (FEN-5), the entries return without counting a delivery (MSG-9).

**No progress.** An attempt that for longer than `stall_after` has no completed P3 or P4 call and no change of progress in `heartbeat` ends with the reason `stalled`: the attempt is spent, the task is retried, and the core raises the alarm `alarm.task.stalled` (CNT-4). The count starts from the latest of three moments: the start of the attempt, the last completed call (`done` or `error`), the last change of progress; a running call in `queued` or `leased` does not move the count. Without `stall_after` in the type there is no such check.

**A tool call does not wait for a worker.** A call has a dispatch deadline `start_by` (ceiling and default 30 s, the type can lower it with the field `timeouts.tool_start_by`): a call in `queued` that no toolhost of the target took within this deadline ends with `error(no_executor)`, and the model sees an error instead of the attempt waiting for `attempt_timeout`; if the target has no live toolhost or the plugin is in the `down` state, the call gets `error(plugin_unavailable)` at once (DLV-16, OBS-10). A write call with such an outcome gives the approval `exec_failed(no_executor)`.

**Action task.** An `action.<a>` task is created in the approval transaction directly in `working`, without an attempt and a lease: its work is the single write call. `done` of the call gives `completed`, `error` and `denied(held)` give `failed(exec_failed)`, and `exec_unknown` gives `failed(exec_unknown)`; the dispatcher filter, the `deadline` and `reaper` loops and the `start_by` check skip such tasks, and the deadline is held by the action's `ttl` and the call's `timeout_s` (CNT-7, APR-7).

**Core downtime does not spend an attempt.** The core downtime window is the interval from the last successful tick of any background loop in the store to the moment the core is ready; the core writes nothing at the start of the window (there may be nowhere to write) and computes it when leaving `not_ready`. When ready, the core, with one C1 `lease.extend` operation under a lock, extends every lease that was `active` at the start of the window to "the moment of readiness plus the lease period"; a second copy of the core changes nothing with the same operation if the lease has already been extended to a later deadline. A lease that the holder did not extend even after that ends with the reason `core_unavailable`: the attempt is not spent and does not count toward `max_runs`, and the task returns to the queue without a pause. A read call in that case gets `error(core_unavailable)`, a write call `exec_unknown`, and a delivery returns to `planned` without spending an attempt. A worker adapter that could not reach P1 or P4 repeats `heartbeat` with random jitter until `lease_remaining_s` runs out. Fixtures: the core is stopped for 5 minutes in the middle of a task, and after the start the task is finished without a spent attempt; two copies of the core leave downtime a minute apart, and the lease is extended once (FEN-5).

**The holder stops itself.** Every holder of work outside the core (an agent adapter, a `job` worker, a toolhost with plugins, a channel, a sink) counts the lease period on its own monotonic clock from the `lease_remaining_s` of the last P1 answer and stops working if it has not extended the lease by the deadline minus a margin (10 s by default). After that it writes nothing either to the farm or to the outside world, except a message to the core about the end (`fail` or `release`). On `abandoned`, `lease_expired` and self-stop, the toolhost sends the plugin an MCP cancellation and after `kill_after` (10 s by default) terminates its process. The check is observable: a `job` worker gets SIGSTOP for 3 minutes; after SIGCONT the core receives no mutating call with its token except `fail` or `release`, and the external system stub receives no write at all (FEN-6).

**The fencing boundary.** The farm store checks the token, but an external system (systemd, someone else's API, a file queue) does not. Therefore every write to the outside world carries a work id shared by all attempts: an action's write call passes `approval_id` (for a campaign item `<approval_id>:<n>`), a sink writes idempotently by `delivery.id`, and an outgoing webhook passes it in the `webhook-id` header. A task type whose `job` worker writes to an external system declares `side_effects: external` and passes it `work_id`; a type that does not pass the key gets `max_attempts: 1`. A retry of a delivery to a channel without an idempotency key is acceptable, and `card_update` edits the already sent message rather than sending a new one (FEN-7).

## Reasons and retries

The reasons for the end of an attempt, for refusals and for waiting are in one registry, `spec/model/reasons.yaml`. The registry decides whether a reason spends an attempt, whether the task is retried and with what pause. In `fail` a worker passes a code from the registry, `retryable` and `retry_after_s`; records contain no free text reason.

| Reason | Spends an attempt | Retry | Result if there is no retry | Version |
|---|---|---|---|---|
| `lease_expired`, `runtime_error`, `model_error`, `attempt_timeout`, `output_schema_invalid` | yes | yes, with a `backoff` pause | `failed(attempts_exhausted)` | v0.1 |
| `stalled` | yes | yes; alarm `alarm.task.stalled` | `failed(attempts_exhausted)` | v0.1 |
| `worker_drain`, `core_unavailable` | no | yes, at once | | v0.1 |
| `provider_unavailable`, `provider_rate_limited` | no | yes, with a pause and full random jitter until `deadline` (CAP-2, GW-12) | `failed(deadline_exceeded)` | v0.1 |
| `provider_config` | no | not by timer: waits for a fix until `deadline`; alarm by code (GW-13) | `failed(deadline_exceeded)` | v0.1 |
| `held` (hold with `cancel_running`) | no | after the hold is removed | | v0.1 |
| `provider_quota` | no | waits for a pool share (GW-10) | `failed(deadline_exceeded)` | v0.1 |
| `preempted`, `seat_changed` | no | yes (MGR-11, IDN-10) | | v0.1 |
| `awaiting_model_grant`, `grant_revoked` | no | after the grant decision or on the base model (MGR-17) | | v0.2 |
| `provider_spend_cap` | no | no; alarm by code | `failed` | v0.1 |
| `budget_exhausted` during work, `deadline_exceeded`, a reason from `non_retryable` | no | no | `failed` | v0.1 |
| `attempts_exhausted`, `runs_exhausted` | | | `failed`, visible among "dead" work | v0.1 |
| `exec_failed`, `exec_unknown` (only an `action.<a>` task) | | no; a new action request is needed | `failed` (CNT-7) | v0.1 |
| `no_executor`, `plugin_unavailable` (tool call) | no | the call is not retried, the model sees the error (DLV-16) | `error` of the call; for a write call `exec_failed` of the approval | v0.1 |
| `initiator` (the initiator or an external program canceled its own task, result `canceled(initiator)`), `admin` (the tenant admin or an operator canceled it, result `canceled(admin)`), `rights_revoked`, `person_inactive` | no | no | `canceled` | v0.1 |
| `by_manager`, `preempt_limit`; from v0.2 `superseded_by_takeover` | no | no | `canceled` | v0.1 |
| `tenant_mismatch`, `input_schema_invalid`, `no_group_cap`, `no_data_class`, `tier_exceeded`, `depth_exceeded`, `tree_too_large`, `group_invalid`, `ref_invalid`, `held`, `over_capacity`, `budget_exhausted`, `memory_untrusted` | | no | `rejected` at enqueue | v0.1 |
| `idempotency_conflict`, `gone`, `not_ready`, `fenced` | | | an answer to the call, there is no task and records do not change; reading a file in the state `expired` or `purged` also gets `gone` | v0.1 |

A runtime run that ended before the first model answer with the reason `provider_unavailable`, `provider_rate_limited`, `provider_quota`, `provider_config` or `core_unavailable` does not count toward `runs_counted`; exceeding the run ceiling gives `failed(runs_exhausted)` (FEN-3, GW-13). Only one layer retries a provider error: the client inside P4 at most once, the runtime does not retry, and after that the core retries the task according to the type's policy (GW-12).

**Dead work.** A task that has exhausted its attempts, runs or deadline, and a delivery that has exhausted its attempts, are visible in the "dead" view (P2 `deadletter.list`, a summary by type and reason) and produce an event. P2 `task.redrive` and `delivery.redrive` are operator operations through `farmctl` (PRM-14); they create new work with a reference to the original and the key `system:redrive:<id>:<n>`, and permissions and ceilings are checked again. There is no retry for `action.*` tasks: a new action request is needed (DLV-4, APR-7). A delivery failure does not touch the task (DLV-3).

### Codes of other parts

The same registry holds the codes of other parts of the farm. Each part describes its codes in its own document, while the registry is one; below is an index. The two codes about rate limiters differ by prefix: `provider_rate_limited` is an answer from the model provider, `service_rate_limited` is the farm's own limiters (services and scripts, cards, wake-ups, control operations); a permission refusal is called `forbidden` everywhere.

| Part | Codes | Rules |
|---|---|---|
| port answers to a call; records do not change | `fenced` (the holder stops working), `not_ready` (retry after `retry_after_s`), `not_found` (one answer for foreign and nonexistent), `type_not_allowed` (an event outside the type's `events`, P1 `emit`), `gone`, `handle_invalid`, `forbidden`, `already_terminal`, `version_unsupported` | FEN-2, FEN-4, OBS-8, PRM-9, IDM-4, RET-3, LCY-7 |
| permissions and human decisions (`docs/permissions.md`) | `rights_revoked`, `person_inactive`, `egress_denied`, `self_approve`, `privileged_group`, `already_decided` (a different decision or a different person on an already decided subject, IDM-3), `expired`, the `needs` field in a permission refusal, `service_rate_limited` (the card ceiling `attention.cards_per_tenant_hour`), `forbidden` (the initiator lacks `service.control:<service>` or `monitor.script_run`, D-73), `operator_only`; from v0.2 `details_not_opened`; approval outcomes `exec_unknown`, `exec_failed(held)`, `exec_failed(no_executor)`, `canceled(held)`, `canceled(ref_invalid)`, `canceled(restored)`, `canceled(initiator)` (cancellation by the initiator through P2 `task.cancel`) | PRM-3, PRM-13, PRM-14, APR-2, APR-4, APR-6, APR-7, APR-9, APR-14, APR-18, HLD-3, IDM-3, MAN-10, RET-6 |
| pool and dispatcher (`docs/coordination.md`, profile `core`) | `group_invalid`, `waiting_reason` from a closed list, `activity.ignored(not_subject)` (an incoming message in a group from a non-principal), the lint `assistant_in_default_types` | DSP-3, DSP-4, DSP-7, DSP-10, MSG-17 |
| bot messages and campaigns (`docs/messaging.md`, `docs/coordination.md`) | `address_invalid`, `data_class_exceeded`, `delivery_exhausted`, `inbox_full`, `sender_quota`, `hops_exceeded`, `msg_type_not_allowed` (a message type outside the role's `messages.send`), `note_not_allowed` (a note on a type without the right to one), `data_schema_invalid`, `discarded(held)`, the lint `summary_fields_required`; `service_rate_limited` and `wake.skipped(budget_exhausted | service_rate_limited)` (the wake-up ceilings `wake_day_eur`, `wakes_per_day`); `resource_busy`, `estimate_exceeded`; `discarded(recipient_gone)` (the recipient of the inbox entry no longer exists); for a campaign `campaign_frozen`, `campaign_policy_exceeded`, `campaign_untrusted_limit`, the pause reasons `gate` and `human` (the "Pause" button), the stop reason `threshold_exceeded` | MSG-3, MSG-4, MSG-6, MSG-9, MSG-10, MSG-11, MSG-13, MSG-15, REP-11, HLD-3, ORC-1, ORC-4, ORC-5, ORC-12, ORC-14, BUD-12 |
| operations (`docs/operations.md`) | `over_capacity`, `held`, `restored`, `provider_spend_cap`, `plugin_unavailable` (v0.1, DLV-16), `draining` and `unknown` (`health` values), `runaway_spend` (a hold by `hour_eur`), `contain_auto` (a hold of an incident's first measure and of a maintenance window, removed by the tenant admin or an operator), `pause_action` (a maintenance window without `maintenance_hold`), `monitor_export_stalled`, the event `schedule.skipped` with the reason `waiting`; from v0.2 `schedule_time_invalid` (refusal of a proposal after which the backup schedule would lose a slot); hold reasons from `holds.yaml` (`secret_in_outbound`, from v0.2 `spend_rate` and `suspended`); erasure request reasons `forget`, `offboarding`, `bot_retired`, `tenant_delete`, `worker_revoke`, `subject_request`, `manual` | CAP-1, CAP-2, HLD-1, HLD-3, HLD-6, HLD-7, LCY-1, OBS-10, RET-6, RET-7, TIM-3, TIM-5 |
| models and money (`docs/models-and-money.md`) | `provider_unavailable`, `provider_rate_limited`, `provider_config`, `provider_quota` (table above), `budget_exhausted` | GW-10, GW-11, GW-12, GW-13, CAP-2, BUD-1 |
| bots, memory and commission (`docs/bots.md`, `docs/commission.md`) | `rank_exceeded`, `memory_untrusted`, the notice `principal_left`, `fact_insufficient`, `fact_mismatch`, `stale`, `seat_changed`, `veto_lenses_same_family`, the marks `defaulted_majority` and `reopened_after_wrong_leaver`, cancellation of an incident attribution `incident_dismissed`; from v0.2 `manager_absent`, `not_eligible`, `candidate_ineligible`, `governance_budget`, `voters_removed`, `commission_locked` | GOV-3, GOV-13, MEM-3, MEM-6, IDN-10, IDN-13, GOV-12, GOV-14, GOV-15, GOV-19, GOV-34, GOV-35 |
| farm manager (`docs/farm-manager.md`) | `target_not_implicated`, `target_excluded`, `protected_target`, `control_rate_limited` (rate limit of control operations), the lint `manager_line_too_small`, the notice `manager_spend_paused`, for an incident `incident.dismiss {reason_code, details}` with the refusal `dismiss_reason_required` without a reason code and the disputed attribution `disputed` (D-79); `manager_disabled` and `canceled(manager_disabled)` for operations and tasks of the manager in a tenant with `farm_manager.enabled: false` (MGR-42, D-77); the seat stop reason `human` (kill switch, MGR-15); from v0.2 `use_normal_path`, `chain_too_deep`, the `necessity` lens reasons `evidence_below_threshold`, `covered_by_alarm`, `out_of_scope`, `grant_not_eligible`, `grant_over_budget`, `grant_rate_limited`, `grant_expired_unanswered` | MGR-6, MGR-8, MGR-9, MGR-14, MGR-15, MGR-17, MGR-18, MGR-20, MGR-32, MGR-42 |
| services and monitoring scripts (`docs/task-language.md`, `docs/monitoring.md`) | `service_rate_limited` (the limit of service and script actions per hour, the limit of `routine` runs), `forbidden` (no role permission or the initiator's own permission, a node outside the catalog, D-73), `script_unknown` (the script from the catalog is not in the node's list at the adapter), `approval_required` (the request goes as a card: `start`, `stop` or untrusted text, D-70, D-74), `exec_unknown` for a `monitor_script` delivery | APR-17, APR-18, INJ-15, MON-8, MON-10, DLV-1 |
| scripts (`docs/scripts-and-translator.md`) | `script_invalid`, `script_ref_unknown`, `script_too_large`, `script_untrusted_limit`, `script_superseded`, `no_role_cap` | SCR-2, SCR-5, TRN-4 |
| improvement cycle (`docs/improvements.md`) | `proposal_scope`, `schema_invalid` (and an `apply` refusal by the kind schema, MAN-1), `evidence_mismatch`, `forbidden_change`, `untrusted_direction`, `metric_unknown`, `recently_declined`, `proposal_rate_limited`, the `parked` reasons `score`, `veto_lens_missing`, `review_budget`, `conflict`, `evidence_drift`, the `dismissed` reasons `no_op`, `veto`, `below_min`, `low_score`, the `realization_failed` reasons `object_failed`, `timeout`, the revert reasons `harm_observed`, `superseded`, `other` (the commit line `Botaru-Revert: <id> <reason>`), the mark `self_measured` | PRP-1, PRP-2, PRP-3, PRP-5, PRP-8, PRP-9, PRP-10, PRP-11, PRP-16, PRP-18, PRP-21, PRP-22, COM-4, COM-5, COM-6, COM-7, MAN-1, MAN-5 |
| tool calls (`spec/lifecycles/toolcall.md`) | `no_executor`, `plugin_unavailable`, `lease_expired`, `core_unavailable`, `exec_unknown`, `denied(held)` | DLV-16, FEN-5, HLD-3, APR-7 |
| delivery and sinks | the section "Delivery refusals" below, including `withheld(channel_ceiling)`; for a sink `sink_not_export` (export of a subject's data only to a sink with `export_only`) | DLV-4, DLV-6, DLV-7, DLV-12, SEC-9, SEC-10 |

## Waiting and continuations

### An attempt never waits

An attempt never waits for a person, another task, a timer or an external event. If a bot needs an answer, it answers and ends the attempt; waiting lives only in core records from a closed list, and a waiting task does not hold a lease (CNT-4, D-41).

| What is awaited | Where the waiting lives | What moves the work on | Version |
|---|---|---|---|
| a person's decision on an action | the approval (`pending`) | the core enqueues `action.<a>` in the approval transaction (APR-7) | v0.1 |
| a person's answer to a bot's question | the follow-up window `(conversation, initiator, followup_until)` if the result carried `need_input: true`; otherwise nothing | a reply-to answer or free text within the window becomes a new task with `continues` (CNT-5) | v0.1 |
| a schedule slot | the schedule | the `schedule` loop | v0.1 |
| the end of a child task | the child's record | a `reply` continuation (CNT-1) | v0.1 |
| the end of a campaign | the campaign | a `campaign` continuation | v0.1 |
| a script step button, the end of a step | the script run | the `script` loop, a `script` continuation | v0.1 |
| an external event, a timer | a waiting record from the type's `await` | a continuation on a match or a timeout (CNT-6) | v0.2 |
| a grant of a stronger model | the grant request | the task is back in the queue with the grant (MGR-17) | v0.2 |

The same rule applies to the script steps `confirm`, `action`, `wait` and `await`: a step waits in the run record, not in an attempt.

### Clarification from a person

A clarification is a new task in the same conversation. Example: `bob` writes to the bot in a personal chat "show the service errors for this morning". The bot answers: "Which service: nginx or app-api?", and the task is `completed`. `bob` answers with a reply-to on this message: "app-api". The channel passes `reply_to`, and the core puts `continues: <previous task>` into the new task's record and adds it to `refs`, only if the conversation and the tenant are the same and the person who answered is the initiator of the previous task or has `task.read:<type>` and `data:<mark>`. The adapter gets the result of the previous task as an attachment block; if the previous task had `untrusted_seen`, it passes to the new one. Only the core sets the `continues` field (CNT-5).

Not everyone uses reply-to in a chat, so there is a second path without it. If the task result carried the closed field `need_input: true` (the type declares it in `result_schema` with the mark `x-botaru-read-by: code`), the core opens a follow-up window for the type's `followup_window_s` (30 minutes by default; `0` turns it off) for the pair (conversation, initiator). Free text from `bob` in the same personal chat within this window, without a command and without a reply-to, is linked by the channel code to the question the same way as a reply-to: a new task of the same type with `continues`. The window closes on the first such incoming message, at the end of the period, or with a command; under the bot's question there is the code text "you can simply reply with a message within 30 minutes". In a group chat the window applies only to the initiator, and others answer with reply-to.

### Continuations

A continuation is a task that the core enqueues on a trigger from the closed list `spec/model/continuation-triggers.yaml`: `reply` (the end of a child task from `enqueue_child`), `campaign` (the end of a campaign), `script` (the end of a script step), and from v0.2 `timer` and `await`.

- **Type and goal only from git.** The type and goal template of a continuation come from declarations: the task type's `continuation`, a campaign's `then` from the role's list, a script step, `await.then` and `await.on_timeout`. The model can choose a value only from the declared list, and model arguments and event data do not set the type (CNT-1).
- **In the trigger's transaction.** A continuation is created in the same transaction as its trigger, with a key from the closed list; a double trigger gives one task. If it cannot be enqueued (the tree was canceled, the initiator was deactivated or lost permissions, the budget is exhausted, a hold), the core writes an event with the reason, and the person in `origin` gets a ready code text (CNT-2).
- **Inheritance.** A continuation copies the source's initiator, `origin` and conversation; the parent is the task that ordered it; the depth is 1 more and at most 5; the mark is the maximum of the participants' marks; `untrusted_seen` is an OR over the participants. The core rechecks the initiator's permissions on creation (CNT-3).
- **An action task after approval is not a continuation:** the core enqueues it under APR-7.

Example: the manager `ops-lead` assigns an analyst, through `enqueue_child`, to analyze the night's journal and ends its attempt. When the child is `completed`, the core enqueues for the manager a continuation of the type from its type's `continuation`, with the key `system:cont:reply:<child>` and a `data` attachment with the closed fields of the child's result.

### Waiting for an external event

From v0.2 a task type can declare `await: {event, match, timeout, then, on_timeout}`. The core creates a waiting record at `complete`; a matching event or a timeout enqueues the continuation. The values for the `match` comparison come only from result fields with `x-botaru-read-by: code` and closed fields of the event data; the timeout is mandatory, 7 days by default and no more than the farm ceiling (CNT-6). The `expectation` loop keeps the waiting deadlines.

## Structured result

At `complete` the core checks the attempt's result against the type's full schema; if it fails, that is `output_schema_invalid`, and the attempt is spent. From v0.2 the provider's constrained decoding counts only as a hint: the provider sheet lists the JSON Schema keywords that are removed from the schema projection for the provider, and the core does the check regardless (OUT-2).

Result fields that code reads (route filters, continuations, waits, `summary_fields` summaries, the campaign summary table, script conditions, card texts) are marked `x-botaru-read-by: code` and have only closed leaf kinds: `enum`, `enum_from`, `const`, bounded numbers, `boolean`, strings with a format from the registry. Code does not read or parse the model's free text; the lint rejects a reference from a route, continuation, wait, summary or script to a field without the mark (OUT-1).

```yaml
result_schema:
  type: object
  additionalProperties: false
  required: [answer, verdict]
  properties:
    answer:   {type: string, maxLength: 4000}                      # read by a person
    verdict:  {enum: [cause_found, no_cause, need_access], x-botaru-read-by: code}
    checked:  {type: integer, minimum: 0, maximum: 100000, x-botaru-read-by: code}
```

A worker hands in result files through P1 `file.put`; the core writes a file record with a mark no lower than the task's mark and the type's `result_ttl` expiry (ART-1, ART-3). Each part of the result carries the data origin mark `dataorigin` (`model` or `code`), which the core sets (DLV-13). No record written by the model, or by code from model output, has a person as its subject or contains a field that rates a person (DLV-14).

## Delivery

### At least once

A delivery is a core record "send this content to this recipient". The core creates it in the transaction of the transition that produces it, and it is sent by a channel (E1), a sink (E2) or the monitoring adapter (E5), which takes the delivery through P1 `claim` (DLV-2). Deliveries for E5 are separate delivery kinds: `monitor_alarm` (a change of an alarm episode, `firing` or `resolved`), `monitor_audit_head` (the head of the audit chain of a tenant or of `farm`, at least once a day, MON-3), `monitor_script` (a monitoring system script on a tenant node: a service action with `via: monitor` or a `monitor/script_run` request, MON-10) and `monitor_note` (the code note `service_control_done` after every service or script action, MON-8), from v0.2 `monitor_note` also with a bot's note on a problem (MON-7); there are no people among its recipients, and the monitoring system itself passes the alarm to people (MON-1, MON-7, D-62). A delivery has its own leases, its own retries with a pause and its own final result; its failure does not restart the task (DLV-3). The delivery record carries the target `target: {kind: chat | subject | sink | monitor, id}`, which the core computes from the address; `claim(delivery)` hands the delivery only to a worker recorded at `enroll` for this target, and the worker itself does not compute the addressee (DLV-6). The channel credentials for a chat are not in C1 but in the secret store, by the `credentials_ref` reference of the chat binding. The `monitor_script` delivery is an exception to "at least once": a script changes the world, so it is handed out at most once, and a lease that expired without a result ends it as `dead` with the reason `exec_unknown`. The adapter hands in the script result (exit code and output truncated to 4096 bytes) through P1 `complete`; the core writes it into the task journal as untrusted data (DLV-1, DLV-3). Every task and every delivery is handed out at least once and ends in a final state with an event (DLV-1). The core creates a separate delivery kind `privacy_notice` with the field `notice_version` at a principal's first contact and when the notice version changes in the tenant's `privacy` block, regardless of `ai_disclosure` and the `native` mode (SEC). Delivery states: `planned`, `sending`, `delivered`, `withheld`, `dead` (`spec/lifecycles/delivery.md`).

### Addresses

The delivery target comes only from a closed list; there are no addresses in the task body, the result, bot messages or event data (DLV-6). A route and a schedule set addresses with a `deliver_to` list (the fields are described in `docs/task-language.md`), and the core delivers the proposal digest and proposal cards only to the tenant's `insights.present_to` address (DIG-2).

| Address | Meaning | Condition |
|---|---|---|
| `origin` | back to where the task came from; signed by the core on intake | child tasks and continuations inherit `origin` |
| `group:<group>` | a personal chat to each member of a principal group | each recipient is active and has `data:<mark>` |
| `chat:<binding>` | a bound group chat | summary only (DLV-15); cards and a ceiling above `summary` with `membership_assurance: enforced` (DLV-7) |
| `sink:<sink>` | a sink; the address is built from the base in the sink manifest, the tenant and the delivery id | |
| `{approval_initiator: {action: <action>}}` | the personal chat of the initiator of the approval whose number is in the `approval_id` field of the event data | the approval belongs to the route's tenant, its action is named in the route, it is `executed`, and the initiator is a person |
| monitoring adapter (E5) | the organization's monitoring system; only the core sets this address, for the export of an alarm episode, a monitoring script and a note on a problem, and a route or a schedule does not set it | the delivery holds only numbers, `enum` values, tags and catalog ids, no ids of people; a note only from a role with `outputs: [monitor_note]` on a problem on a node of its own tenant (MON-1, MON-7) |
| `insights.present_to` | the address from the tenant manifest for the proposal digest `proposal_digest` and proposal cards | the mark of each item is checked against the recipient's ceiling: above the ceiling, a group gets only a code heading, and the details go to the personal chats of those who have `data:<class>` (DIG-2) |

For each human recipient the core checks that they are active and have `data:<mark>`, and for a file delivery that the file's mark is not above their permission (ART-3). Campaign items do not create their own deliveries: one card shows the campaign's progress (ORC-10).

### A group chat gets only a summary

An answer to a request from a group chat is split (D-48). Only a summary goes to the chat: a table of the `summary_fields` that code draws from the closed result fields, ready code texts and, if the type declared one, a text part of up to 800 characters marked `dataorigin: model`, only when the task has no `untrusted_seen` and the summary's class is not above the binding ceiling. Everything else (the full text, files, data above the ceiling) goes to the person who asked, in a personal chat, and the ready text "details sent privately" goes to the chat. If a personal message cannot be sent, the chat gets a ready text asking to open a personal chat with the bot, and the content does not go to the chat. A route or schedule delivery to a group chat also carries only a summary (DLV-15, REP-9).

Example. The chat `lab` is bound to `demo` with the ceiling `summary`. `alice` writes to the chat: "@demo_bot how many CTD profiles for September passed the check?". The type `ctd.qc.status.v1` has the class `internal` and `summary_fields: [checked, flagged]`. After `complete`:

- the chat `lab` gets the code table "checked 412, with remarks 7" and the text "details sent privately";
- `alice` gets in a personal chat the model's full answer with the list of stations and the report file, if she has `data:internal`;
- if `alice` has never written to the bot personally and the channel cannot start a personal chat, `lab` gets the ready text "open a personal chat with @demo_bot to get the details", and the model's answer does not go to the chat.

A group chat works only after the operator binds it (P2 `chat.bind`). A binding ceiling above `summary` and cards in a group are allowed only if the channel driver declares `membership_assurance: enforced`: it sees participants joining and stops deliveries to the chat (`withheld`) when an unknown one appears; otherwise P2 `chat.bind` rejects the binding. Incoming messages in a group from principals outside the tenant are ignored, and intermediate statuses go only as ready texts from code (DLV-7). The channel ceiling and the binding ceiling act together, and the lower one applies (SEC-9).

### Cards

The channel draws an approval card from the approval record with a code template; there is no model text on it, and a button holds only `(subject_kind, id, nonce)` (APR-3). Each approval transition creates a `card_update` delivery, which edits the already sent message by its external id. Cards go to approvers' personal chats; to a group only with `membership_assurance: enforced`. The script run card `script_confirm` (`card_update` edits the run's progress) and the run result `script_summary` go only to the initiator's personal chat (D-48).

### Sinks and webhooks

A sink (E2) writes the result to file storage or an external system. It writes idempotently by `delivery.id`, and a file that contains model text gets `ai_generated: true` in its metadata (FEN-7, DLV-13). From v0.2 an outgoing webhook is a kind of sink: the address is built from the base in the sink manifest, and every send carries `delivery.id` in the `webhook-id` header and the sink's signature.

### No streaming

Model text goes to people only after `complete`, after the result has been checked against the type's full schema, and after the outgoing text has been scanned for secrets (INJ-11); there is no streaming and no editing of a message as it is generated. Progress is shown only as a status from a closed list: a stage from the type's `stages` and a percentage in `heartbeat`. Code takes the status text from the message catalog, and the status in the channel is updated at most once per `heartbeat` interval (DLV-11). Outgoing text goes without link previews and without markup, and links outside `link_allowlist` are cut; if a secret is found, the message is not sent, and the tenant admins get an event without content (INJ-11).

### A person knows they are talking to a bot

The channel driver declares `ai_disclosure`: `native` if the external system itself shows that the sender is a bot, or `first_contact`. With `first_contact`, before the first model text, the core creates a delivery of the code text `ai_notice` to each principal at first contact in this channel and to each newly bound chat. The text says that this is a bot, that the answers are written by an external provider's model, where the conversation is stored and what the operator sees. The core keeps the flag `ai_disclosure.ack` with the `ai_notice` delivery time on the (principal, channel) pair and on the chat binding. Deliveries with model text to this recipient wait in `planned` while the flag is absent; if `ai_notice` ended as `dead`, they end as `withheld(ai_notice_dead)`, and on the next incoming message from the principal the core creates `ai_notice` again: the person is not left without answers forever. The `/about` command repeats the same with code text at any time: what this bot is, who the tenant and the operator are, where `/mydata` is. A driver without `ai_disclosure` does not pass `apply` (DLV-12).

### Origin mark

Every part of a result, message and delivery carries the mark `dataorigin`: `model` if the model wrote the text, otherwise `code`; only the core sets it. The channel passes the mark the way the driver declared: `provenance: metadata` (in the message metadata) or `visible_label` (as a visible label). A sink writes `ai_generated: true` into the metadata of a file with model text, and the delivery record keeps the mark. From v0.2 the same mark is part of the artifact provenance block (DLV-13, PRV-2).

### Records do not rate people

No record written by the model, or by code from model output (a finding, a fact, an observation series, a proposal, a review, a report, a result field for code), has a principal of kind `human` as its subject or contains a field that rates a person. Reports count people only as a group or a tenant, and people's decisions (presses, answers) are recorded as audit facts. The schema lint does not allow the kind `human` in the subject field of such records (DLV-14).

### Language, preferences and pace

- **Language.** Code texts (cards, refusals, statuses, notices, `ai_notice`, `privacy_notice`, command answers) come from the message catalog `spec/bindings/channels/messages/<locale>.yaml` by code and the recipient's `locale`. In v0.1 `locale` exists for a principal (set at invitation and with the `/lang` command) and for a chat binding; the skeleton ships the catalog for `en`, and the installation supplies other locales; the order is: the recipient's `locale`, then `en`. From v0.2 `locale` for a schedule and for a tenant, other languages and `reply_locale` to the runtime for the model's answer. There is no translation; argument values and resource ids are not translated. Mandatory catalog keys: command answers `cmd.<command>.*` (including `cmd.lang.*`, `cmd.forbidden.needs`), `cmd.followup.hint`, `notice.ai`, `notice.privacy`, `notice.not_registered`, `notice.principal_left`, `card.copy.initiator`, `card.copy.remind_button`, `card.remind`, `card.remind.rate_limited`, `card.attribution_dispute`, `card.self_measured`; without any of them the catalog does not pass the skeleton CI (DLV-9, REP-12, `docs/messaging.md`).
- **Preferences (v0.2).** `Principal.prefs` (`locale`, `quiet_hours`, `digest`) can only postpone a notice or collect notices into a summary. They do not change the recipient or the ceiling and do not delay task results and cards; the person sets them with a channel command, or an admin does, not the model (DLV-10). The farm does not deliver alarms to people, so their silencing and quiet hours are set by the monitoring system (MON-1).
- **Pace (v0.2).** Deliveries follow the pace declared by the channel driver; waiting for the pace is not a failure, and the delivery stays `planned` (CAP-3).

### Delivery refusals

| Reason | When | What the person gets |
|---|---|---|
| `channel_ceiling` | the mark is above the channel or binding ceiling (SEC-9, DLV-6) | a ready code text; an answer in a group chat goes privately |
| `unknown_member` | there is an unknown participant in the group chat (DLV-7) | deliveries to the chat stop until a check |
| `recipient_inactive`, `no_data_class` | the recipient is deactivated or lacks the permission for the mark | nothing; an event and the audit |
| `held` | a hold with `no_delivery` on the scope (HLD-3) | nothing except messages to admins about the hold itself |
| `ai_notice_dead` | `ai_notice` to this recipient ended as `dead` (DLV-12) | nothing; an event; on the next incoming message from the recipient `ai_notice` is created again |
| `secret_in_outbound` | the channel found a secret in outgoing text (INJ-11); two cases within an hour for one role set an auto-stop (HLD-6) | nothing; an event without content to admins |
| `attempts_exhausted` | the delivery attempts ran out (DLV-4) | the delivery is `dead`, visible among "dead" work |

The number of delivery attempts and the pauses are set by the channel binding within the farm ceiling (by default 5 attempts with a pause from 10 s, doubling up to 10 minutes).

## Example: from a message to an answer

1. `bob` (group `staff`) writes to `@demo_bot` in a personal chat: `/logs why was www.example.org slow yesterday evening?`. The channel checks authenticity and submits an `Activity` (P1 `submit_activity`); the core computes the task key from the update id. The channel code selects the type `ops.investigate.v1` by the command.
2. The core writes the record: `tenant: demo`, initiator `bob`, `origin` his personal chat, the conversation of that personal chat, `max_tier: 1`, `data_mark: raw_logs`, `untrusted_seen: false`, priority class 2, because this is a direct request from a person to a type with `interactive: true`. This is `bob`'s first contact in the channel, and the driver declared `ai_disclosure: first_contact`, so the core creates an `ai_notice` delivery.
3. A worker of the role `ops-reader` takes attempt 1 (P1 `claim`). The model calls `logs_tail(source="nginx-error", since="2026-10-04T16:00:00Z", lines=200)`. The read toolhost on `web1` takes the call; after the first log output the core sets `untrusted_seen: true` with the source `logs/tail`.
4. The worker process crashes (`kill -9`). The lease is not extended, and the `reaper` loop ends the attempt with the reason `lease_expired`: `attempts_spent` is 1, and the task is in `submitted` with a 5 s pause.
5. Lease 2 on another worker finishes the work: `cmd_run(command="service_status", host="web1", service="app-api")`, then `complete` with `verdict: cause_found`. A late `complete` of attempt 1, had it arrived, would have got `fenced`.
6. In the same transaction the core writes `completed`, the event and the delivery of the answer to `origin`. The answer delivery waits in `planned` until `ai_notice` is `delivered`, then the channel sends the text without markup and previews, marked `dataorigin: model`: "From 19:05 to 19:40 nginx did not get answers from app-api in time: 37 timeouts, 412 responses 504. At 19:41 app-api restarted and is again close to its memory limit. One log line contained text that looked like an instruction: it is data and did not affect the service. If it happens again, I can request a restart of app-api; the approver decides".

## What is in which version

| Version | What appears |
|---|---|
| v0.1 | the task record and the write rule, the counters `lease_no`, `attempts_spent`, `runs_counted`, the field `personal_subjects`; enqueue with the order of checks, the `needs` field in a permission refusal, `goal_trust` for a goal from an external program; the tool call dispatch deadline with `no_executor` and `plugin_unavailable`; the action task in `working` with the outcomes `exec_failed` and `exec_unknown`; the downtime window from the last tick and `lease.extend`; the follow-up window after `need_input`; the delivery `target`; `privacy_notice`; `ai_disclosure.ack`, the repeat of `ai_notice` and `/about`; the `locale` of a principal and a chat with the `en` catalog; idempotency keys with tombstones and a closed list of core keys; A2A states without waiting ones; a lease with a token and an epoch, ceilings 120 s, 30 s, 3 attempts, 6 runs; `stall_after`; core downtime without spending an attempt; holder self-stop; work id in an external write; the reason registry and the index of codes of other parts; `waiting_reason` from a closed list; the tenant dispatcher rule for the attempt deadline and the number of attempts; dead work and `redrive` by the operator; clarification by reply-to; a structured result with fields for code; a closed address list, `approval_initiator`, the alarm export delivery for the monitoring adapter E5; summary only to a group chat; result numbers only from a tool answer (`data.describe.v1`, D-72); erasure of a person's data at their request (`erase`, `file.purge`) with the field `personal_subjects`, not waivable for a production tenant (D-76); cards and `card_update`; the sink; no streaming; `ai_notice`; `dataorigin`; the ban on rating people; after the core and the first bot (D-71, D-80): bot messages, inboxes and the return of read entries under MSG-9, the auto-report, the `insights.present_to` address for the proposal digest and cards (D-71); the continuations `reply`, `campaign`, `script` with inheritance; keys for campaigns, scripts, incidents and their closing pass, seat reads, the seat daily review, control and fact selection by the successor; the waits `resource_busy`, `not_released`, `provider_quota`; the deliveries `monitor_script` (at most once) and `monitor_note` with the code note `service_control_done`; the card `script_confirm` and the result `script_summary` to the initiator's personal chat; the seat epoch in the token; the reasons `preempted`, `seat_changed`, `provider_quota`, `by_manager`, `preempt_limit`; the delivery refusal on a `no_delivery` hold |
| v0.2 | waiting for an external event `await` and a timer; model grant; keys for a ballot, a shadow review, the necessity lens and a batch of model grant requests; the waits `group_quota` and `model_grant_pending`; `mode: validate`; the provider's constrained decoding as a hint only; a note on a monitoring problem as a delivery for E5; `locale` of a schedule and a tenant, `reply_locale`; quiet hours and a notice digest; channel pace; an outgoing webhook as a sink; the artifact provenance block |

## Why it works this way

- An attempt that waits for a person would hold a lease, a runtime and money for hours and use up the retry ceilings; a waiting record in the core costs nothing and survives restarts.
- Fencing protects the store but not the outside world, so an external write carries the work id, and an action is executed at most once.
- Core downtime and a provider failure are infrastructure failures, not task failures: if they spent attempts, a five-minute core restart would turn live work into dead work.
- The closed list of core keys makes a double trigger harmless, and a tombstone keeps erasure from "freeing" a key for a second task.
- An address only from a closed list and only a summary in a group chat keep an injection or a model error from sending data to someone other than the person who asked for it.
- A person must know they are talking to a bot, and a record must know that the model wrote the text: the core sets the marks, not the model.

<!-- generated: rules -->
## Rules of this section

Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md](../conformance/rules.md).

| Rule | About | Since | Non-waivable |
|---|---|---|---|
| [CNT-1](../conformance/rules.md#CNT-1) | The continuation type comes only from git | v0.1 | yes |
| [CNT-2](../conformance/rules.md#CNT-2) | A continuation in the trigger's transaction | v0.1 | yes |
| [CNT-3](../conformance/rules.md#CNT-3) | Inheritance for a continuation | v0.1 | yes |
| [CNT-4](../conformance/rules.md#CNT-4) | An attempt never waits | v0.1 | yes |
| [CNT-5](../conformance/rules.md#CNT-5) | A person's answer is linked by a reply | v0.1 | yes |
| [CNT-6](../conformance/rules.md#CNT-6) | Waiting for an event only from the type | v0.2 | yes |
| [CNT-7](../conformance/rules.md#CNT-7) | An action task lives together with its write call | v0.1 | yes |
| [DLV-1](../conformance/rules.md#DLV-1) | At least once, no silent losses | v0.1 | no |
| [DLV-2](../conformance/rules.md#DLV-2) | A transition and its consequences in one transaction | v0.1 | no |
| [DLV-3](../conformance/rules.md#DLV-3) | A delivery failure does not touch the task | v0.1 | no |
| [DLV-4](../conformance/rules.md#DLV-4) | Dead work and redrive | v0.1 | no |
| [DLV-5](../conformance/rules.md#DLV-5) | The channel offset moves after the core answers | v0.1 | no |
| [DLV-6](../conformance/rules.md#DLV-6) | The delivery target comes only from a closed list | v0.1 | yes |
| [DLV-7](../conformance/rules.md#DLV-7) | A group chat only of admitted members | v0.1 | yes |
| [DLV-9](../conformance/rules.md#DLV-9) | Code texts from the message catalog by language | v0.1 | no |
| [DLV-10](../conformance/rules.md#DLV-10) | Notification preferences only defer | v0.2 | yes |
| [DLV-11](../conformance/rules.md#DLV-11) | No streaming of model text | v0.1 | yes |
| [DLV-12](../conformance/rules.md#DLV-12) | A person knows they are talking to a bot | v0.1 | yes |
| [DLV-13](../conformance/rules.md#DLV-13) | A machine-readable origin mark on text | v0.1 | yes |
| [DLV-14](../conformance/rules.md#DLV-14) | Farm records do not assess a person | v0.1 | yes |
| [DLV-15](../conformance/rules.md#DLV-15) | A group chat gets only a summary | v0.1 | yes |
| [DLV-16](../conformance/rules.md#DLV-16) | A tool call does not wait for an executor beyond its deadline | v0.1 | yes |
| [FEN-1](../conformance/rules.md#FEN-1) | A lease with a growing number and a token | v0.1 | no |
| [FEN-2](../conformance/rules.md#FEN-2) | A stale token gets fenced without writes | v0.1 | yes |
| [FEN-3](../conformance/rules.md#FEN-3) | Ceilings of leases, attempts and runs | v0.1 | yes |
| [FEN-4](../conformance/rules.md#FEN-4) | Cancellation through heartbeat, work without a lease is rejected | v0.1 | no |
| [FEN-5](../conformance/rules.md#FEN-5) | Core downtime does not spend an attempt | v0.1 | yes |
| [FEN-6](../conformance/rules.md#FEN-6) | Self-stop of a holder by its own clock | v0.1 | yes |
| [FEN-7](../conformance/rules.md#FEN-7) | An external write carries the work id | v0.1 | yes |
| [IDM-1](../conformance/rules.md#IDM-1) | Task idempotency key | v0.1 | yes |
| [IDM-2](../conformance/rules.md#IDM-2) | A closed list of core keys | v0.1 | no |
| [IDM-3](../conformance/rules.md#IDM-3) | A repeat of holder operations gives the same answer | v0.1 | no |
| [IDM-4](../conformance/rules.md#IDM-4) | An event is unique by source and id, which the core sets | v0.1 | no |
| [IDM-5](../conformance/rules.md#IDM-5) | One channel update gives one task | v0.1 | no |
| [OUT-1](../conformance/rules.md#OUT-1) | Result fields read by code are of closed kinds | v0.1 | yes |
| [OUT-2](../conformance/rules.md#OUT-2) | Provider constrained decoding is only a hint | v0.2 | no |
