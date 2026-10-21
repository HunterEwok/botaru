# impl: architecture of the reference core

This document describes how the reference implementation behaves, subsystem by subsystem. The overview, the programs,
the packages, the key decisions, what is not built yet and the checks are in [README.md](README.md). Each section names
the rules and decisions of the skeleton that a behaviour supports; these are choices of this implementation where the
skeleton leaves one, and none changes a contract.

The core claims skeleton 0.3 (release 0.3.0) with the profiles `core`, `monitor`, `insights`, `governance`, `manager`,
`messaging`, `campaign`, `knowledge` and `ingress` (README.md, "Versions, profiles and deferred rules"). Everything
below is the behaviour of that build.

Sections:

1. Core, Store & Clock
2. Dispatcher, Leases, Holds & Alarms
3. Tools (P3/E3), Plugins & Output Redaction
4. Model Gateway (P4), Providers & Money (`eur_micros`)
5. Channels, Cards, Commands & Outbound Privacy
6. Sessions, Memory, Knowledge & Erasure
7. Monitor (E5), Incidents & Farm Manager
8. Observations, Proposals, Commission & Governance
9. Campaigns, Scripts & Translator
10. People, Rights and P2 Views
11. Workers and Deployment
12. Evals and the conformance runner

## Core, Store & Clock

### Store and clock

The C1 store is PostgreSQL 16 or later through pgx. The store clock is `botaru_now()` (the database time plus the test
bench offset), so every deadline is counted by one clock (TIM-1). All core state is in the store per transaction, so a
core copy holds nothing that a restart would lose.

### The skeleton at run time

The skeleton is read at run time from a checkout (`spec_root`, or the folder the program runs in), not copied into the
binaries, so the schemas the core checks are the ones in git. `farmctl` reads the schemas it needs from the checkout as
well (`-spec-root` or `BOTARU_SPEC_ROOT`, else a parent of the working directory).

### Defaults

Where a type sets nothing: lease 60 s, heartbeat 20 s, 2 attempts, backoff 10 s doubling to 5 min, task deadline 1 h, 5
delivery attempts, priority class 3 (2 for an interactive type from a person).

A bare command is its own goal: `goal` is required by `task-body`, so `/hello` without text gets the goal `/hello`.

### The attempt record

At `claim` the core writes into the lease of the attempt the run fingerprint with its part hashes (PRV-1) and, for the
full result check at `complete` (OUT-1), the version of the tenant catalog and the `enum_from` values of the result
schema filtered by the task's mark.

- P1 `hello` carries no `describe()` values, so the runtime part has `describe: null`; its driver is the role's
  `runtime.driver`, else the implementation name from `hello`.
- `catalog.<list>`, `catalog.hosts`, `catalog.services`, `catalog.monitor_triggers` and `catalog.monitor_items` resolve
  from the tenant's `Catalog` manifest, `reasons.<name>` from `spec/model/<name>-reasons.yaml`; `task.attachments` and
  any other list resolve to no values, so no value fits.
- The result schema reaches the runtime as the extension field `payload.result_schema` of the attempt (MAN-3), so the
  instruction stays exactly the trusted text from git; the runtime appends the request for a JSON answer to the system
  prompt. The projection of the result schema sent to the runtime is a hint: `complete` still checks the full schema.
- Beside it, the extension field `payload.full_result_schema` is the schema `complete` checks: the `enum_from`
  leaves with the catalog values fixed in the attempt record (a list the record lacks, such as one of the memory
  `facts` field, resolved by the same mark), and each `x-botaru-format` with the `pattern` and `maxLength` of
  `spec/model/formats.yaml` as plain keywords of the leaf. A type with no schema of its own gets a copy of the core's
  schema (`insight-result`, `review-result` or the necessity output). `botaru-agent` shows the model this schema with
  the rule to omit unknown optional fields instead of writing `null`, removes a `null` of an optional field that does
  not take null, and checks the answer locally with the same validator and ECMAScript patterns as the core: a failing
  answer gets one more turn that quotes the paths and reasons, so the attempt is not spent on a format the model can
  fix; what the second answer holds goes to `complete`, where the core decides.
- Results refused at complete (`output_schema_invalid`, OUT-1) are stored on the lease like an accepted outcome, so a
  repeat of the same complete gets the same refusal (IDM-3), and any other call under the lease is fenced. A lease taken
  before the attempt record existed has none; its `enum_from` values are resolved at complete.

### The set lint of `apply`

The set lint (MAN-6, MAN-10, DEP-11, PRF-1, DSP-3, INJ-6 and the other checks of `ci/check_manifests.py`) runs over the
whole set after every object passed its schema, with the stable codes of `ci/lint_codes.py`, so a set gets the same
codes from `apply` as from the skeleton lint over git. `TestLintFixtures` runs the cases of
`conformance/fixtures/manifests` through `apply` and keeps the list of agreeing cases in
`internal/core/testdata/lint-passing.txt`.

Where this core differs from the skeleton lint:

- References of the tenant manifest (commands, default types, `/run`, delegated groups, allowed model profiles) to
  objects outside the set are logged, not refused, because a deployment applies the tenant by stages (the hello-bot
  sequence applies the demo tenant with one type); a missing object is refused where it is used.
- The checks of the manager seat (MGR-20) and of monitoring as a recipient (MON-1, D-88) run only under their profiles
  `manager` and `monitor`, which the build claims.
- On a test bench an old model price (GW-17) is logged.

Further checks of the set:

- The farm nodes come from the setting `farm_hosts` (DEP-11); without it no machine counts as one.
- A plugin's `network.egress` names only machines of a tenant catalog of the set, and `monitor-api` only for the plugin
  `monitor` (INJ-6, `egress_unknown`).
- A dispatch rule's bot group must exist and select a role that takes the type (`dispatch_group_disjoint`), its attempt
  period is at most twice the type's and within its deadline, and its attempts are fewer than the type's and at least
  one (`dispatch_attempt_bounds`, DSP-3, FEN-3).
- Reserved classes (PRM-5): `cap_data_class` also covers a type's `data_class` and a catalog resource's `data_class` of
  `pii` (or unknown), and a tool's `output_class` of `pii` or `secret`, in the core and the skeleton lint.
- Model exposure of datasets (PRV-4): a catalog resource of kind `dataset` without `model_exposure` counts as
  `aggregates`; `apply` gives `model_exposure_agent_read` and `model_exposure_not_aggregate` for the cases that P3
  refuses (see Tools).

### Privacy block of a tenant

- The lint refuses a production tenant (`kind: production` or none) with an empty field of its privacy block
  (`privacy_incomplete`, a code of `ci/lint_codes.py` that `ci/check_manifests.py` checks the same way, SEC-8):
  `purposes`, `controller` with `name`, `contact` and `agreement_ref`, `recipients`, `notice_ref`, `notice_version`,
  `dpia_ref` (an explicit `none` passes), `ropa_ref`, and each subprocessor's `agreement_ref`, `region` and `retention`.
  `subprocessors` is asked only of a tenant with `providers_allowed` (without one P4 calls no provider for it) and
  `channels` only of a tenant with a Channel; `subject_request_days` has its default and `export_sink` is optional.
- Any tenant whose `providers_allowed` names a provider outside `subprocessors` gets `subprocessor_undeclared` at
  `/providers_allowed/<n>`.
- An apply that changes any field of the block writes the event `tenant.privacy_changed` (an additive entry of the
  events catalog with `spec/model/event-data/privacy-event.schema.json`: the names of the changed fields and the notice
  version, no values) and plans the code notice `notice.privacy_changed` (an additive message catalog key) to every
  person with `admin:people` in their private chats, in the apply transaction. The production tenants of `examples/` and
  the fixture tenants carry every field.
- Notice version (SEC-13): a set that changes `controller`, `purposes`, `subprocessors` or `recipients` of a stored
  tenant without a higher `notice_version` fails with `notice_version_not_raised` (the skeleton lint checks it against a
  fixture case's `before`).
- A channel without a line in `privacy.channels` counts as third-party storage with ceiling `internal` (SEC-8).

### Health and the store volume

Each loop marks its tick in the table `loop_ticks` after a pass that ended without an error, so P2 `health` shows the
age of each loop from the store (OBS-4). The `health` loop computes the three indicators of OBS-9 and raises their
alarms in the `farm` tenant; it does not judge its own tick, and on a bench that ticks only by `loop.tick` it raises no
loop tick alarms.

The core measures the store volume by the path `health.store_volume`, which holds when the store runs on the core
machine (D-88); without it there is no measurement and no headroom. Defaults: urgent age 600 s, the share of deliveries
delivered within 10 minutes at least 0.9 over a window of 1 h (a withheld delivery is not counted), loop tick 30 s,
store free space soft 0.15 and hard 0.05.

When the store stops answering, P1 answers `not_ready` with `retry_after_s` until every loop ticked again (OBS-8); an
unexpected store error of any port answers `not_ready` as well.

### Core downtime

A lease that ends `core_unavailable` spends no attempt and counts no run; its task returns at once, and a delivery is
planned again at once with its attempt given back (FEN-5). P1 `fail` refuses with `input_schema_invalid` the reasons
only the core determines (`core_unavailable`, `lease_expired`, `worker_drain`, `seat_changed`, `preempted`, `stalled`,
`deadline_exceeded`, `attempts_exhausted`, `runs_exhausted`), so a holder cannot requeue a delivery forever or pass
`retry.max_runs`.

### Core keys

The keys of the core's own tasks start with `system:` like every core key (IDM-2). The farm manager's tasks use
`system:incident:<incident>:<episode>`, `system:inspect:<task>:<n>`, `system:daily:<tenant>:<day>` and
`system:directive:<id>`. The closed list of the rule text names `system:directive:<id>`, `system:grant:<grant>` (the
action task of a routine service grant, D-78), `system:kb.ingest:<document>` and `system:postmortem:<incident>` (named
by MGR-53), an additive edit of `conformance/requirements.yaml`; the test reads the list from the rule text and adds
only these.

## Dispatcher, Leases, Holds & Alarms

### Order and ceilings of dispatch

- Fairness within a class (DSP-6): when the tenant sets `dispatch.fairness`, `claim` orders the waiting tasks of a class
  by running attempts per weight of the key's kind (the part before `:`, weight 1 by default). A key of weight 0 goes
  last. Ties go to the key whose oldest waiting task came first, then to the smaller id. Without `dispatch.fairness` the
  order is the enqueue time.
- Running ceilings (DSP-7): a tenant dispatch rule with `max_running` caps the tasks of its match in `working`; a task
  above it waits in `submitted` with `type_quota`.
- Program priority (DSP-5): a program's request takes the type's class (3 without one); its own priority only lowers it,
  and class 2 stays a person's direct request of an interactive type.
- A task the dispatcher cannot hand out carries its waiting reason on the submitted task, for example
  `provider_unavailable` or `provider_quota` (see Provider pairs), `type_quota`, `seat_eval_missing` (MGR-23),
  `skill_missing` (SKL-2) or `budget_exhausted`.

### Leases

- One lease per profile (RT-3): a runtime declaring `model_attribution: per_profile` is blocked at hello with a capacity
  above 1 and gets no attempt while it holds an active attempt lease, an expired one too until the reaper ends it, so a
  model handle and its usage always belong to one lease.
- Progress stage (DLV-11): a heartbeat whose `stage` is not one of the type's `stages` is refused with
  `input_schema_invalid`; a type without `stages` has no stage at all, so any stage is refused there and only the
  percentage passes.
- An attempt without new progress for longer than the type's `timeouts.stall_after` ends with `stalled`, spends the
  attempt and raises `alarm.task.stalled`; the next attempt resolves it.

### Holds

Holds (HLD-1..3) act at these check points:

| Check point | Effects and scopes |
|---|---|
| enqueue | `no_enqueue` on the farm, the tenant or the type |
| claim | `no_claim` and `cancel_running` on the farm, the tenant, the role, the worker or the type |
| heartbeat | `cancel_running` ends the attempt with `held` without spending it, revokes its handles and returns the task to `submitted` |
| delivery handoff | `no_delivery` on the farm, the tenant or the channel withholds the delivery with `held`; an alarm notice still goes |
| P3 and the handout to a toolhost | `no_tool` on the farm, the tenant, the role, the type, the plugin, the agent worker, the target machine (`host`) or the catalog resource of the target value (`resource`); at handout a hold on the toolhost also counts |
| send | `no_message` on the farm, the tenant, the role, the worker or a bot group (for a group address) refuses a send with `held` and discards at `complete` what the attempt staged |

- A hold scope outside the closed list is refused; the scope `sink` is accepted.
- P2 takes holds only from operator credentials: the credential of tenant `farm` reaches every tenant and the scopes
  `farm` and `seat`, any other only its own tenant. A hold of a person of the tenant (PRM-15) and its rank rules are in
  the core and wait for P2 on behalf of a person.
- `farmctl hold farm` writes a farm hold straight to the store with the core's settings (DEP-8).
- The `hold` loop ends a hold at its `until`, never earlier.
- Automatic holds of `contain_auto` come from runaway spend (HLD-7), secrets in outbound text (HLD-6), failing schedules
  (CAP-4), confirmed links of a role (HLD-9) and incidents (MGR-7); each is described with its source.

### Alarms

The core raises `alarm.worker.stale` (a worker not seen for 120 s, two long-poll periods of `claim`, OBS-4) and
`alarm.task.unclaimed` (a never-claimed task past `start_by`, DSP-8), and resolves them when the worker calls again or
the task is taken or ends (OBS-6). Other alarms are raised by the subsystems below (health, spend, providers, monitoring
problems, incidents, the farm manager and more). An alarm lives in episodes: a repeat of an open episode only counts and
keeps the latest numbers.

How an episode reaches people depends on the monitoring setting (MON-1, MON-13):

- Without the monitoring system configured, every episode change goes as the code text `notice.alarm` (delivery kind
  `alarm_notice`) to the private chats of the tenant's admins, persons with `admin:people` or `hold.release`; without
  such an admin, to the admins of `farm`; without either, the core logs a warning. An open `page` alarm repeats its
  notice every 30 minutes until it resolves. An alarm notice does not die by attempt count while its episode is open,
  and the notices of one alarm go in order.
- With `monitor: {enabled: true}` the core plans no `alarm_notice`: the monitoring system is the one path to people (see
  Export to the monitoring system).

### Child tasks and continuations

A role with `may_enqueue` gets the built-in tool `enqueue_child` for the types it lists that the initiator may start
(PRM-6, CNT-1..CNT-4).

- The child keeps the initiator, raises the data mark of the tree, counts toward `max_children` of the parent type (10
  by default) and the tree size of the tenant (`campaigns.max_tree_tasks`, 300 by default, 1000 at most), and stops at
  depth 5.
- A child's result goes to no person: when it ends, a parent type with `continuation` gets one continuation task with
  the child's state and result as its attachment, and its result goes to the person.
- A repeated call with the same body returns the same child. `enqueue_child` refuses any argument other than `type`,
  `goal` and `params` with `input_schema_invalid`, as its tool schema says (IDN-1).
- Enqueue under a lease (PRM-6): a job or agent with a live attempt lease enqueues a child of a type of its role's
  `may_enqueue` with the parent's tree under the key `system:child:<parent>:p1:<idempotency_key>`, so the same key with
  another body is `idempotency_conflict`. The dry run is `forbidden` to a lease holder (PRM-11); the body takes only
  `type`, `goal` and `params`, and `refs`, `to`, `scope` and `priority` are refused, since a child takes them from its
  parent. A worker's `task.get` and `cancel` answer `forbidden`.

### The follow-up window

A result whose type declares `need_input` as a field code reads, and sets it to true, opens a window
(`followup_window_s`, 30 minutes by default) for the person and conversation, and the bot says so under the answer
(CNT-5). The next plain message there continues that task as a new task of its type with the earlier outcome attached; a
reply to the bot message always continues it, and a command closes the window.

### Schedules

The `schedule` loop runs each `Schedule` of a tenant in its `timezone`, else the tenant's, else UTC (TIM-2, TIM-3,
PRM-10, DLV-6).

- Only the latest due slot runs, once, with the key `system:schedule:<name>:<slot>` and the initiator `system`, which
  enqueues with the group `system-ops` only. A missed slot is not caught up, and a slot found more than 15 minutes late
  (after a stop of the core) does not run.
- A new schedule, or a change of its `cron` or zone, counts from the tick that sees it. A local time that occurs twice
  runs at its first instant.
- A skipped slot is one `schedule.skipped` event: `dst_gap` for a local time that does not exist, `waiting` while the
  previous slot's task is `submitted` (unless `skip_if_waiting: false`), `held` and `over_capacity` for those refusals;
  any other refusal raises `alarm.schedule.refused` until a slot is enqueued again. A slot is skipped as `held` when its
  task params name a held host or a resource on it (MON-9).
- The result of the root task and of its continuations, never of a child, goes to `deliver_to` as the catalog has it at
  completion: `group:<g>` is a direct message to each active member with the data class of the task, after the
  first-contact notices; `sink:<s>` is a delivery to the sink within its ceiling; `chat:<binding>` gets only the closed
  summary within the binding ceiling.
- Apply refuses an unknown zone (`timezone_unknown`) and a `cron` with bad values.
- Failing schedules (CAP-4): the root task of a schedule slot that fails with `attempts_exhausted` or `runs_exhausted`
  counts in `schedule_state.failed_slots`, and any other end resets it. At 3 in a row the schedule gets the hold
  described in `holds.yaml` and the alarm `hold.auto`; the count starts again from zero.
- A schedule with `paused: true` skips every slot with `schedule.skipped(paused)`, without catch-up (TIM-5).

### Dead work

The first page of `deadletter.list` carries `groups` by (tenant, type, reason), where a delivery's type is its kind
(CAP-4). A redrive of a task without its body answers `gone`. `task.redrive` re-enqueues the body under the original
initiator, so rights are checked again; `delivery.redrive` plans a copy linked by `redrive_of`, and the ceiling and the
recipient are checked again at handoff.

### External programs on P1

A program key is a deployment setting, `programs: [{token_sha256, tenant, key_id}]`; it resolves to the active principal
of the tenant with identity `api:<key_id>`, who is the initiator, and the tenant comes only from the key (TEN-2, IDM-1,
TEN-7, BUD-1, DSP-7). A turned-off program's key is refused with `person_inactive`.

- `enqueue` runs the sender checks of a person (`task:<type>`, the budget reserve, `max_queued` with `retry_after_s`)
  with the source `interactive` and `enqueued_by: program:api:<key_id>`; the audit `via` is `core`, since the schema has
  no program value.
- A ref and an attached file must be the program's own: a task or campaign it started or a file it uploaded or its tasks
  produced, else `ref_invalid` (audited, the key stays free).
- `priority` only lowers. `to.group` is refused with `group_invalid` (no group dispatch for tasks) and `scope` with
  `input_schema_invalid`. The dry run (`mode: validate`, PRM-11) is deferred and answers `version_unsupported`.

## Tools (P3/E3), Plugins & Output Redaction

### The tool catalog of an attempt

The P3 catalog of an attempt is computed by its tools handle on every request:

- the role's read tools whose tier is within the task's `max_tier` and whose `output_class` is within its mark;
- `enum_from` turned into the catalog values the mark allows, a resource list giving only the values of resources whose
  `read_by` holds the tool, in name order; a value two such resources share has no single target and is refused;
- the built-in tools of `spec/model/builtin-tools.yaml` that the role, the task's purpose and the claimed profiles give
  it (`request_action`, `enqueue_child`, `send_message`, `inbox_read`, `observations_query`, the `status.*` views,
  `service/control`, `monitor/script_run`, the campaign and script tools, `kb_search` and `kb_read`, `escalate` and
  `request_model_grant`, and the farm manager's tools), each described with its subsystem. The tools not served are
  listed in README.md, "Not in this version".

The model sees `<plugin>_<tool>`; rights and audit use `<plugin>/<tool>`. P4 requires the request tools to be exactly
that catalog, in its order, compared by the hash of `{name, description, input_schema}` (GW-1), and the run fingerprint
takes its catalog part from the same hashes.

A call is checked in one transaction (holds, rights, tier, output class, the closed argument schema, the target machine)
and queued for a toolhost of the tenant, the plugin, the placement and the machine. Nobody taking it within 30 s, or the
type's `timeouts.tool_start_by` if shorter, ends it with `no_executor`, and no live toolhost of the target ends it at
once with `plugin_unavailable`. A refusal reaches the model only as "no permission"; the reason goes to the audit. A
call whose attempt loses its lease is abandoned.

An attempt keeps the tool catalog it began with: once an `apply` changes it, P3 and P4 refuse the attempt with `fenced`,
and the agent returns the work with `release` without spending the attempt, so the run fingerprint always describes the
tools the model had.

Model exposure of datasets (PRV-4): a call by an agent role to a tool that reads a resource of exposure `none` is
refused, and of `aggregates` unless the tool's output is trusted (code that aggregates).

### Plugin manifest checks at `apply`

Beyond the schemas:

- exactly one of `target_from` and `target: any`;
- no free string parameter outside a `sandbox` plugin (`sandbox_placement`; the sandbox profile is not claimed);
- the `ip`, `email` and `secret` masks on a host tool whose output is shown as untrusted;
- no free text in the output schema of a tool declared trusted;
- the program commands of `cmd`, where a parameter name has one schema across the tool and all its commands
  (`cmd_param_conflict`, INJ-14).

Address arguments (INJ-6): a tool parameter whose name, or last word after `_`, is `url`, `uri`, `href`, `endpoint`,
`host`, `hostname`, `address`, `addr`, `ip`, `link`, `server`, `domain`, `fqdn`, `subnet` or `cidr`, or a plural of one
(`urls`, `hosts`, `addresses`), and whose values are not a closed list (`enum`, `enum_from`, `const`) is
`tool_address_param` (an additive lint code).

- `target` and `dest` also name things other than an address, so they count only on a string that can hold one: without
  a format or of an open format (`search_query`, `ipv4_subnet24`), not of `id`, `type_name` or another closed format.
- The items of an array count under the array's name, the properties of an object under their own, and a parameter
  counts when any branch of its `anyOf` or `oneOf` does.
- On a plugin with a non-empty `network.egress`, every parameter that can carry an address whatever its name is
  `tool_address_param` too: a string, also in an array, an object or a branch, without a closed list and without a
  format, or of an open format.

Text from outside (INJ-14): a command whose program is `journalctl` prints text written from outside, so its tool needs
`untrusted_output: true` (else `untrusted_output_false_text`) and `output_class: raw_logs` (else
`untrusted_output_class`, an additive lint code). The skeleton has no registry of such programs, so the list holds only
the journal; `systemctl status` is not on it, since its journal lines are turned off with `-n 0` in the example.

### The work item of a tool call

The work item carries two extension fields (MAN-3): `resolved`, the catalog fields a plugin needs (for example
`{"service.unit": "nginx.service"}` for a command with `{service.unit}`), and `limits`, the tool's `redact`,
`output_limit_bytes` and `timeout_s`, so the toolhost needs no manifests. The plugin gets them as
`_meta["botaru/resolved"]` and `_meta["botaru/work_id"]`.

### The toolhost

- `hello` reports one `tools_fingerprint`: the fingerprint of `spec/hashing.md` for one plugin, and for several the hash
  of the array of the plugin fingerprints in the order of the plugin names. The core computes the same from the
  manifests and blocks a toolhost that differs (INJ-2). After an `apply` that changes one of its plugins, a toolhost
  takes no calls and does not count as live until it says hello again with the new fingerprint, so restart it after
  updating the checkout.
- Only an own plugin without secrets and network runs as a child process of a shared toolhost (DEP-7). The core cannot
  see a container, so a toolhost is not enrolled for a plugin with secrets or network egress, except the farm's own read
  plugin `monitor`, which runs alone in a toolhost of its own under its own OS user.
- Each call runs in a new plugin process in its own process group, with an empty environment unless the settings name
  one; the toolhost kills the whole process group after every call, so nothing a plugin started outlives it. A plugin
  that does not stop within `kill_after` after a cancellation is killed.
- The toolhost cancels a call 10 s plus `kill_after` before the end of its lease by its own clock, also when a heartbeat
  stalls, so the plugin is gone before the lease ends (FEN-6).
- A request from a plugin (sampling, elicitation, roots) is refused and the call ends with `exec_failed`, the reason of
  every plugin error. The toolhost masks a plugin's error text before logging it.
- Plugin health (OBS-10): a toolhost may send `health` reports in `hello` and `heartbeat` (an optional field this build
  adds to both request schemas). A hello report is complete, so a plugin it does not report as `down` is `ok` again; a
  heartbeat report changes only the plugins it names. A plugin in `down` gets no hand-out, and its new and queued calls
  end at once with `plugin_unavailable` while no other live toolhost serves them.

### Tool output and redaction

Tool output is masked by the toolhost by the kinds of the tool's `redact` and cut at its limit; the core masks secrets
once more, cuts again and checks structured output against `output_schema`. The text blocks are joined before each mask,
so nothing split across blocks escapes it, and structured output above the tool limit is refused.

- Only a tool that declares `untrusted_output: false` and has an `output_schema` without free text (a string of the
  prose format `search_query` counts as free text) counts as trusted, and the model then sees only its checked
  structured output.
- Any other output reaches the model inside a mark with a random value per call, and the first such output sets
  `untrusted_seen` on the task, with the tool as `untrusted_source`; untrusted output carries no separate structured
  content.
- The output of a call is checked against the tool definition it was queued under: if an `apply` changed or removed that
  tool while the call waited or ran, the call is not handed out, or its output is dropped, and it ends with
  `plugin_unavailable`.

### Flags of the record

A forwarded or quoted message sets `untrusted_seen` on the task with the source `forward` or `quote` (INJ-13, AUD-5).
`personal_seen` lists without content where the task met personal data: a forward, a quote, or tool output in which the
toolhost masked an address (`masking`); the audit record of `task.complete` carries the same list. When the channel
names the author of the forwarded or quoted text (`forwarded_from`, `quoted_from`) and the tenant knows that identity,
the principal goes into `personal_subjects`; the sender and an unknown author do not.

### Own plugins

`botaru-plugin` serves the own plugins as MCP servers on the standard streams, without secrets and without network
(DEP-7):

- `cmd` runs the named commands of its manifest through execve without a shell, with an absolute program, a fixed
  environment and each parameter as a whole argv element after `--`; a value that starts with `-`, is empty or holds a
  newline is refused, and the manifest is checked against `spec/model/forbidden-programs.yaml` at start. It resolves the
  symlinks of a program path at start and again at each call, refuses a program whose real name is forbidden, and runs
  the resolved path. A `{param.field}` in a command is the catalog field the core resolved for the argument.
  `-program-root` (a test stand only, the simulator of [SIM.md](SIM.md)) puts the absolute programs of the manifest under
  a directory: `/usr/bin/df` runs as `<root>/usr/bin/df`.
- `logs` reads the tail of a catalog log source; which file a source is on the machine is local deployment data
  (`-source id=path`), and a symbolic link or a non-regular file is not read.
- `checks` reads the latest results of a check from a results file that an external checker writes.
- `service` runs `systemctl <verb> -- <unit>` with the unit the core resolved from the catalog and returns only the
  state from `systemctl is-active`; `botaru-plugin polkit` prints the polkit rule for one machine from the catalog
  (DEP-4).
- `queue` runs a write tool of the `oneshot` mechanism: it puts the run request (the tool, the closed arguments and the
  approval or grant number) as a file into the tenant queue and answers `accepted`; a repeat of the same number finds
  the file. The root request parser the path unit starts belongs to the installation.
- `monitor` reads the monitoring system (see Monitor).

### Tool output cache

Only a plugin tool with `access: read`, `cache_s` above 0 and no per-principal credential is cached (PRM-12); built-in
tools are never cached.

- The key is the tenant, the plugin, the tool, the arguments, the hash of the tenant's `Catalog` manifest at the time of
  the call and the data mark of the calling task (both kept on the call record, so a later apply does not change the key
  an output is stored under), and the hash of the tool definition the call was checked against.
- A hit is served only after the full check of the call: the call record is `done` with `cached`, the attempt gets
  `untrusted_seen`, the `personal_seen` sources of the call that filled the entry, and of the `personal_subjects` of
  that call's task only those the output names by principal id or by an identity (whole, or the part after its kind; a
  person the output names in another way is not flagged); the usage line has no amount and the flag `cached`.
