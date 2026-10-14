# Tasks and delivery

## In short

This document follows a task after it is submitted: the record the core writes, the order of enqueue checks, how an idempotency key prevents a second task, how a lease and fencing make a late worker harmless, which reasons spend an attempt, where waiting lives, and how the answer reaches people. The idea throughout: everything that permissions, addresses and retries depend on is decided by core code from store records, not by the model or the sender. An attempt never waits; waiting lives in core records, and the core enqueues the continuation (D-41). A delivery is handed out at least once, its address comes only from a closed list, and a group chat gets only a summary (D-48).

The task language (body, catalog, types, manifests) is in [docs/task-language.md](task-language.md), permissions and approvals in [docs/permissions.md](permissions.md), and the transition tables in `spec/lifecycles/task.md`, `lease.md`, `toolcall.md` and `delivery.md`.

## How it works

### The task record

A task is a body, which the sender submits, and a record, which only the core writes; the schema rejects a request carrying any record field ([IDN-1](../conformance/rules.md#IDN-1)). The record holds:

- identity: `id` (never reused), `tenant` from the credentials, `type`;
- on whose behalf: `initiator` (a person, a `service` program or `system`), the initiator set (plus the initiators of everything the attempt read, PRM-6), `enqueued_by`;
- where to answer: `origin` (signed by the core at intake), `conversation`, and `continues` for a follow-up (CNT-5);
- the tree: `parent`, `root`, `depth` (at most 5), `correlationid`, `refs`;
- trust: `max_tier`, `data_mark` fixed at enqueue (PRM-4, PRM-5), `untrusted_seen` with its source, only ever set (INJ-13), and `personal_subjects` for erasure;
- dispatch: `to_group`, `priority_class`, `fairness_key`, `scope`, `state`, `waiting_reason`, `run_at`, and the counters `lease_no`, `attempts_spent`, `runs_counted`;
- links: `approval_id`, `campaign`, `campaign_item`, `script_run`, `script_step`, `acting_bot`, `bot_epoch`, `seat_epoch`, `incident`, `trace` (OBS-2), and from v0.2 `grant`, `takeover_of`.

A goal from an external program sets `untrusted_seen` with source `goal:service` unless the type declares `goal_trust: authored`. A route task for a monitoring problem of the tenant's own nodes with a trigger from `monitor_triggers` is not flagged, and its record shows source `monitor` (D-82).

The body and result are content with a retention class (RET-1); events, the audit and journals carry only metadata (AUD-2). A deleted task leaves a tombstone (RET-3).

### Enqueue

Tasks come from a person in a channel (P1 `submit_activity`, initiator by verified identity), an external program (P1 `enqueue` with its own key, D-47), a `job` worker (P1 `enqueue` with its attempt token), a model (P3 `enqueue_child`), and from the core: schedules, event and inbox routes, auto-reports, approvals, redrives, continuations, campaign items, script steps and incidents. Everything the core creates has initiator `system`, the parent's initiator or the approval's initiator, and a core key.

The core checks in this order, and the first failure stops it. A refusal creates nothing, does not take the key, goes to the audit, gives a person a code text and the model a short "no permission" (PRM-8). A permission refusal carries `needs: [<permission>]`, filled by code, and the person's text names the group that grants it and who grants it.

1. The core is ready, otherwise `not_ready` with `retry_after_s` (OBS-8).
2. The tenant equals the credentials' tenant, otherwise `tenant_mismatch` and an alarm (TEN-2).
3. The request schema: no record fields, no worker field, no `system:` key, no `action.*` or unknown type (`input_schema_invalid`, DSP-4, APR-5).
4. The key ([IDM-1](../conformance/rules.md#IDM-1)): the same initiator and body hash (RFC 8785, `spec/hashing.md`) returns the existing task with `existing: true`; another body gets `idempotency_conflict`; a deleted task's key gets `gone`.
5. The initiator is active (`person_inactive`).
6. Holds come before permissions: `no_enqueue` gives `held`, except erasure and export (HLD-3).
7. Permissions: every initiator has `task:<type>` or a family permission (PRM-7); a worker also needs the type in `may_enqueue`, and depth, children, tree size, tier and group within limits (PRM-6, DSP-4).
8. The mark: the maximum of the type's class and the parent's; the initiator needs `data:` for it. The distrust flag is set; a memory-writing type gets `memory_untrusted` under the flag (MEM-3).
9. References: own objects only, one `ref_invalid` for foreign and missing (TEN-7).
10. Parameters against `params_schema`, defaults filled (INJ-1).
11. Tenant ceilings: `over_capacity` with `retry_after_s` (CAP-1).
12. The budget reserve in every applicable limit (BUD-1).
13. The record, the `task.submitted` event, the reserve and the key in one transaction (DLV-2).

From v0.2 `mode: validate` runs the same checks and writes nothing (PRM-11).

### Idempotency keys

A key, unique per (tenant, key) (D-43), absorbs a network retry, a channel retry or a second loop copy. A tombstone keeps the key taken after the task is deleted, so erasure does not free it for a second task. The core computes a channel message's key from (channel, update id), and the channel moves its offset only after the core answers ([DLV-5](../conformance/rules.md#DLV-5)).

Keys starting with `system:` are set only by the core from a closed list ([IDM-2](../conformance/rules.md#IDM-2)). The route name or the slot is part of the key, so a trigger firing twice gives one task, while two routes on one event give two. The list: `schedule:<schedule>:<slot>`, `approval:<id>`, `event:<route>:<source>:<id>`, `activity:<channel>:<update>`, `redrive:<id>:<n>`, `child:<parent>:<body hash>`, `msg:<attempt>:<n>`, `wake:<route>:<window>`, `report:<task>:<route>`, `review:<proposal>:<lens>`, `cont:<trigger>:<source>`, `script:<run>:<step>`, `campaign:<campaign>:<n>`, `incident:<class>:<subject>:<window>`, `incident:<incident>:close`, `control:<epoch>:<n>`, `inspect:<basis>:<n>`, `daily:<tenant>:<day>` (MGR-41), `ingress:<source>:<delivery>` (ING-3); from v0.2 `escalation:<attempt>`, `takeover:<task>`, `grants:<tenant>:<window>` (MGR-18), `review:<proposal>:<lens>:shadow` (COM-10) and `necessity:<escalation>` (COM-13).

Example: `report-client` of `demo` enqueues a nightly report with key `nightly-2026-10-05`. The answer is lost, it retries with the same body and gets the same task with `existing: true`. A different body would get `idempotency_conflict`. Half a year later, after the body expired, the same key gets `gone`, not a new task.

### Lifecycle and waiting

The states follow A2A: `submitted`, `working`, `completed`, `failed`, `canceled`, `rejected`. A final task is never restarted; a retry after the end is a new task with a `refs` link. There are no `input_required` or `auth_required` states ([CNT-4](../conformance/rules.md#CNT-4)): a bot that needs something from a person answers with a question and ends.

Only the dispatcher moves a task out of `submitted`; until then it carries a `waiting_reason` (see [docs/coordination.md](coordination.md)). Under pressure no work is dropped and no attempt is spent; a task nobody took by `start_by` raises `alarm.task.unclaimed` (DSP-8).

### Attempts, lease and fencing

An attempt is one dispatch of a task to a worker. `claim` creates a lease with `lease_no` one higher and the token `(epoch, work_id, lease_no, worker_id)` ([FEN-1](../conformance/rules.md#FEN-1)). The holder extends it with `heartbeat`, ends with `complete` or `fail`, and returns work with `release`. Tool calls and deliveries use the same mechanism. One attempt lease is one runtime run, with E4 id `<task id>:<lease_no>`.

| Counter | Grows on | Does not grow on | Ceiling |
|---|---|---|---|
| `lease_no` | every `claim` | nothing resets it | none; it is part of the token |
| `attempts_spent` | outcomes that spend by the registry: `lease_expired`, `runtime_error`, `model_error`, `attempt_timeout`, `output_schema_invalid`, `stalled` | `held`, `core_unavailable`, `worker_drain`, provider waits, preemption, seat change | `max_attempts`, at most 3 |
| `runs_counted` | a run in which the model answered at least once | runs that ended before the first model answer; `job` tasks | `max_runs`, at most 6 |

So two `provider_unavailable`, one `held` and three `lease_expired` give `lease_no` 6, `attempts_spent` 3 and `failed(attempts_exhausted)`.

**Fencing** ([FEN-2](../conformance/rules.md#FEN-2)). A process can sleep through a garbage collection pause or network outage longer than its lease and wake without knowing. Every mutating call carries the token, and the store answers `fenced` and writes nothing for an old lease, store epoch or seat epoch. Acceptance: 10,000 tasks on 8 competing workers give zero duplicates and zero losses; after `kill -9` the second attempt finishes and the first one's late `complete` is fenced. Children, tool calls, messages and action requests of a canceled or fenced attempt are rejected and their results go nowhere ([FEN-4](../conformance/rules.md#FEN-4)); a holder learns of cancellation from the `heartbeat` answer.

**Deadlines** ([FEN-3](../conformance/rules.md#FEN-3)). The type sets them and the farm caps them: lease 120 s, heartbeat 30 s, 3 attempts, 6 runs. A tenant dispatch rule can at most double the attempt deadline and only lower `max_attempts` (DSP-3).

```yaml
timeouts:
  start_by: 10m      # nobody took it: alarm.task.unclaimed
  lease: 120s
  heartbeat: 30s
  attempt: 5m
  stall_after: 3m    # no progress: the attempt ends stalled
  deadline: 30m      # the whole task with all attempts
retry:
  max_attempts: 3    # 1 for a non-idempotent action
  max_runs: 6
  backoff: {initial: 5s, factor: 2, max: 60s}
```

**No progress.** An attempt with no completed P3 or P4 call and no progress change for longer than `stall_after` ends `stalled`: the attempt is spent, the task retried, and `alarm.task.stalled` raised (CNT-4). A call still queued does not count as progress.

**Tool calls do not wait for a worker.** A call not taken within its `start_by` (30 s at most) ends `error(no_executor)`, and with no live toolhost or a `down` plugin it gets `error(plugin_unavailable)` at once, so the model sees an error instead of the attempt timing out ([DLV-16](../conformance/rules.md#DLV-16)).

**Action task.** An `action.<a>` task is created in the approval transaction directly in `working`, without an attempt or lease: its work is the single write call. `done` gives `completed`; `error` or `denied(held)` gives `failed(exec_failed)`; an unknown outcome gives `failed(exec_unknown)` (CNT-7, APR-7).

**Core downtime spends nothing** ([FEN-5](../conformance/rules.md#FEN-5)). The downtime window runs from the last background loop tick to readiness. On readiness the core extends, once and under a lock, every lease that was active at the start of the window. A lease the holder still does not extend ends `core_unavailable`: no attempt, no run counted, no pause. A read call gets `error(core_unavailable)`, a write call `exec_unknown`, and a delivery returns to `planned`. Otherwise a five-minute core restart would turn live work into dead work.

**The holder stops itself** ([FEN-6](../conformance/rules.md#FEN-6)). Every holder outside the core counts its lease on its own monotonic clock from `lease_remaining_s` and stops at the deadline minus a margin (10 s), writing nothing more except `fail` or `release`. A toolhost cancels the plugin over MCP and kills it after `kill_after`. Check: a `job` worker under SIGSTOP for 3 minutes makes no mutating call and no external write after SIGCONT.

**The fencing boundary** ([FEN-7](../conformance/rules.md#FEN-7)). The store checks tokens, but systemd or a foreign API does not. So every external write carries a work id shared by all attempts: `approval_id` (or `<approval_id>:<n>` for a campaign item), `delivery.id` for a sink, the `webhook-id` header for a webhook. A `job` type that writes outside declares `side_effects: external` and passes `work_id`, or gets `max_attempts: 1`. `card_update` edits the sent message rather than sending a new one.

### Reasons and retries

One registry, `spec/model/reasons.yaml`, holds every reason for the end of an attempt, a refusal or a wait, and decides whether it spends an attempt, whether the task is retried and after what pause. A worker passes a code from it in `fail`; records hold no free-text reasons.

| Reason | Spends | Retry | Without a retry |
|---|---|---|---|
| `lease_expired`, `runtime_error`, `model_error`, `attempt_timeout`, `output_schema_invalid`, `stalled` | yes | with `backoff` | `failed(attempts_exhausted)` |
| `worker_drain`, `core_unavailable`, `preempted`, `seat_changed` | no | at once | |
| `provider_unavailable`, `provider_rate_limited`, `provider_quota` | no | with a pause and jitter until `deadline` (CAP-2, GW-10, GW-12) | `failed(deadline_exceeded)` |
| `provider_config` | no | waits for a fix; alarm (GW-13) | `failed(deadline_exceeded)` |
| `held` | no | after the hold is removed | |
| `awaiting_model_grant`, `grant_revoked` (v0.2) | no | after the decision (MGR-17) | |
| `provider_spend_cap`, `budget_exhausted`, `deadline_exceeded`, `non_retryable` | no | no | `failed` |
| `exec_failed`, `exec_unknown` (action tasks) | | no; a new request is needed | `failed` |
| `initiator`, `admin`, `rights_revoked`, `person_inactive`, `by_manager`, `preempt_limit` | no | no | `canceled` |
| enqueue refusals (`no_group_cap`, `over_capacity`, `held`, ...) | | | `rejected` |
| `idempotency_conflict`, `gone`, `not_ready`, `fenced` | | | an answer to the call; nothing changes |

Only one layer retries a provider error: the P4 client at most once, the runtime never, then the core by the type's policy (GW-12).

**Dead work.** A task out of attempts, runs or deadline, and a delivery out of attempts, appear in P2 `deadletter.list`. P2 `task.redrive` and `delivery.redrive` are operator operations through `farmctl` (PRM-14): they create new work with key `system:redrive:<id>:<n>` and recheck permissions. An `action.*` task is never redriven ([DLV-4](../conformance/rules.md#DLV-4), APR-7). A delivery failure does not touch its task ([DLV-3](../conformance/rules.md#DLV-3)).

The same registry holds the codes of every other part, each explained in its own document: [docs/coordination.md](coordination.md) (dispatch, campaigns, scripts), [docs/messaging.md](messaging.md), [docs/permissions.md](permissions.md), [docs/operations.md](operations.md), [docs/models-and-money.md](models-and-money.md), [docs/bots.md](bots.md), [docs/farm-manager.md](farm-manager.md), [docs/improvements.md](improvements.md) and [docs/task-language.md](task-language.md) (services). Two naming conventions hold across them: `provider_rate_limited` comes from the model provider and `service_rate_limited` from the farm's own limiters, and a permission refusal is always `forbidden`.

### Waiting and continuations

An attempt never waits for a person, a task, a timer or an event; a waiting task holds no lease (CNT-4, D-41). Each wait has a core record and a mover:

| Waiting for | Lives in | Moved on by |
|---|---|---|
| a decision on an action | the approval | the core enqueues `action.<a>` (APR-7) |
| a person's answer | the follow-up window, if the result had `need_input: true` | the answer becomes a new task with `continues` (CNT-5) |
| a schedule slot | the schedule | the `schedule` loop |
| a child, a campaign, a script step | its record | a `reply`, `campaign` or `script` continuation |
| an event or timer (v0.2) | a waiting record from the type's `await` | a continuation on match or timeout (CNT-6) |
| a stronger model (v0.2) | the grant request | the task returns with the grant (MGR-17) |

**Clarification.** `bob` asks "show the service errors for this morning"; the bot answers "Which service: nginx or app-api?" and the task completes. `bob` replies to that message with "app-api". The core gives the new task `continues: <previous>` if the conversation and tenant match and the person is the initiator or may read the previous task; the previous result comes as an attachment, and its flag carries over ([CNT-5](../conformance/rules.md#CNT-5)). Since not everyone uses reply-to, a result with `need_input: true` (a code-read field) also opens a follow-up window of `followup_window_s` (30 minutes) for that person in that conversation: their next free-text message becomes the answer, and a code hint says so.

**Continuations** come only from the triggers in `spec/model/continuation-triggers.yaml`: `reply`, `campaign`, `script`, and from v0.2 `timer` and `await`. Their type and goal come only from declarations in git, never from model arguments or event data ([CNT-1](../conformance/rules.md#CNT-1)). They are created in the trigger's transaction with a core key, and if one cannot be enqueued the person gets a code text ([CNT-2](../conformance/rules.md#CNT-2)). They inherit initiator, `origin` and conversation, add 1 to depth, take the maximum mark and OR the flags, and the initiator's permissions are rechecked ([CNT-3](../conformance/rules.md#CNT-3)). An action task after approval is not a continuation.

### Structured result

At `complete` the core checks the result against the type's full schema; a failure is `output_schema_invalid` and spends the attempt. From v0.2 a provider's constrained decoding is only a hint, and the check runs regardless ([OUT-2](../conformance/rules.md#OUT-2)). Fields that code reads (route filters, continuations, waits, summaries, campaign tables, script conditions, cards) are marked `x-botaru-read-by: code` and have only closed leaves; code never parses free text ([OUT-1](../conformance/rules.md#OUT-1)).

```yaml
result_schema:
  type: object
  additionalProperties: false
  required: [answer, verdict]
  properties:
    answer:   {type: string, maxLength: 4000}               # read by a person
    verdict:  {enum: [cause_found, no_cause, need_access], x-botaru-read-by: code}
    checked:  {type: integer, minimum: 0, maximum: 100000, x-botaru-read-by: code}
```

Result files go through P1 `file.put` with a mark no lower than the task's (ART-1, ART-3). Every part carries `dataorigin`, set by the core ([DLV-13](../conformance/rules.md#DLV-13)). No record written by the model, or by code from model output, has a person as its subject or rates a person; reports count people only as a group ([DLV-14](../conformance/rules.md#DLV-14)).

### Delivery

A delivery is a core record "send this content to this recipient", created in the transaction that produces it and taken through `claim` by a channel (E1), a sink (E2) or the monitoring adapter (E5) (DLV-2). It has its own leases, retries and final state, and its failure does not restart the task. Every task and delivery is handed out at least once and ends with an event ([DLV-1](../conformance/rules.md#DLV-1)). The exception is `monitor_script`, which changes the world: it is handed out at most once, and an expired lease ends it `dead(exec_unknown)`. The E5 kinds are `monitor_alarm`, `monitor_audit_head` (MON-3), `monitor_script` (MON-10) and `monitor_note` (MON-8, MON-7); none has people among its recipients (MON-1). States: `planned`, `sending`, `delivered`, `withheld`, `dead`.

**Addresses** ([DLV-6](../conformance/rules.md#DLV-6)). There are no addresses in bodies, results, messages or event data. The record's `target` is computed by the core, and `claim` hands a delivery only to a worker enrolled for that target. The closed list:

| Address | Meaning |
|---|---|
| `origin` | back where the task came from; children and continuations inherit it |
| `group:<group>` | a private chat to each active member with `data:` for the mark |
| `chat:<binding>` | a bound group chat: summary only |
| `sink:<sink>` | a sink, address built from its manifest |
| `{approval_initiator: {action: <a>}}` | the private chat of the person who requested an executed approval of that action |
| monitoring adapter | set only by the core, for alarms, scripts and notes (MON-1, MON-7) |
| `insights.present_to` | the proposal digest and cards (DIG-2) |

Each human recipient must be active and have `data:` for the mark, and for a file the file's mark (ART-3).

**A group chat gets only a summary** ([DLV-15](../conformance/rules.md#DLV-15), D-48). The chat gets a code table of `summary_fields`, code texts and, if declared, a model text of up to 800 characters only when the task has no `untrusted_seen` and the class fits the binding. The full answer and files go to the asker privately, and the chat gets "details sent privately". If a private message is impossible, the chat gets a request to open one, never the content. A group chat works only after the operator binds it; a ceiling above `summary` and cards need a driver with `membership_assurance: enforced`, which withholds deliveries when an unknown member appears ([DLV-7](../conformance/rules.md#DLV-7)). The channel and binding ceilings combine, and the lower wins (SEC-9).

Example: `alice` asks in `lab` (ceiling `summary`) how many September CTD profiles passed. The chat gets "checked 412, with remarks 7" and "details sent privately"; alice gets the full answer and file privately if she has `data:internal`.

**Cards.** The channel draws approval cards from the record with a code template; a button holds only `(subject_kind, id, nonce)` (APR-3), and each transition edits the card with `card_update`. Script cards go only to the initiator's private chat.

**Sinks and webhooks.** A sink writes idempotently by `delivery.id`, marking files with model text `ai_generated: true`. From v0.2 an outgoing webhook is a sink (DLV-17): exactly the manifest's `https://` base, a host re-resolved at every send and refused if it is farm, loopback, private or link-local, a signed POST in the Standard Webhooks format, no redirects, and file references instead of bytes.

**No streaming** ([DLV-11](../conformance/rules.md#DLV-11)). Model text goes out only after `complete`, the schema check and the secret scan (INJ-11). Progress is a stage from the type's `stages` and a percentage, drawn by code. Text goes without previews or markup, links outside `link_allowlist` are cut, and a found secret stops the message and notifies admins without content.

**A person knows it is a bot** ([DLV-12](../conformance/rules.md#DLV-12)). A driver declares `ai_disclosure`: `native` or `first_contact`. With `first_contact`, the core sends the code text `ai_notice` before the first model text to each new principal and chat, and model deliveries wait in `planned` until it is delivered. If it ends `dead`, they end `withheld(ai_notice_dead)` and the next incoming message retries the notice. `/about` repeats it any time. `privacy_notice` is a separate delivery at first contact and when the notice version changes.

**Language, preferences and pace.** Code texts come from `spec/bindings/channels/messages/<locale>.yaml` by the recipient's `locale`, falling back to `en` ([DLV-9](../conformance/rules.md#DLV-9)); the mandatory keys are listed in [docs/messaging.md](messaging.md). From v0.2 the order is the recipient's, the schedule's, the tenant's locale, then `en`, and the core writes the choice into `reply_locale` so the model does not pick its language ([DLV-18](../conformance/rules.md#DLV-18)); nothing is translated (D-90). From v0.2 `Principal.prefs` can postpone or batch `deferrable` deliveries but never results, cards or notices ([DLV-10](../conformance/rules.md#DLV-10)), and channel pace keeps deliveries `planned` without spending attempts (CAP-3).

**Refusals.** `channel_ceiling` (the answer goes privately), `unknown_member` (the chat stops until checked), `recipient_inactive` and `no_data_class` (an event only), `held` (a `no_delivery` hold), `ai_notice_dead`, `secret_in_outbound` (two in an hour stop the role, HLD-6), and `attempts_exhausted` (dead work). A binding sets attempts and pauses within the farm ceiling: 5 attempts, from 10 s doubling to 10 minutes.

### Versions

v0.1 carries everything above except: `await` and timers, model grants and their keys, shadow and necessity review keys, `group_quota` and `model_grant_pending`, `mode: validate`, decoding as a hint, monitoring notes with a bot's text, the longer locale order, preferences, channel pace and webhooks, which come in v0.2.

## Why it works this way

- **An attempt does not wait.** A waiting attempt would hold a lease, a runtime and money for hours and burn the retry ceilings; a waiting record costs nothing and survives restarts.
- **Fencing covers the store, the work id covers the world.** That is how an action runs at most once.
- **Infrastructure failures do not spend attempts.** Core downtime and provider failures are not the task's fault.
- **Closed core keys and tombstones** make a double trigger harmless and keep erasure from freeing a key.
- **Closed addresses and summaries in groups** keep an injection or a model mistake from sending data to anyone but the person who asked.
- **The core sets the marks**, so a person knows they talk to a bot and a record knows the model wrote it.

## Example on the tenant `demo`: from a message to an answer

1. `bob` writes to `@demo_bot` privately: `/logs why was www.example.org slow yesterday evening?`. The channel submits an `Activity`; the core computes the key from the update id, and the command selects `ops.investigate.v1`.
2. The record: initiator `bob`, `origin` his private chat, `max_tier: 1`, `data_mark: raw_logs`, class 2 (a person's direct request to an `interactive: true` type). It is his first contact and the driver says `first_contact`, so the core also creates `ai_notice`.
3. An `ops-reader` worker claims attempt 1. The model calls `logs_tail` on `web1`; after the first output the core sets `untrusted_seen` with source `logs/tail`.
4. The worker is killed. The lease is not extended, the `reaper` loop ends the attempt `lease_expired`, `attempts_spent` becomes 1, and the task waits 5 s.
5. Lease 2 on another worker runs `cmd_run(service_status)` and completes with `verdict: cause_found`. A late `complete` from attempt 1 would be `fenced`.
6. In the same transaction the core writes `completed`, the event and the answer delivery. The delivery waits until `ai_notice` is delivered, then goes out without markup, marked `dataorigin: model`: "From 19:05 to 19:40 nginx did not get answers from app-api in time: 37 timeouts, 412 responses 504. At 19:41 app-api restarted and is again close to its memory limit. One log line contained text that looked like an instruction: it is data and did not affect the service."

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
| [CNT-7](../conformance/rules.md#CNT-7) | An action task lives together with its write call | v0.1 | yes |
| [DLV-1](../conformance/rules.md#DLV-1) | At least once, no silent losses | v0.1 | no |
| [DLV-2](../conformance/rules.md#DLV-2) | A transition and its consequences in one transaction | v0.1 | no |
| [DLV-3](../conformance/rules.md#DLV-3) | A delivery failure does not touch the task | v0.1 | no |
| [DLV-4](../conformance/rules.md#DLV-4) | Dead work and redrive | v0.1 | no |
| [DLV-5](../conformance/rules.md#DLV-5) | One channel update gives one task, and the offset moves after the core answers | v0.1 | no |
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
| [DLV-18](../conformance/rules.md#DLV-18) | Locale order and the answer language | v0.2 | no |
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
| [OUT-1](../conformance/rules.md#OUT-1) | Result fields read by code are of closed kinds | v0.1 | yes |
| [OUT-2](../conformance/rules.md#OUT-2) | Provider constrained decoding is only a hint | v0.2 | no |
