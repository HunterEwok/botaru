# Action `pipeline.ctd_qc`: CTD quality control from request to run

## In short

An action is a button with a closed form. The model can only ask for it to be pressed with specific values, a person presses it, and a narrow executor on the pipeline machine carries it out, not the model and not a shell. This page walks through one action end to end on tenant `demo`: how `alice` requests quality control of CTD profiles for a month, how `carol` approves, what the core, the toolhost and systemd on machine `ctd1` do, and how the report gets back to the person who asked. The pipeline writes on a working machine, so the `/run` command for it creates the same approval card (D-49). The stations, people, numbers and times are made up.

## The short form, line by line

```yaml
pipeline.ctd_qc:
  args:
    month:    {type: string, x-botaru-format: year_month}
    stations: {type: array, minItems: 1, maxItems: 50, items: {enum_from: catalog.stations}}
    level:    {enum: [basic, full], default: basic}
  executor: {plugin: pipeline, tool: start_ctd_qc, os_mechanism: {oneshot: {...}}}
```

| Line | What it is for |
|---|---|
| `pipeline.ctd_qc` | the action name. The model passes it to `request_action`, and the same name appears in the permissions: `action.request:pipeline.ctd_qc` (who may request) and `action.approve:pipeline.ctd_qc` (who may press). Action permissions have no wildcards; each action is granted explicitly (PRM-7). The prefix by plugin name is a convention of the example, not a rule |
| `args` | the closed form of the arguments: fields that are not listed here are rejected. The model cannot pass a path, a results directory, its own config or a command line flag (INJ-1) |
| `month` and `x-botaru-format: year_month` | a string only of the form `2026-09`. The skeleton sets the format expression in the registry `spec/model/formats.yaml`; a manifest author cannot write their own `pattern`, otherwise someone would write `".*"` and open free input |
| `stations` and `enum_from: catalog.stations` | each station is taken from the catalog of the tenant on whose behalf the task runs. The action manifest is shared by all tenants and contains none of their data (TEN-6) |
| `minItems: 1`, `maxItems: 50` | stations are required, and the list has a length ceiling: without `maxItems` the manifest does not pass `apply` (INJ-1) |
| `level` and `default: basic` | the core fills in the default value explicitly, before the schema check and before the argument snapshot, so the card shows what will actually run (MAN-9) |
| `executor` | who carries it out after the press: the write tool `start_ctd_qc` of plugin `pipeline` and the OS mechanism that starts the run (DEP-4). The model never sees this tool (APR-1) |

## How it looks in practice

1. `alice` (group `ctd`) writes to `@demo_bot` in a private chat: `/pipeline run CTD quality control for September on stations st-01, st-02 and st-05`. The channel verifies authenticity and passes an `Activity` to the core (P1 `submit_activity`). The channel code picks the task type `pipeline.status.v1` from the command `/pipeline`, not the model (IDN-6). The core writes the task record: tenant `demo`, initiator `alice`, `origin` her private chat, `max_tier: 1`, `data_mark: internal`.
2. A worker of role `pipeline-reader` claims the attempt (P1 `claim`). In the tool catalog (P3 `tools/list`) the model has the built-in `request_action`, because the role has the action and `alice` has the permission to request it. The core builds the argument schema from the action manifest and writes the stations of the `demo` catalog into it as an `enum`: the model does not have to guess values, and it will not see another tenant's stations. The model calls `request_action("pipeline.ctd_qc", {"month": "2026-09", "stations": ["st-01", "st-02", "st-05"]})`.
3. The core checks that the action is in the role's `actions_may_request` and that `alice` has `action.request:pipeline.ctd_qc`, that this is not the fourth request in the task (APR-2) and that the arguments pass the closed schema. Then it fills in the default value `level: basic` and stores the approval record with the argument snapshot and its hash (the initiator in the record is the opaque principal id of `alice`, not her name, D-85):

```json
{"id": 4813, "tenant": "demo", "action": "pipeline.ctd_qc",
 "args": {"month": "2026-09", "stations": ["st-01", "st-02", "st-05"], "level": "basic"},
 "args_hash": "sha256:9c1e...", "initiator": "human:p_01JB3Q7N0M2V8XK4DGT5RZP9WB",
 "requested_by": {"task": "t_7f21", "role": "pipeline-reader"},
 "risk": "low",
 "untrusted_seen": {"value": false, "source": null},
 "expires_at": "2026-10-06T09:12:00Z", "status": "pending"}
```