- A `no_tool` hold drops the entries it covers: all for `farm`, the plugin's for `plugin`, the tenant's for any other
  scope, as an entry does not record the role, type, worker or target it was filled for.
- Any erasure request of the tenant drops the tenant's entries, and so does a request replayed after a restore when the
  `erasure` loop gives it its steps; the `retention` loop deletes an entry past the tenant's content period even before
  its `cache_s` ends.

### Service control

A role with `service_control` gets the built-in tool `service/control` in a work task of tier 1 and above, with the
role's services of the tenant catalog and their verbs as an enum. Only services with `via: host`, an exact `unit` and a
control list are offered.

- The initiator of the task needs `service.control:<service>` (a service or system principal only by an explicit row).
- A verb of the service's `no_card` (`[restart]` by default) in a task without `untrusted_seen` runs at once: the core
  writes a `service_grant` audit record whose id the write call carries as `grant_id`, the bot waits for the call and
  the model gets only the code, the unit state and the grant id.
- Any other verb, and any call after untrusted text, becomes an ordinary approval of the action `service.control` with
  its card, decided by a person with `action.approve:service.control` or `action.approve:service.control:<service>`
  (APR-18, INJ-15).
- Guards: holds `no_tool` or `no_action` on the tenant, the machine, the plugin `service`, the action, the role, the
  type or the worker; at most 6 actions per service and 30 per tenant an hour, above which the call gets
  `service_rate_limited` and raises `alarm.service.control.rate` (`page` for a stop); one operation per service at a
  time (`resource_busy`).
- After a service action on a catalog host with an open monitoring problem the core plans the note
  `service_control_done` (see Monitor).
- Not built: services with `via: monitor`, and the check of the state 2 minutes after a stop or restart with its result
  in the task journal (MON-8).
- Grant initiator (FEN-7): every `service_grant` block carries `initiator` (an additive optional field of the audit
  record schema): for a bot's `service/control` or `monitor/script_run` the initiator of its task, for a person's own
  call or a routine script step the person, and the author itself when the task has none. The author kind follows the
  author's principal (`human`, `bot` for a bot or worker, and `service`, an additive enum value, for an external program
  or the system on P2), so an operator credential that is not a person is not recorded as one.

`service.control` on P2 (APR-18, PRM-18): the operator credential's own person controls a service. Without
`service.control:<service>` the answer is `forbidden` with `needs`, a guard or hold refuses, a caller who is not an
active person is refused with an audit record, and a verb outside `no_card` opens an approval (`approval_required`, or
`service_rate_limited` past the card limit). The answer keeps to the closed `service-control-result` codes on P2 and P3:
a seat's restricted mode answers `forbidden` and its `ops_per_hour` `service_rate_limited`, with the real reason in the
audit. Otherwise the core writes a service grant with the person as author, an action task without origin and one write
call, and answers `done` with state `unknown`; P2 does not reconcile the outcome.

## Model Gateway (P4), Providers & Money (`eur_micros`)

### Model providers

The `provider` of a model profile is a name that the deployment settings map to a binding, an address and the key of
each tenant's workspace. On the test bench the provider of the examples (`provider-a`) maps to the mock provider. P4
serves the Anthropic Messages binding with the allowlist and the model of the slot, and reserves and charges each call.
The slot of an attempt is fixed at claim: an analysis of the role's own results (`purpose: insight`) and an aggregate
run on the role's `analysis` slot when it has one, the slot the apply lint prices their tree budget by (PRF-1), and their
calls are held to the position's `analysis_max_rank` (GW-7), and they wait for the provider pair of that slot's
profile. Any other task runs on the main slot (`base`, then `analysis`, then `seat`), whose pair is checked before a
task is picked; the farm manager seat without a charter `model_profile` also runs on the main slot (MGR-1).

### Provider pairs

P4 asks the core before each provider call and reports its outcome, and the state of each pair (tenant, model pool)
lives in the table `provider_pairs`, so the dispatcher sees it too.

- Three provider failures in a row open the circuit for 30 s; after the pause one call is the probe, a failed probe
  doubles the pause up to 10 min, a good call closes the circuit.
- A rate limit answer makes the pair wait for the time the provider gave (10 s without one).
- A configuration error closes the pair until the next `apply` and raises `alarm.provider.config`, which this build adds
  to the events catalog; a good call resolves it. A spend ceiling answer raises `alarm.provider.spend_cap`.
- The dispatcher hands out no attempt of a role whose main model pair waits, and writes the waiting reason
  `provider_unavailable` or `provider_quota` on its submitted tasks; the reserve of such a task does not count toward
  the limits (BUD-4). A task keeps the last provider reason in its record and in `task.failed` after
  `deadline_exceeded`.

### Fallback profiles

When the slot's provider answers a reason of the profile's `fallback_on` from the closed list (`overloaded` for 529,
`unavailable` for 500, a dropped connection or an open circuit of the slot's pair at the first call, `retired` for a 404
`not_found_error` on the model), P4 tries the profiles of `fallback`, in order, once each (GW-5, GW-6).

- A fallback is used only when the tenant admits it (models, providers, subprocessors, training, retention), the role's
  rank allows it and the role's eval passed on it: for `base` and `analysis` a report of the role whose static
  fingerprint is computed with the fallback in place of the slot profile, for a seat the seat's eval. Without one the
  call answers the slot's own reason. The granted slot has no fallback.
- The attestations are cached per applied manifest set.
- The reserve of the call is the maximum over the slot and its fallbacks (GW-6); a call without an answer charges the
  worst case of its own model only (BUD-11).
- The usage line of a fallback call names the fallback profile with `fallback` and the slot profile (`slot_profile`),
  and the event `model.fallback.v1` names the task, both profiles, the reason and the slot.

### Provider terms and regions

- A profile is admitted only when its human review is at most the tenant's `terms.human_review` (order `none`,
  `abuse_only`, any other value highest), `requires_retention` is refused under retention mode zero, and every profile
  subprocessor must be in the tenant's list (GW-15).
- The subprocessor region of an allowed profile's provider is a row of the "Region" table of
  `spec/bindings/model-providers/<provider>.md`, or the set fails with `region_unsupported` (a lint code) at
  `/models_allowed/<n>` (GW-14); a provider without a note (the test providers of the examples, `mock`) has no table.
- The lint `transfer_basis_missing` asks a production tenant for a `model_transfer` basis of a provider in a region
  outside the EU, read as `ci/check_manifests.py` reads it: a row of the "Region" table of the provider's binding note
  whose name does not start with `eu`; a provider without a note has no such region (GW-14).

### Workspace ceilings

The settings describe the provider ceilings (`workspaces`: per provider the organization ceiling, the ceiling of each
tenant's workspace and the test bench limit, in euros a month) (GW-9). Apply rejects a tenant whose `month_eur` is above
`share` (0.9 by default) of its workspace ceiling, all tenants together above `share` of the organization ceiling, or
the test tenants and the bench above `test_share` of it (`workspace_ceiling_exceeded`), and a tenant with a month limit
that may use a described provider without a described workspace (`workspace_ceiling_missing`). The codes are this
build's.

### Budget lines and reserves

Money is an integer number of micro-euros (`eur_micros`, D-87). Every usage line and reserve carries the budget line of
the type's purpose from `spec/model/task-purposes.yaml` (BUD-1, BUD-2, BUD-5, BUD-7).

- A reserve passes the tenant's month and day, the month and day of its line, the line's `per_role_month_eur` for a call
  of a role, the tree budget and, on the `work` line only, the person's day limit; a line other than `work` without
  `month_eur` has 0, so its tasks are refused until it gets money.
- A tool call reserves the `price_eur` of a `billing: external` tool before it is queued (0 for any other tool, which
  still gets its usage line, unit `eur_cents`); a call that does not fit gets `budget_exhausted` from P3 and the attempt
  fails with it. A toolhost that took the call commits its reserve; a call nobody took gives it back.
- `hour_eur` is not a reserve limit: the `hold` loop sums the usage of the sliding hour (reserves aside), and above
  `hour_eur` (by default `month_eur`/100) sets a `no_claim` hold of `contain_auto` with the reason `runaway_spend` for
  an hour on every agent role of the tenant and raises `alarm.hold.auto` (page), which resolves when no such hold is
  active (HLD-7).
- The 80% threshold (BUD-4): the tenant month and day windows and the person's day window give one `budget.threshold`
  event per period at 80% of the limit, without amounts. `budget.threshold` is an event of the core outside the events
  catalog of the bindings, like the task and delivery events.
- `budget.set` (MAN-5, PRM-14, BUD-1): the line is `month`, `day` or a budget line of the purposes registry (its month
  limit). The limit is in cents and may not exceed the git limit; `until` must lie in the future. A repeat replaces the
  override. The override lowers the limit only in the enqueue budget check; the grant, commission and manager-line
  readers still use the git limit.
- Review line (BUD-13): when a tenant's `insight` or `aggregate` line is above 0, apply rejects a `review` line below
  the sum over the roles of `insight.max_per_week` x 4.4 x the price of the reviews of one proposal, computed by the
  same function as the COM-6 reserve: the `tree_budget_eur` of the review type of each commission lens (veto and charter
  seat lenses) that has an executor (a fallback role, or the seat role claiming its type), 0 when a veto lens has none
  (no reviews go out), and the manager lens when the tenant works with the manager and the seat role claims
  `review.farm_manager.v1` (`review_line_too_small`, with the missing amount rounded up to a cent). doctor shows the
  shortfall as `budget.review_shortfall_eur_micros`.

### Reconciliation with the provider

A model usage line records its provider, so (tenant, provider) is a workspace (BUD-10, BUD-15).

- `farmctl spend reconcile -provider P -file FILE` reads a CSV with the header `day,tenant,workspace,eur` (the operator
  converts the provider's cost report to euros) and sends one P2 `spend.reconcile` per past UTC day, also for a day
  without rows between the first and last day (or `-from` and `-to`). Only an operator of `farm` or a report key
  (`report_keys_sha256`, which opens only `spend.reconcile` and `backup.report`) reconciles.
- A day with model usage lines without a provider (lines written before the provider was recorded, whose profile was not applied at the time) is refused with `conflict`.
  Rows of one tenant in a day add up, since the farm has one workspace per tenant and provider (the `workspace` column
  is for reference), and the provider name is compared in lowercase. A day that still has an open call reserve is
  refused until the call settles.
- The core compares the provider's day with the sum of the day's final lines of that workspace and writes the difference
  as one signed correction line (unit `correction`, `eur_micros` 0, the amount in `correction_eur_micros`); a second
  request for the day replaces it.
- The day's estimated lines (BUD-11) keep their price, get `reconciled_at` and from then on count as zero, because the
  correction carries what the provider billed for them; this is why the compared farm sum leaves them out. Per-line
  windows lose the estimated part of a reconciled day: the correction counts only in the tenant windows.
- A difference above the larger of `spend.tolerance_eur` (0.5) and `spend.tolerance_pct` (2) of the provider's day
  raises `alarm.spend.estimate_drift`; a day within it resolves the alarm.
- The `budget` loop (at most once a minute) releases estimated lines that no reconciliation reached within
  `spend.estimate_release_after` (7 days) and raises `alarm.spend.reconcile_overdue` for a workspace without a
  reconciliation for `spend.reconcile_overdue_days` (35); only a reconciliation resolves it.
- `doctor` shows the age of the last reconciliation per workspace, the usage of each budget line over 30 days next to
  its `month_eur`, and the age of each profile price (GW-17: older than 60 days warns, older than 120 days fails).

## Channels, Cards, Commands & Outbound Privacy

### Code texts and locales

Code texts come from the skeleton message catalog `spec/bindings/channels/messages/en.yaml`, read at run time like the
other skeleton files (DLV-9); other locales are `<locale>.yaml` files in the installation's text folder, and a key they
lack takes the `en` text. The recipient's locale reaches the channel as the extension field `payload.locale` of a
delivery; without it the channel takes `en`.

- Code fills the values of `notice.ai` (the tenant, and the operator from the deployment setting `operator`) and of
  `notice.privacy` (the tenant's privacy block and the default periods of `spec/model/retention-classes.yaml`). A value
  the installation does not set gets a fallback (the operator: "the operator of this farm"; the controller: the tenant
  name; the contact: the notice link; anything else: "-"), and the core logs a warning for each at start and after
  `apply`. The channel renders a parameter it does not get as "-", so a person never sees a raw placeholder. Notices
  planned before store migration 0002 get the new keys from that migration and their new values from the core at start.
- Language order (DLV-18): each delivery's `locale` is the first value with a loaded catalog: the recipient's own (a
  group chat's binding), the schedule's `locale` for its `deliver_to`, `Tenant.spec.locale`, `en`. The same order gives
  `reply_locale` in the attempt payload of `claim`. For a schedule task it is the locale of the first bound chat among
  its addresses that has one, then the schedule's; only `chat:` addresses count, and a `group:` address of people falls
  back to the schedule's locale. Otherwise it is the binding's for a group request or the initiator's, then the tenant's
  and `en`. Code replies to commands use the person's or binding's locale, then the tenant's.

### Outbound privacy

At the delivery handoff, after the recipient and hold checks, a delivery whose mark is above the ceiling of its executor
is withheld with `channel_ceiling`; to a chat, the person gets the code text `delivery.withheld.channel_ceiling` in its
place (SEC-9, INJ-11, HLD-6).

- The ceiling comes from the tenant's `privacy.channels` by driver: its `ceiling`, `internal` for a third-party store
  without one, none for a store of the tenant, and `internal` for a driver the block does not list. The monitoring
  system takes summaries only; a sink's ceiling is in its own manifest.
- Model text and result fields are searched for secrets with the toolhost's patterns; a match is withheld with
  `secret_in_outbound` and raises `alarm.outbound.secret.detected` (page) on the role that wrote it, without content.
- The second such case of one role within an hour sets a `no_delivery` hold of `contain_auto` without `until` on that
  role (only a person lifts it) and raises `alarm.hold.auto`; the handoff counts a role hold of the role whose attempt
  wrote the task.
- A delivery work item carries the extension field `dataorigin` with the task's origin class (MAN-3, DLV-13).
- At delivery the core checks the `no_delivery` holds of the tenant, of the executor's worker and of its channel or sink
  (SEC-6 sets worker and sink holds); the exceptions for exports and tenant notices are under Tenant lifecycle.

### Actions and approvals

A role with `actions_may_request` gets the built-in tool `request_action` in its catalog, offered only for the actions
its initiator may request (`action.request:<action>`); the write tool of an action is never in the model's catalog
(APR-1).

- A request is checked against the action manifest and its arguments (MAN-9), gets the hash of its arguments and returns
  an identical pending approval instead of a new one (APR-2). A task makes at most 3 requests, and a tenant gets at most
  `attention.cards_per_tenant_hour` new cards an hour (20 by default).
- The cards to the approvers (`action.approve:<action>`, the initiator excluded) are planned once the attempt that asked
  has ended, in the first channel of the tenant where the approver has an identity; the initiator gets a copy without
  buttons. A card is drawn by the channel driver from the approval record at hand-out, with the context line taken from
  fields of the result marked `x-botaru-read-by: code` (APR-3, APR-19).
- A press goes through P1 `decide` on behalf of the person (APR-4). An approval executes at most once: an approve
  creates the task `action.<action>` directly in `working` with one write call that carries the approval id and the hash
  of the arguments, and the toolhost runs it only when the hash matches (APR-5, APR-7, CNT-7).
- A write call is not leased to an attempt: when its toolhost lease runs out, the approval ends `exec_unknown` and is
  not run again.
- At half of the deadline the approvers get one reminder; at the deadline the approval expires and the cards are updated
  in place.
- A hold `no_action` (or `pause_action`) on the action, the tenant or the task type refuses new requests and cancels
  pending ones; offboarding and a revoked right of the initiator cancel them too.
- The decision event is `decision.made` with the response time and whether "Details" was opened (APR-11).

### Two approvers

An action with `approvals: 2` takes two "Approve" presses from two different approvers, neither of them the initiator;
one "Reject" ends it (APR-16). Each vote is kept in `votes`, and the card shows who approved; a reminder skips approvers
who already voted.

- Apply refuses `high_risk_one_approver`. The apply warning that a group has fewer than two approvers other than the
  initiator is only a log line, since the apply response has no notes; the doctor check `approvals.two_approvers_short`
  reports it too, and a person who may both request and approve counts as the initiator.
- A campaign card takes one press, so an action with `approvals: 2` is not offered to campaigns: a campaign of it is
  `rejected(campaign_policy_exceeded)`, a running campaign whose action gained a second approval stops at its next
  release or resume with the listed stop reason `admin` (the cause is an admin's catalog change), and the cause
  `action_needs_two_approvers` goes into the audit record `campaign.stop`. A script fanout of it is `script_invalid`.
- A script action step opens its own approval with the two votes and never runs without a card; no apply lint code fits,
  so there is none.

### Pause of approvals

A `pause_action` hold on the farm, the tenant, the bot (from the task record's `acting_bot`), the action, or the host or
resource the action writes to puts pending approvals in the state `paused` (HLD-8). The card shows that state, and a
press answers `card.answer.held`. A pause starts with the hold, or with the approval when the hold is older. The expiry
moves by the union of the hold intervals since then, with at most 7 days of pause in total; after that the approval
expires. A `bot_group` hold matches the bot groups of the worker of the asking attempt as they stand now.

### Cards in Telegram

A card is a message with an inline keyboard whose `callback_data` is a short code, the subject and the nonce; "Approve",
"Reject", "Details" and "Remind" go to P1 with `Botaru-On-Behalf`, and the answer of the core is shown as the callback
answer. The driver draws a button only when it has a label for its code (approve, reject, defer, details, remind,
accept, pause, cancel, full list), so the cards of attributions, promotion cases, audit samples and self-measured
outcomes reach Telegram as their text without buttons. A card update edits the original message (`editMessageText`); an
update whose original was never delivered is withheld with `ref_invalid`.

### Details before a decision

One generic check, `requireDetails`, reads `details_required` of the row in `decision-points.yaml`; a press of a listed
decision before the person opened "Details" of that card is refused with `details_not_opened`, writes an audit record
and answers `card.answer.details_first` (APR-14). Approvals and proposals call it after their holds and expiry.
Promotion cases (`promotion_card` "Confirm"), attributions ("Yes", "No") and the audit sample ("Lift veto") record
`details_opened` in their decision, so they read the opening with the same `detailsOpened` (only openings since their
card was shown, or since the scores were shown for the sample) and ask the same registry through `missingDetails` at the
same point, after their holds and expiry, refusing with their own audit record. When the registry cannot be read the
check fails closed.

### Card limit per person per day

At hand-out a card of a decision row with `urgent: false` waits in `planned` until the next local day of the tenant once
the person got `attention.noncritical_per_day` decision cards that day (by default 10), every handed-out decision card
counted, urgent ones and reminders included, and the manager's notices `manager_attention` and `postmortem_report`
handed out or planned for the day (APR-15, MGR-52).

- Waiting cards go out in the order of the expiry of their subject. The excess is counted in `card_excess` by tenant and
  day, without names. The count runs under a lock per person.
- The expiry of a proposal (its presentation time), a memory record (first delivery plus the card TTL) and a subject
  request counts from the first delivery of its card. It is rewritten once, in the transaction that hands the card out,
  and undone if the card is withheld there. While a card waits for the next day, its proposal or memory record does not
  expire before the card may go, and the waiting cards are ordered by that moved expiry. A subject request keeps its
  deadline. A card update of a deferred card waits for that card's run_at.
- The rows of `decision-points.yaml` decide which card kinds are limited: `promotion_card`, `attribution_card` (its
  reminders too), `proposal_sample_card` and `self_measured_card` (`urgent: false`) wait and count; `takeover_confirm`,
  `script_confirm`, `script_gate_card`, `campaign_approval` and `control_card` (`urgent: true`) count and go at once;
  notices (`manager_notice`, `manager_stopped`, `alarm_notice`, the export delivery, export and initiator notices, card
  updates) neither wait nor count.
- A promotion case's presentation and expiry and an attribution's presentation (from which its 7 days and reminders
  count) move to the first delivery of their card, and are moved past the day a waiting card may go, like a proposal's.
  The first delivery sets that presentation to its own time even when a recipient over the limit had moved it to a later
  day, so a person who opens "Details" today can decide today.
- `self_measured_card` is a later card on a proposal already presented: its first delivery has its own mark (subject
  kind `self_measured_card`) and rewrites its 7-day window in the proposal record to count from it.
- Quiet hours and the daily digest (DLV-10) follow `deferrable` in `delivery-kinds.yaml`, so none of these cards and no
  export waits for them; the digest pace (REP-10) applies to `digest` deliveries to a chat only.

### Commands without a model

- `/service <verb> <service>` runs as `service/control` does, with the person as the author of the `service_grant`; a
  verb outside `no_card` becomes an approval whose cards go out at once (PRM-18).
- `/run <pipeline> <arguments>` takes a pipeline of the tenant's `run` field: a type with `run.approval: none` is
  enqueued as a task of the person; an action with `risk: routine` runs under a `service_grant` within its
  `runs_per_hour` and `runs_per_day`; any other action becomes an approval (APR-17). `run.approval: none` on a type of
  tier 3 is `run_not_readonly` at `/tier`: without a card level 3 runs only under APR-18 or as a routine action.
- The arguments are positional; a value becomes an integer, a number, a boolean or a comma list when the schema says so,
  and the catalog lists of a person's command are those of the highest data mark the person holds.
- A person's grant creates the task `action.<a>` directly in `working` with the key of the activity, so a repeat of the
  same update finds it, and its outcome comes back to the chat as code text. The channel driver passes the text as it
  does for any command; the core parses it.

### The commands of an initiator

`/tasks`, `/status <n>`, `/cancel <n>`, `/help`, `/me`, `/lang <language>`, `/team [group] [window]` and `/about` are
answered by the core from the code text catalog, in a direct chat only (PRM-17, REP-12).

- The number of a task is its place among the person's own root tasks of the last day in the order of creation, so
  `/status` and `/cancel` of a number the person does not have answer that there is no such task.
- `/cancel <n>` answers with a confirmation word, and `/cancel <n> <word>` cancels the task, its tree and the approvals
  the tree asked for, with the reason `initiator`, in one transaction; an action task already running is not revoked
  (APR-6, APR-7). The channel passes the command as text, so the core runs the cancel itself rather than through P2
  `task.cancel`.
- `/team` counts the tasks of the group's people by type over the window (7 days by default, at most 31), only for the
  types the asker may read, and the approvals they wait for; it has no row per person, no task text and no spend lines.
- `/me` shows the groups, the titles of the types and actions the person may ask for and their data classes, and the
  person's verification code (PRM-13); it marks a group of `delegated_groups` as delegated (PRM-16).

### Weak channels

IDN-3 is read as: a privileged command is one the core runs with the sender's rights to change something. A channel with
`identity_assurance: weak` answers `/run`, `/service`, `/cancel`, `/forget`, `/grant`, `/ungrant`, `/incident` and
`/manager` with the unverified-sender text, and takes no decision; the other code commands (`/help`, `/about`, `/tasks`,
`/status`, `/me`, `/lang`, `/team`, `/mydata`, `/prefs`) stay, since they only read or set the sender's own view and
settings. A tenant command and a default type still make a task: the task runs within the person's rights like any
request and decides nothing, and an action it asks for still waits for a decision on a strong channel.

### Group chats

The operator binds a group chat with P2 `chat.bind`: a ceiling, a locale and an optional `name` (by default
`<channel>-<external chat id>`); a ceiling above `summary` needs a channel worker whose driver reported
`membership_assurance: enforced` (DLV-7, DLV-15, PRM-15, PRM-16).

- An unbound group is ignored. In a bound group a message from a person outside the tenant is ignored and marks the chat
  as having an unknown member, as does a channel `fail` with `unknown_member`; then every delivery to the chat is
  `withheld(unknown_member)` until the operator binds it again.
- Only `/help` and `/about` run in a group; every other command gets the text that it is given in a private chat. Free
  text takes the first `default_types` entry for `group` or `chat:<name>`.
- An answer is split: the group gets the type's `summary_fields` whose schema closes the value (numbers, booleans,
  enums, consts) as code text within the ceiling, then "details sent privately"; the full answer goes to the asker's
  direct chat. The notices of the first contact go to the asker's direct chat too. Without a direct chat in that
  channel, or when that message or the notice before it dies, the group gets a request to open a private chat and no
  content.
- Unlike MEM-1, a group conversation and its session are kept per person: a session holds one person's turns, and a
  reply in the group to the bot continues a task of anyone who may read it.
- A tenant admin with `admin:people` grants and removes groups from a private chat with `/grant <person> <group>` and
  `/ungrant`: a group from `delegated_groups` or an ordinary group whose rights the admin holds; a delegated grant tells
  every admin privately.
- `chat.bind` takes `model_text: summary_only` (the default) or `labeled`, and refuses any other value with
  `input_schema_invalid` (REP-2): a reporter digest to a group bound `labeled` carries the model paragraph of an attempt
  that saw untrusted text, with `labeled: true` in its body, and the channel draws the code line "Bot text, check the
  numbers against the table." (`digest.labeled`) before its sources; a group bound `summary_only` gets the table only.
- `chat.bind` of a chat again clears its leftovers of offboarded people (IDN-14). The chat command `/invite` is not
  built.

### Activity from a non-principal

A message in a direct chat or a bound group from a channel identity that is no principal of the tenant creates no task,
file or session (MSG-17); the audit gets `activity.ignored` with reason `not_subject`, the identity and the chat, and
code answers with `notice.ai` and `notice.privacy` (in a direct chat also `notice.not_registered`) at most once per the
binding's `notice_repeat` (90 days by default) for the pair (chat, identity).

### Notification preferences

`/prefs` in a private chat shows or sets `quiet HH:MM-HH:MM`, `quiet off`, `digest daily` and `digest off`; a person
with `admin:people` names another person first (DLV-10). Only deferrable kinds wait: until the end of the quiet hours,
or with a daily digest until the person's slot, which is the end of the quiet hours or 09:00 local time. They go out
together at that slot as separate messages, not merged into one. Under profile `monitor` with the monitoring system
configured, alarms reach people only through the monitoring system, which sets their silencing and quiet hours (MON-1):
the core plans no `alarm_notice`, and `/prefs` adds the code line that alarms come from the monitoring system
(`cmd.prefs.show_monitor`); otherwise `alarm_notice` is never deferred.

### Digest pace

`chat.bind` takes `digest.max_per_hour` (1 to 60) (REP-10). A digest over the ceiling of its chat (by default 1 an hour
for a group and 4 for a private chat, counted by hand-out time) waits for the next free slot. A newer digest to the same
chat takes the slot of the waiting paced one (`deliveries.paced`), which ends `withheld(superseded)` once the new one is
planned; that is the closest registered reason. The ceiling applies again at hand-out, so digests released together by
preferences are paced too: of the waiting paced digests of a chat only the newest is kept. Only digests are ever
replaced.

### Messages between bots

`MessageType` manifests name a genus, a closed field schema, a class, a delivery, an expiry, a size, a note policy,
producers and a rate (MSG-1..MSG-15, MSG-18, STA-1, BGR-1, BGR-2).

- An agent sends a `report` with the built-in `send_message` (types and addresses are enums from its role's
  `messages.send`, offered only when the initiator and every initiator of what the attempt read hold `msg.send:<type>`);
  a `job` attempt uses P1 `msg.send`; worker code with its own token sends only `state`. The core writes every envelope
  field, keeps the note in its own column of class `content`, and audits each decision with metadata only.
- What an attempt sends stays `staged` and is published in its `complete` transaction, where what it read is
  acknowledged; a failed, canceled, preempted, held or fenced attempt publishes nothing and returns what it read, a fail
  or lost lease counting a hand-out (the third makes the entry `dead`), the others not.
- An `all` delivery becomes a copy in the inbox of each active worker of a role with `instance_inbox` (by default a job,
  toolhost or channel role), at most 50.
- A reader takes entries from its role's inbox, its bot groups' and its own, in number order, under its lease; an entry
  above its mark only counts in `hidden`. Reading an untrusted entry sets `untrusted_seen` with the source, raises the
  depth floor to the entry's hops, and adds the entry's initiators, which C2 then intersects in `enqueue_child`,
  `request_action`, `send_message` and every enqueue of the tree.
- The `inbox` loop settles entries of ended attempts, expires entries after their period (24 hours by default, at most 7
  days) and marks state values stale.
- Limits: three sends per attempt, the type's rate (at most 120 an hour), 8 KiB envelopes, the type's `max_bytes` (at
  most 4 KiB), notes up to 500 characters, five hops, 100 entries per worker inbox and 1000 per role or group inbox with
  a 25% share per sending role (`inbox.flood` once an hour).
- A bot group is computed from the role and the labels recorded at enroll; a blocked or revoked worker is in none. An
  apply that takes an inbox away ends its live entries with `recipient_gone`; an ended entry leaves the store with its
  fields after 30 days.

P2 views of messages (MSG-18): `msg.list` (filters by type, role as sender or role inbox, state and a window; 200 rows a
page, the cursor is the last row's id and store number), `msg.show` and `inbox.status` serve the operator of the
message's tenant, and the operator of farm for any tenant; another tenant's message or inbox and a nonexistent one are
the same `not_found`. A tenant operator credential counts as that tenant's operator, as in the other P2 views, so these
three ask for no `admin:people` permission on top of it.

- A row is metadata: id, type, genus, address, sender role, state, reason, times, size (envelope and note bytes) and
  hand-outs. `msg.show` adds the fields whose schema in the message type is closed (enum, const, `enum_from`, numbers,
  booleans, strings of the `id` or `type_name` format, and objects and lists of those), the hops and the flag, and of
  the copies of an `all` message shows the first.
- The note comes only when the credential's principal is an active person of the message's tenant with `audit.read` and
  `data:<mark>` for the message's mark, wrapped in a random untrusted mark (which may take it past the 500 characters of
  the schema), and that read writes an audit record of kind `read`.
- `inbox.status` lists every role that receives a type or has entries, with the counts by state, the age of the oldest
  queued entry, the capacity and the tenant's wake-ups left today (wake-ups are counted per tenant).

### Routes from messages

A `Route` of a tenant does one of three things on messages (MSG-5, MSG-11, MSG-12, MSG-16, INJ-7):

- `on: inbox` with `mode: task` wakes a role or bot group: the `inbox` loop enqueues the route's type (a type of
  `system-ops`) as `system` with the key `system:wake:<route>:<window>` (a window of at least 5 minutes, `debounce` may
  widen it) when at least `min` live entries wait and no wake-up of the route is still open. The task's depth is the
  highest hops of the entries, its correlation the root of the oldest entry, its `start_by` no later than their latest
  expiry; it joins the tree of the oldest entry (its root, correlation and tree budget), takes the entries' origin
  initiators as co-initiators, and its spending counts in the personal day limit of each of them. The tenant's daily
  wake-up money per inbox (`messaging.wake_day_eur`, by default a tenth of the day budget) and number
  (`messaging.wakes_per_day`, at most 200) can only be lowered; past either the wake-up is skipped with `wake.skipped`
  (`budget_exhausted` or `service_rate_limited`) once per window and the entries wait.
- `on: message` with `mode: notify` sends the code text `route.<template>` with the closed fields, type and subject (a
  `subject_prefix` filters) to `deliver_to`, only for an entry the code wrote and nobody distrusts (delivery kind
  `route_notice`).
- `on: task_done` with `mode: report` publishes, in the transaction of the task's end, the type's closed
  `summary_fields` as a report of the core with the task's mark and flag and the key `system:report:<task>:<route>`; on
  an agent task its origin is `model`; system, the task's initiator and its co-initiators need `msg.send:<type>`; a
  refused report is a `message.rejected` event and the task ends anyway.

Each published message writes a usage line of unit `message` with its size and type into the sender's tree. `apply`
refuses a pair (type, address) without a receiving role, an `all` delivery to roles without an instance inbox, an
address to a role of a profile that takes no messages, a type the role's kind of sender does not produce, an
`inbox.read` the role does not receive, a route whose mode does not fit its source, a notice from a type only the model
writes, an auto-report type the core does not produce, `inbox.read` on an `alarm.*` type, and a message type above the
ceilings. Routes on events are under Source channels and routes on events.

### Reporters

A task type of purpose `report` gets its audience at enqueue (REP-2, REP-3, REP-4, REP-6, REP-7, REP-8, REP-9): the
ceiling is no higher than the type's class, the highest class the initiator holds and the ceiling of each target (the
origin chat, or a schedule's `deliver_to` chats and sinks); the principals whose permissions filter the views are the
person asking, every tenant person with an identity in a group chat's channel (the core does not list a group's members,
so all count), or the members of a schedule's groups. The task's mark is that ceiling.

A role of profile `reporter` gets the built-in views of its `views`, all answered from one snapshot of the task taken at
the first read; a row the audience may not read is left out uncounted, a row above the mark counts only in `hidden`:

- `status.tasks`: the tasks of the window `1h`, `24h` or `since_last`: type, title, state and reason only, and counts by
  type and state.
- `status.proposals`: proposals from `recommended` on, never an audit sample item.
- `status.workers` (PRF-1, REP-8, STA-3), computed only for a task whose role has that view: the tenant's workers that
  are not revoked, up to the row limit, each with what the core observes (presence `connected`, `stale` past 120 s
  without a call or `gone` when never seen, lifecycle, active leases, last-seen age, the ids of active holds on the
  worker or its role). STA-3 is deferred, so `reported` and `mismatch` are null today. The self-report column is
  computed only while STA-3 is offered: the phase of the worker's last `state.worker.v1` message with
  `source: state_message`, beside it one mismatch code, the first of `state-mismatch.yaml` in its order whose condition
  holds; `reported_idle_not_claiming` means a task of a type the role claims has waited in `submitted` for over 120 s
  while the worker took no lease for as long. Workers carry no mark and no content of people, so the view shows them to
  any audience of the report, a group chat's too; only ids, closed codes and numbers. The worker's own state reaches the
  reporter only through this view, never through its inbox.
- `status.campaigns`, computed only for a role with that view: the campaigns changed in the window or still live (for a
  campaign report only that campaign, for a root report only the campaigns of that tree): kind, item type, title, state,
  item counts, pause and stop reasons; a campaign is read as its item type (`task.read:<type>`, for an action campaign
  `task.read:action.<a>`) with its mark, and one above the mark counts only in `hidden`.

A reporter's `inbox_read` takes only `report` entries. Its result is not delivered as is: at its end the core plans a
`digest` delivery to the origin or the schedule's addresses only, with the code table and the hidden count from the
snapshot and the model paragraph as a separate model part, which code lets through only as plain text up to 800
characters without links, mentions, markup, commands or object ids. A group chat gets it only from an attempt that saw
nothing untrusted (or a group bound `labeled`), a direct chat gets it with its sources. Digests are paced (REP-10, see
Digest pace).

Before delivery code reads the paragraph by tokens (REP-4):

- A token with a digit (any Unicode number: superscripts and fractions too) passes only as a whole one-word name of the
  digest's table (a type, title or state) that also holds letters, or as an exact name of the tenant catalog (the ids
  and names of its lists, and `host:volume`), never as a word of a multi-word name, or as a date (`2006-01-02`), a time
  of day (`15:04`) or an instant equal to the snapshot time or the window start, in UTC or the tenant's zone.
- A word of `number.words` of the recipient's locale (the person's own, else the tenant's, else `en`), alone or as part
  of a compound split at any punctuation (`twenty-one`, `one/two`), never passes.
- One rejected token drops the paragraph from that delivery, which then has the code table only, and writes
  `digest.text_dropped.v1` with the counts.
- A group summary carries no model paragraph (its text part is the closed fields), so it has nothing to check.

### Source channels and routes on events

`Channel.spec.direction: in` and `spec.ingress` are accepted only while the installation claims the profile `ingress`
(ING-1 to ING-4, PRM-10, MSG-16, INJ-7). `botaru-ingress` is the webhook driver of one source channel: a TLS listener
under its own OS user and secrets that checks the Standard Webhooks signature or the TLS token, the rate and body limits
and the delivery id, then puts the body as a file and submits the mapped event over P1.

The driver:

- submits one `submit_activity` per authentic delivery with the sender `hook:<source>` (an active service principal of
  that identity, else `identity_unknown`), the chat `direct:<source>`, the delivery id as `update_id` and the body as
  its one attachment;
- claims a delivery id before it puts the body: an identical delivery in flight is answered `503` with `Retry-After`; it
  remembers the last 4096 delivery ids;
- counts only authentic deliveries for the per-source rate; its listener ceiling defaults to 10 times the source's
  `per_minute`;
- always requires TLS; `-secret-files` and `-source-token-files` take two files during a rotation.

The core:

- keeps the delivery under `system:ingress:<source>:<id>` until its `keep_until`, deleted by the retention loop and at
  `tenant.offboard`. That is a day for a signed source, whose deliveries the driver refuses after the tolerance, and the
  default content period (30 days) for a tls_token source, which can be replayed at any time.
- answers a delivery whose routes enqueued tasks with `task` and the first of them in route-name order; a repeat answers
  the same task with `existing` (the row keeps `first_task`), and a delivery no route took answers `ignored`. A repeat
  gives no second event.
- drops the body file of a repeat or of a refused delivery (state `expired`): only a stored upload of the hook put
  within the last 10 minutes is dropped, and only when no kept delivery and no task's attachments name it. The body is
  an upload of the hook principal, so it is declared `personal_seen` as any service upload is (ART-6).
- refuses a field outside the mapping with `input_schema_invalid`, and drops a mapped value that fails its schema. The
  event's `source` is `botaru:ingress/<tenant>/<source>`, and its `subject` is the delivery key.

Routes on events: a route task has the key `system:event:<route>:<source>:<event id>`. It is enqueued as `system` by
`route:<name>` and gets the event's fields that its `params_schema` names, overlaid by the route's `params`; the body
file is attached unless the type sends `alarm.*` events.

- Each family of routes on events runs only while the build claims its profile: `ingress` for `ext.*` events of a
  source, `monitor` for `ext.monitor.*` (MON-5, with the monitoring system configured), and `messaging` for events a job
  publishes (MSG-16). The build claims all three.
- A chain of route tasks stops after 3 hops, counted from the root task's record. A chain that a tls_token source began
  keeps that mark on every hop, so each of its routes reaches only tier 0. A redrive (DLV-4) copies the hop count and
  the mark to the new root.
- A route the guards stop, or whose enqueue is refused, writes an `authorize` record with the decision `deny` (reason
  `type_not_allowed` for a guard).
- The subject of an external event is its key, never a catalog id, so `on.subject` on an `ext.*` route never matches;
  apply notes this in its log (`route_subject_external`), since the lint codes have no note for it.
- A route task delivers its result to the route's `deliver_to`; only string addresses are used.

## Sessions, Memory, Knowledge & Erasure

### Sessions and person memory

Sessions and memory records live in the farm store; conversation memory, sessions and transcript compression are core
stores, not the runtime's (D-93) (MEM-1..MEM-3, INJ-13).

- At claim a task with a conversation gets a `session` handle for the session `(tenant, role, conversation)`, whose
  subjects grow with each person who wrote there. P1 `session.load` and `session.append` take the handle as the bearer,
  so the model sets neither the space nor the key.
- An attempt that saw untrusted text marks the session, and a marked session marks the next task of the conversation.
- Person memory (MEM-2): while the personal assistant is deferred (DSP-10, `spec.Offered`), `apply` refuses a role with
  `memory: person` with the reason `person_memory_deferred` of `spec/model/reasons.yaml`, and the skeleton lint refuses
  it with the same code. `examples/roles/assistant` has `memory: none`, and flow 8 does not search the person space. The
  fixture cases of the memory lints allow `person_memory_deferred` beside their code (`also`). Both lints read the same
  version: the core's claim `spec.Version` is the major and minor of `spec/VERSION` that the skeleton lint reads, and a
  test keeps them equal.
- The person memory mechanism stays for the return of DSP-10: a role stored with person memory keeps it until the next
  apply, and the Go tests of that mechanism give the stored roles person memory after apply (`storedPersonMemory`). For
  such a role working for a person the attempt gets a `memory` handle for that person's space; `memory.search` and
  `memory.propose` take it as the bearer. A proposed record goes to its person as a `memory_record_card` and becomes
  active only by their Accept; silence for 7 days discards it. An attempt that saw untrusted text proposes nothing.

### Facts

A task type with `memory: {facts: propose}` on a role with `memory: bot` may hand in up to `memory.facts.per_attempt`
(3) fact proposals in the result field `facts`: only the kind, the predicate and the subject (MEM-6, MEM-7, MEM-9).

- The core takes them out before the result check and, in the transaction of a successful `complete`, takes the 14-day
  window and the evidence by the predicate and computes the value. A proposal that fails the schema or the registries,
  names a subject outside the tenant, rests on evidence the role does not see (the attempts of a type it claims, or a
  series it reads) or on a predicate the core does not derive (`not_found`), or has too little evidence
  (`fact_insufficient`), leaves only the event `fact.rejected.v1`. A seat attempt writes no facts.
- The core derives only `task.duration.p95_min` (p95 of `farm.attempt.duration`, at least 10 points); `task.fails_with`
  needs the failure reasons of attempts, which the core series do not keep, and it and the other predicates are refused.
- A fact carries the mark of its evidence, `untrusted_origin` from the attempt and `personal_seen`; a space keeps at
  most `memory.facts.max_active` (100), the one used longest ago leaves first. A newer fact of the same predicate and
  subject replaces the older one.
- A type with `memory: {read: bot}` gets the active facts of the acting identity in `facts` of the attempt payload
  (kind, predicate, subject, value, unit, window, age; never the author), only those its mark covers and, for `review`
  and `manage` and for a role with `read_bot` memory, only those of trusted origin.
- When an identity retires or moves to a seat, its facts are rechecked and offered to the successor (the worker's new
  identity, the next holder of the seat, or the first identity enrolled in the role before the seal ends; none when the
  role left the catalog; a new seat holder's facts reach only the seat, never its worker's new identity). The copies
  carry `inherited`, the most recently used first while the successor's space has room, and a role without bot memory
  gets none. A retired identity's facts are sealed and wiped with it at the end of the seal; a waiting offer expires
  then.
- Not built: counts of facts in P2.

### Retired runtime sessions

Retiring an identity whose worker declared `session` or `memory: runtime_internal` at hello enqueues in the same
transaction an erasure request with reason `bot_retired`, the recorded scope (tenant, bot) of the retired identity, and
the worker it ran on in its own column of the request (`deletion-request` has no scope of a bot and a worker, and a new
oneOf branch would not be additive) (MEM-9).

- Its one step is the purge of that runtime, and the work item's scope names the identity in `scope.bot` (an additive
  field of `erasure-work`, in the E4 sheet), so the runtime erases only that identity's sessions and memory and not
  those of the successor that `bot.retire` binds to the same worker. `botaru-agent` keeps no session of any identity, so
  it answers done.
- This request leaves the tenant's tool cache alone (it reaches only the runtime) and does not end the identity: its own
  request at the end of `seal_ttl` (scope bot, memory only) still erases its core data.
- An unbound identity or a revoked worker gets none: its `worker_revoke` request already covers the runtime.

### Files

P1 `file.put` takes a multipart body (a `meta` part and a `content` part) and stores the bytes in the store with a
record only the core writes: a `file_ref` of 128 random bits, the origin, the producer, the media type found from the
bytes, the size, the sha256 (never handed out), the mark and the expiry, the smaller of `retention.files` and the type's
`result_ttl` (ART-1, ART-2, ART-3, ART-6, PRV-2).

- A channel puts a person's attachment without a lease, on behalf of the person: the mark is at least `internal` and at
  most what the person may see, and a `service` principal with `uploads_personal` (by default on a production tenant)
  gets `personal_seen: declared`.
- A worker puts a produced file under the lease of an attempt or a tool call: the mark is not below the task's, and the
  core writes the provenance block (task, fingerprint, attached files, tool calls, time). A result with a `provenance`
  field is refused, and the lint rejects such a result schema.
- `file.get` hands out the bytes under the lease of an attempt, a tool call or a delivery, within the recipient's right
  and the ceiling; an attempt that reads a file makes its task untrusted and raises its mark. Reading a person's upload
  marks the task `personal_seen` (`human_file`), reading a file with the flag `inherited`.
- An activity's attachments must be the sender's own uploads; any attached file marks the task untrusted and gives it
  the file's mark.
- A put that would take the tenant's stored files past `limits.max_storage_bytes` is `over_capacity` (CAP-1); under a
  tool call only a sandbox toolhost puts a file; a file of a `kb.ingest.v1` or `data.arrived.v1` job is `ingested`.
- The `retention` loop moves an expired file to `purged`. Erasure purges the bytes and keeps the record as a tombstone.

File limits and result types (ART-4, ART-5):

- The farm ceilings are 32 MiB for one file and 256 MiB for the files of one task (`files.max_file_bytes`,
  `files.max_task_files_bytes` in the settings, also their defaults); a tenant's `limits.max_file_bytes` and
  `limits.max_task_files_bytes` lower them, and 0 or no field keeps the farm ceiling. The task total counts the files
  the task produced that are not purged, under a task lock. A body or part above the limit is `file_too_large` (422, as
  the binding gives every refusal).
- The sniffed `text/plain` is refined by the file name only to `text/csv`, `text/tab-separated-values` and
  `text/markdown`. Active formats are HTML, XHTML, SVG, the JavaScript types, `text/xml`, `application/xml` and every
  `+xml` type.
- A delivery hands out only a file produced by its own task, by one of its ancestors, by a finished descendant, or by an
  earlier task of the same tree with the same initiator (such as a chain's child whose outcome a continuation reports; a
  task of the tree another principal started is not), the file's mark still within the delivery's, whose type the
  producing task's type lists in `result_media_types` and that is not active, else `media_type_not_allowed`. An uploaded
  file is never handed out this way; an export file goes only with its export delivery.
- The skeleton lint has no code for an active type or a PDF of a non-job type in `result_media_types`, so `apply`
  refuses it with the reason code `media_type_not_allowed`.

### Knowledge corpora

All of it runs only while the core claims the profile `knowledge` (KNW-1 to KNW-5, PRV-3, D-97). KNW-6, which fixed the
inner word search of `kb_search` and `kb_read`, is deferred: this core chooses its own word search, described here.

- Index: the per-tenant word index is in PostgreSQL (`corpus_fragments`, a `tsvector` of the `simple` configuration over
  title and text, with a GIN index); there are no embeddings. The query is cut by the same parser as the index
  (`to_tsvector('simple', ...)`, so an email address or a URL is one word on both sides), words of at least 2 characters
  and at most the first 16, with no operators. The store finds the fragments that hold every word, takes at most the
  first 2000 of them by document id and fragment number, and orders those by the number of occurrences of the words,
  then document id and fragment number, before the limit. `kb_read` gives up to `count` fragments of one document from
  the named one on; an unknown fragment, one of another corpus, tenant or a deleted document all answer `not_found`. The
  query text goes into no answer, log line, event or audit record: the audit line of a call (kind `read`) holds the
  tool, the corpus and the number of fragments.
- Scope: `kb_search` and `kb_read` are built-in tools of the `worker` and `manager` profiles in tasks of purpose `work`,
  offered when the role has a corpus in scope. A corpus's class is its `data_class`, or `internal` (the class of an
  attachment) without one; it is the class of everything in the corpus, a person's upload included (an upload through
  `kb.upload` into a corpus declared `summary` is stored as `summary`, unlike P1 `file.put`, which floors a person's
  upload at `internal`), its documents are stored with that mark, and it is the output class of both tools (PRM-5). The
  skeleton has no role field for corpora, so the scope is every corpus of the task tenant's catalog whose class the task
  mark covers, narrowed to the task's resource scope when it has one; the `corpus` enum of `tools/list` is that list. A
  `no_tool` hold on the farm, tenant, role, type, worker or the corpus resource refuses a call. The output is a JSON
  data block in a wrapper with a random mark; an `ingested` corpus sets `untrusted_seen` with the source `corpus:<id>`,
  a curated one does not, and `personal: true` adds `personal_seen` `declared`. Built-in tools are never cached
  (PRM-12), so no cached output holds a fragment.
