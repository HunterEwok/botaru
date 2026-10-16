# Examples

## In short

The `examples/` folder shows the skeleton at work on the sample tenant `demo`: manifests, task bodies and flows of port operations from a person's request to what that person sees in the channel. The examples are not normative: the rules live in the registry `conformance/requirements.yaml` and in the `docs/` documents, and an example only shows how they fit together. But the examples pass the manifest lint `ci/check_manifests.py` (the same checks that an implementation's `farmctl apply` performs) and the `public-safety` check, so an example that breaks a rule fails CI. The skeleton contains no farm code: the implementation writes the core, `farmctl` and the runner, so the examples here are checked only for form. Start with [action-pipeline-ctd-qc.md](action-pipeline-ctd-qc.md): it walks through one action end to end, from a manifest line to a systemd unit. Then read [flows.md](flows.md): twelve short flows with different task types. How the examples serve quality checks is described in [docs/quality.md](../docs/quality.md).

## What is in the folder

| Path | What |
|---|---|
| `README.md` | this file: contents, people and machines, how to read |
| `action-pipeline-ctd-qc.md` | the action `pipeline.ctd_qc` end to end: manifests, the card, what will not pass, the executor on the pipeline machine |
| `flows.md` | twelve flows in words: who asks, which task type, which port operations, what people see |
| `flows/<name>/` | the machine form of flows 1-12 (flows 2 and 4 are v0.2 and need the `sandbox` profile): manifests, task bodies, expected events and audit lines; `ci/check_manifests.py` now checks the P1 request bodies and call arguments, and the implementation's runner will replay a flow against a live core |
| `hello-bot/` | the smallest bot of the `core` profile (D-71): a role, a task type with only the required fields, a group and the expected sequence as a fixture envelope (`submit_activity`, the `ai_notice` and `privacy_notice` deliveries on first contact, `claim` -> `heartbeat` -> `complete`); the core delivers the answer to the person by the task `origin`, and there are no messages between bots; the command `/hello` is declared in `tenants/demo/tenant.yaml`; the black-box runner will replay it against the mock model once the runner arrives with the implementation ([docs/quality.md](../docs/quality.md)); for now `ci/check_manifests.py` checks the `call` bodies of the sequence against the operation request schemas |
| `tenants/demo/` | the tenant's manifests: `tenant.yaml`, `groups.yaml` (`Group` objects), `catalog.yaml`, `routes.yaml`, `schedules.yaml`, `sinks.yaml`; from v0.2 the source channel `channels-ingress.yaml` (profile `ingress`, binding `spec/bindings/channels/webhook.md`) and the curated corpus documents in `knowledge/` (profile `knowledge`). Group chat bindings are not in git: the operator creates them with the P2 command `chat.bind` (PRM-14) |
| `tenants/demo2/` | a second tenant for isolation examples; it has `farm_manager.enabled: false` (D-77) |
| `tenants/farm/` | the charter of the service tenant `farm` with the farm manager seat profile `farm_manager.model_profile` (GOV-33); `apply` rejects a tenant with the manager enabled when no farm charter sets it (`manager_profile_missing`) |
| `deploy/farm.yaml` | a deployment description following the `deploy-topology` schema: `farm_hosts: [core1, backup1]`, the processes on the machines, the OS users of executors per tenant (`os_users`, DEP-10); the file is not run, and the manifest lint takes the farm nodes from it |
| `roles/`, `tasktypes/`, `tools/`, `messagetypes/`, `actions.yaml`, `model-profiles/` | global manifests, shared by all tenants and without their data (TEN-6); among the types `kb.ingest.v1` (v0.2, KNW-4) and `manager.postmortem.v1` (v0.3, MGR-53) |
| `skills/` | from v0.2 a role skill in the Agent Skills format, `pipeline-tmp-cleanup`, pinned by its tree hash in `roles/ops-reader/role.yaml` (SKL-1, SKL-2) |

## People, the bot and the machines

All names, addresses and numbers are made up. The people's names are training names: in core records a person is stored by the opaque principal id `human:p_<ULID>`, and the runner maps the name to the id from the test bench data (D-85).

