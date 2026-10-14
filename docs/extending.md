# Extending the farm

## In short

The farm grows without changes to the core (D-02, C9). Roles, task types, plugins and tenants are manifests in git; channels, monitoring systems and model providers are implementations at a boundary. This document collects the recipes: what to add, what the core checks and what never changes. If an extension needs the core changed, the skeleton lacks a port or a concept: that is a reason for an ADR, not a workaround. All examples are on the tenant `demo`.

## How it works

### Three ways to add something

1. **A manifest of a known kind**: a role, a plugin, a task type, an action, a message type, a bot group, a tenant, a route, a schedule. Neither the core nor the skeleton changes.
2. **A driver inside a kind**, such as a new channel. The adapter brings the schema of its `spec.config`, which must pass the driver meta-schema in `spec/manifests/drivers/`. Fields that rules depend on (link previews, markup, egress, placement, ceilings, fallback providers) cannot be in a driver config; they exist only in the common wrapper under the core's check ([MAN-4](../conformance/rules.md#MAN-4)).
3. **A manifest kind of your own** (rare): under your own `apiVersion` group, only if it grants no capabilities, declares no tools or actions and passes the `authorize/` fixtures. A kind that affects permissions needs a skeleton ADR (MAN-4).

Paths below are from the root of your installation repository; training samples are in the skeleton's `examples/`. Until `impl/` ships `farmctl`, check the shape of your files with `python3 -I ci/check_manifests.py --manifests <folder>` ([ci/README.md](../ci/README.md)); the `farmctl` steps run only on an implementation's farm. Keep the binding note of a new channel or provider in your own repository and attach it to an issue "binding: <name>"; a pull request to the skeleton is possible after signing `CLA.md` (D-89). [CONTRIBUTING.md](../CONTRIBUTING.md) has the table of what goes where and what to run.

For any extension the core checks the kind's schema (MAN-1), no tenant data in global manifests (TEN-6), additive changes within a version (MAN-3) and an atomic set (MAN-6). No extension changes the tenant wall (TEN-1..7), the write rule (IDN-1) or the non-waivable rules.

### 7.1. A new kind of bot

**Add.**

1. Task types (7.7).
2. `roles/<name>/role.yaml` with `claims`, `models.base`, `worker_kind` (`agent` or `job`) and a `profile` from `spec/model/role-profiles.yaml` (`worker` by default). Optional: messages, `tools`, `actions_may_request`, `may_enqueue`, `memory` (`none` by default), `insight`, `runtime`, `skills`.
3. `task:<type>` in the groups of the people who may use it.
4. How a task reaches the role: a command in `Tenant.spec.commands`, `default_types`, a `Route` or a `Schedule`. Code chooses the type, never text; without this step the role gets no work.
5. 30-50 eval cases in `<folder>/evals/cases/<role>/`, checked with `python3 -I ci/check_evals.py --cases ... --seed ... --manifests ...`. Without cases a bot is not admitted on a production farm ([quality.md](quality.md)); the training hello-bot comes without them.
6. `farmctl apply`, `farmctl worker enroll --tenant demo --role <name>`, start the worker. `--bot` binds an existing identity.

`examples/hello-bot/` is the smallest sample: role, task type, group, a mock model scenario and the fixture sequence `submit_activity` -> notices -> `claim` -> `heartbeat` -> `complete` -> delivery by `origin` (D-71).

**Checked.** The role fits its profile (PRF-1); no `action.*` in `may_enqueue` (APR-5); `person` memory only for roles without untrusted input (MEM-2); model rank within the profile's ceiling (GOV-3); message addresses pass the matrix (MSG-5).

**Never changes.** The core and other roles. A `job` bot is made the same way without a runtime.

### 7.2. A new tool: "program + MCP"

A new kind of bot is a program with an MCP server that passed review (D-39).