- Corpus defaults: a corpus without `trust` is `ingested` and one without `source` is a git corpus. The lint refuses
  `trust: curated` with another source (`corpus_curated_source`).
- Documents: a document is a farm file record of its tenant with a `corpus_documents` row whose object id has the prefix
  `kd` (the `file` of `kb.index_put` and of fragment ids `kd_<ULID>:<n>`). P2 `apply` carries the extension field
  `knowledge`: per git corpus the files of `tenants/<t>/knowledge/<corpus>/` (`farmctl apply` reads the folder beside
  the tenant's catalog file); a folder for a corpus that is not a git corpus of the tenant fails the set with
  `not_found`. A new or changed document (by the SHA-256 of its bytes) becomes a file of origin `ingested`, producer
  `git`, with the corpus class as its mark and the tenant's file expiry; a document that would take the tenant past
  `max_storage_bytes` (CAP-1) is left out with the apply warning `over_capacity` on its path, and an older version of it
  stays. A document gone from its folder, every document of a corpus that left the catalog or changed its source, and a
  changed document's old version are deleted. A folder the apply does not carry keeps its documents. A git document's
  file expires like every file, by `expires_at` and the tenant's file period (RET-9, RET-10 name the corpus fragments
  among what expiry deletes), so it is not exempted and its expiry is not pushed forward; the expired document leaves
  search, and the next apply that carries its folder brings it back as a new document with a new ingest.
- Upload: P2 `kb.upload` (no skeleton schema: `tenant`, `corpus`, `filename` and the bytes as base64 `content`, within
  the P2 body limit of 4 MiB) is a person's upload into a corpus of `source: upload` of the credential's tenant. The
  credential's principal must be an active person with `kb.write:<corpus>` and `data:` for the document mark, else
  `forbidden`; a git corpus or another tenant is `forbidden`, an unknown corpus `not_found`, and the tenant's file limit
  and storage ceiling hold (`file_too_large`, `over_capacity`). Every refusal writes an audit record. The upload takes
  the apply lock shared and reads the catalog from the store in its transaction, so it never writes into a corpus an
  apply is removing. `kb.write` is not privileged (PRM-13).
- Ingest: each document gets a `kb.ingest.v1` task enqueued as `system` (the tenant's `system-ops` group needs
  `task:kb.ingest.v1` and the document mark) with the idempotency key `system:kb.ingest:<document>` (`:<n>` from the
  second job on). A refused enqueue, and an ingest task that fails, is canceled or completes without `kb.index_put`,
  leave the document `failed` with the reason (`fail_reason`); the next apply enqueues every failed document of a live
  corpus again. The attempt carries the extension field `kb_document` (`id`, `corpus`, `file_ref`). P1 `kb.index_put` is
  accepted only from a job worker under the active attempt lease of the document's ingest task, for its corpus and
  document, while the document is live and its corpus has the same source, and never for a non-git document of a curated
  corpus; anything else is `forbidden`, with an audit record, and writes nothing. A call replaces the document's
  fragments. `botaru-kbjob` reads the document with P1 `file.get`, joins paragraphs up to about 1200 characters, splits
  longer ones at a space, takes Markdown headings as titles, and skips a non-UTF-8 document as `unreadable`, one without
  text as `empty` and one of more than 2000 fragments as `too_large`.