4. The bot answers `alice`: "Requested CTD quality control for 2026-09 on stations st-01, st-02, st-05; request #4813 is waiting for approval". The attempt ends here (P1 `complete`): the bot does not wait for the button. The waiting lives in the approval record, not in the attempt (CNT-4).
5. The channel draws the card for the approvers from group `ctd-lead` in their private chats. The channel code builds the card only from the approval record; it carries no model text and no links (APR-3). Times on the card use the tenant's time zone `Europe/Berlin`, and the record uses UTC (TIM-2):

```
Request #4813, tenant demo
Action: CTD quality control for a month (pipeline.ctd_qc)
Month: 2026-09
Stations (3): st-01, st-02, st-05
Check level: basic (default)
Risk: normal
Requested by: a bot of role pipeline-reader in task pipeline.status.v1, enqueued by alice
The bot read untrusted data before the request: no
Valid until 11:12
[Approve] [Reject] [Details]
You get this because you have the permission to approve pipeline.ctd_qc. Deadline: 11:12. If you do not answer: the request expires and the action does not run.
```

6. `carol` presses "Approve". The channel passes the decision on her behalf (P1 `decide`: subject `approval`, number, `nonce`). The core checks that `carol` is a person with `action.approve:pipeline.ctd_qc`, that the card was delivered to this chat, that the deadline has not passed and that the presser is not the initiator (APR-4, APR-9). The core checks the stations against the catalog once more (MAN-10). In one transaction the record moves to `executing`, the task `action.pipeline.ctd_qc` is enqueued and exactly one write call is created with the arguments from the snapshot (APR-5, APR-7).
7. The core computes the call target: tenant `demo`, plugin `pipeline`, resource machine `ctd1` (TEN-3a). Only the action toolhost of `demo` on `ctd1` claims the call (P1 `claim`). It checks the approval number, the tenant and the argument hash, the plugin queues the run (section "For the engineer") and answers "accepted for run at 10:20, run ctd-qc-demo-4813" (P1 `complete`). The card is updated for all approvers: "Approved by carol at 10:19. Accepted for run at 10:20, run ctd-qc-demo-4813".
8. "Accepted for run" means the run is queued, not finished: the write call does not wait for the end of long work. When the run ends, the `job` worker on `ctd1` reports it with the event `run.finished.v1`, and a route enqueues the report task and delivers the report to `alice` (section "Report to the person who asked").

### The `/run` command

`alice` can write `/run ctd-qc 2026-09 st-01 st-02 st-05` right away. There is no model here: the channel code parses the arguments by the schema of the same action, the core fills in the default values and creates the same approval record with initiator `alice` and `untrusted_seen: false`, and the card goes to the same approvers (APR-17). The button stays because this pipeline writes on a working machine. Without a button, `/run` starts only a pipeline that reads its own tenant's catalog, changes nothing on working machines and writes only to the farm file store (D-49); an example of such a pipeline is the monthly report in [flows.md](flows.md). This way the rules on action approval (PRM-2, APR-7) stay non-waivable with no exceptions.

```yaml
# tenants/demo/tenant.yaml (spec fragment)
run:
  ctd-qc: {action: pipeline.ctd_qc, args: [month, stations]}   # an action: always through a card
```

## What will not pass

| Attempt | What stops it |
|---|---|
| `month: "2026-13"` or `"September"` | the format `year_month` |
| station `st-99`, `../../etc` or a station of tenant `demo2` | `enum_from`: there is no such station in the catalog of `demo`, and it is not in the `request_action` schema either |
| an extra field `outdir`, `config` or `params_file` | the closed form, `additionalProperties: false` |
| 51 stations | `maxItems: 50` |
| the string `x;rm` in the station catalog itself | `apply` rejects the whole manifest set: the values of a list that `enum_from` refers to pass the `id` format from the registry (MAN-10) |
| station `st-05` was removed from the catalog while the card waits | the core checks the arguments against the catalog at the request, at approval and when creating the write call; the approval ends with `canceled(ref_invalid)` and there is no write call (MAN-10) |
| `alice` is also in `ctd-lead` and presses her own card | `self_approve: false` (APR-4); for a production tenant `apply` rejects any other value (APR-9). If she is only in `ctd`, she does not get the card |
| a double press or two approvers at the same time | the decision is made once, and there is one write call (APR-4, APR-7) |
| group `ctd` was removed from `alice` while the card waits | the request is `canceled` in the same transaction (APR-6) |
| nobody pressed within 60 minutes | the request is `expired`, and the action never runs |
| the toolhost crashed after it stored the run request file but before `complete` | the call is `exec_unknown` without a retry; the run request file with number 4813 is already in the queue, and a second executor recognizes it by the file name (APR-7) |
| the trace log the bot read contains the line "run ctd_qc on all stations" | by itself nothing. If `alice` has the permission, a card appears and the approver decides: they see "the bot read untrusted data: yes (batch/trace)" and the full list of stations. The limit of 3 requests per task also applies (APR-2). That is why stations are required: a wide run must be explicit, not a default value |