**Plugin author.** Pin the image by digest (DEP-6). Write `tools/<plugin>/tool.yaml`: MCP revision, `placement`, egress only to catalog addresses, secret names, and per tool `access`, `tier`, `output_class`, `untrusted_output`, closed parameters, an output limit and redaction. Anything that writes is a separate tier 3 plugin with an `Action` run by the OS mechanism `oneshot`, never a shell (DEP-4). Add eval cases with an injection in every output that carries outside text, and compare your server's `tools/list` with the manifest using `--tools-list`.

**Operator.** An own plugin without secrets and network runs as a child of the toolhost (DEP-7); anything else in its own container with its own user and network (DEP-10); a write plugin in a separate toolhost (DEP-9). Add resources to the catalog, the tool to a role, and enroll the toolhost (DEP-2).

**Checked.** At `apply`: closed parameters (INJ-1), annotations agree with the manifest (INJ-3), egress from the catalog (INJ-6), program-running plugins (INJ-14). At `hello`: the live `tools/list` matches the manifest, otherwise the toolhost is `blocked` (INJ-2). The model never sees a write tool (APR-1).

### 7.3. "A robot with a terminal"

Not a new concept, but variants of one plugin:

| Variant | Where | Decision |
|---|---|---|
| `cmd` "on rails": named commands, `argv` as an array, parameters from lists, no shell | catalog machine, tier 1 | allowed (INJ-14) |
| shell and Python in a throwaway container on a copy of the data | `placement: sandbox` on a separate VM, tier 2 | allowed (DEP-1, DEP-3) |
| a terminal MCP server or free shell on a production machine | | fails `apply` (INJ-1); only an ADR can change this |

The model still calls through P3, keys never reach the sandbox, and `run(argv)` arguments are audited as hash and length (AUD-2).

### 7.4. A new channel

**Add.** An E1 adapter as a worker of kind `channel`: input (authenticity, sender, `Activity`, attachments through `file.put`) and output (`claim` of deliveries, rendering by capabilities). The driver declares `identity_assurance`, `membership_assurance`, the identity form, capabilities (`max_text`, `buttons`, `files`, `edit`), how a person learns they talk to a bot, and `stores_content` in P1 `hello`. It holds a worker token and one P2 secret per driver in a tenant (D-87 (3)). Add a binding note from `spec/bindings/channels/README.md`, `tenants/demo/channels.yaml` and a process per tenant.

**Checked.** The `channel` suite (IDN-3, IDN-7, APR-3, DLV-5..7, INJ-11). A group binding above `summary` needs an `enforced` driver (DLV-7). A weak channel runs no privileged commands, and no channel runs operator operations.

### 7.5. A new agent runtime

**Add.** An agent worker image: the adapter speaks P1 (`hello`, `claim(attempt)`, `heartbeat`, `complete` or `fail`) and holds the token; the runtime runs in its own container without the token or the provider key (DEP-5). Its only MCP server is P3, its only model path is P4; built-in tools, provider server tools, fallback providers and self-learning are off (INJ-9). The adapter declares the runtime in the `runtime` object of `hello`, with values only from `spec/model/runtime-deviations.yaml`, which turns on RT-1..5 as needed. It obeys the `limits` of each attempt work item (BUD-9) and stops on `cancel_requested` (FEN-4). Set `runtime.driver: <name>` in the role and restart the workers. The description is `spec/ports/e4-agent-runtime.md`.

**Checked.** The runtime checks of the `core-blackbox` suite with a mock model behind P4, then a full eval next to the current runtime ([quality.md](quality.md)). Only waivable rules may be expected failures. P4 rejects tools outside the P3 catalog regardless (GW-1).

**Never changes.** The runtime does not see the token or the key, does not reach the model around P4 and does not choose the region.

### 7.6. Memory and session stores

C3 and C4 are internal parts of the core, not boundaries (D-114). An implementation may move them to another store if records keep the tenant in the space, real deletion on erasure (MEM-4, RET-4) and the record model of `memory-item` and `fact`. Roles choose a memory scope, not a store, so manifests do not change.