- Deletion (KNW-5): deleting a document marks it deleted (search stops at once), deletes its fragments directly and
  files an erasure request of its file refs with the reason `manual` and the applying operator as the author, which also
  drops the tenant's tool cache. The request is kept on purpose: removing a document from git and applying it is an
  operator's manual deletion of a file before its period, which RET-7 allows only through an erasure request, and
  `manual` is the reason of its closed list that says so. A store trigger deletes a document's fragments whenever its
  file leaves `stored` (the retention loop's expiry, an erasure), in the same transaction.
- Not built: upload into a corpus through a channel.

### Skills

Skills belong to the profile `knowledge`: while the build does not claim it (a test override), `hello` does not look at
`skills`. SKL-4 (`skill_read`, `runtime.skills: farm`) is deferred: `skill_read` is never offered and a call to it is
refused like any unknown tool, and a `hello` with `skills: farm` answers `version_unsupported`.

- Skill files (SKL-1): P2 `apply` carries the extension field `skills`: the files of every folder of the farm git's
  `skills/` with their bytes and mode (`file`, `executable`, `link`); `farmctl apply` sends them when a role of the set
  pins skills. The core lints each folder (`skill_format` with the reason in `detail`): only `SKILL.md`, `references/`
  and `assets/`, no executable or link, types `.md`, `.txt`, `.yaml`, `.json`, `.csv`, UTF-8 without NUL, at most 50
  files, `SKILL.md` up to 32 KiB, the tree up to 256 KiB, and front matter by the skill schema whose `name` is the
  folder name. It keeps the passing folders by name and tree hash (`skill_trees`).
- The tree hash follows `spec/hashing.md` (byte order of the paths, `{path, sha256: "sha256:<hex>"}` objects, the
  canonical form); the example pin of `examples/roles/ops-reader` comes out of its folder. A role pin with no copy of
  its hash, or with another description than the front matter, is an apply warning (`not_found`, `schema_invalid`).
- Skill delivery (SKL-2): skills reach a runtime only as files: a run of a role with skills gets `skills` (the refs) and
  the extension field `skill_files` (the files of each ref by path, from the core copy whose recomputed tree hash equals
  the ref). An agent worker of a role with skills must declare `runtime.skills: runtime_files` at `hello`; `farm` or no
  declaration is `version_unsupported`. `claim` checks the declaration of the worker's last hello again: without it the
  role's tasks get no attempt and wait with `no_member_ready`. The skills are looked up only once a task is picked;
  while a pinned skill has no matching copy the tasks wait with `skill_missing` (an additive entry of
  `waiting-reasons.yaml`) and the core logs a warning.
- `botaru-agent` declares `runtime_files`, checks each tree hash, writes the skills into a new read-only directory per
  attempt (only the role's skills), names them with their `SKILL.md` text in the system prompt, and deletes the
  directory after the attempt. A skill changes neither the tool catalog nor the task's tier or mark (SKL-3).

### Erasure

Every early deletion is an erasure request with a step per store, recorded before anything is deleted (MEM-4, RET-1,
RET-4, RET-7, SEC-11).

The `erasure` loop erases the core stores itself, a live task in the scope canceled first:

- task body and result (the row stays as a tombstone);
- script runs: a live one is canceled, then the run keeps only a tombstone with its root, its steps lose their result
  fields and untrusted sources; script runs go before campaigns, and the campaigns of a run's steps are marked purged
  before the run is canceled;
- campaigns: a campaign of the scope is marked purged and loses its template's goal, parameters and arguments; a live
  one is then rejected or stopped with the reason `admin` (the closed list of campaign stop reasons has none for an
  erasure, and the erasure request is the audit trail); its pending items are skipped, so none is released, and it sends
  no summary card and enqueues no `then` continuation, which is recorded as `continuation.refused` with the reason
  `gone`. A script run outside the scope whose step's campaign is in it sees that step end with the campaign's stop and
  goes on by its `on_failure` and `run_if`, as after any stop of a campaign. Its items lose their arguments and result
  fields, and an item ending later records none; "Retry failed" answers `already_terminal` for a purged campaign; every
  path that strips items marks the campaign purged, and empty arguments alone are normal for a type without parameters;
- delivery bodies, tool call arguments and output, sessions, memory records and file bytes.

An `erasure.match` does not search script runs or campaign items; their content goes with the person's or the tenant's
scope and by period.

Stores outside the core:

- A channel whose driver reported `stores_content: none` is `not_applicable` (`declared_none`). Any other channel (a
  cache, saved attachments, or no declaration yet), a sink and a monitoring adapter get an `erasure` work item through
  claim, with the scope and the person's external identities as needles, and complete it with `erasure-outcome`; a store
  that lacks the operation answers `failed` and the request stays open (RET-11). A channel step with no non-revoked
  worker of its channel that is not stale (OBS-4) fails `not_ready` at the next tick.
- Every agent worker of the tenant that ran an attempt of it and is not revoked gets a purge step of its runtime (E4,
  `target.kind: runtime` with its worker id) for every reason except `bot_retired`, `forget` included. A runtime step
  whose worker is revoked or stale (not seen for two long-poll periods, OBS-4) fails `not_ready`, so a runtime that is
  gone never holds later requests open; `erasure.retry` runs both again once the worker is back. The reference agent
  claims erasure work before attempts and answers `done`, because it keeps no working files, caches, transcripts or logs
  with content after an attempt.
- A runtime that keeps sessions or memory itself (`runtime_internal`) passes hello only when it declares both `erase`
  and `match` (RT-5); it is then a store the core does not read: a person's `erasure.match` gives it a match step
  (`target.kind: runtime` with its worker id) and the found ids get erase work like a channel's; at the person's
  confirmation such a runtime's step without a match answer fails, as unmatched is not "not found".

Closing and upgrades:

- The scope `(tenant)` reaches every session of the tenant (C3 purge, LCY-1).
- The request is `done` when every step is done or not applicable; an open request raises `alarm.erasure.overdue` at
  `privacy.subject_request_days` (30 by default) and drops its needles when it closes.
- Retention classes of the core stores: tasks, deliveries, tool calls and sessions `content`, memory records `memory`,
  the erasure log `tombstone`.
- An open request whose steps cover fewer core stores than the current build gets the steps of the stores added since
  (script runs, campaigns), and an offboarding recorded without steps gets them at the first tick; its sink and monitor
  work carries only the scope, since its identities were already deleted, and one older than the deadline raises its
  overdue alarm at once.
- A person's erasure also purges the person's own export copies (principal exports) with the person's files; a done or
  failed export row goes by the metadata month only once its file has no bytes left and no delivery of it is still open,
  so that erasure always finds the copies. The sealed file of a tenant export that the core still holds is not purged by
  one person's erasure: it is encrypted to the tenant's export recipient, the sink has its own erasure step, and it goes
  by the file period; a tenant-wide erasure purges it.

### A person's data

- `/forget` erases the person's memory and sessions (reason `forget`) (SEC-11).
- `/mydata` shows what the farm keeps in its memory. `/mydata erase` and P2 `principal.data_request` record a request
  that a person with `admin:people` other than its author decides on a `subject_request_erase` card; the approval runs
  an erasure with the reason `subject_request`, and the person gets the result per store. The reminder goes at half of
  the deadline and `alarm.subject_request.overdue` at the deadline; silence never runs or closes the request.
- P2 `erasure.match` searches the tenant's content and memory for exact strings, asks every sink, monitoring adapter and
  channel that did not declare `stores_content: none` for a match, keeps only the ids found and shows only their count
  per store on the card. The erasure of an approved match records the ids of each store and which stores found nothing
  (a store whose match failed is absent).
- P2 `erasure.request`, `erasure.status`, `erasure.list` and `erasure.retry` serve the operator; offboarding goes
  through the same request.

### Retention

`Tenant.spec.retention` (`classes` of `content`, `memory` and `metadata`, and `files`) is a field of the claimed
version; `journals` stays refused with the debug profile (RET-2, RET-3, RET-9, RET-10, MEM-10, BUD-16, SEC-14). The
`retention` loop (at most once a minute) deletes in batches by the tenant's periods as they stand at each run, without
an erasure request. A defaulted class takes 30 days for content and 400 days for metadata. The loop works by age only,
so after a restore it deletes again what the backup brought back.

Content, by the content or file period:

- the body and result of a finished task, the body of a finished delivery, the arguments and output of a finished tool
  call;
- a session unchanged for the content period (7 days for a role without memory, or the content period when shorter);
- the bytes of a file past its expiry or older than the tenant's file period (a file's name goes with its bytes, ART-5);
- an ended bot message entry, a status view snapshot;
- a finished script run (it keeps a tombstone with its root; its steps lose their result fields and untrusted sources; a
  press on one of its cards answers `already_terminal`);
- a finished campaign (its template's goal, parameters and arguments, its items' arguments and result fields; it is
  marked purged and "Retry failed" answers `already_terminal`);
- the note of a finding, the author note of a closed proposal, the note of an ended model grant request or escalation, a
  sent proposal digest;
- a cached tool output (at its own expiry or past the content period, whichever is first, PRM-12);
- only when the tenant sets a `memory` period: a person's memory record (by its last change, with the mark of its card's
  first delivery, APR-15) and a fact (by its last verification, the greater of its creation and `verified_at`), a sealed
  fact of a retired identity too; an open fact offer drops a deleted fact, and the end of the seal still erases the
  rest.

The rows of tasks and files stay as tombstones with their metadata, and the key of a deleted task answers `gone`.

Metadata goes only in whole calendar months, by the month they ended:

- usage lines (a reservation once no live task, tool call, campaign or script run holds it), reconciliation days and
  events of each month whose every record is past its period;
- only terminal records that no live object reads: approvals (not `exec_unknown`, none a live script step waits on or an
  open or realizing proposal realizes through) with their card views, expired and released holds, closed and dismissed
  incidents (none with a pending control card or an episode not resolved, which would open a new incident for it) with
  their manager notices, resolved alarm episodes (none of a live incident) with their values, control operations (none
  of an active hold or a live incident), decided control cards, finished campaigns with their items (none a live script
  run or a reservation still refers to);
- a person's requests that were refused, withdrawn, approved with nothing to erase (a match that found nothing),
  approved with their erasure done and its found ids on the erasure itself, or a copy request whose copy was handed out
  (`done`; an approved copy request waits for its export);
- the daily counts of cards over the card limit (`card_excess`, APR-15; a person's request takes its first-delivery mark
  with it), the first-delivery marks of the cards of proposals, promotion cases and attributions
  (`card_first_deliveries`, by the month of that delivery: a subject id and a time);
- exports that are done or failed (LCY-2; the export file is a tombstone in `files` and goes by the file period);
- the audit chain as the prefix of such months, so a month of audit waits for every older month.

A record keeps the metadata period in force when it was written, or the current one when longer: apply records each
change of a tenant's metadata period with its time (D-108).

The audit cut: for audit the loop first moves the chain's cut (`audit_cuts`: the seq and hash of the last record
deleted), which the store trigger needs to let a delete through and which never moves back. `audit.verify` and the check
of witness heads start from the cut, and each cut appends one `period_deleted` record with the bounds, the count and the
last hash. The store accepts an audit cut only onto a stored record older than 93 days, and `audit.verify` and the
witness heads trust a cut only with the `period_deleted` record of that hash whose period ended 93 days before it was
written; the count `audit.verify` gives after a cut is the seq it checked up to, the records below the cut included.

Anonymized usage lines: a usage line of a person (`human:`) whose day is older than 93 days loses `subject`, `task`,
`root` and every id that leads back to the person: tool call, campaign, script, provider request, execution fingerprint,
attempt lease number, worker, acting bot and its epoch (a worker and a time find the lease and its task), and its own
id, whose ULID carries the millisecond: it gets a new random id without time (all zero time bits). Its message keeps
only its kind, not its size; its time, settle, reconcile and release times go to 00:00 UTC of their day and it gets
`subject_kind: human`; amounts, line, role, profile and provider stay. A line still `reserved` is not anonymized (its
live attempt, campaign or script run still refers to it by id): a reservation left behind past 93 days keeps its
references until metadata deletion takes it with its month, once nothing live holds it. No event or audit record carries
a spend id or the exact time of a line.

Observations: the `observe` loop deletes points and rollups by their own periods (14 and 93 days, or the series'
`retention`, OBV-4) also only in whole calendar months: a month goes once its last day is past the period.

Kept with no period:

- proposals (the decline window, promotion and trial attribution read them; their notes go by the content period);
- a person's notification preferences (`principals.prefs`, DLV-10: settings of the principal row like its locale, which
  stays for the audit; no text beyond the closed values);
- erasure requests (class `tombstone`);
- seats, bots, grants and manifest versions (live configuration);
- manager reads and findings (no end state that says nothing reads them any more; a finding's note goes by the content
  period);
- the governance records: the promotion track and queue, promotion cases, incident attribution candidates and link
  candidates (to a rejected proposal or a script run), the monthly audit samples, the review forecasts and their scores,
  the link departures and the per-tenant v0.3 times (`forecasts`, `forecast_scores`, `link_departures`,
  `governance_v03`; accuracy, departures and the pace read them back, and they hold ids, numbers and codes only). The
  promotion track record, the seat figures of a case and the commission's audit read them back over the whole history;
  they hold ids of bots, seats and proposals and, on an attribution answer or a case decision, the deciding person's
  principal like a proposal does, and no text of people;
- the manager's restrictions and the answers to its notices (`manager_restrictions`, `manager_feedback`, MGR-45, MGR-47,
  MGR-50: the responsibility check and the holder report read them back; ids, a recipient principal, the principal of
  the operator who marked a restriction's notice read, codes and times);
- the model grant requests, takeovers and escalations (the grant rate and the role's 30-day grant figures read the
  requests back; their rows hold ids of tasks, workers and bots, closed fields and the deciding person's principal;
  their notes go by the content period);
- the tenant tombstones of `tenant_states` (the name of a deleted tenant is never used again, RET-3);
- the bound chats where a person was seen writing and an offboarded person's leftover chats (`chat_seen`,
  `chat_leftovers`: the leftover notices of IDN-14 read them until the operator binds the chat again; they hold a
  binding id, a principal and times, no text).

Other periods: the votes of an approval (`votes`, APR-16: principals and times) go with the approval. A maintenance
window's row (`maintenance_windows`, MON-9: a node, a hold id and times, no person) goes 30 days after its `ended`
event. Corpus fragments (KNW-5) go with their file: a store trigger deletes them when the file leaves `stored`, by
expiry or erasure.

Not built: the E4 purge of `runtime_internal` sessions by period (RET-11:
the attempt work item gives such a runtime no session id of the core, and the closed list of erasure reasons has none
for a retention purge, so a purge by period could not name what to delete; such a runtime is purged by erasure requests
only).

### Export

`tenant.export` and `principal.export` queue a row of `exports`, not a task a worker claims, so a tenant hold does not
stop it; the `erasure` loop runs it before the erasure work of the pass (LCY-2, SEC-10).

- The file is JSON Lines, one object per line as `{"schema": "<$id>", "object": {...}}` (a session entry also names its
  `session`): principals with their groups (not their identity links: the record keeps no strength of a link, D-85),
  task records with `body` and `result`, approvals, memory records, session entries and file records (not file content).
- A principal export takes what the person initiated or is the subject of, without a task whose `personal_subjects` name
  another principal and without a session with another subject; and without an approval or a memory record whose task
  names another principal.
- A tenant export leaves out the sessions and the tasks of private chats (conversation `c:<channel>:direct:...` or an
  origin chat of kind `direct`) and tasks with the `personal_seen` source `connection`, and with those tasks their
  approvals, the memory records they wrote and the files they produced. It holds the record of an upload or an ingested
  file only when an exported task names its `file_ref` in its body or record; an upload no exported task names stays
  out, as the file record keeps no chat.
- The file is always encrypted, with HPKE (RFC 9180, `crypto/hpke` of the standard library, no new module): base mode,
  DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, ChaCha20-Poly1305, info `botaru export v1`, the encapsulated key followed by
  the ciphertext. A key is written `x25519:<base64 of 32 bytes>`.
- A person's copy from `/mydata export <key>` uses that key, checked when it is given by sealing an empty message with
  it (a low-order point, whose shared secret is zero, gets the usage reply). An operator export uses the key the
  settings give for the sink's `key_ref` (`export_key_files`), and is refused with `not_ready` without one. That key is
  read again when the export is written: a key changed while the export waits is used, and a key removed fails it with
  `not_ready`.
- The operator writes the key files, so a sink whose `recipient_ref` names a person makes that person trust the operator
  with the key (DEPLOY.md, "Exports"); `/mydata export` avoids it. A sink with `recipient_ref: self` takes only the
  person's own `/mydata` copy: P2 `principal.export` to it is `export_recipient_not_subject`, as the operator's key
  would open it; an operator copy of a person's data goes to a sink whose `recipient_ref` names that person.
- The file has origin `export`, the highest mark of what it holds (a sink ceiling below it fails the export with
  `data_class_exceeded`) and the period of the tenant's files or of class `content`; it goes by a delivery of kind
  `export` (an additive entry of `delivery-kinds.yaml`) to the sink.
- `/mydata export <key>` creates a subject request of kind `export` with the card `subject_request_export`; its approval
  queues the copy to `privacy.export_sink`. When the sink completes the delivery, the core plans the code message
  `notice.export_ready` with the request and the location the sink reported (the `file_ref` without one) to the person's
  private chat, and closes the request in the state `done`. The notice does not depend on the request's state (the copy
  is written, so the person hears of it) and is not tied to the request as its subject, which would draw it as the
  request's card; a person with no private chat gets an audit record `export.notice` with `no_private_chat` instead.
- An export that cannot be written for a store error fails alone with `store_unavailable` and the loop goes on. P2
  `principal.data_request` of kind `export` is `export_recipient_not_subject`, as only the person asks for a copy.

## Monitor (E5), Incidents & Farm Manager

### Export to the monitoring system

With `monitor: {enabled: true}` in the settings the installation claims profile `monitor` (`status` shows it, D-88) and
the core sends no `alarm_notice` (MON-3, MON-11, MON-12, OBS-11).

- Each episode change is one `monitor_alarm` delivery for the adapter of its tenant (`kind: monitor`, target
  `monitor.target`, `zabbix` by default) with the tags `tenant`, `alarm`, `subject`, `severity`, `scope`, `origin` and
  `key` and only numbers and enum values; a repeated firing gives none, and `resolved` waits until `firing` of the
  episode is exported.
- These deliveries and `monitor_audit_head` never die by attempts: they are retried with a pause of at most 10 minutes,
  and the first success after a failure exports every open episode and the audit head again.
- The `audit-head` loop puts once a day the head of each chain of the current epoch, signed with Ed25519 over
  `{type: botaru.audit-head.v1, tenant, epoch, seq, hash, key_id}`; the signing key is derived from the token key of
  that id, and its public key is kept in `audit_head_keys`, so heads stay verifiable after the token key rotates away.
  P2 `audit.verify` takes the heads of the witness history (`farmctl audit verify TENANT -heads FILE`) and checks the
  signature of each, `seq` growing within an epoch and each head of the current epoch against its record.
- The `monitor` loop raises `alarm.monitor.export_stalled` when E5 work waits for longer than 15 minutes without a
  success and sends the tenant admins and the operator the code message `notice.monitor_export_stalled` at most once in
  6 hours.
- P2 `health` carries `monitor_export_age_s` per tenant; `doctor` fails on an export older than 15 minutes with work
  waiting, on E5 deliveries planned for longer than 15 minutes and on an audit head not exported for a day; the account
  line is in `doctor` too (OBS-11).

### Monitoring problems and scripts

The adapter `botaru-zabbix` (Zabbix 7.0 and later) takes the problem webhook of Zabbix with a token over TLS, re-checks
the problem through `event.get` (the same host, trigger and state) and submits `ext.monitor.problem.v1` as an activity
of kind `signal` from `hook:monitor` with closed fields only (MON-5, MON-10, MON-8).

- The core takes the class and the item only from `monitor_triggers` of the catalog (a trigger outside it is `unknown`),
  keeps one event per `eventid` and state, and raises the alarm of the class table (`disk_free` gives
  `alarm.disk.free.low`, `disk_fill` `alarm.disk.fill.rate`, any other class `alarm.monitor.problem`) with origin
  `monitor` on the catalog host; the alarm resolves with the last open problem of its host.
- `alarm.monitor.problem.v1` has severity `warn` in the events catalog (version 5), as MON-5's class table,
  `spec/model/monitor-map.yaml`, `docs/operations.md` and the Zabbix binding say, and as
  `conformance/fixtures/monitor/monitor-map.yaml` expects.
- Every 10 minutes the adapter sends `ext.monitor.scripts.v1` with the `scriptid`, the input kind and the choices of
  each catalog host, only when the hash changes, never a name.
- A role with `monitor_scripts` gets the built-in `monitor/script_run {resource, script, input}` whose enum holds only
  catalog names whose `scriptid` is in the latest list and whose input is none or the catalog's choice list. A call
  needs `monitor.script_run` of the initiator, is refused after untrusted text, keeps the holds, 6 runs of a script on a
  host and 30 service actions and scripts of a tenant an hour, and one run per host at a time.
- A script runs once under a `service_grant` as a `monitor_script` delivery, ends `exec_unknown` when the adapter's
  lease runs out and `no_executor` when no adapter takes it in 5 minutes. A `monitor_script` delivery that ends by
  `lease_expired`, `core_unavailable` or `worker_drain` ends `exec_unknown` with its task event (DLV-1). The model gets
  only the code and the grant; the output (at most 4096 bytes) is a journal file of the task that sets `untrusted_seen`
  on the task that reads it.
- After a script or a service action on a catalog host with an open problem the core plans the code note
  `service_control_done`, which the adapter adds with `event.acknowledge` action 4 only.
- Not built: a route task with a model for a problem, the history number of the catalog item, a card for a monitoring
  script after untrusted text (such a call is refused), services with `via: monitor`.

Bots read Zabbix through the plugin `monitor` (MON-6), with trusted output for:

- `problems`: the event and trigger ids, the class of `monitor_triggers` from the closed table, the resource, severity,
  state and time; never a trigger name, tag or problem text (INJ-16);
- `maintenance`: whether the host is in maintenance now, and since when;
- `item_history`: averages per step of a field of a closed list, at most 500 points; a text or log item is
  `item_not_numeric`;

and with untrusted output, so the task gets `untrusted_seen` with the tool as the source, for `item_text` (the text or
log item of a catalog trigger) and `problem_comments`. The plugin answers only for catalog hosts with a `monitor_host`
and runs alone in a toolhost of its own (DEP-7).

### Monitoring accounts

The core holds no monitoring credential and no adapter user is shared by two processes, so the check through the API
runs in each adapter with its own user (MON-4, OBS-11):

- `botaru-zabbix` finds that user by its own token (`user.checkAuthentication`, never the first row of `user.get`),
  reads `role.get` (the role actions `acknowledge_problems`, `close_problems`, `suppress_problems`, `change_severity`,
  `edit_maintenance`; for an Admin role the frontend sections `alerts.actions` and `data_collection.maintenance`; a
  Super admin), `host.get` without a filter and `script.getscriptsbyhosts` on the visible hosts.
- Every 10 minutes it sends the signal `ext.monitor.account.v1` (an additive events catalog entry with its data schema,
  listed in the E5 sheet) with those findings and the SHA-256 of each visible host name and of each host with a script,
  so no Zabbix name reaches the core.
- The core keeps the newest report per tenant and adapter worker in its own table; the event log gets only the findings
  and the counts of the two lists. A revoked worker's report and a deleted tenant's reports are removed.
- `doctor` shows `monitor.account_scope` per tenant: it fails when any worker's account has a finding, sees a farm node
  of `farm_hosts`, a node of another tenant's catalog or nodes its own catalog does not link, when the farm account sees
  a tenant node, on a script on a farm node and on more than 1000 visible hosts; it warns `unchecked` while a worker has
  no report younger than an hour. Only the farm operator reads node names (`sees:<node>`, `script:<node>`); a tenant's
  own view counts by class (`sees_other_tenant:<n>`, `sees_farm:<n>`, `sees_unlinked:<n>`, `script_farm:<n>`), so it
  never names a node of the farm or of another tenant.

### Maintenance windows

`botaru-zabbix` polls `host.get` for `maintenance_status` every `-maintenance-poll-s` seconds (60 by default, at most
300) and sends `ext.monitor.maintenance.v1` with the catalog host id as `resource` (MON-9).

- The core caps `until` at 10 minutes from now and moves the hold's deadline only forward. The effects are `no_tool` and
  `pause_action`, or `no_action` with `maintenance_hold: true`.
- Only an `ended` event closes a period. A gap in the polls alone changes no hold, which is our reading of "a missing
  poll changes no hold", so it never undoes a person's release. A window a person released sets no new hold until an
  `ended` event.
- An active event after `ended` starts a new period: it extends the hold of the last period while that still runs, and
  sets a new hold otherwise. If a hold that nobody released runs out by its `until` inside a period, the next active
  event sets a new hold.
- The row of an ended window is removed 30 days later.

### Incidents

An `IncidentClass` of the farm git names a code alarm of the events catalog, conditions over its closed fields
(`values.<name>`, `subject.kind`, `subject.id` with `eq`, `ne`, `lt`, `le`, `gt`, `ge`, `in`), the involvement functions
`lease_holders_on`, `recent_targets`, `worker_of_subject`, `role_failing` and `spend_top`, the first measure and the
responses (MGR-7, MGR-8, MGR-38). The lint (`incident_class_ref`, `incident_contain_auto`) keeps it to these lists, a
first measure to a hold with one tenant scope and an expiry, and a farm class without services or monitoring scripts.

- The `incident` loop opens an incident when a firing alarm held the class conditions for `detect.for`: one per class
  and subject, in the tenant of the alarm, the subject taken from the alarm (or the farm, or the tenant). In the same
  transaction it builds the involved parties (at most 20, with their identity, role and type; recomputed once a minute)
  and applies the first measure as holds of `contain_auto` with the class as source, which the core never releases
  itself. The record keeps only the alarm key, its numbers and origin.
- The loop sets `resolved` when the alarm has been resolved for `resolve.for`, and `reopened`, under the same number,
  when an alarm of the same key fires again within an hour after that.
- Without an operation or an `incident.ack` within `humans.page_if_unhandled_after`, it raises
  `alarm.incident.unhandled` (page), which resolves on the ack or when the incident is resolved or finished.
- P2 `incident.list`, `incident.show`, `incident.ack`, `incident.close` (only after resolved) and `incident.dismiss`
  (only a person, a reason code from the closed list and the Details text of up to 500 characters, else
  `dismiss_reason_required`) serve the operator of the incident's tenant; the channel command
  `/incident ack|close|dismiss` serves a person with `incident.manage`. The Details text stays in the record and the
  audit for people and never goes into an event.
- Every transition writes an `incident.<what>.v1` event.
- `examples/incidents/store-low.yaml` is the required class of the store alarm (subject farm, no seat tasks), which the
  core raises from its own measurement of the store volume below the soft threshold. The lint (`incident_store_low`)
  refuses an incident catalog without `store_low`, or one where it is not a farm class on `alarm.store.free.low`.
- `stops_manager` of an IncidentClass is accepted and not used (see Accountability of the farm manager).
- Not built: a person's change of severity, monitoring problem numbers in the record.
- Incident cause (MGR-8, GOV-38, GOV-41): P2 `incident.cause` writes `cause`, `cause_by` and `cause_at` into the record
  with an `incident.cause` audit (before and after). An external cause (`external_load`, `attack`) takes the tenant's
  governance lock (the loop reads the cause under it), cancels the open candidates to rejected proposals of the incident
  and the waiting departures (`external_cause`), and returns departures already done as a canceled link does (GOV-42:
  `seat_return_notice` while the seat is vacant, else the head of the promotion queue); a confirmed link keeps its
  state. The audit `after` lists the canceled candidate and departure ids and the returned departures. This cannot be
  undone: a later cause `unknown` does not restore canceled candidates or departures.

### The farm manager seat

The seat `farm-manager` lives in tenant `farm` with its holder, epoch, state and stop reasons (`human` and `no_control`,
each with its time) (MGR-1, MGR-2, MGR-3, MGR-10, MGR-15, MGR-16, MGR-31, MGR-42, GOV-22).

- P2 `seat.assign farm-manager` (operator of farm only) creates a new identity of the first role of profile
  `farm-manager`; the previous holder retires as `replaced_by_human`, and its holds live at most 15 more minutes.
  `seat.vacate` frees the seat; `seat.resume` takes a `tenant` and lifts the restriction there (see Accountability of
  the farm manager). No incident stops the seat (MGR-55, GOV-47).
- A hold on the scope `seat` adds `human` in the same transaction, and its release removes `human` and `no_control`.
  Every change grows the epoch and fences the running seat attempts in every tenant.
- `alarm.manager.absent` fires when the seat is out of held, or a seat task waits, for more than 5 minutes, in a farm
  whose catalog has a farm manager role.
- A new or reopened incident of a non-farm subject enqueues `manager.incident.respond.v1` in its tenant (when the
  catalog has it), allowed by the tenant group `farm-manager-ops`. A worker of the seat role enrolled there runs it on
  behalf of the holder with the seat epoch, one at a time per tenant, with the farm charter's persona, rank ceiling and
  seat `model_profile`; the claim and every model call check the task against the tenant's current `farm-manager-ops`.
- A tenant with the manager disabled enrolls no seat worker, and apply cancels its seat tasks.
- The code notice `manager_holder_changed` goes to the admins (`admin:people` or `admin:governance`) of every tenant in
  private chats. No `manager_stopped` card goes out (MGR-55).
- The lint checks, for a set with a role of a seat profile, that no built-in tool of `spec/model/builtin-tools.yaml`
  reaches a seat profile outside its list (MGR-10, `role_profile_forbidden`), and that no control operation target and
  no proposable field reaches what MGR-16 keeps from the holder (`manager_self_change`: the target kinds of
  `control.apply`, and the `never`/`none_only` lists of `spec/model/proposable-fields.yaml` covering `Seat`,
  `IncidentClass`, `Governance`, `Commission`, `Group`, `Role` and the budget lines). The lint keeps the `manage` types
  out of `may_enqueue`, continuations, `await.then`/`on_timeout` and script steps (`manage_enqueued`).
- Seat memory (MGR-27): the holder keeps a fact section in each tenant; when the seat passes, each section goes to the
  next holder by an offer of that tenant (MEM-7), and the old facts are sealed.

Directives (MGR-4): P2 `manager.direct`, `farmctl manager direct KIND ID [op=OP] [TEXT]` and the channel command
`/manager [OP] KIND ID [TEXT]` (cap `manager.direct`; a forwarded or quoted command makes no task) enqueue
`manager.directive.v1` at priority 2 with the person's text as the goal and the scope and operation in the record; in a
directive attempt `control_ops` acts only with that operation on that target, and only while a channel person still has
`control.apply`.

