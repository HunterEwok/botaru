# Architecture

## In short

The botaru farm is built as a thin core with replaceable edges around it. The core (the core process, its background loops, the tool gateway P3, the model gateway P4 and the store logic) holds all decisions: who may do what, what to hand out to whom, how much was spent, what is written to the audit. Everything else (agents, pipelines, tool executors, channels to people, file sinks, the monitoring system adapter) is workers: processes that take work from the core themselves and hand back the result. Watching machines and services and delivering alarms to people the farm takes from the organization's monitoring system and does not duplicate (D-59). The boundaries between the parts are set by thirteen ports of three kinds (D-38, D-66); every port has a sheet in `spec/ports/`. Every object belongs to exactly one tenant, and nothing crosses this wall: neither data, nor model context, nor provider keys. The skeleton describes the model, the ports, the contracts and the checks; an implementation lives separately and only adds port implementations, drivers and manifests.

## The core and its parts

The **core** is the farm code that all rules trust. Its parts:

| Part | What it does | Where it is described |
|---|---|---|
| Store C1 with the queue | Atomic transitions of tasks, leases, calls, approvals, deliveries, events, usage and audit. The task queue in C1 is the farm's pool itself, partitioned by tenant. Reference: PostgreSQL (D-03) | `spec/ports/c1-task-store.md` |
| Dispatcher | Decides what `claim` returns: a filter (tenant, role, labels, bot group, holds) and an order (priority class 1-5, fairness, queue). Work is handed out only through `claim` and deterministically (DSP-1, DSP-2, DSP-5); an empty bot group is legal, and tasks for it wait (DSP-8) | `docs/coordination.md` |
| Message inboxes | Messages between bots by role and group; published only in the transaction of a successful `complete` (MSG-9). Part of the conformance profile `messaging`, which an implementation may leave unclaimed (D-71) | `docs/messaging.md` |
| P1 WorkerProtocol | The only entry point of workers: `hello`, `claim`, `heartbeat`, `complete`, enqueueing tasks, channel input | `spec/ports/p1-worker-protocol.md` |
| P2 AdminAPI | Everything people change; the reference client is `farmctl` | `spec/ports/p2-admin-api.md` |
| P3 ToolGateway | The only source of tools for the runtime; an MCP server inside the core (D-45) | `spec/ports/p3-tool-gateway.md` |
| P4 ModelGateway | A model call: request check, reserve, charge, the tenant's key and workspace (D-45) | `spec/ports/p4-model-gateway.md` |
| C2 Authorizer | The "allowed or not" decision in the same transaction as the transition | `spec/ports/c2-authorizer.md` |
| Background loops | Time-driven duties: expired leases, schedules, deliveries, deadlines, erasure, series rollups, alarm episodes and their export to the monitoring system. Each one marks its tick, and from the ticks P2 `health` tells the monitoring system whether the core is alive (MON-2). Deterministic; they do not call the model | `spec/ports/background-loops.md` |

`farmctl` is the people's client of P2: it applies manifests from git, records people and workers, sets holds and shows the state. As a P2 client it does not bypass the core: every command goes through the same permission checks and is written to the audit (AUD-1). `farmctl` run on the core machine with the credentials of the core store is part of the core, including in the list of core processes and in the deployment lint: only core processes have access to the store (DEP-8). That is why, when the core process is down, `farmctl hold farm` writes a hold of the whole farm straight into C1, without channels, and an emergency stop works even when all other paths are broken (HLD-1).

There is no separate MCP server between the runtime and the core: the only MCP server of the runtime is the core's P3, and the tool handle goes in the header of every request. That is why the runtime has no worker token (DEP-5), and a tool call does not go through P1 on the agent's side: the core records the call itself, and the right toolhost takes it through `claim`.

A **worker** is a process that takes work through P1 `claim`. Worker kinds: `agent` (an E4 adapter plus a model runtime), `job` (a deterministic pipeline without a model), `toolhost` (an executor of MCP plugins, E3), `channel` (a channel to people, E1), `sink` (a file sink, E2), `monitor` (the monitoring system adapter, E5). A **bot** is not a process but an identity with a number, a role, a persona and a memory; its work runs on workers, and the identity outlives a change of worker. An **external program** is a client with its own key: a principal of kind `service` with groups, like a person (D-47); it enqueues tasks through P1 `enqueue` and reads its own tasks through `task.get`, and there is no separate permission model for programs.

What is deliberately not made a port: the event stream (it is the CloudEvents contract plus `emit` and the event log in C1), the scheduler (the `schedule` loop runs schedules; an external timer, if needed, calls P1 `enqueue` with a slot key), the identity provider (the step "credentials -> principal" is a mandatory part of P1 and P2), the secret store (secrets are referred to by name, the deployment supplies the values, SEC-1, SEC-2), usage accounting, files and the audit log (parts of C1), the plugin (its contract is the MCP profile in the E3 sheet), telemetry (the binding contract and rule OBS-1).

## Three kinds of ports

| Kind | Who calls | Where the boundary is | Ports |
|---|---|---|---|
| Core protocol | workers, the runtime and people call the core | network: HTTP, MCP | P1 WorkerProtocol, P2 AdminAPI, P3 ToolGateway, P4 ModelGateway |
| Port behind the core | the core calls a replaceable part | inside the core or its binding | C1 TaskStore, C2 Authorizer, C3 SessionStore, C4 MemoryStore |
| Edge port | an adapter takes work through P1 and talks to the outside world | inside the edge process | E1 Channel, E2 Sink, E3 ToolHost, E4 AgentRuntime, E5 Monitor |

Work is pulled: a worker makes a long-polling `claim` itself, a toolhost contacts the core itself, and a channel and the monitoring adapter take their deliveries themselves. The core pushes work to nobody and goes outside only to the model provider, through P4. One handout mechanism (a lease plus a fencing token) serves a task attempt, a tool call and a delivery, so the toolhost, the channel and the sink speak the same protocol as the agent. The meaning of the ports is the same as in hexagonal architecture: the application is driven in the same way by people, programs and tests and is checked separately from its databases and devices.

