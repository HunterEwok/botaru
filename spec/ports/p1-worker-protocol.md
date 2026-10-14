# P1 WorkerProtocol

Sheet version: 0.1.0.

## In short

P1 is the only entry of farm processes into the core. Work is pull-based: a worker takes work itself with the `claim` operation with long polling (long-poll), holds it with a lease with a fencing token and hands it in with `complete` or `fail`. One dispatch mechanism serves four kinds of work: a task attempt (`attempt`), a tool call (`toolcall`), a delivery (`delivery`) and a step of an erasure or search request at an edge store (`erasure`), so the toolhost, the channel and the sink speak the same protocol as the agent. This sheet also covers enqueueing tasks (`enqueue`), channel input (`submit_activity`, `decide`), events (`emit`), bot messages (`msg.*`), files, sessions and memory for the agent adapter. The sheet sets the meaning; paths, waiting times and status codes are set by the binding `spec/bindings/http/`.

## Who calls and who implements

- **Implemented by** the core. The reference binding from v0.1 is the HTTP v1 envelope `spec/bindings/http/README.md`: paths, headers, lease token, errors; bodies are `spec/model` schemas by the map `spec/ports/operations.yaml`. An OpenAPI 3.1 description referencing the same schemas will appear from v0.2.
- **Called by** workers of all kinds: `agent` (through the E4 adapter, the worker token is held by the adapter, not by the runtime), `job`, `toolhost` (E3), `channel` (E1), `sink` (E2), `monitor` (E5). An external program (a subject of kind `service`, D-47) calls with its key only `enqueue`, `task.get`, `cancel` and reads of its own campaigns and script runs.
- **Credentials.** P2 `worker.enroll` issues the worker token; it records the tenant, role, kind, labels, and for a toolhost the plugins, placement and machine (DEP-2). The core takes the tenant and the subject of each operation from the credentials (TEN-2). Worker tokens are not accepted on P2 (IDN-7).
- **Liveness.** Any P1 call updates the worker's `last_seen`; liveness is `last_seen` at the core, not the worker's self-report (STA-2).

## Operations