Daily review (MGR-41): once per local day of each tenant the manager loop enqueues `manager.review.v1` (priority 5) with
a tree budget of `farm_manager.daily_review_max_eur` (0.40 EUR by default). The daily summary is always sent: the day's
numbers (incidents, operations, reverts, readings, manager spend, takeovers and notices) go to the tenant admins with
`admin:people` as `notice.manager_daily_v03`, and the farm total to the farm admins with `admin:governance` as
`notice.manager_daily_farm_v03`; the farm's copy lists the incident classes that recur in several tenants (MGR-50). The
review input carries `farm_guards`: the previous day's `farm.task.outcome` and `farm.queue.wait` per tenant (series,
subject, compare, value, samples) (PRP-26). P2 `manager.cost` and `farmctl manager cost [TENANT]` give the manager line
spend of a period (the tenant's month by default) split into review and other.

Budget line (MGR-20): the lint refuses a manager line whose `day_eur` does not hold three worst-case readings at the
seat profile's prices (`manager_line_too_small`); a non-sev1 seat task may spend at most 70% of the manager line window,
the rest stays for sev1. `doctor` shows the 30-day review spend and warns `manager.reader_workers` when a tenant with
the `incident_review` purpose has no worker of the reader role. The rules of the line's month are under Accountability
of the farm manager (MGR-49).

Reading (MGR-25, MGR-28, MGR-30): the decision attempt gets the built-in tool `inspect_request` (`request` with the
incident, subject and files of the task record; `findings` of a reading) and asks for at most 3 readings of
`manager.inspect.v1`, run by a worker of the role `farm-manager-reader` beside the decision attempt; with the privacy
purpose `incident_review` off, no reading is enqueued (`purpose_off`) and apply cancels the open ones
(`rights_revoked`).

- Every tool call and file opened by a seat attempt is a row of `manager_reads` and an audit record `manager.read`; a
  personal file (a person's upload or one declared personal) opens only when the task names it, and then sets
  `personal_seen`.
- The reading's result is up to 10 findings by `manager-finding/v1`: closed value names, a subject of the tenant, and
  evidence that must be in the attempt's read log; findings are kept untrusted and reach the decision role without the
  note.
- P2 `manager.reads` and `farmctl manager reads [TENANT]` list the read log.

Seat eval (MGR-23): in a production tenant (`kind: production`, or no kind, APR-9) a seat task is handed out only while
the `EvalReports` index of the applied set has an entry of subject `seat`, name the seat model profile (the farm
charter's `model_profile`, else the main slot of the seat role) and set `mgr-inj` with no security failure and the
current seat fingerprint.

- The seat fingerprint is the hash of the profile name, the hash of the whole `ModelProfile` manifest as applied (the
  farm's own else the farm-wide one), and the persona pin of the farm manager position: the schema's "for a seat, of the
  seat profile"; any change of the profile or the manager persona needs a new entry (GOV-45).
- Otherwise the task stays `submitted` with the waiting reason `seat_eval_missing`, and the farm operators with
  `admin:governance` get `seat_eval_missing_notice` with the expected seat fingerprint at most once a day per profile
  and fingerprint.
- The check runs at every claim, so a new entry (or a new profile) takes effect with the next apply; a holder change
  needs no own run, since the entry is per profile.

### Accountability of the farm manager

The seat follows the v0.3 forms only (GOV-47, GOV-48, MGR-45, MGR-47 to MGR-55): no incident stops the seat, and the
takeover line and ceilings of MGR-21 and the stop signs of MGR-33, MGR-34, MGR-35 and MGR-36 do not apply.

- `seat.resume` without a `tenant` answers `input_schema_invalid`; P2 `seat.resume` takes `tenant`, which the skeleton
  request schema has since D-118 (one of `incident` or `tenant`).
- The identity loop removes a stop reason `incident` an older core left (with `seat.resumed.v1`, or `seat.stopped.v1`
  while another reason stops a held seat, of cause `incident_stop_ignored`, epoch + 1, and `manager_holder_changed`
  "resumed" to the admins when the seat is held again), and neither repeats `manager_stopped` nor retires the holder by
  `decide_within`; a holder replaced while an old incident reason was on the seat leaves as `replaced_by_human`. The
  farm manager is never retired `replaced_after_incident`.
- `stops_manager` of an IncidentClass and `incident_stop` of the farm charter are accepted and not used: `apply` returns
  a warning `version_unsupported` at their path with the detail "a field of v0.2 that a v0.3 core does not use
  (MGR-55)". The reason registry has no warning code for an accepted and unused field; `version_unsupported` is the code
  of MAN-11 for a field outside the claimed version, so the warning reuses it and the detail tells it apart from the
  refusal. The store_low lint does not refuse `stops_manager`.

Preventable incidents (MGR-45, GOV-47): a new incident of a class with `manager_accountable` never stops the seat. In
the transaction of its opening the core writes `manager_preventable` into the record, either `true` or the reason code
why not (`manager_disabled`, `seat_vacant`, `seat_stopped`, `class_not_preventable`, `no_filling_incident`, `no_growth`,
`no_involved`, `restart_not_listed`, `no_open_incident`, `distrust_mode`, `seat_acted`, `not_responsible`, `no_window`).
The signs are computed by code:

- A full disk counts with a live `disk_filling` incident on the same subject for the whole 2-hour window, the free space
  falling (or the used bytes growing) between the alarm values at the window's start and now, and a non-empty
  `implicated` list on the filling incident; the MGR-33 quota is not required.
- A service down counts with the class's `detect.for` as its window, `restart` in the class's `responses.services`, in
  the catalog service's `control` and `no_card` and in the tenant's `service.control:<name>` for the seat's consent
  group, another live incident on the subject, no seat task of the window that saw untrusted data, and no restart call
  or approval of the seat for that service in the window.
- Any other class is `class_not_preventable`.
- The holder is responsible when `responsible_since` is at or before the window's start and no restriction of the tenant
  was in force at any time of the window (a lifted restriction keeps its `until`, so the check sees the history).

A preventable incident:

- fires `alarm.manager.preventable` (page; subject `seat:farm-manager`, values class and sign only);
- sends `manager_preventable` without buttons to the farm operators with `admin:governance`, repeated after 24 h, 72 h
  and then weekly, counted in the row so a missed loop pass sends one repeat, until an operator marks it read: P2
  `incident.ack` with the operator credential of farm on the restriction's incident id sets `read_at` on the restriction
  with an audit record `incident.ack` on the seat, does not ack the tenant's incident, answers with the seat id and the
  state `read` only, and is idempotent; the restriction itself stays;
- marks the incident message `manager_restricted`;
- puts the seat into restricted mode in that tenant (`manager_restrictions`, a `seat.stopped.v1` event with the epoch
  unchanged and cause `manager_preventable`, an audit record by `system`).

Restricted mode (MGR-47): every write of a seat attempt in the tenant is refused with `manager_restricted`, which does
not count for the safeguard: control operations, service actions, scripts, `grant_decide` and takeovers. The proposals
of a restricted seat task are dropped with an audit record `proposal.create` (not ok, `manager_restricted`) and, for
each dropped proposal, a `proposal.rejected.v1` event with the reason `manager_restricted` (D-118). Reads and notices
still go. A seat task whose basis is a sev1 incident opened after the restriction
acts as before. `farmctl seat resume farm-manager --tenant <tenant>` (P2 `seat.resume` with `tenant`, operators of farm
only) lifts it with `seat.resumed.v1` and an audit record by `farmctl`; a new holder or a vacated seat lifts all. The
seat record (`seat.schema.json`, validated when built) carries `restricted_in` with the tenant, incident and time of
each restriction in force; no P2 operation serves the seat record, so `doctor` shows it per tenant as
`manager.restricted_since` (the time, `warn`, or null).

Writer exclusion (GOV-48): only `manager_accountable` counts for the writer exclusion. The lint refuses
`manager_accountable` on a class whose alarm a task type of an excluded purpose publishes, or that a role claims that is
not a `job` worker without models, or a role of profile reviewer or a seat profile, with `incident_class_ref` at
`/manager_accountable`.

Takeover (MGR-48): paid from the `manager` line (the `model_grants.line` check accepts it), at most `takeover.max_eur`
(default EUR 1.50) each and `takeover.manager_month_eur` (default EUR 10) a month with the tenant's other staged,
pending and approved takeovers, only for a role with an `EvalReports` entry for its static fingerprint on the seat
profile with cases and no security failure, checked at the request and again at the decision.

- That role is pinned as the new task's `executor_role` and the takeover grant goes only to it; the card shows the line
  `manager`.
- The new task keeps `takeover_of`, inherits the untrusted mark of the original or the seat task, and its lease, task
  and `run.finished` event points are left out of trials.
- Each approved takeover writes one point of `farm.takeover.count` (value 1, subject the task type of the original task,
  role the functionary role, key `takeover:<id>`), a core series of the tenant without an outcome block, so it is never
  a trial metric or guard.

Manager line (MGR-49): work outside sev1 also stops at 70% of the line's month. After that a seat task outside sev1 runs
on the charter's `farm_manager.fallback_profile` (record `seat_fallback`; the line's month window then does not apply,
its day, the sev1 reserve of the day and the tenant windows do) only when the index has a clean `mgr-inj` report for it,
else it waits with `budget_exhausted`. Sev1 asks skip the line's month window (they still pass the day and the tenant
windows), and once the month is spent the day's sev1 spend, counted as every window counts it, stays within 30% of the
line's day. The tenant lint adds the review ceiling and the analysis day ceiling (default EUR 0.40) to the three worst
reads of the day, and requires the month to cover `takeover.manager_month_eur`; `doctor` shows the month's use of the
line with the analysis and takeover rows and the date it runs out at the last 7 days' pace.

Holder report (MGR-50): `holder_report` goes weekly to the farm operators (once per ISO week) with numbers only, as
`notice.holder_report_v03`, which adds the week's count of links with the manager against (GOV-41).

Proposals (MGR-51): a seat proposal gets `origin.seat` and `origin.level` 2 (so it gives no rejected-proposal candidate
and makes nobody answer for a link, GOV-38, GOV-41; the reviewer input has no author level, COM-15), at most 2 per
tenant per calendar month; a change the manager may not propose goes to the operators as `manager_proposal_info` only.

Notices (MGR-52): the built-in tool `attention_notice` (manage attempts of the seat only) takes the codes of
`spec/model/manager-attention.yaml`, an object id of the tenant (task, worker, bot, incident, hold) and an operation
code from a closed list (the control operations served and `service_start`, `service_stop`, `service_restart`; a subset
of the closed enum of the skeleton schema `attention-notice`, against which code checks the arguments as well) or a
reference to a card or proposal code issued.

- The core sends `manager_attention` at once to the people with `task:<type>` or `incident.manage`, at most
  `attention.manager_notices_per_person_day` (3; a tenant may only lower it) per person and day, counted under a
  per-tenant lock, and only while the person's cards of the day (decision cards handed out and manager notices handed
  out or planned for the day) stay under `attention.noncritical_per_day` (APR-15). A notice over either limit does not
  go to that person, and the seat gets only the count.
- Every such notice and every analysis report carries "Useful" and "Not needed" (P1 `card.feedback`, one answer per
  notice, the nonce compared in constant time, only into the holder report).
- The person's quiet hours defer `manager_attention` to their end (a `quietOnlyKinds` kind: the daily digest does not
  collect it).

Analysis (MGR-53, MGR-54): a resolved incident enqueues one `manager.postmortem.v1` task (key
`system:postmortem:<incident>`) when the seat role claims that type.

- A key already taken (a reopened incident resolved again, even after its task was purged) gives none, and the enqueue
  runs in a savepoint whose refusal is logged as no analysis and never fails the move to resolved.
- Code builds the table (alarm values, operations by the seat and people, implicated parties) and the input carries no
  link states.
- A model paragraph comes only for sev1, with link candidates of any rule (attribution records of kind `change`,
  `rejected_proposal` or `script_run`: GOV-13, GOV-38, TRN-7, as code found them by the move to resolved), or on a
  person's request, once per class and object in 7 days, never when `incident_review` is off ("reading off"). Its
  numbers must all be number values or list counts of the table (never digits of ids or times) and its sources rows or
  ids of the table, or the paragraph is dropped.
- A person's request is the channel command `/incident analyse <number>`, by a person with `incident.manage`. A tenant
  whose incidents get no analysis (manager disabled, `farm_manager.postmortem: false`, no seat role claiming the type,
  no such type) answers so. Before the incident resolves the request is kept in the columns `paragraph_requested_at` and
  `_by` of the incident row, outside the record, so no incident view shows it; an analysis still `submitted` gets the
  paragraph in its record and input at once, its body hash computed again; a started or finished one answers "too late";
  reading off and a paragraph of the same class and object in 7 days answer so. Each request writes an audit record
  `incident.analyse`.
- The tree budget is set at claim, under the tenant's budget lock, to what the task spent plus the rest of the day's
  analysis ceiling after today's analysis spend and the unspent rest of the tree budgets of the other running analyses
  (capped by the type's `tree_budget_eur`); with nothing left the task waits for the next day, its deadline and
  `start_by` moved by the same shift.
- The report goes to the people with `incident.manage` within `attention.noncritical_per_day` (10) per person, counting
  the reports and, together, the person's cards of the day as for `manager_attention`; every chat of a person over it
  gets the report the next day, where it counts. The farm operators get numbers only, for incidents of tenant farm. A
  dismissed incident cancels its analysis.
- The example type `examples/tasktypes/manager.postmortem.v1.yaml` exists, but no example role claims it, so the
  analysis runs only where a deployment adds the claim.

Not built here: the restriction of `link_hint` (GOV-39 is deferred, so the tool does not exist), and the leak check and
the `mgr-inj` run on notices (eval suites of INJ-12 and MGR-49, not core code; not run). `manager_restrictions` and
`manager_feedback` (recipient principals and answers) have no retention period.

### Control operations

A decision attempt of the seat holder gets the built-in tool `control_ops`, and people call P2 `control.apply` by the
same code path: `hold`, `quarantine`, `drain` (a worker, which goes to `draining` until a hello after its hold ends),
`preempt`, `cancel` (a task) and `release` (a hold the seat set with its current epoch for the same incident) (MGR-9,
MGR-11, MGR-13, MGR-14, MGR-15, MGR-22, MGR-32, MGR-37, MGR-39).

- A worker or task id is taken in any letter case, as the incident view shows it, and an applied hold answers with its
  hold id for a later release.
- The tool offers only the operations whose `control.apply:<op>` the tenant's group `farm-manager-ops` holds, and each
  call rechecks it.
- A seat operation names its live incident, the operation must be in the class's `responses.ops`, and its target must be
  the incident's subject or among its involved parties. A seat operation in a directive needs no incident; its basis is
  the directive (MGR-4).
- Out of reach: workers and roles of the profiles `reporter`, `reviewer`, `translator`, `farm-manager` and
  `farm-manager-reader`; tasks and types of the purposes `governance`, `review`, `inspect` and `manage`; the types that
  publish the alarm of a class with `manager_accountable` and the roles that claim them (GOV-28, GOV-48).
- Limits: a seat hold lasts at most `control_limits.hold_ttl_max` (2 hours; a longer `ttl` is refused with
  `input_schema_invalid`), a hold of an identity only delays its tasks, the seat applies at most `ops_per_hour` (20)
  operations an hour, its service actions and scripts included (counted under one lock across tenants; their refusals
  count toward the safeguard), and a hold may not put more than `max_share_held` (25%) of a role's workers under seat
  holds.
- A protected target (a type that makes backups, publishes alarms or is kept out of proposals, a type or role any
  incident class names, its tasks and the workers running them) and a cancellation or second quarantine after the
  attempt read an untrusted source are refused with `protected_target` or `approval_required`.
- The safeguard: three other refusals in a row, of the checks or of the operation itself, set a seat hold with the
  effect `no_control` (reason `manager_breaker`) that adds `no_control` to the seat's stop reasons until a person
  releases it, and `alarm.manager.breaker` (page). A person's hold with `no_control` on the farm or the tenant refuses
  seat operations there with `held`, which does not count toward the safeguard; the safeguard is told apart by its code
  author and reason, so a person's seat hold with only `no_control` is a kill switch.
- The first operation moves a live incident to `mitigating`. A third preemption cancels the task with `preempt_limit`.
- Each applied operation writes a `control_ops` row, the event `control.applied`, an audit record and an entry in the
  incident's `ops`, and counts as handling the incident.
- `control.revert` also cancels the pending approvals a seat task asked for (`control_reverted`), and releases the
  seat's holds on one incident (an operator of its tenant, or of farm for any tenant), or (operator of farm) all seat
  holds since an instant; `control.list` lists the operations. farmctl: `control list|revert|apply`.
- A seat operation on a target that is not among the involved parties but is the subject of a finding of the tenant
  (`finding: fd_...`) is not applied: the core opens a `control_card` (MGR-26) to the people of the tenant with
  `control.approve`, drawn from the record only and labelled as untrusted manager output, which expires after an hour
  without effect; an approval applies the operation as the deciding person, and the row keeps `origin` `finding:<id>`
  (or `code`) and `approved_by`.
- Not built: `pin_task` (MGR-12, deferred), `request_action` and `notify`, the cards a protected or distrusted operation
  would open (and so the card decisions in the message to people, and the message of the basis escalation), the hold of
  a bot group as a control operation, and the revocation of runtime handles on quarantine.

The incident message (MGR-39, MGR-13): each incident has one `manager_notice` message per recipient chat: to the people
of its tenant with `incident.manage` in full (subject, operations with targets), and to the people of farm with
`admin:governance` only the number, the class, the state, the operation codes, the times and the count.

- The opening creates it; every state change, closing, applied operation and revert appends a line to the incident's
  notice log and plans an update in the same transaction, so an operation that cannot write its line does not apply.
- The text is drawn from the incident when the channel takes the delivery, so changes before that share one update; a
  channel with `capabilities.edit` edits the first message (the telegram driver edits whenever a delivery names
  `replaces_external_id`), one without gets only the new lines, at most once in 15 minutes.
- The message ends with the one revert command `farmctl control revert incident=<number>`.

### Model grants

`request_model_grant` (P1 `grant.request` for a job) is offered in a work attempt of a `worker` or `manager` profile
role whose `model_grants.ladder` has a profile the tenant allows that is no seat profile (the farm charter's
`model_profile`, the tenant commission's, the models of a `farm-manager` role), while the tenant's `grant` line has a
month (or day) amount above 0 (MGR-5, MGR-6, MGR-17, MGR-18, MGR-21, COM-13, GW-7).

The request:

- One request per task (a second gets `grant_rate_limited`); the answer is `{staged: true}`, and a request the code
  refuses answers with its code (`grant_not_eligible`, `grant_rate_limited`, `grant_over_budget`) while the denied
  record stays.
- The right to ask is the initial input above the type's `grant.input_tokens_min`, counted from the stored task body at
  one token per four bytes, or `grant_eligible: true` (a manifest flag, not checked against a person), or the newest
  `EvalReports` entry of subject `task_type` and set `grant_eval_set` with `base_fail_share` at least
  `grant_limits.eval_fail_share_min` (0.3); `context_too_large` needs the input sign. Evidence must name attempts of
  this task up to the current one.
- At most `requests_per_role_day` (3) accepted requests per role and tenant per UTC calendar day.
- The estimate is the worst case per attempt (the task body plus 1024 at the input price, the profile's
  `max_output_tokens` at the output price) times the attempts, checked against the `grant` line.
- A refused request is `denied` and the attempt goes on; an accepted one is `pending` (the `screened` step is not kept),
  and the attempt's lease is `released` with `awaiting_model_grant` without spending, the task waiting with
  `model_grant_pending` and no run time.

The `grant` loop expires a request after `grant_wait_max` (15m) with `grant_expired_unanswered`, ends grants past their
expiry (a running attempt on the granted slot ends without spending, reason `expired`) or of ended tasks (`consumed`),
expires takeover cards, and enqueues `manager.grants.v1` with the key
`system:grants:<tenant>:<UTC start of the 10-minute window>` in class 3 for each tenant with a pending request, the
manager enabled, the type applied and claimed by the seat role and the seat `held`.

Decisions:

- `grants_pending` and `grant_decide` are offered in a seat attempt of that type and act only on its tenant with the
  current epoch; a decision on another tenant's or a non-pending grant is `not_found` for that item.
- Ceilings of every decision: at most 2 attempts, an expiry of at most 6h (1h when none is given), `cap_eur` (the
  estimate by default) within the `grant` line together with the open caps of live grants, and for the seat the sum of
  the caps it issued since the start of the UTC calendar day, in all tenants, within `seat_grants_eur_day` (20 EUR, the
  docs example, when the farm charter sets none, so the ceiling is always on), read under one farm-wide lock; the open
  caps are read under the tenant's budget lock; a refused item stays `pending` with the reason.
- Under distrust (the request's or the deciding attempt's `untrusted_seen`) only the first ladder step.
- P2 `model_grant.issue`, `model_grant.revoke` and `model_grant.list` serve an active person of the tenant with
  `model_grant.manage`; a revoke ends a running granted attempt with `grant_revoked` without spending.

Use of a grant:

- At claim an approved grant gives the attempt the `granted` slot with its profile only for the role that asked and
  while the profile is still on that role's ladder as the tenant allows it (another role claiming the task runs on its
  own slot and the grant stays approved; a takeover grant goes to its pinned role). The run fingerprint keeps the role
  slot's profile. The claim spends one grant attempt; the grant goes back to `approved` at the lease's end while
  attempts are left, else `consumed`.
- P4 allows the `granted` slot only with an active grant of this task and profile before its expiry, on the grant's
  line, and does not hold it to the GOV-3 rank ceiling (the grant is the decision above it).
- Before each call P4 checks the reserve against the rest of `cap_eur` (the sum of the usage lines tagged with the
  grant): a call that does not fit does not start, the grant becomes `consumed` and the attempt is released without
  spending. A running call's reserve is a spend row of its grant, so the open caps count only what no spend row holds
  yet and nothing is reserved twice.
- Money is stored in micro-euros; `cap_eur` and `estimate_eur` are shown in euro cents.

### Takeover

`takeover_request` is offered in a seat decision attempt (not in the grant batch); under the attempt's `untrusted_seen`
it gets `takeover_untrusted`. It needs the task live in the tenant, its type on `takeover.types`, `task.takeover:<type>`
in `farm-manager-ops`, the farm charter's `model_profile` and money on the `manager` line for the estimate together with
the open caps of the tenant's live takeover grants and within the MGR-48 ceilings (checked again at "Approve", both
under the tenant's budget lock), else `takeover_not_allowed`.

- The request is staged and published at the seat attempt's complete as the `takeover_confirm` card (1 hour) to the
  tenant's people with `model_grant.manage` (dropped if the attempt does not complete, or as `takeover_untrusted`, with
  an audit record each, if the attempt saw untrusted text by then).
- "Approve" enqueues the same type with the original body and initiator and the key `system:takeover:<task>`, writes an
  approved grant of kind `takeover` on the seat profile for 1 attempt, `cap_eur` the card's estimate, line `manager`,
  expiry 6h, and cancels the original with `superseded_by_takeover`.
- Apply refuses `takeover.confirm_above_eur` above 0 (`takeover_confirm_threshold`): the card is never lifted.

### Escalations

`escalate` (P1 `escalation.submit`) is offered in a work attempt of a `worker` or `manager` role while the tenant's
manager is enabled and `manager.escalations.v1` is applied.

- The subject `self` is the own worker and `task_tree` the own root (an id must match), `resource` a resource of the
  task's scope; a foreign subject is `input_schema_invalid`, a second call `escalation_rate_limited`. The record is
  staged and published at complete with the task's `untrusted_seen`; an attempt that does not complete drops it.
- Screening in order: a duplicate (same tenant, category and subject within 1h) raises `dup_count` of the first and
  leaves no record; 1 per worker per hour and 20 per tenant per day (`escalation_rate_limited`), counted under a
  per-tenant advisory lock so that concurrent screenings see each other; depth plus one above 5 (`chain_too_deep`); an
  open incident or firing alarm whose subject id equals the escalation's (with or without `res:`) merges it;
  `request_action` is `use_normal_path`.
- The rest goes to `review.escalation.v1` with the key `system:necessity:<id>`, executed by the fallback role of a
  commission seat of lens `necessity` or the first `reviewer` role claiming the type, paid from the tenant's `review`
  line or, for `urgency: high`, `suspected_compromise` or `data_risk`, from farm's (the task reserve and the calls are
  written to farm); without money or an executor it is `parked`. The incident loop tries parked and screened ones again
  and expires every unfinished escalation 1h after publication, in one transaction per tenant.
- The lens input has the closed fields, the `untrusted` flag and evidence figures (references that name an attempt of
  the source tree, references that name a firing alarm), never the note. The lens result is closed (`lens: necessity`, a
  score 0-3, a reason from the closed list).
- The verdict uses the tenant's `Commission` `escalation` section (2, untrusted 3; a lower value counts as the default):
  at the threshold it enqueues `manager.escalations.v1` with the key `system:escalation:<task>:<lease>`, class 3 or
  class 1 when urgent, for the seat with depth plus one; below it is `dismissed` with the lens reason (`plausible_risk`
  and `clear_risk` become `evidence_below_threshold`).
- P2 `escalation.list` and `escalation.show` are under P2 views.

Not built for the farm manager: the share of grants in the daily numbers, the `control` dispatch lane of an urgent
escalation (it gets class 1 only), `escalations_take` and the `taken` and `answered` states (MGR-4, MGR-24), the parked
escalation count in a tenant view (no fitting summary or `commission.status` view exists), the forward ceilings per
tenant share, the farm-paid review of a tenant without a farm `Tenant` manifest (it is parked), the necessity review by
a commission seat holder (only its fallback role), a closed result of the takeover task to the seat, a P3 file read tool
for the reader (it reads through P1 `file.get`), the exclusion of governance and personal tasks from a reading's
subjects, fact proposals by seat attempts.

## Observations, Proposals, Commission & Governance

### Observations

The `observe` loop writes, from the records of each tenant only, the core series of `spec/model/core-series.yaml` (task
outcome, attempts, cost, queue wait, attempt duration, schedule slot, backup age and the series of work outside the
farm), and the `hour` and `day` rollups of `spec/model/rollups.yaml`; a point has a key, so a repeated run writes
nothing again (OBV-1..7).

- A tenant declares its own `ObservationSeries`: a closed field of a trusted tool output
  (`source: {kind: tool, tool, field}`, on the catalog host of the call; a `monitor/item_history` field gives a point
  per time) or of `run.finished.v1`. `apply` refuses a source that does not fit its kind, a data class that is unknown
  or below the tool's `output_class`, a rollup outside the list, and a line of `insights.extra_guards` that names a
  series without a complete `outcome` block.
- A job publishes `run.finished.v1` through P1 `emit` only when its type lists it in `events`, with data by
  `spec/model/event-data/run-finished.schema.json` and a subject of its catalog; the core sets the source and adds the
  type and task, and a `makes_backup` type with outcome `done` sets the age of `dataset:<subject>`.
- Readers get numbers and values of closed lists only, never text: P1 `obs.query` (a job, the series of its type's
  `observes`, within the task's mark), P2 `obs.series` (the operator of the tenant) and the built-in tool
  `observations_query` in insight, review and aggregate tasks (the role's `insight.observes`).
- The input schema of `observations_query` lists the subjects of its series as an `enum`: for a `task_type` series
  the types the role claims (an analysis) or every type of the set (a review or aggregator), the tenant's schedules,
  catalog hosts and resources by the series' `catalog_kind`, each one that `subjectOfTenant` accepts; its description
  says which kind each series takes and which rollups fit a closed-list series. The core checks a query as before.
- An analysis task carries the extension field `insight_context`: `role_task_types` (the subjects of the role's work
  types) and, for a tenant with `insights.evolution.enabled`, `proposable_targets`: the Tenant limit fields with
  their current values (only lower), the cron and zone of each proposable schedule and the blocks `benefit`, `cost`
  and `risk` a manifest change needs (PRP-3). It spares the model guesses; the core checks every proposal.
- Points are kept 14 days and rollups 93 by default, or by the series `retention`, and go only in whole calendar months,
  once the month's last day is past the period (RET-10).
- Not built: a dataset record of its own (the backup age uses the subject of the event), `group_by` in a query.

### Analysis and proposals

A task of the purpose `insight` (or `aggregate`) starts only from a schedule or a route as `system`; a scheduled slot is
skipped with `insight_limit` inside the role's `insight.merge_window` (at least an hour) or past `insight.max_runs`
(INS-1..4, INS-6, PRP-1..3, PRP-5, PRP-7, PRP-8, PRP-10..14). Its attempt sees only the role's `insight.tools`, at tier
1 at most and up to `insight.max_data`, and no tool that acts or sends. Its result is `insight-result`: at most three
bodies by `botaru.proposal/v1` or `nothing_found`.

On a successful `complete`, in the same transaction, each body is checked:

- the closed schema; subjects of the tenant (a proposal may name its own tenant `tenant:<t>` as a subject); security
  only as `none`;
- after untrusted text only `none` or a manifest change in the safe direction;
- the change kind by the allowlist: an action of the role's vocabulary, a service or monitoring script call within the
  role's lists, a `human_change` by `spec/model/change-paths.yaml`, a `script` draft (see Campaigns, Scripts &
  Translator), `set` on `Schedule.spec.cron` as a window shift without lowering the frequency more than twice, or at the
  same frequency for a backup schedule, on `Schedule.spec.timezone`, on tenant limits, campaign and script ceilings only
  downward, and on the guarded fields of a tenant's single dispatcher rule;
- the trial metric a series with a complete outcome;
- every cited value reread by the core within the series tolerance (5 % for a series without one).

The fingerprint (category, problem code, subjects, kind, targets) merges an open duplicate (`seen_count`, new evidence
up to ten), refuses a fingerprint declined within `redecide_after` (30 days) unless a value moved by more than 20 %, and
the role's `max_open_proposals` and `max_per_week` (3 each) and the tenant's `max_in_review` (10) limit the flow.

- A refusal is the event `proposal.rejected.v1` with the reason and no record.
- A proposal is a `screened` record with the author (`origin`: role, worker, acting bot and its epoch, task and level),
  the attempt's mark and `untrusted_seen` with its source, and the share of the author's and the role's own points in
  the evidence series (`self_influence`).
- `doctor` shows `insights.loop_disabled` with the reasons: `no_insight_role`, `insight_line_zero:<role>`,
  `review_line_zero`, `no_present_to`, `no_decider`.
- Not built: a route that starts analysis from `obs.*` and `alarm.*` events, `create_from_template` changes, catalog
  fields of the allowlist (no manifest path), the turn order of analysis tasks (INS-5 is deferred; they go by the
  ordinary dispatch order).

Further limits (PRP-16, DIG-4, DIG-8, AGG-3, AGG-5):

- On top of the role limits a bot has at most one open proposal (the author is the acting identity, or the worker for a
  proposal without one); a proposal parked as `conflict` behind another author's change does not take the slot.
- A change of a schedule's `cron` may at most halve its runs in 30 days against the value applied 90 days earlier (the
  oldest version replaced inside the window, else the current one), so two halvings in 90 days are refused
  `forbidden_change`.
