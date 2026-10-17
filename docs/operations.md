# Operating the farm

## In short

This section describes how the farm lives on ordinary days and on bad days: what the operator sees, how the core shows it is alive, how alarms reach people through the organization's monitoring system, how to stop part of the farm or the whole farm, how long data is kept and how it is erased, what to do about backups, overload, failed dependencies, time, tenants, workers and the rollout of a new core version. It also collects the farm's obligations towards people whose data it handles. The main idea: everything that security and data safety depend on during a bad day is held by core code and by records in the store, not by people's memory and not by the model. Code raises alarms; the monitoring system delivers them to people and checks core liveness from outside; stopping is a recorded "hold" object; early erasure goes through an erasure log that survives a restore from backup. Legal choices (controller, purposes, region, periods, texts for people) belong to the organization (D-12): the skeleton gives the fields that record them and the checks that refuse an empty field or a contradiction. The recipes at the end are sequences of port operations, without the commands of a particular implementation.

## How it works

### What the operator sees

**One trace per task tree.** A task a person enqueued can produce child tasks, tool calls, model calls, approvals and deliveries: one task tree with a root. The core links every event to the root (`correlationid`) and to its direct cause (`causationid`: the parent task, the approval, the event, the incoming message or the schedule slot), and all tasks of the tree share one W3C trace id in the task field `trace` ([OBS-2](../conformance/rules.md#OBS-2)). Spans cover the attempt, the P4 model call, the tool call, the delivery and the decision on an approval; the core assigns the span parent itself by the P3 and P4 handle, so the runtime needs no trace context. Tracing context from outside decides nothing: `baggage` and `tracestate` are dropped at the core boundary, and a foreign `traceparent` is kept only as a link ([OBS-3](../conformance/rules.md#OBS-3)).

**Telemetry without content.** Traces, metrics, process logs and health reports carry only ids, sizes, hashes, times, codes and closed names ([OBS-1](../conformance/rules.md#OBS-1), AUD-2). The OpenTelemetry GenAI conventions include content attributes that instrumentation enables with one setting, so the allowlist lives in `spec/bindings/telemetry/otel.md` with a pinned version and the collector drops the rest. Metric labels come from closed sets; ids of people and chats are never labels. A log that may hold content (a transcript, plugin stderr) is a store of class `content`. Prompts are never logged in production: analysis uses the session transcript or a replay on a test tenant.

**Metrics.** The metrics catalog `spec/bindings/telemetry/metrics.yaml` (v0.2, OBS-13) is a contract for dashboards that survives a change of implementation. Its 14 metrics cover queue, outcomes and duration, model and tools, budget and approvals, deliveries, liveness and holds; only what the farm alone knows from its records. Machine, disk, network and service metrics stay with the monitoring system (D-59). No core decision reads a metric, and a core observation series that matches a catalog metric has the same name (OBV-5).

**P2 `status` and `doctor`.** `status` gives a summary, `doctor` checks consistency and names the mismatches; together they show, as metadata only:

- holds; loop tick ages, `last_seen` of workers and a core drain deadline (OBS-4, LCY-6); the health indicators and store free space (OBS-9, CAP-5);
- per tenant, the alarm path ([OBS-11](../conformance/rules.md#OBS-11)): `monitor_export_age_s`, E5 deliveries in `planned` older than 15 minutes, the adapter account's scope, the audit head's freshness, `seq` and history retention (MON-3, MON-4, MON-11);
- open erasure requests (RET-7); backup age, the last restore check and whether the erasure log backup lags the store backup (RET-8);
- usage reconciliation and an open spend gap (BUD-10, RET-6); the applied commit against the one the operator named (MAN-5); provider caps (GW-9), the `review` line (BUD-13), model `retires_at`, 30-day usage per line (BUD-15);
- per tenant: "improvement cycle disabled" (INS-6), commission lens coverage (COM-7), "manager disabled" (D-77), "shadow disabled" (COM-10), from v0.2 "commission locked" (GOV-35).

Each line lives where its response schema has the field. The alarm path lines are `doctor` checks; the `status` schema has no monitoring lines. Of the per tenant limits, `doctor` and the apply lint carry "improvement cycle disabled" (INS-6), `status` carries "manager disabled" as the list `farm_manager_disabled`, and `commission.status` carries the commission ones (lens coverage, `shadow: off`, `commission_locked`).

**Tenant debug journal.** An operator with `admin:debug` can read a tenant's attempt journals and transcripts. That is a read of the tenant's content, so the tenant decides with `Tenant.spec.operator_access.debug_journal`: `deny`, `notify` (the default: an audit record and a code message to the tenant admins) or `allow` (audit record only). An external client learns about every look into its data, and the operator can still analyze an incident. The journal is described under [People's data](#peoples-data).

### Liveness and health

**Workers.** A heartbeat belongs to a lease, so an idle worker sends none. The core instead keeps `last_seen`, the time of the worker's last P1 call of any kind, including a long-poll `claim` and `msg.receive`. A worker unseen for longer than two long-poll periods gives a `worker.stale` event and an alarm ([OBS-4](../conformance/rules.md#OBS-4)). This is an observation, not a worker state, and the worker's own "I am `idle`" does not change it (STA-2). Its leases expire, the `reaper` loop returns the work to the queue, and the vanished worker gets nothing new because it does not call `claim`.

**Background loops.** Core loops are deterministic, never call the model, are idempotent and safe with two instances. Each marks a tick in the store; a missed tick of any loop makes P2 `health` answer "not alive". The operations loops:

| Loop | What it does | Version |
|---|---|---|
| `worker-liveness` | counts `last_seen`, writes `worker.stale` and an alarm | v0.1 |
| `hold` | keeps hold deadlines, applies effects to pending approvals and deliveries, sets auto-stops | v0.1 |
| `alarm` | keeps alarm episodes, puts every episode change as a delivery for E5, re-exports open episodes after an outage (MON-1, MON-11) | v0.1 |
| `erasure` | sends erasure requests to the stores, collects outcomes, raises an alarm at the deadline | v0.1 |
| `schedule` | enqueues a task for every slot, in UTC by the schedule time zone | v0.1 |
| `retention` | deletes by the periods of the retention classes (RET-2, RET-10) | v0.1 |
| `health` | computes the three health indicators and, without profile `monitor`, the age of backups and restore checks (OBS-9, RET-8) | v0.1 |
| `audit-head` | at least once a day puts every tenant's audit chain head as a delivery for E5 (MON-3) | v0.1 |

The other loops (`reaper`, `deadline`, `delivery`, `approval-expiry`, `budget`, `incident`, `identity` and the rest) follow the same contract; the full list is in `spec/ports/background-loops.md`.

**Plugins, channels and the provider.** One schema, `spec/model/health-report.schema.json`, describes the health of every part: component, state (`ok`, `degraded`, `down`), checks from a closed list and time, with codes and no error texts, since an external error text may carry foreign text ([OBS-10](../conformance/rules.md#OBS-10)). A call to a plugin in `down`, or on a toolhost that is not visible, gets `plugin_unavailable` at once, so the model decides itself instead of waiting for the lease deadline. `down` only narrows dispatch.

**Three health indicators.** Each part may be alive while the farm is stuck. The core computes ([OBS-9](../conformance/rules.md#OBS-9)):

| Indicator | What it catches |
|---|---|
| age of the oldest task of priority classes 1 and 2 in `submitted` (DSP-5) | the queue is stuck and people wait for an answer |
| share of deliveries `delivered` within 10 minutes of creation | the channel or the delivery loop does not work |
| tick age of every loop | one loop hung while the others work |

Thresholds come from the deployment binding; an indicator past its threshold raises `alarm.health.urgent_age`, `alarm.health.delivery_share` or `alarm.health.loop_tick`. The values are in the daily summary and in the P2 `health` answer, so people learn about a stuck farm even when its own channel is broken.

**Core liveness from outside.** The core cannot report its own death, so a process outside it polls P2 `health` once a minute ([MON-2](../conformance/rules.md#MON-2), D-60): the organization's monitoring system, or without one the deployment watchdog `kind: watch`, which alerts people its own way (`watch.notify_ref`, D-88). The poll uses a separate key that works only for `health`; the key lives in the deployment configuration, the handler answers without the store and outside the worker pool (a hung store must give "not alive", not silence), and during rotation two key values are valid. "Alive" requires an answering store and on-time ticks of all loops. The answer holds only numbers:

```yaml
# P2 health answer (sketch)
alive: true                      # with false: reason store_unavailable | loop_stale | not_ready | draining
store_free_bytes: 88046829568    # CAP-5
store_free_ratio: 0.41
oldest_urgent_age_s: 12          # OBS-9
delivered_10m_ratio: 0.99
loop_tick_age_s: {reaper: 8, delivery: 3, alarm: 11, hold: 9}
monitor_export_age_s: {demo: 40, farm: 12}   # MON-11
```

During a planned drain the answer is `alive: false`, `reason: draining` with `drain_until_s`, and the farm template stays quiet within that time (LCY-6). Otherwise no answer or "not alive" beyond the threshold makes the monitoring system wake people by its own path. This catches a stopped core, a hung or full store, a hung loop, a switched-off machine. The monitoring system does not run on the core machine and holds no farm credential but the `health` key; the deployment lint checks that no farm process but the adapter and the watchdog holds that key, while the machine the monitoring system runs on is not in the description and stays the installation's to keep (DEP-10). Each adapter process also writes a tick item once a minute, and the template raises a problem when it goes silent: then the alarm export has stopped.

### When the store is unavailable

The store (C1) holds all farm state. If it does not answer, the core neither loses work nor starts it ([OBS-8](../conformance/rules.md#OBS-8)): P1 answers `not_ready` with `retry_after_s`, P4 starts no model calls (a result with nowhere to be written is money spent for nothing), the channel does not move the external offset so incoming messages arrive again (DLV-5), loops mark no ticks and `health` says "not alive". A lease that expired meanwhile and was not extended after readiness ends with `core_unavailable` and does not spend the attempt (FEN-5). The core leaves this mode only when the store answers and every loop has ticked again.

### The monitoring system

The organization already runs a monitoring system (for example Zabbix) that watches machines and services and knows how to wake people, run on-call rotations, escalate and silence. The farm does not repeat this (D-59). It adds what the monitoring system cannot see (tasks, bots, money, secrets, the farm manager) through one edge port, E5 Monitor; the Zabbix 7.0 binding is in `spec/ports/e5-monitor.md` (D-66). Linking to a monitoring system is the conformance profile `monitor` (D-88); a farm without it is described at the end of this part.

| What | Monitoring system | Farm |
|---|---|---|
| machines, services, disks, load | observes, raises problems | reads with the `monitor` plugin; takes problems on catalog nodes as input |
| tasks, bots, money, secrets, workers, core store | receives farm alarms as tagged values | measures with code, raises alarms (INJ-7) |
| alerting, "Acknowledged", escalation, on-call, silence | does all of it | nothing of its own with profile `monitor` (D-62) |
| core liveness, audit witness | polls `health`; keeps the audit head | answers; writes the head daily |
| maintenance windows, scripts | people set windows; it runs scripts | holds its bots (v0.2); bots run allowed scripts |

**The adapter and its accounts.** The `monitor` adapter is an E5 edge process like a channel adapter: it takes deliveries for E5 by P1 `claim`, and the core never calls the monitoring system itself. An adapter serves one tenant (TEN-4) with its own process, worker token and monitoring account; one more process for `farm` alone maintains the farm nodes and template ([MON-4](../conformance/rules.md#MON-4), D-61). A tenant account sees only its tenant's node groups, so the tenant boundary is held by the monitoring system's permissions, not only by adapter code. No farm account can acknowledge, close, silence or reclassify a problem or touch notification actions and windows; it can only add a message. `doctor` fails if any account's rights are wider, including script permission on a farm node.

The farm template lives in git in `deploy/monitor/`: items for alarm export, the adapter tick, the audit head, the `health` poll and triggers on tags. An organization machine is linked to a catalog resource by `Catalog.spec.hosts[].monitor_host`, and only nodes linked this way are read, accept problems or give maintenance holds. A farm machine (core, store, channel, adapter, backups) cannot be a catalog resource, and a machine, a (machine, service) pair or a (node, script) pair belongs to one tenant only; `apply` rejects anything else (DEP-11, D-75). Otherwise every prohibition "on farm nodes" would rest on nobody writing the core machine into a catalog.

**Alarm export.** For every alarm episode change the core puts a delivery for E5 in the same transaction as the event, and the adapter writes it as a value on a farm node ([MON-1](../conformance/rules.md#MON-1), [MON-12](../conformance/rules.md#MON-12)). People configure alerting in the monitoring system by tags:

| Tag | Value | Example |
|---|---|---|
| `tenant` | tenant of the alarm; `farm` for the farm itself | `demo` |
| `alarm` | name without `alarm.` and `.vN` | `queue.age.p95` |
| `subject` | catalog id of the subject | `db1` |
| `severity` | `info`, `warn`, `page` (Information, Warning, High) | `page` |
| `scope` | `tenant` or `farm` | `tenant` |
| `origin` | `code` for a farm measurement, `monitor` for an alarm raised on a monitoring problem | `code` |
| `key` | `<tenant>:<alarm>:<subject>`, so recovery closes exactly its own problem | `demo:queue.age.p95:demo` |

The farm builds no notification text: the monitoring system's template builds it from the tags. An alarm with `origin: monitor` is usually excluded from the notification action, since the original problem already woke people.

**The export does not die.** A dead alarm delivery would mean nobody was woken, or a problem left open forever. So export deliveries retry with a pause of up to 10 minutes while the episode is open, go in `seq` order, and after an outage the `alarm` loop re-exports every open episode and audit head ([MON-11](../conformance/rules.md#MON-11)). If a tenant's export fails for 15 minutes, the core raises `alarm.monitor.export_stalled` and sends the tenant admins and the operator the code message `monitor_export_stalled`, at most every 6 hours: the only message that bypasses the monitoring system, and it says only that alarms are not getting through.

**Audit chain head.** The audit is a hash chain (AUD-6). To make an edit made directly in the store visible, the `audit-head` loop puts the signed head `(epoch, seq, hash, key_id, sig)` of every tenant and of `farm` into a `monitor_audit_head` delivery at least once a day, and the adapter writes it into the monitoring history ([MON-3](../conformance/rules.md#MON-3)). The signature is needed because the adapter is outside the trust boundary and could write any value with its own token. `seq` decreases only with a growth of `epoch` after a restore. P2 `audit.verify` checks every head in the history, not only the latest; `doctor` reads the item's actual history retention and warns below 90 days. The witness holds only while the store operator does not also administer the monitoring system: a separation of duties, not a property of code.

**Monitoring problems in the farm.** A problem the monitoring system finds on a catalog node becomes farm input ([MON-5](../conformance/rules.md#MON-5), D-63), a special case of the external event input (ING-1, ING-2):

1. The monitoring system sends a webhook to the tenant adapter on open and on close; the adapter checks the token over TLS and that the node is linked to its catalog. A problem on an unlinked node is dropped and counted.
2. The adapter submits the event through P1 `submit_activity` as the `service` principal `hook:monitor`; the core writes `ext.monitor.problem.v1` with closed fields only. Trigger names, tags and descriptions never enter: people write them and they can contain anything (INJ-16).
3. The key `system:ingress:monitor:<eventid>:<state>` makes a repeated webhook harmless.
4. The class comes from the catalog by `triggerid` (`Catalog.spec.monitor_triggers: [{triggerid, class, item}]`), so editing trigger tags cannot raise `page`, open an incident or set a hold. An unknown trigger gives class `unknown`.
5. A route enqueues a `job` task without a model that rechecks the problem through the API, reads the number from item `item` and raises or closes the farm alarm by the class table `spec/model/monitor-map.yaml`.
6. From there it is an ordinary alarm: the incident catalog opens an incident (MGR-7), and the alarm is exported back with `origin: monitor`.

| Class | Farm alarm | Severity | What next |
|---|---|---|---|
| `disk_free` | `alarm.disk.free.low` | `page` | incident `disk_full` if the volume has a full set of quotas (MGR-33) |
| `disk_fill` | `alarm.disk.fill.rate` | `warn` | fill rate incident |
| `unknown` | `alarm.monitor.problem` | `warn` | a record and an export only |

Severity comes from the class table, not from the problem, or a trigger editor could raise `page` for the farm. The monitoring system of a task's own tenant is a trusted source (D-82): a route task on a catalog trigger gets no `untrusted_seen` and records its source as `monitor`; a trigger outside the catalog sets the flag (INJ-13).

**Bots read monitoring.** The `monitor` plugin is read-only ([MON-6](../conformance/rules.md#MON-6)) and answers only about nodes of the task tenant's catalog; plugin code checks this, not the model.

| Tool | Returns | Untrusted |
|---|---|---|
| `monitor/problems` | problems with `triggerid`, class and item from the catalog, resource, severity, state | no |
| `monitor/maintenance` | whether the machine is in maintenance now, and since when | no |
| `monitor/item_history` | numeric history and trends; a text item gets `item_not_numeric` | no |
| `monitor/item_text` | text and log item values | yes, source `monitor_item_text` |
| `monitor/problem_comments` | comments by people and bots | yes, source `monitor_comment` |

**Services and scripts.** Bots start, stop and restart services on their tenant's catalog machines ([MON-8](../conformance/rules.md#MON-8), D-67), and run monitoring system scripts on their tenant's nodes ([MON-10](../conformance/rules.md#MON-10)). The catalog declares the managed services per machine (`services[]`: `name`, `unit`, `control` from `start`, `stop`, `restart`, `via: host` or `via: monitor` with `scripts`, `no_card`) and the allowed scripts (`monitor_scripts_allowed: [{scriptid, name, hosts}]`). `no_card` defaults to `[restart]` and may never contain `start` or `stop` (D-74).

```yaml
# tenants/demo/catalog.yaml (fragment)
spec:
  hosts:
    - id: web1
      monitor_host: web1.example.org
      monitor_scripts_allowed:
        - {scriptid: "7", name: nginx-cache-clear, hosts: [web1]}
        - {scriptid: "9", name: app-api-restart, hosts: [web1]}
      services:
        - {name: nginx, unit: nginx.service, control: [restart, start, stop], via: host}
        - {name: app-api, control: [restart], via: monitor, scripts: {restart: app-api-restart}}
    - {id: db1, monitor_host: db1.example.org}
    # core1.example.org cannot appear here: it is a farm node (DEP-11)
  monitor_triggers:
    - {triggerid: "23801", class: disk_free, item: "31044"}
```

The role manifest grants the permission (`service_control: [nginx]`, `monitor_scripts: [nginx-cache-clear]`); a bot may touch the intersection of catalog and role. An initiator also needs their own capability `service.control:<service>` (D-73); `system` tasks need an explicit line in `system-ops`, the farm manager in `farm-manager-ops` (APR-18). A person with the capability can write `/service restart nginx`; the same tool runs.

The model chooses only from closed lists (`service/control {service, verb}`, `monitor/script_run {resource, script}`); code takes the machine, unit and script id from the catalog. `via: host` uses the OS mechanism `unit` (`spec/bindings/os/systemd.md`) with an access rule `apply` generates from the catalog (DEP-4). Scripts go as a `monitor_script` delivery: the adapter checks the node and that the `scriptid` is in the list the monitoring system gives its account (`ext.monitor.scripts.v1`), runs it once and returns the outcome code; the output lies in the task journal as untrusted text. A restart or script goes without a card under an audit record `service_grant`; start and stop go with a card (APR-3, APR-7); a task that has seen untrusted text hands any of them to a person (INJ-15, D-70).

Guards, which the catalog may tighten: 6 actions per hour per service or node script and 30 per tenant, then `service_rate_limited` and `alarm.service.control.rate`; one operation per service or node at a time; holds give `denied(held)`; code verifies the service 2 minutes after a stop or restart. Every action leaves an audit record, a journal entry and a code note `[botaru <bot_id>]` (template `service_control_done`) on the related open problem.

**Bot notes (v0.2).** A role with `outputs: [monitor_note]` may add a note of up to 1000 characters to a problem on its own tenant's node (MON-7, D-64). The monitoring system is read by people outside the tenant, so a note has the ceiling `summary`, passes redaction and the outgoing secret check (INJ-4, INJ-11) and the tenant canary check (TEN-5); at most 10 notes per hour per tenant. A note decides nothing: acknowledging, closing, severity and silence stay with people.

**Maintenance windows (v0.2).** People set a window in the monitoring system; the farm then stops its own bots on that machine ([MON-9](../conformance/rules.md#MON-9)). The adapter polls the maintenance status of catalog nodes every `maintenance_poll_s` (60 s by default, at most 300) and sends `ext.monitor.maintenance.v1` with `until` = poll time plus two intervals; it does not compute window schedules, which would rebuild the monitoring system's scheduler (D-100). The core sets a `host` hold with `no_tool` and `pause_action`, reason `maintenance`, author `contain_auto`, and each later poll moves `until` forward. Reading tools are held too, because people work on the machine. Pending approvals wait instead of being canceled: windows are set by people who are not farm principals, and must not become a way to cancel approvals; `hosts[].maintenance_hold: true` gives `no_action` instead. Slots that need the machine are skipped with `held`. The hold ends at most two poll intervals after the window or at `hold.release`, after which the same window sets no new hold. Before v0.2 a person sets it by hand.

**Installation without a monitoring system.** A production farm can run without one, even on a single server (`examples/deploy/single.yaml`). The core then delivers alarms itself as the code text `alarm_notice` to the tenant admins, with `page` repeating every 30 minutes until `incident.ack` and no escalation ([MON-13](../conformance/rules.md#MON-13)); the watchdog polls `health`; backups go encrypted to external storage; the E5 parts (export, problems, the plugin, scripts, notes, windows) are absent; and there is no external audit witness, which `doctor` names as the price. Adding a monitoring system later means declaring the profile and the adapters, and `alarm_notice` stops.

### Alarms

An alarm is an event `alarm.<name>.vN` produced only by code: the core or a `job` worker (INJ-7). Text from logs, messages and model answers can neither raise nor suppress one. All alarm data follows `spec/model/event-data/alarm.schema.json` ([OBS-6](../conformance/rules.md#OBS-6)):

```yaml
alarmkey: "disk.free.low:db1"   # name + subject id
state: firing                   # firing | resolved
severity: page                  # info | warn | page
subject: {kind: resource, id: db1}
values: {free_pct: 7}           # numbers and enums only
```

An alarm lives in episodes (`spec/lifecycles/alarm.md`): `firing` opens one, `resolved` closes it, `seq` counts changes. A repeated `firing` only increments `repeat_count`: a once-a-minute check gives one event and one export, not sixty an hour. An installation has one path of alarms to people: export to the monitoring system with profile `monitor`, core delivery of `alarm_notice` without it (MON-1). Farm channels never carry alarms to people otherwise, and there are no alarm routes or rights to receive alarms.

Alarms raised by operations code:

| Alarm | When | Rule |
|---|---|---|
| `alarm.worker.stale` | a worker unseen for two long-poll periods | OBS-4 |
| `alarm.store.free.low` | store volume below the soft threshold | CAP-5 |
| `alarm.erasure.overdue` | an erasure request open past its deadline | RET-7 |
| `alarm.schedule.refused` | the enqueue refused a schedule slot for a reason other than a hold or a capacity limit, for example a missing line in `system-ops` or money; it resolves at the next enqueued slot | TIM-3, PRM-10 |
| `alarm.provider.spend_cap` | the provider refused on the spend cap | CAP-2 |
| `alarm.hold.auto` | the core set an auto-stop | HLD-6, HLD-7, CAP-4, HLD-9 |
| `alarm.health.urgent_age`, `.delivery_share`, `.loop_tick` | a health indicator past its threshold | OBS-9 |
| `alarm.backup.overdue`, `alarm.backup.restore_check_overdue` (`warn`) | without profile `monitor`: no good backup report for 26 hours, or no good restore check for 8 days, or a failed one | RET-8 |
| `alarm.spend.unreconciled` (`warn`) | a restore completed with `--spend-pending`; daily limits are halved meanwhile | RET-6 |
| `alarm.spend.reconcile_overdue`, `alarm.spend.estimate_drift` (`warn`) | no reconciliation for `spend.reconcile_overdue_days` (35), or the daily sum drifts from the provider's | BUD-10 |
| `alarm.monitor.export_stalled` (`warn`) | export failing for over 15 minutes | MON-11 |
| `alarm.incident.unhandled` (`page`) | no operation and no P2 `incident.ack` within `page_if_unhandled_after` | MGR-7 |
| `alarm.manager.breaker` (`page`) | the farm manager seat safeguard tripped | MGR-14 |
| `alarm.disk.free.low`, `alarm.disk.fill.rate`, `alarm.monitor.problem` | a monitoring problem rechecked by code | MON-5 |
| `alarm.service.control.rate` (`warn`, `page` for a stop) | a service or script action hit the limit | MON-8, MON-10 |

Other parts raise their own alarms by the same schema, for example `alarm.task.unclaimed` (DSP-8), `alarm.task.stalled` (CNT-4) and `alarm.manager.absent` (MGR-2); the farm manager's incident catalog opens incidents on code alarms (`docs/farm-manager.md`). A new alarm kind is a `job` task type emitting `alarm.<name>.vN` by the common schema; the export happens by itself, and alerting on the new tag is configured in the monitoring system. An agent type does not produce alarms.

### Hold

**What a hold is.** A hold (`Hold`) is a recorded object: a scope, effects from a closed list, a reason, a deadline `until` (or none) and an author ([HLD-1](../conformance/rules.md#HLD-1)). It belongs to one tenant (TEN-1): holds on `farm` and `seat` are records of `farm`. While it is active, C2 denies what it forbids at every check point, before the rights formula, as a non-waivable ban (PRM-2, PRM-3). A hold deletes nothing and changes no manifest. Emergency stop, tenant suspension, worker quarantine, urgent plugin recall, schedule pause and maintenance stop are all holds; there are no other switches. Its lifecycle is in `spec/lifecycles/hold.md`.

```yaml
# P2 hold.set: the request body; the core writes id, author, source and time
scope: {kind: tasktype, tenant: demo, name: report.daily.v1}
effects: [no_enqueue, no_claim, cancel_running]
reason: runaway_loop                # from reasons.yaml
until: "2026-10-07T18:00:00Z"       # or null: until release
```

**Scopes and effects** are closed lists in `spec/model/holds.yaml`.

| Scope | Covers | Who sets it besides the operator | Version |
|---|---|---|---|
| `farm` | the whole farm | nobody: operator only, also `farmctl hold farm` | v0.1 |
| `tenant` | one tenant | a tenant person with `hold.set:<scope>` | v0.1 |
| `role`, `tasktype` | a role or task type | that person; an auto-stop; the farm manager seat holder | v0.1 |
| `worker` | one worker (quarantine) | that person; the seat holder | v0.1 |
| `plugin`, `action`, `channel` | a plugin, action or channel of a tenant | that person | v0.1 |
| `host` | a catalog machine | that person; from v0.2 a maintenance window (MON-9) | v0.1 |
| `resource`, `bot_group`, `bot` | a catalog resource, a bot group or a bot | that person; the seat holder | v0.1 |
| `seat` | the farm manager seat | operator with `admin:farm` only (MGR-15); the seat safeguard (MGR-14) | v0.1 |
| `schedule` | one schedule | that person; an auto-stop (CAP-4) | v0.2 |
| `governance` | one governance case | a person with `admin:governance` in its tenant (GOV-14) | v0.2 |

| Effect | Forbids | Version |
|---|---|---|
| `no_enqueue` | new tasks in the scope (`rejected(held)`), except core tasks for erasure and export | v0.1 |
| `no_claim` | dispatch of work of the scope | v0.1 |
| `no_message` | sending bot messages and wake-ups; entries accumulated by an attempt are dropped at `complete` (`discarded(held)`); reading inboxes is allowed | v0.1 |
| `cancel_running` | running attempts: `cancel_requested` in the heartbeat answer, handles revoked, attempt ends `held` without being spent | v0.1 |
| `no_tool` | tool calls: `denied(held)`, including a call still `queued` | v0.1 |
| `no_action` | new `request_action` and decisions on pending ones; pending approvals become `canceled(held)` | v0.1 |
| `no_delivery` | deliveries to the scope: `withheld`, except messages to admins about the hold | v0.1 |
| `no_control` | control operations of the seat holder | v0.1 |
| `pause_action` | new `request_action`; pending approvals wait, their deadline extended by the paused time (at most 7 days), the card marked paused; `no_action` wins ([HLD-8](../conformance/rules.md#HLD-8)) | v0.2 |

**How a hold acts.** From the next check, without `apply` or restart ([HLD-3](../conformance/rules.md#HLD-3)). A write call in `queued` hit by `no_tool` or `no_action` gets `denied(held)` and its approval ends `exec_failed(held)`, so nothing runs late (APR-7). A write call the toolhost already took is not revoked: a write to the world that started cannot be recalled, and its outcome is recorded (ORC-11). While a hold with `cancel_running` or `no_claim` covers a task, `claim` does not dispatch it, or the task would loop between dispatch and cancel. Campaigns in the scope pause in the same transaction (ORC-11); a script step rejected by a hold becomes `failed(held)`, and from v0.2 a hold on a run pauses the whole run (SCR-10). A person's hold on a `bot` moves its identity to `suspended` until the last such hold ends (IDN-9); a seat holder's hold on a bot only delays its tasks (MGR-9). An urgent recall of a plugin, action, task type or channel is a hold first and a git removal later, so it does not wait for review.

**Who sets and releases.** A hold only narrows and cannot become a path to extra rights ([HLD-2](../conformance/rules.md#HLD-2)). Holders of `hold.set:<scope>` set holds in their own tenant through `farmctl` or a direct chat command (PRM-15); the rights are privileged and granted only through `farmctl` (PRM-13); holds on `farm` and `seat` are operator operations (PRM-14). Code sets holds by an auto-stop rule, as the first measure of an incident class (MGR-7) and from v0.2 by a maintenance window, all with author `contain_auto` and the source written next to it. The model, a worker, a route, a schedule and a coordinator bot never set or release holds.

The farm manager seat holder with the current epoch sets and releases its own holds through `control.apply` (MGR-9), within limits code computes (MGR-14, MGR-37): targets from the list built for the task's basis, at most `hold_ttl_max` (2 hours), no protected targets, never more than a quarter of a role (that becomes a `control` card for people). Overruns (`control_rate_limited`), 5 refusals in 10 minutes or a spend jump trip the safeguard: a `no_control` seat hold that only an operator with `admin:farm` releases. A replaced holder's holds live at most 15 more minutes (MGR-1); when an incident stops the seat they live out their deadline (GOV-22). Details are in `docs/farm-manager.md`.

Release requires `hold.release` on the scope and a rank not below the author's: operator, then tenant admin, then the seat holder. `hold.release` alone is not enough; otherwise a `demo` admin could lift a suspension for overspending.

| Who set the hold | Who releases it |
|---|---|
| the operator (including `suspended`, and `compromised` at a secret revocation, SEC-6) | only the operator |
| code inside the tenant (`contain_auto`) or a tenant person with `hold.set` | the tenant admin or the operator |
| the seat holder | itself (own holds), the tenant admin through P2 `control.revert`, the operator |
| the seat safeguard, or anyone on `farm` and `seat` | only the operator |

The core never releases a hold by itself: not on closing an incident, clearing an alarm or deleting a maintenance window. A code hold nobody releases ends at its deadline.

**Auto-stops.** Code sets holds only by rules from a closed list; a new rule needs an ADR of the skeleton, because a hold changes availability for everyone. Each raises `alarm.hold.auto`. Injected text can trigger an auto-stop but not bypass it: a deliberate fail-safe.

| Trigger | Hold | Rule |
|---|---|---|
| the channel refused twice within an hour to send a role's message because it found a secret (INJ-11) | `role`, `no_delivery`, no deadline | [HLD-6](../conformance/rules.md#HLD-6) |
| the seat holder exceeded its limits | `seat`, `no_control`, no deadline | MGR-14 |
| tenant spend in a sliding hour above `budgets.hour_eur` (default `month_eur`/100) | `role` with `no_claim` on every agent role, reason `runaway_spend`, one hour; `job` tasks run | [HLD-7](../conformance/rules.md#HLD-7) |
| the last 3 slots of a schedule ended `attempts_exhausted` or `runs_exhausted` (v0.2) | `schedule`, `no_enqueue`, reason `schedule_failing` | [CAP-4](../conformance/rules.md#CAP-4) |
| 3 confirmed incident links to one role's work in 90 days (v0.3) | `role`, `no_action`, reason `role_incidents` | [HLD-9](../conformance/rules.md#HLD-9) |

The threshold of the role hold is the same for every role; a tenant may only make it stricter in its catalog with `role_incident_hold: {links: 2, window: 180d}` (at most 3 links, a window in days only, from `90d` to `365d`, HLD-9). The schema refuses a looser value, and a field left out keeps the default.

A series of requests with a foreign tenant or foreign handles from one credential is not a hold but a move of the worker to `blocked` (TEN-2).

**Ready-made sets.**

| What | Hold |
|---|---|
| tenant suspension (P2 `tenant.suspend`) | `tenant`, reason `suspended`, `no_enqueue`, `no_claim`, `no_tool`, `no_action`, `no_delivery` |
| worker quarantine | `worker`, `no_claim`, `cancel_running` |
| urgent plugin recall | `plugin`, `no_tool` |
| a leaked bot token | `channel`, `no_delivery`, `no_enqueue` |
| maintenance on `db1` | `host`, `no_tool`, `pause_action`, reason `maintenance` (from v0.2 set by the window) |
| suspected theft of the core key | `farm`, `no_action`, `no_tool` |

**Emergency stop of the whole farm.** `farmctl hold farm` writes a `farm` hold directly to the store, without a running core and without a channel; `farmctl` on the core machine with core credentials counts as part of the core, and no other process reaches the store (DEP-8). A hold does not help when the core itself is compromised; for that there is an external stop (containers, bot token, provider keys), tested once on a test tenant before the first production one. Both are in the recipes.

### Retention and erasure

**Retention classes.** Every stored object kind and every store belongs to exactly one class ([RET-1](../conformance/rules.md#RET-1)); a schema declares it in `x-botaru-retention` and skeleton CI fails without it. Classes and defaults are in `spec/model/retention-classes.yaml`:

| Class | What is in it | Default period | Deleted |
|---|---|---|---|
| `content` | task bodies, results, artifacts, files, attachments, sessions, transcripts, attempt journals, bot messages, sink archive, channel cache | 30 days; sessions of roles with `memory: none` 7 days | one object at a time |
| `memory` | memory records | until `forget`, offboarding or the end of the tenant | only by an erasure request |
| `metadata` | audit, transition journal, events, usage, decisions | 400 days | only by whole months |
| `tombstone` | tombstones and the erasure log (ids, key hash, time) | while the tenant lives | by whole periods |
| `observation` | series points and rollups | 14 and 93 days (OBV-4) | by whole periods |

**Periods.** The `retention` loop deletes by period. In v0.1 it deletes only `content` and `observation` by the defaults ([RET-2](../conformance/rules.md#RET-2)); without this the store would grow until v0.2, and an operator at the CAP-5 threshold would have no right to clean up old sessions. From v0.2 the tenant sets `Tenant.spec.retention` within the class bounds (RET-9), and the loop deletes every class in every copy the farm keeps, no later than 24 hours after the period ([RET-10](../conformance/rules.md#RET-10)): C3 `prune`, C1 `file.purge` with corpus fragments (KNW-5) and cached tool output (PRM-12), `task.purge`, and E4 `purge` inside runtimes. A shorter `metadata` period applies only to records written after it, so a manifest edit cannot erase audit already written (D-108). Metadata is never edited row by row: the audit is append-only (AUD-3), and deleting `principal` and `Identity` rows de-identifies it without editing. A retired bot's seal does not extend periods (MEM-10); at the end of the seal its sealed data is erased by a request with reason `bot_retired` (MEM-9).

**Tombstones.** Deletion does not free uniqueness ([RET-3](../conformance/rules.md#RET-3)): a deleted task leaves `(tenant, idempotency_key hash, task id)`, and a repeat of its key gets `gone`, not a new task (IDM-1). Ids are never reused. A tombstone stores no hash of an external id: a phone number or an email address can be found by brute force.

**Erasure request and erasure log.** Any erasure before the class period goes only through an erasure request (`ErasureRequest`), already in v0.1 ([RET-7](../conformance/rules.md#RET-7)). Reasons:

| Reason | Created by | Scope |
|---|---|---|
| `forget` | `/forget` by the owner (MEM-4) | record id or `(tenant, subject)` |
| `offboarding` | P2 `principal.offboard` | `(tenant, subject)` |
| `bot_retired` | the core at retirement (sessions inside the runtime) and at the end of the seal | session ids; the bot identity id |
| `tenant_delete` | P2 `tenant.delete` (v0.2) | `(tenant)` |
| `worker_revoke` | P2 `worker.revoke` of a worker whose runtime keeps state (v0.2) | worker id |
| `subject_request` | an approved `/mydata erase` or `erase_match`; an operator through `farmctl` | `(tenant, subject)` or found object ids |
| `manual` | an operator, P2 `erasure.request` | `(tenant)`, `(tenant, subject)` or object ids |

A request holds the scope, reason, author, time and the outcome per store (`done`, `failed`, `not_applicable`), never content. It enters the erasure log before the first byte is deleted; the `erasure` loop sends it to the stores and keeps it open, with `alarm.erasure.overdue` at the deadline, until every store answered `done` or `not_applicable`. The operator creates and retries requests (`erasure.request`, `erasure.retry`); tenant admins see their own (`erasure.status`, `erasure.list`). No process and no `farmctl` command deletes content around a request: data erased by hand would come back with the first restore from yesterday's backup. The erasure log is backed up separately and at least as often as the store, and replayed after a restore (RET-5, RET-6). The lifecycle is in `spec/lifecycles/deletion-request.md`.

**Erase operations in the ports.** Every port that stores content erases by `(tenant)`, `(tenant, subject)`, bot identity id and object ids ([RET-4](../conformance/rules.md#RET-4)):

| Store | Operation | From |
|---|---|---|
| sessions (C3) | `delete`, `purge` | v0.1; scope `(tenant)` v0.2 |
| memory (C4) | `erase` | v0.1 |
| files and task content (C1) | `file.purge`, `task.purge` (a tombstone remains) | v0.1 (D-76) |
| sink archive (E2) | `erase` | v0.1 |
| runtime with `runtime_internal` (E4) | `purge` (RT-1, RT-2) | v0.1 |
| monitoring system (E5) | `erase`: `(tenant)` removes the tenant's template items; `(tenant, subject)` is `not_applicable`; for note ids a binding whose API cannot delete notes answers `failed`, declared in advance, which does not keep the request open | v0.1 |
| channel attachments and cache (E1) | `erase`, `match`; until then a channel declaring `driver.stores_content: none` gets `not_applicable` with `basis: declared_none` written by the core (D-86 (8)) | v0.2 ([RET-11](../conformance/rules.md#RET-11)) |
| any other runtime (E4) | `purge` of working files, caches and logs | v0.2 |

`not_applicable` without a `basis` is not accepted. Erasure by subject carries `needles` (the person's handles and addresses), kept until the end so an edge store knows whom to delete. Every such adapter passes the `erasure` suite. A content store without its own operation answers `failed`, and the request stays open with an alarm.

**What erasure does not reach.** The model provider keeps requests by its own terms, recorded in the provider binding and checked against the privacy block (GW-15). The conversation in the messenger cloud stays there; the channel binding records what the bot can delete. History in the monitoring system contains no data about people.

### Backups and restore

**A backup is production.** It holds content, so it follows the rules of a production machine ([RET-5](../conformance/rules.md#RET-5), DEP-1): encrypted, operator-only, not on the core machine but on a farm machine `backup1` (DEP-11) or in external storage (`backup.target: external`, D-88). For external storage the backup is encrypted on the core machine with `backup.key_ref`, which the storage does not have, and the storage credentials can only write new objects; a production installation without a second machine must do this (`backup_external_required`) and runs its restore check on a temporary VM. A backup lives no longer than the longest `content` period plus the backup window, so erased data leaves backups by itself. The skeleton does not choose a backup method or count.

**After a restore**, before readiness, the core ([RET-6](../conformance/rules.md#RET-6)):

1. increments the store epoch, part of every lease token and handle, so all work and handles of the old epoch get `fenced` (FEN-2);
2. moves `pending` approvals to `canceled(restored)` and an `executing` approval whose write call was `queued` or `leased` to `exec_unknown`; actions are not repeated (APR-7);
3. replays the erasure log from its separate backup (MEM-4);
4. answers `not_ready` to `claim` and `enqueue` and starts no model calls;
5. waits until the spend gap since the backup is filled with `spend_reconciled` lines from the provider's report (BUD-10). If the report cannot be had in time, `restore.complete --spend-pending` makes the core ready with the gap marked open, `alarm.spend.unreconciled` and halved daily limits until `spend.reconcile` closes it.

The restore completes by P2 `restore.complete`, or by the core itself when it sees the erasure log backup is ahead of the store. What is lost is normal: channel updates accepted after the backup moment (people repeat them), pending approvals (requested again), and schedule slots outside their catch-up window.

**Backup reports and the restore check.** Backups are taken by an external process the core knows nothing about, so after every store backup, erasure log backup and restore check that process reports with P2 `backup.report` and its narrow key ([RET-8](../conformance/rules.md#RET-8)):

```yaml
# P2 backup.report (sketch)
kind: backup                      # backup | restore_check
started_at: "2026-10-07T02:00:00Z"
finished_at: "2026-10-07T02:14:00Z"
ok: true
size: 41234567890
location_id: store-daily          # id from the deployment configuration; no paths
```

A backup whose restore was never checked does not count. At least weekly a fresh backup is restored on a test bench under the same rules, the `restore/` check set and `doctor` run there, and a `restore_check` report follows. With profile `monitor` the monitoring system watches backup age from the backup tool; without it the `health` loop raises `alarm.backup.overdue` after 26 hours without a good backup and `alarm.backup.restore_check_overdue` after 8 days without a good check. A backup script that silently failed in July is noticed the next day, not at restore time.

### Capacity and dependency failure

**Tenant limits.** Money does not hold all work: `job` tasks spend nothing, while a route in a loop can enqueue tens of thousands of tasks a day and fill the shared disk. So from v0.1 the core limits each tenant with counters below the farm ceilings ([CAP-1](../conformance/rules.md#CAP-1)):

```yaml
# tenants/demo/tenant.yaml (fragment)
spec:
  kind: production
  timezone: Europe/Berlin       # IANA name (TIM-2)
  budgets: {month_eur: 200, day_eur: 10, hour_eur: 2}   # hour_eur drives HLD-7
  operator_access: {debug_journal: notify}
  retention:                    # v0.2 (RET-9); v0.1 uses class defaults
    classes: {content: 30d, metadata: 400d}
    files: 30d
    journals: 7d
  limits:
    max_queued: 5000            # tasks in submitted
    max_enqueue_per_hour: 2000
    max_storage_bytes: 21474836480   # 20 GiB of files and artifacts
    max_file_bytes: 209715200        # one file (ART-4, v0.2)
    max_task_files_bytes: 1073741824 # all files of one task (ART-4, v0.2)
```

20% of `max_queued` (at least 2 places) is reserved for priority classes 1 and 2 (DSP-5): otherwise a provider key broken on Friday would fill the queue overnight with schedule tasks, and Monday's urgent question would get `over_capacity`. Exceeding a limit gives `rejected(over_capacity)` with `retry_after_s` and does not take the idempotency key; a schedule skips the slot with `schedule.skipped(over_capacity)`. Only the tenant that exceeded is refused. Tree size and running tasks are held by `max_tree_tasks` (ORC-8) and `max_running` (DSP-7).

**Storage headroom.** A full store volume stops the whole farm and leaves nowhere even to write the incident, so the core measures free space and keeps headroom ([CAP-5](../conformance/rules.md#CAP-5)). Below the soft threshold, enqueues of classes 3 to 5 are refused and `alarm.store.free.low` is raised; the number is also in the `health` answer, in case the export cannot be written. Below the hard threshold P1 answers `not_ready` to everything except `heartbeat`, `complete`, `fail` and `release`, while P2 and hold operations still work. The headroom is not part of any tenant's `max_storage_bytes`.

**Space quotas on volumes.** `Catalog.spec.volumes` lists each catalog volume with its `writers` and a `quota_bytes` per writer (MGR-33). The core shows whether every writer has a quota and their sum is below the `disk_full` threshold; only then can a "disk full" incident stop the farm manager seat. Example: on `res:ctd1:data` (4 TB) `demo` has 2 TB, sink `archive` 0.5 TB, pipeline `ctd-qc` 1 TB; 3.5 TB is below the 3.8 TB at which `disk_full` fires. The used-space series is `dataorigin: code`, read from the monitoring history ([CAP-6](../conformance/rules.md#CAP-6)); the core enforces no quota and raises no volume alarm itself.

**Refusal reasons.** Nothing but the task itself spends its attempt. Reasons of this section (the full list is in `docs/tasks-and-delivery.md`):

| Reason | Retry | Outcome |
|---|---|---|
| `provider_unavailable`, `provider_rate_limited`, `provider_quota` | the core retries with pause and jitter; not dispatched while the circuit is open or the pair waits | `failed(deadline_exceeded)` |
| `provider_config` | not by timer; waits for a fix | same, with an alarm |
| `provider_spend_cap` | no | `failed`, alarm |
| `service_rate_limited` | after `retry_after_s` | farm rate limiter: service actions, scripts, notes, cards |
| `forbidden` | no | a refused right, including `hold.release` below rank |
| `plugin_unavailable` | no; the model decides | a tool call error at once |
| `over_capacity` | for the sender, after `retry_after_s` | `rejected` at enqueue |
| `held` | no; a task canceled by `cancel_running` waits in `submitted` | `rejected`, `denied`, `canceled`, `withheld`, `discarded`, `exec_failed` |
| `core_unavailable` | yes, without a pause | lease expired while the core was down (FEN-5) |
| `not_ready` | after `retry_after_s` | the core is not ready |
| `restored` | no | approvals canceled by a restore |
| `schedule_time_invalid` | no | a backup schedule change that loses a slot (TIM-5) |
| `gone` | no | key, `redrive` or `file.get` of deleted data |

**Model provider failure.** A provider failure spends no attempts ([CAP-2](../conformance/rules.md#CAP-2)). A provider error is retried by one layer: the P4 client at most once, the runtime never, then the core retries the task with a pause up to 60 s plus jitter, so recovered capacity is not hit all at once (GW-12). On a series of failures P4 opens the circuit of the (workspace, model pool) pair and the dispatcher stops dispatching to it (GW-11). Only runs where the model answered count toward the run limit (FEN-3). A fallback provider around P4 is forbidden (DEP-5): degradation goes by refusal and alarm.

**Delivery rate and dead work.** From v0.2 a channel driver declares a `rate`, and extra deliveries wait in `planned` without spending attempts (CAP-3). Dead work is a task `failed` or a delivery `dead` with `attempts_exhausted`, `runs_exhausted` or `deadline_exceeded` (DLV-4); there is no separate dead letter queue. From v0.2 P2 `deadletter.list` groups it by (tenant, type, reason), so one broken check does not hide everything else (CAP-4). After the `content` period `redrive` answers `gone`.

### Time

The only clock for deadlines is the store clock ([TIM-1](../conformance/rules.md#TIM-1)): leases, `deadline`, approval and handle deadlines, budget windows, slots, hold and retention periods. P1 answers carry remaining seconds, so a worker with a shifted clock works correctly. Contracts use UTC; a time zone is an IANA name and exists only in `Tenant.spec.timezone` (budget windows, texts for people) and `Schedule.spec.timezone` ([TIM-2](../conformance/rules.md#TIM-2), BUD-4).

A schedule slot is a UTC instant with minute precision, and the slot task key is `system:schedule:<name>:<slot>` ([TIM-3](../conformance/rules.md#TIM-3), IDM-2). The slot format is part of the idempotency key, so it is fixed from v0.1: changing it later would run a slot twice at the seam. The time zone is checked against the database at `apply`; `CRON_TZ` inside `cron` is not allowed; a change applies from the next slot. Every schedule task type needs a `deadline`. A skipped slot is the event `schedule.skipped` with a reason (`dst_gap`, `waiting`, `held`, `over_capacity`, and from v0.2 `paused` for a schedule with `paused: true`, TIM-5) and is not caught up; `skip_if_waiting` (default `true`) skips while the previous slot's task waits or the provider circuit is open.

```yaml
# tenants/demo/schedules.yaml (fragment)
kind: Schedule
metadata: {name: daily-report, tenant: demo}
spec:
  cron: "30 2 * * *"
  timezone: Europe/Berlin
  task: {type: report.daily.v1}   # the type has a deadline
```

| Clock change in `Europe/Berlin` | What this schedule does |
|---|---|
| 2026-03-29: 02:30 does not exist | skipped, `schedule.skipped` with `dst_gap` |
| 2026-10-25: 02:30 happens twice | one run at the first, 00:30Z, key `system:schedule:daily-report:2026-10-25T00:30Z` |

Daily tasks are better placed outside the night window of clock changes. From v0.2, before "Accept" on a bot's proposal to change a schedule, code checks it ([TIM-5](../conformance/rules.md#TIM-5)): an invalid expression gets no button, the card lists local times that disappear or repeat within a year, and a backup schedule that would lose a slot is `rejected(schedule_time_invalid)`. Maintenance windows are not computed ahead (D-100). Proposals are described in `docs/improvements.md`.

### Tenants and people leaving

A tenant is declared in git as `tenants/<name>/tenant.yaml` and appears at `farmctl apply`. Its states `active`, `suspended`, `offboarding`, `deleted` are in `spec/lifecycles/tenant.md` ([LCY-1](../conformance/rules.md#LCY-1)); tenant operations are operator-only and never run from a channel (PRM-14). A suspension is a `tenant` hold with reason `suspended` that only the operator releases; data, credentials and the provider key are untouched. `tenant.offboard` revokes what lets the tenant spend and act; the core forgets the tenant's provider key, and revoking it and the workspace at the provider needs an organization admin key the core does not hold, so the answer prints that operator step (GW-3, D-42, D-103). `tenant.delete`, no earlier than the end of the export window (30 days by default), creates an erasure request with scope `(tenant)`, and a tenant with the same name is refused afterwards. Export (`tenant.export`, `principal.export`, v0.2) writes JSON Lines by the `spec/model` schemas only to a sink the operator names, never to a chat or the model ([LCY-2](../conformance/rules.md#LCY-2)).

A principal goes through `active`, `off`, `offboarded` ([LCY-3](../conformance/rules.md#LCY-3)). An admin's `/off` deletes nothing and is reversible: it cancels the person's pending approvals (APR-6) and tasks (`canceled(person_inactive)`); schedules stay, and admins decide on them. `principal.offboard` (an admin with `admin:people`, or the operator for people with privileged groups) does the same, is final (`subject_closed` on any later operation), frees the person's channel identities for reuse and erases memory and sessions through an erasure request ([LCY-8](../conformance/rules.md#LCY-8), D-98). Core records name the person by `human:p_<ULID>` from the start, so nothing is rewritten: the name is deleted from the principal record and the audit keeps the opaque id (D-85). A person who returns gets a new principal; old rights do not come back (D-100).

### Workers and plugins

A worker in `draining` (P2 `worker.drain`) gets no new work and finishes until `drain_deadline`; then the heartbeat answer asks for `release`, and the attempt is not spent ([LCY-4](../conformance/rules.md#LCY-4)). The work version is pinned at the task type (`.vN`), not at the worker, so a new image must understand every `.vN` in its role's `claims`. `worker.revoke` of a worker whose runtime keeps sessions or memory cleans it by an erasure request with reason `worker_revoke`. The core accepts the current and the previous major P1 version; a worker outside gets `version_unsupported` at `hello`, so core and workers upgrade at different times ([LCY-7](../conformance/rules.md#LCY-7)). From v0.2 a plugin manifest's `source.accepted` lists up to two (image digest, `tools/list` fingerprint) pairs during an update, and `hello` passes with either (LCY-5); the model always sees the manifest's tool descriptions.

```yaml
# tools/logs/tool.yaml (fragment)
source:
  image: registry.example/tools/logs@sha256:<digest>
  accepted:
    - {image: "registry.example/tools/logs@sha256:<digest>", tools_fingerprint: "sha256:<digest>"}   # current
    - {image: "registry.example/tools/logs@sha256:<digest>", tools_fingerprint: "sha256:<digest>"}   # new
```

### Rolling out a new core version

A core restart must not burn the attempts of running tasks ([LCY-6](../conformance/rules.md#LCY-6), LCY-7):

1. The operator calls P2 `core.drain` with a deadline and sets a maintenance window on the core node in the monitoring system. `claim` answers `not_ready`; running attempts and started P4 calls finish; the core stops.
2. During the drain `health` answers `reason: draining` with `drain_until_s`, and the liveness trigger stays quiet within it; the window covers a rollout that runs past the deadline.
3. A lease that expired while the core was down ends with `core_unavailable` and is not spent (FEN-5).
4. While `active` leases exist, the store schema changes only by addition: expand now, contract in the next rollout.
5. The new core is not ready until the store answers and every loop has ticked (OBS-8); workers with the previous P1 major version keep working.
6. `health` says "alive" again; the operator checks `doctor` and removes the window.

A window on the core node sets no farm hold: it is a farm node, not a catalog resource. A rollout without `core.drain` interrupts running attempts, but expired leases still do not spend them.

## People's data

The farm handles people's messages, files, questions and names in logs. This part collects what the farm owes them and what it gives the organization to meet its own obligations under data protection law. The four obligations every installation must be able to show are: **erasure on request**, **retention by period**, **the notice "you are talking to a bot"** with the privacy notice, and **a recorded legal basis and terms for the model provider**. Erasure at a person's request is part of v0.1 and cannot be waived on a tenant with `kind: production` (D-76); the copy of one's data arrives in v0.2. How the data is protected (redaction, the untrusted flag, audit) is in `docs/permissions.md`.

### What the farm stores about people

A principal is whoever acts inside one tenant: a person, an external program or `system`; a person in two tenants is two records, counted separately.

| What | Class | How it is erased |
|---|---|---|
| principal, identity, memberships | principal record | identity and name erased at offboarding; the audit keeps the opaque id (LCY-3, D-85) |
| task bodies, results, artifacts, files, sessions, bot messages | `content` | by period or erasure request; sessions also by `forget` |
| a person's memory; a retired bot's sealed memory | `memory` | `/forget`, offboarding, tenant period; end of the seal |
| approvals, decisions, subject requests, audit, events, usage | `metadata` | whole months (AUD-3); the `erase_match` string is wiped after the search |
| attempt journal, a person's connections (v0.2) | `content` | by period; connections also by revocation and offboarding (SEC-7) |
| backups | classes of the originals | by backup lifetime (RET-5) |
| at the model provider, in the messenger cloud | their terms | not by the farm |

Names and external ids live only in principal and identity records, never in the audit, events, transition journal or usage (AUD-2). Erasing an identity de-identifies the journals without editing a line, and the hash chain stays intact (AUD-6).

The farm does **not** keep a "personal data" class: personal data arrives as free text everywhere, and data classes (`summary` < `internal` < `raw_logs`) express sensitivity for permissions, so `pii` is rejected. It redacts addresses on the toolhost by default (IPv4 to /24, IPv6 to /48, email in full; INJ-4). It never makes a person the subject of a fact, finding, series, proposal, review or alarm (MEM-6, OBV-2): reports count people only as a group. It does not mix tenants (TEN-2, TEN-5, D-42), does not show the model secrets (SEC-1), and does not download links from messages (INJ-8).

**The "personal data seen" flag.** To answer "where do you have my data", tasks, files, artifacts, bot messages, findings and memory items carry `personal_seen`, set only by the core on events code sees ([AUD-5](../conformance/rules.md#AUD-5)): redaction replaced an address, an attempt read a person's upload or another person's task, a forward or quote, a flagged object, or a source declaring personal data. The flag only grows, is inherited, and is invisible to the model. For another person, a forward or a quote the core also records whose data it is in `personal_subjects`, so erasure by `(tenant, subject)` reaches a person's data in other people's tasks.

**What goes to the monitoring system.** Only numbers, `enum` values, tags and catalog ids; the `subject` tag is never a person (MON-1). Notes are the only text, and pass redaction and secret checks. The privacy block names the monitoring system as a recipient with ceiling `summary` (SEC-9); it is not a subprocessor, since it runs at the organization.

### The legal basis and the model provider

The tenant manifest's `privacy` block records as data what the organization decided ([SEC-8](../conformance/rules.md#SEC-8)):

```yaml
# tenants/demo/tenant.yaml (fragment)
spec:
  privacy:
    purposes:
      - {purpose: operations, lawful_basis: public_task}
      - {purpose: assistance, lawful_basis: legitimate_interest}
      - {purpose: incident_review, lawful_basis: legitimate_interest}
    controller: {name: "Example Org", contact: privacy@example.org, agreement_ref: "contracts/demo-processing"}
    subprocessors:
      - provider: provider-a
        agreement_ref: "contracts/provider-a-dpa"
        region: region-a
        retention: {mode: standard, max_days: 30}   # or {mode: zero}
        terms: {training: false, human_review: abuse_only}
    channels:
      - {driver: messenger, third_party_storage: true, ceiling: internal}
    recipients: [model_provider, messenger, monitoring]
    notice_ref: "privacy/demo-notice"
    notice_version: 3
    dpia_ref: "dpia/demo-2026"          # an explicit none is allowed
    ropa_ref: "ropa/demo"
    subject_request_days: 30
```

Purposes come from the closed list `spec/model/processing-purposes.yaml`; each has a `lawful_basis` (`public_task` by default, `legitimate_interest`, `contract`, `consent`, `legal_obligation`). `consent` is allowed only where there is a consent record and a withdrawal operation. Only `incident_review` changes core behavior: switched off, the farm manager reads no content of the tenant and running read attempts are canceled in the same transaction (MGR-20); with `farm_manager.enabled: false` it does not work on the tenant at all (D-77).

For a production tenant `apply` refuses an empty field: at least one purpose with a basis, a controller with contact and agreement, a subprocessor with agreement, region and retention mode, a line per channel, `dpia_ref` and `ropa_ref`. For any tenant every provider in `providers_allowed` must be a subprocessor, so a fallback model cannot become a new subprocessor without a decision; `recipients` must contain `monitoring`; a change of controller, purposes, subprocessors or recipients without a higher `notice_version` is refused. A test tenant still needs the subprocessor list, or the model is not called.

Everything the model reads goes to the provider, so its terms are applied by the gateway P4 inside the core, never by the runtime: P4 sets the region from the subprocessor and rejects a region in a runtime request (GW-14, GW-1); a model profile whose provider retention is longer than `max_days`, that requires retention under `mode: zero`, or whose `provider_training` or `provider_human_review` contradicts `terms` fails `apply` (GW-15). This covers every P4 call, including the embedding model, fallback and evals. A region outside the EU needs a transfer basis `spec.model_transfer` (`scc`, `adequacy` or `consent` with a review date) in the tenant manifest (D-87 (2)). Each tenant has its own provider workspace and key (D-42).

A channel with third-party storage has ceiling `internal` by default ([SEC-9](../conformance/rules.md#SEC-9)). A delivery above the ceiling gets `withheld(channel_ceiling)` and the person gets a code text without content; raw logs go to such a channel only if the tenant raised the ceiling in git. A person's `data:raw_logs` says who may see raw lines, the channel ceiling says where they may be sent.

### The notice "you are talking to a bot"

A person must know they are writing to a bot no later than the first contact. The channel driver declares how: a built-in marker of the messenger, or a code text before the first model text to every person and newly bound chat; until it is delivered, no model text goes to that recipient. A channel without the declaration fails `apply`. Model text carries `dataorigin: model` in records and an origin mark in artifacts, so people and code can tell it from code text (`docs/tasks-and-delivery.md`).

Separately, the privacy notice `privacy_notice` is a code delivery at first contact ([SEC-13](../conformance/rules.md#SEC-13)), even when the messenger shows a bot badge: controller and contact, purposes and bases, recipients, subprocessors with region and retention, retention periods, a link to the full text and how to submit a request. It is assembled from the privacy block and the message catalog (key `notice.privacy`, DLV-9) and carries `notice_version`; a new version is delivered again at the next contact, and model text waits until it is. There is no separate report about a person's data (D-114): the notice says what is kept, and the copy gives the data itself.

### Erasure on request and other rights

A person sees their assistant's memory with `/memory` and erases an item for real with `/forget`, in every store including the runtime (MEM-4). `/mydata erase` and from v0.2 `/mydata export` create through P2 `principal.data_request` a `SubjectRequest` and a `subject_request` card for people with `admin:people` ([SEC-11](../conformance/rules.md#SEC-11)). People decide, because the controller may be obliged to keep part of the data; the model neither creates nor decides a request. States: `open`, `approved`, `rejected` (terminal), `done` (terminal), `failed` (retryable). Silence neither executes nor closes: at half of `subject_request_days` admins are reminded, at the deadline the operator is notified. The card shows counts per class, the deadline and what silence does.

An approved erasure is one erasure request with reason `subject_request` and scope `(tenant, subject)` through every port: objects the person initiated or owns, objects with them in `personal_subjects`, group sessions whose key includes them, sealed memory of retired bots. Before erasing the identity, the core creates an `erase_match` request for their external ids and display name, to catch mentions without the flag. The person gets a code text with the outcome per store, including what lies outside the farm.

A copy goes only to the person ([SEC-10](../conformance/rules.md#SEC-10)): `/mydata export` carries the person's public key, and the export task writes only to a sink with `export_only` and `recipient_ref: self`, encrypted with that key, so no admin or operator can read it. It leaves out objects whose `personal_subjects` name other people; a sink without `export_only` gets `sink_not_export`.

**People who are not principals.** Someone unknown to the farm may be named in another person's task. An admin with `admin:people` creates `erase_match` through P2 `erasure.match` with an exact string, and a second person with the same permission approves (in a single-admin tenant a farm admin, D-86 (8)). The core searches all `content` and `memory` objects of the tenant and, through the ports' `match` operation, runtime transcripts, the sink archive and the channel cache; a store without `match` gives `failed`, not "nothing found", which is why a production role with a runtime-internal session needs an adapter with `match` (RT-1). The string is wiped right after the search and never enters audit, events or the erasure log, not even as a hash. Matching objects are erased whole, with tombstones; free text is not edited. Objects that appear later need a new request.

From v0.2 per-person usage keeps its link to the person for 93 days only, then the `retention` loop strips `subject`, `task` and `root` and truncates time to the day: the only edit of a `metadata` row ([SEC-14](../conformance/rules.md#SEC-14), BUD-16).

**Privacy report (v0.2).** P2 `tenant.privacy_report` gives the data protection specialist one summary per tenant (SEC-12): purposes, classes with periods and object counts (with `personal_seen`), subprocessors, channels with their ceilings, the DPIA and record links, open and overdue subject requests, and the count of incident review reads. Metadata only.

**Reading content for incident review.** With `incident_review` on, only the farm manager's reader role reads, separate from the decision role; one attempt sees one tenant with that tenant's key (MGR-24); it never sees memory, sessions, sealed records or secrets; it opens a person's file only when code named that file as the incident subject (MGR-30); every read is in the tenant audit with basis, object, size and hash (MGR-28).

**Attempt journal and connections (v0.2).** The attempt journal (model requests, responses and tool output of one attempt) is `content` with the tenant's `journals` period, read only through P2 `task.journal` with `admin:debug` and `data:<mark>`, and off on a production tenant until the tenant sets a period (AUD-4); `operator_access.debug_journal` decides whether the tenant is told. A person's connection to their own email or calendar is created by the person through the consent adapter, chosen by the core by the task initiator, and revoked by offboarding or an erasure request (SEC-7).

### What the organization decides

| Decision | Where it is recorded |
|---|---|
| controller, agreement, purposes and lawful basis | `privacy.controller`, `privacy.purposes` |
| provider terms, region, zero retention, allowed providers | `privacy.subprocessors[]`, applied only by P4 |
| messenger with third-party storage | `privacy.channels[]` with the ceiling |
| retention periods; subject request deadline | `Tenant.spec.retention`; `subject_request_days` |
| DPIA and record of processing | `dpia_ref`, `ropa_ref`, fed by the privacy report |
| texts for people | the message catalog, `notice_ref`, `notice_version` |
| farm manager on the tenant; reporting operator journal reads | `farm_manager.enabled`; `operator_access.debug_journal` |
| who sees the monitoring system | its own settings; the farm sends nothing about people there |

## Recipes

Every recipe is a sequence of port operations; the commands of an implementation live in its documentation. The core rollout and the weekly restore check are above.

### Emergency stop

1. Part of the farm: P2 `hold.set` with the needed scope and effects (for example `tasktype` with `no_enqueue`, `no_claim`, `cancel_running`).
2. The whole farm: `farmctl hold farm`; it is written to the store directly and works without the core process or the channel.
3. Core suspected compromised: external stop. Stop the core containers, revoke the bot token at the channel's external system, disable the tenants' keys at the model provider.
4. Investigate with `status`, `doctor` and the audit. Release with P2 `hold.release` by someone not below the author in rank (operator holds and `farm` only by the operator). Revert the external stop by hand.

### Planned work on a machine

1. Set a maintenance window on the machine's node in the monitoring system, for example `db1`.
2. From v0.2 the core sets a `host` hold with `no_tool`, `pause_action`, reason `maintenance`; before v0.2 set it with P2 `hold.set`.
3. Finished early: the tenant admin or the operator releases with P2 `hold.release`; otherwise it ends at `until`.
4. Meanwhile bots get `denied(held)` for `service/control` and `monitor/script_run` on the machine; people manage the services.

### Update a plugin

1. A separate PR lists the current and new (digest, fingerprint) pair; a changed tool description runs the plugin's eval cases.
2. P2 `apply`; roll out the new toolhost image.
3. When all toolhosts run it, remove the old pair and `apply` again.
4. On a vulnerability: P2 `hold.set` on scope `plugin` with `no_tool` first, then git.

### Update a worker or a runtime

1. Check that the new image understands every `.vN` in the role's `claims` and a P1 version within the core's window.
2. P2 `worker.drain` with `drain_deadline`; wait for running work.
3. Replace the image and enroll (P2 `worker.enroll`).
4. P2 `worker.revoke` the old worker; if its runtime kept state, check `erasure.status`.

### Release a change on a test tenant first

`apply` changes shared roles, types, plugins and actions for every tenant at once, so:

1. Release the new role as a copy under another name, for example `ops-reader-next`.
2. Enroll the test tenant's (`demo2`) workers with it (P2 `worker.enroll`). People's rights do not change, since groups grant `task:<type>`; sessions live under another key meanwhile.
3. After 7 days move the change into `ops-reader` and delete the copy.
4. For a task type, `.v(N+1)` next to `.vN` does the same (MAN-3).

### New tenant

The git side (`tenants/<name>/`, catalog, groups, roles) is in `docs/extending.md`. For the alarm path:

1. In the monitoring system: an account for this tenant's adapter with rights only on its node group (MON-4); the farm template's items and triggers for the tenant from `deploy/monitor/`; a problem webhook action to the tenant adapter.
2. In the catalog: `hosts[].monitor_host`, `monitor_scripts_allowed`, `monitor_triggers`; no farm machine.
3. Enroll the tenant's `monitor` adapter (P2 `worker.enroll`) with its token and account; start it.
4. `doctor` for the tenant: worker `active`, no E5 deliveries in `planned` older than 15 minutes, fresh `monitor_export_age_s`, account scope only its groups, the audit head in the monitoring history (OBS-11).
5. Send a test `info` alarm and see it in the monitoring system with the `tenant` tag.

### Suspend and delete a tenant

1. P2 `tenant.suspend`. To return: the operator releases this hold (P2 `hold.release`).
2. P2 `tenant.offboard`; then revoke the key and workspace at the provider by the step the answer prints.
3. P2 `tenant.export` to a sink the operator names.
4. After the export window, P2 `tenant.delete`; watch `erasure.status` until `done`.

### A person left or asks to erase their data

1. Temporarily: an admin's `/off`; access closes, nothing is deleted.
2. For good: P2 `principal.offboard`; memory and sessions go through an erasure request.
3. Erasure request: the person writes `/mydata erase`, a person with `admin:people` approves the card, the core creates the erasure request (or the operator creates it with P2 `erasure.request`); watch `erasure.status`. Non-waivable on production. From v0.2 the person can take a copy first with `/mydata export`.
4. Someone who is not a principal asks: P2 `erasure.match` with the exact string, a second admin approves.
5. Erased data stays in backups until their lifetime ends and does not come back after a restore.

### Restore from backup

1. Stop the channels, then the core.
2. Restore the store; the core increments the epoch, cancels pending approvals, marks unclear write calls `exec_unknown`, replays the erasure log.
3. Bring the provider's spend report through the reconciliation process (BUD-10), then P2 `restore.complete` and `doctor`. If the report cannot wait: `restore.complete --spend-pending`, run with halved limits and `alarm.spend.unreconciled`, reconcile as soon as the report appears.
4. Start the workers, the channels last. Tell people to re-request pending approvals and repeat requests sent after the backup moment.

### Rotating secrets

Everything the core issues or signs carries a key id, and old and new keys are valid together within the rotation window (SEC-5). The deployment description names the method of each secret under `secrets` (`overlap` with a `window`, or `cutover`); the deployment lint rejects a named secret without one (`secret_rotation_missing`). A revocation on compromise takes effect at the next call, without overlap, and the operator sets a hold without deadline on the affected scopes, which only the operator releases (SEC-6).

| Secret | Order | `doctor` shows |
|---|---|---|
| `health` key (deployment configuration) | add a second value and reload; switch the monitoring poll (two key macros in the template); wait for a good poll; remove the old value | `health` never answered `unauthenticated` |
| P1 worker token, including adapters | `worker.enroll` issues a new one, the old is valid until `worker.revoke` or the window ends; restart; `monitor` adapters one at a time | `last_seen` with the new token; no stale E5 deliveries |
| adapter token in the monitoring system | create a second token of the account; restart the adapter; confirm `monitor_export_age_s`; revoke the old | fresh export age; rights not wider (MON-4) |
| problem webhook token | the adapter accepts two values; change the webhook action; remove the old | rejected webhooks do not grow |
| model provider key | create a new key; add it next to the old in P4; P4 reloads; revoke the old at the provider | no `provider_config`; the daily GW-9 probe passes |
| channel driver P2 secret (D-87 (3)) | `worker.enroll` with `rotate_p2: true`; restart the channel processes one at a time | old secret rejected after the window |
| channel bot token | no overlap at the external system: change it and restart the adapter; the incoming queue waits (DLV-5) | fresh last channel poll (OBS-10) |
| audit signing key | new key with a new id; `verify` accepts both within the window (AUD-6) | `verify` passes over all heads |
| core token key (`token_key_file`: lease tokens, handles, token hashes) | a new key file with a new `token_key_id`; the old key and its id move to `previous_token_keys`; restart the core; a worker token is rehashed under the new key at its first call; after the window (the longest lease, and every worker called once) remove the old key | no growth of `lease_mismatch` and `unauthenticated` |

## Why it works this way

- The core cannot report its own death, and the organization already has alerting, on-call and escalation. So an external poll checks liveness, "alive" requires the store and every loop, and alarms reach people only through the monitoring system: one place to acknowledge, not two (D-59, D-60, D-62). The export is that only path, so it cannot die like a chat delivery.
- Narrow monitoring accounts and notes instead of "Acknowledged" leave decisions about problems to people; script rights only on tenant nodes keep a stolen key away from the core machines (D-61, D-64, D-69). Whatever people outside the farm edit there (trigger tags, script names, windows) is, for the farm, either a closed catalog vocabulary in git or a pause, never a cancellation.
- A service restart is frequent and reversible, so it needs no card within lists people set; start and stop change availability and do (D-67, D-68, D-74). The role says what the bot can do, the initiator's capability what the person may ask (D-73).
- One "hold" entity gives one check point in C2, one audit and urgent narrowing without git. Release by rank keeps a tenant admin from lifting an operator's suspension, while code holds inside a tenant stay in the tenant's hands.
- A restore would otherwise break the approval, lease and erasure rules; the epoch, `canceled(restored)` and the erasure log replay make it safe, which is why early erasure goes through the log from v0.1.
- A provider failure, a core restart or an overload must not burn attempts, or an hour-long outage would fail tasks en masse. The slot format, limits and headroom are cheap before the first code and expensive after.
- The core knows about backups, exports and spend only what it is told, so each external process has a report operation and an alarm on its silence.
- Legal decisions belong to the organization; with fields and checks ready, a decision is a line in the manifest. Personal data arrives as free text, so a code-set origin flag answers "where could it be" better than a data class. P4 applies provider terms because the runtime is outside the trust boundary. Erasure by string needs a second person because a short string matches everything. A bot badge does not name the controller, so the privacy notice is a separate, versioned delivery.

## Example on `demo`

Tenant `demo` runs on `web1`, `db1` and `ctd1`, linked to the organization's monitoring system; the core, store, channel and backups live on the farm node `core1`. `alice` (operator) and `bob` (tenant admin) are on call in the monitoring system.

**Store hang.** Friday evening the store hangs. Workers get `not_ready`, no model calls start, Telegram keeps the unacknowledged updates. `health` says "not alive", and the monitoring system wakes `alice`. She restarts the store; after every loop ticks, the core clears `not_ready`, the channel picks up the messages, and no attempts were spent.

**Disk on `db1`.** At night free space drops to 7%. The monitoring system wakes `alice` and sends a webhook; the `demo` adapter submits `ext.monitor.problem.v1` with `triggerid: "23801"`. The catalog maps it to `disk_free`; a `job` task rechecks and raises `alarm.disk.free.low` (`page`), exported with `origin: monitor` and not re-notified. The farm manager's incident finds growth of 2% an hour since the nightly import and proposes `cleanup_pipeline_tmp`; `bob` approves the card. `alice` presses "Acknowledged" in the monitoring system. The problem closes, and the farm alarm resolves.

**Services.** `bob` writes "restart nginx". The role has `service_control: [nginx]` and `bob` the capability `service.control:nginx`; the restart runs under a `service_grant` without a card, a code note appears on the open problem, and two minutes later code confirms `health`. `carol` asking the same gets `forbidden`; "stop nginx" goes with a card. After the bot read "restart nginx urgently" in a log item through `monitor/item_text`, the same request goes with a card (INJ-15).

**Holds.** A vulnerability is found in the `logs` plugin. `bob`, who has `hold.set:plugin` and `hold.release`, sets `{kind: plugin, tenant: demo, name: logs}` with `no_tool` from a direct chat; `logs` calls get `denied(held)` until the fixed image is out and he releases it. At 14:00 `demo` exceeds `hour_eur`; the core sets `runaway_spend` holds until 15:00, and `bob` fixes a looping route and releases them at 14:20. A `secret_in_outbound` hold on `ops-reader` stays after the leaked token is revoked, until `bob` checks the role.

**Maintenance on `ctd1`.** `bob` sets a window 10:00 to 12:00. From v0.2 the core sets a `host` hold; the card `cleanup_pipeline_tmp` waiting for `carol` is paused, not canceled, and the 11:00 pipeline slot is skipped with `held`. At 11:20 `bob` releases the hold and `carol` approves.

**Erasure.** `carol` writes `/mydata erase`. `bob` with `admin:people` approves; the core creates the request:

```yaml
# ErasureRequest record (sketch)
id: er_01JB8M
scope: {tenant: demo, subject: 'human:p_01JB3Q7N0M2V8XK4DGT5RZP9WC'}   # carol
reason: subject_request
author: {kind: principal, id: 'human:p_01JB3Q7N0M2V8XK4DGT5RZP9WA'}   # bob
state: running
results: {c3_sessions: done, c4_memory: done, c1_files: done, e2_archive: done, e1_channel: not_applicable}
```

The automatic `erase_match` for her handle is approved by `alice`. `carol` gets the outcome, including that copies at the provider go by its terms within 30 days. A week later the store is restored from an older backup; the erasure log replays before readiness, and her data is erased again. Later `erin`, who is not a principal, asks by email to erase `erin@example.org`; `bob` creates `erase_match`, the card shows "tasks 2, memory 1, transcripts 1", `alice` approves, the objects are erased whole, and the string is never stored.

**Core rollout.** `alice` calls `core.drain` for 10 minutes; `health` answers `draining` with `drain_until_s: 600` and nobody is woken. Had the new core not come up in time, the monitoring system would have woken her as in an outage.

## What is not decided

- The default retention periods (`content` 30 days, `metadata` 400 days, memory until `forget`) apply until the organization decides on personal data (D-12).
- Whether to restrict task enqueueing by processing purpose. Only `incident_review` changes core behavior today; linking purposes to task types can come later without a schema break.
- Whether a restart needs a card when the task has seen external logs rather than the monitoring system: by default yes (INJ-15, D-70, D-82).
- Whether to accept monitoring problems about farm nodes themselves as input: by default no; they go to people directly.

<!-- generated: rules -->
## Rules of this section

Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md](../conformance/rules.md).

| Rule | About | Since | Non-waivable |
|---|---|---|---|
| [AUD-5](../conformance/rules.md#AUD-5) | The "personal data seen" flag | v0.1 | no |
| [CAP-1](../conformance/rules.md#CAP-1) | Tenant ceilings on counters | v0.1 | no |
| [CAP-2](../conformance/rules.md#CAP-2) | A dependency failure does not spend an attempt | v0.1 | no |
| [CAP-4](../conformance/rules.md#CAP-4) | Dead work and schedule pause | v0.2 | no |
| [CAP-5](../conformance/rules.md#CAP-5) | Storage headroom | v0.1 | yes |
| [CAP-6](../conformance/rules.md#CAP-6) | Volume free space is read from the monitoring system | v0.1 | no |
| [HLD-1](../conformance/rules.md#HLD-1) | A hold as a recorded and visible object | v0.1 | yes |
| [HLD-2](../conformance/rules.md#HLD-2) | Who sets and lifts a hold | v0.1 | yes |
| [HLD-3](../conformance/rules.md#HLD-3) | How a hold acts | v0.1 | yes |
| [HLD-6](../conformance/rules.md#HLD-6) | Auto-stop "secret in outbound" | v0.1 | yes |
| [HLD-7](../conformance/rules.md#HLD-7) | Auto-stop on spend rate | v0.1 | no |
| [HLD-8](../conformance/rules.md#HLD-8) | A pause of approvals set by a person | v0.2 | yes |
| [HLD-9](../conformance/rules.md#HLD-9) | Auto-hold of a role after confirmed incident links | v0.3 | yes |
| [LCY-1](../conformance/rules.md#LCY-1) | Tenant lifecycle | v0.2 | yes |
| [LCY-2](../conformance/rules.md#LCY-2) | Tenant and principal export | v0.2 | no |
| [LCY-3](../conformance/rules.md#LCY-3) | Principal lifecycle | v0.2 | yes |
| [LCY-4](../conformance/rules.md#LCY-4) | Worker drain and removal | v0.2 | no |
| [LCY-6](../conformance/rules.md#LCY-6) | Core drain before rollout | v0.1 | no |
| [LCY-7](../conformance/rules.md#LCY-7) | Worker port version window | v0.1 | no |
| [LCY-8](../conformance/rules.md#LCY-8) | Offboarding is final and frees the identity | v0.1 | yes |
| [MON-1](../conformance/rules.md#MON-1) | Alarms to people along one installation path | v0.1 | yes |
| [MON-2](../conformance/rules.md#MON-2) | An external poll checks core liveness | v0.1 | yes |
| [MON-3](../conformance/rules.md#MON-3) | Audit chain head in the monitoring history | v0.1 | yes |
| [MON-4](../conformance/rules.md#MON-4) | Permissions of the farm account in the monitoring system | v0.1 | yes |
| [MON-5](../conformance/rules.md#MON-5) | Monitoring problems into the farm | v0.1 | yes |
| [MON-6](../conformance/rules.md#MON-6) | The monitor plugin is read only | v0.1 | no |
| [MON-8](../conformance/rules.md#MON-8) | Service control | v0.1 | yes |
| [MON-9](../conformance/rules.md#MON-9) | Monitoring maintenance windows give a farm hold | v0.2 | no |
| [MON-10](../conformance/rules.md#MON-10) | Monitoring system scripts | v0.1 | yes |
| [MON-11](../conformance/rules.md#MON-11) | Export of alarms and the audit head does not die | v0.1 | yes |
| [MON-12](../conformance/rules.md#MON-12) | Alarm export to the monitoring system | v0.1 | yes |
| [MON-13](../conformance/rules.md#MON-13) | Alarms to people by core delivery without a monitoring system | v0.1 | yes |
| [OBS-1](../conformance/rules.md#OBS-1) | Telemetry without content | v0.1 | yes |
| [OBS-2](../conformance/rules.md#OBS-2) | One trace per task tree | v0.1 | no |
| [OBS-3](../conformance/rules.md#OBS-3) | External trace context decides nothing | v0.1 | yes |
| [OBS-4](../conformance/rules.md#OBS-4) | Worker liveness and loop ticks | v0.1 | no |
| [OBS-6](../conformance/rules.md#OBS-6) | Common alarm schema | v0.1 | yes |
| [OBS-8](../conformance/rules.md#OBS-8) | The store is unavailable | v0.1 | yes |
| [OBS-9](../conformance/rules.md#OBS-9) | Three health indicators | v0.1 | no |
| [OBS-10](../conformance/rules.md#OBS-10) | Health of plugins, channels and the provider | v0.1 | no |
| [OBS-11](../conformance/rules.md#OBS-11) | The `doctor` lines of the alarm path per tenant | v0.1 | no |
| [RET-1](../conformance/rules.md#RET-1) | A retention class for every stored object | v0.1 | yes |
| [RET-2](../conformance/rules.md#RET-2) | Retention periods and deletion at expiry | v0.1 | yes |
| [RET-3](../conformance/rules.md#RET-3) | Tombstones keep uniqueness | v0.1 | yes |
| [RET-4](../conformance/rules.md#RET-4) | Erase operations in every port with content | v0.1 | yes |
| [RET-5](../conformance/rules.md#RET-5) | A backup is part of the production perimeter | v0.1 | yes |
| [RET-6](../conformance/rules.md#RET-6) | Restore from backup | v0.1 | yes |
| [RET-7](../conformance/rules.md#RET-7) | Early deletion only through the erasure log | v0.1 | yes |
| [RET-8](../conformance/rules.md#RET-8) | Backup reports, with core alarms only without the monitoring system | v0.1 | no |
| [RET-10](../conformance/rules.md#RET-10) | Deletion by period in every copy | v0.2 | yes |
| [RET-11](../conformance/rules.md#RET-11) | Channel cache, every runtime and tenant scope in erasure | v0.2 | yes |
| [SEC-8](../conformance/rules.md#SEC-8) | Tenant privacy block | v0.1 | no |
| [SEC-9](../conformance/rules.md#SEC-9) | Raw logs to a channel only by tenant decision | v0.1 | no |
| [SEC-10](../conformance/rules.md#SEC-10) | A copy of a person's data only to that person | v0.2 | no |
| [SEC-11](../conformance/rules.md#SEC-11) | A person's request and deletion through ports | v0.1 | no |
| [SEC-13](../conformance/rules.md#SEC-13) | The privacy notice as a separate delivery | v0.1 | no |
| [SEC-14](../conformance/rules.md#SEC-14) | Per-principal usage is anonymized after 93 days | v0.2 | no |
| [TIM-1](../conformance/rules.md#TIM-1) | One clock, the store clock | v0.1 | yes |
| [TIM-2](../conformance/rules.md#TIM-2) | UTC in contracts, a time zone only for the tenant and the schedule | v0.1 | no |
| [TIM-3](../conformance/rules.md#TIM-3) | Schedule slot format, time zone and time transitions | v0.1 | yes |
| [TIM-5](../conformance/rules.md#TIM-5) | Schedule check before "Accept" | v0.2 | no |