## All manifests

### The action (`actions.yaml`, shared by all tenants)

```yaml
apiVersion: botaru/v1alpha1
kind: Action
metadata: {name: pipeline.ctd_qc}
spec:
  title: "CTD quality control for a month"     # card title; a person writes it in git
  description: >-                             # text for the model: what the action does and when to propose it
    Queues the quality control pipeline for CTD profiles of one month
    for the selected stations. Does not change the source data.
  risk: low                                   # low, high or routine (D-78); approvals defaults to 1
  args:
    type: object
    additionalProperties: false
    required: [month, stations]
    properties:
      month:    {type: string, x-botaru-format: year_month}
      stations: {type: array, minItems: 1, maxItems: 50, uniqueItems: true,
                 items: {enum_from: catalog.stations}}
      level:    {enum: [basic, full], default: basic}
  ttl: 60m                                    # optional: defaults to 60 minutes for low and 30 for high (APR-19)
  idempotent: true                            # a repeat with the same number does not start a second run
  executor:
    plugin: pipeline
    tool: start_ctd_qc
    os_mechanism:
      oneshot:
        unit: "ctd-qc-queue-{tenant}.service"      # a unit per tenant; the deployment fills in {tenant}
        started_by: "ctd-qc-queue-{tenant}.path"
        wait: false                                # do not wait for long work: the call result is "accepted for run"
```

The action manifest carries `title` for the card, `description` for the model, a closed schema with `required`, a deadline, the idempotency flag, the risk and the executor (MAN-9). The executor is named in one form: the plugin, its write tool and the OS mechanism (DEP-4). For actions it is always `oneshot`; the second form, `unit`, exists only for the tool `service/control`, with which a bot holding the role permission and the initiator permission (D-73) restarts a catalog service without a card if `restart` is in its `no_card`; `start`, `stop` and everything else go by card (MON-8, D-74). Any long work needs `wait: false`: if the unit started with waiting, the write call would hang for the whole run and end with `exec_unknown`. The field `risk` is shown on the card (APR-10). An action that irreversibly deletes data, changes permissions or exports data outside the farm is declared with `risk: high`, and in a production tenant two different people approve it (APR-16). Quality control writes only its own result files and does not change the source data, so it has `risk: low`. Class `routine` (D-78) is declared with `writes_only`, `idempotent: true` and the counters `runs_per_hour`, `runs_per_day`, and goes without a card within them; it does not fit quality control, because a run occupies the machine for hours.

### Month format (`spec/model/formats.yaml`)

```yaml
year_month: {pattern: "^[0-9]{4}-(0[1-9]|1[0-2])$", maxLength: 7}
```

### Stations (`tenants/demo/catalog.yaml`, tenant data)

```yaml
apiVersion: botaru/v1alpha1
kind: Catalog
metadata: {name: demo}
spec:
  resources:
    - id: ctd-stations
      title: "Monitoring stations with CTD profiles"
      host: ctd1                   # the pipeline machine: the write call goes here (TEN-3a)
      list: stations               # enum_from: catalog.stations refers to this list
      format: id                   # each value passes the id format at apply
      values: [st-01, st-02, st-05, st-07, st-09]
```

The model does not see data paths, addresses or credentials: only station `id`s.

### The executor (`tools/pipeline/tool.yaml`, shared)

```yaml
apiVersion: botaru/v1alpha1
kind: ToolPlugin
metadata: {name: pipeline}
spec:
  source: {image: "registry.example.org/botaru-pipeline@sha256:<digest>"}
  placement: host                  # production machine: no free input
  network: {egress: []}
  tools:
    - name: start_ctd_qc
      access: write                # never gets into the model's catalog
      tier: 3
      output_class: internal
      untrusted_output: false      # answers only with the acceptance time and the run number
      args_from: action:pipeline.ctd_qc   # one schema, in the action
      output_limit_kb: 4
      timeout_s: 20
```