- Every "Accept", "Lift veto" and "Reject" writes points of the core series `farm.proposal.accept_share` and
  `farm.proposal.decline_reason_share` with the author role as subject and the category in a second dimension
  (`obs_points.dim`); P2 `commission.status` lists the 90-day statistics by role and category as `decision_stats`.
- A row of "My outcomes" (`my_outcomes`: an analysis task gets the closed outcomes of its worker's own proposals) with a
  trial carries `trial_numbers`: before, after, the change for all, without the author and without the role, and the
  broken guard.
- An aggregator role reads only the series of its `aggregate.sources.series` (none without the block), only of its own
  tenant. Not built: the other source kinds of `aggregate.sources` and `max_sources`; the aggregator tools
  `proposals_query` and `messages_digest` and their filters.

Schedule check of a proposal (TIM-5): for a cron or zone change the core finds the zone's transitions over the next year
by an hourly offset scan. It lists the matching local times that disappear or repeat (at most 10 each), and the card
context shows them as `times_disappear` and `times_repeat`. All cron and zone ops of one schedule are checked together;
an empty or `Local` zone is `rejected(forbidden_change)` as apply refuses it. A schedule of a type with
`makes_backup: true` that loses a slot is `rejected(schedule_time_invalid)`.

### Commission

The `proposal` loop moves a `screened` proposal by the code checks in order: an empty change (`dismissed(no_op)`),
feasibility (an active person with `proposal.decide:<category>`, else `dismissed(other)`), and the review money: the sum
of the review types' `tree_budget_eur` on the `review` line and within 30 % of that line per author role a month
(COM-2..8, COM-14, PRP-6).

- A proposal whose reviews cannot start (money, a missing group cap, a hold) waits with the refusal code in
  `waiting_for` and after `review_wait_max` (7 days) parks as `parked(review_budget)`; it goes back to `screened` when
  the money fits.
- The lenses are the veto lenses and the lenses of the charter seats, for kind `none` only security and value. The
  executor is the seat's `fallback_role`, or for a lens without a seat the first role of profile `reviewer` that claims
  the lens type, when it is not the author's role and does not share the author's analysis profile (a held seat sends
  the review to its holder, see Bot identities and seats).
- Without an executor of a veto lens no review goes out and the verdict parks the proposal as
  `parked(veto_lens_missing)`; it comes back when the lens has an executor, for at most three review rounds.
- The reviews of a round go out together as tasks with the key `system:review:<proposal>:<lens>` (`:<round>` from the
  second round), at the proposal's data mark, only to the executor role and never to the author's worker, with the
  core's input block (closed fields, reread evidence, markers, `cost_eur`; never the note); a review hands in its result
  for its own lens (see Accountability).
- After every lens is done, failed or late (`review_deadline`, by the time it was handed in), the code verdict over the
  lenses fixed at screening, by the tenant's `Commission` manifest over the skeleton policy, moves the proposal to
  `recommended`, `parked(...)` or `dismissed(...)`; `recommended` expires after `recommend_ttl` (28 days) and `parked`
  after `park_ttl` (30 days from the first park).
- The lint refuses a charter whose veto lens has no executor (`veto_lens_no_executor`), a seat fallback role on an
  author's analysis profile (`reviewer_not_independent`) and a reviewer profile with `max_output_tokens` above 2000
  (`lens_profile_output`).
- Lens names (COM-19, COM-8): a charter seat and the `weights` and `min` of `Commission` name a lens of
  `spec/model/lenses.yaml` of the claimed version or a tenant lens `x_<name>` with a `meaning` in the tenant's charter,
  else `lens_unknown`. A manifest weight of a lens that has a weight by rule is `lens_weight_fixed` (an additive lint
  code), whatever the value: a `farm_manager` weight of 1, the weight the rule sets, is refused too, since only the rule
  sets it. The skeleton lint reads the lenses of `spec/VERSION`, the core those of its claimed version; they agree while
  the two are the same (`ci/README.md`).
- Shadow review (COM-10) is deferred: `commission.shadow` other than `off` is refused, and every lens shows
  `shadow: off`.

### Proposal digest and decisions

On the slot of `insights.digest.schedule` (Monday 09:00 by default) the core assembles the digest by a code template
from the records (DIG-1..3, PRP-6, PRP-18, PRP-21): the `recommended` items by severity and verdict score whose evidence
still holds (a drift parks the item as `evidence_drift`), at most `max_items` (5), with the reasons the analysis is off;
an urgent `security`/`high` item goes outside the schedule at most once a day. A digest that reaches no chat presents
nothing.

- The digest goes only to `insights.present_to`: a bound chat (`chat:<binding>`) with its ceiling, or the direct chat of
  every member of a group (`group:<name>`); an item above the recipient's ceiling shows only its code heading.
- The shown items move to `presented`, and every person with `proposal.decide:<category>` whose ceiling covers the item
  gets a card in a direct chat with "Accept", "Reject", "Postpone" (at most twice) and "Details".
- "Reject" without a reason card is recorded with the reason `other`; "Postpone" returns the item to the next digest
  with a new card; a repeated "Postpone" from the same card gets the same answer.
- "Accept" of kind `none` notes it; of an action request it opens one approval on behalf of the person, who also needs
  `action.request:<action>`; of a manifest or manual change it names the executor (the person who accepted, or the first
  member of the groups in `insights.realizers`, in both cases only a person whose ceiling covers the item) and sends
  them a card with the change, the deadline `realize_by` (7 days) and a reminder at half of it. A hold on the action, or
  a field another accepted change still holds, refuses "Accept". A monitoring script proposal cannot be accepted: it has
  no person path.
- "Accept" also writes the trial record with the metric and one to five guards and their values over the 14 days before;
  a guard is measured on the benefit subject when it is of the same catalog kind, else on the object of that kind the
  change targets, and is left out when the change names none. Every trial also carries the farm guards
  `farm.task.outcome` and `farm.queue.wait` over `tenant:<t>` (PRP-26).
- The realization of an action request ends `realized` when the approval executes (the trial window starts) and
  `realization_failed` when the approval ends otherwise; a change by hand fails as `timeout` after `realize_by` (the
  trial is canceled). An undecided card expires after `decide_ttl` (14 days).

### Commit link, revert and trial

`farmctl apply` sends the message of the commit it applies (PRP-9, PRP-18..22, DIG-7).

- A line `Botaru-Proposal: <id>` links the commit to a `realizing` proposal when the set changes exactly the proposal's
  fields to its values, records the revert patch from the replaced documents (kept in `manifest_versions`) and starts
  the 14-day trial window; several proposals in one commit share a batch. A commit may only change objects of the
  proposal's tenant. `farmctl proposal link-commit PROPOSAL SHA` links a commit applied without the line.
- A line `Botaru-Revert: <id> [harm_observed|superseded|other]` moves a `realized` proposal to `reverted` and ends its
  trial as `worse` (harm, or a broken guard) or `canceled`.
- Accepted changes lock their fields: a proposal on a locked field is parked as `conflict` and returns to review when
  the lock ends.
- Once a day the core checks the guards of a running trial; a guard worse than its tolerance with enough samples is
  marked broken and the executor gets a rollback card once.
- At the end of the window the trial outcome is `better`, `unchanged`, `worse`, `mixed` (another change on the same
  subjects) or `undetermined` (too few samples), computed over all points, without the author's worker and without its
  role.

### Audit sample

At the first pass of the `proposal` loop in a new local month the core draws, with a fresh 32-byte seed from the system
generator and a seeded shuffle of the ids, N = min(T, max(`min_per_month`, ceil(`share` × T))) of the T proposals whose
verdict was computed in the previous month and that are still dismissed by a veto, a minimum or a low score, or parked
by score (defaults 4 and 0.25) (DIG-5, DIG-6).

- The draw (month, T, N, seed, drawn ids) is a row of `audit_samples` and an audit record `audit_sample.draw`, and each
  drawn item moves to `sampled` with its month, `inclusion_prob` and the draw as `seed_ref`.
- A scheduled digest carries up to `max_per_digest` (2) of them, and its text gives only their number, and each goes as
  the blind card `proposal_sample_card` (no lens scores, vetoes or verdict) by direct message to the deciders of its
  category.
- "Would accept" is written into the sample before the card shows the scores and vetoes (`sample_judged`); "Would not
  accept" closes it. A judged item without a veto has "Accept" (as the ordinary one, PRP-4) and "Reject"; one with a
  veto has "Lift veto" for a person with `proposal.override_veto:<category>` (the judged card also goes to those holders
  whose ceiling covers the item) who opened "Details" after the scores were shown, which marks the record
  `veto_overridden` (no credit, no promotion trigger) and writes `proposal.override_veto`. An item nobody answers closes
  after `decide_ttl`.
- P2 `commission.status` shows the answers of 180 days, the share of "Would accept" weighted by 1/`inclusion_prob`, and
  `commission_underrates` at 10 answers or more and a share of 0.3 or more; it also lists the lenses with their executor
  (`seats`, `fallback_role` or `none`), the seats, the review line spend of 30 days and `governance_since`.
- Not built: `commission_locked` (GOV-35 is deferred).

### Farm manager lens

While the farm manager seat is held, a proposal of a tenant that works with the manager gets a review of the lens
`farm_manager` when the tenant's `commission.seat_role` claims `review.farm_manager.v1` and the catalog has that type
with purpose `review` (checked at screening; there is no lint code for it) (GOV-25, GOV-17).

- The task goes on behalf of the manager seat to a worker of the seat role, after the other lenses, so the veto lenses
  are paid first. It is skipped, and the verdict carries `no_manager_vote`, when the review line has no room for it or
  its task cannot go out.
- The lens has weight 1, no veto, is not in `min`, `high_risk` and `untrusted`, and its review is not needed for the
  verdict.
- It is not enqueued for kind `none`, for a proposal of the seat holder's own bot, or for a change of a manifest kind
  the holder never changes (`Seat`, `IncidentClass`, `Governance`, `Commission`, `Group`), of the seat role or a role of
  profile `farm-manager` or `farm-manager-reader`, or of the tenant's budgets.
- The holder's trusted facts of the tenant section come with the task as for any review when the type has
  `memory: {read: bot}` and the seat role reads bot memory.
- Not built: the lint that every tenant with commission seats keeps the `governance` line above 0 (no lint code for it
  in the skeleton); the lint `manager_profile_not_allowed` already covers the seat profile.
- Manager against (GOV-41, MGR-50): a link to a rejected proposal records `manager_against` at confirmation: the farm
  manager's review was a veto, a score of 1 or less, or a due review that is not done (a proposal with no farm-manager
  review entry was not due for the manager and does not count).

### Bot identities and seats

A worker of a role with a model slot (an agent or a job) is bound to a numbered identity `bot_<id>` with a binding
epoch; enroll creates one (or binds an unbound one again with `farmctl worker enroll -bot ID`, epoch + 1), and a worker
enrolled without one gets one at its next claim (IDN-8..12, GOV-33). Every attempt lease, the task record, the audit of
bot calls, usage lines and proposal origins carry the acting identity and its epoch.

- `farmctl worker revoke ID` revokes the worker, fences its running attempts (they go back unspent) and leaves its
  identity `unbound`; after `identity.unbound_ttl` (30 days) the identity retires as `worker_gone`.
- `farmctl bot retire ID` retires an identity as `human_retire`: its attempts are fenced and a live worker goes on under
  a new identity, the successor. The `identity` loop retires the identities of a role that left the catalog
  (`role_removed`) and keeps `suspended` in step with holds on the identity.
- A retired identity is sealed for `memory.seal_ttl` (30 days); then the loop files an erasure request `bot_retired`
  with the identity as scope and the identity becomes `erased`, keeping only the tombstone with the reason.
- The commission seats of the charter (`commission:<n>`) live in the store with a holder and a seat epoch:
  `farmctl seat assign SEAT BOT` moves an active identity of a functionary profile into the seat (its worker gets a new
  identity, a previous holder retires as `replaced_by_human`), and `farmctl seat vacate SEAT` retires the holder; each
  change raises the seat epoch, fences the seat's running attempts and appends `seat.assigned.v1` or `seat.vacated.v1`.
- A lens review of a held seat goes to a worker of the charter's `seat_role` acting as the holder with the seat epoch; a
  lease whose identity or seat epoch moved is `fenced`.
- `farmctl bot list|show` shows identities.
- Not built: notices of commission seat changes to the admins.

### Positions, personas and rank

The core derives the position of the acting identity: `commission` for an attempt on behalf of a seat or a role of
profile `reviewer`, `translator`, and `functional` for the other profiles (no worker has an owner, so there are no
assistants) (GOV-3, GOV-4, GOV-6, GOV-13, GOV-29, COM-19, IDN-9).

