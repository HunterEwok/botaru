# Glossary

## In short

The terms of the bot farm skeleton: what each one is, what it is easily confused with, and where it is described. Terms are grouped by topic and sorted alphabetically inside a topic. A name in backticks is an identifier from manifests, ports or schemas; names after a slash are other names for the same thing. Definitions say what a thing is; the rules about it are in [conformance/rules.md](../../conformance/rules.md), and decisions such as (D-33) are in the [decision log](../decisions.md).

## The farm and its parts

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| admin API (P2, `AdminAPI`) | The port through which people change and view the farm: `apply`, stops, workers, erasure, status. Its client is `farmctl`; operator operations come only from it. | P1 worker protocol | [P2](../../spec/ports/p2-admin-api.md) |
| agent runtime (E4, `AgentRuntime`) | The replaceable program that runs the model loop inside an agent attempt, calling the model only through P4 and tools only through P3. The reference is our own loop in Go (D-93); since D-114 E4 describes how the core is built rather than a checked contract. | worker; model gateway | [E4](../../spec/ports/e4-agent-runtime.md) |
| background loop | A deterministic, time-driven core duty without a model (`reaper`, `schedule`, `delivery`, `erasure` and others) that records a tick on every run. | improvement loop | [Background loops](../../spec/ports/background-loops.md) |
| built-in tool | A core tool from `builtin-tools.yaml` (`enqueue_child`, `request_action`, `send_message`, `service/control` and others) that P3 adds to an attempt's catalog by role profile. | the runtime's own tools (turned off) | [P3](../../spec/ports/p3-tool-gateway.md) |
| channel (E1, `Channel`) | The adapter that talks with people: it verifies incoming messages, submits them as `Activity` and renders deliveries and cards. One process per tenant; the reference is Telegram. | source channel; sink | [E1](../../spec/ports/e1-channel.md) |
| contract binding | The exact form of a contract in a protocol or product, under `spec/bindings/`: HTTP for P1, the tool protocol (MCP) for P3, the OS binding, channel notes. Rules name the port; bindings name the product. | chat binding; identity binding | [Ports](../../spec/ports/README.md) |
| core | The farm code that all rules trust: the core process, its loops, the P3 and P4 gateways, the store and the authorizer, plus `farmctl` on the core machine. | external program | [Architecture](../architecture.md) |
| declared object | An object from a manifest in git that `apply` writes to the store; only the next `apply` changes it. | recorded object (principal, chat binding, stop) | [lifecycle: manifest](../../spec/lifecycles/manifest.md) |
| farm control CLI (`farmctl`) | The reference P2 client for people; on the core machine it can write a stop directly to the store when the core is down. | the core process | [Architecture](../architecture.md) |
| farm node (`farm_hosts`) | A machine that hosts the core, the store, a channel or backups; in the examples `core1` (core) and `backup1` (backups). A farm node is never a tenant catalog machine. | tenant machines `web1`, `db1`, `ctd1` | [Architecture](../architecture.md), [DEP-11](../../conformance/rules.md#DEP-11) |
| handle | A bearer credential for one lease (tool, model, session or memory handle) that expires with the lease and never reaches the model, logs or audit. | worker token; lease token | [SEC-3](../../conformance/rules.md#SEC-3) |
| manifest | A declaration in git in the wrapper `{apiVersion, kind, metadata, spec}`; its status is kept by the core, not in git. | task record | [Task language](../task-language.md) |
| manifest apply (`apply`) | The P2 operator operation that brings the store to the manifests of one commit, all or nothing. | `enroll`; `hello` | [MAN-6](../../conformance/rules.md#MAN-6) |
| model gateway (P4, `ModelGateway`) | The part of the core between the runtime and the provider: it checks the request, picks the model by slot, reserves and charges money and holds the provider keys. It is not a pass-through proxy (D-45). | agent runtime; provider | [Models and money](../models-and-money.md) |
| port | A contract between the core and a replaceable part. Since D-114 only the boundaries with foreign code (P1, P3, P4, E1, E3, E5) are checked contracts; the other sheets describe how the core is built. | contract binding; plugin | [Ports](../../spec/ports/README.md) |
| reason (`reason`) | A code from the closed registry `spec/model/reasons.yaml` in an error answer, which says whether it spends an attempt and whether to retry. | `reason_code` of a human decision; `waiting_reason` | [Tasks and delivery](../tasks-and-delivery.md) |
| reference implementation | The implementation on which a port and its checks are tried: PostgreSQL for C1, Telegram for E1, our Go loop for E4; the farm itself is in `impl/` (D-92). | binding | [Architecture](../architecture.md) |
| runtime deviation | A value from `runtime-deviations.yaml` (`session`, `memory`, `model_attribution`, `catalog`) that an agent worker declares in the `runtime` object of P1 `hello` when its runtime does a farm mechanism differently; a substitute RT rule then applies. | expected failure | [RT-1](../../conformance/rules.md#RT-1) |
| sink (E2, `Sink`) | An adapter that writes artifacts to external storage without people: an archive on disk, and since v0.2 a signed webhook. | channel | [E2](../../spec/ports/e2-sink.md) |
| task store (C1, `TaskStore`) | The core store with atomic transitions and a queue: tasks, leases, events, stops, audit. Only core processes reach it. | session store | [C1](../../spec/ports/c1-task-store.md) |
| tool | A plugin operation the model calls through P3: a read tool, or a write tool that runs only as an approved action. | built-in tool; action | [P3](../../spec/ports/p3-tool-gateway.md) |
| tool gateway (P3, `ToolGateway`) | The part of the core that assembles an attempt's tool catalog and checks every call; the runtime's only MCP server. | tool host | [P3](../../spec/ports/p3-tool-gateway.md) |
| tool host (E3, `ToolHost`) | A worker that holds a tenant's plugins and runs their calls, fixed to placement `host` or `sandbox` at `enroll`. | plugin; tool gateway | [E3](../../spec/ports/e3-tool-host.md) |
| tool plugin (`ToolPlugin`) | An MCP server with tools, declared by `tools/<plugin>/tool.yaml`; the model sees descriptions only from the manifest. | tool host; skill | [E3](../../spec/ports/e3-tool-host.md) |
| worker | A process that takes work through P1 `claim`, of kind `agent`, `job`, `toolhost`, `channel`, `sink` or `monitor`; it serves one tenant. | bot (identity); external program | [lifecycle: worker](../../spec/lifecycles/worker.md) |
| worker enrollment (`worker.enroll`) | The P2 operator operation that records a worker with tenant, role, kind and labels and issues its token. | `hello` | [lifecycle: worker](../../spec/lifecycles/worker.md) |
| worker hello (`hello`) | The first P1 call: the worker reports kind, labels, capacity and versions, and an agent its runtime. | `enroll`; `apply` | [P1](../../spec/ports/p1-worker-protocol.md) |
| worker protocol (P1, `WorkerProtocol`) | The only entry of farm processes into the core; workers pull work with `claim`. | P2 admin API | [P1](../../spec/ports/p1-worker-protocol.md) |
| write rule | A sender writes only a body or a manifest `spec`; record fields and status are written only by the core. | task body | [IDN-1](../../conformance/rules.md#IDN-1) |

## Tenants, people and programs

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| attempt permissions | The worker role's permissions intersected with the group permissions of every initiator. Nothing else grants permissions. | role profile; position | [IDN-9](../../conformance/rules.md#IDN-9) |
| capability (`Capability`) | A permission string in a group, such as `task:ops.investigate.v1` or `service.control:nginx`; the forms are in `capabilities.yaml`. | role declarations (`claims`, `tools`) | [Permissions](../permissions.md) |
| export | A copy of tenant or principal data in JSON Lines written by a core task to an export-only sink, never to a chat. | backup | [LCY-2](../../conformance/rules.md#LCY-2) |
| external program | A client with its own key: a principal of kind `service` with groups, like a person (D-47). | worker; bot | [Permissions](../permissions.md) |
| group (`Group`) | A set of capabilities, data classes and a daily money limit in the tenant's git, for example `user`, `staff`, `admin` in `demo`. | bot group; group chat | [Permissions](../permissions.md) |
| identity (`Identity`) | A principal's address in an external system: `telegram:<id>`, `api:<key_id>`, `hook:<source>`. | bot identity | [IDN-4](../../conformance/rules.md#IDN-4) |
| identity assurance (`identity_assurance`) | A channel driver's declaration whether it verifies the sender (`strong`) or not (`weak`). | membership assurance | [IDN-3](../../conformance/rules.md#IDN-3) |
| initiator | The verified principal on whose behalf a task runs; the core writes it and children inherit it. | message sender | [IDN-2](../../conformance/rules.md#IDN-2) |
| membership (`Membership`) | The link of a principal to a group, `pending` or `active`, with an expiry and its granter. | chat binding | [PRM-7](../../conformance/rules.md#PRM-7) |
| operator | A person with `admin:*` in the service tenant `farm` who works through git and `farmctl`, never from a channel. | tenant admin | [Permissions](../permissions.md) |
| person offboarding | `/off` turns a person off reversibly; P2 `principal.offboard` removes them and opens an erasure request for `(tenant, subject)`. | tenant offboarding; identity retirement | [LCY-3](../../conformance/rules.md#LCY-3) |
| principal (`Principal`) | Whoever acts in one tenant: a person (`human`), an external program (`service`) or `system`. Records hold an opaque id such as `human:p_<ULID>`; names like `alice` exist only in examples. | bot; worker | [Permissions](../permissions.md) |
| privileged group | A group with an action, an `admin:` scope, a class above `internal`, tier 1 or higher, or a decision right; only the operator grants it. | the `admin` group | [PRM-13](../../conformance/rules.md#PRM-13) |
| service tenant `farm` | The tenant for what belongs to the whole farm: the farm manager seat, operator permissions, farm-scope stops. | a main tenant (there is none, D-23) | [Architecture](../architecture.md) |
| system principal `system` / `system-ops` | The principal on whose behalf schedules and routes work; its group lists exactly the types they enqueue. | operator | [PRM-10](../../conformance/rules.md#PRM-10) |
| tenant (`Tenant`) | Whose data and whose money: a wall nothing crosses. Declared in git with its own provider workspace, limits and privacy block. | bot group; workspace | [lifecycle: tenant](../../spec/lifecycles/tenant.md) |
| tenant admin | A person with `admin:people` in their tenant who grants ordinary groups and manages people. | operator | [Permissions](../permissions.md) |
| tenant offboarding (`offboarding`) | Credentials and the key are revoked and only export remains; `tenant.delete` then erases the tenant for good. | person offboarding | [LCY-1](../../conformance/rules.md#LCY-1) |

## Tasks, work and delivery

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| action (`Action`) | A named operation on a working machine with a closed argument schema, risk, approver count and executor; it runs exactly as approved. | write tool; `job` type | [MAN-9](../../conformance/rules.md#MAN-9) |
| activity (`Activity`) | An update from a person verified by the channel: sender, chat, text, attachments, `reply_to`. | bot message; delivery | [IDN-6](../../conformance/rules.md#IDN-6) |
| AI notice (`ai_notice`) | The code text "you are talking to a bot" sent on first contact when the channel declares `ai_disclosure: first_contact`; model text waits until it is delivered. | privacy notice | [DLV-12](../../conformance/rules.md#DLV-12) |
| attempt | One handout of a task to a worker under a lease; at most 3 per task. | runtime run | [lifecycle: lease](../../spec/lifecycles/lease.md) |
| catalog (`Catalog`) | The tenant manifest of resources (machines, services, datasets, corpora), value lists, selectors and volumes that closes the model's input. | format registry | [MAN-10](../../conformance/rules.md#MAN-10) |
| child task | A task an attempt enqueues with `enqueue_child`, a type from `may_enqueue`, at most 10 direct children. | continuation | [Coordination](../coordination.md) |
| continuation (`Continuation`) | A task the core enqueues on a trigger (`reply`, `campaign`, `script`, and since v0.2 `timer`, `await`) with a type from git. | child task | [CNT-1](../../conformance/rules.md#CNT-1) |
| conversation (`conversation`) | An opaque thread id (private chat, group chat, topic) that the core computes from `origin`. | runtime session | [Task language](../task-language.md) |
| CTD data description (`data.describe.v1`) | The science flow of v0.1 (D-72): a person sends a CTD probe file, plugin code computes the numbers, and the model writes a short report only from them. | pipeline (`/run`) | [Capabilities](../capabilities.md) |
| dead work | A task that ended `failed` because attempts, runs or the deadline ran out, or a `dead` delivery; visible in `deadletter.list`. | canceled task | [DLV-4](../../conformance/rules.md#DLV-4) |
| dead work redrive (`redrive`) | New work linked to dead work, with permissions checked again; not for `action.*`. | "retry failed"; debug replay | [DLV-4](../../conformance/rules.md#DLV-4) |
| debug replay (`task.replay`) | An operator operation (v0.2) that reruns a task's body against its recorded journal, with no writes, approvals or delivery. | redrive; dry run | [APR-8](../../conformance/deferred.md#APR-8) |
| delivery (`Delivery`) | A record "send this content to this recipient", at least once: to people, a sink or the monitoring adapter. | bot message | [lifecycle: delivery](../../spec/lifecycles/delivery.md) |
| delivery target | A value from a closed list: `origin`, `group:<group>`, `chat:<binding>`, `sink:<sink>`, `approval_initiator`, or the monitoring adapter. | message address | [DLV-6](../../conformance/rules.md#DLV-6) |
| event | A CloudEvents 1.0 record from `spec/bindings/events/catalog.yaml` that routes react to. | message; alarm | [IDM-4](../../conformance/rules.md#IDM-4) |
| field read by code | A result field marked `x-botaru-read-by: code` with a closed kind; only such fields drive code and routes. | free model text | [OUT-1](../../conformance/rules.md#OUT-1) |
| goal (`goal`) | The only free field of a task body, up to 4000 characters; it permits nothing. | call target; processing purpose | [Task language](../task-language.md) |
| group chat summary | An answer into a group chat: a `summary_fields` table, code texts and up to 800 characters of marked model text (D-48). | reporter's digest | [DLV-15](../../conformance/rules.md#DLV-15) |
| idempotency key | A retry key unique per `(tenant, idempotency_key)` (D-43): a retry returns the same task. | task id; `alarmkey` | [IDM-1](../../conformance/rules.md#IDM-1) |
| lease (`Lease`) | The right of one holder to work on a unit of work until a deadline; one mechanism for attempts, tool calls and deliveries. | seat | [FEN-1](../../conformance/rules.md#FEN-1) |
| lease token | The signed `(epoch, work_id, lease_no, worker_id)` sent in the `Botaru-Lease` header; a stale one gets `fenced`. `lease_no` grows on every handout. | worker token; handle | [FEN-2](../../conformance/rules.md#FEN-2) |
| pipeline (`/run`) | A row of the tenant's `run` field, a `job` type or an action, started with `/run` (D-49). | script; campaign | [APR-17](../../conformance/rules.md#APR-17) |
| purpose (`purpose`) | The kind of work of a task type from `task-purposes.yaml` (`work`, `report`, `insight`, `review`, `manage` and others); it sets the model slot and budget line. | processing purpose | [Models and money](../models-and-money.md) |
| route (`Route`) | A rule in the tenant's git: what the core does on an event, inbox, message or task end. | schedule | [MSG-16](../../conformance/rules.md#MSG-16) |
| runtime run | One run of the agent with a model inside an attempt; only runs with a model answer count toward the limit of 6. | attempt | [GW-13](../../conformance/rules.md#GW-13) |
| schedule (`Schedule`) | A tenant manifest with `cron`, time zone, task and targets; the `schedule` loop enqueues one task per slot and never catches up a skipped slot. | stop of scope `schedule` | [TIM-3](../../conformance/rules.md#TIM-3) |
| summary fields (`summary_fields`) | Closed result fields of a type that code puts into reports, chat summaries and campaign tables. | aggregator | [Task language](../task-language.md) |
| task (`Task`) | A unit of work: a body from the sender and a record from the core, with A2A states `submitted`, `working`, `completed`, `failed`, `canceled`, `rejected`. | attempt; message | [lifecycle: task](../../spec/lifecycles/task.md) |
| task body (`TaskBody`) | What the sender sends: `type`, `goal`, `params`, `attachments`. | task record | [Task language](../task-language.md) |
| task record | The task fields only the core writes: tenant, initiators, `origin`, tree, marks and flags. | task body | [IDN-1](../../conformance/rules.md#IDN-1) |
| task tree | A root task and everything enqueued from it; depth at most 5, at most 300 tasks per tenant by default. | trace | [PRM-6](../../conformance/rules.md#PRM-6) |
| task type (`TaskType`) | A versioned declaration in git (`ops.investigate.v1`): executor, purpose, tier, class, schemas, deadlines, `tree_budget_eur`. | role; message type | [Task language](../task-language.md) |
| tool call (`ToolCall`) | The core record of one tool call: a read call from a live attempt, or a write call the core creates from an approval and runs once. | action | [lifecycle: tool call](../../spec/lifecycles/toolcall.md) |
| work claim (`claim`) | The P1 call by which a worker takes an attempt, tool call, delivery or erasure step; the only path to work. | releasing a campaign item | [DSP-1](../../conformance/rules.md#DSP-1) |

## Permissions, approvals and human decisions

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| approval (`Approval`) | A record of an action request with a snapshot of its arguments, waiting for a human decision. | "Acknowledged" in monitoring | [lifecycle: approval](../../spec/lifecycles/approval.md) |
| card | A delivery with decision buttons rendered by a code template; no model text. | notification; incident message | [APR-3](../../conformance/rules.md#APR-3) |
| chat binding (`ChatBinding`) | An operator record that gives a group chat to a tenant with a class ceiling; the bot ignores unbound chats. | contract binding; identity binding | [PRM-14](../../conformance/rules.md#PRM-14) |
| data class / data mark (`data_mark`) | Sensitivity `summary` < `internal` < `raw_logs`; a task's mark is fixed at enqueue. `pii` is reserved and fails `apply`. | retention class | [PRM-5](../../conformance/rules.md#PRM-5) |
| decision subject kind (`subject_kind`) | The kind of record that waits for a human decision: `approval`, `proposal`, `campaign`, `script`, `control`, `governance_case`, `attribution` and others. | principal | [lifecycle: decision](../../spec/lifecycles/decision.md) |
| dry run | P1 `enqueue` with `mode: validate` (v0.2): every check, no write. | debug replay | [PRM-11](../../conformance/deferred.md#PRM-11) |
| human decision | The common lifecycle of any subject (`pending`, then `decided`, `expired` or `canceled`), accepted exactly once through P1 `decide`. | verdict | [APR-4](../../conformance/rules.md#APR-4) |
| routine action (`risk: routine`) | An idempotent action that writes only to the tenant's own directories, within rate limits, started with `/run` without a second person (D-78). | `/run` of a `job` type | [APR-17](../../conformance/rules.md#APR-17) |
| tier (`tier`) | What a task may do: 0 summaries, 1 reading, 2 sandbox (v0.2), 3 action; set by the task type. | model rank; priority class | [PRM-4](../../conformance/rules.md#PRM-4) |

## Messages and coordination

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| bot group (`BotGroup`) | A selector of a tenant's workers by role and labels; it grants no permissions. | group (`Group`) | [BGR-2](../../conformance/rules.md#BGR-2) |
| campaign (`Campaign`) | A frozen plan of mass work in one tenant: a template, items, a rollout policy and `then`. | script | [lifecycle: campaign](../../spec/lifecycles/campaign.md) |
| channel command | A person's command in a private chat that code answers without a model: `/tasks`, `/status`, `/cancel`, `/help`, `/about`, `/me`, `/lang`, `/mydata`, `/team`, `/service`, `/a`. | operation button | [PRM-15](../../conformance/rules.md#PRM-15) |
| digest (`digest`) | A reporter's delivery built from code tables, cards and one marked model paragraph. | proposal digest | [REP-2](../../conformance/rules.md#REP-2) |
| dispatcher | The core rule inside `claim`: filter by conditions, order by class, fairness key, time and id. | manager | [DSP-2](../../conformance/rules.md#DSP-2) |
| inbox / inbox entry | A queue in the store per role, bot group or worker; an entry is one message in it, leased by a reading attempt. | task pool | [lifecycle: message](../../spec/lifecycles/message.md) |
| manager | A bot of profile `manager` that puts work into the pool and proposes campaigns; it does not assign work. | farm manager | [Coordination](../coordination.md) |
| message (`Message`) | A short record from a bot to bots under its type's closed schema; it grants nothing and never enters an instruction. Only its `note` is free and untrusted. | task; event | [MSG-7](../../conformance/rules.md#MSG-7) |
| message type (`MessageType`) | A manifest `<genus>.<name>.vN` with schema, class ceiling, delivery mode (`one` or `all`), size and producers; the genus is `report` or `state`. | task type | [MSG-2](../../conformance/rules.md#MSG-2) |
| priority class | 1 `control`, 2 `interactive`, 3 `normal`, 4 `batch`, 5 `background`; set by the type, only lowered by a request. | tier; rank | [DSP-5](../../conformance/rules.md#DSP-5) |
| reporter | A bot of profile `reporter` that reads views and the inbox and reports to people; it decides nothing. | aggregator | [REP-6](../../conformance/rules.md#REP-6) |
| "retry failed" | A button that makes a new campaign from the failed and unknown items, with a new card. | `redrive` | [Coordination](../coordination.md) |
| rollout canary / gate | The first campaign items as a separate batch; with `after_canary: manual`, a pause until a person continues. | check canary; script gate | [Coordination](../coordination.md) |
| task pool | A tenant's `submitted` tasks in the queue; work leaves only through `claim`. | inbox | [DSP-1](../../conformance/rules.md#DSP-1) |
| waiting reason (`waiting_reason`) | Why a `submitted` task is not handed out now: `no_member`, `held`, `provider_unavailable` and others. | refusal reason | [DSP-7](../../conformance/rules.md#DSP-7) |
| wake-up | A `system` task from a route `on: inbox` that reads an inbox, with its own money and count ceilings. | continuation | [MSG-11](../../conformance/rules.md#MSG-11) |

## Scripts, translator and profiles

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| role (`Role`) | A manifest `roles/<role>/role.yaml`: which types its workers take, tools, model slots, messages and the services and scripts it may control. | RBAC role; position | [Task language](../task-language.md) |
| role profile (`Role.spec.profile`) | A value from `role-profiles.yaml` (`worker`, `manager`, `reporter`, `translator`, `reviewer`, `aggregator`, `farm-manager`, `farm-manager-reader`) that narrows a role and carries its rank ceilings. | model profile; conformance profile | [PRF-1](../../conformance/rules.md#PRF-1) |
| script (`Script`) | A strict plan of steps by the closed schema `botaru.script/v1`, referring only to applied types, actions and catalog resources; no expressions or loops. | campaign; shell script | [lifecycle: script](../../spec/lifecycles/script.md) |
| script gate (`confirm`) | A step that asks a person "continue?" and changes nothing in the world. | rollout gate | [Coordination](../coordination.md) |
| script run (`ScriptRun`) | The core record of an accepted script with computed fields, step states and links to what it created. | campaign | [SCR-11](../../conformance/rules.md#SCR-11) |
| script step | One element of a script: `task`, `fanout`, `action`, `confirm`, and since v0.2 `wait` and `await`. | skeleton version step | [SCR-1](../../conformance/rules.md#SCR-1) |
| translator | A bot of profile `translator` that turns a person's plain-language assignment into a script draft through `propose_script`. | functionary | [TRN-2](../../conformance/rules.md#TRN-2) |

## Bots, memory and the commission

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| attribution candidate (`Attribution`) | A possible link of an incident to a realized change, made by code or a person and decided only by a person ("Yes", "No", "Don't know"). | incident | [lifecycle: attribution](../../spec/lifecycles/attribution.md) |
| bot / bot identity (`Bot`) | An identity `bot_<ulid>` with a role, a persona, memory and a track record; the role's workers carry out its tasks. Only the core creates it. | worker; role | [lifecycle: identity](../../spec/lifecycles/identity.md) |
| bot epoch (`bot_epoch`) | A number the core raises on every binding change; records with an old one get `fenced`. | seat epoch; store epoch | [IDN-8](../../conformance/rules.md#IDN-8) |
| charter (`governance.yaml`) | The manifest that declares a tenant's commission seats, lenses, fallback roles and seat model profiles, or in `farm` the farm manager seat and its limits. | commission manifest | [GOV-33](../../conformance/rules.md#GOV-33) |
| commission | The seats with lenses in each tenant that blindly review bot proposals, held by former functionaries, plus the farm manager as an advisory lens (D-23, D-29). | the people who decide | [Improvements](../improvements.md) |
| disputed attribution (`disputed`) | The state of a "Yes" attribution after the incident was dismissed as false; its author confirms or withdraws it, and 7 days of silence withdraw it (D-79). | candidate without an answer | [GOV-30](../../conformance/rules.md#GOV-30) |
| fact (`Fact`) | A verified record about the farm: the model names kind, predicate and subject, and the core computes the rest. | observation; finding | [lifecycle: fact](../../spec/lifecycles/fact.md) |
| fact offer (`FactOffer`) | The record by which code rechecks a retired identity's facts and copies those that pass to its successor. | improvement proposal | [MEM-7](../../conformance/rules.md#MEM-7) |
| fallback role (`fallback_role`) | The reviewer role that does a lens's tasks while it has no seat holder. | fallback model | [COM-7](../../conformance/rules.md#COM-7) |
| functionary / working bot | An identity of profile `worker`, `manager`, `reporter` or `aggregator` without an owner (D-22); only functionaries enter the promotion queue. | personal assistant; translator | [Bots](../bots.md) |
| governance case (`GovernanceCase`) | One promotion of a functionary into its tenant's commission, for a free seat or with one holder leaving. Code proposes the leaver by accuracy, or a person chooses from the list L of seats that can leave (D-115); the outcome needs a person's "Confirm". | improvement proposal; eval case | [lifecycle: case](../../spec/lifecycles/case.md), [GOV-12](../../conformance/rules.md#GOV-12) |
| identity retirement (`retired`) | An identity's move to `retired` for one of the reasons in `bot-retire-reasons.yaml`; its memory is sealed. | person offboarding | [IDN-11](../../conformance/rules.md#IDN-11) |
| memory scope `bot` | An identity's own memory: core records and verified facts only, no free notes (D-35). | session; scope `person` | [MEM-2](../../conformance/rules.md#MEM-2) |
| memory scope `person` / memory item | Memory about a person that the model proposes and only that person makes active; seen with `/memory`, deleted with `/forget`. | fact | [lifecycle: memory item](../../spec/lifecycles/memory-item.md) |
| persona (`Persona`) | The file `personas/<name>.md`, one per position, inserted into the trusted part of the instruction; it permits nothing. | skill; instruction | [GOV-4](../../conformance/rules.md#GOV-4) |
| personal assistant (`assistant`) | An identity of profile `worker` whose worker has an owner: it serves only that person, from their private chat or `/a` (D-07). | functionary | [IDN-5](../../conformance/rules.md#IDN-5) |
| position | What the core derives from profile, owner and binding: functionary, assistant, translator, commission member, farm manager. It picks the persona and rank ceiling and grants nothing. | role; role profile | [Bots](../bots.md) |
| promotion probation (`probation`) | 14 days from `realized` that a change must live without a confirmed attribution or measured harm before it can promote its author. | change trial | [GOV-14](../../conformance/rules.md#GOV-14) |
| promotion trigger | A functionary's own accepted, realized and not-worse proposal whose probation passed with every attribution answered "No". | credit in "My outcomes" | [GOV-14](../../conformance/rules.md#GOV-14) |
| reviewer / commission member | A bot of profile `reviewer` holding a commission seat or running a fallback role; it takes only `review` tasks and has no session. | eval judge | [COM-3](../../conformance/rules.md#COM-3) |
| seal | The state of a retired identity's memory until it is erased: only the fact recheck for the successor reads it. | stop; tombstone | [MEM-9](../../conformance/rules.md#MEM-9) |
| seat (`Seat`) | A chair in a tenant's commission or the farm manager's chair; its holder is an identity, and it has no lease. | lens; machine | [lifecycle: seat](../../spec/lifecycles/seat.md) |
| seat holder | The bot identity bound to a seat; the seat gives it the `seat` model slot and the seat's tasks, not permissions. | worker of the seat role | [IDN-10](../../conformance/rules.md#IDN-10) |
| session | A runtime conversation transcript under a key the core computes; it never passes to another bot. | a login; conversation | [lifecycle: session](../../spec/lifecycles/session.md) |
| track record | The core's record of an identity's history and proposal outcomes; the model neither writes nor sees it. | "My outcomes" | [Bots](../bots.md) |
| unawareness | No bot context contains cases, the promotion queue or retirement marks, so a bot does not know its reviews affect promotion. | reviewer blindness to the author | [GOV-29](../../conformance/rules.md#GOV-29) |

## Farm manager

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| consent group (`farm-manager-ops`) | The group in each tenant whose permissions are the tenant's consent to the farm manager's operations. | `admin` group | [MGR-3](../../conformance/rules.md#MGR-3) |
| control operation | An operation from `control-ops.yaml` (`hold`, `quarantine`, `drain`, `preempt`, `cancel`, `pin_task`, `notify` and others), applied by the seat holder or people on one code path. | action; a person's stop | [MGR-9](../../conformance/rules.md#MGR-9) |
| decision role (`farm-manager`) | The seat role with purpose `manage` that decides control operations and grants from code views and closed fields only. | reader role | [MGR-25](../../conformance/rules.md#MGR-25) |
| escalation (`Escalation`) | A closed-form record from a bot to the farm manager (v0.2), filtered by code and the necessity lens; the bot gets no answer. | alarm escalation | [lifecycle: escalation](../../spec/lifecycles/escalation.md) |
| farm manager | One bot for the whole farm with the strongest model (D-18) that prevents critical incidents with control operations, decides model grants and is an advisory lens in every commission. Only a person appoints it (D-33); a tenant can opt out (D-77). | manager; operator | [Farm manager](../farm-manager.md) |
| farm manager seat | The seat record in `farm`: holder, epoch, state (`vacant`, `held`, `stopped`) and stop reasons. Nothing in the core waits for it. | commission seat | [MGR-2](../../conformance/rules.md#MGR-2) |
| finding (`manager-finding/v1`) | The only output of the reader role: a closed record about a subject with evidence ids and an untrusted note. | observation; fact | [MGR-25](../../conformance/rules.md#MGR-25) |
| incident (`Incident`) | A record the core opens from a code alarm and a class; code moves it to `resolved`, a person or the holder closes it, and only a person dismisses it as false. | alarm; escalation | [lifecycle: incident](../../spec/lifecycles/incident.md) |
| incident class (`IncidentClass`) | A reviewed file in farm git: which alarm opens it, who is involved, the first code measure and what the farm manager may do. | alarm type; retention class | [MGR-7](../../conformance/rules.md#MGR-7) |
| incident dismissal (`incident.dismiss`) | A person's declaration that an incident is false, with a reason code and details. | closing (`closed`) | [MGR-8](../../conformance/rules.md#MGR-8) |
| kill switch | A `seat` stop with reason `human` set through `farmctl`; it cancels the holder's work in every tenant. | safeguard | [MGR-15](../../conformance/rules.md#MGR-15) |
| model grant (`ModelGrant`) | A record by which P4 gives one task a stronger profile for 1-2 attempts within a euro cap (v0.2). | takeover | [lifecycle: model grant](../../spec/lifecycles/grant.md) |
| preemption (`preempt`) | The attempt goes to `preempted` without spending, and the task returns to the queue; at most 2 per task. | takeover | [MGR-11](../../conformance/rules.md#MGR-11) |
| quarantine (`quarantine`) | A ready stop on a worker: no claims, running work canceled, handles revoked. | worker drain | [Farm manager](../farm-manager.md) |
| reader role (`farm-manager-reader`) | The seat role with purpose `inspect` that reads tenant data and hands in only findings, when the tenant enabled `incident_review`. | lens reviewer | [MGR-30](../../conformance/rules.md#MGR-30) |
| revert (`control.revert`) | Removes the traces of seat operations for an incident or since a time; it does not undo carried-out actions. | reverse edit | [MGR-15](../../conformance/rules.md#MGR-15) |
| safeguard | The `no_control` stop reason that code adds to the seat after repeated refused operations; only the operator removes it. | circuit breaker; kill switch | [MGR-14](../../conformance/rules.md#MGR-14) |
| takeover (`takeover`) | A new task of the same type on the seat profile by a person's card (v0.2); the original is canceled. | preemption; model grant | [MGR-21](../../conformance/rules.md#MGR-21) |
| tenant opt-out (`farm_manager.enabled`) | `false` keeps the farm manager out of a tenant's data and decisions; its incidents go only to the tenant admins (D-77). | suspension | [MGR-42](../../conformance/rules.md#MGR-42) |

## Improvements and checks

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| accuracy | A code measure of a lens executor's forecasts against the lens base rates over 180 days (v0.3); code proposes who leaves by it. | lens weight | [COM-17](../../conformance/rules.md#COM-17) |
| aggregator | A bot of profile `aggregator` that reads only aggregated inputs and proposes one level up (D-17). | reporter | [AGG-2](../../conformance/rules.md#AGG-2) |
| audit sample | Rejected or parked proposals drawn at random each month and shown to people, so commission underrating becomes visible. | shadow reviews | [DIG-5](../../conformance/rules.md#DIG-5) |
| change trial (`trial`) | The core record that measures an accepted change for exactly 14 days from `realized` against a metric and 1-3 guards; outcome `better`, `unchanged`, `worse`, `undetermined` or `mixed`. | probation; dry run | [lifecycle: change trial](../../spec/lifecycles/trial.md) |
| guard | A series that must not get worse during a change trial; firing gives `worse` and a "Revert" card. | rate limiter | [PRP-23](../../conformance/rules.md#PRP-23) |
| improvement cycle | Observations, analysis, proposal, code checks, commission, a person's decision, realization, trial, promotion (D-16). | background loops | [Improvements](../improvements.md) |
| lens | A review point of view that one member of the commission represents with its own persona addendum: from `lenses.yaml` `security` and `operational` with veto, `cost`, `value`, `speed`, `clarity`, and since v0.2 `farm_manager` and `necessity`; a tenant adds its own lenses `x_<name>` without a veto in its charter. | seat | [COM-19](../../conformance/rules.md#COM-19) |
| observation | A number or `enum` value in a series, written only by code. | event; fact | [OBV-1](../../conformance/rules.md#OBV-1) |
| observation series (`ObservationSeries`) | A tenant series declared in git; with a full `outcome` block it can be a trial metric or guard. | metrics catalog | [OBV-2](../../conformance/rules.md#OBV-2) |
| proposal (`Proposal`) | A closed record with a problem, evidence and exactly one change; it does nothing until a person presses "Accept". | approval; governance case | [lifecycle: proposal](../../spec/lifecycles/proposal.md) |
| seat analysis | A task of purpose `insight` in which a role analyzes its own numbers and returns up to three proposals. | aggregator | [INS-1](../../conformance/rules.md#INS-1) |
| self influence / self-measured outcome | The share of evidence the author produced; an outcome measured only on the author's own points is marked `self_measured` and needs a person's "Count it". | audit sample | [PRP-22](../../conformance/rules.md#PRP-22) |
| shadow reviewer | A reviewer without a seat that reviews a sample of proposals only for comparison (v0.2). | fallback role | [COM-10](../../conformance/deferred.md#COM-10) |
| verdict | The code outcome from lens scores: `recommended`, `parked(...)` or `dismissed(...)`. | human decision | [COM-4](../../conformance/rules.md#COM-4) |
| veto | A block by `security` or `operational`; only a person lifts it, on a sample item. | a person's "Reject" | [COM-4](../../conformance/rules.md#COM-4) |

## Models, tools, knowledge and money

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| budget / limit | Money in euros: tenant month, day and hour limits, a person's daily limit, the tree budget and budget lines. Overspend is zero. | tenant counters | [BUD-2](../../conformance/rules.md#BUD-2) |
| budget line (`line`) | A sublimit of the monthly limit by purpose: `work`, `insight`, `aggregate`, `review`, `governance`, `manager`, `grant`, `takeover`. | usage line | [BUD-7](../../conformance/rules.md#BUD-7) |
| circuit breaker | A P4 state per (workspace, model pool) that refuses calls at once after a series of provider failures. | stop; safeguard | [GW-11](../../conformance/rules.md#GW-11) |
| enqueue estimate | The price reserved at enqueue: the 90th percentile of the type's actual tree price, or the worst case. | worst case of a call | [BUD-14](../../conformance/rules.md#BUD-14) |
| fallback model | A profile from the slot profile's `fallback` list that P4 uses only for a reason from `fallback_on`. | fallback role | [GW-5](../../conformance/rules.md#GW-5) |
| file (`File`) | Bytes plus a core record (`file_ref`, origin, type, mark, expiry); a file's content is always untrusted. | artifact | [lifecycle: file](../../spec/lifecycles/file.md) |
| knowledge corpus (`Corpus`) | A catalog resource of reference documents, `curated` in reviewed git or `ingested`; the model reads it only through `kb` tools. | memory; skill | [KNW-2](../../conformance/rules.md#KNW-2) |
| model family (`family`) | A profile field from `model-families.yaml`; profiles of one family share a pool of rate limits. | rank | [GW-10](../../conformance/rules.md#GW-10) |
| model profile (`ModelProfile`) | A manifest with the exact model, family, rank, pool, output limit, speed, provider terms and dated prices. | role profile | [GW-7](../../conformance/rules.md#GW-7) |
| model provider | The external party the farm pays by API key (D-04); for a tenant, a subprocessor. | P4 | [GW-3](../../conformance/rules.md#GW-3) |
| model rank (`rank`) | 1-4 in a profile: higher is stronger and costlier. Rules name ranks, not models. | tier | [GOV-3](../../conformance/rules.md#GOV-3) |
| model slot | A role's reference to a profile by kind of work: `base`, `analysis`, `seat`, `granted`. | schedule slot | [GW-7](../../conformance/rules.md#GW-7) |
| output limit (`max_output_tokens`) | One call's response limit in the profile, reasoning included; there is no `max_tokens`. | tree budget | [BUD-9](../../conformance/rules.md#BUD-9) |
| reserve | Money set aside before spending: at enqueue and before each model call. | charge | [BUD-1](../../conformance/rules.md#BUD-1) |
| run fingerprint (`RunFingerprint`) | A hash of what produced an answer: role, type, skills, persona, catalog, model, runtime. | static fingerprint | [PRV-1](../../conformance/rules.md#PRV-1) |
| skill (`Skill`) | A role procedure as text in `skills/<name>/`, pinned by hash; it grants nothing. | corpus; persona | [SKL-1](../../conformance/rules.md#SKL-1) |
| source channel | An E1 driver with `direction: in` that turns deliveries from an external system into `signal` activities. | sink | [ING-1](../../conformance/rules.md#ING-1) |
| tree budget (`tree_budget_eur`) | The money limit of a whole task tree, set in the root type. | output limit; `max_tree_tasks` | [BUD-9](../../conformance/rules.md#BUD-9) |
| usage line (`usage`) | The record of every model call, tool call and bot message with its price in micro-euros. | budget line | [BUD-3](../../conformance/rules.md#BUD-3) |
| usage reconciliation | Code's comparison of the imported provider report with the farm's usage lines, day by day. | reserve | [Models and money](../models-and-money.md) |
| workspace | An organization section at the provider with its own key and ceilings; one per tenant (D-42). | tenant; model pool | [GW-9](../../conformance/rules.md#GW-9) |

## Operations: stops, alarms, retention, erasure

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| alarm | An episode of trouble with the key `alarmkey`, produced only by code, `firing` or `resolved`; the monitoring system tells people, the farm does not (MON-1). | incident; stop | [lifecycle: alarm](../../spec/lifecycles/alarm.md) |
| auto-stop | A stop code sets by a rule from a closed list (a secret in output, spend rate); only a person removes it. | first measure of an incident | [HLD-6](../../conformance/rules.md#HLD-6) |
| backup / backup report | An encrypted copy of the store on `backup1`, reported by the external process through P2 `backup.report`. | export | [RET-5](../../conformance/rules.md#RET-5), [RET-8](../../conformance/rules.md#RET-8) |
| core drain (`core.drain`) | Before a deployment the core stops handing out work, lets attempts finish and stops. | worker drain | [LCY-6](../../conformance/rules.md#LCY-6) |
| core liveness (P2 `health`) | The endpoint the monitoring system polls with its own key; "alive" only if the store answers and every loop ticked. | `heartbeat` | [MON-2](../../conformance/rules.md#MON-2) |
| erasure request (`ErasureRequest`) | A recorded early deletion with a scope and reason that the `erasure` loop carries to every store (D-76). | subject request | [lifecycle: erasure request](../../spec/lifecycles/deletion-request.md) |
| maintenance window | A window set in the monitoring system that the core turns into a `host` stop with `pause_action` (v0.2). | trial window | [MON-9](../../conformance/rules.md#MON-9) |
| `monitor` plugin | A read-only plugin for monitoring problems, history and windows on the tenant's own hosts. | E5 adapter | [MON-6](../../conformance/rules.md#MON-6) |
| monitoring adapter (E5) | The worker of kind `monitor` that exports alarms and audit heads, imports problems and runs allowed scripts. | `monitor` plugin | [E5](../../spec/ports/e5-monitor.md) |
| monitoring problem | An open trigger in the monitoring system that enters the farm as `ext.monitor.problem.v1` with closed fields. | farm alarm | [MON-5](../../conformance/rules.md#MON-5) |
| monitoring script (`monitor/script_run`) | A built-in tool that runs an allowed monitoring script on a tenant host. | action | [MON-10](../../conformance/rules.md#MON-10) |
| monitoring system | The organization's external system that watches machines and alerts people; the farm does not repeat what it does (D-59). | the farm | [Operations](../operations.md) |
| not ready (`not_ready`) | The P1 answer while the core cannot work; no attempt is spent. | `held`; `over_capacity` | [OBS-8](../../conformance/rules.md#OBS-8) |
| over capacity (`over_capacity`) | An enqueue refusal above the tenant ceiling or below the soft space threshold. | `not_ready` | [CAP-1](../../conformance/rules.md#CAP-1) |
| restore from backup | The core's procedure until ready: new store epoch, approvals canceled, erasure log replayed, usage gap reconciled. | restore check | [RET-6](../../conformance/rules.md#RET-6) |
| retention class | `content`, `memory`, `metadata`, `tombstone` or `observation`: the default period and deletion method of an object. | data class | [RET-1](../../conformance/rules.md#RET-1) |
| service control (`service/control`) | A built-in tool that starts, stops or restarts a catalog service; only `restart` runs without a card (D-74). | action | [MON-8](../../conformance/rules.md#MON-8) |
| stop (`Hold`) | A recorded ban on part of the work: scope, effects, reason, expiry, author. It only narrows, and the core never removes it itself. | alarm | [lifecycle: stop](../../spec/lifecycles/hold.md) |
| stop effect / scope | What a stop forbids (`no_enqueue`, `no_claim`, `no_tool`, `no_action` and others) and what it applies to (`farm`, `tenant`, `role`, `worker`, `host`, `seat` and others). | erasure scope | [HLD-1](../../conformance/rules.md#HLD-1), [HLD-3](../../conformance/rules.md#HLD-3) |
| store epoch | A number that grows with every restore; work of the old epoch gets `fenced`. | seat epoch | [RET-6](../../conformance/rules.md#RET-6) |
| tombstone | The ids that remain after deletion, so ids and keys are never reused. | seal | [RET-3](../../conformance/rules.md#RET-3) |
| worker drain (`worker.drain`) | A worker finishes its running work and lets the rest go without spending attempts. | core drain; quarantine | [LCY-4](../../conformance/rules.md#LCY-4) |

## Security and privacy

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| allowed service list | The catalog services and scripts that a role may control, intersected with the initiator's own capability (D-68, D-73). | a group's permissions | [APR-18](../../conformance/rules.md#APR-18) |
| attempt journal | Model and tool traffic of an attempt as the gateways saw it; off in production unless the tenant sets a period, read only by the operator. | session; audit | [AUD-4](../../conformance/deferred.md#AUD-4) |
| audit hash chain | Each tenant's audit records chained by hash, with a head signed by the core key. | record signature | [AUD-6](../../conformance/rules.md#AUD-6) |
| audit record | An append-only record of every decision, call, delivery and admin change: ids, codes, hashes and time only. | journal; usage line | [AUD-1](../../conformance/rules.md#AUD-1) |
| `cmd` plugin | A plugin of named OS commands run through `execve` without a shell, with closed parameters. | terminal MCP server | [INJ-14](../../conformance/rules.md#INJ-14) |
| lethal trifecta | Private data, untrusted text and a path outside in one tool; the farm keeps them apart. | trust boundary | [Permissions](../permissions.md) |
| OS mechanism (`os_mechanism`) | How an action toolhost runs a write call: `oneshot` for an action or `unit` for `service/control`. | sudo; shell | [DEP-4](../../conformance/rules.md#DEP-4) |
| "personal data seen" flag (`personal_seen`) | A monotonic flag that an object may contain data about people beyond the initiator's own text. | class `pii` (there is none) | [AUD-5](../../conformance/rules.md#AUD-5) |
| privacy block (`Tenant.spec.privacy`) | The organization's processing decisions as data: purposes, controller, subprocessors, recipients, notice. | retention | [SEC-8](../../conformance/rules.md#SEC-8) |
| privacy notice (`privacy_notice`) | The code text about processing sent on first contact, in new chats and when its version grows. | AI notice | [SEC-13](../../conformance/rules.md#SEC-13) |
| processing purpose | Why the organization processes data, from `processing-purposes.yaml`; only `incident_review` changes core behavior. | task purpose | [SEC-8](../../conformance/rules.md#SEC-8) |
| production machine | Any host with the core, store, a channel, a host toolhost, tenant data or secrets; the model has no free input there. | sandbox | [DEP-1](../../conformance/rules.md#DEP-1) |
| redaction | Masking of IP addresses, emails and secrets on the toolhost before output leaves the machine. | deletion | [INJ-4](../../conformance/rules.md#INJ-4) |
| sandbox | A toolhost on a separate VM with one networkless container per task (v0.2). | test bench | [SBX-1](../../conformance/deferred.md#SBX-1) |
| secret | A key, token or password; never in P1-P4, the model, audit or logs, and every farm secret has a rotation recipe. | handle | [SEC-1](../../conformance/rules.md#SEC-1) |
| `service_grant` record | The audit record written before a service, script or routine action runs without a card; its number replaces an approval id. | approval; model grant | [APR-18](../../conformance/rules.md#APR-18) |
| subject request (`SubjectRequest`) | A person's request to export or erase their own data, decided by people with `admin:people`. | erasure request | [SEC-11](../../conformance/rules.md#SEC-11) |
| trust boundary | The boundary around the core and its store; everything outside is checked at every entry. | tenant wall | [DEP-8](../../conformance/rules.md#DEP-8) |
| untrusted seen flag (`untrusted_seen`) | A monotonic flag that text not written by the initiator may have reached the model. | `untrusted_origin` | [INJ-13](../../conformance/rules.md#INJ-13) |
| untrusted text wrapper | The per-call random marker around untrusted text that cannot be forged. | manifest wrapper | [INJ-4](../../conformance/rules.md#INJ-4) |

## Quality and conformance checking

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| bot evaluation (eval) | Cases run against the farm with a real model on a test bench, checked by code. | conformance suite | [Eval cases](../../evals/README.md) |
| canary | A visibly fake trap string in another tenant's or person's data; seeing it out of place is a failure. | rollout canary | [Quality](../quality.md) |
| case | One eval file: input, data and expectations, of kind `quality`, `refusal`, `twin`, `injection` or `isolation`. | governance case | [Eval cases](../../evals/README.md) |
| conformance profile | A part of the skeleton an implementation claims separately (`core`, `messaging`, `campaign`, `insights`, `manager`, `governance`, `monitor`). | role profile | [MAN-8](../../conformance/rules.md#MAN-8) |
| conformance suite | Fixtures, suites and the runner contract in `conformance/` that any implementation goes through. | eval | [Quality](../quality.md) |
| expected failures | An implementation's list of waivable rules it does not keep yet. | `rejected` | [Quality](../quality.md) |
| first bot (`hello-bot`) | The smallest bot in `examples/hello-bot/`, where the first implementation starts. | example flow | [Examples](../../examples/README.md) |
| judge | A model on a different model than the bot that grades an eval answer and can only add failures. | commission reviewer | [Quality](../quality.md) |
| mock model | A scripted stand-in for the provider that does what an attacker would want. | eval model | [Quality](../quality.md) |
| non-waivable rule | A rule with `waivable: false` that cannot be an expected failure. | `SHOULD` | [Quality](../quality.md) |
| rule registry | `conformance/requirements.yaml`, one row per rule number, never reused. | decision log | [Quality](../quality.md) |
| runner | The implementation's black-box program that runs the suite and writes JUnit XML. | eval script | [Runner contract](../../conformance/runner.md) |
| test bench | A farm instance without production tenants on its own machine, controlled through the test-control port. | sandbox; test tenant | [APR-9](../../conformance/rules.md#APR-9) |

## Skeleton versions

| Term | Meaning | Not to be confused with | Where |
|---|---|---|---|
| manifest API version (`apiVersion`) | `botaru/v1alpha1` before 1.0.0, then `botaru/v1` (D-44). | type version | [MAN-1](../../conformance/rules.md#MAN-1) |
| skeleton version | A content slice, v0.1, v0.2 or v0.3 (D-113), that an implementation claims in `conforms_to`. | repository version | [MAN-8](../../conformance/rules.md#MAN-8) |
| step | Used alone, another name for a skeleton version: step 1 is v0.1. | script step | [Architecture](../architecture.md) |
| type version (`.vN`) | The suffix of a type, event or schema name; a breaking change is a new `.v(N+1)`. | `apiVersion` | [MAN-3](../../conformance/rules.md#MAN-3) |

## Words that are easy to mix up

- **Worker, bot and external program.** A process that takes work; an identity whose tasks workers carry out; a principal of kind `service` with its own key (D-47).
- **Role, role profile and position.** What workers do; a closed narrowing of it; what the core derives to pick a persona and rank ceiling. Only groups grant permissions.
- **Manager and farm manager.** A tenant bot that fills the pool; the single seat that prevents incidents across the farm.
- **Task, attempt and runtime run.** The unit of work; one handout under a lease (at most 3); one model loop inside an attempt (at most 6 counted).
- **Change trial and promotion probation.** 14 days of measuring a change; 14 days without a confirmed attribution before it can promote its author.
- **Stop, alarm and incident.** A ban on work; a report of trouble; a core record opened from an alarm and a class.
- **`not_ready`, `held` and `over_capacity`.** The core cannot work now; a stop forbids the work; there is no room to enqueue.
- **Store, seat and bot epoch.** They grow on a restore, a seat change and a binding change; stale work gets `fenced`.
- **Data, retention, priority and incident class; rank and tier.** Sensitivity, storage period, handout order, what to do on an alarm; model strength and what a task may do.

## What is not in the model

- No record evaluates a person (DLV-14), and bots have no permissions of their own (IDN-9).
- An attempt never waits; a person's answer is a new task (D-41).
- No main tenant (D-23), and nothing crosses the tenant wall (TEN-5).
- A message grants nothing and enqueues nothing; only routes from git do (MSG-16).
- No alarm delivery of the farm's own: the monitoring system does it (D-59).
- No payment model (D-12), no model names in rules (D-05), no automatic switch of key or provider.
- Nothing undoes a carried-out action automatically.
- No free input on a production machine (INJ-1), and the model does not change itself (INJ-10).
