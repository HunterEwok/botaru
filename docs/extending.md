# Extending the farm

## In short

The farm grows without changes to the core: roles are extended by manifests, and new channels, runtimes and stores are connected as port implementations (D-02). This document collects the extension recipes. Each recipe says which files to add, what the core checks, which rules apply and what never changes. If an extension requires changing the core, the skeleton lacks a port or a concept: that is a reason for an ADR, not for a workaround. All examples are given for the tenant `demo`.

## The general rule

Something new is added in one of three ways.

1. **A new manifest of a known kind** (a role, a plugin, a task type, an action, a message type, a bot group, a tenant, a route, a schedule and others). Neither the core nor the skeleton files change.
2. **A new implementation of an existing port**, if needed as a driver within a kind. The adapter brings the schema of its `spec.config`, and it passes the driver meta-schema; `farmctl` reads it from `spec/manifests/drivers/` and from the schema folders named in the deployment. Fields that rules depend on (link previews, markup, accepting messages "on behalf of the chat", egress, placement, ceilings, fallback providers) are forbidden in a driver config: they exist only in the common wrapper under the core's check (MAN-4).
3. **A new manifest kind** (rarely). An implementation may register its own kind under its own `apiVersion` group if the kind grants no capabilities, declares no tools or actions and passes the permission fixtures `authorize/`. A kind that affects permissions is added only by a skeleton ADR (MAN-4).

A manifest in git always sits in one wrapper:

```yaml
apiVersion: botaru/v1alpha1
kind: Role            # Tenant | Group | Role | TaskType | ToolPlugin | Action | Catalog | Route | Schedule
                      # | Channel | Sink | MessageType | BotGroup | ObservationSeries | ModelProfile | Commission | ...
metadata:
  name: ops-reader
  tenant: demo        # only for kinds that belong to a tenant
  labels: {}
spec: { }             # schema spec/manifests/<kind>.schema.json
# status is not written in git: the core keeps it (commit and errors)
```

Paths in the recipes are given from the root of the installation repository; training samples of the same files are in the skeleton's `examples/` (for example `examples/roles/<name>/role.yaml`), and your own additions are kept in your own installation repository. The skeleton contains no farm code: until there is an implementation, instead of `farmctl apply` the shape of your files is checked by `python3 -I ci/check_manifests.py --manifests <folder>` (see [ci/README.md](../ci/README.md), section "Your own files outside the skeleton"), and the `farmctl` steps and starting workers are done only on an implementation's farm. Keep the binding note of a new channel or provider (from the template `spec/bindings/.../README.md`) in your own repository and attach it to an issue "binding: <channel>"; a pull request with it to the skeleton is possible after signing the Contributor License Agreement `CLA.md` (D-89), and this is the only step of the recipes that changes the skeleton. What goes where and what to run is collected in the table of [CONTRIBUTING.md](../CONTRIBUTING.md).

What the core checks for any extension: the kind's schema and `apiVersion` (MAN-1), no tenant data in global manifests (TEN-6), "additive only" within a version (MAN-3), atomicity of the set (MAN-6). What does not change with any extension: the tenant wall (TEN-1..7), the write rule (only the core writes system fields, IDN-1), the non-waivable rules.

## v0.1 recipes

Recipes 7.11-7.14 and 7.16-7.20 (message type, bot group, analysis for a role, commission lens, campaign, manager, aggregator, incident class, persona) describe parts that in v0.1 come after the core and the first bot (D-71).

### 7.1. A new kind of bot

**What to add.**