## Thirteen ports

| Port | Purpose | Kind | Sheet since | Thin | Reference |
|---|---|---|---|---|---|
| P1 WorkerProtocol | The only entry point of farm processes into the core | core protocol | v0.1; OpenAPI 3.1 binding since v0.2 | no | the core; the deployment chooses the transport |
| P2 AdminAPI | Everything people change and view | core protocol | v0.1 (v0.1 operations only) | no | `farmctl`; admin commands in a channel on behalf of a verified person |
| P3 ToolGateway | The tool catalog and tool calls for the runtime | core protocol | v0.1 | no | the core; the MCP tool protocol, the revision in the binding |
| P4 ModelGateway | A model call with check, reserve and charge | core protocol | v0.1 | no | the core, a field allowlist per provider |
| C1 TaskStore | Atomic transitions and the queue | behind the core | v0.1 | no | PostgreSQL |
| C2 Authorizer | A permission decision with a reason | behind the core | v0.1 (the permission table as a fixture) | no | a function in the transition's transaction |
| C3 SessionStore | Conversation transcripts under a key that includes the tenant | behind the core | v0.2 | yes | a table in the farm store |
| C4 MemoryStore | Long-term memory of people and bots, facts | behind the core | v0.1 | yes | a table in the farm store |
| E1 Channel | Input from people and deliveries to them | edge | v0.1 (v0.1 operations only) | no | Telegram |
| E2 Sink | Writing artifacts to external storage; `match` and `erase` for erasure | edge | v0.2 (the reference with `match` and `erase` since v0.1) | yes | disk |
| E3 ToolHost | Running MCP plugins on a machine or in a sandbox; starting, stopping and restarting catalog services with the OS mechanism `unit` | edge | v0.1 | no | toolhost |
| E4 AgentRuntime | The model loop inside an agent | edge | v0.1 | no | own loop in Go (D-93) |
| E5 Monitor | Export of farm alarms and the signed audit head to the external monitoring system, the farm nodes and template in it; input of its problems and running its scripts on catalog nodes through the catalog's closed vocabulary | edge | v0.1 (v0.1 operations only) | no | the organization's monitoring system (reference binding in the E5 sheet); a user of its own per adapter process |

The sheets P4, E3, E4 and E5 are part of v0.1 because v0.1 rules rest on them: the circuit breaker and the single retry layer of the model gateway, plugin placement by secrets and network, the RT replacement rules of the runtime, alarms to people only through the monitoring system, and the external check that the core is alive (MON-1, MON-2). The sheets for people and channels (P2, E1) are also in v0.1, but only with the operations of their version; operations of later versions enter a sheet as a minor version of the sheet. A "thin" port (C3, C4, E2) is one page, one reference adapter and a minimal set of checks; a thin port without a second implementation by the release after v0.2 is folded into the prose of the core sheet (D-46, D-80).

## What all ports share

- **The principal and tenant** of every operation the core takes from the credentials: of a worker, a channel, an external program, an operator or a handle (TEN-2). Bodies, requests, model arguments and messages do not carry the tenant. If an adapter of an external protocol sees a tenant field in a request, it compares it with the credentials; a mismatch gives `tenant_mismatch` and an alarm.
- **Errors** have one form: problem details per RFC 9457 plus a `reason` member from the closed registry `spec/model/reasons.yaml`. The registry says for every reason whether it spends an attempt and whether to retry. The model gets only a short "no permission"; the full reason is written to the audit (PRM-8).
- **Handles** (for tools, the model, a session, memory) are bearer credentials: at least 128 bits of randomness, they expire together with the lease, travel in the authorization header and never get into the model context, logs or audit (SEC-3). A handle is bound to the store epoch.
- **Not ready.** While store C1 is unavailable, or after a restore from backup until the reconciliation is finished, P1 answers `not_ready` with `retry_after_s`, and P4 starts no new model calls (OBS-8, RET-6). A lease that expires during that time does not spend the attempt (FEN-5).
- **Idempotency.** Repeating a changing operation with the same arguments gives the same answer, and side records are written once (IDM-3).
- **Erasure and search.** Every port with content has its own erasure operation by the scope `(tenant)`, `(tenant, subject)` and by a list of ids, with the outcome reported into the erasure request: C1 `file.purge` for files, C3 `delete` and `purge`, C4 `erase`, E1 `erase`, E2 `erase`, E4 `purge`, E5 `erase` (RET-4); erasing a person's data at their request is part of v0.1 and cannot be waived for `kind: production` (D-76), so v0.1 has all these operations except `erase` of channel E1 (v0.2; until then a channel without a cache answers `not_applicable`). The search operation `match(tenant, subject, needles) -> ids` exists for stores that the core does not read (E2, E4 and E5 since v0.1, E1 since v0.2): erasure by string match reaches them as well, and a store without `match` counts as `failed`; notes in the monitoring system give `not_applicable` for the scope `(tenant, subject)` and may give `failed` for a list of ids, and such an outcome does not keep the request open. Earlier versions have only what rules of those versions rest on: C3 and C4 erase on `forget` and when a person leaves (MEM-4), the reference sink E2 supports `match` and `erase` since v0.1, E4 `purge` and `match` are mandatory for runtimes with `runtime_internal` (RT-1, RT-2, RT-5), and C4 erases the sealed data of a departed bot (MEM-9). The session key of a group chat includes the principals who wrote, so the scope `(tenant, subject)` finds group sessions too (MEM-1). A store without an erasure operation answers `failed`, and the request stays open with an alarm (RET-7).
- **Telemetry** only by an attribute allowlist, without prompts, output or arguments (OBS-1).
- **Sheet version** of its own, by SemVer; a new operation is a minor version of the sheet.

## Runtime deviations

An agent runtime connects through port E4. In `describe()` it declares how it carries out four farm mechanisms, and it chooses values only from the closed list `spec/model/runtime-deviations.yaml`. The first value in each row is the farm model; any other value turns on its replacement rule, and that rule is non-waivable.