| Who | Groups in `demo` | Who this is in the examples |
|---|---|---|
| `alice` | `user`, `researcher`, `ctd`, `lead`, `admin` | lead of the CTD data group and tenant admin: requests quality control and the monthly report, and as a researcher works with geodata and finding tables and writes in the group chat |
| `bob` | `user`, `staff`, `approver`, `ops-oncall` | sysadmin: disk, logs, the night outage, CTD file descriptions; on call with the permission for `nginx` (D-73) |
| `carol` | `staff`, `approver`, `ctd-lead`, `runs-lead` | approves pipeline actions and cleanups of temporary directories |
| `erin` | `user`, `runs`, `assist` | lab technician; she has a personal assistant |
| `system` | `system-ops` | the system principal: schedule and route tasks are enqueued on its behalf (PRM-10) |

The tenant's Telegram bot is called `@demo_bot`. The operator bound the lab group chat `lab` with the ceiling `summary`, and the group chat `staff` with the ceiling `internal` (its driver declares `membership_assurance: enforced`).

| Machine | What runs on it |
|---|---|
| `core1` | farm node: the core, the store, the channels of all tenants, the `audit-head` loop; it holds no tenant resources and does not appear in the catalogs of `demo` and `demo2` (DEP-11) |
| `backup1` | second farm node: store backups, restore from `restore/`; also in `farm_hosts` |
| `web1` | machine of `demo`: the site `www.example.org` (nginx) and the service `app-api`; the read toolhost of `demo` with the plugins `logs`, `cmd`, `metrics`, `checks`; the action toolhost of `demo` with the write plugin `service` (the `unit` mechanism for `nginx` and `app-api`); the sink `checks`; the monitoring scripts `app-api-restart` and `nginx-cache-clear` |
| `db1` | machine of `demo`: the data directory `/data`, the sensor database and the service `postgresql`; the read toolhost of `demo` with the plugins `cmd`, `logs`, `metrics`, `storage`, `geo`; the action toolhost of `demo` with the write plugin `pipeline`; the job worker of `demo`; `postgresql` is not declared in the service catalog, and bots do not touch it |
| `ctd1` | machine of `demo`: the CTD quality control and lab run pipelines; the read toolhost of `demo` with the plugins `batch` and `data` (the tool `data/describe`, D-72), a separate action toolhost of `demo` with the write plugin `pipeline` and the job worker of the watcher `pipeline.watch.v1`; the service `ctd-qc` is in the catalog without `no_card` |
| `sbx1` (sandbox VM) | from v0.2 and only with the `sandbox` profile: the sandbox toolhost of `demo` (`placement: sandbox`) with the plugin `sandbox` (`dataset_get`, `run`, `list_out`) for the role `analyst` and the type `data.analyze.v1`; one container per task without network and secrets, and the VM holds no production data (DEP-1, SBX-1, SBX-2). The single-server example `deploy/single.yaml` has no sandbox VM and no tier 2 (D-104) |

Monitoring stations in the catalog of `demo`: `st-01`, `st-02`, `st-05`, `st-07`, `st-09`. Each machine has its own action toolhost, which runs no read plugins (DEP-9). A machine belongs to one tenant (D-75): `web1`, `db1` and `ctd1` belong only to `demo`, `demo2` has its own machines, and the farm nodes `core1` and `backup1` belong to nobody.

## Tenant `demo` in manifests

Every manifest sits in the wrapper `apiVersion`, `kind`, `metadata`, `spec`; until release 1.0.0 the version is `botaru/v1alpha1` (D-44, MAN-1). The text of the examples sometimes shows only `spec` in fragments, but the files always have the wrapper. The manifest fields are described in [docs/task-language.md](../docs/task-language.md).