1. Choose or describe task types (7.7).
2. `roles/<name>/role.yaml`. Required fields: `claims` (the task types the role takes), `models.base` (the base model slot), `worker_kind` (`agent` or `job`); `profile` (a role profile from `spec/model/role-profiles.yaml`) is `worker` by default. Optional: `messages.send` and `messages.receive` (message types and addresses), `instance_inbox`, `views`, `insight` and `models.analysis` (analysis, 7.13), `may_enqueue` (without `action.*`), `tools`, `actions_may_request`, `memory` (`none` by default), `runtime` (the runtime driver), `skills` (skills from the repository with a hash).
3. In the tenant's groups, grant `task:<type>` to those who may use it.
3a. State how a task reaches the role: a command in `Tenant.spec.commands` (like `/hello` in `examples/tenants/demo/tenant.yaml`), the type in `default_types`, a `Route` or a `Schedule`. Code chooses the task type, not text; without this step the role gets no work.
4. 30-50 eval cases for the new kind: without cases the bot is not enabled. This is an admission requirement of the farm ([docs/quality.md](quality.md), "Admission of new parts"); the skeleton CI does not check the number of cases. A case goes into `<folder>/evals/cases/<role>/` of your own repository, your own people, chats and test bench role into `<folder>/evals/seed.yaml`, and it is checked with `python3 -I ci/check_evals.py --cases <folder>/evals/cases --seed <folder>/evals/seed.yaml --manifests <folder>/manifests` (your own seed extends the skeleton's test bench data). The training hello-bot comes without cases: 30-50 cases are the admission threshold on a production farm, not a condition for a training sample.
5. `farmctl apply`, `farmctl worker enroll --tenant demo --role <name>`, start the worker container. The `--bot` flag binds the worker to an existing identity.

A ready-made sample is in `examples/hello-bot/`: a role, a task type with only the needed fields (the optional ones are listed in it as a comment), a group, a sequence in the fixture envelope (P1 `submit_activity`, the deliveries `ai_notice` and `privacy_notice` on first contact, `claim` -> `heartbeat` -> `complete` and delivery of the answer to the initiator's chat by `origin`), without messages between bots and without the `messaging` profile (D-71), the expected events and audit, and a mock model scenario behind P4.

**What the core checks.** The role fits its profile: a reporter has no plugins and does not enqueue tasks, a reviewer has no actions and no messages (PRF-1). `may_enqueue` has no `action.*` (APR-5). `person` memory only for a role whose types are all of class `summary` and that has no tools with untrusted output (MEM-2). The rank of the role's model profiles is not above `max_rank` of its profile (GOV-3). Message addresses pass the matrix; reviewers and the farm manager cannot be addressed (MSG-5).

**Rules.** PRF-1, APR-5, MEM-2, GOV-3, MSG-5, DSP-1, TEN-6.

**What never changes.** The core, the protocols, other roles. `claim` selects work by (tenant, the role's `claims`, labels), and C2 computes permissions. A deterministic bot (`job`) is made the same way, without a runtime.

### 7.2. A new tool: "program + MCP"

A new kind of bot is a program with an MCP server that passed review (D-39): a plugin, a role and a task type. The real number of bot kinds equals the number of programs that passed review and lint.

**Plugin author.**

1. The program's MCP server: a ready-made one or a small wrapper. The image is pinned by digest (DEP-6).
2. `tools/<plugin>/tool.yaml`: the MCP revision, `placement`, `network.egress` (addresses only from the tenant catalog), secret names; for each tool `access`, `tier`, `output_class`, `untrusted_output`, parameters of closed kinds only, an output limit, redaction, its own description. Server tools that are not in the manifest are not visible to the model. The global manifest has no OS user: it is set per tenant in the deployment description (DEP-7, DEP-10), and the global manifest carries no tenant data (TEN-6).
3. Everything that writes moves into a separate tier 3 write plugin and an `Action` manifest with an executor: the narrow OS mechanism `oneshot`, not a shell and not sudo (DEP-4). Starting, stopping and restarting a service is not a new action: the service is declared in the tenant catalog, and the OS mechanism `unit` executes it (recipe 7.33, MON-8).
4. Eval cases with an injection in the output of every tool that reads external text.
5. A check from your own folder: the manifest shape with `python3 -I ci/check_manifests.py --manifests <folder>`; dump the `tools/list` of your server to a file and compare it with the manifest: `python3 -I ci/check_manifests.py --manifests <folder> --tools-list tools.json` (INJ-2, INJ-3).

**Installation operator.**

1. Placement. An own plugin without secrets and without network runs as a child process of the toolhost (DEP-7). A plugin with a secret, with network or from a third party runs in its own container, described in advance in the deployment, with its own OS user and its own network (DEP-10). A write plugin lives in a separate toolhost, and the OS mechanism allows the action only to that toolhost's user (DEP-9).
2. Resources into the tenant catalog, the tool into the role's `tools`, `task:<type>` into a group.
3. `farmctl worker enroll` for the toolhost fixes the placement, the plugins and the machine (DEP-2).

**What the core checks.** At `apply`: closed parameters (INJ-1), MCP annotations do not contradict the manifest (INJ-3), no parameters with `x-mcp-header` (INJ-5), egress only from the catalog (INJ-6), the rules of the program-running plugin (INJ-14); the deployment lint checks the user, the network and the secrets of every process (DEP-10). At `hello`: the live `tools/list` matches the manifest, otherwise the toolhost is `blocked` (INJ-2).

**Rules.** INJ-1..6, INJ-14, DEP-1..4, DEP-6, DEP-7, DEP-9, DEP-10, TEN-3a, TEN-6, APR-1, MON-8.

**What never changes.** The model has no write tools of plugins (APR-1), descriptions and schemas are taken from the manifest, and the toolhost has no inbound port.

### 7.3. "A robot with a terminal"

This is not a new concept but variants of one plugin.

| Variant | Tier | Where | Decision |
|---|---|---|---|
| The `cmd` plugin "on rails": named commands, `argv` as an array, parameters from lists, run without a shell | 1 | the monitored machine, `placement: host` | allowed (INJ-14) |
| Shell and Python in a throwaway container without network and secrets on a copy of the data: a sandbox toolhost with the tools `run`, `dataset_get`, `list_out` | 2 | `placement: sandbox`, only a separate VM; the sandbox toolhost equals root on this VM | allowed (DEP-1, DEP-3) |
| A terminal MCP server (`send_keys(text)` and the like) or a free shell on a production machine | | | fails `apply` with any `pattern` (INJ-1); this can be changed only by an ADR and a new rule |

**What the core checks.** Closed parameters on a production machine (INJ-1), the list of forbidden programs and the shape of `argv` (INJ-14), the sandbox placement (DEP-1).

**Rules.** INJ-1, INJ-14, DEP-1, DEP-3, AUD-2.

**What never changes.** In any variant the model calls the terminal through P3. The model key and the farm token never get into the sandbox. The arguments of `run(argv)` are stored in the audit and events as a hash and a length (AUD-2). There is no new worker kind.

### 7.4. A new channel

**What to add.**

1. An adapter of port E1 as a worker of kind `channel`: input (authenticity of updates, the sender, `Activity`, attachments through `file.put`), output (`claim` of deliveries, rendering by capabilities, the receipt).
2. The driver declares `identity_assurance` (`strong` or `weak`), `membership_assurance` (`enforced` or `none`), the identity form (for example `email:<address>`), the capabilities `max_text`, `buttons`, `files`, `edit`, and the way to tell a person they are talking to a bot (a built-in channel mark or a message on first contact); the privacy notice `privacy_notice` with `notice_version` goes separately and with either way. Without buttons, cards are not sent to the channel. The channel's credentials are separate: a worker token for P1 and the P2 secret of the driver, one per driver in a tenant (issued by `worker.enroll` of the channel process, the field `p2_token`); the person and the chat of a P2 call are taken from the headers `Botaru-On-Behalf` and `Botaru-Chat`, and a personal chat needs no binding (D-87 (3)). The values live in the deployment, not in C1. In v0.1 the channel closes erasure work with a "stores nothing" record, based on the declaration `driver.stores_content: none` in P1 `hello`, so that the erasure request does not hang (D-86 (8)); since v0.2 the channel implements `match` and `erase` over outgoing messages and the cache (RET-4).
3. A binding note from the template `spec/bindings/channels/README.md`: how the channel's fields map onto `Activity` and `Delivery`, and how authenticity and chat membership are checked. Keep it in your own repository next to the driver schema and attach it to an issue "binding: <channel>", or send a pull request to the skeleton after signing the Contributor License Agreement `CLA.md` (D-89).
4. `tenants/demo/channels.yaml` (`Channel` objects separated by `---`), a worker token per tenant, a separate process.

**What the core checks.** The channel check suite: IDN-3, IDN-7, APR-3, IDM-5, DLV-5..7, INJ-11; input from a non-principal in a chat with `membership_assurance: none` gives no `Activity` and no `file.put`. The driver meta-schema does not let through the fields for previews, markup and accepting "on behalf of the chat" (MAN-4). A group chat binding with a ceiling above `summary` is rejected unless the driver is `enforced` (DLV-7).

**Rules.** IDN-3, IDN-7, APR-3, IDM-5, DLV-5..7, DLV-11, INJ-11, TEN-4, MAN-4.

**What never changes.** The core, P1, C2, other channels. A channel with weak sender verification (for example email without a domain signature check) gets `identity_assurance: weak` and does not execute privileged commands (IDN-3). A channel does not execute operator operations on anyone's behalf.

### 7.5. A new agent runtime

**What to add.**

1. An adapter of port E4 (`describe`, `run`, `cancel`) inside the agent worker image; the runtime in its own container without a provider key and a worker token (DEP-5).
2. The runtime's only MCP server is P3, and the model only through P4. Built-in tools, server-side tools of the provider, fallback providers and background self-learning are off (INJ-9).
3. `describe()` chooses values only from the closed list and turns on the replacement rules RT-1..4.
4. `runtime.driver: <name>` in the role, `apply`, restart of the workers.

**What the core checks.** The runtime check suite with a mock model behind P4, then a full eval next to the current runtime. The adapter's expected failures may contain only rules with `waivable: true`. The model gateway independently rejects a request with tools outside the P3 catalog (GW-1).

**Rules.** INJ-9, RT-1..4, FEN-4, MEM-4, DEP-5, GW-1.

**What never changes.** The runtime does not see the worker token or the provider key, does not reach the model bypassing P4 and does not set the call's region.

### 7.6. A new memory or session store

**What to add.**

1. A C4 adapter (since v0.1) or a C3 adapter (since v0.2) with the same record model: the space always includes the tenant, and deletion on an erasure request is mandatory (C3 `delete` and `purge`, C4 `erase`).
2. Registration by name in the core's deployment configuration. Roles choose a memory scope, not a store, so the manifests do not change.
3. Migration of records in the `memory-item` format (JSON Lines per the schema).

**What the core checks.** The check suite: tenant isolation, the class filter, real deletion, the flow "proposal -> decision of the person whose memory it is" for the scope `person` (`decide` with the subject `memory_record`), refusal on `untrusted_seen`, facts of a closed schema.

**Rules.** TEN-5, MEM-1..4, RET-1, RET-4, RET-7.

**What never changes.** The core computes the space, not the model (MEM-1); a bot's memory is only core records and verified facts (D-35).

### 7.7. A new task type

**What to add.**

1. `tasktypes/<type>.yaml`: `worker_kind`, `tier`, the data class, the instruction, `params_schema`, the result schema, the purpose `purpose` (sets the model slot and the budget line), the priority class, `requires`, deadlines, retries, events, the money limit for the whole task tree `tree_budget_eur` (BUD-9), `max_children`; if needed `inbox.read`, `summary_fields` for the auto-report, the flag `proposable`.
2. Result fields that code reads (script conditions, the campaign summary, the auto-report) are marked `x-botaru-read-by: code` and are of closed kinds only (OUT-1).
3. The type into a role's `claims`, `task:<type>` into a group, and a channel command if needed.
4. 5 eval cases.

**v0.1 example: CTD data description** (D-72). The type `data.describe.v1` in `examples/tasktypes/data.describe.v1.yaml`: `worker_kind: agent`, `tier: 1`, class `internal`, no parameters, the file is taken from the task's attachment; the result fields `verdict`, `rows`, `depth_min_m`, `depth_max_m`, `temp_min_c`, `temp_max_c`, `salinity_min`, `salinity_max`, `missing_pct` are marked `x-botaru-read-by: code`, and the model writes the `report` text. The role `data-reader` (`examples/roles/data-reader/role.yaml`: `profile: worker`, `claims: [data.describe.v1]`, `tools: [data/describe]`, `memory: none`). The plugin `data` (`examples/tools/data/tool.yaml`) provides one tool `data/describe` (`access: read`, `tier: 1`, `untrusted_output`, `redact`, `target: any`, the file by `enum_from: task.attachments`), which reads the file by the attachment id and computes the numbers with code; the model writes a short report only from the result fields (OUT-1), and only the redacted file header gets into the context. The channel command `/describe` with an attachment enqueues a task of this type; an example is in `docs/task-language.md`, eval `data/describe-numbers-match`.

A pipeline that a person with the permission starts with the command `/run` without a button is allowed only if it reads the catalog, changes nothing on working machines and writes only to the farm's file store (D-49); a pipeline that writes on a working machine keeps the approval button.

**What the core checks.** The schemas are valid; `params_schema` of closed kinds only (INJ-1); `alarm.*` in events only for `job` and without input from agent tasks (INJ-7); the name is not in the namespace `action.*` (APR-5); the purpose sets the slot, the budget line and the class (BUD-7).

**Rules.** INJ-1, INJ-7, APR-5, MAN-3, OUT-1, PRM-4, PRM-5, BUD-9.

**What never changes.** Within `.v1` only optional fields are added; a breaking change is `.v2` next to `.v1` (MAN-3).

### 7.8. A new sink

**What to add.** An E2 adapter of kind `sink` (the reference one to disk since v0.1, replaceable since v0.2): the write is idempotent by `delivery.id`, the address is built from the base in the manifest, the tenant and the id; the operations `match` (search for a principal's rows, the answer is ids only) and `erase` by scope and by a list of ids since v0.1. A `Sink` manifest with a class ceiling and a retention period; a sink for operator exports also with `export_only: {recipient, key_ref}` (a single recipient, encryption). An outgoing webhook (v0.2) is a kind of sink; there is no separate concept for it.

**What the core checks.** The sink check suite; the delivery class is not above the manifest's ceiling; a sink takes only deliveries with its own name in `target` (TEN-3); the `erasure` suite: `match` finds a principal's canary, and `erase` by these ids removes it (RET-4, RET-7, v0.1).

**Rules.** DLV-3, DLV-6, RET-4, RET-7, TEN-3, TEN-4.

**What never changes.** The address is never taken from the model's answer or the task body (DLV-6).

### 7.9. A new tenant

**What to add.**

1. `tenants/<t>/`: `tenant.yaml` (the kind: test or production; the limits `spec.limits`: tasks in the queue, enqueues per hour, store bytes (CAP-1); the time zone `spec.timezone` (TIM-2); `self_approve`; the privacy block with `purposes[].lawful_basis` and `recipients[]` (including `monitoring`); `operator_access.debug_journal`; dispatcher rules; since v0.2 retention periods by class `spec.retention` (RET-2)), `groups.yaml` (including `system-ops`, PRM-10), `catalog.yaml` (machines of the tenant only: `web1`, `db1`, `ctd1`, never the farm node `core1`, DEP-11; machines have `monitor_host`, `monitor_scripts_allowed` and `monitor_triggers`), `routes.yaml`, `schedules.yaml`, `channels/` (a chat binding with `credentials_ref`), `sinks/`, the commission manifest, the charter `governance.yaml`.
2. Its own bot and channel process, its own workers and toolhosts, its own runtime instance, its own workspace and key at the model provider with its own spending and rate ceilings (D-42); its own E5 adapter process with its own monitoring system user that has permissions only on this tenant's node groups (`deploy/monitor/<t>/`), the template items and the webhook for it (MON-4).
3. Operator: `farmctl apply`, the service principal `system` (`farmctl principal add --tenant <t> --system`), the first admin by invitation `farmctl invite --tenant <t> --group admin` with a check of the verification code (the tenant's first admin needs no `--requested-by`, PRM-13); until the check the membership is `pending`, because the group `admin` is privileged (PRM-13). All these operations belong to the operator (PRM-14). A tenant exists only as a declared object: there is no command for adding a tenant that bypasses git (MAN-5).
4. MGR-20 applies. The `apply` lint: the `models_allowed` of every active tenant contains the seat's model profile `farm_manager.model_profile` (slot `seat`, GW-7), and the `manager` line is greater than 0. If the tenant's `privacy.purposes` contains the purpose `incident_review` and no worker of a role with the profile `farm-manager-reader` is enrolled in the tenant, `farmctl doctor` prints a warning, and `inspect` tasks in this tenant wait for a worker; without this purpose the reader role is not needed.

**What the core checks.** The provider key only in P4, one per tenant, workspace ceilings in place (GW-3); for a production tenant the lint forbids "approves itself"; `system` has exactly the types that its schedules and routes enqueue (PRM-10); `apply` rejects a catalog where `monitor_host` or `machine` matches a farm node (DEP-11); `doctor` per tenant: the `monitor` worker is `active`, there are no E5 deliveries in `planned` older than 15 minutes, the adapter user sees only the groups of its tenant (MON-4, MON-11). The isolation fixtures (TEN-1..7) pass against the deployment.

**Rules.** TEN-1..7, GW-3, PRM-10, PRM-13, PRM-14, CAP-1, TIM-2, MGR-20, MAN-5, DEP-11, MON-4.

**What never changes.** There is no main tenant; data of one tenant never reaches another, neither through memory, nor through the model context, nor through the farm manager.

### 7.10. An external program enqueues tasks

**What to add.** A principal of kind `service` with the identity `api:<key_id>` and groups, like people (D-47). P2 issues the key, and it is stored as a hash (IDN-4).

**What the core checks.** The program sends a task body to P1 `enqueue` with its key and reads its own tasks through `task.get`; permissions are computed by the same check as for people (PRM-9). An adapter of an external agent protocol, if one appears, sits in front of `enqueue` and compares the tenant field with the credentials (TEN-2).

**Rules.** TEN-2, IDN-2, IDN-4, PRM-9, IDM-1.

**What never changes.** There is no separate permission model for programs (D-40); a program cannot decide on cards.

### 7.11. A new message type

**What to add.** A `MessageType` manifest (global, like a task type) named `<genus>.<name>.vN`: the genus from the closed list `spec/model/message-kinds.yaml` (`report` or `state`), a closed field schema, a class ceiling, the delivery (`one` or `all`), a period, a size, whether a note is allowed and of what length, who may send (`producers`), a rate. Then the type into the `messages.send` of the sending roles and into the `inbox.read` of the types that read it.

**What the core checks.** The leaves of the field schema are of closed kinds only; a `state` type with `agent` among the producers fails `apply` (MSG-2, STA-1); addresses pass the role matrix (MSG-5).

**Rules.** MSG-1, MSG-2, MSG-5, MSG-6, MSG-7, STA-1, TEN-6.

**What never changes.** A message grants no permissions, enqueues no tasks and creates no cards (MSG-7); only an ADR introduces a new message genus.

### 7.12. A new bot group and dispatch rule

**What to add.** A `BotGroup` manifest of the tenant: a selector by roles and worker labels (the core computes the membership from the `enroll` records). A dispatcher rule in `tenant.yaml`: task type -> bot group, priority class, `max_running`.

**What the core checks.** The group membership is computed from the recorded labels, and `hello` can only narrow them (BGR-1); a group grants no permissions (BGR-2); an empty group is legal, and tasks for it wait (DSP-8).

**Rules.** BGR-1, BGR-2, DSP-1, DSP-2, DSP-5, DSP-8.

**What never changes.** A task cannot be addressed to a specific worker (DSP-4); only an ADR introduces a new dispatcher filter condition.

### 7.13. Turn on analysis for a role

**What to add.** In the role, the section `insight` (which observation series the role sees) and the slot `models.analysis`; a task type with the purpose `insight`; observation series `ObservationSeries` in git; money on the tenant's `insight` line (by default the lines other than `work` are 0, and analysis is turned on explicitly with money). If a role has money for analysis, the money for reviews must cover its proposal ceiling.

**What the core checks.** Analysis runs at most once per role per merge window (INS-4), with a tier not above 1 and permissions no wider than the main work; the output is only proposals, which the core creates at `complete`; only code writes series (OBV-1).

**Rules.** INS-1..4, OBV-1, OBV-2, BUD-7, PRP-1..3.

**What never changes.** A proposal executes nothing; only a human's button creates an object of the ordinary path (PRP-4).

### 7.14. A new commission lens

**What to add.** In the tenant's `Commission` manifest, a lens row: a role of profile `reviewer` and a task type with the purpose `review`, a weight and a threshold in the verdict policy; a review template in git; an eval with an injection in a proposal. Lenses are bound to the charter's seats (GOV-33).

**What the core checks.** Reviewer independence: another role, not the author's worker, another model profile; the reviewer does not see the author's note, the author's identity or other ratings (COM-2). Without the ratings of the veto lenses the outcome is `parked`, not a pass (COM-4).

**Rules.** COM-2..6, PRF-1.

**What never changes.** Code computes the verdict; a lens does not see who the author is, and no wording of the template links a rating with promotion or departure.

### 7.15. Change the model lineup

**What to add.** Only `ModelProfile` manifests (the exact model, fallbacks, family, rank, limits) and the list of profiles allowed to the tenant. Roles refer to slots, not to models. The names of specific models live only in `examples/model-profiles/` (D-05).

**What the core checks.** The profile's rank is not above `max_rank` of the role profile (GOV-3); a fallback model only if declared (GW-5); P4 chooses the model by the slot and the tenant's list (GW-7).

**Rules.** GW-5, GW-7, GOV-3, BUD-7.

**What never changes.** Roles, task types and permissions; a model change gives no new tools.

### 7.16. A new mass action (campaign)

**What to add.** In the `Action` manifest, the section `campaign`: `allowed`, `max_items`, `max_in_flight`, `canary_min`, `after_canary`, `max_failures_max`; a separate field `exclusive: resource` (at most one issued write call per resource, counting campaign items and single approvals, ORC-12). Target selectors in the tenant catalog, if targets are chosen as a group. A channel command (`/campaign ...`) that chooses the coordinator type by code (IDN-6). Fixtures on a fake toolhost.

**What the core checks.** Only a live attempt proposes a campaign; the plan, the items and the hash are fixed (ORC-1); width and budget limits (ORC-5, ORC-8); the initiator's permissions for every item (ORC-6); one approval covers exactly the snapshot, the hash is checked for every item, and an item is executed at most once (APR-4, APR-7).

**Rules.** ORC-1, ORC-5, ORC-6, ORC-8, ORC-14, APR-4, APR-7.

**What never changes.** Without a human's button no campaign action is executed; the list of items can only shrink.

### 7.17. A new manager

**What to add.**

1. Decide who the manager is: code (`job`) or a model (`agent`). Everything that touches actions or alarms, or repeats on a schedule, is done by code.
2. `roles/<name>/role.yaml` of profile `manager`: `may_enqueue` (executor types), `actions_may_request` (if campaigns have actions), `campaigns: {propose, then}`, for an agent `memory: none` if the role reads item outcomes with untrusted text (MEM-2).
3. Task types of the manager and of the continuation (7.7); the executor types have their summary fields filled in.
4. Grant initiators `task:` on the manager types. The initiator must have the permissions on the executor types separately: the manager role does not give them (ORC-6).
5. Eval: 30-50 cases per role, among them cases with an injection in the executors' reports.
6. `farmctl apply`, `worker enroll`.

**What the core checks.** The children and campaigns of the manager pass the permissions of all initiators (ORC-6, MSG-7); tree limits by root (PRM-6).

**Rules.** ORC-1, ORC-6, PRM-6, MSG-7, PRF-1.

**What never changes.** The core, the protocols and other roles; the manager does not get the executors' permissions.

### 7.18. A new aggregator

**What to add.** A role of profile `aggregator` with a type of purpose `aggregate`; the series and reports it reads; the budget line `aggregate`.

**What the core checks.** The aggregator reads only series rollups, closed fields of reports and proposals, and decision statistics; notes, logs, files and memory are not available to it; marks do not drop; it works inside one tenant (AGG-2, AGG-4, TEN-5).

**Rules.** PRF-1, AGG-2, AGG-4, OBV-4.

**What never changes.** The aggregator only proposes, like a functionary; it does not see data of another tenant.

### 7.19. A new incident class

**What to add.** An `IncidentClass` file in the farm git with human review: a code alarm (including one raised for a monitoring system problem after a recheck by code, MON-5), a condition, involvement from a closed list of functions, the first measure by code, the allowed answers of the farm manager, when to call people, `change_sensitive`; the field `stops_manager` only for a class whose alarm is built from series with `dataorigin: code`.

**What the core checks.** Classes are set only by files in git (MGR-7); only the core opens an incident, from an alarm and a class (MGR-8); for a class with `stops_manager` the lint checks the sources of the series (GOV-28); only the core and `job` produce alarms (INJ-7).

**Rules.** MGR-7, MGR-8, GOV-28, INJ-7.

**What never changes.** The incident catalog is not proposed by bots and not changed by the farm manager (MGR-16).

### 7.20. A new persona

**What to add.** A persona file `personas/<name>.md` (at most 4 KiB, with a paragraph about the only legitimate path to a stronger model, D-31), a reference to it by hash in the profile's trusted instruction; an eval.

**What the core checks.** The file hash; a persona grants nothing (GOV-4); the unawareness lint: neither the persona, nor the instructions, nor tool descriptions link ratings with promotion and departure.

**Rules.** GOV-4, GOV-29, INJ-10.

**What never changes.** A persona grants no permissions and is not changed by the model.

## v0.2 recipes

### 7.21. A new knowledge corpus

**What to add.** A corpus entry in the tenant catalog (curated from git and trusted, or uploaded and untrusted), the `kb` plugin in the role's `tools`, a data class, an eval with an injection in a document.

**What the core checks.** Knowledge enters the context only through P3; the corpus index belongs to one tenant (TEN-5); the content of an uploaded corpus is untrusted.

**Rules.** KNW-1, TEN-5, INJ-13.

**What never changes.** A corpus never becomes an instruction.

### 7.22. A new source of external events

**What to add.** An input-only channel driver with an authenticity check; the identity form `hook:<source>`; the mapping schema into `ext.*` events; a binding note `spec/bindings/channels/<source>.md`; the fixture "annotation with an instruction".

**What the core checks.** Free text of the event goes into a file with `untrusted_seen`; an external alarm does not become `alarm.*` (INJ-7); the core sets the key of the incoming event. Problems of the organization's monitoring system do not follow this recipe but go through the E5 adapter (recipe 7.31, MON-5).

**Rules.** ING-1..4, INJ-7.

**What never changes.** An external event grants no permissions and approves no actions.

### 7.23. A new script in git

**What to add.** A script file in the format `botaru.script/v1` in the tenant's git with human review: script steps from existing task types and actions, conditions on closed fields.

**What the core checks.** The same acceptance as for a translator's draft: the schema, references, the graph, the vocabulary, the initiator's permissions for every step, limits, the estimate; a run only by the initiator's button, and the approval of a run does not approve actions (SCR-6, SCR-7).

**Rules.** SCR-1..9.

**What never changes.** A script contains no loops and no nested scripts and executes no actions without their own approvals.

## Operations recipes

### 7.24. A new content store

**What to add.** Any adapter that stores content declares a retention class and implements its own erasure operation by the scopes `(tenant)`, `(tenant, subject)` and by a list of ids, with the outcome `done`, `failed` or `not_applicable` per erasure request.

**What the core checks.** Since v0.2 the `erasure` suite (RET-4); before v0.2 a store without its own erasure operation answers an erasure request with `failed`, and the request stays open with an alarm (RET-7).

**Rules.** RET-1, RET-4, RET-7, AUD-2.

**What never changes.** Content lives only in erasable stores with a period.

### 7.25. A new alarm kind

**What to add.** A `job` task type with the event `alarm.<name>.vN` per the common alarm schema and a `severity` (`info`, `warn`, `page`). The farm has no recipients and no alerting routes: an alarm episode goes to the monitoring system with the tags `tenant`, `alarm`, `subject`, `severity`, and whom to wake, how to repeat, escalate and when to stay silent, people configure there by these tags (MON-1).

**What the core checks.** An agent type produces no alarms, and model fields do not lead to alarms (INJ-7); only numbers, `enum` values and catalog ids go into the export.

**Rules.** INJ-7, OBS-6, MON-1.

**What never changes.** Code decides alarms, and only the monitoring system delivers them to people.

### 7.26. A new auto-stop rule

Only a skeleton ADR: a hold changes availability for everyone, so a new condition of an automatic hold is a change of the model (HLD-2).

### 7.27. A new telemetry backend

Only the implementation and the deployment. The skeleton does not change if the collector keeps the attribute allowlist without content (OBS-1).

### 7.28. A new role profile

Only a skeleton ADR. Profiles are the closed list `spec/model/role-profiles.yaml`: each has its own model rank ceiling and its own set of built-in tools (PRF-1). A new profile changes what a bot can be at all.

### 7.29. A new manifest kind

A kind from an implementation is registered under its own `apiVersion` group, grants no capabilities, declares no tools or actions and passes the `authorize/` fixtures. A kind that affects permissions is added only by a skeleton ADR (MAN-4).

### 7.30. A new model provider

**What to add.**

1. A binding sheet `spec/bindings/model-providers/<provider>.md` per the requirements of [spec/bindings/model-providers/README.md](../spec/bindings/model-providers/README.md); the sample is the v0.1 reference binding to the Anthropic Messages API `anthropic.md` (D-85). The sheet contains: the allowlist of request fields (GW-1), the cache control fields and the rule for where P4 puts the cache point (GW-16), the way to set the region (GW-14), the mapping of the provider's responses to the reasons `provider_unavailable`, `provider_rate_limited`, `provider_quota`, `provider_spend_cap` and `provider_config`, where the response carries `request_id` (a retry only without it), the way to read workspace ceilings (GW-9).
2. `ModelProfile` profiles of this provider: prices in euro per million tokens, the retention period at the provider, the rank, `max_output_tokens` and `tokens_per_s` (P4 computes the response deadline from them), `provider_training: false`, `provider_human_review`, `subprocessors[]` (BUD-9, GW-15).
3. The provider in `providers_allowed` and among the subprocessors of the tenant's privacy block (SEC-8); each tenant has its own workspace and key (GW-3).

**What the core checks.** The model gateway check suite with a mock provider: refusal of fields outside the allowlist, the number of calls equals the number of usage lines, tenant canaries, refusal reasons per the binding table; a daily probe of every pair (workspace, model profile).

**Rules.** GW-1, GW-3, GW-9, GW-14, GW-15, GW-16, BUD-9, SEC-8.

**What never changes.** Only P4 has the provider key, and the runtime does not reach the provider bypassing P4.

### 7.31. Another monitoring system

**What to add.** An implementation of port E5 for the organization's monitoring system as a worker of kind `monitor`: one process per tenant and a process in `farm`, each with its own monitoring system user. A binding sheet `spec/bindings/monitor/<system>.md` following the reference binding in the E5 sheet: how alarm values map onto items and triggers with tags and in `seq` order, where the signed audit head is stored and how `doctor` reads the length of its history, how the system polls P2 `health` and does not wake people within `drain_until_s`, how the problem webhook works (it carries `triggerid`, and the catalog gives the class), the recheck, the script list by hash and running by `scriptid`, the note, maintenance windows, `match` and `erase` of notes, which roles the users have. The `deploy/monitor/` files for this system.

**What the core checks.** The suite `conformance/suites/monitor` with a mock system: one record per episode transition and not a single E1 delivery, the `health` answer with a stopped loop and an unavailable store, a repeated webhook gives one event, a node outside the catalog is dropped, a note does nothing except adding a message, a script outside the catalog and outside the list is not run, a repeated delivery does not run a script a second time. `farmctl doctor` checks the permissions of the user of every adapter process, including that the `demo` user does not see `demo2` nodes and that no script is available to it on farm nodes (MON-4); the deployment lint checks that the monitoring system is not on a farm node and knows only the `health` key (MON-2). Export deliveries are retried until they are uploaded, and a stall longer than 15 minutes gives people the code message `monitor_export_stalled` (MON-11).

**Rules.** MON-1..5, MON-7..11, TEN-4, AUD-6.

**What never changes.** The farm does not deliver alarms to people itself and has no liveness check process of its own; adapter users in the monitoring system do not acknowledge, close or silence problems and do not run scripts on farm nodes or on nodes of other tenants (D-60, D-61, D-62, D-69).

### 7.32. A new monitoring problem class

**What to add.** A row of the closed table `spec/model/monitor-map.yaml`: the class and the farm alarm that the recheck raises; if needed, an incident class for this alarm (recipe 7.19). A problem gets its class not from a trigger tag but from the tenant catalog row `monitor_triggers: [{triggerid, class, item}]`: which triggers the farm accepts, with which class, and from which item the recheck reads the number. A problem for a trigger outside the catalog arrives with the class `unknown` and gives only `alarm.monitor.problem` of severity no higher than `warn`, without an incident.

**What the core checks.** Only closed fields go into the event `ext.monitor.problem.v1`, and the class is taken from the catalog by `triggerid`; only a `job` raises the alarm, after a recheck by code (MON-5, INJ-7). A route task for a trigger from the catalog does not get `untrusted_seen`, and for a trigger outside the catalog it does (INJ-13, D-82).

**Rules.** MON-5, INJ-7, MGR-7.

**What never changes.** The trigger name and the texts of the monitoring system do not get into the farm.

### 7.33. A managed service and a monitoring script

Bots with a role permission themselves start, stop and restart services on the catalog machines of their tenant and run monitoring system scripts on catalog nodes (D-67, D-69). No new plugin is needed for this: both tools are built in.

**What to add.**

1. The service into the tenant catalog: `Catalog.spec.hosts[].services[]` with `name` (for example `nginx`), `control` (a subset of `start`, `stop`, `restart`) and `via`. For `via: host` the field `unit` with the exact name of the service on the machine (for example `nginx.service` on `web1`); for `via: monitor` the field `scripts: {<verb>: <name from monitor_scripts_allowed>}`. Optionally `health` (how code checks the service after an action: a URL or a monitoring system item from a closed list) and stricter guards. The service's machine is a tenant catalog machine, not a farm node (DEP-11).
2. Scripts into the catalog: `hosts[].monitor_scripts_allowed: [{scriptid, name, hosts, choices}]`; `name` is the only name the model sees, and `choices` is the allowed input if the script has any. The role's permission in its manifest: `service_control: [nginx]` or `service_control: ["*"]` for all services of the catalog; for scripts `monitor_scripts: [<name>...]` or `["*"]`. People grant the permissions by editing the manifest in git, like the other role fields. For a person, the permission `service.control:nginx` gives the command `/service restart nginx` without a card (D-68).
3. For `via: host` the machine needs an action toolhost of this tenant; the access rule for the OS mechanism `unit` is generated by `apply` from the catalog and is not written by hand; which mechanism and rule it is, the binding `spec/bindings/os/systemd.md` says (DEP-4).
4. For scripts: the link of the node with the resource (`monitor_host`) and the script itself in the monitoring system, available to this tenant's adapter user on the tenant's node group; a script with input is allowed only with input from a list set in the script itself that matches the catalog's `choices` (MON-10). The core only checks the script name from the API by `scriptid` and does not show it to the model.
5. Eval cases: a bot without the permission gets a refusal naming the needed permission; a bot with the permission does it; an injection in a trigger or script name does not get into the model context; a route task for a problem with a trigger from the catalog restarts the service without a card; a task that read a log line from a log item through `monitor/item_text` or other untrusted text goes through a card (INJ-15, D-82).

**What the core checks.** At `apply`: a service without `control`, a verb outside the three, `via: host` without `unit` and `via: monitor` without a script for the verb are rejected; services of the core and of farm nodes in a tenant catalog fail the lint (MON-8). On a call: the service and the verb from the closed lists of the catalog and the role's permissions, the script by `scriptid` from the adapter's latest list for the node, the guards (6 actions per service and 30 per tenant per hour, one operation per service at a time; exceeding them gives `service_rate_limited`), holds and the operator's kill switch. A call from the allowed list is executed without a card by the audit record `service_grant` (APR-18); if the task saw untrusted text, the call goes through a card (INJ-15). A monitoring system problem of the tenant itself for a trigger from the catalog does not set the flag (INJ-13, D-82). After the action, code writes the note `service_control_done` to the monitoring system and checks the service state after 2 minutes.

**Rules.** MON-8, MON-10, MON-4, APR-18, INJ-15, DEP-4, DEP-9, DEP-11, HLD-3.

**What never changes.** Code chooses the machine, the service and the script from the catalog, not the model; the model has no free input into a script and no names from the monitoring system; services and nodes of the farm (`core1`) are not available to tenant bots.

## When an ADR is needed

An ADR is needed for a decision that changes a port or a contract: an operation or a guarantee of a port sheet, a wire schema, a lifecycle state, a closed list that permissions depend on (D-86 (13)). An ordinary new rule that changes no port and no contract is recorded as a registry row that refers to a decision D-NN, and it needs no ADR. These also need an ADR: a new auto-stop rule, a new message genus, a new dispatcher condition, a new kind or predicate of a fact, a new role profile, a new manifest kind that affects permissions, allowing a terminal with free input on a production machine. If no recipe above helps and the core has to be changed, a port is missing: that is also an ADR.

## What is in which version

| Version | What appears |
|---|---|
| v0.1 | Recipes 7.1-7.5, 7.7, 7.9-7.20 and 7.24-7.33: bot, plugin, terminal, channel, runtime, task type (with the example `data.describe.v1`, D-72), tenant, external program, model lineup; content store, alarm kind, auto-stop, telemetry, role profile, manifest kind, model provider, another monitoring system (export, liveness, nodes, problem input, scripts and code notes; bot notes and maintenance windows since v0.2); the example `examples/hello-bot/` without messages between bots (D-71); message type, bot group, analysis for a role, commission lens (D-71), campaign, manager with campaigns, aggregator, incident class, persona, monitoring problem class, managed service and monitoring script; 7.6 for memory (C4); `--bot` at `enroll`; the tenant charter and the `manager` line |
| v0.2 | Recipes 7.21-7.23: knowledge corpus, external event source, script in git; 7.6 for sessions (C3), 7.8 (sink); the outgoing webhook as a kind of sink |

## Why it works this way

- **Extension without changes to the core.** There is one thin core; everything that changes often lives in manifests and adapters (D-02, D-03).
- **One check point.** Whatever is added, the core checks it at `apply`, at `hello` and at every permission check point; there is no separate path "for insiders".
- **Closed lists instead of trust.** Tool parameters, message genera, role profiles, runtime deviations and control operations are closed lists; an element can be added, but in a way the core can check.
- **A port instead of a workaround.** If an extension requires changing the core, this is a signal that the model lacks a concept, and it is added explicitly through an ADR.