| Field | Values | What the deviation means | Rule |
|---|---|---|---|
| `session` | `farm`, `runtime_internal`, `none` | the runtime keeps the session itself: one profile per (tenant, role, principal) or per bound group chat; the profile store is cleaned by the tenant's retention period, erasure reaches it through the adapter (`purge`), and a principal's rows are found through `match`; the core keeps the session's untrusted mark | RT-1, RT-5 |
| `memory` | `farm`, `bridge`, `runtime_internal`, `none` | `bridge` equals `farm`. Memory inside the runtime: approval of entries is on, self-learning is off, model skills only after review; only the scope `person`, because a bot's memory is core records and verified facts (D-35); a task with `untrusted_seen` for such a role is rejected at enqueue with `rejected(memory_untrusted)`; a principal's rows are found through `match` | RT-2, RT-5 |
| `model_attribution` | `per_attempt`, `per_profile` | the model handle is issued per runtime profile, not per attempt: worker capacity is 1, the handle lives while the worker holds a lease, and usage is attributed to that lease | RT-3 |
| `catalog` | `per_attempt`, `per_role` | the runtime reads the tool catalog once per role: the catalog equals the role's read tools and the built-in tools of its profile, and the core checks everything that depends on the task on every call | RT-4 |
| `built_in_tools` | only `[]` | no deviations: built-in shell, files, web, browser and the provider's server-side tools are always off | INJ-9 |

Regardless of `describe()`, the model gateway checks that a request has no tools outside the P3 catalog (GW-1). So a runtime that "forgot" to turn off a built-in tool fails at the gateway instead of gaining an extra capability.

## Contract formats

| What | Format | Why |
|---|---|---|
| Data, task bodies, manifests, events | JSON Schema 2020-12, the single source of types; custom annotations only `x-botaru-*` | the current version of the standard; MCP understands tool schemas in this dialect by default |
| String formats of parameters | the closed registry `spec/model/formats.yaml`; only the skeleton sets regular expressions | a `pattern` of its own in a manifest can be bypassed (INJ-1) |
| P1 over HTTP | OpenAPI 3.1, only references to `spec/model` | the binding has no types of its own; long polling needs no streams |
| P2 | a list of operations in the sheet | there is no second client; OpenAPI arrives together with it |
| Events | CloudEvents 1.0 and the catalog `spec/bindings/events/catalog.yaml` | mandatory `id`, `source`, `specversion`, `type`; the pair `source` and `id` is unique (IDM-4) |
| Tools | the MCP tool protocol; the revision (not lower than 2026-07-28), the sessionless mode and passing client capabilities in `_meta` are set by the binding `spec/bindings/mcp/` | a runtime with another revision connects through a new binding, and the rules do not name the revision |
| OS mechanism for actions and services | the narrow mechanisms `oneshot` and `unit` with an access rule that `apply` generates; the reference binding `spec/bindings/os/systemd.md` (systemd units, a polkit rule) | the OS of the production server is not chosen by the rules: the rules say "OS mechanism", and the names live in the binding |
| Store | the SQL binding of the reference: signatures of the store functions and their tests | grows together with the reference implementation |
| Ports | Markdown, the words MUST, SHOULD, MAY per RFC 2119 | the meaning separate from the bindings |
| Scripts | a closed script interpreter per the schema `botaru.script/v1`: core code issues the steps, and conditions come only from atoms of a closed list (SCR-1, SCR-3, SCR-8) | we do not take a general-purpose process engine: its language is wider than core code can check at acceptance |
| Not taken now | protobuf, TypeSpec, AsyncAPI, OpenRPC, Smithy, WIT | the input and result of tasks are set by manifest schemas at run time; a second schema dialect is not needed |

**One source per fact.** The shape of data lives only in JSON Schema (`spec/model/`, `spec/manifests/`), a rule only as a row of the registry `conformance/requirements.yaml`, a lifecycle only as a file in `spec/lifecycles/`, a port operation only as a row of a sheet, a refusal reason only as a row of `reasons.yaml`. Everything else refers by identifiers: the OpenAPI operation `wp.claim` -> the sheet operation `P1.claim` -> the rule `FEN-2` -> the concept "lease". The skeleton CI checks these references.

**Conformance checking** follows three patterns. Fixtures are data without code: they need only a JSON parser. Requirements are a "golden list" of rules with a number, MUST-style text, a check method, a version and a profile. The runner is a black box at the core's address with an expected failures file, where both an unexpected failure and an entry that started to pass fail the run; its contract is written in `conformance/runner.md`. The runner controls clocks, background loop ticks, process stops, store faults and event injection through the test bench control port `spec/ports/test-control.md`, which the core opens only when the settings contain a test bench key, the store carries the test bench flag, and the store never had a tenant other than `kind: test` over its whole life, including a retired one; with the port enabled, the core does not start on a farm with such a tenant, and an empty farm is not a test bench (D-86 (4)); a production farm has no such port (D-85). A non-waivable rule (`waivable: false`) in the expected failures file fails the run. An implementation declares the skeleton version and conformance profiles in the field `conforms_to: {skeleton: <version>, profiles: [core, ...]}` (profiles `core`, `messaging`, `campaign`, `insights`, `manager`, `governance`); the claim is visible in P2 `status` and in the runner's report, and the runner checks the active rules of the claimed profiles whose `version` is not newer than the claimed version (MAN-8). So a first implementation can conform for the core without the farm manager.

| Profile | What is in it | Rule families (main ones) |
|---|---|---|
| `core` | everything without which the core does not hand out work and does not hold the tenant wall: tenants and the runtime, leases and idempotency, tasks and deliveries, permissions and approvals, security and deployment, holds, alarms and the monitoring system, money and the model gateway, the dispatcher and worker liveness | TEN, RT, FEN, IDM, DLV, CNT, OUT, MAN, IDN, PRM, APR, INJ, SEC, AUD, DEP, OBS, HLD, RET, CAP, TIM, LCY, MON, BUD, GW, ART, DSP, STA |
| `messaging` | messages between bots, inboxes, the reporter | MSG, REP |
| `campaign` | campaigns, scripts and the translator | ORC, SCR, TRN |
| `insights` | observations, analysis, proposals, the commission, the change trial | OBV, INS, AGG, DIG, PRP, COM |
| `manager` | the farm manager, incidents, control operations | MGR |
| `governance` | bot identities, seats, governance cases, bot memory | GOV, MEM |