```yaml
# tenants/demo/tenant.yaml (fragment)
apiVersion: botaru/v1alpha1
kind: Tenant
metadata: {name: demo}
spec:
  kind: production              # production tenant; a test tenant is written kind: test
  timezone: Europe/Berlin       # time zone of schedules, budget windows and texts to people (TIM-2)
  self_approve: false           # for a production tenant apply rejects any other value (APR-9)
  limits: {max_queued: 5000, max_enqueue_per_hour: 2000, max_storage_bytes: 21474836480}   # 20 GiB
  budgets: {month_eur: 300, day_eur: 15, hour_eur: 3}   # tenant ceilings (BUD-1); role lines sit inside them
  attention: {cards_per_tenant_hour: 20}                # above this, cards get service_rate_limited (APR-2)
  delegated_groups: [researcher]                        # groups the team lead grants from the channel (PRM-16): only without actions and decisions
  farm_manager: {enabled: true}   # D-77, MGR-42
  link_allowlist: [www.example.org, docs.example.org]
  models_allowed: [base-std, analysis-std, review-a, review-b, seat-std, seat-alt, manager-std, translate-std]
                                # seat-std, seat-alt, manager-std, translate-std are needed by the commission, the manager and the translator
  privacy:                      # privacy block (SEC-8); all fields in docs/operations.md
    purposes:
      - {purpose: operations, lawful_basis: legitimate_interest}
      - {purpose: assistance, lawful_basis: consent}
      - {purpose: data_processing, lawful_basis: legitimate_interest}
      - {purpose: incident_review, lawful_basis: legitimate_interest}
    subprocessors:
      - {provider: provider-a, agreement_ref: "contracts/provider-a-dpa", region: region-a, retention: {mode: standard, max_days: 30}}
    recipients: [monitoring]    # the organization's monitoring system receives notes with ceiling summary
    channels:
      - {driver: telegram, third_party_storage: true, ceiling: internal}
    notice_ref: https://docs.example.org/botaru/privacy
    notice_version: 3           # the notice is delivered on first contact and when the number changes (SEC-13)
  commands:                     # command -> task type; the text after the command is the goal
    "/logs": ops.investigate.v1
    "/health": ops.health.v1
    "/geo": geo.describe.v1
    "/pipeline": pipeline.status.v1
    "/describe": data.describe.v1   # a file in the message goes as an attachment (D-72)
    "/a": assist.chat.v1            # the assistant only by command: it is not in default_types (DSP-10)
    "/hello": hello.greet.v1        # the first bot, examples/hello-bot/
    # /lang, /service, /tasks, /status, /cancel, /help, /me, /team are handled by the channel itself (PRM-15, PRM-17, PRM-18)
  run:                          # /run <name> <arguments>
    ctd-qc: {action: pipeline.ctd_qc, args: [month, stations]}            # writes on ctd1: always a card
    batch-resume: {action: pipeline.batch_resume, args: [run, profile]}
    month-report: {type: report.monthly.numbers.v1, args: [month]}       # run.approval: none, no card
  default_types:                # free text: the first task type that fits the chat and is allowed to the sender
    - {type: ops.summary.v1, chats: [direct]}
    - ops.summary.v1
```

```yaml
# tenants/demo/catalog.yaml (spec fragment: machines, services and monitoring)
hosts:                           # the tenant's machines; core1 and backup1 cannot be listed here (DEP-11)
  - id: web1
    monitor_host: web1.example.org
    monitor_scripts_allowed:     # names for the model come only from here (INJ-16)
      - {scriptid: "17", name: app-api-restart,  hosts: [web1.example.org]}
      - {scriptid: "21", name: nginx-cache-clear, hosts: [web1.example.org], choices: [all, static]}
    services:
      - {name: nginx,   unit: nginx.service, control: [start, stop, restart], via: host, no_card: [restart]}   # only restart without a card (D-74)
      - {name: app-api, control: [restart], via: monitor, health: api-health, scripts: {restart: app-api-restart}}
  - id: db1
    monitor_host: db1.example.org                       # no services: bots do not touch postgresql
  - id: ctd1
    monitor_host: ctd1.example.org
    services:
      - {name: ctd-qc, unit: ctd-qc-runner-demo.service, control: [start, stop, restart], via: host, no_card: []}   # everything by card
monitor_triggers:                # the problem class by triggerid, not by its name
  - {triggerid: "4401", class: disk_full,    item: vfs.fs.size}
  - {triggerid: "4407", class: service_down, item: proc.num}
```

Catalog tools take the machine from the task or call field that the manifest declares: `cmd/run` has `target_from: host`, and `host` must be a key of the catalog's `hosts`; `data/describe` has `target: any` and the attachment by `enum_from: task.attachments`. The model does not see a script or trigger name that is missing from `monitor_scripts_allowed` or `monitor_triggers`, and a call with a foreign `scriptid` gets `script_unknown` (INJ-16).

