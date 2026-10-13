# Farm operations

## In short

This section describes how the farm lives on ordinary days and on bad days: what the operator sees, who learns about trouble and how, how to stop part of the farm or the whole farm, how long data is kept and how it is erased, what to do about backups, overload, failures of external dependencies, time and the rollout of a new core version. The main idea: everything that security and data safety depend on during a bad day is held by core code and by records in the store, not by people's memory and not by the model. Code raises alarms, and the organization's monitoring system delivers them to people and also checks core liveness itself (`docs/monitoring.md`); stopping is a recorded "hold" object; early erasure goes through the erasure log, which survives a restore from backup. The section relies on the task and delivery model (task, lease, attempt, delivery), on permissions (C2 checks holds first) and on the model gateway P4 (retries and the circuit breaker). The recipes at the end are written as sequences of port operations, without the commands of a particular implementation.

## What the operator sees

### One trace per task tree

A task that a person enqueued can produce child tasks, tool calls, model calls, approvals and deliveries. All of this is one task tree with a root (`root`): the first task from which the rest started. To analyze any case with one query, the core links every event to the root and to its direct cause (OBS-2):

| Field | Value | Who writes it |
|---|---|---|
| event `correlationid` | id of the task tree root | the core |
| event `causationid` | id of the direct cause: the parent task; the approval (for `action.*`); the event (for a route task); the incoming message (for a channel task); the schedule slot | the core |
| task record field `trace` | W3C `traceparent` of the root: all tasks of the tree have one trace id | the core |
| trace spans | attempt, model call in P4, tool call, delivery, decision on an approval | the core and the edges |
| context propagation | `WorkItem.trace` for the toolhost, channel and sink; `_meta.traceparent` in a plugin call | toolhost |

The core writes the `correlationid` value (the root id) already in v0.1: the tree limits are counted by it (MSG-11, ORC-8). The `trace` field, `causationid` and spans are written from v0.1 too (OBS-2, D-94). The agent runtime does not need the trace context: the core knows the attempt by its P3 and P4 handle and assigns the span parent itself.

Tracing context that arrives from outside (from an external program, a runtime, a plugin) decides nothing (OBS-3). The tenant, principal, task and rights are not taken from it. `baggage` and `tracestate` from outside are dropped at the core boundary, because W3C Baggage carries arbitrary key-value pairs, and a plugin or a log collector would record them. A foreign `traceparent` is kept only as a link to the foreign trace.

### Telemetry without content

The rule "audit and events hold only metadata" (AUD-2) applies to everything the farm hands out for observation: traces, metrics, process logs and health reports (OBS-1). They contain only ids, sizes, hashes, times, codes and names from closed lists. The OpenTelemetry conventions for GenAI include content attributes (input and output messages, system instructions, tool call arguments and results), and ready-made instrumentation enables them with one setting. Therefore:

- the allowlist of attributes and names lives in the binding `spec/bindings/telemetry/otel.md` with a pinned version of the conventions, and the farm's telemetry collector drops everything outside the list;
- metric labels come only from closed sets: tenant, task type, role, plugin, tool, reason, state. Ids of people, chats and external systems are not labels: this is both privacy and the number of series;
- runtime telemetry goes only through the E4 adapter or does not go at all;
- a log that may contain content (a runtime transcript, plugin stderr, debug output of a channel) is a content store of class `content` with a retention period and erasure, and only metadata leaves it.

A debug log with prompts is not enabled in production. Analysis goes by the session transcript (it is already a content store with a retention period) or by replaying the case on a test tenant.

### Metrics

The metrics catalog (v0.2) is a contract for dashboards and alarms that survives a change of implementation. It lives in `spec/bindings/telemetry/metrics.yaml` and covers the four monitoring signals: latency, traffic, errors and saturation. It has 14 metrics with labels from closed sets: queue (`botaru.queue.depth`, `botaru.queue.oldest_age_s`, `botaru.claim.wait_s`), outcomes and duration (`botaru.task.outcome`, `botaru.attempt.duration_s`), model and tools (`gen_ai.client.operation.duration`, `gen_ai.execute_tool.duration`, `botaru.model.tokens`), budget and approvals (`botaru.budget.used_ratio`, `botaru.approval.count`), deliveries (`botaru.delivery.outcome`), liveness (`botaru.worker.last_seen_age_s`, `botaru.loop.last_tick_age_s`) and holds (`botaru.hold.active`). The check "every catalog metric is in the output" runs as a black box. Where a core observation series coincides with a catalog metric, they have the same name (OBV-5).

### P2 `status` and `doctor`

The operator sees the state of the farm with two P2 operations. `status` gives a summary, `doctor` checks consistency and names the mismatches. Both show only metadata:

- active holds with scope, effects, reason, deadline and author (HLD-5);
- the rights and scope of every farm account in the monitoring system, the freshness of the audit head in its history, the monotonicity of the heads' `seq` and the retention period of the witness history (MON-4, MON-3);
- for each tenant: whether there is a `monitor` worker in `active` and the age of its `last_seen`, the age of the last successful export to the monitoring system `monitor_export_age_s`, deliveries for E5 in `planned` older than 15 minutes (OBS-11, MON-11);
- the tick age of every background loop and missed ticks, the `last_seen` of workers (OBS-4); during a core drain, the drain deadline and a warning if it is exceeded (LCY-6);
- the three health indicators (OBS-9) and the free space of the store (CAP-5);
- open erasure requests and their outcomes per store (RET-7);
- the age of the latest backup of every storage location, the date and outcome of the last restore check from the reports of the backup process, and whether the erasure log backup is older than the store backup (RET-8);
- the age of the last usage reconciliation for every provider workspace (OBS-12) and an open spend gap after a restore with halved limits (RET-6);
- the applied commit and the hash of the manifest set against the commit named by the operator (MAN-5);
- the workspace caps at the provider and the outcome of the daily probe (GW-9), lack of money in the `review` line for reviews (BUD-13), a warning about an approaching model retirement date (`retires_at` of the model profile), the table of fixed costs per tenant (BUD-15);
- for a tenant where the improvement cycle is disabled, the line "improvement cycle disabled" with the reasons (INS-6), and who covers each lens of the commission (COM-7);
- per tenant: the farm manager opt-out (`Tenant.spec.farm_manager.enabled: false`, the line "manager disabled", D-77), the farm manager pause for money reasons (`paused_in`, MGR-14), "shadow disabled" (COM-10), and from v0.2 "commission locked" (`commission_locked`, GOV-35) (OBS-11).

### Tenant debug journal

An operator with the right `admin:debug` can read a tenant's attempt journals and transcripts to analyze a case. This is a read of the tenant's content, and the tenant decides how to treat it with the field `Tenant.spec.operator_access.debug_journal` with the values `deny` (the operator does not read journals; analysis only by metadata or on a test tenant), `notify` (reading is allowed; every read gives an audit record and a code message to the tenant admins: who, when, which task) and `allow` (reading is allowed with an audit record, without a message). The default is `notify`: an external client learns about every look into their data, and the operator does not lose the ability to analyze an incident. The rule and the place of the field in the processing description belong to `docs/privacy.md`.

## Liveness and health

### Workers: `last_seen`

A heartbeat belongs to a lease, so an idle worker waiting for work sends no heartbeat. The core sees worker liveness differently: a worker has `last_seen`, the time of its last P1 call of any kind, including waiting in `claim` (long-poll: the worker keeps the request open until work appears) and `msg.receive` (OBS-4). A worker that has not been seen for longer than two long-poll periods gives a `worker.stale` event and an alarm by code. This is an observation of the core, not a new state in the worker lifecycle. The worker's self-report (its own "I am `idle`") does not affect this observation and does not change core decisions (STA-2). The background loop `worker-liveness` counts by `last_seen`.

Example: at night the network of an analyst worker in tenant `demo` goes down. Its last self-report is `idle`, but `last_seen` is stale. The core writes a `worker.stale` event and raises the alarm `alarm.worker.stale`, its leases expire, and the `reaper` loop returns the work to the queue, where another worker of the same role takes it. The vanished worker gets no new work simply because it does not call `claim`.

### Background loop ticks

The core's background loops (core duties that work without an outside request) are deterministic, never call the model, are idempotent and safe with two instances. Every loop marks a tick in the store, and the tick age is visible in `status` and `doctor` (OBS-4). A missed tick of any loop makes the P2 `health` answer "not alive", and the monitoring system wakes people (MON-2). Operations loops:

| Loop | What it does | Version |
|---|---|---|
| `worker-liveness` | counts `last_seen`, writes `worker.stale` and an alarm | v0.1 |
| `hold` | keeps hold deadlines, applies effects to pending approvals and deliveries; sets auto-stops | v0.1 |
| `alarm` | keeps alarm episodes (`firing`, `resolved`, `seq`, `repeat_count`), puts every episode change as a delivery for the monitoring adapter E5 and re-exports open episodes after a connection failure (MON-1, MON-11) | v0.1 |
| `erasure` | sends erasure requests to the stores, collects outcomes, raises an alarm at the deadline | v0.1 |
| `schedule` | enqueues a task for every slot in UTC by the schedule time zone, skips nonexistent local time | v0.1 |
| `retention` | deletes by the periods of retention classes: in v0.1 only `content` and `observation` by defaults, from v0.2 by `Tenant.spec.retention` (RET-2) | v0.1 |
| `health` | computes the three health indicators, the age of backups and restore checks from the reports of the backup process, and raises alarms (OBS-9, RET-8) | v0.1 |

The other core loops (`reaper`, `deadline`, `delivery`, `approval-expiry`, `budget`, `provider_check`, `membership`, `inbox`, `observe`, `proposal`, `campaign`, `script`, `incident`, `control`, `identity`, `grant`, `governance`, `expectation`) follow the same contract and mark their ticks the same way; their full list with versions is in `spec/ports/background-loops.md`.

### Plugins, channels and the provider

Health is described for all parts of the farm by one schema, `spec/model/health-report.schema.json`: component (`loop`, `store`, `provider`, `worker`, `plugin`, `channel_upstream`), state (`ok`, `degraded`, `down`), checks with names from a closed list, and time (OBS-10). The toolhost reports the state of each plugin in `hello` and `heartbeat`, the channel reports the time of the last successful poll of the external system, P4 keeps the state of the model provider. The reports have no error texts, only codes: the error text of an external system may contain foreign text. A call of a plugin in `down` or of a plugin on a toolhost that is not visible gets `plugin_unavailable` at once: the model sees a call error and decides itself instead of waiting for the lease deadline. The `down` state only narrows dispatch and never widens it.

### Three health indicators

The core computes three indicators that show the farm is stuck even if each part on its own is alive (OBS-9):

| Indicator | What it means | What it catches |
|---|---|---|
| age of the oldest urgent task | how long the oldest task of priority classes 1 and 2 has been waiting in `submitted` (DSP-5) | the queue is stuck, people are waiting for an answer |
| share of deliveries within 10 minutes | the share of deliveries that reached `delivered` no later than 10 minutes after creation | the channel or the delivery loop does not work |
| tick age of every loop | how long it has been since the last tick of every background loop | one loop hung, the others work |

The thresholds and the counting window of the share are set by the deployment binding. An indicator beyond its threshold gives an alarm by code (`alarm.health.urgent_age`, `alarm.health.delivery_share`, `alarm.health.loop_tick`). The values are part of the daily summary to operators and of the P2 `health` answer that the monitoring system polls (MON-2), so people learn about a stuck farm even when its own channel is broken.

### Core liveness from outside