The dispatcher rules (DSP) and the worker liveness rules (STA) are part of `core`, not of `messaging`: without them `claim` hands out nothing, whatever profiles are claimed. The exact profile of every rule is written in `conformance/requirements.yaml`. Within v0.1, per D-71, the `core` profile and the first bot come first, followed by the profiles `messaging`, `insights`, `campaign`, `manager` and `governance`. Only `core` is mandatory: an implementation may leave the other profiles unclaimed, and then their rules are not checked for it.

## Three object origins and the write rule

| Origin | Who creates it | Where the truth is | How it changes | Examples |
|---|---|---|---|---|
| Declared | the operator, by a commit to git | git; the store holds a copy with a status | `farmctl apply`, atomically and idempotently, towards the desired state | `Tenant`, `Group`, `Role`, `TaskType`, `ToolPlugin`, `Action`, `Catalog`, `Route`, `Schedule`, `Channel`, `Sink`, `MessageType`, `BotGroup`, `ObservationSeries`, `ModelProfile`, `Commission`, `IncidentClass`, `Governance` |
| Recorded | a person, by a P2 command (in `farmctl` or on their behalf in a channel) | the store | commands with audit | principal, identity, membership, chat binding, worker with `enroll` data, hold, seat |
| Working | the core and workers through the protocol | the store | only transitions of its own state machine | task, lease, tool call, approval, delivery, event, usage, session, memory item, fact, message, proposal, campaign, script run, incident, bot identity |

**The write rule.** The sender writes only the body (of a task and a message) or the `spec` (of a manifest). The task record, the system fields and the status are written only by the core; the schema rejects a request that contains such fields (IDN-1). So neither the model nor someone else's program can substitute `tenant`, `initiator`, `max_tier`, `data_mark`, `untrusted_seen` or `approval_id`. This is the main structural rule of the model: every new object of the skeleton states which of its fields only the core writes.

**Manifest status.** `apply` brings the store to the latest `spec` of the set and writes the status: the number of the applied commit, the hash of the set and the list of errors. A set is applied as a whole or not at all (MAN-6). An object that disappeared from git is shown by `apply` together with its dependents and is not deleted by it (MAN-2). Whether the live plugins match the manifest is checked by the toolhost's `hello` (INJ-2); there is no separate loop for it.

A **manifest in git** always sits in one wrapper:

```yaml
apiVersion: botaru/v1alpha1
kind: Role
metadata:
  name: ops-reader
  tenant: demo        # only for kinds that belong to a tenant
  labels: {}
spec: { }             # schema spec/manifests/<kind>.schema.json
# status is not written in git: the core keeps it (commit and errors)
```

## Tenants and the service tenant `farm`

A **tenant** answers the question "whose data is this and whose money". It is a wall that nothing crosses. Seven rules hold it:

- every record belongs to exactly one tenant, and the tenant never changes (TEN-1); a reference to an identity or a seat of another tenant is stored only as an id and an epoch in fields that the core writes, and nothing is read through it;
- the tenant and the principal are taken from the credentials, with no exception for any role (TEN-2);
- `claim` and `msg.receive` hand out only work and entries of their own tenant (TEN-3); a tool call is taken only by the toolhost whose record matches the call's `target` (TEN-3a);
- every worker process serves one tenant, and a catalog resource is declared in exactly one tenant (TEN-4);
- memory, sessions, files, inboxes, observation series, the catalog, knowledge, the sandbox and the model context do not mix tenants, and the model provider has a workspace and a key per tenant (TEN-5);
- global manifests carry no tenant data (TEN-6);
- the references `file_ref` and `refs` point only to the tenant's own objects; a foreign object and a nonexistent one give the same refusal (TEN-7).

Example. The tenants `demo` and `demo2` have websites on the same machine `web1`. Then two toolhosts run on `web1`: one is recorded in `demo`, the other in `demo2`, and each has its own resource catalog. A tool call from a `demo` task on the resource `host-web1` gets the `target` (demo, `logs`, host, `web1`), and only the `demo` toolhost can take it. The `demo` model cannot name a `demo2` resource: no such value is in the closed list of its task.

**A workspace per tenant.** Every tenant has its own workspace and key at the model provider, so that prompt caches and bills do not mix, and every workspace has its own spending and rate ceilings, because the organization's limits at the provider are shared (D-42). Only P4 has the provider key (GW-3). The core checks every day that the ceilings are in place. Details are in `docs/models-and-money.md`.

**Tenant kind and limits.** A tenant has a kind: `kind: test` or `kind: production`, and there are no other kinds; for a production tenant the lint forbids the "approves itself" setting (APR-9). A tenant has limits on the queue, on enqueues per hour and on disk space; exceeding them gives the refusal `over_capacity` rather than a silent slowdown for everyone (DSP-7, CAP-1).

**The service tenant `farm`.** There is no main tenant: every tenant has its own commission and its own data (D-23). The service tenant `farm` holds only what concerns the farm as a whole: the registry of the farm manager seat (holder, epoch, state, stop reasons) and the holder's identity in numbers and codes, the permissions of the farm operators, and the holds of the scopes `farm` and `seat`; a hold of any other scope is a record of the tenant of its scope (TEN-1, HLD-1). There are no model calls for control in `farm`. The farm manager's work happens in the tenants: the tasks of its seat are records of the tenant they concern, they are executed by workers of the seat's roles enrolled in that tenant, and model calls go with that tenant's key and are paid from its `manager` line (MGR-3, MGR-20, MGR-24). One attempt sees the data of exactly one tenant. This way the farm manager can watch over all tenants without moving data of one to another (D-30). A view of the whole farm is built only by core code, for the operators of `farm`, from allowlisted metadata (OBS-1, AGG-5); no bot attempt reads records of more than one tenant (TEN-5).