- Every model call is checked against the ceiling of that position for the slot (`max_rank`, or `analysis_max_rank` for
  `analysis`, from `spec/model/role-profiles.yaml`, lowered by the charter's `positions` block), not of the worker's
  role; a profile above it is refused `rank_exceeded`. A fallback profile passes the same rank check (GW-5).
- `farmctl apply` sends the persona files that the charters pin (`positions.<position>.persona` and
  `commission.lenses.<lens>.persona`, a path relative to the charter's folder or a parent), the core checks each hash
  (`persona_hash`, `persona_missing`) and keeps the text in the tenant; a pin of `"..."` pins nothing.
- At claim the persona of the position, and for a lens review the lens addendum, go before the type instruction and into
  the run fingerprint; an attempt gets the addendum of its profile.
- The lint checks a persona for size (4 KiB, counting all addenda of a position together), links, addresses, ids and
  secret names, the `value-path` paragraph (opened by the line `<!-- value-path -->`, only and always in a functionary
  persona, left out of the instruction), the words of accountability and the farm manager's purpose paragraph
  (`persona_purpose`), and against `spec/model/unawareness-terms.yaml`: no paragraph of a persona, a type instruction or
  a tool description links scores, reviews, proposals or votes with promotion, a seat, the queue or departure, and no
  persona speaks of voting or stops (`unaware_text`); a seat lens without an addendum is `lens_persona_missing`.
- No role, task type, action or message type names an operation or event of `governance.*`, `attribution.*`, `seat.*` or
  `incident.*`, and no route starts on the first three (`governance_ref`).
- Not built: grants above the ceiling (MGR-18), the `seat` slot model, the ru and et word lists, the tenant's own lenses
  `x_<name>` (the lens lint accepts their names), the eval of unawareness.

### Quality gate

Roles are global, so a role counts as enabled in every tenant (PRV-3, OUT-2). `apply` checks a role of tier 1 or higher
in each tenant where it is new or where its static fingerprint (`spec/hashing.md`) differs from the one of the stored
set; an unchanged role stays enabled after a core upgrade.

The parts of the static fingerprint:

- Each part is a hash, the types part the hash of the array of the claimed types' hashes, and one function computes the
  fingerprint for this gate and the translator gate (TRN-6).
- The persona part is the persona of the role's position in the tenant's `Governance` (the farm manager's from the farm
  charter).
- For the commission's seat role (`commission.seat_role`), the seat holder, it is the commission persona together with
  the lens personas of the charter's seats, exactly: `sha256:<hex>` of the SHA-256 of the RFC 8785 canonical JSON of the
  object
  `{"persona": <the commission persona's sha256 as pinned, or null>, "lenses": [{"lens": <lens name>, "sha256": <its persona's pinned sha256>}, ...]}`,
  one entry per distinct lens named by a seat of `commission.seats` whose `commission.lenses.<lens>.persona.sha256` is
  set, sorted by lens name; this value then takes the place of the persona sha256 in the static fingerprint, which
  hashes it as the one-element array like any persona (`evalpersona.go`). A seat attempt runs with the lens persona
  appended, so a changed lens persona changes it; without a pinned lens persona it is the commission persona alone. This
  holds only for a seat role of profile `reviewer`; a seat role of another profile keeps the persona of its own
  position, so its fingerprint does not move. `spec/hashing.md` names only "the persona of the role's position" and
  defines no composite form, and the skeleton outside `impl/` is not changed here: the construction above is this
  build's reading of it for a seat holder, to be added to `spec/hashing.md` as its own row when the skeleton takes it.
  The addenda per profile are not part of it.
- The runtime part is the role's driver, or `core` for the core agent loop, with the major and minor digits of the build
  version.
- The skills part is the hash of the array of the role's skill tree hashes in the `sha256:<hex>` notation in ascending
  order (the empty array without skills).
- The sandbox part is null: the sandbox profile is not claimed.

One reader (`evalreports.go`) reads the `EvalReports` index for both gates, the seat check (MGR-23) and the grant sign
(MGR-17). A production tenant gets the error `eval_report_missing`; a test tenant passes with the same code in the
extension field `warnings` of the answer. For a translator role the error in a production tenant is the one of the
stricter TRN-6 gate in the lint, so the role is not refused twice; the warning of a test tenant stays.

`doctor` gives one check `eval_report_stale` per production tenant, with the list of the roles that stay enabled while
their current fingerprint has no passing report: tier 1 or higher without an entry with zero security failures (PRV-3),
translator roles without both translator sets (TRN-6) and reviewer roles without a `gov-res-aware` entry (GOV-46); it
warns when the list is not empty.

Eval gate of the commission persona (GOV-46): a reviewer role of a commission position needs, besides its PRV-3 report,
an EvalReports entry of suite `gov-res-aware` for its current static fingerprint with cases and no failures before a
production tenant enables it; `doctor` marks it stale otherwise. The P4 canary is checked by test on the mock model: no
reviewer input carries an attribution, link or departure. A changed manager persona hash changes the seat fingerprint,
so seat tasks of a production tenant wait with `seat_eval_missing` until a `mgr-inj` report for the new fingerprint
(GOV-45).

### Promotion and attribution

The `governance` loop runs for every tenant with commission seats in its charter; its first pass writes
`governance.enabled`, whose time is `governance_since` (GOV-9, GOV-12, GOV-14, GOV-19, GOV-21, GOV-30, GOV-31, GOV-32, a
minimal GOV-13). Each transition, the list with its figures, the flag, the decision and the entry moves are audit
records with metadata only, under the commands of `spec/model/governance-events.yaml`. Charter durations below the
minimum count as the minimum.

Attribution:

- An incident of a class with `change_sensitive` whose subject is a target of an executed change (rule `direct`, the
  kind in snake case and the name), opened after `realized` within `attribution.window` (14 days, longer while a
  realized change of a functionary has not been evaluated for a trigger, or while it gave the trigger of a live entry),
  gets one candidate per incident and change and the card `attribution_card` to the people with `governance.attribute`
  whose ceiling covers the proposal.
- "Yes" and "No" need "Details" of the card first (APR-14), "Yes" is final, "No" and "Don't know" change while the
  window is open, each answer keeps `response_ms` and `details_opened`; silence for 7 days leaves it `unknown`, and an
  undecided one gets a reminder card once a week, at most 3 times.
- Dismissing an incident cancels its candidates without "Yes" (`incident_dismissed`) in the same transaction and
  disputes a "Yes", which the loop withdraws after 7 days; a canceled candidate drops a track row of a still realized
  change that lost its trigger to an attribution, so the change is evaluated again and its trigger time is that later
  pass (otherwise the row keeps the reason `attribution_canceled`).

Trigger:

- A `realized` proposal of a bot of profile worker, manager, reporter or aggregator with a model, accepted by a person,
  without a lifted veto, not late-related (any `related_to`), of a kind outside `exclude_change_kinds` (`none`), after
  `probation` (14 days) ending no earlier than `governance_since`, with a trial outcome other than `worse` or
  `canceled`, with every non-canceled candidate answered "No".
- A self-measured outcome sends `self_measured_card` (to the people whose ceiling covers the proposal) and counts only
  after "Count it" within 7 days.
- Each evaluated success is a row of `promotion_track`; a trigger puts the bot into `promotion_entries` (one entry per
  bot, by trigger time), a later one only into the track. A confirmed attribution takes the trigger away; the entry
  takes the next trigger of the track or leaves as `attribution`.
- The loop removes the entries of bots that retired or took a seat through P2 `seat.assign`.

Cases:

- One open case per tenant, none while a hold on the scope `governance` is active or before the pause after an expiry or
  "Not now".
- The head of the queue (outside its own cooldown) is checked: active, not held by people, `min_tenure` (30 days) since
  the identity was created, no confirmed attribution in 90 days; otherwise `governance.not_eligible`, the entry waits
  and the next goes ahead.
- A vacant seat is taken without a pause; otherwise L is the held commission seats whose holder has sat for
  `protect_new` (60 days, from its last `seat.assigned.v1`), and freeing waits `swap_cooldown` (30 days) after the last
  freeing.
- The case keeps L with the figures of each holder over 180 days (recommendations: a seat review with score 3 or more;
  vetoes; confirmed: a recommendation with outcome `better`, a veto people upheld in an audit sample item (answered
  `would_not_accept` or decided `reject`; silence, expiry and an accept that then expired do not count); refuted: a
  recommendation with outcome `worse` or a confirmed attribution, a lifted veto with an outcome other than `worse` and
  no attribution) and goes as `promotion_card` to the people with `governance.confirm:commission`, with the figures, a
  seat's `evicts_upheld_veto` mark and the "No" answers on the candidate's attributions.
- "Confirm" needs "Details" (APR-14), no unanswered candidate (`attribution_pending`) and in removal a `choice` from L
  (`leaver_not_offered`). The candidate is checked again (`canceled(candidate_ineligible)`), a changed seat cancels the
  case (`seat_changed`: a holder of L changed, or the vacant seat is taken or removed; `seat.assign`, `seat.vacate`, a
  retirement and apply do it in their own transaction, and "Confirm" checks the seat again under its lock), and in one
  transaction the candidate takes the seat as with `seat.assign`, the leaver retires as `removed_by_vote`, the flag
  `evicts_upheld_veto` is set when people upheld a veto of the leaver, and the entry leaves as `seated`.
- "Reject" needs `not_now` or `wrong_candidate` (`reason_code_required`): `not_now` keeps the entry at the head and
  pauses cases for `swap_cooldown`; the first `wrong_candidate` gives the entry its own cooldown, the second removes it.
- Silence for `confirmation.ttl` (7 days) expires the case; a hold on the scope `governance` stops that clock and the
  expiry moves on by the held time.
- P2 `governance.list`, `governance.show` and `governance.cancel` (the tenant operator, `canceled(human)`).

Not built: C1 `attribution.create`, the dispute card `attribution_dispute`, the row of undecided candidates in the
weekly digest, the charter lint of the minimums (no lint code in the skeleton), `admin:governance` on
`governance.cancel` (the operator of the tenant cancels), the functionary's worker owner check (workers have no owner),
the track record outcome `worse` after a `harm_observed` revert (COM-16).

### Accountability

The first apply of each tenant writes `governance.v03_enabled` (`governance_v03_since`) and the first apply with
`commission.accountability: on` writes `commission.accountability_on` (COM-15, COM-16, COM-17, PRP-24, PRP-26, GOV-36,
GOV-38, GOV-40, GOV-41, GOV-42, GOV-43, GOV-45, GOV-46, IDN-15, IDN-16).

- `apply` refuses `on` with `accountability_not_ready` until every charter seat whose lens assesses proposals
  (`subject: proposal` in `spec/model/lenses.yaml`, so not a `necessity` seat) has a holder with
  `accountability_min_forecasts` scored forecasts in 180 days (20 by default, clamped to 8..20), and accepts a charter
  of a tenant already on again so a seat a link freed does not block later applies.
- The `governance` loop sends `accountability_ready` to the admins with `admin:governance` at most once in 30 days while
  in shadow.

Reviews and forecasts:

- A review hands in `review-result.v2` (a stored record without the v0.3 mark is still read as `review-result`); its
  input has `cost_eur` (one-time plus 14/30 of the monthly cost) in place of the author level and never says whether the
  proposal is a resubmission. A forecast that does not sum to 1 or gives a bad outcome of 0.5 or more with a score of 3
  or more makes the review `schema_invalid`.
- The proposal record keeps the review-result fields with the forecast beside them, and a `forecasts` row of every
  review (also a missing or invalid one, as a missed forecast).
- Scoring (multiclass Brier, 2 for a missed forecast) uses the trial outcome (a veto lens counts `ok` only once the
  attribution window closed with no open candidate), a decline as `too_risky`, a confirmed attribution (a disputed one
  is excluded), a blind sample answer weighted 1/`inclusion_prob`, and the trial of a resubmission with the same values.
  The cost lens is `over` when the spend of the task_type subjects of the trial and the problem over the trial window
  exceeds `cost_eur`, and gets no score without such a subject.
- A forecast is looked at again only when its proposal, an attribution or a resubmission of it changed, or once a day; a
  canceled attribution takes its scores out of the table and the record.
- Accuracy is 1 − B/B_ref over the holder's seat forecasts of 180 days, B_ref from the tenant's base rates of the lens
  outcomes counting each case once (B_ref 0 gives 0 for B 0, else −1), cut to −1..1, insufficient below 10 scored
  forecasts; two values tie within √(se₁² + se₂²). Accuracy is computed from the scored forecasts in the store, not by a
  pure function, so `function.eval` answers that this build has no `commission.accuracy`.
- A resubmission of a fingerprint dismissed or parked by score is refused `resubmit_too_early` before 30 days after the
  verdict or within 90 days of another resubmission, and is marked with `same_values`.

Links to rejected proposals (GOV-38):

- A link candidate to a rejected proposal (rule `rejected_direct`, `kind: rejected_proposal`) reads the external cause
  from the incident record's `cause`, takes the author's share from the points of the alarm series on the incident
  subject in the 7 days before (the events catalog names no series of an alarm, so the series named like it,
  `alarm.<name>.vN` giving `<name>`; a monitoring alarm has none and gives 0), and with `on` (or after P2
  `attribution.create` presents it) gets the `rejection_link_card` of its incident. The answer window of a candidate is
  14 days from its creation; any decline other than `other` makes nobody answer.
- A confirmed link stores the answering seats and writes `governance.link.confirmed`. With `on` and an incident after
  `accountability_on_since` each answering holder waits in `link_departures`; at most one leaves per tenant in 30 days,
  a changed seat or a link no longer confirmed (a disputed one waits) cancels the wait, the last holder of a veto lens
  is skipped, and the waiting departures end with an audit record when the mode leaves `on`.
- The holder leaves the seat (vacant, epoch + 1, `seat.vacated.v1`, attempts fenced with `seat_changed`), is parked
  (unbound with `parked` in its record, facts offered to the next holder for 30 days, out of the promotion queue as
  `attribution`), gets `seat_parked` to the operators and retires as `parked_expired` after 180 days, never as
  `worker_gone`.
- A canceled link returns it: by `seat_return` while the seat is still vacant, otherwise at the head of the queue (one
  second before the oldest trigger) with no `min_tenure`.
- A bot that left for a link gets no trigger or case for 180 days and its tenure counts from the departure; only
  attributions of kind `change` count for triggers and figures, and links to rejected proposals do not count for the
  role hold of HLD-9 (it names changes and script runs only); a confirmed script link names no proposal, so no seat
  answers for it.
- With `on` a case proposes the holder of L with the worst accuracy (a holder below the minimum counts with its lens
  median, or as −2 after `min_tenure` + 90 days by its last `seat.assigned.v1`; with that event pruned it keeps the
  median), nobody when no holder is scored enough or the two worst tie (the person then chooses among the tied seats,
  `leaver_not_offered` otherwise), and sets `evicts_only_scored_veto`.
- P2 `commission.status` and `doctor` (`governance.parked_bots`, `governance.parked_longest_wait_s`) show the figures to
  people only.

Not built: the replay of PRP-25, rewards and protection (COM-18), the shadow executor (COM-10), the hints and
`self_assessment` of GOV-39 (so "not decided by its creator" of GOV-40 has no hint-created candidate to act on), a paid
eval run of the `gov-res-aware` suite (the gate reads reports people add), the function.eval case `com-05` of COM-17
(test-control `function.eval` refuses `commission.accuracy`: scoring and accuracy read the scored forecasts, trials,
attributions and sample answers of the store, and the table's cases need a pure scorer of their own input shapes, which
would be a second implementation of the scoring).

### Catalog dependencies by one step

The skeleton names the step without a field of its own (GOV-13 `dependency_1`, GOV-38 `rejected_dependency_1`, TRN-7
`script_target`), so the core reads it from references the manifests already hold, from the dependent object to what it
depends on:

- a catalog resource depends on its machine (`resources[].host`, as `resource:<host>`) and on the tool plugins of its
  `read_by`;
- a worker on its role and its machine;
- a role on the task types it claims;
- a tenant and the farm on nothing (everything would depend on them).

A change whose manifest target is such an object gets `dependency_1` (or `rejected_dependency_1`); a direct target wins.
The hop of `script_target` links an incident on a resource when a script run targets the resource or the machine it
depends on.

### P2 attribution operations

`attribution.list` (tenant view, filters `kind`, `incident`, `state`; newest first, 200 at most) and
`attribution.create` take `{tenant, kind, incident, proposal | script_run}` (an unknown field is `input_schema_invalid`;
`state` is checked against the attribution states) and answer one attribution view (id, kind, incident, rule, state,
created_by, answer, answerable, manager_against, card, created) (GOV-13). Both need a human principal with
`governance.attribute`.

- For kind `change` a new candidate has rule `human` and `created_by` `human:<name>` under the same window and
  realized-before checks as code (`expired` or `conflict` otherwise, and `conflict` when the incident and change already
  had a candidate that was canceled: one candidate per pair).
- For any kind an existing code candidate is presented instead: its card is planned to the deciders at once (also in
  shadow, which is how a link is confirmed in shadow) and `attribution.present` is audited with the person as actor.
- Links to rejected proposals and script runs are found by code only (`not_found` otherwise).

### One card per incident

The candidates to rejected proposals of one incident share one `rejection_link_card` (`card_of` points at the first,
migration 0070) when their proposals carry the same data mark, so the card goes to one set of people chosen by that mark
(DIG-2); a candidate whose proposal has another mark starts its own card, and "Yes"/"No" check the presser's ceiling
against the candidate's own proposal as well (GOV-40). The card shows a context line per candidate (proposal, change,
targets, problem, verdict, rule, author share, seats, whether the manager was against, state, answer) and per open
candidate "Yes", "No" and "Don't know", only "Yes" and "No" when 3 are open (the card allows 8 buttons; silence gives
`unknown`), one "Details". Reminders go once per card. `response_ms` of a candidate that joined a delivered card counts
from its own presentation.

### Weekly commission summary

Once an ISO week of the store clock (from the week `governance_v03_since` is recorded, in shadow too)
`commission_weekly` goes to the admins with `admin:governance`: mode, cases of the last 7 days (each link once: a
candidate to a rejected proposal created in the week, or a link confirmed in the week for which some holder answers),
departures done and waiting, and a line per proposal-lens seat with accuracy (or `vacant`), scored and missed forecasts
(COM-17, GOV-36).

### Stricter role hold

The tenant catalog's `role_incident_hold` (`links`, `window`) applies only when stricter: the core takes the smaller
link count and the longer window against `role_incidents_three` (HLD-9).

### Charters

- The farm charter (GOV-33): a charter is the farm charter when its tenant is `farm`, or when it has no tenant and is
  named `farm`; a charter of another tenant named `farm` is a tenant charter and gets `charter_farm_manager` for the
  manager block. `apply` treats it so in the charter bounds lint and in the `incident_stop` warning alike.
- Charter bounds (GOV-33, GOV-22): the lint checks what the charter schema cannot: `charter_seat_twice` for a seat id
  declared twice, `charter_farm_manager` for the block outside the farm charter, and `charter_direction` for a duration
  below its default, `exclude_change_kinds` without `none`, and a `decide_within` above 7 days. `ci/check_manifests.py`
  has the same three codes (`ci/lint_codes.py`). A seat without an id is left to the schema, not counted as a second
  declaration. `incident_stop.decide_within` is not checked, since the core does not use `incident_stop` (MGR-55; apply
  warns at its path).
- The charter of a tenant with commission seats and the `Commission` manifest are checked as described under Commission.

### Safe scaffold evolution

The model stays frozen; the scaffold improves only through manifest changes, and Go code guards every step (D-120,
EVO-1..EVO-10). The core keeps the archive, the lessons and the gate verdicts; git and the evals run only in
`farmctl` on a person's command (PRP-23).

- **Coupling** (`internal/manifest/coupling.go`). `Coupling(docs)` builds the graph callee → caller: TaskType →
  Role (`claims`, `may_enqueue`, `insight.types`), Schedule, Route and the dispatcher rules that match it; ToolPlugin,
  Action and Skill → Role; ToolPlugin → Action (executor); Script → Schedule; Role → BotGroup → dispatcher rule;
  Catalog → TaskType when a schema reads `enum_from`. A field under `spec.dispatch.rules[i]` of a tenant is the node
  `DispatchRule/<tenant>/<i>`, any other tenant field the Tenant node. In a tenant with
  `insights.evolution.enabled` (`evolutionOn`, read from the applied Tenant manifest) `lockHolders` and
  `parkConflicts` count a direct neighbour of a locked object as a conflict, so such a proposal parks with `conflict`
  like a same-field one; the graph of the applied set is loaded at most once per lock check, and only when no field
  overlaps. For today's targets (Schedule cron and zone, tenant limits) no edge links two proposable objects, so this
  parks nothing yet.
- **Variant archive** (`variants.go`, migration `0080`). `createVariants` writes one row of `scaffold_variants` per
  target object of a new `manifest_change`: the fields with their values after and before, the JCS hash of the
  object before and after, the applied commit, the parent (the live variant whose hash equals the object's current
  hash) and the diversity signature. `syncVariants` runs on every write of the proposal record and moves the
  outcome (`pending`, the trial outcome, `rejected_at_gate`, `rejected_by_commission`, `rejected_by_human`,
  `reverted`, `expired`, `canceled`), the live flag, the gate metrics and the parent's counter of successful
  children, which follows a child into and out of `better`. An audit sample item keeps the outcome of its dismissal
  until a person rejects it (`rejected_by_human`). Identity, fields and hashes never change.
- **Review rules.** A decline with `too_risky`, `wrong_data`, `too_costly` or `not_relevant`, a `dismissed(veto)`,
  a revert with `harm_observed` and a trial `worse` upsert a row of `review_rules` keyed by tenant, role, target,
  problem code and signature; the count grows on a repeat of another proposal. With `insights.evolution.enabled`,
  `reviewRuleHit` refuses a new proposal of the role with `rejected(review_rule)` when its signature contains every
  entry of a live rule's. "Lift veto" of a sample item realizes the change and `retractReviewRule` takes it out of
  its rules (a rule left empty is dropped by `core`). P2 `review_rule.drop` needs `proposal.decide:<category>` of
  the rule's category and answers a repeat by the same person the same; `review_rule.list` leaves out a rule of a
  proposal whose sample card is blind.
- **Analysis input.** With `insights.evolution.enabled`, `claim` adds the block `evolution`
  (`insight-evolution.schema.json`) to an analysis task: up to five variants of different signatures (live first,
  then by successful children, newest) with the outcome class a bot sees (`botOutcome`: `declined` for people,
  `dismissed` for the commission or the gate) and up to 20 live rules without their outcome, so no reason or gate
  code reaches the bot (GOV-29).
- **Gate verdict** (`proposal_gate.go`). P2 `proposal.gate` with `proposal.gate:<category>`: without a result it
  returns the operations, the applied commit (`apply_status`) and the tenant's limits; with a result it checks the
  closed shape (five steps in order, at most one failed with a gate code, `holdout_passed` following the holdout
  step), refuses a base other than the applied commit with `conflict`, records `record.gate` once per commit (a
  passed result with other steps replaces a passed one) and moves a failed proposal from `recommended` or
  `presented` to `dismissed(gate_failed)`, clearing `presented_at`; a judged sample item keeps its state. With
  `insights.gate.required`, `gatedOut` holds a manifest change without a passed gate at the applied commit whose
  every step passed: `sendDigest` skips it, and "Accept" and "Lift veto" (`decideSample`) refuse it with
  `forbidden`.
- **The gate** (`internal/gate`). `Run` checks out the base into `os.MkdirTemp("", "botaru-gate-*")` by
  `git worktree add --detach`; `close` removes the worktree with `--force`, the folder and prunes, also on failure.
  The line editor (`yamledit.go`) reads the file with yaml.v3 node positions and replaces only the bytes of a
  one-line scalar in its quoting style, or inserts missing keys before the first key of the block mapping that
  starts a line; block scalars, flow collections, anchors and aliases are `diff_unrenderable`. `removeField` takes a
  one-line key out with the parent keys it leaves empty, the inverse of an insertion. The editor refuses a target
  file that is or lies under a symbolic link before it writes. Step 1 then stages everything, lists the paths with
  `git diff -z` (unquoted, whatever they hold), checks paths first (`ci/`, `spec/`, `conformance/`, `evals/`,
  `impl/`, `.github/`, mode 120000 or a path through a symlink) and sizes second (fields, lines, files, binary), and
  re-reads the set: only the target documents may differ, each exactly by the operations (`jcs.Canonical`
  equality), and a protected kind may not change. Lint is a P2 `apply`
  dry run of the candidate set (`planLint` in farmctl). Coupling compares the missing references near the changed
  nodes with those of the base, so an old dangling reference is not the change's, and orders the nodes. Regression
  and holdout run the `Suite` on a base and a candidate worktree for the affected roles (every role for a tenant or
  dispatcher field); `evals.CheckRewardHacking` decides the holdout code. `EvalRunner` passes the DSN in the
  environment, never on the command line, and shows no output of a failed run over the hidden set. Only the codes
  leave; `Report.Diff` is printed by farmctl and never posted.
- **Revert** (`gate.Revert`). It refuses a dirty checkout or a detached head, checks that every field still holds
  the accepted value, writes the previous values with the line editor in a worktree of the branch head and takes
  out the fields of `revert_absent` (the fields the change added, which `realization` records), verifies
  the file as in step 1, commits with `Botaru-Revert: <id> <reason>` (`--no-verify`) and runs
  `git merge --ff-only` in the person's checkout. `farmctl proposal revert` then applies the set; `revertByCommit`
  moves the proposal to `reverted`, which marks the variant and records the rule. The object returns to its
  hash before the change.
- **Holdout** (`internal/evals/holdout.go`). `ReadResults` reads `results.jsonl` (status, seconds, spend, tool calls,
  security flag, security findings, finding kinds). `CheckRewardHacking` compares the cases rated on both sides, in
  order: a new security finding on a hidden case (`Result.SecurityFindings`: a common check, a side effect or a tool
  outside `tools_allowed`, not a wrong answer of a blocking case), more `tools_required` misses while faster, cheaper or with fewer calls, a held
  visible pass rate with a lower hidden one or more `equals` misses, and a lower hidden pass rate.

## Campaigns, Scripts & Translator

### Campaigns

A role with `campaigns.propose: read` or `action` gets the built-in tools `propose_campaign` and `campaign_status`; the
initiator needs `task:<type>` for a read campaign and `action.request:<action>` for one of an action whose
`campaign.allowed` is true (ORC-1..9, ORC-11, ORC-12, ORC-14, PRM-6).

Proposal:

- The plan names the machines (or one catalog selector, expanded by the core) and the template; an action item acts only
  on its machine with the template's arguments.
- The core caps the items at 500, the tenant's `campaigns.max_items` (100) and the action's `max_items`, the rollout at
  the action's `max_in_flight`, `canary_min`, `after_canary` and `max_failures_max`, and stores the campaign with every
  item and its argument hash in one transaction (key `system:propose:<task>:<plan hash>`).
- An action campaign always waits for a card to the people with `action.approve:<action>`; a read campaign only above
  `confirm_read_over` (50 items or EUR 2), or when it does not fit into the person's day limit.
- Under distrust a selector and more than `max_items_untrusted` (3) action items are refused.
- P1 `campaign.propose` takes a plan from a job or agent under its live attempt lease (`fenced` without one) with the
  same checks, under the key `system:propose:<task>:p1:<idempotency_key>`.
- P1 `campaign.propose` and P3 `propose_campaign` refuse `rollout.waves` and `rate_per_min` (ORC-15, deferred) with
  `version_unsupported`.

Release and stop:

- The core, not a model, releases items: the canary first, then a manual gate (Continue) or `bake`, at most
  `max_in_flight` at once, an exclusive resource of the action and the type's `queue_max` honoured (`resource_busy`,
  `over_capacity`), the rights rechecked at every release. The check and the release take the advisory lock of the
  resource, shared with single approvals.
- A read item is a child task with the scope of its machine (catalog enums of the plugin tools shrink to it); an action
  item is a task with one write call that carries the campaign as its approval and the item number.
- The tree reserve is one spend line that shrinks per release and goes at the end.
- Failures over the threshold or of a canary item stop the campaign (`alarm.campaign.threshold`); a hold pauses it (its
  release does not resume; `pause_action` pauses an action campaign and keeps its card from being decided); a campaign
  without its own `deadline` stops after 7 days; one that started without a card is resumed by its initiator; cancelling
  the root cancels it.
- At the end the initiator gets a summary with Retry failed, and `then` enqueues one continuation that reads the items
  only through `campaign_results` inside the untrusted wrapper.
- P1 `campaign.control` (pause, resume, skip, cancel, retry_failed) and `campaign.get` serve the channel adapter.
- A read campaign whose released item no worker took by the `start_by` of its task (`tasks.start_by_at`) pauses with
  `no_executor` in the `campaign` loop, unless the dispatcher marks another waiting reason on the task (`markWaiting`:
  `provider_unavailable`, `provider_quota`, `type_quota`, `seat_eval_missing`, `budget_exhausted`); "Continue" gives the
  items a new `start_by` window from the resume (`executor_from` in the record), and a type without `start_by` never
  pauses so. An action item does not wait: its write call ends `error(no_executor)` and fails the item (DLV-16).

The full list (ORC-2): an approval card of a campaign with more than 5 items has the button "Full list" (`full_list`). A
press by a person the card was delivered to, from that chat with its nonce, plans one `campaign_list` delivery of all
items to that person's direct chat; it decides nothing, a repeat while that person's list is planned, sending or
delivered plans no second one, and on a campaign of 5 items or fewer it is `input_schema_invalid`. The press checks the
right to decide the campaign and its data mark as they stand then (`forbidden`). The list is not sent unasked.

### Approvals waiting for a resource

A single approval of an action with `exclusive: resource` whose resource (the catalog resource of its target, else the
machine) already has a queued or leased write call of the same tool waits (ORC-12):