The argument schema is stored once, in the action, and the write tool refers to it (`args_from`): otherwise two copies of the schema would drift apart sooner or later, and the lint rejects a second schema source (MAN-9). The shared plugin manifest has no OS user: each tenant has its own, set in the deployment description.

### Deployment on `ctd1` (`deploy/compose.example.yaml`, fragment)

```yaml
hosts:
  ctd1:
    kind: production
    processes:
      - {name: th-demo-read, kind: toolhost, tenant: demo, placement: host, user: botaru-th-demo,
         networks: [core], secrets: [worker-token-th-demo-read]}
      - {name: batch, kind: plugin, parent: th-demo-read, mode: stdio, secrets: [], egress: []}
      - {name: th-demo-act, kind: toolhost, tenant: demo, placement: host, tools: write,
         user: botaru-act-demo, networks: [core], secrets: [worker-token-th-demo-act]}
      - {name: pipeline, kind: plugin, parent: th-demo-act, mode: stdio, secrets: [], egress: []}
      - {name: job-demo, kind: job, tenant: demo, user: botaru-job-demo, networks: [core],
         secrets: [worker-token-job-demo]}
    os_users: {ctd-qc-run: {demo: botaru-ctd-demo}}   # ctd1 belongs only to demo (D-75); demo2 is on its own machines
```

The action toolhost is separate and runs no read plugins. A machine belongs to one tenant (D-75): `ctd1` appears only in the catalog of `demo`, and `demo2` has its own machines with their own toolhosts, OS users, queue and units; the deployment lint rejects a description where one machine appears for two tenants (`lint:deploy/two-tenants-one-host`), and a description where a tenant's toolhost or plugin sits on the farm node `core1` or `backup1` (DEP-11).

### Who may request and who presses

```yaml
# roles/pipeline-reader/role.yaml (spec fragment)
actions_may_request: [pipeline.batch_resume, pipeline.ctd_qc]

# tenants/demo/groups.yaml (fragment: the group name and its spec)
ctd:
  caps: ["task:pipeline.status.v1", "action.request:pipeline.ctd_qc"]
  data: [summary, internal]
ctd-lead:                          # lead of CTD data
  caps: ["action.approve:pipeline.ctd_qc"]
  data: [summary, internal]

# tenants/demo/tenant.yaml (spec fragment)
kind: production
self_approve: false                # nobody may approve their own request (APR-9)
```

An action can be requested only if both the bot's role and the human initiator have it (APR-2). A bot gets no more permissions than the person on whose behalf it works (PRM-2). Both groups are privileged because they contain an action, and only the operator grants them, through `farmctl` (PRM-13).

### Report to the person who asked

```yaml
# tasktypes/pipeline.watch.v1.yaml (spec fragment): the pipeline watcher, a job worker on ctd1
worker_kind: job
tier: 1                            # reads result files on a working machine
data_class: internal
events: [run.finished.v1]          # without this line P1 emit rejects the event
```

```yaml
# tenants/demo/schedules.yaml (fragment)
apiVersion: botaru/v1alpha1
kind: Schedule
metadata: {name: pipelines-watch, tenant: demo}
spec:
  cron: "*/5 * * * *"
  task: {type: pipeline.watch.v1}
---
# tenants/demo/routes.yaml (fragment)
apiVersion: botaru/v1alpha1
kind: Route
metadata: {name: ctd-qc-finished, tenant: demo}
spec:
  on: {event: run.finished.v1, subject: ctd-stations}
  task: {type: pipeline.ctd_qc.report.v1}
  deliver_to:
    - approval_initiator: {action: pipeline.ctd_qc}   # the private chat of whoever requested this action
```

```yaml
# tenants/demo/groups.yaml (fragment: the group name and its spec)
system-ops:
  caps: ["task:pipeline.watch.v1", "task:pipeline.ctd_qc.report.v1"]   # and the task types of other schedules and routes
```