The core cannot report its own death, so core liveness is checked by a process outside the core (MON-2): the organization's monitoring system, and in an installation without one the deployment watchdog `kind: watch`, which alerts people by its own path (`watch.notify_ref`, D-88). Once a minute the monitoring system polls P2 `health` with a separate key that works only for this operation. The core answers "alive" only if the store answers and all background loops marked their ticks on time; the answer has only numbers: the free space of the store (CAP-5), the three health indicators (OBS-9) and the tick ages. No answer or "not alive" for longer than a threshold: the monitoring system wakes people by its own path, not through the farm channel. During a planned drain before a rollout the core answers "not alive" with reason `draining` and the number `drain_until_s`, and the liveness trigger does not fire within this period (LCY-6). The `health` key lives in the deployment configuration, the handler answers without the store, and during a key rotation two values are valid. The monitoring system does not run on the core machine and gets no farm credentials except the `health` key. If the export of alarms to the monitoring system fails for longer than 15 minutes, the core sends the tenant admins and the operator the code message `monitor_export_stalled`, no more often than once every 6 hours (MON-1, MON-11). This is described in detail in `docs/monitoring.md`.

## When the store is unavailable

The store (C1) holds all farm state: the queue, leases, approvals, events. If it does not answer, the core neither loses work nor starts it (OBS-8):

- P1 answers all operations with `not_ready` and `retry_after_s` (in HTTP with the `Retry-After` header), that is, "not ready, retry later";
- P4 starts no new model calls: work whose result has nowhere to be written is money spent for nothing;
- the channel does not move the external system offset (DLV-5): incoming messages stay at the channel's external system and will arrive again;
- the background loops do not mark ticks, and the core answers the monitoring system "not alive" (MON-2);
- a lease that expired during the unavailability and was not extended after readiness ends with reason `core_unavailable` and does not spend the attempt: this is an infrastructure failure, not a task failure (FEN-5).

The core leaves this mode only when the store answers and all background loops have marked a tick again.

Example on `demo`: on Friday evening the store hangs. Workers get `not_ready` and wait, there are no model calls, the channel does not acknowledge new updates, and they pile up at the channel's external system (Telegram keeps undelivered updates for up to 24 hours). A few minutes later the monitoring system sees that the core answers "not alive" and wakes the operator `alice` with its own alert. She restarts the store, the core waits for the ticks of all loops, clears `not_ready`, the channel picks up the accumulated messages, and the tasks continue without spent attempts.

## Alarms

### Alarm form

An alarm is an event `alarm.<name>.vN` that only code produces: the core or a worker of type `job` (INJ-7). Text from logs, messages and model answers can neither raise nor suppress an alarm. The data of all alarms follows one schema, `spec/model/event-data/alarm.schema.json` (OBS-6):

```yaml
alarmkey: "disk.free.low:db1"       # alarm name + subject id from the catalog
state: firing                       # firing | resolved
severity: page                      # info | warn | page
subject: {kind: resource, id: db1}
values: {free_pct: 7}               # only numbers and enums per the alarm schema
```

An alarm lives in episodes: `firing` opens an episode with the key `alarmkey`, `resolved` closes it; an episode has a change number `seq` and a counter `repeat_count`. A repeated `firing` with the same `alarmkey` opens no new episode, writes no event and gives no export, but only increments `repeat_count`: a check once a minute gives one export and one event, not sixty an hour. The life of one alarm episode is described in `spec/lifecycles/alarm.md`.

### How an alarm reaches people

An installation has one path of alarms to people (MON-1, D-88). Without the `monitor` profile, for every episode change the core itself puts a delivery of the code text `alarm_notice` into the direct chat of the tenant admins, alarms about the farm to the admins of `farm`; a repeated `firing` gives no message, and `page` reminds every 30 minutes until `incident.ack` (MON-13). With the `monitor` profile the farm has no delivery of alarms to people of its own (MON-12): the core puts every episode change as a delivery for the monitoring adapter E5, and the adapter passes it to the organization's monitoring system as a value on the farm node with the tags `tenant`, `alarm` (the name without `alarm.` and `.vN`), `subject` and `severity`; alarms about the farm itself also get the tag `scope: farm`. Alerting, repeats, "Acknowledged", escalation, on-call duty and silence are configured there by tags. Farm channels do not deliver alarms, and there are no alarm routes to people and no rights to receive alarms. An export delivery does not die by the number of attempts: it is retried while the episode is open, goes in `seq` order, and after a connection failure the core re-exports all open episodes (MON-11). The only code message that bypasses the monitoring system is `monitor_export_stalled` to the tenant admins and the operator when the export fails for longer than 15 minutes. How the export works and what the monitoring system does is described in `docs/monitoring.md`.

### Alarms of this section

| Alarm | When it is raised | Rule | Version |
|---|---|---|---|
| `alarm.worker.stale` | a worker has not been seen for longer than two long-poll periods | OBS-4 | v0.1 |
| `alarm.store.free.low` | the free space of the store volume is below the soft threshold | CAP-5 | v0.1 |
| `alarm.erasure.overdue` | an erasure request has been open longer than its deadline | RET-7 | v0.1 |
| `alarm.provider.spend_cap` | the model provider refused on the spend cap | CAP-2 | v0.1 |
| `alarm.hold.auto` | the core set an auto-stop | HLD-6, HLD-7, CAP-4 | v0.1 |
| `alarm.health.urgent_age`, `alarm.health.delivery_share`, `alarm.health.loop_tick` | a health indicator is beyond its threshold | OBS-9 | v0.1 |
| `alarm.backup.overdue` (`warn`) | a storage location has had no `backup.report {kind: backup, ok: true}` report for longer than 26 hours, or a report with `ok: false` arrived | RET-8 | v0.1 |
| `alarm.backup.restore_check_overdue` (`warn`) | no `backup.report {kind: restore_check, ok: true}` report for longer than 8 days, or the restore check failed | RET-8 | v0.1 |
| `alarm.spend.unreconciled` (`warn`) | the restore was completed with the operation `restore.complete --spend-pending`, and the spend gap has not yet been reconciled with the provider's report; while it holds, the daily limits of all tenants are cut in half | RET-6 | v0.1 |
| `alarm.spend.reconcile_overdue` (`warn`) | a provider workspace has had no `spend_reconciled` line for two days | OBS-12 | v0.1 |
| `alarm.spend.estimate_drift` (`warn`) | the daily sum of the workspace usage lines differs from the provider's daily sum by more than the tolerance from the deployment settings | BUD-10 | v0.1 |
| `alarm.monitor.export_stalled` (`warn`) | the export to the monitoring system for a tenant has failed for longer than 15 minutes; it reaches the monitoring system when the connection returns, and meanwhile people get the code message `monitor_export_stalled` | MON-11 | v0.1 |
| `alarm.incident.unhandled` (`page`) | an incident has had neither an operation nor P2 `incident.ack` (a person took the incident) within the `page_if_unhandled_after` of its class; it is cleared by an operation, `incident.ack` or closing the incident (`resolved`, `dismissed`) | MGR-7 | v0.1 |
| `alarm.manager.breaker` (`page`) | the safeguard of the farm manager seat tripped | MGR-14 | v0.1 |
| `alarm.disk.free.low`, `alarm.disk.fill.rate`, `alarm.monitor.problem` | the monitoring system reported a problem on a catalog node, a `job` task rechecked it by code | MON-5 | v0.1 |
| `alarm.service.control.rate` (`warn`; `page` if a service stop hit the limit) | a `service/control` or `monitor/script_run` call hit the action limit: 6 per hour per service or script on a node, 30 per hour per tenant; the call got `service_rate_limited` | MON-8, MON-10 | v0.1 |

Names are given without the version suffix `.vN`. Other parts of the farm raise their own alarms by the same schema, for example `alarm.task.unclaimed` (DSP-8), `alarm.task.stalled` (CNT-4) and `alarm.manager.absent` (MGR-2). The farm manager's incident catalog opens incidents on the code alarms `alarm.disk.free.low`, `alarm.store.free.low`, `alarm.budget.burn.rate`, `alarm.outbound.secret.detected`, `alarm.auth.tenant_mismatch.count`, `alarm.worker.crash.rate`, `alarm.action.unknown.attempt`, `alarm.model.provider.errors` and `alarm.queue.age.p95` (MGR-7). The farm itself does not measure alarms about resource and machine disks: they come from monitoring system problems (MON-5). If an incident has had neither an operation nor P2 `incident.ack` (a person took the incident) within the `page_if_unhandled_after` of its class, the core loop `incident` raises the alarm `alarm.incident.unhandled` with `severity: page`, and the monitoring system wakes people; "Acknowledged" on the alarm itself is pressed in the monitoring system (`docs/farm-manager.md`). A new kind of alarm is added like this: a `job` task type with the event `alarm.<name>.vN` by the common schema (`severity` goes in the alarm data); the export happens by itself (MON-1), and alerting by the new `alarm` tag is configured in the monitoring system. An agent type does not produce alarms.

## Hold

### What a hold is

A hold (`Hold`) is a recorded object: a scope, effects from a closed list, a reason, a deadline `until` (or no deadline, until release) and an author (HLD-1). Like any record, a hold belongs to one tenant (TEN-1): a hold on the scopes `farm` and `seat` is a record of the service tenant `farm`, a hold on any other scope is a record of the tenant of its scope. While a hold is active, C2 at every check point (PRM-3) denies everything it forbids, and does so before the rights formula, as a non-waivable ban (PRM-2). The model gets a short "no permission" refusal (PRM-8), the full reason is written to the audit. A hold deletes nothing and does not change manifests. It expresses an emergency stop, a tenant suspension, a worker quarantine, an urgent plugin recall, a schedule pause and stopping bots on a machine during a maintenance window: there are no separate switches for these. The life of a hold is described in `spec/lifecycles/hold.md`.

```yaml
# P2 hold.set: the request body; the core writes the id, author, source and time
scope: {kind: tasktype, tenant: demo, name: report.daily.v1}
effects: [no_enqueue, no_claim, cancel_running]
reason: runaway_loop                # from reasons.yaml
until: "2026-10-07T18:00:00Z"       # or null: until release
```

### Scopes and effects

Scopes and effects are closed lists in `spec/model/holds.yaml`. The scopes `schedule` and `governance` appear in v0.2; the other scopes and effects of this section exist from v0.1.

| Scope | What it covers | Who sets it | Version |
|---|---|---|---|
| `farm` | the whole farm | only the operator (`farmctl`), including with the command `farmctl hold farm` | v0.1 |
| `tenant` | one tenant | the operator; a tenant person with `hold.set:<scope>` | v0.1 |
| `role`, `tasktype` | a role or a task type in a tenant | the operator; a tenant person with `hold.set:<scope>`; an auto-stop; the holder of the farm manager seat | v0.1 |
| `worker` | one worker (quarantine) | the operator; a tenant person with `hold.set:<scope>`; the holder of the farm manager seat | v0.1 |
| `plugin`, `action` | a plugin or an action in a tenant | the operator; a tenant person with `hold.set:<scope>` | v0.1 |
| `channel` | a tenant channel | the operator; a tenant person with `hold.set:<scope>` | v0.1 |
| `host` | a machine from the catalog | the operator; a tenant person with `hold.set:<scope>`; from v0.2 a monitoring maintenance window (MON-9) | v0.1 |
| `resource` | a catalog resource | the operator; a tenant person with `hold.set:<scope>`; the holder of the farm manager seat | v0.1 |
| `bot_group`, `bot` | a bot group or one bot | the operator; a tenant person with `hold.set:<scope>`; the holder of the farm manager seat | v0.1 |
| `seat` | the farm manager seat | only an operator with `admin:farm` (kill switch, MGR-15); the seat safeguard by code (MGR-14) | v0.1 |
| `governance` | one governance case (its pause) | a tenant person with `admin:governance` in the case's tenant (the "Pause" button on the `governance_stuck` card, or P2) | v0.2 |
| `schedule` | one schedule | the operator; a tenant person with `hold.set:<scope>`; an auto-stop (CAP-4) | v0.2 |