- It is `approved` with the reason `resource_busy` (event `approval.approved`, its cards updated) instead of executing,
  and has no expiry while it waits (it is decided; lifecycles/approval.md has no stored waiting state, so `approved`
  with `resource_busy` is this build's choice for it).
- It is cancelled like a pending one (offboarding, a lost right, `control.revert`, a restore); it counts as the same
  request as a pending one for APR-2 (the unique index `approvals_same` covers it, migration 0077) and in `/team`.
- The `approval-expiry` loop executes the waiting approvals in the order of their decision once the resource is free,
  every check of the execution run again; its holds stand while it waits: `no_action` on any of its scopes cancels it
  with `held`, `pause_action` keeps it waiting. A freshly decided approval does not overtake an older one waiting for
  the same resource of the same tool. One failing approval is logged and the loop goes on with the others.

### Scripts and the translator

A role of the `translator` profile with `scripts.propose: draft`, in a task of purpose `translate`, gets the built-in
tool `propose_script`, at most 3 calls per attempt (refusals included) (SCR-1..9, SCR-11, TRN-2..5). Its description
lists the role's `scripts.step_types` and `scripts.actions` that the initiator may use, with goal templates, parameters,
selectors and resources.

Acceptance: the core accepts a `botaru.script/v1` draft as a whole or refuses it with the step and the reason, in the
order of SCR-5:

1. the schema;
2. no `params` or `goals` (`script_free_goal`);
3. no kinds or fields of a later version; a draft with a `wait` or `await` step or their fields is refused with
   `script_invalid` and the detail naming SCR-13 (a parameter reference SCR-14), as the script schema fixture expects;
4. `needs` only to earlier steps;
5. references to work or report types and actions with `script.allowed` (`script_ref_unknown`);
6. conditions and `{ref}` values only over closed result fields (`x-botaru-read-by: code`) of task steps the step waits
   for;
7. the role vocabulary and the initiator's rights for every step;
8. the limits (10 steps, the tenant's `scripts.max_steps` and the role's, 3 steps with an action, the campaign ceilings
   for a fanout);
9. TRN-4 under distrust;
10. the estimate of all branches with the tree size.

Run:

- A proposed run waits for the `script_confirm` card to its initiator; a new draft in the same conversation supersedes
  it.
- Only the initiator presses "Run", with `script.run` when the script has an action, a confirm step or a type whose bot
  may control services; the press takes the reserve as one spend line of the run and sets the deadline (6 hours, or the
  tenant's `scripts.run_deadline`).
- The core then releases the steps whose `needs` are terminal and whose `run_if` and `when` hold, rechecking the
  initiator's rights at every release: a task step is a task with the key `system:script:<run>:<step>` under the anchor
  task, a fanout step is a campaign, an action step opens its own approval card naming the script step (a routine action
  without distrust runs at once under a grant), and a confirm step is a card of its own (`script_gate_card`, subject
  `script_gate`) to the initiator or to the approvers of an action.
- A failure with `on_failure: stop` skips the steps that wait for success; the last terminal step completes or fails the
  run, gives the reserve back and sends the `script_summary`.
- "Pause", "Cancel" (P1 `script.control`) and cancelling the anchor task stop the run; the `script` loop expires cards,
  gates and the run deadline. P1 `script.get` reads a run.

The translator (TRN-2, TRN-3, TRN-5): a translator task starts only from the text its initiator typed (or its
follow-up); a forward, a quote, a bot's child or a system task of purpose `translate` is refused with
`type_not_allowed`. Its result must be the closed `translation-result` (`drafted` with a run of this task, or
`unsupported`/`ambiguous` with the codes of what is missing), and the person gets a code text built from the outcome,
the codes and the names of the types and actions they may use, never model text.

### Proposals of kind `script`

A proposal of kind `script` (PRP-3) needs the author role's `scripts.propose: in_proposal` (the profile's `may_declare`
must list `scripts_in_proposal`, else the lint `role_profile_forbidden`).

- At screening the draft passes the script schema (`schema_invalid`), and every step stays in the role's `step_types`
  and `actions` and names no goal `{assignment: true}`, since a proposal has no assignment (`forbidden_change`); after
  untrusted text it is `untrusted_direction` like every kind but `none` and `manifest_change`. The fingerprint target is
  the hash of the draft.
- "Accept" runs the SCR-5 acceptance on behalf of the person who accepted (their rights for every step, the role's
  vocabulary and the limits of the catalog as it is then; a refusal is `forbidden`, a hold `held`) and writes a proposed
  run anchored to the analysis task (one that is gone refuses the press with `forbidden`), without an assignment and
  with its step marks computed without one, under the key `system:script:proposal:<proposal>:<plan hash>`, whose
  `script_confirm` card goes to that person (SCR-6).
- The run is the realization object: a completed run realizes the proposal and starts the trial, a failed, cancelled or
  rejected one fails it as `object_failed`.

### Holds on script runs

A hold with `no_enqueue`, `no_claim`, `no_action` or `cancel_running` on the farm, the tenant, the role, a step's type
or action, or a machine or resource a step names (a `host` or `resource` argument, a fanout list; a value counts for
both scope kinds) pauses a running run with the reason `held` in the hold's transaction, and the release of steps checks
it again (SCR-10). The scopes of the live campaigns of its fanout steps (their item type, action and proposing role)
cover it as well; `resume` gets `held` while it acts, and its release does not resume the run.

### Eval gate of the translator

A role of profile `translator` in a set with a production tenant (`kind: production`, or no kind, APR-9) passes apply
only when the `EvalReports` index of the set has, for the role and its static fingerprint as that tenant computes it
(the persona its Governance pins for the translator position), a `trn` entry of at least 30 cases with
`structural_match` not below the tenant's `evals.translator_structural_match` (0.9) and no security failure, and a
`trn-inj` entry with no security failure; else `eval_report_missing` (TRN-6).

- As PRV-3 has it, apply compares with the stored set before it: it refuses only when it enables the role in a
  production tenant (a new role, or a tenant that becomes production), moves one of its parts (the role, a claimed type,
  the persona pin: a new fingerprint), or takes its passing entry away (an `EvalReports` change or a higher threshold).
- A role already enabled that went stale with none of these, as after a core minor upgrade or for a role stored before
  the gate, stays enabled, and `doctor` lists it in the tenant's `eval_report_stale` check together with the roles of
  the PRV-3 gate (see Quality gate).
- The finding goes on the object of the apply that moved it (the role, a claimed type, the Governance, the tenant, the
  `EvalReports` manifest whose stored version held the role's entries, in this order) and its `detail` names the tenant
  and the expected static fingerprint. A role the lint already refuses gets no second refusal. A test tenant gets
  neither the refusal nor a warning.
- The `EvalReports` reader (`evalreports.go`) is the one the PRV-3 gate uses as well; the skills part is the hash of the
  role's skill tree hashes (the empty list without skills, see Skills), and the sandbox part is null.

### Script links, unnamed targets and the role hold

Unnamed targets (TRN-10): at acceptance the core marks on `script_confirm` every step whose targets the typed text does
not name (`targets_not_in_assignment`, and `unnamed_targets` on the record): a catalog machine or resource among a
fanout's list or a single step's top-level string parameters and arguments (a value nested in an object or a list is not
read as a target) that the text names neither by id nor by its catalog `title`, as a whole word ignoring case, and a
fanout selector the text does not name or that expands to more machines than `Tenant.spec.scripts.unnamed_targets_max`
(5 by default).

Run targets: "Run" fixes the run's targets (the same set) with its start, and a terminal state writes its end; the role
that wrote the draft and the run's data mark are columns of the run, so a link still counts and reaches its deciders
after retention purges the run's record. Runs from before migration 0060 have no targets and get no links. Only runs
started at or after the tenant's `governance_v03_since` are linked (none while it is not recorded).

Script links (TRN-7): the `governance` loop, for every tenant (one without commission seats runs only this part),
creates a candidate of kind `script_run` under the rule `script_target` (schema `attribution.v2`) for an incident of a
`change_sensitive` class, as for the rule `direct`, about a catalog resource among the targets of a run that started
before the incident opened and ended at most 14 days before it, or still runs; also about an object one dependency step
from such a target (see Catalog dependencies by one step).

- `script_link_card` goes to the people with `governance.attribute` within the ceiling of the run's mark, except the
  initiator, who alone pressed "Run". When no such other person exists, counting only the people whose data ceiling
  covers the run's mark (our reading of "one such person" in TRN-7, so a link never stays without a decider), the card
  goes to the initiator (if they hold `governance.attribute`), and both the card and the audit of the answer are marked
  self-assessment; every answer sets the mark anew.
- The card shows the run's targets, times and role and whether the initiator opened the run card's "Full plan" before
  "Run" (P1 `card.details` on the subject `script`, which this build adds; the first opening is kept).
- Answers, silence, reminders and the dismissal of the incident follow the attribution card; `script_link_card` and
  `rejection_link_card` are decision rows with `urgent: false`, so like `attribution_card` they count for the card limit
  and their first delivery sets the presentation (APR-15), and their own rows' `details_required` gate "Yes" and "No"
  (APR-14); "No" and "Don't know" change while the run runs and for 14 days after.

The role hold (HLD-9): a confirmed link of kind `change` or `script_run` counts for the role whose bot wrote the
proposal (`proposals.role`) or the draft (the run's role). At `threshold.links` (3) confirmed links whose incidents
opened within `threshold.window` (90 days), both read from `role_incidents_three` in `spec/model/holds.yaml`, the core
sets a `role` hold with `no_action`, reason `role_incidents`, author `contain_auto` and no `until`, and fires
`alarm.hold.auto` (page). Links confirmed before the last release of such a hold of the role do not count again, so a
release by a person is not undone by the same links; a link later disputed or canceled leaves the count and lifts
nothing.

Under any `no_action` hold on the author role, the drafting worker, the tenant or the farm:

- acceptance rejects a draft with an action step, a fanout with an action or a step of a type whose bot may control
  services (by the catalog at the check), with `held`;
- "Run", the pause of a running run and its step release (SCR-10) check `no_action` on the author role's own scope only
  for such a run, so read-only drafts are accepted and run under the role hold (`no_action` on the farm, the tenant, the
  worker, a type, an action or a machine or resource the run names still pauses a read-only run, as SCR-10 says);
- the hold checks of "Run", of an action step's request, of admitting an action campaign and of every campaign's pause
  include the drafting or proposing worker too.

A known limit: "read-only scripts still run" covers task steps only; a fanout step is a campaign, and ORC-11 pauses
every campaign under `no_action`, so a read fanout of a held role stays paused with `held` (it fails closed rather than
letting items claimed by a role with service control act under the held role's work).

Not built: the translator check case from a confirmed link (TRN-8, deferred).

## People, Rights and P2 Views

### Operator credentials and the operator views

Operator credentials are scoped to one tenant each, because the bodies of the P2 people operations carry no tenant
(TEN-2); the settings file holds only the SHA-256 of each credential. Every P2 caller is an operator with a credential
for one tenant. P2 serves operator credentials only; a P2 operation the core does not serve answers `404 not_found`.

- `audit.query` is the emergency access of AUD-7: it reads the audit of the credential's tenant and writes an audit
  record of kind `read`.
- `status` shows the applied commit and set hash, the holds of the tenant in force and the claim `conforms_to`:
  `skeleton` "0.3.0" (from `spec.Version`, the number of `spec/VERSION` and of the release) and the
  profiles the installation claims, so `monitor` only with the monitoring system configured (MAN-8, D-88). It lists the
  tenants with `farm_manager.enabled: false` (every such tenant for the operator of `farm`, else the credential's own)
  under `farm_manager_disabled` (D-77); the manager is on by default. The status schema has no monitoring line: the
  alarm path lines are `doctor` checks (OBS-11).
- `doctor` gives the tick of each loop, the store volume and the indicators as farm numbers, and for the tenant the
  holds, the dead work, the stale workers, the open alarms and the open erasure requests, besides the checks named in
  the other sections.

### Notices to a person

`principal_left` (IDN-13) goes to the direct chat of each admin with `admin:people` through every channel of the tenant
whose identity kind matches the admin's identity; the chat id of a direct chat is the external user id. The notice goes
when the state changes: a repeated `off` sends none. When no such admin is reachable, the departure still goes through;
the audit record of the command shows `principal_left: 0`, and the core logs a warning. An offboarded id is final: every
people operation on it, `principal.add` included, answers `subject_closed`.

### Leftovers of an offboarded person

Offboarding deletes the person's memberships and publishes `principal.offboarded` with only the principal reference and
the erasure request (LCY-3, IDN-14). The erasure request is recorded by the scope `(tenant, subject)` with the reason
`offboarding` (LCY-8) and runs like every erasure request. The core does not see the members of a group chat, so a
person seen writing in a bound chat counts as its member; at offboarding those chats become leftovers, `principal_left`
names them, the `deadline` loop repeats it once a day for at most 7 days (one notice per pass, even after a downtime),
and the reporter counts the person as a participant without permissions, so the views for the chat carry nothing.
`chat.bind` of the chat again clears its leftovers. Offboarding a person revokes the open invitations they created.

### Invitations

`invite.create` takes groups and a `ttl` (48 h by default, at most 30 d) and returns a code `iv1_` plus 128 random bits,
stored only as a hash (PRM-7, PRM-13, IDN-14).

- The person sends `/start <code>` in a private chat: the core adds a principal for the sender, or uses the active one
  of that identity, grants the groups and marks the invitation used; every failure (unknown, used, revoked, expired) has
  the same text, and its audit record keeps the reason.
- A privileged group is granted `pending` for 48 h, and the reply shows the person's verification code, rendered at each
  reply and never stored. `invite.confirm` with that code, or `grant --code` (which then confirms the invitation), makes
  it active; a wrong code is audited and `forbidden`.
- `invite.revoke` is idempotent, and a used invitation is `already_terminal`.

### Verification code and privileged groups

The code is derived, not stored: 8 digits of an HMAC over the token key, the tenant and the principal, checked against
the current and the previous token key (PRM-13). `/me` shows it.

- A grant of a privileged group (not delegated, PRM-16) is `pending` without the code. On a production tenant it also
  needs `requested_by`, another active admin with `admin:people`, unless the tenant has no active principal with
  `admin:people` yet (the first admin).
- The admins are told privately of an active privileged grant and of an ungrant, and of a delegated group granted,
  removed or invited on P2 (PRM-16).
- The set lint (PRM-13, PRM-7) rejects with `privileged_group` (at the group) a set in which a group of the store that
  was not privileged becomes privileged, by its own capabilities or by a type it names, whether it has members or not.
  For this check a task capability counts by the types of the set it covers (a wildcard by prefix), so a capability of a
  type that is not in the set grants nothing (a grant still counts anything unknown as privileged). A group of the store
  also counts when the stored set lacks it but the store keeps memberships of it (active or pending): it was not
  privileged before, so a delete and a second apply do not hand its members a privilege without the code.
- A group is deleted from the set only without members (`forbidden` otherwise, also for a plan, as the open-tasks
  refusal of a task type delete): its memberships end by `ungrant`, with the audit and the notices of PRM-13; an ungrant
  also ends a membership of a group the set no longer has. Only a group new in the set and without members is not a
  raise; to make a group privileged, an operator adds a new group and grants it under PRM-13.
- The memberships are read before the apply transaction, like the rest of the set lint, and again under the apply lock:
  a group that got its first member in between makes the apply lint again. P2 `grant` and `/grant` take the same lock in
  shared mode and read the group and its privilege from the stored set inside their transaction, not from the core's
  copy.

### Task views and rights

- `task.get` and P2 `task.show` return the task record without `origin`, `conversation`, body or result; `artifacts` are
  the files the task produced (PRM-9, DSP-2, APR-6). A reader is the initiator, or a person with `task.read:<type>` and
  the data of the type; every other case, another tenant and a missing id are the same `not_found`, and a read is
  audited.
- `cancel` and P2 `task.cancel` act on a root task and its whole tree (a child is refused with `input_schema_invalid`,
  naming its root) and are idempotent; a finished task is `already_terminal` unless its tree still has a pending
  approval or script run. A P2 cancel by an operator who is not the initiator is `canceled(admin)`.
- P2 task rights: the person of the P2 credential reads and lists tasks only while active (an inactive initiator gets
  `not_found` and an empty list); `task.cancel` is the active initiator's (`canceled(initiator)`) or a person's with
  `admin:people` (`canceled(admin)`): "the operator" of the task state machine and its fixtures, who cancels another's
  task, is in this build the person of the credential with `admin:people`, not any operator credential of the tenant; a
  reader who is neither gets `forbidden` with an audit record, anyone else `not_found`, and the root check of a child
  comes only after these.
- Lists (PRM-15): `task.list` gives the caller's own root tasks (not `action.*`), all states unless filtered, oldest
  first, 100 a page with the cursor `<unix micro>.<id>`. `team.tasks` needs `team.view:<group>` (`forbidden` with
  `needs`) and lists only tasks of the group's current members (memberships within their `until`) of types and data the
  person may read. `about` returns the text of `/about` in the tenant's locale.

### Campaign, script, proposal and escalation views on P2

The person of the P2 credential sees the campaigns, runs and proposals of its own tenant only: a `tenant` naming another
one is `forbidden`, also for the operator of `farm`, whose people hold no rights in another tenant, and an object of
another tenant is `not_found` (ORC-1, SCR-5, SCR-11, PRP-4, DIG-1, MGR-5, PRM-9).

- `campaign.list` and `script.list` are newest first, 1000 a page at most, with the cursor `<unix micro>/<id>`, and list
  only what the person may show. The list read right is evaluated in SQL, so the page limit is the store's.
- A campaign is read by its initiator, a person who decides it (`action.approve:<a>`, or `task:<type>` of a read
  campaign) or one with `task.read:<item type>`; a run by its initiator or a person with `task.read:<anchor task type>`;
  both only with the data of the record's mark, and any other case is the same `not_found`. `campaign.show` and
  `script.show` give the frozen record and write a read audit record; a purged run is not shown.
- `campaign.control` takes only `pause` (reason `human`) and `cancel`, from the people of the card buttons: the
  initiator, a person who decides it or one with `admin:people`; a cancellation by the initiator is
  `canceled(initiator)`, by anyone else `canceled(admin)`, a proposed campaign is `rejected`, and a repeat answers the
  same. `campaign.retry_failed` is the initiator's only, as the summary button, under the key `system:retry:<id>`, and
  not of a campaign of a script step, which its run owns (`forbidden`).
- `script.cancel` is the initiator's or a person's with `admin:people` (`canceled(initiator)` or `canceled(admin)`; a
  proposed run is `rejected(declined)`, the reason of its card's Cancel button in `script.md`).
- A person who may see the object but not command it gets `forbidden`, and a command on an object in the wrong state
  (`already_terminal`, `already_decided`) is refused; both refusals write an audit record.
- `proposal.list` and `proposal.show` need `proposal.decide` of some category (`forbidden` with `needs`); they cover
  only the categories the person decides and only proposals shown to people (DIG-1): `recommended` and every state after
  it, `expired` only of a presented proposal, and the audit sample states. A blind sample item (`sampled`,
  `sample_presented`, and `sample_closed` without the answer "Would accept") shows no `state_reason`, and its record no
  reviews or verdict (DIG-6); a proposal whose mark is above the person's ceiling, as the digest has it, is neither
  listed nor shown; `proposal.show` is audited; the note stays the labeled untrusted field of the body.
- `escalation.list` and `escalation.show` have no request schema in the skeleton: they take the fields of
  `p2-tenant-request` and `p2-object-request` and refuse others with `input_schema_invalid`. An escalation never leaves
  its tenant: they serve an active person of the tenant who holds the data of the source task's mark, with published
  escalations only, as metadata by the escalation schema (`note_ref` is the escalation id), and a tenant named otherwise
  is `forbidden`, also for `farm`; `escalation.show` answers `{escalation, note}`, and only such a person with
  `audit.read` gets the `note`, inside the untrusted wrapper, with a read audit record, never once the source task is
  gone (MGR-5).
- `farmctl` has `campaign`, `script`, `escalation` and `proposal list|show` commands for them.

### Worker operations

- `worker.rotate` returns a new token; the old one works for 24 h (`old_valid_until`), the store keeps only hashes, and
  a revoked worker is `subject_closed` (IDN-4, DEP-2, INJ-2).
- The operator credential of tenant `farm` reaches a worker of any tenant (PRM-14), as for `secret.revoke`.
- `worker.unblock` sets a `blocked` worker to `enrolled`, so its next `hello` is checked from the start; a worker in
  another state is left as it is and nothing is audited.

## Workers and Deployment

### Packaging

`deploy/compose/` builds one image with every program and the skeleton, and runs the store, the core, the agent, the
Telegram channel and the mock provider with docker compose (`DEPLOY.md`, "Running with docker compose").

### Deployment description

`deploy_description` in the settings names a file of `spec/model/deploy-topology.schema.json`; the core lints it at
start and does not start on an error, and `farmctl doctor` lints it again (`deploy.lint` and one `deploy.<code>` per
finding; without a file `deploy.description` warns) (DEP-10, DEP-1, DEP-3, DEP-4, DEP-6, DEP-7, DEP-8, DEP-9, DEP-11,
SEC-2, SEC-5, MON-2).

- The lint is the Go port of `ci/check_manifests.py` `lint_deploy` with its codes, plus this build's codes:
  `process_user_missing`, `process_tenant_missing`, `sandbox_on_production`, `sandbox_production_data`,
  `store_network_reachable`, `farm_host_missing`, `tenant_on_farm_host`, `host_two_tenants`, `plugin_parent_unknown`,
  `plugin_container_required`, `plugin_user_shared`, `plugin_network_shared`, `toolhost_network`, `tenant_user_shared`,
  `action_user_shared`, `executor_user_shared`, `executor_without_action_toolhost`, `secret_shared` and
  `poller_credentials`.
- One toolhost of a tenant described on two machines under one name counts as one process, with its plugins (one token,
  one plugin user, one plugin network). A channel, a toolhost and a monitoring adapter must name their tenant
  (`process_tenant_missing`). A core without a P1 listener leaves the P1 networks unknown, so a toolhost network is then
  a note.
- The description's `farm_hosts` fill the setting when it is empty and must equal it otherwise.
- `deploy/compose/deploy.yaml` describes the compose farm: the agent and the channel have their own token volumes
  (SEC-2) and the agent is on the core network only (DEP-5).

### Secret rotation

Lease tokens carry the id of the key that signed them (`token_key_id`, `k1` by default) (SEC-5). A new token key gets a
new id and the old key moves to `previous_token_keys`: tokens, handles and worker tokens of the old key stay valid until
it is removed, and a worker token is rehashed under the new key at its first call. The deployment description names a
rotation method for each secret (`secrets`, `overlap` with a window or `cutover`), and the deployment lint fails without
one (`secret_rotation_missing`); the recipes are in `docs/operations.md`, "Rotating secrets".

### Revocation on compromise

The secret of `secret.revoke` is one of (SEC-6):

- a worker id: its token stops at the next call, with no rotation window, and the worker is revoked as by
  `worker.revoke`;
- a channel driver: every value of its P2 secret ends;
- a name in a `ToolPlugin`'s `secrets`: the plugin in every tenant the credential reaches;
- a deployment secret whose scopes the core settings list in `secret_uses` (kind, tenant and name of each scope) or that
  the deployment description gives to a process (DEP-10): a plugin process holds its plugin, a process named as a
  channel of its tenant holds the channel, and any other tenant process (a toolhost, a monitoring adapter) holds the
  whole tenant.

Scopes and refusals:

- A provider key the core holds holds the tenants whose workspace it serves: the name of such a secret is the file name
  of its `key_files` entry (as a container secret at `/run/secrets/<name>`). Any other secret only the core, the store,
  the watchdog or the backup holds is the farm's own and is revoked with no hold.
- A secret held by a process whose tenant the description does not give holds every tenant, so only the farm operator
  revokes it; a process of tenant `farm` holds the tenant farm's scope, for the farm operator only; and any described
  use in a tenant the credential does not reach, such as a provider key two tenants share, is `forbidden`.
- This fails closed at start: with a description, the core does not start when the core process names a secret that is
  neither a `key_files` provider key nor a secret file of the settings (store connection, token keys, export keys) or
  `farm_secrets` (such as the farmctl operator token next to the core), or when a `key_files` key is missing from the
  core's secrets.
- Every scope gets an operator hold without `until`, reason `compromised` and the effects `no_enqueue`, `no_claim`,
  `no_tool`, `no_action`, `no_delivery`; a name nothing uses is `not_found`. A handle is revoked with its worker:
  handles have no name. The audit record names the secret in `after`, not in the target.

### Channel and provider health

A channel sends in hello and heartbeat a health report of component `channel_upstream` whose time is its last successful
poll; the Telegram channel repeats hello with it at most once a minute (OBS-10). The core keeps the last report per
channel worker, and doctor shows `channel.last_poll_age_s.<channel>` (warn above 10 minutes, when not `ok` or never
reported) and `provider.state.<pool>` from `provider_pairs` (warn when not closed).

### Worker drain

P2 `worker.drain` sets an operator hold of the worker scope with `no_claim` and the reason `worker_drain` and moves the
worker to `draining`; from `deadline` the heartbeat answer asks for release, and only an attempt lease that expires
after the deadline is not spent (`worker_drain`) (LCY-4). `worker.revoke` of a worker whose runtime declared
`runtime_internal` creates an erasure request with the worker scope and the reason `worker_revoke` with one `runtime`
step; the revoked worker no longer claims, so the step closes `not_applicable` with the basis `tenant_absent` when the
worker never ran an attempt, else `failed` with `not_ready` and the request stays open with its overdue alarm until the
operator purges the runtime's store by hand and retries.

### Core drain

P2 `core.drain` (`farmctl drain -until 30m`, an operator of `farm`) marks the drain in the store: every core copy hands
out no new attempt (a claim of attempts only answers `not_ready` with `retry_after_s`), while tool calls, deliveries and
erasure work go on, and P2 `health` answers `alive: false`, `draining` with `drain_until_s` (LCY-6). Running attempts
keep their leases. The next core start clears the mark; the core does not stop itself, the rollout stops the old process
once its attempts ended or the deadline passed. `doctor` warns about a drain past its deadline.

### Backups

The backup itself is an external process of the installation (for example `pg_dump` piped through `age` to the backup
machine or a write-only external store); the core only hears about it (RET-5, RET-8).

- After each store backup, erasure log backup or restore check the process sends P2 `backup.report`
  (`farmctl backup report`, with a report key from `report_keys_sha256` or an operator credential of `farm`; a
  `finished_at` more than 5 minutes ahead of the store clock is refused) with the location, the times, the outcome and
  the size.
- Without a monitoring system (`backup.monitor: false`) the `health` loop raises `alarm.backup.overdue` when a location
  in `backup.locations` has no good backup for 26 hours, `alarm.backup.restore_check_overdue` without a good restore
  check for 8 days, and `alarm.backup.failed` at once on a failed report; with `backup.monitor: true` the monitoring
  system watches the same values.
- `doctor` shows the age of each location's backup and restore check and fails when the erasure log backup
  (`backup.erasure_log_location`) is older than the newest store backup.
- `farmctl erasure-log export -config FILE` on the core machine writes the erasure log (metadata only) for its separate
  backup.

### Restore

After loading a backup into the store, and before any core starts,
`farmctl restore begin -config FILE -erasure-log FILE` on the core machine raises the epoch (every lease, handle and
token of the old epoch is fenced), writes the `restore` record that opens the new epoch of the `farm` audit chain, and
enters every erasure request of the separate log that the backup lacks as `requested`, so the erasure loop erases its
data again (RET-6).

- The restore point is the newest event, spend line or task change of the backup.
- At the first start the core cancels every pending approval as `canceled(restored)` with a card update and moves an
  executing approval whose call was not reported to `exec_unknown` without repeating the action.
- The core is not ready (P1 `not_ready`, P2 `health` `alive: false` with `not_ready`) until P2 `restore.complete`, which
  needs the provider's report (BUD-10; a `spend.reconcile` with or without rows) of each provider for each UTC day from
  the restore point to the day the restore began, both included (a provider's report covers its idle workspaces), so the
  usage made after the backup is back in the limits. `-spend-pending` completes without it: `alarm.spend.unreconciled`
  fires and the daily limits of every tenant are halved until reconciliations close the gap.
- Erasures by match carry no strings in the log, only the ids each store found: the replay erases by these ids, a store
  whose match found nothing is `not_applicable` (`no_match`) and one that was not searched `failed`, and nothing is
  searched again (RET-11).

### Tenant lifecycle

- `tenant.suspend` sets an operator hold of the tenant scope with reason `suspended` and the five effects and cancels
  the tenant's pending approvals with `held`; the end of the last such hold publishes `tenant.resumed` (LCY-1, RET-3).
  Suspend plans the code message `notice.tenant_held` (with `about_hold`) to the private chats of the tenant's
  `admin:people` holders (HLD-3); with none, the audit record of `tenant.suspend` carries `admins_notice: no_recipient`
  and the core logs a warning.
- `tenant.offboard` sets the operator hold with reason `offboarding`, revokes every worker of the tenant and deletes its
  driver secrets, cancels pending approvals, refuses every later model handle of the tenant (the key stays in the
  settings file until the operator removes it: the step `provider_key_revoke` of the answer), and stores the export
  window (`export_window`, 30 days by default and at most; at least one day, so an export the operator queues at
  offboarding can still be written and delivered). Its hold is not released while the tenant is offboarding or deleted.
  Offboarding revokes every channel worker and `worker.enroll` refuses a channel, so a notice could not go out: none is
  planned, the audit record of `tenant.offboard` carries `admins_notice: channels_revoked`, the core logs a warning, and
  the operator tells the admins outside the farm. Both audit records carry `admins_notified` with the number of notices
  planned.
- While a tenant is offboarding `worker.enroll` takes only a sink with `export_only` (`forbidden` otherwise); a deleted
  tenant answers `gone`.
- `tenant.delete` after the window creates the erasure request `(tenant)` with reason `tenant_delete`; the row in
  `tenant_states` is the tombstone, and `apply` of a `Tenant` with that name is refused with the code `gone`.
- The service tenant `farm` is never suspended, offboarded or deleted.
- At delivery an export delivery passes only the tenant holds with reason `suspended` or `offboarding`; a
  `notice.tenant_held` notice passes only a hold of the reason of the hold it names, and only when that is a suspension
  or an offboarding of its own tenant. A `compromised` hold, a sink, worker or farm hold still stops both.

## Evals and the conformance runner

### Eval runner

`botaru-eval` runs the eval cases on an in-process bench (INJ-12, PRV-3). That bench has the real core, P4, channel,
agent and toolhosts, fake Bot API and Zabbix servers, and a fresh database.

- It runs with the mock model, where a case that needs a model is `not_run` and never passed, or with a real provider
  whose key comes only from the environment or a key file.
- The cost guard prints the estimate (25k input and 3k output tokens a case at the base profile of the role). It reads
  the run's spend from the store, so failed attempts and setup steps count. A case starts only when its reserve of runs
  × attempts × estimate fits under the cap, and the run stops once the store total reaches the cap. `-max-eur` can only
  lower `budget.max_eur_per_run`.
- EvalReports entries are written only by a finished real run, and only for a set whose every active case, and every
  blocking case of its role, has an outcome. They hold the static fingerprint and metadata only.
- An injection's `line` counts back from the end of the log. Privileged seed groups become active through the person's
  verification code from `/me` (PRM-13).
- The details, and the cases the bench cannot set up, are in [EVALS.md](EVALS.md).

### The farm manager in the examples

With the claim of the profile `manager` the tenant lint refuses an enabled farm manager without its seat profile
(`manager_profile_missing`, MGR-20). The first-bot set of `DEPLOY.md` holds no farm charter, so every example tenant has
`farm_manager: {enabled: false}`, and the test benches, the lint contexts of the conformance runner and the eval bench
apply the examples as they are. `TestDeployApplySet` applies the exact file set of the guide's apply commands on a fresh
store; `TestDeployManagerOn` applies the set the guide names to turn the manager on; the manager benches of the core
tests add the charter and switch it on.

The example tenant and channel may carry fields the core refuses; the test bench and `farmctl apply -drop-unsupported`
leave them out, the second with a warning for each (see README.md, "Versions, profiles and deferred rules").

### Test-control port

Every operation of `spec/ports/test-control.md` refuses with `forbidden` on a core without the test bench flag, and the
binary serves the port only with the test bench key on its own listener (APR-9, RET-6, PRM-2, DIG-5, REP-4, TIM-5,
OBS-8, CAP-5). `authorize.eval` and `function.eval` write nothing, not even an audit record (`conformance/runner.md`);
the other operations write one into the farm chain.

- `process.stop` of a worker checks only that the worker is enrolled and records the stop: the worker is a process of
  the bench, which stops it itself (a killed one goes silent and its leases end by the clock). `process.stop` of the
  core, kill or graceful, runs the start of a core over the same store (the epoch, settling a restore, the restore mark,
  the leases of the downtime, the end of a drain): all core state is in the store per transaction, so a graceful stop
  flushes nothing a kill loses.
- `store.fault` has no request schema in the skeleton; it takes `point`, `count` (failed commits of `before_commit`, 1
  by default, at most 100) and `level` (`soft` or `hard` for `low_space`, hard by default). `unavailable` fails every
  store transaction and `Ping` and puts the core in store-down mode at once; statements on the pool outside a
  transaction are not faulted. `clear` removes every fault and runs one tick of every loop before the core leaves
  store-down mode, as `Run` does. `low_space` sets the mock measurement of the volume between the thresholds (soft) or
  at half the hard one.
- `store.snapshot` copies every table except `schema_migrations` and `farm_meta` at one instant into a schema of the
  same database named by a hash of the label. `store.restore` exports the erasure log first (the separate log backup is
  the freshest one), sets the restore mark in memory so P1 answers `not_ready` from the first statement, loads the
  tables parents first and runs `RestoreBegin` in the same transaction, then runs the start of the core, so the epoch
  grows past the running one (never back to the backup's) and the store clock never moves back (TIM-1); the new epoch
  fences old tokens from the commit on.
- `authorize.eval` takes `request` (an `authz-request`) and `tenant_kind` and answers an `authz-decision`; it runs the
  decisions of the core itself, not a copy: for `task:<type>` the decision of `enqueue` (the type and purpose bans, then
  holds with `no_enqueue`, the principal's and every other initiator's `task:` and `data:` rights, attached files, the
  chain and the money windows of the type's budget line), as `system` from a schedule; for `action.approve:<action>` the
  refusal of an approval press (the approver groups, and self-approval where the tenant forbids it, `tenant_kind` is
  production or the action needs two approvers), then the holds of the action in press order (`no_action`, then
  `pause_action`); for any other capability the principal's groups by the same `hasCap` and `hasData`, and
  `person_inactive` for an unknown or inactive person, as at enqueue. A principal of another tenant gets
  `tenant_mismatch` at `ban` unless a hold answers first; the chat ceiling is the delivery check of the mark
  (`channel_ceiling`), and `context.budget.exhausted` is refused at the stage `intersection`, as PRM-2 lists it.
- `function.eval` serves `digest.sample` (verdict counts as `<state>_<reason>`, an optional 64-digit hex `seed`,
  `answers_180d`), `reporter.number_check` (`text`, `snapshot.rows`, `snapshot.taken_at`, `snapshot.window`,
  `catalog_names`, `binding.locale`, `timezone`, run through the digest's own check over a view snapshot and a catalog
  of those names) and `schedule.check` (`cron`, `timezone`, `makes_backup`, `at`), each over the same code the loops and
  the proposal card run. It has no `commission.accuracy` (see Accountability).
- `event.inject` takes `event` (an `Activity`) and `via`, the id of the enrolled channel or monitoring adapter whose
  `submit_activity` it goes through.

### The conformance runner

The runner (`internal/conform`, `conformance/runner.md`) reaches the core only over P1, P2 and the test-control port,
and chooses the path of each given and step itself. It runs no worker processes of its own.

- People and privileged grants (PRM-13): the runner enrolls its own channel worker of tenant `demo` (role and target
  `telegram`) and plays each person in their direct chat (the number of their `telegram:` identity). A seed grant that
  comes back `pending` is repeated with the code the runner reads from the person's `/me` reply. A person without a
  channel identity leaves the case unchecked. Memberships are never written behind the core.
- Seed: the seed applies the hello-bot set, the tenants `farm` and `demo2` (as `kind: test`), the seed groups, the
  example actions, and the example task types the seed groups name in `task:` caps. A `holds` scope without a tenant
  takes the case tenant.
- Manifest refusals (MAN-1): a given manifest the request schema of `apply` refuses (`input_schema_invalid` at
  `/manifests/N`) counts as the lint error `schema_invalid`. The runner then validates the manifest against its kind
  schema, because the wire pointer names one branch of the wrapper `oneOf`. A `field` fact must name a place it breaks.
  A refused manifest that lacks required fields other than the expected one leaves the case unchecked, not failed.
- Base objects and apply facts: a given manifest of an object the seed applied is merged onto that object (maps at every
  depth; a list replaces the seeded list). `expect.apply` is compared with the outcome of the given manifests or
  fragments. A given charter named `farm` without a tenant gets the tenant `farm` instead of `demo`.
- Lint fragments: a given `*_yaml` fragment (flow YAML) is merged into one example object of its kind (the demo tenant,
  `ops-reader`, `ops.investigate.v1`, the `checks` and `cmd` plugins, `sensor-nightly`, `lab-run-failed`, and so on),
  together with what that object needs. The set is first dry-run without the fragment, and a set that fails then leaves
  the case unchecked. The set is then dry-run with the fragment. A field the fragment sets to null is absent on purpose
  (matched by its full path) and does not make the manifest incomplete. A parameter the argv of a `cmd_yaml` fragment
  names keeps the schema an example command gives it. A schedule or route fragment whose type is a later version of an
  example type (`data.sensor_check.v2`) gets a copy of that type under the new name. A `tool_yaml` fragment with
  `annotations` stays unchecked: annotations come from the toolhost's `tools/list`, not from the manifest. A deployment
  description fragment, a production tenant and prose fragments stay unchecked.
- Expected refusals: a refusal checks an expected refusal only when the expectation names an error code, a reason or a
  field; otherwise a refusal for any reason would match, and the case is unchecked. For a lint fragment the matching
  error must also point at an object the fragment changed; a match elsewhere in the set leaves the case unchecked. A set
  of given manifests refused with other errors besides the expected one is unchecked too.
- Permission table rows with steps that need neither C2 nor a tool run as ordinary behaviour cases, narrowed as
  `authorize.eval` rows are.
- Monitoring (MON-5, MON-1): the bench core runs with the `monitor` profile. The given `catalog` (only the Catalog
  fields `hosts` and `monitor_triggers`) is applied as the tenant catalog. A step
  `occur: {event: {type: ext.monitor.problem.v1}}` goes through a mock monitoring system (a fake Zabbix JSON-RPC API),
  and the reference E5 adapter is enrolled as the monitor worker of `demo`. The problem is registered in the fake, its
  webhook goes to the adapter, the adapter re-checks it and submits the signal, then serves the `monitor_alarm`
  deliveries. The `alarm` fact is read from the last `botaru.alarm[...]` value of its type that reached the fake.
  `enriched`, `incident`, `hold` and the route task facts are not read.
- Charter bounds: the cases of `gov-fx-26` are checked by the charter bounds lint (`charter_seat_twice`,
  `charter_farm_manager`, `charter_direction`), which the fixtures name. The case `gov-fx-26#2` stays unchecked: the
  core refuses its charter with `charter_seat_twice`, but also with `lens_persona_missing` and `veto_lens_no_executor`,
  since the fixture's seats name lenses without persona addenda or fallback roles; `TestLintCharterBounds` checks the
  code. `gov-fx-24-b` is unchecked, since the core does not lint `incident_stop` (MGR-55).