### v0.1

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| `hello` | all workers | The worker reports its kind, implementation, labels (only a narrowing of those recorded at `enroll`), capacity, protocol versions; a toolhost also the plugins, the placement and the fingerprint of the live `tools/list`; a channel the driver capabilities. Answer: `worker_id`, tenant, role, lease periods. A mismatch with the `enroll` record moves the worker to `blocked`. May be repeated | `unauthenticated`, `version_unsupported`, `blocked`, `not_ready` | v0.1 | TEN-4, DEP-2, INJ-2 |
| `claim` | all workers | Input: work kinds `kinds[]` (`attempt`, `toolcall`, `delivery`, `erasure`) and the waiting time. Answer: a work unit (`WorkItem`) or empty. The core dispatcher decides what to give: the filter (tenant, the role's `claims`, `requires` by labels, bot group, holds, the call's `target` and the delivery's `target`: kind and name of the channel, sink or monitoring adapter from the `enroll` record) and the order (priority class, fairness, queue). Each dispatch is a new lease with `lease_no` one higher than the previous one of this work unit, a token `(epoch, work_id, lease_no, worker_id)` with a core signature (`key_id`, `sig`; a call with a foreign `worker_id` or a wrong signature gets `lease_mismatch`), the deadline `lease_until`, the remainder `lease_remaining_s` and the trace context; for a personal assistant task the core writes `owner` into the lease (the initiating subject from the private chat), by which the memory namespace and the session are chosen, and there is no worker per person (DSP-10) | `draining`, `not_ready`, `blocked` | v0.1 | TEN-3, TEN-3a, FEN-1, DSP-1, DSP-2, DSP-4, DSP-10, HLD-1 |
| `heartbeat` | lease holder | Extends the lease. Input: the token, a progress stage from a closed list and the share done, usage for reference. Answer: `lease_until`, `cancel_requested` | `fenced` | v0.1 | FEN-2, FEN-4, DLV-11 |
| `complete` | lease holder | Hands in the result: the attempt result by the type's schema, a delivery receipt or a call result. In the same transaction the core writes the event, the deliveries (including the answer to the chat by `origin`) and the usage; it also publishes the messages staged by the attempt and acknowledges the inbox entries it read | `fenced`, `output_schema_invalid` | v0.1 | FEN-2, DLV-2, DLV-6, MSG-9, OUT-1 |
| `fail` | lease holder | Input: the token, `reason` from `reasons.yaml`, `retryable`, `retry_after_s`. Answer: the retry time or the end. The reason registry sets whether the reason spends an attempt | `fenced` | v0.1 | FEN-2, FEN-3, CAP-2 |
| `release` | lease holder | Returns the work to the queue; the attempt is not spent (reason `worker_drain`) | `fenced` | v0.1 | FEN-2 |
| `enqueue` | a `job` or `agent` worker with an attempt token and a type in the role's `may_enqueue`; an external program with its key | Enqueues a task. Input: body, `idempotency_key`, priority class (only lowering within the type), `refs`, the bot group address `to.group`. Answer: `TaskRef{id, existing}`. A repeat with the same key, initiator and body returns the existing task. A permission refusal carries the `needs` field: the group or capability that was missing (PRM-8) | `rejected{reason}` (`ref_invalid`, `input_schema_invalid`, `no_group_cap`, `no_data_class`, `tier_exceeded`, `depth_exceeded`, `tree_too_large`, `person_inactive`, `budget_exhausted`, `held`, `over_capacity` with `retry_after_s`, `memory_untrusted`), `tenant_mismatch`, `idempotency_conflict`, `group_invalid`, `gone`, `not_ready` | v0.1 | IDM-1, TEN-2, TEN-7, PRM-6, APR-5, BUD-1, DSP-7, RET-3, RT-2 |
| `cancel` | the initiator, an external program for its own tasks | Cancels the task and its children; the outcome is `canceled(initiator)`, the pending approvals of this task are closed | `not_found` | v0.1 | PRM-9, IDM-3, APR-6 |
| `task.get` | the initiator, an external program, a subject with `task.read:<type>` and `data:<mark>` | Returns the task record without the address signature, the state and the artifacts | `not_found` the same for a foreign and a nonexistent task | v0.1 | PRM-9 |
| `file.put` | channel (attachments), `job` and sandbox toolhost (result files) | Accepts bytes, the core writes the file record (tenant, producer, class, period) and returns a `file_ref` with 128 random bits | `data_class_exceeded`, `over_capacity`, `fenced` | v0.1 | ART-1, ART-3, TEN-7 |
| `file.get` | channel and sink under a delivery lease, toolhost under a call lease, E4 adapter under an attempt lease | Returns the file bytes if the file belongs to the tenant of the work and its mark is not above what the recipient is allowed; from v0.2 a deleted or expired file gives `gone` | `not_found`, `fenced`; from v0.2 `gone` | v0.1 | ART-3, PRM-9, TEN-7 |
| `submit_activity` | channel; E5 monitoring adapter (an activity of kind `signal` with the identity `hook:monitor`, MON-5) | Input: `Activity` (the verified identity of the sender, chat, text, `file_ref` attachments, `reply_to`). Answer: a reference to the task, `ignored` or a ready code answer. Code chooses the task type (a command from the tenant's `commands`, `/run` by the `run` field, the tenant's `default_types`, `continues` by `reply_to`). The core computes the idempotency key from (channel, update id) | `identity_unknown`, `not_ready` | v0.1 | IDN-3, IDN-6, IDN-7, IDM-5, CNT-5 |
| `decide` | channel on behalf of the verified person | Input: `decide(subject_kind, id, nonce, decision, reason_code)` and the verified identity of the person who pressed with the chat of the press (in the binding as headers, not body fields). Subjects in v0.1: `approval`, `memory_record` (a record of a person's memory, MEM-3), `proposal`. Accepted only from an active `human` subject with a strong identity and with the permission for this subject and this decision, only from the channel and chat where the card was delivered, and in a group chat only under `membership_assurance: enforced`; the transition happens once. Each decision writes a decision event with the fields `decided_by`, `response_ms` and `details_opened` | `forbidden`, `expired`, `already_decided`, `self_approve`, `held` (a hold with `no_action`) | v0.1 | APR-4, APR-9, APR-11, IDN-3, REP-6, IDM-3, HLD-1 |
| `card.details` | channel on behalf of the verified person | Card details by a code template after the `data:<class>` check; records the opening for this person and this card (the `details_opened` field of the decision event) | `forbidden`, `expired` | v0.1 | APR-11, APR-14 |
| `card.remind(subject_kind, id, nonce)` | the channel of the card copy on behalf of the initiator | The "Remind approvers" button on the card copy without decision buttons that the initiator got in a private message (`approval_initiator_copy`): the core repeats `card_update` to the approvers who have not decided yet, no more than once in 5 minutes per approval | `forbidden`, `expired`, `service_rate_limited` | v0.1 | APR-19, APR-4 |
| `emit` | core; a `job` worker with an attempt token | Publishes an event of a type from the task type's `events` with data by the schema. The core sets `source` and `id` | `type_not_allowed`, `fenced` | v0.1 | IDM-4, INJ-7 |
| `session.load`, `session.append` | E4 adapter | Reads and appends the conversation session by the session handle; `append` is idempotent by `entry.uuid` | `handle_invalid`, `fenced` | v0.1 | MEM-1, INJ-13 |
| `memory.search`, `memory.propose` | E4 adapter | Search in the memory namespace and a record proposal by the memory handle; the core computes the namespace; `propose` is idempotent by the content hash | `handle_invalid`, `data_class_exceeded`, `memory_untrusted` | v0.1 | MEM-1, MEM-3, TEN-5 |
| `msg.send` | a `job` worker with an attempt token (data by code); worker code with its own token (only genus `state`) | Stages a message by a type from git: the address `role:` or `group:` from the role's `messages.send`, or `reply_to` an entry read by this attempt (by its handle); only the core writes a `worker:` address. Closed fields and `note`. The message is published only in the transaction of the attempt's successful `complete` | `address_invalid`, `msg_type_not_allowed`, `data_schema_invalid`, `note_not_allowed`, `data_class_exceeded`, `inbox_full`, `sender_quota`, `hops_exceeded`, `service_rate_limited`, `held`, `fenced` | v0.1 | MSG-1, MSG-4, MSG-5, MSG-10, MSG-11, STA-1, HLD-3 |
| `msg.receive` | a worker with an attempt token, if the task type declared `inbox.read`; worker code with its own token (only entries of genus `state`) | Takes entries of the inbox of its role, group or instance under a lease and returns a `handle` for each entry, like the built-in tool `inbox_read`; `reply_to` accepts only the handle of an entry read by this attempt. Reading an untrusted entry sets `untrusted_seen` on the attempt, an entry above the task mark is not returned (`hidden: N`) | `fenced`, `draining` | v0.1 | MSG-4, MSG-6, MSG-7, MSG-8, TEN-3 |
| `obs.query` | a `job` worker with an attempt token | A closed query of observation rollups: series, subject, window and rollup from an `enum` within the task type's `observes` and the task mark; the answer holds only numbers and `enum` values, its size is limited, points of another tenant are not visible. This is how a pipeline takes numbers for a report without access to the store | `fenced`, `type_not_allowed` | v0.1 | OBV-3, DEP-8, TEN-5 |
| `msg.ack`, `msg.nack` | the recipient of an entry | Acknowledges what was read (in the `complete` transaction) or returns the entry to the queue; after three dispatches the entry goes to `dead(delivery_exhausted)` | `fenced` | v0.1 | MSG-9 |
| `claim` (identity fields) | all workers | In the lease record and the attempt token the core writes `acting_bot` and `bot_epoch`, and for seat tasks also `seat_epoch`. Seat tasks are dispatched by an ordinary `claim` only to workers of the seat roles enrolled in the task's tenant, and only at the current seat epoch | `fenced` (the seat epoch changed) | v0.1 | IDN-8, MGR-1, TEN-3 |
| `campaign.propose` | the coordinator's `job` worker with an attempt token | Proposes a campaign: template, targets (the core expands the selector), rollout policy, `idempotency_key`. The core checks the permissions of each item, the limits and the reserve and puts the campaign up for approval | `rejected{reason}`, `idempotency_conflict`, `fenced` | v0.1 | ORC-1, ORC-5, ORC-6, ORC-8, ORC-14 |
| `campaign.get` | the initiator, an external program, an attempt of its own tree | Returns the campaign record without free text | `not_found` | v0.1 | PRM-9 |
| `campaign.control` | channel on behalf of the person who pressed | The commands `pause`, `resume`, `skip(n)`, `cancel`, `retry_failed` with the card `nonce` | `forbidden`, `already_terminal` | v0.1 | APR-4, ORC-1 |
| `script.get` | the initiator, an external program, an attempt of its own tree | Returns the script run without free text | `not_found` | v0.1 | PRM-9 |
| `script.control` | channel on behalf of the person who pressed | The commands `pause`, `resume`, `cancel` for a script run; approval of the run and of the stops goes through `decide` with the subjects `script` and `script_gate` | `forbidden`, `already_terminal` | v0.1 | SCR-6, SCR-7, SCR-10, SCR-11 |
| `decide` (new subjects) | channel on behalf of the verified person | The subjects `campaign`, `script`, `script_gate`, `control` and `subject_request` of kind `erase` (a request to delete a person's data, D-76) | as for `decide` | v0.1 | APR-4, REP-6 |
| version window | all workers | The core accepts the current and the previous major version of P1; a worker outside the window gets `version_unsupported` at `hello`, not errors in the middle of work | `version_unsupported` | v0.1 | LCY-7 |

Messages between bots (`msg.*`), observations (`obs.query`), seat tasks, campaigns and scripts come in v0.1 after the core and the first bot (D-71); an implementation that claims only the `core` conformance profile is not required to have these operations. The first bot `examples/hello-bot/` does without them: P1 `claim` -> `heartbeat` -> `complete`, and the core delivers the answer to the person to the chat by `origin` (DLV-6).

### v0.2

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| `obs.append` | a `job` worker with an attempt token | Appends a point to a series from the `observes` of the attempt's task type; the core sets the time, the tenant and the producer; the model does not write points | `fenced`, `type_not_allowed`, `input_schema_invalid`, `ref_invalid`, `service_rate_limited` | v0.2 | OBV-1, OBV-2, OBV-8 |
| `escalation.submit` | a worker with an attempt token | An escalation to the manager by a closed schema; no more than one per attempt; the answer is only `{staged: true}` | `rejected{reason}`, `escalation_rate_limited`, `fenced` | v0.2 | MGR-5, MGR-6 |
| `grant.request` | a worker with an attempt token | A request for a model grant by a closed justification; code derives the right to ask from signs that the model does not control; the answer is only `{staged: true}` | `grant_not_eligible`, `fenced` | v0.2 | MGR-17, MGR-18 |
| `decide` (new subjects) | channel on behalf of the verified person | The subjects `takeover`, `governance_case`, `attribution` and `subject_request` of kind `export`; for the subject `proposal` also the answers of a blind sample card and "Lift veto". "Lift veto", "Confirm" of a case result and "Yes" on an attribution are accepted only if this person opened `card.details` of this card before. For the subject `attribution` the answers "No" and "Don't know" can be changed while the attribution window is open; "Yes" and the closing of the window are final, and this is the only exception to the one-time decision | as for `decide`; `details_not_opened` | v0.2 | APR-4, APR-14, GOV-9, GOV-13, SEC-11, DIG-6 |
| health in `hello` and `heartbeat` | toolhost, channel | Health report of plugins and the driver by a closed schema; the core compares it with its own observations | | v0.2 | OBS-10, STA-3 |
| `enqueue` with `mode: validate` | as for `enqueue` | A dry run: all enqueue checks without writing the task, without the idempotency key and without a budget reserve; it answers for a foreign and a nonexistent object the same way as a real enqueue; an audit line with the dry-run flag; a dry run that passes answers with reason `dry_run` and the `validate` block (type, `max_tier`, `data_mark`, the decision, the budget estimate) to the calling principal, not to the model; a caller with an attempt or lease token gets `forbidden`; a dry run counts against `max_enqueue_per_hour` (CAP-1) | as for `enqueue` | v0.2 | PRM-11, IDM-1 |
| `kb.index_put` | the ingest job of a corpus (`kb.ingest.v1`) under its lease | Writes the fragments of one document into the per-tenant corpus index in C1, only for the corpus and file of the leased task (another corpus, file or tenant is `forbidden`; a `curated` corpus only for files from git); a repeat for the same file replaces its fragments; the fragments are deleted with the file (C1 `file.purge`); the built-in tools `kb_search` and `kb_read` read only this index (D-97) | `forbidden`, `fenced`, `over_capacity` | v0.2 | KNW-5, KNW-4 |

### v0.3

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| `card.feedback` | channel on behalf of the person who pressed | The buttons "Useful" and "Not needed" on a farm manager notice `manager_attention` and an analysis report `postmortem_report`; the answer goes only into the share of useful notices in the holder report and changes nothing else | `forbidden`, `not_found` | v0.3 | MGR-52, MGR-50 |
| `decide` (new kinds) | channel on behalf of the verified person | The subject `attribution` for the candidates of kinds `rejected_proposal` (card `rejection_link_card`) and `script_run` (card `script_link_card`) with "Yes" only after `card.details`; the subject `proposal` for "Send to decide" on the digest item `replay_item` | as for `decide`; `details_not_opened` | v0.3 | GOV-40, TRN-7, DIG-9 |

## Guarantees

- MUST: exactly one holder holds a work unit. Each `claim` creates a lease with `lease_no` one higher; a changing call with an old token gets `fenced` without side effects (FEN-1, FEN-2). `lease_no` also grows on outcomes that do not spend an attempt, and the core counts attempts (`attempts_spent`) and runtime runs (`runs_counted`) separately (FEN-3, GW-13).
- MUST: the token carries the store epoch. After a restore from a backup, work and handles of the previous epoch get `fenced`; until reconciliation ends `claim` and `enqueue` answer `not_ready` (RET-6).
- MUST: while the C1 store is unavailable, P1 answers all operations with `not_ready` with `retry_after_s`, and the core starts no new model calls (OBS-8). Below the hard threshold of free store space all operations except `heartbeat`, `complete`, `fail` and `release` get `not_ready` (CAP-5); during a core drain `claim` answers `not_ready` (LCY-6).
- MUST: an answer with a lease carries `lease_remaining_s`, the remaining seconds by the store clock; the absolute time in the answer is for reference (TIM-1).
- MUST: a lease that expired while the core was not running or answered `not_ready` does not spend the attempt: on leaving `not_ready` the core extends such leases with the C1 operation `lease.extend`, and a lease not renewed even after that ends with `core_unavailable` (FEN-5).
- MUST: a transition, its event, the deliveries, messages and usage it produced are written in one transaction (DLV-2).
- MUST: work is dispatched at least once and ends in a terminal state with an event; a tool call is not dispatched again (DLV-1).
- MUST: a work unit carries the minimum for its kind: an attempt has no `origin`, address signature and external ids; a call for a toolhost has no `goal`, initiator and `origin` (SEC-4).
- MUST: the dispatcher gives out work only through `claim`, deterministically; a task has no "specific worker" field (DSP-1, DSP-2, DSP-4).
- MUST: holds are checked first; a task under a hold is not dispatched (HLD-1).
- MUST: a cancellation comes in the answer to `heartbeat`; calls and children of an attempt that lost its lease are refused (FEN-4). A pipeline, a plugin and a channel stop working by their own clock when the lease expired (FEN-6), and an external write carries the work id (FEN-7).
- MUST: a repeat of `heartbeat`, `complete`, `fail`, `release`, `cancel`, `decide` with the same arguments gives the same answer (IDM-3).
- MUST: there is no streaming output of model text; progress only as a stage from a closed list (DLV-11).
- SHOULD: the `claim` waiting time and the lease periods come from the binding; a task type may shorten the lease period and the heartbeat, but not exceed the farm ceilings (FEN-3).

A work unit (sketch; each kind has its own schema with a minimum of fields):

```json
{ "work_id": "w_01JB...", "kind": "attempt", "lease_no": 2, "epoch": 7, "tenant": "demo",
  "lease_until": "2026-10-05T16:20:00Z",
  "payload": {
    "task": { "id": "t_01JB...", "type": "ops.investigate.v1", "body": {},
              "record": { "max_tier": 1, "data_mark": "raw_logs", "untrusted_seen": false } },
    "instruction": "text of the role and task type",
    "handles": { "tools": "<handle>", "model": "<handle>", "session": null, "memory": null } } }
```

```json
{ "work_id": "c_01JB...", "kind": "toolcall", "lease_no": 1, "epoch": 7, "tenant": "demo",
  "start_by": "2026-10-05T16:05:30Z",
  "payload": { "target": { "plugin": "logs", "placement": "host", "host": "web1" },
               "tool": "tail", "args": { "source": "nginx-access", "lines": 200 },
               "approval_id": null, "grant_id": null, "args_hash": null } }
```

The core takes the machine in `target` from the argument that the tool manifest named in `target_from` (here `source` is bound to the resource `host-web1`); a tool with `target: any` gets a `target` without `host`, and any toolhost of the tenant with this plugin and placement takes it (TEN-3a). A call has `start_by`: 30 seconds from the record; a call that nobody took by this deadline ends with `error(no_executor)`, and the model gets a tool error, not a wait (CNT-4). A call to a plugin whose toolhost is `down` immediately gets `plugin_unavailable`.

```json
{ "work_id": "d_01JB...", "kind": "delivery", "lease_no": 1, "epoch": 7, "tenant": "demo",
  "payload": { "target": { "kind": "channel", "name": "telegram-demo" },
               "delivery": { "id": "d_01JB...", "kind": "task_result", "to": "origin",
                             "recipient": { "chat": { "kind": "direct", "external_id": "1001" } },
                             "body": { "form": "result", "text": "Hello, alice!" } } },
  "token": { "epoch": 7, "work_id": "d_01JB...", "lease_no": 1, "worker_id": "wk_01JB...",
             "key_id": "k1", "sig": "<core signature>" } }
```

A delivery carries a `target` (kind `channel`, `sink` or `monitor` and the name from `enroll`), and `claim(delivery)` gives it only to the process with that name (TEN-3). Where to render is said by `recipient` (the chat and its external id in the channel); the body shape is set by `body.form`: `result`, `card` (schema `card`), `text_code` (a text catalog key and parameters) or `data`. For `card_update` the field `replaces_external_id` names the `external_id` of the earlier delivery of this card to this chat.

## Bindings

- Schemas: the request and response of each operation by the map `spec/ports/operations.yaml` (rows `P1.*`); `spec/model/work-item.schema.json` (by `kind`), `enqueue-request.schema.json`, `activity.schema.json`, `message.schema.json`, `file-record.schema.json`, `erasure-work.schema.json`, `erasure-outcome.schema.json`, `reasons.yaml`; the `claim` filter conditions in the closed list `spec/model/dispatch-predicates.yaml` (D1-D20, DSP-3) and the task waiting reasons in `spec/model/waiting-reasons.yaml`.
- HTTP: `spec/bindings/http/README.md` (paths `/v1/p1/<operation>`, headers `Botaru-Lease`, `Botaru-On-Behalf` and `Botaru-Chat`, long-poll waiting, files, problem details and statuses, idempotency) from v0.1; an OpenAPI 3.1 description `spec/bindings/http/p1.openapi.json` (only `$ref` to `spec/model`) from v0.2, generated by `ci/gen_openapi.py` from the map, whose operations are named by the map id, for example `P1.claim` (MAN-12).
- Checks: the `conformance/suites/core-blackbox` suite, fixtures `lease/`, `delivery/`, `messages/`, `dispatch/`.

## What the port does not do

- It does not push work and messages into a running attempt: only pull and reads on request (MSG-8).
- It does not accept `owner` from a worker and does not bind a worker to a person: the core itself writes the owner of an assistant task into the lease (DSP-10).
- It does not execute tool calls: for the runtime the entry to tools is P3, and a toolhost takes a `toolcall` through `claim`.
- It does not accept task record fields from a worker (`tenant`, `initiator`, `origin`, `max_tier`, `data_mark`, `untrusted_seen`, `approval_id`, `grant_id`): only the core writes them (IDN-1).
- It does not offer operations of people: everything people change goes through P2; decisions on cards go through `decide` only from a `human` subject.
- It does not accept `action.*` in `enqueue`: only the core enqueues such tasks on approval (APR-5).
- It does not open cases, change the seat holder, or create incidents and alarms at a bot's request: bots have no such operations (GOV-6).