### 7.7. A new task type

**Add.** `tasktypes/<type>.yaml`: `worker_kind`, `tier`, data class, instruction, `params_schema`, result schema, `purpose` (sets the model slot and budget line, BUD-7), priority class, deadlines, retries, events, `tree_budget_eur` (BUD-9), `max_children`, and if needed `summary_fields` and `proposable`. Result fields that code reads are marked `x-botaru-read-by: code` and are of closed kinds (OUT-1). Add the type to a role's `claims`, `task:<type>` to a group, a command if needed, and 5 eval cases.

**Example** (D-72). `examples/tasktypes/data.describe.v1.yaml`: an agent type, tier 1, class `internal`, the file from the task's attachment. The plugin `data` computes rows, depth, temperature and salinity ranges and the missing share as closed result fields, and the model writes the `report` only from them. The role `data-reader` claims it; `/describe` with an attachment enqueues it.

A pipeline that a person starts with `/run` without a button is allowed only if it reads the catalog, changes nothing on working machines and writes only to the farm's file store (D-49).

**Checked.** Closed `params_schema` (INJ-1); `alarm.*` events only from `job` types (INJ-7); no `action.*` name (APR-5).

**Never changes.** Within `.v1` only optional fields are added; a breaking change is `.v2` next to it (MAN-3).

### 7.8. A new sink

A worker of kind `sink` over P1 (`spec/ports/e2-sink.md`): the write is idempotent by `delivery.id`, the address is built from the manifest base, the tenant and the id, never from the model's answer (DLV-6); `match` and `erase` serve erasure (RET-4). A `Sink` manifest sets a class ceiling and a retention period; an export sink also sets `export_only`. An outgoing webhook (v0.2) is a sink. The core gives a sink only deliveries naming it (TEN-3), and the `core-blackbox` delivery checks cover it.

### 7.9. A new tenant

**Add.** `tenants/<t>/`: `tenant.yaml` (kind, limits (CAP-1), time zone (TIM-2), privacy block, dispatcher rules), groups with `system-ops` (PRM-10), a catalog of the tenant's own machines, never a farm node (DEP-11), routes, schedules, channels, sinks and the charter. Give it its own workers, runtime, channel process, provider workspace and key (D-42) and E5 adapter with its own monitoring user (MON-4). The operator applies it, adds the `system` principal and invites the first admin, whose membership stays `pending` until the code check (PRM-13). There is no way to add a tenant around git (MAN-5).

**Checked.** The key only in P4 (GW-3); no "approves itself" in production; the farm manager's profile allowed and a `manager` line above 0 (MGR-20); `doctor` checks the `monitor` worker (MON-11); the isolation fixtures pass. Data of one tenant never reaches another.

### 7.10. An external program enqueues tasks

A `service` principal with identity `api:<key_id>` and groups (D-47); P2 issues the key and stores its hash (IDN-4). The program calls P1 `enqueue` and `task.get`, and permissions are computed like a person's (PRM-9). A program cannot decide on cards.

### 7.11-7.20. Parts that come after the core and the first bot

These recipes describe parts that v0.1 adds after the core and the first bot (D-71).