Every 5 minutes the watcher reads the result files that `ExecStopPost` writes, and for each new result it emits the event `run.finished.v1` (P1 `emit`) with closed fields: `run_id: ctd-qc-demo-4813`, `approval_id: 4813`, `subject: ctd-stations`, `duration_s`, `retries`, `exit_code`, `outcome` (OBV-7). The same watcher also reports the end of lab runs ([flows.md](flows.md), flow 6). The core sets the source and number of the event, and the core itself writes the points of the duration, retry and outcome series from this event. The schedule and the route run as the system principal `system`, so both of their task types are listed in group `system-ops` (PRM-10). The route enqueues the report task with the core idempotency key `system:event:ctd-qc-finished:<source>:<id>` (IDM-2). The delivery target comes from the route (DLV-6): the core finds approval 4813 by the number in the event data and accepts the target only if the approval belongs to `demo`, its action is named in the route, it is in state `executed`, and the initiator is a person. The report goes to `alice` in a private chat if she is active and has `data:internal`. The report is a code table per station (how many profiles were checked, how many have remarks) and the pipeline's report file, which the job submits through P1 `file.put`. The report body came from the route, so the task is marked `untrusted_seen` (INJ-13).

## For the engineer: what stands behind `executor` on the pipeline machine

This is no longer the skeleton but the deployment: it shows that the executor is narrow. The units are a sketch, to be checked on a prototype.

The plugin runs inside the action toolhost process and does not need the host's D-Bus. It only puts a run request file into the queue directory, and systemd starts the run.

1. The action toolhost has already checked the approval number, the tenant and the argument hash (APR-7).
2. The plugin checks how much of the call lease is left. If the lease has expired by the toolhost's own clock, work stops and nothing is written: the core decides about a retry (FEN-6).
3. The plugin writes `/var/spool/botaru/demo/ctd-qc/tmp/4813.json` and renames it to `new/4813.json`. If a run request file with this number already exists in `new/` or `taken/`, a second one is not created. This is the idempotency: the file name carries the approval number, and a second executor recognizes the work of the first (FEN-7).
4. The plugin answers "accepted for run at 10:20, run ctd-qc-demo-4813" and does not wait for the end of the run.
5. The unit `ctd-qc-queue-demo.path` sees a non-empty `new/` and starts the oneshot unit of the request parser, owned by root. The parser strictly checks each run request file: only `month`, `stations` and `level`, the format, stations from this tenant's list on this machine. Then it moves the file to `taken/`, writes `params.json` into the run directory and queues the run without waiting: `systemctl start --no-block ctd-qc-demo@4813.service`.
6. The run unit runs as this tenant's pipeline user, and `RuntimeMaxSec` is the run's own clock: a hung run stops by itself. `ExecStopPost` writes the result file even if the pipeline engine crashed before creating the trace, and from it the watcher emits the end event.

```ini
# /etc/systemd/system/ctd-qc-queue-demo.path
[Path]
DirectoryNotEmpty=/var/spool/botaru/demo/ctd-qc/new

# /etc/systemd/system/ctd-qc-queue-demo.service, owned by root
[Service]
Type=oneshot
ExecStart=/usr/local/libexec/ctd-qc-dispatch demo /var/spool/botaru/demo/ctd-qc

# /etc/systemd/system/ctd-qc-demo@.service, owned by root; the instance is the approval number
[Service]
Type=exec
User=botaru-ctd-demo
WorkingDirectory=/srv/ctd-qc/demo/runs/%i
Environment=ENGINE_HOME=/srv/ctd-qc/demo/engine-home
ExecStart=/usr/local/bin/pipeline-engine -C /srv/ctd-qc/engine.config run /srv/ctd-qc/main.flow \
  -profile standard \
  -params-file /srv/ctd-qc/demo/runs/%i/params.json \
  -work-dir /srv/ctd-qc/demo/runs/%i/work \
  -with-trace /srv/ctd-qc/demo/runs/%i/trace.txt \
  --outdir /data/qc/ctd/demo/%i
ExecStopPost=/usr/local/libexec/ctd-qc-finished demo %i
RuntimeMaxSec=12h
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ReadWritePaths=/srv/ctd-qc/demo/runs/%i /srv/ctd-qc/demo/engine-home /data/qc/ctd/demo
```