| Effect | What it forbids | Where it is checked | Version |
|---|---|---|---|
| `no_enqueue` | new tasks in the scope, except core tasks for erasure and export: `rejected(held)` | P1 `enqueue`, P3 `enqueue_child`, routes, schedules | v0.1 |
| `no_claim` | dispatch of work of the scope | P1 `claim` | v0.1 |
| `no_message` | sending bot messages (`rejected(held)`) and wake-ups; entries that the attempt accumulated are dropped at `complete` (`discarded(held)`); reading inboxes is allowed | message sending in P1 and P3, `complete` | v0.1 |
| `cancel_running` | running attempts: they get `cancel_requested`, their handles are revoked, the attempt ends with `held` without being spent; `claim` does not dispatch a task under such a hold | the answer to `heartbeat` (FEN-4), P1 `claim` | v0.1 |
| `no_tool` | tool calls: `denied(held)`, the toolhost does not get the call, including a call already waiting in `queued` | P3 `tools/call`, handing a call to the toolhost | v0.1 |
| `no_action` | new `request_action` calls and decisions on pending ones; pending approvals become `canceled(held)` with `card_update` | P3, P1 `decide` | v0.1 |
| `no_delivery` | deliveries to the scope: `withheld`, except messages to admins about the hold itself | the `delivery` loop | v0.1 |
| `no_control` | control operations of the seat holder | C1 `control.apply` | v0.1 |
| `pause_action` | new `request_action` calls; pending approvals are not canceled but wait: the deadline is extended by the length of the hold, the card carries a pause mark, decisions are not accepted until the hold ends | P3, P1 `decide`, the `approval-expiry` loop | v0.2 |

### How a hold works

A hold takes effect from the next check, without `apply` and without restarts (HLD-3). An enqueue in the scope gets `rejected(held)`, `claim` does not dispatch work of the scope, a tool call gets `denied(held)`, pending approvals of the scope move to `canceled(held)` with a card update, deliveries get `withheld`. A write call in `queued` whose scope is hit by a hold with `no_tool` or `no_action` gets `denied(held)`, and its approval ends with `exec_failed(held)`: nothing is executed late (APR-7). Actions with services and monitoring scripts (`service/control`, `monitor/script_run`) go without a card, but a hold with `no_tool` or `no_action` on their scope rejects them the same way: `denied(held)` (MON-8, MON-10). A hold does not revoke a write call that the toolhost has already taken, and its outcome is recorded: a write to the world that has started cannot be recalled (ORC-11).

With `no_message`, sending bot messages in the scope gets `rejected(held)`, entries that an attempt accumulated are dropped at its `complete` (`discarded(held)`), wake-ups in the scope are not enqueued, and reading inboxes is allowed. With `cancel_running`, running attempts learn about the cancelation in the heartbeat answer, their P3 and P4 handles are revoked, and the attempt ends with reason `held` and is not spent. While a hold with `cancel_running` or `no_claim` on the task's scope is active, `claim` does not dispatch the task: otherwise the task would be dispatched and canceled in a loop.

Campaigns in the scope pause in the same transaction as the hold (ORC-11); a paused campaign without a decision for longer than `escalate_paused_after` (default 2 hours) gives a notice to the tenant admins (`docs/coordination.md`). A script step whose enqueue the hold rejected or whose pending approval the hold canceled becomes `failed(held)`, and the run continues by its `on_failure`; from v0.2 a hold that touches a run pauses the whole run in the same transaction, and only the initiator resumes it after the hold is released (SCR-10). A hold on scope `bot` set by a person moves the bot identity to `suspended` (IDN-9): it has no tasks, reviews or ballots, and the release or expiry of the last such hold returns it to `active` (`spec/lifecycles/identity.md`). A hold by the holder of the farm manager seat on the same bot only delays its tasks and changes neither the identity state, nor its eligibility for promotion, nor its vote (MGR-9).

An urgent recall of a plugin, action, task type or channel is done with a hold, and the removal from git follows later with an ordinary `apply`: this way the recall does not wait for review and rollout.

### Who sets and releases

A hold only narrows: it cannot become a path to extra rights (HLD-2). A hold is set by:

- the operator, on any scope, through `farmctl` (P2 `hold.set`) or, for the whole farm, with the command `farmctl hold farm`. A hold on the scopes `farm` and `seat` and its release are operator operations only (PRM-14);
- a person of their own tenant with the right `hold.set:<scope>`, on scopes of their tenant, through `farmctl` or with a command in a direct chat with the bot (PRM-15);
- the core by an auto-stop rule from the closed list `spec/model/holds.yaml` and as the first measure of an incident class from git (MGR-7);
- from v0.2 the core by a maintenance window in the monitoring system: a hold on scope `host` or `resource` with reason `maintenance` until the end of the window (MON-9).

A code hold inside a tenant (an auto-stop, the first measure of an incident, a maintenance window hold) is recorded with the author `contain_auto`; the source (rule, incident class, `system:monitor`) is written next to it. The model, a worker, a route and a schedule neither set nor release holds. A coordinator bot does not set holds and does not ask for them: people or code set a mass stop.

The only exception: the current holder of the farm manager seat with the current epoch (the number of the holder change, which grows with every change) sets and releases holds with `control.apply` operations (MGR-9). Its holds are limited: the target only from the list that code built for the task's basis; the deadline is no longer than `hold_ttl_max` (2 hours); it can release only its own holds, not holds of people and not auto-stops. The targets do not include workers and types that write the series used to stop it, and its holds do not delay control tasks. It does not stop protected targets (types that raise alarms, backups, system tasks and everything an incident class refers to) with a hold, but asks people with a card. Code computes the seat's limits (MGR-14, MGR-37). An attempt to exceed a rate limit, for example more than 3 quarantines per hour, gets `control_rate_limited` and itself trips the safeguard. An operation after which more than a quarter of a role, rounded down, would be under holds is not applied by the core, which creates a `control` card for people; such a referral does not count as a refusal. The safeguard also trips on 5 `control.apply` refusals within 10 minutes and on a spend jump: in one transaction, a `no_control` hold on the seat, an epoch increment and the cancelation of the holder's running attempts in all tenants; only an operator with `admin:farm` releases such a hold (MGR-14). A holder with an old epoch gets a refusal. When the holder is replaced, its holds live no longer than 15 minutes (MGR-1), and when the seat is stopped by an incident they live out their deadline, no longer than 2 hours, and there are no new ones (GOV-22).

A hold is released by a principal with the right `hold.release` on its scope and not below the hold's author in rank (HLD-2). The ranks from top to bottom: operator, tenant admin (a tenant person with `hold.release`), holder of the farm manager seat. The right `hold.release` by itself does not allow releasing any hold: otherwise a `demo` admin who has it would release a suspension for overspending set by the operator and continue spending money.

| Who set the hold | Who releases it |
|---|---|
| the operator (any reason, including `suspended` at a tenant suspension, `compromised` at a secret revocation, SEC-6) | only the operator |
| code inside the tenant, author `contain_auto` (auto-stop HLD-6, HLD-7, CAP-4; first measure of an incident MGR-7; maintenance window MON-9) | the tenant admin or the operator |
| a tenant person with `hold.set:<scope>` | the tenant admin or the operator |
| the holder of the farm manager seat (`control.apply`) | the holder itself (only its own holds), the tenant admin through P2 `control.revert` with `incident`, the operator |
| the seat safeguard, or the operator on the scopes `farm` and `seat` | only the operator (holds on `farm` and `seat` are operator operations, PRM-14) |

A hold on scope `governance` (from v0.2) is set and released by a person with `admin:governance` in the case's tenant (GOV-23). The rights `hold.set:<scope>` and `hold.release` are privileged and granted only through `farmctl` (PRM-13), like other decision rights. A hold set by code is released only by a person of at least the required rank; otherwise it ends at its deadline. The core does not release holds by itself: not on closing an incident, not on clearing an alarm, not on deleting a maintenance window. C2 checks the rank at the `hold.release` check point and answers `forbidden`.

### Auto-stops

An auto-stop is a hold that code sets by a rule from a closed list. A new auto-stop rule is added only by an architecture decision record of the skeleton (ADR), because a hold changes availability for everyone.

| Rule | What it sets | Registry rule | Version |
|---|---|---|---|
| the channel refused twice within an hour to send a message of one role because it found a secret in it (INJ-11) | a `role` hold in the tenant with `no_delivery`, without a deadline | HLD-6 | v0.1 |
| the operation limits of the farm manager seat are exceeded | a `seat` hold with `no_control`, without a deadline | MGR-14 | v0.1 |
| the tenant's spend within a sliding hour is above its hourly limit `Tenant.spec.budgets.hour_eur` (default `month_eur`/100, BUD-1) | a `role` hold with `no_claim` on every agent role of the tenant, reason `runaway_spend`, author `contain_auto`, deadline one hour; `job` tasks without a model run | HLD-7 | v0.1 |
| the last `K` = 3 slots of a schedule in a row ended with `attempts_exhausted` or `runs_exhausted` (D-100) | a `schedule` hold with `no_enqueue`, reason `schedule_failing`, author `contain_auto`, without a deadline; later slots are skipped with `held` | CAP-4 | v0.2 |

Every auto-stop raises the alarm `alarm.hold.auto`. An injection in text can trigger an auto-stop but cannot bypass it: this is a deliberate fail-safe refusal.

Example on `demo`: twice within an hour the channel did not send a message of role `ops-reader` because it found a token in it. The core sets a hold on scope `role` with `no_delivery`, reason `secret_in_outbound` and author `contain_auto`, without a deadline, and raises an alarm (HLD-6). Half an hour later `bob` revokes the leaked token, and the secret alarm moves to `resolved`, but the hold stays: the core does not release it. The answers of `ops-reader` are not delivered until `bob`, a `demo` admin with `hold.release` on this scope, checks the role and releases the hold with the P2 operation `hold.release`: he can release a `contain_auto` hold. Another example: at 14:00 `demo` has spent more than `hour_eur` within the hour; the core sets a `no_claim` hold on every agent role of `demo` with reason `runaway_spend` and `until` 15:00 (HLD-7). `bob` looks at the journal, finds a route in a loop, fixes it and releases the hold at 14:20 without waiting for the deadline.

A series of requests with a foreign tenant or foreign handles from the same credentials is not a hold but a move of the worker to `blocked` (TEN-2).

### Ready-made sets

| What | How it is expressed |
|---|---|
| tenant suspension | a `tenant` hold with reason `suspended` and the effects `no_enqueue`, `no_claim`, `no_tool`, `no_action`, `no_delivery` (P2 `tenant.suspend`, v0.2) |
| worker quarantine | a `worker` hold with `no_claim` and `cancel_running` |
| urgent plugin recall | a `plugin` hold with `no_tool` |
| a bot token leaked | a `channel` hold with `no_delivery` and `no_enqueue`: outgoing messages are not sent, tasks from this channel's incoming messages are not enqueued |
| a two-hour maintenance window for `db1` | people set the window in the monitoring system; from v0.2 the core sets by it a `host` hold with `until` the end of the window, the effects `no_tool`, `pause_action` (pending cards are not canceled but wait; with `hosts[].maintenance_hold: true` there is `no_action` instead) and reason `maintenance`; schedules whose tasks depend on `db1` skip slots with reason `held`, without catching up (MON-9) |
| suspected theft of the core key | a `farm` hold with `no_action` and `no_tool` |