```yaml
# deploy/farm.yaml (fragment)
farm_hosts: [core1, backup1]      # farm nodes: core, store, channels, backups (DEP-11)
hosts:
  ctd1:
    os_users:                     # OS users of action executors per tenant (DEP-10)
      ctd-qc-run: {demo: botaru-ctd-demo}
```

```yaml
# tenants/demo2/tenant.yaml (spec fragment)
kind: production
timezone: Europe/Berlin
self_approve: false
farm_manager: {enabled: false}    # no manager: the manager line is not needed, doctor shows manager_disabled (D-77)
budgets: {month_eur: 60, day_eur: 4, hour_eur: 1}
```

```yaml
# tenants/demo/groups.yaml (fragment: Group objects separated by ---, shown here as the group name and its spec)
user:                             # as in docs/permissions.md
  caps: ["task:ops.summary.v1", "task:ops.health.v1"]
  data: [summary, internal]
  day_eur: 1.0
lead:                             # reports that show money
  caps: ["task:report.monthly.v1", "task:report.monthly.numbers.v1"]
  data: [summary, internal]
assist:                           # people with a personal assistant
  caps: ["task:assist.chat.v1"]
  data: [summary]
staff:
  inherits: [user]
  caps: ["task:ops.investigate.v1", "task:data.describe.v1", "action.request:cleanup_pipeline_tmp"]
  data: [raw_logs]
  day_eur: 5.0
ops-oncall:                       # on-call staff: the /service command and service requests (D-73)
  caps: ["service.control:nginx", "monitor.script_run"]
  data: [summary]
approver:
  caps: ["action.approve:cleanup_pipeline_tmp", "control.approve",
         "action.approve:service.control", "action.approve:monitor.script_run"]   # service cards when there is untrusted text
  data: [summary]
admin:
  caps: ["admin:people", "hold.set:tenant", "hold.release", "proposal.decide:schedule", "control.approve",
         "task:report.monthly.v1", "task:report.monthly.numbers.v1"]
  data: [summary]
ctd:
  caps: ["task:pipeline.status.v1", "action.request:pipeline.ctd_qc"]
  data: [summary, internal]
ctd-lead:
  caps: ["action.approve:pipeline.ctd_qc"]
  data: [summary, internal]
runs:
  caps: ["task:pipeline.status.v1", "action.request:pipeline.batch_resume"]
  data: [summary, internal]
runs-lead:
  caps: ["action.approve:pipeline.batch_resume"]
  data: [summary, internal]
researcher:                       # in the tenant's delegated_groups: no actions and no decisions (PRM-16)
  caps: ["task:geo.describe.v1", "task:data.describe.v1"]
  data: [summary, internal]
system-ops:                       # only for the principal system: exactly the task types of schedules and routes
  caps: ["task:data.sensor_check.v1", "task:data.sensor_watch.v1", "task:report.sensor.v1", "task:check.http.v1",
         "task:pipeline.watch.v1", "task:pipeline.status.v1", "task:pipeline.ctd_qc.report.v1",
         "task:report.monthly.numbers.v1", "task:report.monthly.v1",
         "service.control:nginx"]   # nginx restart by a route task on a monitoring problem (D-82)
  data: [summary, internal]
  day_eur: 3.0
```

Action permissions are granted explicitly, without wildcards (PRM-7). `apply` computes the `privileged` flag (PRM-13): here `user`, `lead` and `assist` are ordinary groups, and the rest are privileged because they contain actions, decisions, `admin`, a class above `internal` or a task type of tier 1 or higher. Only the operator grants privileged groups, through `farmctl`, and an invitation in the channel grants only ordinary ones. Reports that show money are available only to `admin` and to the group of leads that the tenant declares (here `lead`), as in [docs/permissions.md](../docs/permissions.md): `user` has no wildcard `task:report.*` that would open them to everyone. A child task goes through the initiator's permissions, so `lead` and `admin` also have `task:report.monthly.v1`: the monthly report job enqueues it on behalf of the initiator (PRM-6). Only `assist` has the permission `task:assist.chat.v1`: free text in a private chat becomes an assistant task only for those who have it, and for everyone else it is `ops.summary.v1` (IDN-6). A service permission is needed on both sides (APR-18, D-73): for the bot's role in the manifest and for the task initiator in a group (`service.control:<service>`, `monitor.script_run`; here `ops-oncall`). The principal `system` enqueues a route task for a problem from the monitoring system, so the line `service.control:nginx` is also in `system-ops`: a problem with a trigger from the catalog does not set the untrusted flag, and the `nginx` restart for it goes without a card (D-82, INJ-13). The catalog of `demo` declares the managed services (`nginx` and `app-api` on `web1`, `ctd-qc` on `ctd1`; `postgresql` on `db1` is not in the catalog) and for each a `no_card` list that may contain only `restart` (D-74): everything else always goes by card. Roles get their permissions in their own manifests:

```yaml
# roles/ops-reader/role.yaml (spec fragment)
service_control: [nginx]                 # catalog services the role may touch
monitor_scripts: [nginx-cache-clear]     # names only from the catalog's monitor_scripts_allowed
```

```yaml
# roles/data-reader/role.yaml (the whole file): the second bot of the core profile (D-72)
apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: data-reader}
spec:
  profile: worker
  worker_kind: agent
  claims: [data.describe.v1]
  tools: [data/describe]
  models: {base: base-std}
  memory: none
```

```yaml
# roles/farm-manager/role.yaml (spec fragment): the farm manager
profile: farm-manager
monitor_scripts: ["*"]                   # all names in the tenant's catalog, but not foreign ones and not those of farm nodes
```

The group `approver` decides the cards that such requests turn into when the bot read untrusted text before them (D-70), and the cards of operations outside `no_card` (D-74).

```yaml
# model-profiles/seat-std.yaml and seat-alt.yaml (spec fragments): two profiles a seat may use; the lenses need no different families (D-117)
# seat-std
provider: provider-a
family: family-a
tokens_per_s: 60                         # for estimating duration and timeouts (GW-12)
price: {input: 3.00, output: 12.00, checked: 2026-10-01}   # GW-17: no older than 90 days
provider_training: {value: false, checked: 2026-10-01}
provider_human_review: {value: abuse_only, checked: 2026-10-01}
subprocessors: []
# seat-alt
provider: provider-b
family: family-b
tokens_per_s: 45
price: {input: 3.00, output: 12.00, checked: 2026-10-01}
provider_training: {value: false, checked: 2026-10-01}
provider_human_review: {value: abuse_only, checked: 2026-10-01}
subprocessors: [{provider: provider-c, region: region-a}]
```

```yaml
# tenants/demo/governance.yaml (fragment of spec.commission): only the charter declares commission seats (GOV-33);
# every member represents one lens with the persona addendum of that lens (COM-19); all seats run on seat-std
commission:
  seat_role: commission-member
  model_profile: seat-std
  seats:
    - {id: 1, lens: security, fallback_role: reviewer-a}
    - {id: 2, lens: operational, fallback_role: reviewer-a}
    - {id: 3, lens: cost, fallback_role: reviewer-b}
    - {id: 4, lens: value, fallback_role: reviewer-b}
  lenses:
    security: {persona: {ref: personas/lens-security.md, sha256: "..."}}
    operational: {persona: {ref: personas/lens-operational.md, sha256: "..."}}
    cost: {persona: {ref: personas/lens-cost.md, sha256: "..."}}
    value: {persona: {ref: personas/lens-value.md, sha256: "..."}}
```

Groups carry no alarm permissions: the organization's monitoring system passes farm alarms to people, and it decides who receives and acknowledges them (MON-1).

## How to read the examples