- Each tenant has its own units, queue, directories and OS user, and the tenant name is part of their names. One unit template for everyone does not work: the OS user cannot be taken from the instance name.
- The pipeline path, the config (`-C`), the profile, the work directory and the results directory are fixed in the unit. Only the month, the stations and the check level come from the run request file, and the parser checks them once more: the pipeline engine itself would accept any key from `params.json`.
- The source data is read-only, and `ProtectSystem=strict` enforces that, not a promise in the action description.
- There are no sudoers lines: the bot has no sudo (DEP-3).
- The plugin does not start units itself through the host's D-Bus: a request for a transient unit (`systemd-run`) comes with the same verb `start` as a normal start, and a permission with an imprecise name pattern would give root. So the action plugin goes through the queue and a oneshot unit, and for the `unit` mechanism of the tool `service/control` the core generates the OS access rule from the catalog by the binding `spec/bindings/os/systemd.md`, only for exact `.service` unit names without patterns and only for the verbs `start`, `stop`, `restart` (DEP-4); which OS mechanism implements this rule is said by the binding, not by the rule.

## Which rules apply here

| Rule | Where in the example |
|---|---|
| PRM-2 | the bot gets no more permissions than the initiator; tier 3 only through approval |
| PRM-7 | action permissions without wildcards |
| PRM-10 | the watcher task type `pipeline.watch.v1` and the report task type `pipeline.ctd_qc.report.v1` are listed in `system-ops` |
| PRM-13 | groups `ctd` and `ctd-lead` are privileged, and the operator grants them |
| APR-1 | the write tool `start_ctd_qc` never gets into the model's catalog |
| APR-2 | a request is possible only if both the role and the initiator have the action; no more than 3 requests per task; arguments by a closed schema |
| APR-3 | the channel draws the card only from the approval record |
| APR-4 | a person with the permission presses, once, before the deadline, and not the initiator |
| APR-5 | only the core enqueues the `action.*` task, on approval |
| APR-6 | removing a group from the initiator cancels the waiting request |
| APR-7 | one write call per approval; the toolhost checks the number, the tenant and the hash; `exec_unknown` without a retry |
| APR-9 | a production tenant does not allow self-approval |
| APR-10 | the action risk in the manifest and on the card |
| APR-17 | `/run ctd-qc` creates the same card because the pipeline writes on a working machine |
| MAN-9 | `title`, `description`, `risk`, default values before the snapshot, `args_from` |
| MAN-10 | catalog values pass the format at `apply` and are checked again before the write call |
| INJ-1 | the closed argument form, the format from the registry, `maxItems` on the list |
| INJ-13 | the report task from the route is marked `untrusted_seen` |
| TEN-6 | the action manifest is shared and holds no tenant data; the stations come from the catalog of the task's tenant |
| TEN-3a | the core computes the write call target: tenant, plugin, resource machine |
| DEP-3 | neither root nor sudo for the bot and the plugin |
| DEP-4 | the executor is the plugin, its write tool and a narrow OS mechanism; long work without waiting |
| DEP-9 | a separate action toolhost; the OS mechanism allows the start only to its user |
| FEN-6 | the executor stops work by its own clock if the lease has expired |
| FEN-7 | the run request file carries the approval number, and a second executor recognizes the work of the first |
| CNT-4 | the attempt does not wait for the button: the waiting lives in the approval record |
| OBV-7 | the event `run.finished.v1` with closed fields reports the end of the run |
| IDM-2 | the core sets the key of the report task from the event |
| DLV-6 | the report target comes from the route, not from model text |
| IDN-6 | the channel code picks the task type by the command |

## What goes into which version

| Version | What appears |
|---|---|
| v0.1 | an action with `title`, `description`, `risk` (`low` or `high`), a closed form and default values before the snapshot; the format `year_month`; checking catalog values at `apply` and before the write call; `enum_from` values in the `request_action` schema; `args_from`; an executor with an OS mechanism and `wait: false`; an OS user per tenant; the `/run` command for an action through a card; the run finished event outside the farm and a route with the target "approval initiator" |
| v0.2 | two different approvers for actions with `risk: high` (APR-16) |

## Why it is this way

- The model chooses values only from closed lists, and the person on the card sees exactly what will run: the argument snapshot with the default values filled in.
- Stations are required: a wide run must be an explicit request, not a default value that the model could choose after reading an untrusted log.
- The write call answers "accepted for run", and a separate event reports the end of the work: this way the call does not hang for hours, and `exec_unknown` is not confused with a long run.
- A queue directory and a request parser are easier to verify than a permission to start units by a name pattern, and the plugin does not need the host's D-Bus.
- Everything that belongs to a tenant (OS user, queue, units, directories) is separated per tenant, so a bug in one tenant's plugin does not open the files and queues of another.