The **farm operator** is a person with `admin:*` permissions in the service tenant `farm`; they work with the core only through `farmctl` with their own P2 credentials. The operator operations are the closed list of PRM-14; in the P2 sheet each of them has "operator, `farmctl` only" in the column "Who calls". These are, for example, everything that needs a capability in `farm`, `apply`, workers, `chat.bind`, granting privileged groups and invitations to them, seats, holds of the scopes `seat` and `farm`, and the redrive of a dead task or delivery in any tenant. They go only through `farmctl` and never from a channel, even on behalf of a person with such permissions (IDN-7, PRM-14). A tenant appears only through a manifest in git (`tenants/<name>/tenant.yaml`) and `farmctl apply`; there is no command for adding a tenant that bypasses git (MAN-5).

## Trust boundary and placement

Inside the trusted boundary: the core, store C1, the tool gateway P3 and the model gateway P4 with the provider keys. Outside: workers, runtimes, plugins, channels, sinks, the monitoring adapter and the organization's monitoring system itself. Only core processes have credentials and a network path to the store; everything else talks only through ports (DEP-8). That the model does not get a shell on a production machine is checked by machinery (schemas, lint, placement), not by a reviewer (INJ-1, DEP-1).

| Machine | What runs on it | Why |
|---|---|---|
| Core machine (`core1`, a farm node) | the core process (P1-P4, background loops), store C1, channel processes | the trusted boundary; only P4 has the provider keys. A farm node is not a tenant catalog resource and runs no tenant toolhost: the list of farm nodes (`farm_hosts`, in the example `[core1, backup1]`, on a single server `[core1]`, D-88) is in the deployment description, `apply` rejects a catalog where `monitor_host` or `machine` matches a farm node, and `doctor` checks that the E5 adapter user has no scripts on the farm node group (DEP-11) |
| Tenant catalog machine (`web1`, `db1`, `ctd1`) | toolhosts `placement: host` of their tenant; own plugins without secrets and network as child processes, the others in their own containers; a write plugin in a separate toolhost | a production machine: the model has no shell and no free-form input; only an own plugin runs as a child process, and writing goes only through a separate action toolhost and a narrow OS mechanism: `oneshot` for actions, `unit` for catalog services; `apply` generates the access rule from the catalog, and the deployment installs it; which mechanism it is, the OS binding says (DEP-1, DEP-4, DEP-7, DEP-9, DEP-10, MON-8) |
| Sandbox VM | toolhosts `placement: sandbox` | free-form input only here; no production data, network or secrets; a sandbox toolhost equals root on its VM (DEP-1, DEP-3) |
| Agent machine | E4 adapters and runtimes in their own containers | the runtime has no provider key and no worker token, and its network reaches only P3, P4 and the adapter (DEP-5) |
| Channels, sinks and the monitoring adapter | processes `channel`, `sink` and `monitor`, one process per tenant, on farm nodes; the monitoring adapter also as a separate process in `farm` | the worker token of their tenant (TEN-4); a channel also holds the P2 secret of its driver, one per driver in a tenant (D-87 (3)); every monitoring adapter process has its own monitoring system user with permissions only on the node groups of its tenant: the `farm` process has its own nodes and templates, and a tenant's process reads the nodes of its catalog and runs scripts only on them (MON-4, MON-10) |
| The organization's monitoring system | watching machines and services, alerting people, acknowledgement, escalation, on-call, scripts (reference binding in the E5 sheet) | present if the installation claims the `monitor` profile (D-88); without it, a watchdog `kind: watch` on a farm node polls the core's liveness, and the core delivers alarms to people (MON-13). Not on a farm node; it polls P2 `health` with its own key (the value comes from the deployment configuration, not from C1; two values during a rotation) and wakes people by its own path, not through a farm channel; it has no credentials of the core, the store, the channels or the model provider (MON-2). It receives tenant data only as numbers, `enum` values, catalog ids and notes of class no higher than `summary` after redaction (TEN-5) |
| Backup host | backups of the store | a backup is part of the production perimeter: it is encrypted and kept on a farm node that is neither the core machine nor a tenant catalog machine, or, on a single server, goes encrypted to external storage (RET-5, DEP-11, D-88); the external backup process reports to the core with the P2 operation `backup.report`, from which the backup age and the RET-8 alarms are computed |
| Model provider | outside the farm | a workspace and a key per tenant (D-42) |

The threat model, plugin placement, deployment rules and people's data are described in `docs/security-model.md` and `docs/privacy.md`. The topology is set by the file `deploy/compose.example.yaml`: what runs on which machine and under which user; it is not runnable. The deployment description is data per the schema `spec/model/deploy-topology.schema.json`, and the lint checks the placement rules against it (DEP-10). In it, the top-level key `monitoring: {url, address}` names the organization's monitoring system, the key `farm_hosts` lists the farm nodes (in the example `[core1, backup1]`: the core, the store and the channels on `core1`, backups on `backup1`; the backup process on `backup1` runs outside the core and reports with its own key through P2 `backup.report`, RET-5, RET-8), the key `health_keys` holds one or two valid values of the `health` key, and the monitoring adapter is described as a process `kind: monitor` with the OS user `botaru-mon`, the network `monitor-out` and the secrets `worker-token-monitor` and `monitor-api-token` (one of its own per adapter process); the lint checks that `address` is not a farm node (MON-2) and that no farm node is named in a tenant catalog (DEP-11).

## Contract versions