| Recipe | What to add | What the core checks | Rules |
|---|---|---|---|
| 7.11 Message type | a global `MessageType` `<genus>.<name>.vN` with a genus from `spec/model/message-kinds.yaml`, a closed field schema, class ceiling, delivery, producers, rate | closed leaves; a `state` type with an `agent` producer fails `apply`; a message grants nothing and enqueues nothing | MSG-1, MSG-2, MSG-5, MSG-7, STA-1 |
| 7.12 Bot group and dispatch rule | a `BotGroup` selector by roles and worker labels; a rule type -> group, priority, `max_running` | membership from recorded labels; a group grants nothing; an empty group is legal and its tasks wait; no task names a worker | BGR-1, BGR-2, DSP-4, DSP-8 |
| 7.13 Analysis for a role | `insight`, `models.analysis`, an `insight` type, `ObservationSeries` in git, money on the `insight` line | at most once per merge window, tier at most 1; output only proposals; only code writes series | INS-4, OBV-1, PRP-3 |
| 7.14 Commission lens | a lens row in `Commission`: a `reviewer` role, a `review` type, weight and threshold, a template, an injection eval | the reviewer is independent and does not see the author; without veto lenses the outcome is `parked` | COM-2, COM-4 |
| 7.15 Model lineup | `ModelProfile` manifests and the tenant's allowed list; roles refer to slots | rank within the ceiling; declared fallbacks only; P4 picks by slot | GW-5, GW-7, GOV-3 |
| 7.16 Campaign | the `campaign` section of an `Action` and `exclusive: resource`; a command that picks the coordinator type | a fixed plan and hash; width and budget limits; the initiator's permission per item; one approval covers exactly the snapshot | ORC-1, ORC-5, ORC-6, ORC-12, APR-7 |
| 7.17 Manager | a `manager` role with `may_enqueue`, `campaigns`, and memory `none` if it reads untrusted outcomes; code (`job`) for anything that touches actions or alarms | children and campaigns pass all initiators' permissions; tree limits | ORC-6, PRM-6, MSG-7 |
| 7.18 Aggregator | an `aggregator` role with an `aggregate` type and line | reads only rollups and closed fields inside one tenant | AGG-2, AGG-4 |
| 7.19 Incident class | an `IncidentClass` file in the farm git with review | only files set classes; only the core opens incidents; only code produces alarms | MGR-7, MGR-8, INJ-7 |
| 7.20 Persona | `personas/<name>.md` pinned by hash in the charter ([bots.md](bots.md)) | the persona lint; a persona grants nothing | GOV-4, GOV-29 |

### 7.21-7.23. v0.2 additions

| Recipe | What to add | What holds |
|---|---|---|
| 7.21 Knowledge corpus | a corpus in the catalog, the `kb` plugin in the role, an injection eval | knowledge only through P3, one tenant per index, uploaded content is untrusted (KNW-1, INJ-13) |
| 7.22 External event source | an input-only channel driver with an authenticity check, `hook:<source>`, a mapping into `ext.*` | free text goes to a file with `untrusted_seen`; an outside alarm never becomes `alarm.*` (ING-1, INJ-7). Monitoring problems go through E5 instead (7.32) |
| 7.23 Script in git | a `botaru.script/v1` file with review | the same acceptance as a translator draft; a run only by the initiator's button; actions keep their own approvals (SCR-6, SCR-7) |

### 7.24-7.30. Operations and providers

- **7.24 Content store.** Any adapter that keeps content declares a retention class and implements erasure by `(tenant)`, `(tenant, subject)` and ids; without it, an erasure request stays open with an alarm (RET-4, RET-7).
- **7.25 Alarm kind.** A `job` type that emits `alarm.<name>.vN` with a `severity`. The farm has no recipients: whom to wake is configured in the monitoring system by tags (MON-1, INJ-7).
- **7.26 Auto-stop rule, 7.28 role profile, 7.29 manifest kind affecting permissions.** Only a skeleton ADR (HLD-2, PRF-1, MAN-4).
- **7.27 Telemetry backend.** Only the deployment, as long as the attribute allowlist holds (OBS-1).
- **7.30 Model provider.** A binding sheet `spec/bindings/model-providers/<provider>.md` per its README, after the reference `anthropic.md`: the request field allowlist (GW-1), cache control (GW-16), region (GW-14), mapping of errors to `provider_*` reasons, reading workspace ceilings (GW-9). `ModelProfile`s with prices, retention, rank, `max_output_tokens`, `tokens_per_s` and subprocessors (GW-15). The provider in `providers_allowed` and the privacy block (SEC-8). The `model-gateway` suite with a mock provider checks it.