- **Task body and task record.** The sender writes the task body: four fields, `type`, `goal`, `params`, `attachments`. Only the core writes the task record: tenant, initiator, `origin`, conversation, `max_tier`, `data_mark`, `untrusted_seen` (IDN-1). The examples show the record on a separate line.
- **Type names.** A task type name always carries the version `.vN`: `ops.investigate.v1` (MAN-3). The permission `task:ops.investigate.v1` grants this version, and `task:ops.investigate.*` grants all versions of the family (PRM-7). The core names an action task after the action: `action.cleanup_pipeline_tmp` (APR-5).
- **Tiers.** 0 summaries, 1 reading with narrow tools on a production machine, 2 a sandbox on a separate VM (from v0.2), 3 an action only after a person's button. A bot with `service_control` or `monitor_scripts` in its role and `service.control:<service>` or `monitor.script_run` for the initiator (D-73) restarts a service without a button only if the service is in the catalog's `no_card` (D-74); `stop`, `start`, other operations, scripts outside a person's request and any call with untrusted text in the task go by card (D-70). Actions of class `routine` (`writes_only`, `idempotent`, `runs_per_hour`, `runs_per_day`, D-78) go without a card within their counters.
- **Data classes.** `summary` < `internal` < `raw_logs`. A task's mark is no lower than the class of its task type and does not drop along the chain (PRM-5).
- **Time.** Records, keys and contracts use UTC (TIM-2); texts to people use the tenant's time zone, the sample `Europe/Berlin`; where the zone does not matter, the examples write `UTC` (D-85).
- **Port operations** are written as P1 `claim` or P2 `apply`. The ports are described in `spec/ports/`.
- **Bot answers** in the examples are made up and show the form, not exact numbers.
- **Assistant.** `assist.chat.v1` is enqueued only by the command `/a` (for `assist`) and is not in `default_types`; the assistant is a pool with an `owner` lease for a person (DSP-10).

## Other walkthroughs on `demo`

The documents of the farm parts walk through their mechanisms on the same tenant:

| Document | What it walks through |
|---|---|
| [docs/permissions.md](../docs/permissions.md) | inviting people, the operator granting groups, an action request with an approval card |
| [docs/messaging.md](../docs/messaging.md), [docs/coordination.md](../docs/coordination.md) | the bot group `science-pool`, bot messages, a quality control campaign |
| [docs/coordination.md](../docs/coordination.md) | a translator script draft, the script confirmation card and the steps |
| [docs/improvements.md](../docs/improvements.md) | a proposal to move the nightly report: analysis, commission, a trial with the outcome `better`, audit sample items |
| [docs/bots.md](../docs/bots.md), [docs/improvements.md](../docs/improvements.md) | the identity `bot_17` from enrollment to a commission seat, a case where code proposes who leaves and a person chooses (D-115) |
| [docs/farm-manager.md](../docs/farm-manager.md) | incidents `i-311` and `i-318`: the manager seat, holds and cards to people |
| [docs/operations.md](../docs/operations.md) | the privacy block, `/mydata`, an erasure request, a plugin hold |

## What goes into which version

| Version | What appears |
|---|---|
| v0.1 | `tenants/demo/`, `tenants/demo2/`, `deploy/farm.yaml`, the global example manifests of the `core` profile (D-71); `hello-bot/`; the role `data-reader` and flow 12 (D-72); the example action `pipeline.ctd_qc`; the plugins `geo`, `batch` and `service`; services, `monitor_scripts_allowed` and `monitor_triggers` in the catalog of `demo`, the permissions `service_control` and `monitor_scripts` of role `ops-reader`, the group `ops-oncall`; flows 1, 3, 5, 6, 7, 8, 9, 10, 11 and 12 in machine form, flows 2 and 4 in words; after the core and the first bot (D-71): `farm_manager` of the tenants, the role `farm-manager`, the profiles `seat-std`, `seat-alt`, `manager-std`, the commission charter of `demo`; the machine form of the campaign and translator script flows, which [docs/coordination.md](../docs/coordination.md) walks through in words |
| v0.2 | flows 2 and 4 in machine form together with the sandbox (`flows/02-ctd-sandbox/`, `flows/04-findings-table/`, the plugin `sandbox`, the role `analyst`, the type `data.analyze.v1`, the command `/analyze` and the VM `sbx1` in `deploy/farm.yaml`, profile `sandbox`); an action with `risk: high` and two approvers (APR-16) |

## Why it is this way

- One sample tenant and the same people in all examples: the reader learns them once, and `public-safety` checks a simple list of allowed names.
- The examples pass the manifest lint: an example that quietly drifted from the rules breaks CI instead of misleading whoever writes the implementation.
- The machine form of flows in `flows/<name>/` turns an example into a check: for now the manifest lint checks its bodies, and the implementation's runner will replay the flow against a live core and compare events and audit.