- **The public API of the skeleton** is the folders `spec/` and `conformance/`. The repository version follows SemVer 0.y.z (`spec/VERSION`); what compatibility promises before 1.0.0 (schemas and `.vN`, values of closed registries, rule numbers, `apiVersion`, the text of documents) is written in [spec/versions.md](../spec/versions.md), and every change is visible in the changelog.
- **Skeleton versions** v0.1 and v0.2 are content slices: from which version a concept, an operation or a rule is part of the skeleton and is checked.
- **Manifests** have `apiVersion: botaru/v1alpha1` until release 1.0.0 and `botaru/v1` after it (D-44). A manifest kind from an implementation is registered under its own `apiVersion` group and grants no capabilities (MAN-4).
- **Additive only.** Within version `.vN` of a task type, an event or a schema, only optional fields may be added; a breaking change is `.v(N+1)` next to `.vN` (MAN-3). The skeleton writes the check for JSON Schema itself (`ci/check-additive`): a field cannot be removed or renamed, a required one cannot be added, and the type or `enum` of an input cannot be narrowed. A value of a closed registry is never removed and never changes its meaning within a major version of the skeleton; a retired value stays as an entry. For OpenAPI a breaking change check runs. The core is the main validator, and workers tolerate unknown fields.
- **Port sheets** are versioned separately; the map of all versions (sheets, the `/v1` path, `.vN` suffixes, `apiVersion`) is in `spec/versions.md`.
- **The P1 version window**: the core accepts the current and the previous major version of the worker protocol; a worker outside the window gets `version_unsupported` at `hello` rather than errors in the middle of work (LCY-7). Before a rollout the core goes through a drain: P2 `core.drain` stops handing out work and lets running work finish (LCY-6).
- **Thin ports** C3, C4 and E2 without a second implementation by the release after v0.2 become prose (D-46, D-80).
- **Secret rotation** (v0.1): every secret (a worker's P1 key, the `health` key, the monitoring adapter token, provider keys, a channel secret) has its own recipe in `docs/operations.md`, and two values are valid during a rotation; the core signs the audit head with an Ed25519 key with an id (AUD-6); the hashes and the signature are described in [spec/hashing.md](../spec/hashing.md).
- **Manifest deprecation and key revocation** (v0.2): `deprecated` and `sunset` on a manifest, the full `spec/compatibility.md` (v0.2), a key id on everything the core issues, and revocation on compromise (MAN-7, SEC-5, SEC-6).

## Repository layout

The `botaru` repository is licensed under the GNU AGPL version 3 (AGPL-3.0-only, D-83). Its executable code is in `ci/` (linters for schemas, links, "additive only", manifest rules and public safety) and in `impl/`, where the reference implementation in Go writes the core, `farmctl` and the black-box runner (D-91, D-92); it does not change skeleton files.

```
botaru/
├── README.md                      what this is, what is not here, where to start, how to run ci/
├── LICENSE                        AGPL-3.0-only
├── CONTRIBUTING.md                what I add, where I put it, what I run; contribution terms
├── SECURITY.md                    how to report a vulnerability privately
├── docs/                          the model in words
│   ├── overview.md                the farm on one screen, one message, layers, versions
│   ├── architecture.md            core, ports, tenants, trust boundary, versions, repository
│   ├── extending.md               extension recipes
│   ├── task-language.md           task language, body, catalog, tiers
│   ├── tasks-and-delivery.md      task, lease, call, delivery, manifest
│   ├── permissions.md             permissions, approvals, human decisions
│   ├── security-model.md          threats, trust boundary, deployment
│   ├── privacy.md                 people's data, retention, erasure
│   ├── messaging.md               bot messages, inboxes, the reporter
│   ├── coordination.md            dispatcher, bot groups, campaigns
│   ├── operations.md              hold, alarms, observability, backups, tenant lifecycles
│   ├── monitoring.md              monitoring system: alarm export, core liveness, problems, notes
│   ├── capabilities.md            files, knowledge, skills, external events
│   ├── models-and-money.md        model profiles, slots, budgets, the model gateway
│   ├── improvements.md            observations, analysis, proposals, commission, trial
│   ├── scripts-and-translator.md  scripts, translator, role profiles
│   ├── bots.md                    identity, memory, facts, bot departure
│   ├── commission.md              commission, seats, governance case
│   ├── farm-manager.md            farm manager, incidents, control operations
│   ├── quality.md                 eval, readiness criteria
│   ├── decisions.md               decisions with dates
│   ├── open-questions.md          open questions
│   ├── model/
│   │   ├── glossary.md            glossary with a "do not confuse with" column
│   │   └── relations.md           objects and cardinalities
│   └── adr/                       README.md, template.md and NNNN-title.md in MADR 4.0.0 format
├── spec/                          the public API
│   ├── VERSION                    0.1.0
│   ├── README.md                  how schemas, registries, lifecycles, fixtures and suites are laid out
│   ├── versions.md                version map and compatibility promises
│   ├── hashing.md                 canonical JSON form, SHA-256 hashes, audit head signature
│   ├── model/                     JSON Schema 2020-12 and closed registries (reasons, capabilities, formats,
│   │                              runtime-deviations, builtin-tools, role-profiles, message-kinds, holds,
│   │                              monitor-map, ...)
│   ├── manifests/                 wrapper, a schema per kind, drivers/ with the driver meta-schema
│   ├── lifecycles/                lifecycles: <object>.md
│   ├── ports/                     README.md, 13 sheets (including e5-monitor.md), background-loops.md,
│   │                              operations.yaml (operation map: port, version, schemas, rules),
│   │                              test-control.md (test bench control port, test bench only)
│   └── bindings/                  http/ (README.md and admin.md: the HTTP v1 envelope), mcp/,
│                                  model-providers/ (README.md, the reference anthropic.md, mock.md), events/,
│                                  channels/ (README.md, the reference telegram.md), telemetry/, monitor/, os/
├── conformance/                   how to check any implementation
│   ├── requirements.yaml          rule registry: text, checks, waivable, version, profile
│   ├── rules.md                   readable list of rules by family, generated from the registry
│   ├── retired.md                 retired numbers and what replaced them, generated from the registry
│   ├── fixtures/                  data only: schemas, permissions, transitions, leases, messages, canaries
│   ├── suites/                    check descriptions: core-blackbox, store, channel, toolhost, runtime,
│   │                              model-gateway, monitor, lint; the implementation writes the check code
│   ├── runner.md                  contract of the black-box runner and the test bench
│   └── expected-failures.example.yaml
├── examples/                      only the tenants demo, demo2 and domains under example.org
│   ├── tenants/demo/              tenant, groups, catalog, routes, schedules, channels, sinks
│   ├── hello-bot/                 first bot: role, task type, P1 `claim` -> `heartbeat` -> `complete` with delivery to a chat
│   ├── flows/                     examples as sequences of port operations
│   └── model-profiles/            sample model lineup
├── evals/                         eval case format, cases, test bench data, thresholds
├── deploy/compose.example.yaml    topology: what runs on which machine, not runnable
├── deploy/monitor/                farm nodes and template and the account role for the monitoring system
├── ci/                            the skeleton checks
└── impl/                          the reference implementation in Go (D-92)
```

This document and the other texts of the skeleton are written in words and tables. The machine forms (JSON Schema in `spec/model/` and `spec/manifests/`, the closed registries `spec/model/*.yaml`, the bindings in `spec/bindings/`, the lifecycle YAML next to the files `spec/lifecycles/<object>.md`, the registry `conformance/requirements.yaml`, the fixtures and suites in `conformance/`) live next to the text and are maintained as the single source of their facts.

## Decisions (ADR)

Decisions are recorded in `docs/adr/` in MADR 4.0.0 format: "Context and Problem Statement", "Considered Options", "Decision Outcome", and the optional "Decision Drivers", "Consequences", "Confirmation"; in "Confirmation" an ADR refers to rule numbers. Statuses: proposed, accepted, rejected, deprecated, superseded.

The table below is the list of future ADRs: none of them is written as a file in `docs/adr/` yet, and each one appears with the implementation (D-86 (13)). The status in the table is the status of the decision itself: an accepted decision is already recorded as a row D-NN in `docs/decisions.md` (the column "Decision") or in the skeleton text.

| ADR (appears with the implementation) | Status | What is decided | Decision |
|---|---|---|---|
| A skeleton from the model: 4 layers, 13 ports | accepted | concepts, lifecycles and rules come first; ports are derived from the model | D-38, D-66 |
| A thin core and the queue in the store | accepted | the core is thin, the queue is in C1, the C1 reference is PostgreSQL | D-03 |
| One lease for all kinds of work | accepted | an attempt, a call and a delivery are handed out by a lease with a fencing token | in the skeleton text, without a separate entry |
| Task state names | accepted | the terminal states `completed`, `failed`, `canceled`, `rejected`, as in the A2A protocol | in the skeleton text, without a separate entry |
| Events | accepted | CloudEvents 1.0 and the event catalog | in the skeleton text, without a separate entry |
| Schemas | accepted | JSON Schema 2020-12 as the single source of types, OpenAPI 3.1 for P1 | in the skeleton text, without a separate entry |
| Deployment | accepted | containers and compose | D-08 |
| No `input_required` | accepted | an attempt does not wait for a person's answer; a clarification is a new task in the same conversation | D-41 |
| Scope of the idempotency key | accepted | `(tenant, idempotency_key)`; a task is returned only to the same initiator with the same body | D-43 |
| Manifest `apiVersion` | accepted | `botaru/v1alpha1` until 1.0.0, then `botaru/v1` | D-44 |
| P3 and P4 inside the core | accepted | the tool and model gateways are part of the core, and there is no intermediary between the runtime and the core | D-45 |
| Generalizing the worker protocol | accepted | work instead of tasks, `claim` with work kinds, the tool call moves from the agent's P1 into the core | D-45 |
| Thin ports | accepted | a thin port (C3, C4, E2) without a second implementation by the release after v0.2 becomes prose | D-46, D-80 |
| A workspace per tenant | accepted | a workspace, a key and ceilings of its own at the provider per tenant | D-42 |
| A program as a principal | accepted | an external program is a `service` principal with its own key and groups | D-47 |
| Bot communication | accepted | messages by types from git, inboxes, publication at `complete`; the model writes to a role or a group, and only code writes to one bot | D-14, D-32 |
| Hold | accepted | one concept of stopping by scope, checked first | in the skeleton text, without a separate entry |
| External monitoring | accepted | watching machines and delivering alarms to people belong to the organization's monitoring system; port E5 with a reference binding in its sheet; the monitoring system checks that the core is alive; every adapter process has its own user with narrow permissions; the model sees script names and trigger classes only from the tenant catalog | D-59..D-64, D-66 |
| Technologies in bindings | accepted | the OS mechanism (units, access rule) and the tool protocol revision live in the bindings `spec/bindings/os/` and `spec/bindings/mcp/`; the rules say "OS mechanism" and "tool protocol" | in the skeleton text, without a separate entry |
| A farm node is not a tenant resource | accepted | a machine with the core, the store, a channel or backups is never a tenant catalog resource and runs no tenant toolhost; `apply` and `doctor` check against the list of farm nodes (DEP-11) | in the skeleton text, without a separate entry |
| Service control | accepted | bots with a role permission start, stop and restart services declared by the tenant catalog, with the OS mechanism `unit` or a monitoring system script, and run its scripts on catalog nodes; without a card only a `restart` from the allowed list `no_card` (D-74), start and stop with a card, scripts by the permission of the role and the initiator (D-73); guards, audit and a note in the monitoring system; a task with untrusted text goes with a card (D-70, D-82) | D-67..D-70, D-73, D-74, D-82 |
| Proposals and the commission | accepted | a bot proposes, code counts, a human decides | D-16 |
| New manifest kinds | accepted | `ObservationSeries`, `ModelProfile`, `Commission` | in the skeleton text, without a separate entry |
| Campaign | accepted | a mass action from a plan snapshot with one approval | D-20 |
| Script and translator | accepted | a closed plan of existing types and actions, run only by the initiator's button | D-15 |
| A farm manager across tenants | accepted | `farm` holds only the seat registry and the holder's identity; the records and work of the seat are in the tenants; one attempt sees one tenant; references only by id and epoch (TEN-1, MGR-3, MGR-24) | D-30, D-33 |
| Bot identity and governance | accepted | an identity grants no permissions; a human assigns a seat; votes in v0.2 | D-19, D-21, D-33 |
| Agent runtime | accepted | the reference implementation runs its own loop over P4 without a ready-made agent; the E4 port stays open to other runtimes | D-93 |
| Implementation language | accepted | the reference implementation and the runner are written in Go in `impl/`; ports keep other components free to use any language | D-91, D-92 |

An ADR is needed for a decision that changes a port or a contract: an operation or a guarantee of a port sheet, a wire schema, a lifecycle state, a closed list that permissions depend on (D-86 (13)). An ordinary new rule that changes no port and no contract is recorded as a registry row that refers to a decision D-NN. In addition, these need an ADR: a new auto-stop rule, a new message genus, a new dispatcher condition, a new kind or predicate of a fact, a new role profile, a new manifest kind that affects permissions (MAN-4). If an extension requires changing the core, the skeleton lacks a port: that is also a reason for an ADR, not for a workaround.

## What is in which version

| Version | What appears |
|---|---|
| v0.1 | The core and the thirteen ports; the sheets P1, P2 (v0.1 operations), P3, P4, C1, C2, E1 (v0.1 operations), E3, E4, E5 (v0.1 operations) and the background loop contract; alarm export to the monitoring system and P2 `health` for the external liveness check of the core (MON-1, MON-2); the dispatcher in the core (DSP, STA-2); the first bot `examples/hello-bot/` (P1 `claim` -> `heartbeat` -> `complete` with delivery of the answer to a chat, without messages between bots) and one science flow `data.describe.v1` with the `data` plugin (D-72); erasure of a person's data at their request through the erasure operations of the ports, not waivable for `kind: production` (D-76); contract formats, the rule registry with conformance profiles, the runner and expected failures; three object origins and atomic `apply`; the tenant wall (TEN-1..7), workspace and ceilings per tenant, the tenant kind; farm nodes outside tenant catalogs (DEP-11); the runtime replacement rules (RT-1..5); `match` and `erase` of the reference sink and of a runtime with `runtime_internal`; `not_ready` when the store is unavailable and after a restore, `restore.complete --spend-pending`; `backup.report` from the external backup process; minimal `retention` (classes `content` and `observation`); secret rotation recipes; store credentials only in the core; the service tenant `farm` (operator permissions, the hold of the `farm` scope); operator operations only through `farmctl` (PRM-14); after the core and the first bot (D-71): messages between bots and inboxes, the improvement cycle (series, analysis, proposals, commission, digest), reporters, campaigns, scripts and the translator, the farm manager; the tenant opt-out of the farm manager `farm_manager.enabled: false` (D-77); the registry of the farm manager seat in `farm`; sheet C4 (facts); identity fields in the lease and the token; the P1 version window and the core drain before a rollout with the reason `draining` in `health`; input of monitoring system problems through E5 by the catalog's `monitor_triggers`, scripts by `monitor_scripts_allowed` and the `monitor` plugin for reading (MON-5, MON-6, MON-10); `/service` for people with a permission on a service |
| v0.2 | The OpenAPI binding for P1; sheets C3 and E2; the worker drain period and cleaning of the runtime profile on revocation; accepted plugin pairs; manifest deprecation and `spec/compatibility.md` (v0.2); erasure and search operations in all other ports with content; the attempt journal and debug replay of a task under `operator_access.debug_journal`; export only to a sink with `export_only`; bot notes on problems and maintenance windows of the monitoring system as holds with `pause_action` (MON-7, MON-9) |
| Later | Overflow of work outside the group, high availability of the core, `undo` compensation, a cache of tool output |

## Why it works this way

- **The smallest vocabulary.** A port appears only if a part has to be replaced or checked at a boundary; there are no ports "just in case".
- **One work handout mechanism.** The lease with a fencing token is the same for an attempt, a call and a delivery; so all edges speak one protocol and are checked by one suite.
- **Decisions only in the core.** Permissions, budgets, delivery addresses and records are written by core code; the model and the edges send only bodies, and the schema rejects system fields.
- **The boundary is checked by machinery.** Closed schemas, plugin placement and the deployment lint hold the rule "the model has no shell on a production machine" without a reviewer.
- **We do not duplicate what others do.** The organization already has a monitoring system: it watches machines, wakes people, runs on-call and scripts. The farm hands it its alarms and the check of its liveness and itself watches only what the monitoring system does not see: tasks, bots, money, campaigns, secrets, the farm manager and the commission (D-59, D-60).
- **Neutrality.** The public API is data files, not a library; the chosen components are named reference adapters; the text of registry rules names no products (PostgreSQL, Telegram, a specific monitoring system, a specific runtime), and their names appear only in the binding sheets, the E5 sheet, `docs/monitoring.md` and examples (D-66); checks run as a black box over the network.

## What is not decided

- **Organization decisions** (hosting, the OS of the production server) are not part of the skeleton; the skeleton leaves bindings and `deploy/compose.example.yaml` for them.

<!-- generated: rules -->
## Rules of this section

Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md](../conformance/rules.md).