### Emergency stop of the whole farm

`farmctl hold farm` writes a hold on scope `farm` directly to the store, without a running core process and without a channel (HLD-1). This is possible because `farmctl` on the core machine with the core credentials counts as part of the core; other processes have no access to the store (DEP-8). So a hold can be set even when the core process hangs or the channel is unavailable. The hold does not help if the core itself is compromised. For that there is an external stop that does not need the core: stop the core containers, revoke the bot token at the channel's external system, disable the tenants' keys at the model provider. Its order is written in the recipes below and is tested once on a test tenant before the first production one.

### Example on `demo`

A vulnerability was found in the `logs` plugin. The tenant admin `bob`, who has the rights `hold.set:plugin` and `hold.release` in `demo`, sets a hold with a command in a direct chat with the bot; the channel passes it to P2 `hold.set` on his behalf:

```yaml
scope: {kind: plugin, tenant: demo, name: logs}
effects: [no_tool]
reason: suspected_vulnerability
until: null
```

The next `logs` call in any `demo` task gets `denied(held)`, the toolhost does not get the call, the model sees "no permission". The core writes a `hold.set` event and an audit record, and the hold is visible in `hold.list` and `status`. When the fixed plugin image is rolled out (the recipe "Update a plugin"), `bob` releases the hold with the P2 operation `hold.release`; the core writes `hold.released`.

## Retention and erasure

### Retention classes

Every kind of stored object and every store belongs to exactly one retention class (RET-1). The schema of a stored object declares the class in `x-botaru-retention`; without it skeleton CI fails. An adapter that stores content declares the class in its sheet. The classes and their defaults are in `spec/model/retention-classes.yaml`:

| Class | What is in it | Default period | How it is deleted |
|---|---|---|---|
| `content` | task bodies, artifacts, files, attachments, sessions, runtime transcripts, script run records, attempt journals (AUD-4) and other logs with content, the sink archive, the channel cache | 30 days; sessions of roles with `memory: none` 7 days; the attempt journal per `retention.journals` | one object at a time |
| `memory` | memory records | until `forget`, the person leaving or the end of the tenant | only by an erasure request, with the C4 operation `erase` |
| `metadata` | audit, transition journal, events, usage | 400 days | only by whole periods (a month) |
| `tombstone` | tombstones and the erasure log: id, idempotency key hash, time, no content | while the tenant lives; after its deletion as `metadata` | by whole periods |
| `observation` | points and rollups of observation series | points 14 days, rollups 93 days (OBV-4) | by whole periods |

### Periods

Deletion by period is done by the background loop `retention`, and a minimal part of it exists already in v0.1 (RET-2): the loop deletes objects of the classes `content` and `observation` older than the default period from `spec/model/retention-classes.yaml`, and only those; in v0.1 metadata, tombstones and memory are not deleted by period, and anything earlier than its period is deleted only by a request (RET-7). Without this the store would grow until v0.2, and an operator whose core volume reached the CAP-5 threshold would formally have no right to clean up sessions older than 30 days. From v0.2 the periods are set by `Tenant.spec.retention` within the farm's ceilings, and the loop deletes all classes by them. It holds the periods of the retention classes, plus `files` (the file period, by which the core sets `expires_at` when a file is written, ART-1) and `journals` (the period of attempt journals; on a production tenant without this field the journal is disabled, AUD-4). Content is deleted no later than the period plus the loop period, and is deleted in all copies that the farm keeps: in indexes, caches, runtime stores. The loop deletes C3 records with the operation `prune`, and files after `expires_at` with the C1 operation `file.purge` together with the chunks and vectors of corpora (KNW-5). Metadata and observation series are deleted only by whole periods, never by selective editing of a row: the audit is append-only (AUD-3). Deleting the `principal` and `Identity` rows anonymizes the audit without a single edit of its rows. The seal of a retired bot does not extend retention periods: from v0.2 the `retention` loop deletes its sessions and records whose class period ends earlier, even if the seal is still valid. Already from v0.1, at the end of the seal everything sealed of a retired bot is erased by an erasure request with reason `bot_retired` and the scope of the bot identity id, which the `identity` loop enqueues (MEM-9). The core cannot seal a session inside the runtime (`runtime_internal`), so such a request for it is enqueued at once, in the bot retirement transaction, with a list of session ids, and the runtime adapter deletes it with the E4 operation `purge`.

### Tombstones

Deletion does not free uniqueness (RET-3). After a task is deleted, a tombstone remains (a record of ids only): `(tenant, idempotency_key hash, task id)`. The idempotency key stays taken while the tenant lives: a repeat of the key of a deleted task gets `gone`, not a new task (IDM-1). The ids of a tenant, principal, bot, task, `file_ref` and approval are not reused. A tombstone does not store a hash of the external id: a Telegram id and an email address can be found by brute force.

### Erasure request and erasure log

Any early erasure, that is, erasure before the class period, goes only through an erasure request (`ErasureRequest`), and this is true already from v0.1 (RET-7). There are seven kinds of early erasure; they are the request reasons:

| Reason | Who creates the request | Scope |
|---|---|---|
| `forget` | `/forget` by the owner (MEM-4); C4 `erase` executes it | record id or `(tenant, subject)` |
| `offboarding` | a person leaving, P2 `principal.offboard`: an admin with `admin:people` for people without privileged groups, an operator through `farmctl` for people with them (PRM-14) | `(tenant, subject)` |
| `bot_retired` | the core when a bot retires: in the retirement transaction for its sessions inside the runtime (`runtime_internal`, deleted by E4 `purge`), and the `identity` loop at the end of the seal for everything sealed (MEM-9) | at retirement, the list of ids of these sessions; at the end of the seal, the bot identity id (everything sealed of it) |
| `tenant_delete` | tenant deletion, P2 `tenant.delete` (v0.2) | `(tenant)` |
| `worker_revoke` | revocation of a worker whose runtime keeps sessions or memory itself, P2 `worker.revoke` (v0.2) | worker id (the store of its runtime) |
| `subject_request` | a person's request to erase their data: an operator through `farmctl`; the core on an approved `/mydata erase` request or `erase_match` (v0.1, D-76, SEC-11) | `(tenant, subject)`; for `erase_match` the list of ids of the found objects |
| `manual` | an operator through `farmctl` (P2 `erasure.request`) | `(tenant)`, `(tenant, subject)` or a list of object ids |

A request stores the scope, reason, author, time and the outcome for each store (`done`, `failed`, `not_applicable`), but no content. The scope comes from a closed list: `(tenant)`, `(tenant, subject)`, a bot identity id (everything sealed of it, only reason `bot_retired`), a worker id (the store of its runtime, only reason `worker_revoke`) or a list of object ids. The operator creates and retries requests (P2 `erasure.request`, `erasure.retry`), and the tenant admin also sees the requests of their tenant (P2 `erasure.status`, `erasure.list`). A request enters the erasure log before the first byte is deleted. The background loop `erasure` sends the request to the stores and collects the outcomes; while at least one store has not answered `done` or `not_applicable`, the request is open, and at its deadline it gives the alarm `alarm.erasure.overdue`. No process and no `farmctl` command can delete content directly, bypassing a request. Why: an operator who deleted a person's data by hand would otherwise bring it back with the very first restore from yesterday's backup. The erasure log is backed up separately from the store backups and no less often (RET-5), and after a restore it is replayed again (RET-6). The life of a request is described in `spec/lifecycles/deletion-request.md`.

One request can name many ids. This way an approved `erase_match` (erasure of the data of a person who is not a principal of the farm, by an exact string) deletes all found objects with one request, and after a restore the erasure is replayed by the same ids; the string itself does not enter the erasure log (SEC-11).

Deletion by period is done by the `retention` loop without a request: after a restore it deletes what is overdue again anyway.

### Erase operations in the ports

Every port that stores content has an erase operation by the scope `(tenant)`, `(tenant, subject)`, a bot identity id and a list of object ids (RET-4); the table shows from which version:

| Store | Erase operation | From version |
|---|---|---|
| sessions (C3) | `delete` of one session by id, `purge` by scope or list of ids | v0.1; scope `(tenant)` from v0.2 |
| memory (C4) | `erase` | v0.1 (people's memory and the sealed memory of retired bots) |
| runtime store with `runtime_internal` (E4) | `purge` through the adapter (RT-1, RT-2) | v0.1 |
| files (C1) | `file.purge` | v0.1 (D-76) |
| task content (C1) | `task.purge`: `body`, `result`, the bodies of deliveries and events with content; a tombstone remains | v0.1 (D-76, D-86 (8)) |
| sink archive (E2) | `erase` | v0.1 (D-76) |
| monitoring system (E5) | `erase`: the scope `(tenant)` removes this tenant's items from the farm template; `(tenant, subject)` answers `not_applicable`, because notes carry no personal data (class `summary` and redaction, MON-7); for a list of note ids the reference binding from the E5 sheet answers `failed` if the monitoring system API does not delete notes, the binding sheet declares this in advance, and such an outcome does not keep the request open | v0.1 (D-76) |
| saved attachments and the channel cache (E1) | `erase`; in v0.1 the channel declares `driver.stores_content: none`, and the core itself writes its step with the outcome `not_applicable` with `basis: declared_none`, so the request does not hang because of E1 (D-86 (8)) | v0.2 |
| runtime store without `runtime_internal` (E4) | `purge` through the adapter | v0.2 |

Every operation returns the outcome `done`, `failed` or `not_applicable`, and the core writes it into the erasure request. An outcome `not_applicable` without a `basis` field (how the store made sure there is nothing to delete) is not accepted. Erasure work by subject carries `needles` (the person's handles and addresses), which the core keeps until the end of the erasure so that an edge store knows whom to delete. The adapter of every such port passes the `erasure` suite: without it, it does not conform to the skeleton. A content store without its own erase operation (in v0.1 the attachments and cache of channel E1, and runtimes without `runtime_internal`) answers the request with `failed`, and the request stays open with an alarm; `failed` from E5 for note ids does not keep the request (RET-4, RET-7). Erasure of a person's data at their request is part of v0.1 and cannot be waived for a tenant with `kind: production` (D-76).

### What erasure does not reach

Two things lie outside the farm, and a binding speaks about them, not a rule. The model provider keeps requests by its own terms, and the provider binding records its retention period. Correspondence in the cloud of the channel's external system stays there, and the channel binding records what the bot can delete and what it cannot.

### Example on `demo`

The person `carol` asks to erase her data in tenant `demo` with the command `/mydata erase` (SEC-11, v0.1); `bob` with `admin:people` approves the card, and the core creates an erasure request with scope `(demo, carol)` and reason `subject_request`; an operator can create the same request through `farmctl` (P2 `erasure.request`). The core writes the request to the erasure log, and the `erasure` loop deletes her sessions in C3, memory records in C4, files in C1 (`file.purge`) and the sink archive E2 (`erase`). In v0.1 channel E1 has no erase operation yet; if it declared `driver.stores_content: none` in `hello`, the core itself closes its step with the outcome `not_applicable` with `basis: declared_none` (D-86 (8)). A channel that keeps attachments or a cache and has no erase operation answers `failed`, the request stays open, the operator sees it in `erasure.status`, and an alarm reminds about it at the deadline. A week later the store is restored from a backup taken before the request; the core replays the erasure log before readiness, and the data of `carol` is deleted again. From v0.2 `carol` herself gets an extract of her data with the command `/mydata export` (SEC-10), and E1 `erase` closes the request completely.

```yaml
# ErasureRequest record (sketch)
id: er_01JB8M
scope: {tenant: demo, subject: 'human:p_01JB3Q7N0M2V8XK4DGT5RZP9WC'}   # carol
reason: subject_request
author: {kind: principal, id: 'human:p_01JB3Q7N0M2V8XK4DGT5RZP9WA'}   # bob
created_at: "2026-10-07T09:12:00Z"
state: running
results: {c3_sessions: done, c4_memory: done, c1_files: done, e2_archive: done, e1_channel: not_applicable}   # v0.1: the channel declared stores_content: none, basis: declared_none
```

## Backups and restore

### A backup is a production perimeter

A backup holds content: task bodies, bot memory, audit. So it follows the rules of a production machine (DEP-1): it is encrypted, accessible only to the operator and stored not on the core machine: on a separate farm machine `backup1` from `farm_hosts` (RET-5, DEP-11) or, in a single-server installation, in external storage (`backup.target: external`, D-88). A backup goes to external storage encrypted on the core machine before sending, with the key `backup.key_ref`, which the storage does not have, and the storage credentials can only write new objects; for a production installation without a second machine this is mandatory, and the deployment lint rejects anything else (`backup_external_required`). Such an installation does the restore check (RET-8) on a temporary VM or a machine outside the farm. The lifetime of any backup is no longer than the longest period of class `content` plus the backup cycle window, so deleted data leaves the backups by itself. How many backups to keep and how often to take them is decided by the deployment within these limits: with a `content` period of 30 days this is, for example, daily backups for the last 30 days and none older. The skeleton does not choose a backup method. The erasure log (erasure requests and tombstones, metadata only) is backed up separately from the store backups and no less often.

### What the core does after a restore

A restore from backup breaks three non-waivable rules if nothing is done: an approval would be executed a second time (APR-7), lease numbers would roll back and a token from the "future" would match a newly issued one (FEN-2), a deleted memory record would come back (MEM-4). So after a restore, before the core is ready (RET-6):

1. the store epoch grows. The epoch is a number that is part of the lease token `(epoch, work_id, attempt, worker_id)` and of handles. All work and all handles of the previous epoch get `fenced`;
2. all pending approvals (`pending`) move to `canceled(restored)` with a card update; an approval in `executing` whose write call was in `queued` or `leased` gets `exec_unknown` together with it; actions are not repeated (APR-7);
3. the erasure log is replayed again from its separate backup, including manual erasures through `farmctl`;
4. P1 `claim` and `enqueue` answer `not_ready`, P4 starts no model calls;
5. the core does not become ready until the spend gap since the backup moment has been filled with `spend_reconciled` lines from the provider's report (BUD-10): the operator's reconciliation process brings the report through P2. If waiting for the report is not possible (night, the provider's report will appear in hours, the person with the reporting key is on vacation), the operator completes the restore with the operation `restore.complete --spend-pending`: the core becomes ready, the spend gap is marked open, code keeps the alarm `alarm.spend.unreconciled` (`warn`), and the daily limits of all tenants are cut in half until `spend.reconcile` reconciles the gap; after reconciliation the alarm closes and the limits return.

The restore is completed by P2 `restore.complete` or by the core itself if at startup it sees that the separate erasure log backup is ahead of the store.

### What is lost

This is normal and is written here, not in a rule. Channel updates accepted after the backup moment have already been acknowledged by the core, and the external system will not send them again: these requests are lost, people will repeat them. Approvals that were waiting for a decision are canceled: the actions have to be requested again. Schedules catch up missed slots within their catch-up window. Provider spend between the backup moment and the failure is not recorded in the backup; the core fills it in with `spend_reconciled` lines from the provider's report before readiness, and after that the daily usage reconciliation keeps it (BUD-10).

### Backup reports and the restore check

Backups are taken by an external backup process, and the core itself knows nothing about them. So after every store backup, every erasure log backup and every restore check, the backup process must report to the core with the P2 operation `backup.report` using its narrow key (RET-8):

```yaml
# P2 backup.report (sketch)
kind: backup                      # backup | restore_check
started_at: "2026-10-07T02:00:00Z"
finished_at: "2026-10-07T02:14:00Z"
ok: true
size: 41234567890                 # bytes
location_id: store-daily          # id of the storage location from the deployment configuration; no paths or addresses
```

A backup whose restore has not been checked does not count as a backup. At least once a week the backup process or the operator restores a fresh backup on a separate test bench that follows the same rules as the backup, runs the `restore/` check set and P2 `doctor` on it, and reports `backup.report {kind: restore_check}`. The `health` loop counts by the latest reports: no successful `backup` report for a storage location for longer than 26 hours gives the alarm `alarm.backup.overdue`; no successful `restore_check` report for longer than 8 days gives the alarm `alarm.backup.restore_check_overdue`; a report with `ok: false` gives the same alarm at once. The age of the latest backup of every location, the date and outcome of the last check are visible in `status` and `doctor`; `doctor` fails the check if the erasure log backup is older than the store backup. This way a backup script that silently failed in July is noticed the next day, not at restore time.

## Capacity and dependency failure

### Tenant limits

Money does not hold all work: `job` tasks and calls without a model spend no money, and euro limits do not restrict them, while a route in a loop or a mistake in a schedule enqueues tens of thousands of tasks a day and fills the shared disk. So already from v0.1 the core limits a tenant's work with counters (CAP-1):

```yaml
# tenants/demo/tenant.yaml (fragment)
spec:
  kind: production              # production tenant (APR-9)
  timezone: Europe/Berlin       # IANA name (TIM-2)
  attention:
    noncritical_per_day: 10     # non-urgent decision cards per person per day (APR-15, v0.2)
    cards_per_tenant_hour: 20   # action cards after untrusted text per tenant per hour (APR-2)
  delegated_groups: [researcher] # groups that the tenant admin grants by themselves (PRM-16)
  farm_manager:                 # the farm manager for this tenant (D-77)
    enabled: true               # false: the farm manager reads no data, no review and no lens
    lens: ops-reader            # the farm manager's lens in the tenant
    daily_review: true          # daily review
    model_profile: seat-default # the seat's model profile for this tenant
  budgets:                      # money (BUD-1); hour_eur drives the HLD-7 auto-stop
    month_eur: 200
    day_eur: 10                 # default month/20
    hour_eur: 2                 # default month/100
  operator_access:
    debug_journal: notify       # deny | notify | allow: the operator reading journals through admin:debug
  retention:                    # retention periods (RET-2, v0.2; in v0.1 the class defaults)
    content: 30d
    metadata: 400d
    files: 30d                  # file period, used to set expires_at (ART-1)
    journals: 7d                # attempt journal; on a production tenant without the field it is disabled (AUD-4)
  limits:                       # only below the farm ceilings (CAP-1)
    max_queued: 5000            # tasks in submitted
    max_enqueue_per_hour: 2000  # enqueues within a sliding hour from all sources
    max_storage_bytes: 21474836480   # 20 GiB in bytes: total space ceiling for files and artifacts
    max_file_bytes: 209715200        # 200 MiB: one file (ART-4, v0.2)
    max_task_files_bytes: 1073741824 # 1 GiB: all files of one task (ART-4, v0.2)
```

The farm ceilings are set by the deployment binding; a tenant can only lower them. 20% of the places in `max_queued`, but no fewer than 2, are reserved for tasks of priority classes 1 and 2 (a person's question, a card, something urgent, DSP-5): service tasks (schedules, reviews, analysis, peer reviews) and tasks of classes 3-5 cannot take the reserve and get a refusal when `max_queued` minus the reserve is taken, while a person's question gets a refusal only when the whole `max_queued` is taken. Otherwise a provider key broken on Friday would fill the queue overnight with schedule tasks with a daily `deadline`, and in the morning the question of `alice` "what is wrong with the site" would get `over_capacity`; schedule slots are skipped while the provider circuit is open (`skip_if_waiting`, event `schedule.skipped`, TIM-3). Exceeding a limit at enqueue gives `rejected(over_capacity)` with `retry_after_s` and does not take the idempotency key, so the sender will retry the same request later. A schedule task skips the slot with the event `schedule.skipped(over_capacity)`. A person sees the canned code text "overload", an external program gets an RFC 9457 answer with the `Retry-After` header. The farm refuses early and only to whoever exceeded: other tenants work as usual. From v0.2 the file limits are there too: `max_file_bytes` holds the size of one file, `max_task_files_bytes` the size of all files of one task; exceeding gives a refusal, not a silent truncation of the file (ART-4). The size of a task tree from one root is held by `max_tree_tasks` (ORC-8), and the number of running tasks per type and group by the dispatcher's `max_running` (DSP-7).

### Storage headroom

If the store volume fills up, the whole farm stops, and there will be nowhere even to write an incident about the disk: writing an incident is a transaction into the same volume. So the core itself measures the free space of the store volume and keeps headroom (CAP-5):

| Threshold | What the core does |
|---|---|
| below the soft one | enqueues of priority classes 3-5 get `rejected(over_capacity)`; alarm `alarm.store.free.low`; the free space number is part of the P2 `health` answer (MON-2), because a write to the store and the export of the alarm may no longer go through |
| below the hard one | P1 answers `not_ready` to everything except `heartbeat`, `complete`, `fail` and `release`; P2 and hold operations work; P2 `health` answers "not alive" |

The thresholds are set by the deployment binding. The store headroom is not part of any tenant's `max_storage_bytes`. The incident class "low space for the database" (`store_low`) in the farm manager's catalog relies on the alarm `alarm.store.free.low`; it reaches people through the monitoring system (MON-1), and the monitoring system also sees the space number in the `health` answer (MGR-38).

### Space quotas on volumes

The farm store is not the only disk that can fill up: pipelines, sinks and tenant tasks write to volumes of catalog machines, for example `res:ctd1:data`. The catalog declares volumes in `Catalog.spec.volumes` and links each volume with everyone who writes to it (`writers`: tenants, sinks, pipelines), and every writer on this volume has a space quota `quota_bytes` (CAP-6). At catalog approval and at `apply` of quotas, the core shows people for each volume whether every writer has a quota and whether the sum of the quotas is below the alarm threshold of class `disk_full`. On this depends whether a "disk full" incident can stop the farm manager seat: only then can the disk be filled only by those the farm manager could watch (MGR-33). The used space of each volume is written by a code observation series (`dataorigin: code`): a `job` role reads it from the monitoring system history (the monitoring agent measured the number; the model takes no part in the series). By this series the core checks that the growth was visible for two hours before the incident (MGR-34).

| Volume `res:ctd1:data`, 4 TB | Quota |
|---|---|
| tenant `demo` (task results) | 2 TB |
| sink `archive` | 0.5 TB |
| pipeline `ctd-qc` | 1 TB |

The class `disk_full` fires when less than 5% is free, that is, more than 3.8 TB is used. The sum of the quotas, 3.5 TB, is below that, all writers have quotas, and the MGR-33 condition for this volume is met. If another sink without a quota wrote to the volume, `apply` would show the volume as not meeting the condition, and a `disk_full` incident on it would not stop the farm manager seat. The quota on the volume itself is enforced by the writer: the volume's file system, the sink or the pipeline per the deployment settings; the core checks that the quotas are declared and their sum is below the threshold.

### Refusal reasons

The reason registry `reasons.yaml` distinguishes a task failure from a failure of an external dependency or an overload. The rule is simple: nothing but the task itself spends its attempt. Below are the reasons of this section; the full list of task and delivery reasons is described in `docs/tasks-and-delivery.md`.

| Reason | Spends the attempt | Retry | Outcome | Rule |
|---|---|---|---|---|
| `provider_unavailable` | no | yes: the core retries the task with a growing pause and jitter until the `deadline`; while the circuit is open the task is not dispatched | `failed(deadline_exceeded)` | CAP-2, GW-11, GW-12 |
| `provider_rate_limited` | no | yes, after `retry_after_s` | same; this is the model provider's rate limiter | CAP-2 |
| `service_rate_limited` | no | yes, after `retry_after_s` | a refusal by the farm's rate limiter: actions with services and scripts (MON-8, MON-10), notes (MON-7), cards, wake-ups; for actions with services the alarm `alarm.service.control.rate` | MON-8, MON-10 |
| `forbidden` | no | no | a refusal of a right: `hold.release` not by rank, a service or script outside the role's right, a node of another tenant | HLD-2, MON-8, MON-10 |
| `provider_quota` | no | yes: the dispatcher does not dispatch the task until the tenant has a share of the model pool, and P4 delays a call beyond the share | same | CAP-2, GW-10 |
| `provider_config` | no | not by timer: the task waits for the provider setting to be fixed, the pair's circuit is open | same; an alarm by code | CAP-2, GW-13 |
| `provider_spend_cap` | no | no | `failed`; an alarm by code | CAP-2 |
| `plugin_unavailable` | no | no: the core does not retry the call, the model decides itself | a tool call error at once, the task continues | OBS-10 |
| `over_capacity` | no | yes for the sender, after `retry_after_s` | `rejected` at enqueue, the key is not taken | CAP-1, CAP-5 |
| `held` | no | no; a task whose attempt `cancel_running` canceled waits in `submitted` until the hold is released | `rejected`, `denied`, `canceled`, `withheld`, `discarded`; for the approval of a write call `exec_failed` | HLD-3 |
| `core_unavailable` | no | yes, the task returns to `submitted` without a pause | the lease expired while the core was not working and was not extended after readiness | FEN-5, OBS-8, LCY-6 |
| `not_ready` | no | yes, after `retry_after_s` | a P1 answer while the core is not ready | OBS-8, RET-6, CAP-5, LCY-6 |
| `restored` | no | no | `canceled` for approvals after a restore | RET-6 |
| `schedule_time_invalid` | no | no | `rejected` for a proposal to change a backup schedule if its slot disappears on at least one day (v0.2) | TIM-5 |
| `gone` | no | no | the answer to the key of a deleted task, to `redrive` of a deleted body and to P1 `file.get` of a deleted or expired file (v0.2) | RET-3, CAP-4 |

### Model provider failure

A provider failure spends no attempts (CAP-2). With `provider_unavailable`, `provider_rate_limited` and `provider_quota` the task waits and is retried with a pause until its `deadline` and ends with `failed(deadline_exceeded)`, not `attempts_exhausted`. A provider error is retried by one layer: the provider client in P4 at most once, the runtime does not retry, and after that the core retries the task with a pause up to a ceiling (default 60 s) and random jitter, so that after the provider recovers the waiting tasks do not arrive all at once (GW-12). On a series of failures P4 opens the circuit of its pair (workspace, model pool) and answers `provider_unavailable` at once without contacting the provider, and the dispatcher does not dispatch tasks that need this pair (GW-11). Only runs where the model answered count toward the task's run limit (FEN-3). A provider configuration error (key revoked, model withdrawn) gives `provider_config`: it does not spend the attempt, is not retried by timer, gives an alarm, and the task waits for a fix until its `deadline` (GW-13). `provider_spend_cap` (the provider refuses until access is restored) is not retried: the task ends with `failed`, code raises the alarm `alarm.provider.spend_cap`. A fallback provider bypassing P4 is forbidden (DEP-5): degradation goes by refusal and alarm, not by bypassing the gateway.

Example on `demo`: for forty minutes the model provider answers "overloaded". P4 opens the circuit, the `demo` tasks wait with a pause, attempts are not spent. Tasks with a 30-minute `deadline` end with `failed(deadline_exceeded)`, the others continue when the circuit closes again. No task ends with `attempts_exhausted`.

### Delivery rate

From v0.2 a channel driver declares a rate (`rate` per chat, per group and per bot), and the delivery loop respects it (CAP-3). Extra deliveries wait in `planned`; this is not a failure, and no attempt is spent. A driver without `rate` gets the farm ceiling. Folding many notices of one route to one recipient into one message is done by the route's `batch` mode.

### Dead work

Dead work is a task that ended with `failed`, or a delivery that ended with `dead`, with reason `attempts_exhausted`, `runs_exhausted` or `deadline_exceeded` (DLV-4); there is no separate dead letter queue. From v0.2 P2 `deadletter.list` groups it by (tenant, type, reason), so that one broken check once a minute does not hide everything else (CAP-4). A schedule that had `K` slots in a row end with `attempts_exhausted` is paused by an auto-stop: a hold on scope `schedule` with effect `no_enqueue`, and an alarm. The body of a dead task is `content`; after its period, `redrive` answers `gone`.

## Time

### One clock

The only clock for farm deadlines is the store clock (TIM-1): leases, `start_by`, `deadline`, the approval deadline, the handle deadline, budget windows, schedule slots, hold and retention periods. P1 answers carry the remaining seconds (`lease_remaining_s`), and the absolute time in them is for reference. A worker with a clock shifted by 10 minutes works correctly.

### UTC and time zones

All times in contracts are in UTC in the `datetime` format (TIM-2). A time zone is an IANA name in the `tz` format of the registry `formats.yaml`, and it exists in only two places: `Tenant.spec.timezone` (budget windows and texts for people) and `Schedule.spec.timezone`. The budget windows "day" and "month" are counted in the tenant's time zone (BUD-4); a window boundary is a UTC instant, the same in the reserve, the charge and the refusal text.

The schedule time zone is set now, before the first clock change (TIM-4). Without `Schedule.spec.timezone` the tenant's time zone applies. The time zone name is checked against the time zone database at `apply`, and an unknown name fails. A time zone inside a `cron` expression (`CRON_TZ`, `TZ`) is not allowed. A change of the time zone or of `cron` takes effect from the next slot after it is applied, and the slot key in UTC does not give a second run of an instant that has already run.

### Slot and clock changes

A schedule slot is a UTC instant with minute precision in the `schedule_slot` format of the registry `formats.yaml`, and the key of a slot task is `system:schedule:<name>:<slot>` (TIM-3, IDM-2). The slot format is part of the idempotency key: changing the format later would give a double run at the seam, so it is fixed in v0.1. Local time that does not exist at the switch to summer time is skipped with the event `schedule.skipped` and reason `dst_gap`; local time that happens twice runs once, at the first occurrence. Skipping a slot for any reason is the same event `schedule.skipped` with a reason from a closed list (`dst_gap`, `waiting`, `held`, `over_capacity`), and a skipped slot is not caught up. A schedule task must have a `deadline`: a schedule with a type without `deadline` fails `apply`. The field `skip_if_waiting` (default `true`) skips a slot with reason `waiting` while the task of the previous slot waits in `submitted` or the provider circuit of the needed model slot is open (TIM-3, `docs/task-language.md`).

```yaml
# tenants/demo/schedules.yaml (fragment)
apiVersion: botaru/v1alpha1
kind: Schedule
metadata: {name: daily-report, tenant: demo}
spec:
  cron: "30 2 * * *"
  timezone: Europe/Berlin   # without the field the tenant's time zone applies
  skip_if_waiting: true  # default: the slot is skipped with schedule.skipped(waiting) while the previous task waits
  task: {type: report.daily.v1}   # the type has a deadline, otherwise apply rejects it (TIM-3)
```

| Clock change in time zone `Europe/Berlin` | Local time | What the schedule above does |
|---|---|---|
| 2026-03-29, spring | 02:00 -> 03:00, 02:30 does not exist | the slot is skipped, event `schedule.skipped` with reason `dst_gap` |
| 2026-10-25, autumn | 03:00 -> 02:00, 02:30 happens twice | one run at the first 02:30, which is 00:30Z; key `system:schedule:daily-report:2026-10-25T00:30Z` |

Daily tasks are better scheduled at a time outside the night window of clock changes.

### Schedule check before "Accept"

A bot can propose to change the `cron`, the time zone or the pause of a schedule. From v0.2, before the "Accept" button of such a proposal, the core computes with code the slots of the new schedule for a year ahead and shows on the card (TIM-5): local times that will disappear or repeat at clock changes; overlaps with slots of the tenant's other schedules whose tasks depend on the same resources; overlaps with the change windows of changes that have a running change trial (PRP-21), because a new change of the same subjects in such a window makes the trial outcome `mixed` (PRP-22). An expression or time zone that would fail `apply` gets no "Accept" button. If a slot of a backup schedule (a type with `makes_backup: true`) disappears on at least one day, for example at a clock change, the proposal gets the refusal `rejected(schedule_time_invalid)`. At the press of "Accept" itself the core repeats the check: if another schedule changed in the meantime and the outcome became different, the decision is not accepted, and the card is updated with the new outcome. The model takes no part in this check. Maintenance windows of the monitoring system are not computed a year ahead: that would rebuild the monitoring system's own window scheduler, and a slot inside a window is skipped when it comes (MON-9, D-100). How a proposal reaches the card and how its outcome is measured is described in `docs/improvements.md`.

## Tenant lifecycle

A tenant is declared in git with the file `tenants/<name>/tenant.yaml` and appears at `farmctl apply`; there is no separate command to add a tenant. It goes through four states (LCY-1); the transitions are described in `spec/lifecycles/tenant.md`. Tenant operations (`tenant.suspend`, `tenant.offboard`, `tenant.delete`, `tenant.export`) are operator operations and are not executed from a channel (PRM-14).

In `active` the tenant works according to its rights and periods. In `suspended` there is no new work except core tasks for erasure and export, pending approvals are canceled, deliveries get `withheld` except messages to admins about the hold itself (HLD-3), and data, credentials and the provider key are not touched. In `offboarding` only export is allowed. In `deleted` content and memory are deleted, metadata lives until the end of its period, and the tenant id stays as a tombstone.

A suspension (for example for non-payment) is a `tenant` hold with reason `suspended`, and a return is the release of this hold, and only the operator can release it: a tenant admin with `hold.release` gets `forbidden` (HLD-2). Offboarding (P2 `tenant.offboard`) revokes everything with which the tenant could spend money and act: every tenant has its own workspace and key at the model provider (GW-3, D-42). Deletion (P2 `tenant.delete`) is possible no earlier than the end of the export window, by default 30 days; it creates an erasure request with scope `(tenant)` across all stores, and `apply` of a tenant with the same name is rejected after that. The audit of a deleted tenant lives for the `metadata` period without a link to people and is then deleted in full.

Export of a tenant or a person (P2 `tenant.export`, `principal.export`, v0.2) is an operator operation (PRM-14). It enqueues a core task and writes the result only to a sink named by the operator, in JSON Lines by the schemas of `spec/model` with their `$id` (LCY-2). A person asks for a copy of their data with the command `/mydata export`, and an approved request enqueues the same export task (SEC-11). Export never goes to a chat or to the model: it contains raw data. A person's export contains only objects where they are the initiator and objects that belong to them. Declared objects are not exported: they are in git.

### A person leaving

A principal (a person or an external program, D-47) goes through the states `active`, `off`, `offboarded` (LCY-3). An admin's `/off` deletes nothing and is reversible: in one transaction the person's pending approvals (APR-6) and tasks (`canceled(person_inactive)`) are canceled. Schedules have no creator in their record, so they stay; the tenant admins decide on them. Leaving for good (P2 `principal.offboard`: an admin with `admin:people` for people without privileged groups, an operator through `farmctl` for people with them, PRM-14) does the same, writes the state `offboarded`, which no operation leaves (D-98), and additionally deletes the person's memory and sessions through an erasure request (MEM-4, RET-7). From v0.2 leaving publishes the event `principal.offboarded` and deletes the person's `Identity`, after which only an opaque id remains in the audit. In core records (task, audit, usage, grants, campaign, erasure) the person is recorded by this principal id `human:p_<ULID>` even before leaving, so the records are not rewritten after leaving: the audit keeps the same id, and the name is deleted from the principal record (D-85). A person who returns gets a new principal, and the old rights do not come back; there is no return window (D-100).

## Workers and plugins

### Worker drain and revocation

A worker in `draining` (P2 `worker.drain` with a `deadline`) gets no new work and finishes the running work until `drain_deadline`. From the deadline the heartbeat answer asks for release (`release_requested`), the worker lets the work go through `release`, and the attempt is not spent; a lease of a draining worker that expires after the deadline ends with `worker_drain` without spending an attempt either (LCY-4). The work version is pinned at the task type level (`.vN`), not at the worker: attempts are short, a retry can go to a new image, so the image must understand all `.vN` from the `claims` of its role. At the revocation (P2 `worker.revoke`) of a worker whose runtime keeps sessions or memory itself, the store of its profile is cleaned through the adapter by an erasure request with reason `worker_revoke` and the scope of the worker id (RET-7).

### Worker port version window

The core accepts the current and the previous major version of P1 (LCY-7). A worker outside the window gets `version_unsupported` at `hello`, not errors in the middle of work. This way the core and the workers are upgraded at different times.

### Plugin update

From v0.2 a plugin is updated without the toolhost moving to `blocked` (LCY-5). During the transition `source.accepted` in the plugin manifest lists up to two pairs (image digest, `tools/list` fingerprint), both matching the manifest's `tools`, and `hello` passes with either; an `apply` that removes the old pair makes a toolhost with the old image `blocked`. The model always sees the tool descriptions of the manifest. Deprecation and sunset of a plugin follow the general manifest rule (MAN-7).

```yaml
# tools/logs/tool.yaml (fragment)
source:
  image: registry.example/tools/logs@sha256:<digest>
  accepted:
    - {image: "registry.example/tools/logs@sha256:<digest>", tools_fingerprint: "sha256:<digest>"}   # current pair
    - {image: "registry.example/tools/logs@sha256:<digest>", tools_fingerprint: "sha256:<digest>"}   # new one, for the transition
```

## Rolling out a new core version

A core restart must not burn the attempts of running tasks. So a rollout goes like this (LCY-6, LCY-7):

1. The operator calls P2 `core.drain` with a deadline (an operator operation, PRM-14) and sets a maintenance window on the core node in the monitoring system for the duration of the rollout. The core stops dispatching work: `claim` answers `not_ready` with `retry_after_s`. Running attempts work until the deadline, started P4 calls run to the end, then the core stops.
2. During the whole drain P2 `health` answers "not alive" with reason `draining` and the number `drain_until_s` (LCY-6, MON-2). The liveness trigger in the farm template does not fire within this period, so a forgotten maintenance window does not wake the on-call person; the window is needed in case the rollout takes longer than the drain deadline.
3. A lease that expired while the core was not working and was not extended after its readiness ends with `core_unavailable` and does not spend the attempt: this is an infrastructure failure, not a task failure (FEN-5).
4. While the store has `active` leases, the store schema changes only by addition: expand first, and contract in the next rollout.
5. The new core starts and is not ready until the store has answered and all background loops have marked a tick (OBS-8). Workers with the previous major P1 version keep working.
6. The core answers "alive" to P2 `health` again; the operator checks `doctor` and removes the maintenance window.

A window on the core node does not set farm holds: it is a farm node, not a resource of a tenant's catalog (MON-9, DEP-11).

If the core is rolled out without `core.drain`, running attempts are interrupted, but leases that expired during the downtime still do not spend attempts (FEN-5).

## Recipes

Every recipe is a sequence of port operations. The commands of a particular implementation live in its documentation. The core rollout and the monthly restore check are described in their own sections above.

### Emergency stop

1. Part of the farm: P2 `hold.set` with the needed scope and effects (for example `tasktype` with `no_enqueue`, `no_claim`, `cancel_running`).
2. The whole farm: `farmctl hold farm`. The hold is written directly to the store and takes effect without the core process and the channel.
3. If the core itself is suspected to be compromised: an external stop without the core. Stop the core containers, revoke the bot token at the channel's external system, disable the tenants' keys at the model provider.
4. Investigate with `status`, `doctor` and the audit. Release the hold with the P2 operation `hold.release` by someone not below the author in rank: a `farm` hold and an operator's hold are released only by the operator, a code hold `contain_auto` by the tenant admin or the operator; the core itself does not release holds (HLD-2). The operator reverts the external stop (containers, channel token, provider keys) by hand.

### Planned work on a machine

1. A person sets a maintenance window on the machine's node in the monitoring system, for example `db1`; alerts for it are silent there.
2. From v0.2 the core sets by the window a hold on scope `host` with `no_tool`, `pause_action`, reason `maintenance` and a deadline at the end of the window (MON-9); pending cards for the machine are not canceled but wait. Before v0.2 a person sets such a hold themselves: P2 `hold.set`.
3. The work ended earlier: the tenant admin or the operator releases the hold with P2 `hold.release`. Otherwise the hold ends at `until`.
4. While the hold is active, bots do not start, stop or restart services on the machine and do not run monitoring scripts on it: the calls `service/control` and `monitor/script_run` get `denied(held)` (MON-8, MON-10). People manage the services during the work.

### Update a plugin

1. A separate PR: the plugin manifest lists the current and the new pair (digest, fingerprint). A change of a tool description changes what the model sees, so the PR runs the eval cases of this plugin.
2. P2 `apply`. Roll out the new toolhost image; `hello` passes with the new pair.
3. When all toolhosts are on the new image, remove the old pair from the manifest and `apply` again.
4. Urgently, on a vulnerability: first P2 `hold.set` on scope `plugin` with `no_tool`, then git.

### Update a worker or a runtime

1. Make sure the new image understands all `.vN` from the role's `claims` and a P1 version within the core's window.
2. P2 `worker.drain` with the deadline `drain_deadline`; wait for the running work to finish.
3. Replace the image and enroll the worker (P2 `worker.enroll`); `hello` passes.
4. Revoke the old worker with P2 `worker.revoke`; if its runtime kept sessions or memory itself, check the outcome of the erasure request (`erasure.status`).

### Release a change on a test tenant first

Roles, task types, plugins and actions are shared by all tenants, and `apply` changes them everywhere at once. A staged release is done without a new concept:

1. The new version of the role is released as a copy under another name, for example `ops-reader-next`.
2. The workers of the test tenant `demo2` are enrolled with it (P2 `worker.enroll` with the role `ops-reader-next`). People's rights do not change: groups grant `task:<type>`, not roles. During the transition the `demo2` sessions live under another key, because the session key contains the role.
3. After 7 days the change is moved into `ops-reader` for everyone, and the copy is deleted.
4. For a task type, `.v(N+1)` next to `.vN` gives the same (MAN-3).

### New tenant

The recipe in git (`tenants/<name>/`, catalog, groups, roles) is described in `docs/extending.md`; here is what is needed so that the new tenant's alarms reach people and what `doctor` checks per tenant (OBS-11).

1. In the monitoring system: a separate account for this tenant's adapter with rights only on the tenant's node group (MON-4); the items and triggers of the farm template for this tenant (alarm export, the adapter's sign of life, the audit head) from `deploy/monitor/`; a problem webhook action to the address of the tenant's adapter (MON-5).
2. The tenant's catalog: `hosts[].monitor_host` for the tenant's machines, `monitor_scripts_allowed` and `monitor_triggers` (the closed dictionary of scripts and problem classes); no farm machine is in the catalog, otherwise `apply` rejects it (DEP-11).
3. The process of the tenant's `monitor` adapter: enroll the worker (P2 `worker.enroll`) with its worker token and the account credentials from step 1; start it.
4. Check `doctor` for the tenant: the `monitor` worker is in `active`, there are no deliveries for E5 in `planned` older than 15 minutes, `monitor_export_age_s` is fresh, the adapter account sees only the groups of its tenant, the tenant's audit head appeared in the monitoring history (OBS-11, MON-3, MON-4, MON-11).
5. Check the alarm path: a test `info` alarm of the tenant is visible in the monitoring system with the `tenant` tag.

### Suspend and delete a tenant

1. P2 `tenant.suspend`: a `tenant` hold with reason `suspended`. To return: only the operator, P2 `hold.release` of this hold (HLD-2).
2. P2 `tenant.offboard`: credentials are revoked, the provider key is disabled.
3. P2 `tenant.export` to a sink named by the operator.
4. After the export window P2 `tenant.delete`; watch `erasure.status` until `done`.

### A person left or asks to erase their data

1. Temporarily: an admin's `/off`. Nothing is deleted, access is closed at once.
2. For good: P2 `principal.offboard`. Memory and sessions are deleted through an erasure request.
3. A request to erase data: the person asks with the command `/mydata erase` (SEC-11), a person with `admin:people` approves the card, and the core creates an erasure request with scope `(tenant, subject)` and reason `subject_request`; an operator creates the same request with P2 `erasure.request` through `farmctl`; watch `erasure.status`. This is part of v0.1 and cannot be waived on a production tenant (D-76). From v0.2 the person gets an extract by themselves with the command `/mydata export` (SEC-10).
4. Deleted data lives in backups until the end of their period and does not come back after a restore.

### Restore from backup

1. Stop the channels, then the core.
2. Restore the store from a backup on the production machine; the core increments the epoch, cancels pending approvals, moves unclear write calls to `exec_unknown`, replays the erasure log.
3. The operator's reconciliation process passes the provider's spend report, and the core fills in `spend_reconciled` lines (BUD-10). P2 `restore.complete` (or the core completes by itself); check `doctor`. If waiting for the report is not possible: P2 `restore.complete --spend-pending`, work with the alarm `alarm.spend.unreconciled` and halved daily limits, reconcile spend as soon as the report appears (RET-6).
4. Start the workers, the channels last. Tell people that pending approvals must be requested again and requests after the backup moment repeated.

### Rotating secrets

The general rule: everything the core issues or signs carries a key id, and the old and new keys are valid together within the rotation window (SEC-5); two valid values for every secret during a rotation exist already in v0.1 (DEP). A revocation on compromise takes effect from the next call, without an overlap window, and sets a hold without a deadline on the scopes where the secret was used; the operator sets such a hold, and only the operator releases it (SEC-6, HLD-2). Secret recipes, all in v0.1:

| Secret | Order | What `doctor` will show |
|---|---|---|
| the `health` key (deployment configuration, not the store) | add a second value to the core configuration and reload it without stopping; replace the key in the monitoring system poll (the farm template has two key macros, E5 sheet); wait for a successful poll with the new key; remove the old value | the `health` answer did not change to `unauthenticated`; the rotation window is open or closed |
| the P1 key (worker token, including adapters) | P2 `worker.enroll` issues a new token, the old one is valid until `worker.revoke` or the end of the window; the worker restarts with the new one; for the `monitor` adapter one process at a time, so that the alarm export of other tenants does not stall | `last_seen` of the worker with the new token; no deliveries for E5 in `planned` older than 15 minutes (OBS-11) |
| the adapter token in the monitoring system (the adapter account) | create a second token of the same account; restart the adapter process with it; make sure by `monitor_export_age_s` that the export runs; revoke the old token | the tenant's `monitor_export_age_s` is fresh; the account's rights are not wider (MON-4) |
| the problem webhook token | the adapter accepts two values during the rotation; change the value in the monitoring system's webhook action; remove the old one | the counter of rejected webhooks does not grow |
| the model provider key (the tenant's workspace) | create a new key in the workspace; write it into the P4 configuration next to the old one; P4 reloads and takes the new one; revoke the old one at the provider | no `provider_config` for the tenant; the daily GW-9 probe passes |
| the P2 secret of a channel driver (one per driver in a tenant, D-87 (3)) | P2 `worker.enroll` of the channel process with `rotate_p2: true` issues a new secret, the old one is valid until the end of the window; the channel processes of this driver restart with the new one, one at a time | P2 calls of the channel with the old secret after the window are rejected with `unauthenticated` |
| the channel bot token | the channel's external system gives no overlap: change the token, restart the channel adapter; the incoming queue at the external system is kept (DLV-5) | the time of the last successful channel poll is fresh (OBS-10) |
| the audit signing key (head `(epoch, seq, hash, key_id, sig)`) | a new key with a new id; `verify` accepts heads with the old and new id within the window (AUD-6, SEC-5) | `verify` passes over the whole history of heads |

## What is in which version

| Version | What appears |
|---|---|
| v0.1 | telemetry without content (OBS-1); one trace per task tree with `correlationid` and `causationid` in every event, and external tracing context that decides nothing (OBS-2, OBS-3, D-94); `last_seen`, `worker.stale` and loop ticks (OBS-4); the common alarm schema, episodes with `seq` and `repeat_count` (OBS-6); alarm export and core liveness check by the monitoring system (MON-1, MON-2, `docs/monitoring.md`); `doctor` and `status` lines per tenant (OBS-11); behavior with an unavailable store (OBS-8); plugin health and `plugin_unavailable` (OBS-10); holds with the scopes `farm`, `tenant`, `role`, `tasktype`, `worker`, the effects `no_enqueue`, `no_claim`, `cancel_running`, the command `farmctl hold farm` and release by rank (HLD-1, HLD-2, HLD-3, HLD-5); retention classes (RET-1); deletion of `content` and `observation` by defaults with the `retention` loop (RET-2); tombstones (RET-3); erase operations C3, C4, E4 with `runtime_internal`, C1 `file.purge`, E2 `erase`, E5 `erase` and erasure of a person's data at their request `/mydata erase`, non-waivable on a production tenant (RET-4, D-76); backups and restore (RET-5, RET-6); early erasure through the erasure log (RET-7); tenant limits with a queue reserve for urgent tasks (CAP-1); a dependency failure without spending an attempt (CAP-2); storage headroom (CAP-5); the store clock, UTC, the slot format, the event `schedule.skipped`, `skip_if_waiting` and the schedule time zone (TIM-1, TIM-2, TIM-3, TIM-4); rotation recipes for every secret; `operator_access.debug_journal`; the three health indicators (OBS-9); an alarm about an overdue usage reconciliation (OBS-12); `status` lines about the farm manager, its pause and the commission shadow (OBS-11); the other hold scopes and effects, including `no_message`, holds by the seat holder, the auto-stop "secret in outbound" (HLD-6), the auto-stop by spend rate `runaway_spend` (HLD-7); erasing the memory of a retired bot with an erasure request (RET-7); usage reconciliation before readiness after a restore and `restore.complete --spend-pending` (RET-6); backup process reports `backup.report` and the weekly restore check (RET-8); space quotas on volumes with a used space series from the monitoring history (CAP-6); alarms from monitoring problems (MON-5); an alarm export that does not die (MON-11); core drain with reason `draining` in `health` (LCY-6) and the P1 version window (LCY-7) |
| v0.2 | holds by monitoring system maintenance windows with the effect `pause_action` (MON-9); retention periods `Tenant.spec.retention` and deletion of all classes by period (RET-2); E1 `erase` and E4 `purge` for the other runtimes (RET-4); the extract `/mydata export` (SEC-10); delivery rate (CAP-3); dead work and pausing a schedule with a hold (CAP-4); the schedule check before "Accept" (TIM-5); tenant and principal lifecycles, export (LCY-1, LCY-2, LCY-3); worker drain and revocation (LCY-4); plugin update (LCY-5); the metrics catalog |

The improvement cycle, the farm manager, messages between bots, reporters, campaigns and scripts are part of v0.1 and come after the core and the first bot (D-71, D-80); the first measure hold of an incident, holds by the seat holder, and the effects `no_message` and `no_control` appear together with them.

## Why it works this way

- The core cannot report its own death, and the organization already has a monitoring system with alerting, on-call duty and escalation. So it polls core liveness, the "alive" answer requires an answer from the store and the ticks of all loops, and farm alarms reach people only through it (D-59, D-60, D-62).
- One "hold" entity instead of different switches gives one check point in C2, one audit and urgent narrowing without git. A hold only narrows, so even a hold by the holder of the farm manager seat does not become a path to extra rights.
- A restore from backup would otherwise break the non-waivable rules about approval, leases and memory erasure; the epoch, `canceled(restored)` and the replay of the erasure log make it safe.
- Early erasure through the log is needed already in v0.1: a manual erasure of a person's data without the log would come back with the very first restore.
- A provider failure, a core restart and an overload must not burn task attempts: otherwise an hour-long provider outage or a core rollout makes tasks fail en masse.
- The slot format is part of the idempotency key, and the limits and the headroom protect the shared disk: this is cheap to set before the first code and expensive to change after.
- Only someone not below the author in rank releases a hold: otherwise the tenant admin's right `hold.release` would become a way to lift an operator's suspension. Code holds inside a tenant still stay in the tenant's hands, so that nobody waits for the operator for every auto-stop.
- The core knows about backups, alarm export and usage reconciliation only what it was told; so every external process has a report operation and an alarm on its silence (`backup.report`, `monitor_export_age_s`, `alarm.spend.reconcile_overdue`): trouble that nobody can report is worse than an extra line in `doctor`.

## What is not decided

- The default retention periods (`content` 30 days, `metadata` 400 days, memory until `forget`) apply until the organization decides on personal data (D-12); the skeleton needs only the fields and checks.

<!-- generated: rules -->
## Rules of this section

Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md](../conformance/rules.md).

| Rule | About | Since | Non-waivable |
|---|---|---|---|
| [CAP-1](../conformance/rules.md#CAP-1) | Tenant ceilings on counters | v0.1 | no |
| [CAP-2](../conformance/rules.md#CAP-2) | A dependency failure does not spend an attempt | v0.1 | no |
| [CAP-3](../conformance/rules.md#CAP-3) | Delivery rate to a channel | v0.2 | no |
| [CAP-4](../conformance/rules.md#CAP-4) | Dead work and schedule pause | v0.2 | no |
| [CAP-5](../conformance/rules.md#CAP-5) | Storage headroom | v0.1 | yes |
| [CAP-6](../conformance/rules.md#CAP-6) | Volume quotas | v0.1 | no |
| [HLD-1](../conformance/rules.md#HLD-1) | A hold as a recorded object | v0.1 | yes |
| [HLD-2](../conformance/rules.md#HLD-2) | Who sets and lifts a hold | v0.1 | yes |
| [HLD-3](../conformance/rules.md#HLD-3) | How a hold acts | v0.1 | yes |
| [HLD-5](../conformance/rules.md#HLD-5) | Holds are visible and recorded | v0.1 | no |
| [HLD-6](../conformance/rules.md#HLD-6) | Auto-stop "secret in outbound" | v0.1 | yes |
| [HLD-7](../conformance/rules.md#HLD-7) | Auto-stop on spend rate | v0.1 | no |
| [LCY-1](../conformance/rules.md#LCY-1) | Tenant lifecycle | v0.2 | yes |
| [LCY-2](../conformance/rules.md#LCY-2) | Tenant and principal export | v0.2 | no |
| [LCY-3](../conformance/rules.md#LCY-3) | Principal lifecycle | v0.2 | yes |
| [LCY-4](../conformance/rules.md#LCY-4) | Worker drain and removal | v0.2 | no |
| [LCY-5](../conformance/rules.md#LCY-5) | Plugin update and deprecation | v0.2 | no |
| [LCY-6](../conformance/rules.md#LCY-6) | Core drain before rollout | v0.1 | no |
| [LCY-7](../conformance/rules.md#LCY-7) | Worker port version window | v0.1 | no |
| [OBS-1](../conformance/rules.md#OBS-1) | Telemetry without content | v0.1 | yes |
| [OBS-2](../conformance/rules.md#OBS-2) | One trace per task tree | v0.1 | no |
| [OBS-3](../conformance/rules.md#OBS-3) | External trace context decides nothing | v0.1 | yes |
| [OBS-4](../conformance/rules.md#OBS-4) | Worker liveness and loop ticks | v0.1 | no |
| [OBS-6](../conformance/rules.md#OBS-6) | Common alarm schema | v0.1 | yes |
| [OBS-8](../conformance/rules.md#OBS-8) | The store is unavailable | v0.1 | yes |
| [OBS-9](../conformance/rules.md#OBS-9) | Three health indicators | v0.1 | no |
| [OBS-10](../conformance/rules.md#OBS-10) | Health of plugins, channels and the provider | v0.1 | no |
| [OBS-12](../conformance/rules.md#OBS-12) | Alarm about an overdue usage reconciliation | v0.1 | no |
| [RET-1](../conformance/rules.md#RET-1) | A retention class for every stored object | v0.1 | yes |
| [RET-2](../conformance/rules.md#RET-2) | Retention periods and deletion at expiry | v0.1 | yes |
| [RET-3](../conformance/rules.md#RET-3) | Tombstones keep uniqueness | v0.1 | yes |
| [RET-4](../conformance/rules.md#RET-4) | Erase operations in every port with content | v0.1 | yes |
| [RET-5](../conformance/rules.md#RET-5) | A backup is part of the production perimeter | v0.1 | yes |
| [RET-6](../conformance/rules.md#RET-6) | Restore from backup | v0.1 | yes |
| [RET-7](../conformance/rules.md#RET-7) | Early deletion only through the erasure log | v0.1 | yes |
| [RET-8](../conformance/rules.md#RET-8) | Backup reports and restore checks | v0.1 | no |
| [TIM-1](../conformance/rules.md#TIM-1) | One clock, the store clock | v0.1 | yes |
| [TIM-2](../conformance/rules.md#TIM-2) | UTC in contracts, a time zone only for the tenant and the schedule | v0.1 | no |
| [TIM-3](../conformance/rules.md#TIM-3) | Schedule slot format and time transitions | v0.1 | yes |
| [TIM-4](../conformance/rules.md#TIM-4) | Schedule time zone | v0.1 | yes |
| [TIM-5](../conformance/rules.md#TIM-5) | Schedule check before "Accept" | v0.2 | no |