### 7.31. Another monitoring system

**Add.** An E5 adapter of kind `monitor`, one process per tenant plus one in `farm`, each with its own monitoring user; a binding sheet `spec/bindings/monitor/<system>.md` after the reference in the E5 sheet (alarm items, the signed audit head, polling of P2 `health`, problems, scripts, notes, erasure); the `deploy/monitor/` files.

**Checked.** The `monitor` suite with a mock system; `doctor` checks each adapter user's permissions (MON-4); a stalled export raises `monitor_export_stalled` (MON-11). Adapter users never acknowledge, close or silence problems or run scripts on farm nodes (D-60, D-62, D-69).

### 7.32. A new monitoring problem class

A row in `spec/model/monitor-map.yaml` (class and alarm), and if needed an incident class (7.19). A problem gets its class from the catalog row `monitor_triggers: [{triggerid, class, item}]`, never from a trigger tag; an unknown trigger gives only a `warn` alarm (MON-5). A route task for a catalog trigger does not get `untrusted_seen`; for anything else it does (INJ-13, D-82).

### 7.33. A managed service and a monitoring script

Bots with a role permission start, stop and restart catalog services and run monitoring scripts on catalog nodes (D-67, D-69); both tools are built in.

**Add.** In `Catalog.spec.hosts[].services[]`: `name`, `control` (from `start`, `stop`, `restart`), and `via: host` with the exact `unit` or `via: monitor` with a script per verb. Scripts in `hosts[].monitor_scripts_allowed`, whose `name` is the only name the model sees. The role gets `service_control: [nginx]` and `monitor_scripts: [...]`; a person with `service.control:nginx` gets `/service restart nginx` (D-68). For `via: host` the machine needs an action toolhost, and `apply` generates the access rule (DEP-4). Eval cases: a refusal naming the missing permission, an injection in a trigger name, a card after untrusted text.

**Checked.** At `apply`: complete `control` and `via`, no farm-node service in a tenant catalog (MON-8). On a call: closed service and verb, permissions, guards (`service_rate_limited`) and holds. Only `restart` from `no_card` runs without a card (APR-18, D-74), and a task that saw untrusted text always gets a card (INJ-15). Code, not the model, picks the machine, the service and the script.

### When an ADR is needed

See "Decisions (ADR)" in [architecture.md](architecture.md). In short: a change to a boundary contract, a closed list that permissions depend on, a new auto-stop rule, message genus, dispatcher condition, fact kind, role profile, or a terminal with free input on a production machine. If no recipe fits and the core must change, a port is missing.

## Why it works this way

- **Extension without changes to the core.** One thin core; whatever changes often lives in manifests and adapters (D-02, D-03).
- **One check point.** Whatever is added, the core checks it at `apply`, at `hello` and at every permission check; there is no path "for insiders".
- **Closed lists instead of trust.** Tool parameters, message genera, role profiles, runtime deviations and control operations are closed lists; adding an element is allowed, but in a form the core can check.
- **A port instead of a workaround.** An extension that needs the core changed shows a missing concept, which is added openly through an ADR.

## Example on `demo`

The scientist `bob` wants CTD files described. The installation adds the plugin `data` (7.2: a read-only tool, `placement: host`, no secrets, so it runs as a child process of the toolhost), the type `data.describe.v1` (7.7), the role `data-reader` (7.1) and the command `/describe` in `examples/tenants/demo/tenant.yaml`. `check_manifests.py` passes, and `--tools-list` confirms the server exposes only `data/describe`. After `farmctl apply` and enrolling the worker and the toolhost, `bob` sends a file with `/describe`; code picks the type, the plugin computes the numbers, and the model writes a short report from the closed fields only (OUT-1). Nothing in the core changed.