| Rule | About | Since | Non-waivable |
|---|---|---|---|
| [RT-1](../conformance/rules.md#RT-1) | Session inside the runtime | v0.1 | yes |
| [RT-2](../conformance/rules.md#RT-2) | Memory inside the runtime | v0.1 | yes |
| [RT-3](../conformance/rules.md#RT-3) | A model handle per profile | v0.1 | yes |
| [RT-4](../conformance/rules.md#RT-4) | A catalog per role | v0.1 | yes |
| [RT-5](../conformance/rules.md#RT-5) | Search in the runtime for erasure | v0.1 | yes |
| [TEN-1](../conformance/rules.md#TEN-1) | One record, one tenant | v0.1 | yes |
| [TEN-2](../conformance/rules.md#TEN-2) | Tenant and principal from the credentials | v0.1 | yes |
| [TEN-3](../conformance/rules.md#TEN-3) | Only its own tenant takes work | v0.1 | yes |
| [TEN-3a](../conformance/rules.md#TEN-3a) | Tool call target | v0.1 | yes |
| [TEN-4](../conformance/rules.md#TEN-4) | A process serves one tenant | v0.1 | yes |
| [TEN-5](../conformance/rules.md#TEN-5) | Data and context do not mix tenants | v0.1 | yes |
| [TEN-6](../conformance/rules.md#TEN-6) | Global manifests without tenant data | v0.1 | yes |
| [TEN-7](../conformance/rules.md#TEN-7) | References only to one's own objects | v0.1 | yes |
