# Improvement cycle

## In short

The improvement cycle is the main element of the farm (D-16): bots notice from the core's numbers what in their work can be done better and propose changes themselves. Only the core writes and re-checks the numbers, a proposal passes code checks and a blind commission of bots of its own tenant (D-23), and only people decide, with buttons. An accepted change runs by the farm's ordinary path, and for 14 days the core measures whether the metric the bot promised has improved. The outcome of this trial is seen by the author in "My outcomes", by the commission in its calibration and by people on the promotion card. Credit goes to the author only for their own change that people accepted, that was realized through a verified link and that did not worsen the promised metric (D-24, D-37). This way the cycle rewards measured benefit, not people's approval. The document describes the links of the cycle from observations to handing the outcome over to promotion; commission seats, the charter and promotion itself are described in [commission.md](commission.md) and [bots.md](bots.md).

## Links of the cycle

| Link | Who does it | What comes out | Section |
|---|---|---|---|
| Observations | core, job worker, core from tool output | points of numeric series | "Observations" |
| Seat analysis | a bot of a functional role on a separate model of the `analysis` slot | up to three proposals or the code "nothing found" | "Seat analysis" |
| Aggregator | a bot of the `aggregator` profile | proposals from aggregated data | "Aggregators" |
| Proposal | the core creates a record from the body the bot submitted | a record in state `screened` | "Proposal" |
| Checks and commission | code, then the commission lenses | the code verdict | "Code checks and commission" |
| Digest and cards | the core assembles, a reporter delivers | items for people and an audit sample | "Digest, cards and audit sample" |
| Decision | a person with the permission | "Accept", "Decline", "Postpone" | "Human decision" |
| Realization | a human executor or an object of the ordinary path | `realized` through a verified link | "Realization" |
| Change trial | core | outcome: better, unchanged, worse, undetermined, mixed | "Change trial" |
| "My outcomes" | core | a closed table of outcomes for the author | "My outcomes" |
| Promotion | the core loop `governance` | a queue to the commission | "Handover to promotion" |

On this path the model writes only the proposal body, the lens scores and the reporter's optional introduction. The numbers, checks, verdict, digest, realization and trial outcome are made by code and people.

The lifecycles of a proposal and a trial are described by tables in [proposal.md](../spec/lifecycles/proposal.md) and [trial.md](../spec/lifecycles/trial.md).

## Observations

An observation is a number or a value from a closed list (`enum`) that a bot sees from its seat: attempt duration, queue wait, machine load by hour, age of the last backup of a dataset, the share of failed tasks by reason. People usually do not read these series; analysis, proposal evidence and the change trial stand on them.

### Observation series

An observation series (`ObservationSeries`) is declared in the tenant's git, like other manifests. A series value is only of a closed leaf kind (INJ-1), the subject is taken from the catalog, the data class is not lower than the class of the source, and a series has one tenant (OBV-2). A series without a data class or with a value of a free string kind does not pass `apply`.

```yaml
# tenants/demo/observations/host.load.v1.yaml
kind: ObservationSeries
metadata: {name: host.load.v1, tenant: demo}
spec:
  catalog_kind: host                     # kind of catalog subject
  value: {type: number, minimum: 0, maximum: 10000}
  unit: load_x100
  data_class: internal                   # not lower than the tool's output_class
  source: {kind: tool, tool: monitor/item_history, field: cpu_load}   # monitor plugin (MON-6)
  resolution: 5m
  rollups: [hour.max, hour.mean, day.p95] # from the closed list of rollups
  retention: {raw: 14d, rollup: 93d}
  outcome: {compare: day.p95, better: down, tolerance_pct: 10, min_samples: 10}
```

The `outcome` block is optional for a tenant series. It is needed only if the series is to be made a metric or a guard of a change trial (OBV-6).

### Who writes the points

An observation point (series, subject, time, value, producer, tenant) is written only by code (OBV-1). There are three sources:

| Source | Who writes | Example |
|---|---|---|
| `core` | the core loop `observe` from core records and events of the end of work outside the farm | `farm.task.outcome`, `farm.queue.wait`, `farm.backup.age` |
| `job` | a job worker through P1 `obs.append` with an attempt token (since v0.2), only into series from the `observes` of its task type | a backup check series computed by a job type |
| `tool` | the core extracts a closed field from tool output by the mapping in the tool manifest | `host.load.v1` from the output of `monitor/item_history`: machine load is measured by the monitoring system, and the `monitor` plugin only reads it (MON-6) |

An agent attempt has no operation for writing observations, and the model never writes a point. This way the data on which people later decide cannot be poisoned by model text.

### How observations are read

Observations are read only by a closed query (OBV-3). The bot calls the built-in tool `observations_query` (port P3, tier 1): the series is taken from an `enum` within the role's `insight.observes`, the subject from the catalog, the window from a list (1d, 7d, 14d, 93d), the rollup from an `enum`. An aggregator also has the `group_by` parameter within its `sources`. The answer contains only numbers and `enum` values, its size is limited, and reading does not set the untrusted text flag `untrusted_seen` on the attempt: an instruction cannot be hidden in a number. A value can be distorted by the observed world itself (someone deliberately raises the number of errors), so it is not the numbers by themselves that decide but the commission and people. The points of one tenant are not visible in any query of another (TEN-4).

A `job` worker without a model (for example, the monthly report pipeline) reads the rollups of its own tenant with the same closed query through the P1 operation `obs.query`: the series only from the `observes` of its task type, and it has no store credentials (OBV-3, DEP-8). People read the same rollups with the P2 operation `obs.series`.

### Rollups and retention

A rollup (a series value over a window, for example the maximum over an hour) is set by the series manifest: the window, a function from the closed list `spec/model/rollups.yaml` (`hour.max`, `hour.mean`, `day.p95` and others) and a data class not lower than the series class. Rollups are computed by code (OBV-4). Points and rollups belong to the retention class `observation`: by default points 14 days, rollups 93 days. They are deleted by period only in whole periods (RET-1, RET-2), without selective editing of rows. More about retention classes in [operations.md](operations.md).

### Core series and what "better" means

Series of the `core` source are computed by the deterministic core loop `observe` only from records of the same tenant: attempts, tasks, usage lines, schedule slots and events of the end of work outside the farm (OBV-5). Identical records give identical points. Where a series coincides with a series of the farm metrics catalog, they have one name.

The skeleton sets the core series registry `spec/model/core-series.yaml`, and the core keeps these series for each task type, schedule and dataset of the tenant (OBV-6). Each registry series has an `outcome` block: the unit, the rollup for comparison, which event is good, in which direction the series is better, the tolerance within which there is no change, and the minimum number of events per window (`min_samples`). Without such a shared definition of "slow" and "reliable" a bot either stays silent or draws conclusions from noise, and the change trial has nothing to measure with.

| Series | What it measures | Subject | Comparison | Better | Tolerance | `min_samples` |
|---|---|---|---|---|---|---|
| `farm.attempt.duration` | attempt duration (p50, that is the median, and p95, the 95th percentile) | task type | p95 over the window | lower | 10% | 10 attempts |
| `farm.task.outcome` | task outcome by reason | task type | share of `done` | higher | 5 pp | 14 tasks |
| `farm.task.attempts` | number of attempts per task | task type | mean | lower | 10% | 14 tasks |
| `farm.queue.wait` | queue wait until the first dispatch | task type | p95 over the window | lower | 15% | 20 tasks |
| `farm.task.cost` | task cost by usage lines (BUD-3) | task type | mean | lower | 10% | 14 tasks |
| `farm.schedule.slot` | schedule slot: on time, late, skipped | schedule | share "on time" | higher | 5 pp | 10 slots |
| `farm.backup.age` | age of the last successful backup | dataset | maximum per day | lower | 10% | 10 days |
| `farm.external.duration` | duration of work outside the farm | task type | p95 over the window | lower | 10% | 10 runs |
| `farm.external.retries` | retries of work outside the farm | task type | mean | lower | 10% | 10 runs |
| `farm.external.outcome` | outcome of work outside the farm (the event's `outcome`: `done`, `failed`, `timeout`, `canceled`) | task type | share of `done` | higher | 5 pp | 10 runs |

The tolerance numbers and `min_samples` are skeleton defaults. Only a series with a complete `outcome` block can be a trial metric or guard; a registry series without a complete block does not pass the lint (OBV-6).

```yaml
# spec/model/core-series.yaml (fragment)
- name: farm.task.outcome
  catalog_kind: task_type
  values: [done, failed, deadline_exceeded, canceled]
  outcome: {unit: share, compare: window.share, good: done, better: up, tolerance_pp: 5, min_samples: 14}
- name: farm.backup.age
  catalog_kind: dataset
  outcome: {unit: h, compare: day.max, better: down, tolerance_pct: 10, min_samples: 10}
```

The same registry holds two guard tables. A guard is a series that must not get worse while the benefit is measured: moving a schedule can speed up a backup but delay a report. The core takes from one to five guards from both tables: by the kind of the change target and by the metric the bot promised (PRP-21). The first table, by the kind of target:

| Change target | Guards |
|---|---|
| schedule (`Schedule`) | `farm.schedule.slot` of this schedule, `farm.attempt.duration` of its type |
| backup schedule window | `farm.backup.age` of the dataset (required), `farm.schedule.slot` |
| tenant dispatcher rule | `farm.task.outcome` and `farm.queue.wait` of the type, `farm.task.cost` |
| tenant limit (`limits.*`) | `farm.queue.wait` and `farm.task.outcome` of the affected types |
| a dataset's reference to a backup in the catalog | `farm.backup.age` of the dataset |
| `action_request`, `script`, `human_change` | `farm.task.outcome` of the affected types |

The second table, by metric. Without it a bot could improve a metric at the expense of what is not guarded: doubling the attempt deadline raises the share of `done`, while the p95 duration doubles, and the report arrives after the start of the working day.

| Trial metric | Guards |
|---|---|
| `farm.task.outcome` | `farm.attempt.duration` of the same type |
| `farm.attempt.duration` | `farm.task.outcome` of the same type |
| `farm.task.attempts` | `farm.attempt.duration` and `farm.task.cost` of the same type |
| `farm.queue.wait` | `farm.task.cost` of the type and the load series of the subjects from `subjects`, if such a series is declared with an `outcome` block (for example `host.load.v1` for `db1`) |
| `farm.task.cost` | `farm.task.outcome` and `farm.attempt.duration` of the same type |
| `farm.schedule.slot` | `farm.attempt.duration` of the schedule's type |
| `farm.backup.age` | `farm.schedule.slot` of the backup schedule |
| `farm.external.*` | the other `farm.external.*` series of the same type |

A tenant can only add guards to both tables with the field `insights.extra_guards` (a list "metric: guards"); a table guard cannot be removed, and a manifest with such an attempt does not pass `apply` (PRP-21).

### Work that ends outside the farm

Part of the work continues outside the farm after the task has submitted the outcome "accepted for run": an external pipeline, a database backup outside the manifests. Without a separate signal the farm sees neither the duration nor the retries of such work. So its end comes as the event `run.finished.v1` (OBV-7). A job worker submits it with the `emit` operation of port P1, if this event type is in the `events` of its task type. The event fields are closed: the work id, an optional approval number `approval_id` (by it a route finds the `approval_initiator` address, DLV-6), a subject from its own tenant's catalog, `duration_s`, `retries`, `exit_code` and `outcome` from a closed list. The `source` and `id` fields of the event are set by the core (IDM-4).

```yaml
type: run.finished.v1
data:
  run_id: r_7f3a
  approval_id: 4813       # optional: approval number, if an action started the run
  subject: ctd-stations  # id from its own tenant's catalog
  duration_s: 2710
  retries: 1
  exit_code: 0
  outcome: done          # done | failed | timeout | canceled
```

The points of the `farm.external.*` series and the backup age for such an event are written by the core itself in the `observe` loop, without `obs.append`.

## Seat analysis

Seat analysis is not a new concept but a task type of the `insight` purpose for a functional role. Only roles of the `worker` and `manager` profiles can declare an `insight` section (PRF-1).

### Role section and money

```yaml
# roles/ops-runner/role.yaml (fragment)
spec:
  profile: worker
  models:
    base: base-std             # main work
    analysis: analysis-std     # only the insight and aggregate purposes
  insight:
    types: [ops.insight.v1]
    observes: [farm.task.outcome, farm.schedule.slot, farm.attempt.duration, farm.backup.age, host.load.v1]
    tools: [monitor/item_history]   # a subset of the role's tools, tier not higher than 1; output only numbers (untrusted_output: false, MON-6)
    max_data: internal         # ceiling of the analysis mark
    max_runs: {per: week, count: 2}        # runs by schedule
    max_event_runs: {per: day, count: 1}   # runs from alarm.* and obs.* events
    merge_window: 1h                       # merge window, not shorter than an hour
    max_open_proposals: 3      # limit of the role's open proposals
    max_per_week: 3
---
# tenants/demo/tenant.yaml (spec fragment)
models_allowed: [base-std, analysis-std, review-a, review-b, seat-std, manager-std, translate-std]   # seat-std, manager-std, translate-std are needed by seats, managers and the translator
budgets:
  month_eur: 300
  lines:
    insight: {month_eur: 15, per_role_month_eur: 5}
    review:  {month_eur: 50}
insights:
  present_to: "chat:demo-ops"
  digest: {schedule: "0 9 * * 1", max_items: 5}
  realizers: [bob]
  realize_by: 7d
  decide_ttl: 14d
  recommend_ttl: 28d
  park_ttl: 30d
  review_wait_max: 7d
  redecide_after: 30d
  max_in_review: 10
  extra_guards:                          # only adds to the registry tables
    - {metric: farm.task.outcome, guards: [host.load.v1]}
```

The analysis model is taken only from the role's `analysis` slot (GW-7), so the role's base model and the model for analyzing its own data are different, as D-16 requires. Spending goes to the `insight` budget line (BUD-7), which is 0 by default: analysis is turned on explicitly with money and per role. Analysis, aggregator and review tasks go in the lowest dispatcher priority class (5, `background`) and take no more than their share of the bot group's places (BUD-8), so the main work does not suffer from them. The `apply` lint checks that the money of the `review` line is enough for the reviews of the proposal ceiling of the roles with analysis (BUD-13); the rule is described in [models-and-money.md](models-and-money.md). The price of one review in the lint and the reserve before reviews are the same number: the `tree_budget_eur` of the review type, a required field of every type with a model (COM-6); the model profiles of the lenses have `max_output_tokens` not more than 2000, so that the worst case of one call does not exceed this number. In this example the calculation is as follows. The role `ops-runner` runs analysis at most 2 times a week, that is up to 9 times in 31 days, and each result carries at most 3 proposals (INS-1): up to 27 proposals per month. The types `review.security.v1`, `review.operational.v1`, `review.cost.v1` and `review.value.v1` have `tree_budget_eur` equal to 0.08 EUR (rank 3 profiles: `seat-std` for seats, `review-a` and `review-b` for fallback roles), that is, four lenses cost 0.32 EUR per proposal, and since v0.2 `review.farm_manager.v1` is added to them on the farm manager seat profile `manager-std` of rank 4 with `tree_budget_eur` 0.20 EUR; the shadow is turned off with an explicit `shadow: off`. The lint counts by the full commission, although a proposal of kind `none` is scored by only two lenses (COM-14): what kind the proposals will be is not known in advance. The role's estimate under the v0.2 rules is about 27 × 0.51 = 13.9 EUR. One author role has access to 30% of the `review` line (COM-6), with 50 EUR that is 15 EUR, and the lint passes. The weekly aggregator `ops-aggregator` (section "Aggregators") adds up to 5 runs of 3 proposals, about 7.7 EUR, and the sum of the estimates, about 21.6 EUR, is less than the line's monthly limit. With a `review` line of 40 EUR the role's share would be 12 EUR, and `apply` would reject such a set of manifests, and `farmctl doctor` would show the missing 1.9 EUR. If the shadow is turned on with the default shares (0.25 for lenses with veto and 0.2 for the others, COM-10, D-84), each proposal gets about 0.25 × 2 + 0.2 × 2 = 0.9 more reviews of rank 3, about 0.072 EUR, the role's estimate grows to about 13.9 + 27 × 0.072 ≈ 15.8 EUR, and the lint requires a `review` line of at least 15.8 / 0.3 ≈ 53 EUR.

### Start, permissions and output

A type of the `insight` purpose has a tier not higher than 1, and its attempt has no `request_action`, `enqueue_child`, `propose_campaign`, `propose_script` and `send_message` (INS-1). Analysis is started only by a schedule or by a route from `obs.*` and `alarm.*` events, including alarms that a `job` role raised on monitoring system problems (MON-5); an agent message does not start analysis. Analysis sees the load and disks of machines as series that the core extracted from the output of the `monitor` plugin, not by its own measurement on the machines. A route from events goes only through the merge window `insight.merge_window` (debounce: all events within the window give one run), and within the window at most one analysis of the role runs. The window is not shorter than 1 hour, 1 hour by default; a smaller value does not pass `apply` (INS-4). Otherwise whoever writes to the logs could trigger alarms and burn the `insight` and `review` lines, and a nightly series of problems on `web1` would eat the weekly analysis quota with noise.

The rate is limited by `insight.max_runs` (runs by schedule), `insight.max_event_runs` (runs from events, 1 per day by default; they do not spend a schedule slot) per role in the tenant, and by `insight.max_open_proposals`; the states `screened`, `in_review`, `recommended`, `presented` and `parked` count as open (INS-4). On exceeding them the schedule slot or the route is skipped with an event, and the model is not called.

The initiator of analysis is `system`. The attempt's permissions equal the intersection of the tools `insight.tools` (a subset of the role's `tools`), the `system-ops` group (PRM-10), tier 1 and a mark not higher than `insight.max_data`; at `apply` the lint checks that the analysis is not wider than the role's main work (INS-2).

The output of analysis is a task result by the `insight-result` schema: at most three proposals or the code `nothing_found` (INS-1). There is no separate "propose" tool. Proposals are created by the core only on a successful `complete` of the attempt (P1), in the same transaction (DLV-2); a failed, canceled or `fenced` attempt leaves nothing. A proposal inherits the data mark `data_mark` and the `untrusted_seen` flag of the attempt together with the sources (INS-3, INJ-13).

Analysis can also be done by code. A job type of the `insight` purpose (for example "a dataset in the catalog has no reference to a backup, and the `farm.backup.age` series is empty") submits proposals without a model and without the untrusted text flag. This is the preferred path for everything that a rule can check; a model is needed where several series must be connected and a change must be invented.

### Whose analysis it is

The dispatcher gives `insight` tasks to the role's bots in turn: the run goes to the role's bot with a ready worker that has gone longest without getting an analysis of this role, and in a tie to the bot with the lower id (INS-5; condition D16 in [coordination.md](coordination.md)). Besides the role's series, the analysis input contains the series of the bot's own attempts (by the field `acting_bot`, the bot on whose behalf the attempt ran) and the last rows of its "My outcomes" (DIG-7). The author of a proposal is recorded as the bot whose attempt submitted the result (PRP-14). This way "one's own improvement" does not turn into a lottery of task dispatch.

### The "loop disabled" signal

The improvement cycle is easy to turn off unnoticed: the `insight` and `review` lines are 0 by default. So the core considers a tenant's cycle disabled if at least one reason from a closed list holds (INS-6):

- no role has an `insight` section;
- a role with an `insight` section has an `insight` line equal to 0;
- the tenant's `review` line equals 0;
- there is no `insights.present_to` address;
- a lens with veto has no executor (COM-7, COM-8);
- the tenant has no active person with `proposal.decide`.

The `farmctl doctor` check and the `apply` lint print "improvement cycle disabled" with reasons for each role. The weekly proposal digest with a disabled cycle arrives even without items and carries a code line, for example "Improvement cycle disabled: role ops-runner has an insight line of 0 EUR", if there is an address for it.

## Aggregators

An aggregator is a bot of a role of the `aggregator` profile (PRF-1): it looks not at one machine but at a bot group or the whole tenant, and proposes changes one level higher, for example "three machines out of four have the same nightly peak" (D-17). The purpose of its tasks is `aggregate`, the model of the `analysis` slot, the budget line `aggregate`, and the output is only proposals in the task result (INS-3).

```yaml
# roles/ops-aggregator/role.yaml (fragment)
spec:
  profile: aggregator
  claims: [aggregate.ops.weekly.v1]
  models: {base: base-std, analysis: analysis-std}
  aggregate:
    sources:
      roles: [ops-runner]
      bot_groups: [compute]
      series: [farm.schedule.slot, farm.attempt.duration, host.load.v1]
      message_kinds: [report.ops.v1]     # only the data field
    max_sources: 50
```

An aggregator's inputs are only aggregated (AGG-2):

| Input | Built-in tool | What is visible | What is not visible |
|---|---|---|---|
| series rollups over many subjects (OBV-4) | `observations_query` with `group_by` | numbers | points outside `sources` |
| bot reports and states | `messages_digest` | the `data` field by the closed schema of the message type | message `note`s |
| proposals people have already decided on | `proposals_query` | closed fields, verdicts, people's decisions | authors' `note`s, reviewers' notes |
| decision statistics | `observations_query` over `farm.proposals.*` | shares and counters | feedback texts |

Logs, files, artifacts, sessions, memory and plugin tool output are not available to an aggregator at all. An injection through a bot report can only shift a number within the schema, not append an instruction.

A tenant has one level of aggregators: an aggregator reads only the sources from its `aggregate.sources` and does not read other aggregators and their proposals (AGG-3). `max_sources` limits the number of source entries: roles, bot groups, series and message kinds together. `apply` rejects an aggregator that names an aggregator role in `sources.roles` or through a bot group in `sources.bot_groups` (`aggregator_source_aggregator`), or that lists more entries than `max_sources` (`aggregator_sources_over_max`). At run time `proposals_query` returns no proposal of another aggregator, and `messages_digest` returns no kind outside `sources.message_kinds`. Marks do not drop: the `data_mark` of an aggregator's rollup and proposal equals the highest mark of the inputs, and `untrusted_seen` equals the logical OR of the inputs with their sources (AGG-4).

There is no aggregation between tenants: an aggregator process serves one tenant (TEN-4), and read tools return only its records (AGG-5). There is no bot for a view of the whole farm. The operators of the service tenant `farm` see it through the metrics of the telemetry allowlist (OBS-1) in the monitoring system (`docs/operations.md`) and through the manager's daily digest (MGR-41). No bot attempt, including in tenant `farm`, reads the records of several tenants (TEN-5). A series value of tenant `demo` does not appear in any query, model call or proposal of tenant `demo2` or tenant `farm`.

An aggregator's proposal is scored on its own, like any other, and does not take credit from a bot that proposed the same earlier. If it changes the same field as an open bot proposal, it is a conflict (PRP-9); if it is similar to a proposal people have already decided on, it gets the mark `related_to` and gives no credit (PRP-17).

## Proposal

### Body and record

A proposal (`Proposal`) is a closed record. The bot submits only the body, and it passes the closed schema `botaru.proposal/v1`; the record fields (id, author, fingerprint, marks, state, scores, decision, realization, trial) are written only by the core, like the fields of any farm record (PRP-1, IDN-1).

```yaml
# the body the bot submits (schema botaru.proposal/v1)
category: schedule            # closed list: schedule, backup, capacity, cost,
                              # reliability, security, data_quality, config
problem:
  code: backup_deadline_miss  # closed list of codes per category
  subjects: [task_type:demo.db.backup.v1, schedule:report-nightly, host:db1]
  severity: medium            # low | medium | high
evidence:                     # at most 10 references, the core rereads the values
  - {kind: observation, series: farm.task.outcome, subject: task_type:demo.db.backup.v1, window: 14d, rollup: window.share, value: 0.71}
  - {kind: observation, series: host.load.v1, subject: host:db1, window: 14d, rollup: hour.max, slot: "02:00", value: 910}
change:                       # exactly one kind
  kind: manifest_change
  manifest_change:
    ops:
      - {op: set, target: {kind: Schedule, name: report-nightly}, field: spec.cron, value: "30 3 * * *"}
benefit: {metric: farm.task.outcome, subject: task_type:demo.db.backup.v1, direction: up, expected_change_pp: 20, confidence: medium}
cost: {one_time_eur: 0, monthly_eur: 0, effort: minutes}
risk: {level: low, kinds: [report_delay]}
note: "The report and the backup run in the same window on db1..."   # up to 1000 characters, always untrusted
---
# the record that only the core writes
id: p_0101
tenant: demo
origin: {role: ops-runner, worker: w_0007, acting_bot: bot_17, bot_epoch: 1, task: t_5521, level: 0}
fingerprint: "sha256:..."     # category, problem code, subjects, kind and targets of the change
data_mark: internal
untrusted_seen: {value: false, sources: []}
self_influence: [{series: farm.task.outcome, author_share: 0.0, role_share: 0.0}]
related_to: []
conflicts_with: []
overlaps_with: []
seen_count: 1
state: screened
reviews: []
verdict: null
decision: null
realization: null
trial: null
```

`origin.level` is 0 for a proposal of a bot from its seat and 1 for a proposal of an aggregator. The `note` is not longer than 1000 characters and is always untrusted: code does not read it, reviewers do not get it, and it is shown to people only as a separate marked block in "Details" (PRP-1). Closed fields and decisions belong to the retention class `metadata`, the `note` to the class `content`.

### Change kinds

The `change` field contains exactly one change kind (PRP-3):

| `change.kind` | What is inside | What "Accept" creates | Who actually changes the world | Since |
|---|---|---|---|---|
| `none` ("for information") | only the problem and evidence; only the `value` and `security` lenses score it (COM-14) | nothing; the proposal moves to `noted` | nobody | v0.1 |
| `action_request` | an action and arguments by a closed schema, within the vocabulary of the author role; or a call of `service/control` or `monitor/script_run` within the author role's `service_control` and `monitor_scripts` | an action approval on behalf of the one who accepted, with the source "proposal p" | a person with the permission to approve the action, then the toolhost | v0.1 |
| `script` | a `botaru.script/v1` draft within the `scripts` section of the author role with `propose: in_proposal` | a script run in state `proposed` on behalf of the one who accepted; acceptance by the core (SCR-5) and a run card (SCR-6) | the same people and buttons | v0.1 |
| `manifest_change` | up to 10 `set` operations on allowlist fields or `create_from_template` by a template from git | a patch that the core builds in code and hands to the executor | the executor, with a commit to git | v0.1 |
| `human_change` | a closed description: object, field, direction, value (PRP-13) | a card to the executor with the description and a deadline | the executor, by hand and with a commit | v0.1 |

A proposal runs nothing by itself (PRP-4). No proposal transition, except the human buttons "Accept" and "Lift veto", creates a task above tier 0, a tool call, an approval, a script run, a patch or a message, except the delivery of the digest and cards. The farm does not write to git itself. How a script passes acceptance and a run is described in [scripts-and-translator.md](scripts-and-translator.md).

### What can be changed

A proposal changes only objects of its own tenant (PRP-8). The target of `manifest_change`, `human_change` and a guarded field can only be an object from the tenant's folder in the farm git. For shared objects (role, task type, plugin, action) only the kind `none` is possible, otherwise the refusal `rejected(forbidden_change)`: a task type is shared by all tenants, and changing it based on one tenant's numbers would affect the others.

The fields that may be proposed for change are listed in the allowlist `spec/model/proposable-fields.yaml`. The skeleton sets the list, and a tenant can only narrow it (PRP-3).

| Object | Allowed | Never allowed |
|---|---|---|
| the tenant's `Schedule` | `spec.cron` only as a window shift without lowering the frequency by more than 2 times; `spec.timezone`; `create_from_template` | `spec.paused` (only by kind `none`), `deliver_to`, the task type |
| the tenant's `Catalog` | add a dataset's reference to a backup schedule or replace it with a more frequent one; labels that are not read by selectors, the dispatcher and the involvement rules | remove a backup reference, make a backup less frequent; new resources with addresses; egress |
| `Tenant` | lower `limits.*`, `campaigns.*`, `scripts.*` | budgets up, budget lines, retention periods, `self_approve`, `models_allowed` |
| tenant dispatcher rule (`dispatch.rules[]`) | guarded fields (PRP-12) | the rest |
| `TaskType`, `Role`, `ToolPlugin`, `Action` | only the kind `none` | everything |
| `Group`, `Membership`, `Channel`, `ChatBinding`, `Sink`, `ModelProfile`, `Commission`, the charter, `Seat`, `IncidentClass`, `ObservationSeries` | nothing | everything |

A change of permissions, groups, tools, addresses, models, budgets upward, budget lines, retention periods, the commission, the charter and seats, incident classes, involvement rules, `control-ops.yaml`, the farm manager's limits and safeguard, consent groups, the protected set, the `farm-manager` role, the `farm-manager-ops` group, observation series, farm nodes and templates in the monitoring system (`deploy/monitor/`) and the table of monitoring problem classes is rejected with `rejected(forbidden_change)`. A seat holder does not change these targets with operations either (MGR-16). A proposal about permissions and security is possible only with the kind `none` and the category `security`: people decide themselves what to change in git. The card and the patch show in code the schedule frequency before and after and the affected alarms. A bot proposes a start, stop or restart of a service on a machine with the kind `action_request` and the tool `service/control`, and a monitoring system script with the tool `monitor/script_run`, if its role has the permission on this service (`service_control`) or on scripts (`monitor_scripts`) (MON-8, MON-10, D-67). "Accept" creates an approval of this call on behalf of the one who accepted, and it is run under the same guards as a bot call: 6 actions per service per hour, one operation per service at a time, holds, a note in the monitoring system. Without the permission, and for a proposal with `untrusted_seen`, such an idea is proposed only with the kind `none` (PRP-11, D-70).

A task type and a schedule carry the `proposable` flag (`true` by default) (PRP-10). The `apply` lint requires `proposable: false` for types of any purpose except `work`, for types with `alarm.*` in `events`, for the `job` type that re-checks monitoring system problems and raises alarms on them (MON-5), for the type that writes series from the monitoring history (CAP-6), for types that an incident class refers to, and for types with `makes_backup: true`; a schedule of such a type is not proposed either. The core uses the same protected set for the operations of the farm manager (MGR-32). A target with `proposable: false` gives the refusal `rejected(forbidden_change)`; the only exception is a shift of the backup schedule window by a guarded field.

Backups change only in the safe direction (PRP-11). An allowlist field has its safe direction recorded, if it has one: a tenant limit down, a backup more frequent. A dataset's reference to a backup can be added or replaced with a more frequent backup; removing it or making the backup less frequent is not allowed, and a proposal never lowers the frequency of a backup schedule.

Since v0.2 the limit on lowering a schedule's frequency is counted cumulatively: per 30 days, the new value of `spec.cron` gives at least half of the runs of the value applied 90 days earlier (PRP-16). The core takes that value from the history of applied sets, not from git, because the git history can be rewritten. A proposal below the limit gets `forbidden_change`. Otherwise a series of small "a bit less often" proposals would get around the limit.

Before the "Accept" button for a proposal that changes the `cron`, time zone or pause of a schedule, the core since v0.2 computes the slots of the new schedule a year ahead and shows on the card the local times that disappear and repeat and the overlaps with other schedules and maintenance windows (TIM-5, TIM-3). An expression or time zone that does not pass `apply` gets no "Accept" button. Details in [operations.md](operations.md).

### Guarded fields

Typical reliability and speed improvements concern the attempt deadline, the number of retries, concurrency and the backup window. These fields are dangerous without supervision, so they are part of the `guarded` section of the allowlist and change only with guards (PRP-12):

| Field | Where | Limits | Required guard |
|---|---|---|---|
| type attempt deadline (`timeouts.attempt`) | tenant dispatcher rule | up at most twofold and not beyond the task deadline | `farm.task.outcome`, `farm.queue.wait` |
| number of attempts (`retry.max_attempts`) | same place | only down, not less than 1 | `farm.task.outcome` |
| rule concurrency (`max_running`) | same place | by 1 up or down within the tenant ceilings | `farm.queue.wait`, `farm.task.cost` |
| backup schedule window | backup schedule | a shift without a frequency change | `farm.backup.age` |

A proposal with a guarded field is recommended only if both lenses with veto gave at least 3 and the trial of its change has guards. An untrusted proposal does not change guarded fields. The attempt deadline and the number of retries live in the tenant dispatcher rule, not in the shared task type, so the change stays inside the tenant; dispatcher rules and an example with these fields are described in [coordination.md](coordination.md).

### Change by people's hands

Not every useful idea can be expressed as an allowlist field. For them there is the kind `human_change`: a change that people make by hand, with a closed description (PRP-13). The description contains a tenant object (kind and name), a field from the registry `spec/model/change-paths.yaml`, a direction (`up`, `down` or `set`) and a value (a number or an `enum`). There is no free text in the description; the card and the reviewer input show it in code words. The path registry contains no targets forbidden by the allowlist.

```yaml
change:
  kind: human_change
  human_change:
    target: {kind: Host, name: db1}       # a catalog resource of tenant demo
    path: catalog.host.disk_gb
    direction: up
    value: 200
```

After "Accept" the executor gets a card with the description and a deadline. The proposal becomes realized only by a linked commit that changes the named field in the named direction (PRP-19). This way an idea outside the allowlist gets realization, a trial and credit, and the kind "for information" stays only for what people decide themselves.

### Evidence and self influence

Evidence is at most 10 references only to objects of its own tenant that the author could read (TEN-7, PRM-9, OBV-3). The core itself rereads the cited values at creation and once more before showing to people (PRP-2). A mismatch above the series tolerance at creation gives the refusal `rejected(evidence_mismatch)`, and before showing it removes the item from the digest into `parked(evidence_drift)`. People see the core's numbers, not the bot's numbers.

A bot whose attempts themselves produce the numbers could create a problem and then propose to "fix" it. So for each evidence series the core at creation computes `self_influence`: the share of points produced by the attempts of the author bot (`author_share`; while bots have no numbers, of its worker), and the share of points of its role (`role_share`) (PRP-15). With `author_share` of at least 0.3 the people's card and the reviewers' input carry the mark `self_evidence` without the author's identity. The change trial also computes the outcome three times: over all points, without the points of the author bot and without the points of all bots of its role, so that two bots of one role cannot "worsen before" and "improve after" together (PRP-22).

### Author, duplicates and limits

The core records in `origin` the author bot (`acting_bot`) and its epoch (`bot_epoch`) from the attempt that submitted the result, next to the role, worker, task and level (PRP-14). If the result was submitted by a worker without a numbered bot (IDN-8), the field is empty, and `origin.worker` sets the author. When duplicates merge, the author does not change.

The proposal fingerprint (a hash of the category, problem code, subjects, kind and targets of the change) is computed by the core (PRP-5). An open duplicate does not create a second proposal: `seen_count` grows, and evidence is added. A fingerprint declined by a person is not accepted for `redecide_after` (30 days by default) if the evidence did not change by more than the declared share; the refusal is `rejected(recently_declined)`. The limits `insight.max_open_proposals` and `insight.max_per_week` per role and `max_in_review` per tenant limit the flow; exceeding them gives the refusal `proposal_rate_limited` with an event and without a record. Since v0.2 each bot has at most one open proposal (PRP-16): the bot chooses the best one rather than flooding the commission. The limit is fixed at 1 and has no manifest field; a worker without a numbered bot counts as one bot. A proposal that waits in `parked(conflict)` because of an earlier accepted proposal of another author does not take this slot.

Since v0.2 the core links a proposal with proposals of another author that people decided on in the last 90 days, if they have the same change kind and their targets or fields overlap (`related_to`) (PRP-17). People and reviewers see the mark. If the related proposal was accepted or declined, the later proposal gives the author no credit, although the change itself can pass the commission, people and the trial as usual. Relatedness is computed only among proposals of functionaries and of aggregators (`origin.level` 0 and 1): proposals of the farm manager from the daily review (a role of the `farm-manager` profile, `origin.level` 2, MGR-41) do not enter `related_to` and do not take credit from functionaries, otherwise the manager, who sees every tenant daily, would intercept credit in all tenants.

### Conflicts

Two accepted changes of one field overwrite each other, and the trial measures someone else's value. So the core finds conflicts (PRP-9): two proposals conflict if they change the same field of the same object. For each proposal the core computes `conflicts_with` and `overlaps_with` (shared subjects by change targets). Only one of the conflicting proposals can be accepted: when one of them has moved to `realizing`, the other open ones go to `parked(conflict)`, and their cards go dark. They return to `screened` when the trial of the accepted one has ended. A new proposal that changes a field of an object with a running trial also waits in `parked(conflict)`. In the digest, conflicting recommended proposals go as one block, and the reviewer input gets a code table: changes of the same subjects over 30 days with trial outcomes and open proposals about them.

### Untrusted text

If the analysis attempt saw untrusted text (a log, plugin output, a message with a `note`), the proposal carries `untrusted_seen` and is marked on all cards and in the digest together with the source (PRP-7). Its change can only be `none` or `manifest_change`, and only in the safe direction of the fields; otherwise the refusal `rejected(untrusted_direction)` (PRP-11). It has no script, action, change by people's hands or guarded fields. The strict verdict threshold `untrusted` applies to it (COM-4).

### Refusal at creation

A refusal at creation creates no record: the core writes the event `proposal.rejected.v1` with the reason, like a refusal to enqueue a task. The reasons come from a closed list: `schema_invalid`, `evidence_mismatch`, `forbidden_change`, `untrusted_direction`, `metric_unknown`, `recently_declined`, `proposal_rate_limited`. The author sees its refusals in "My outcomes" and does not spend money on repeats that are dead in advance.

## Code checks and commission

### Code checks

Code checks run before any review in a constant order (COM-5):

1. body schema;
2. evidence: the core rereads the values (PRP-2);
3. duplicate by fingerprint (PRP-5);
4. empty change: all values of the change are already set, outcome `dismissed(no_op)`;
5. conflict (PRP-9);
6. feasibility: the tenant has an active person with the permission to accept and start the realization;
7. money for reviews (COM-6).

A proposal that fails a check goes to `dismissed` with a reason without spending money on reviews; on a conflict and a lack of money it waits according to PRP-9 and COM-6. The order and set of checks are set by the skeleton, not by the tenant manifest.

### Lenses

A lens is the reviewer's point of view: benefit, cost or risk, as D-16 requires. The list of lenses, their review task types and veto (the right to forbid) are set by the skeleton in `spec/model/lenses.yaml` (COM-8):

| Lens | What it scores | Veto | Review task type |
|---|---|---|---|
| `security` | permissions, data, leaks, untrusted text | yes | `review.security.v1` |
| `operational` | reliability, deadlines, load, the possibility of rollback | yes | `review.operational.v1` |
| `cost` | proportionality of price and benefit; code computes the price itself | no | `review.cost.v1` |
| `value` | benefit by the numbers | no | `review.value.v1` |
| `farm_manager` | the farm manager's view, weight 1 (since v0.2) | no | `review.farm_manager.v1` |
| `necessity` | only bot escalations to the manager, not proposals (since v0.2) | no | `review.escalation.v1` |

Code computes the price of a change from the price table of the provider binding and from observations; the cost lens model judges only the proportionality of price and benefit. The `farm_manager` lens is on every tenant proposal since v0.2: the manager is part of every commission with one vote and without veto (D-29, GOV-25). This lens is not set on proposals about the manager seat, its role, the incident catalog, involvement rules, the manager's limits, consent groups, or on its own proposals (GOV-17).

### Who runs the lenses

The lenses are held by commission seats, which the tenant charter describes ([commission.md](commission.md)); a lens whose seats are all empty is run by a fallback role of the `reviewer` profile (PRF-1) that has this lens's type in `claims` (COM-8). Until the v0.2 votes, seat holders are assigned by a person with the P2 operation `seat.assign` (D-21). Each lens at each moment has exactly one executor source: the holders of this lens's seats, if at least one seat is taken, otherwise the reviewer role that the charter names as the fallback (`fallback_role`) (COM-7). A seat whose holder is under a people's hold of scope `bot` counts as free here: its tasks are not dispatched, and the lens is run by other holders or by the fallback role. Lens tasks are not dispatched to two sources at once, and one proposal does not get two reviews of one lens. The `apply` lint requires a holder or a fallback role for each lens with veto, and P2 `commission.status` and the `doctor` check show who covers each lens. This way declaring empty seats does not send all proposals into waiting.

```yaml
# tenants/demo/governance.yaml (charter fragment, described in commission.md)
spec:
  commission:
    seat_role: commission-member
    model_profile: seat-std
    seats:
      - {id: 1, lens: security,    fallback_role: reviewer-a}
      - {id: 2, lens: operational, fallback_role: reviewer-a}
      - {id: 3, lens: cost,        fallback_role: reviewer-b}
      - {id: 4, lens: value,       fallback_role: reviewer-b}
    audit_sample: {min_per_month: 4, share: 0.25, max_per_digest: 2}
```

Reviews are independent (COM-2). The reviewer role is not the author role; a review task is not dispatched to the author's worker and bot; the reviewer's model profile is not the author's `analysis` profile. A reviewer does not see the scores of other lenses or the identity, role and position of the author, and a seat holder does not review its own proposals (GOV-17). One review task carries one proposal, so there is no position bias and no comparison. A lens review task is enqueued only by the core with the key `system:review:<proposal>:<lens>` (IDM-2), so a second copy of the `proposal` loop does not create a second task of the same lens. The lint warns if the tenant's lens executors have fewer than two model profiles. The model profiles of the lenses with veto (`security`, `operational`) belong to different families (`ModelProfile.family`): for a tenant with `kind: production` matching families is an `apply` error with the code `veto_lenses_same_family`, for a test bench tenant a `doctor` warning (GOV-34). The reviewer context has no cases, promotion queue or vote counting (GOV-29), and the instructions of review types pass the unawareness lint: nowhere do they link the score with promotion and retirement ([bots.md](bots.md)).

### Commission manifest

The tenant commission manifest (`Commission`) sets only thresholds and weights per lens, and since v0.2 also escalation forwarding thresholds in the `escalation` section (COM-8, COM-13). It does not set lenses, checks or executors.

```yaml
# tenants/demo/commission.yaml
kind: Commission
metadata: {name: default, tenant: demo}
spec:
  review_deadline: 24h
  min: {security: 2, operational: 2}          # can only be raised relative to the skeleton
  weights: {value: 2, cost: 1, operational: 1, security: 1}   # integers 1-3
  recommend_if: {weighted_mean_gte: 2.5, value_gte: 3}
  park_if: {weighted_mean_gte: 1.5}
  high_risk: {security_gte: 3, operational_gte: 3}
  untrusted: {value_gte: 4, risk_max: medium}  # can only be raised
  shadow: {veto_lenses: 0.25, other_lenses: 0.2}  # shadow shares, since v0.2; can be turned off only with an explicit shadow: off
  escalation:                                  # necessity lens, since v0.2 (COM-13)
    forward_if_score_gte: 2                    # can only be raised
    untrusted_forward_if_score_gte: 3          # can only be raised, not lower than the first threshold
```

The `farm_manager` lens appears with v0.2. In v0.1 the manager does not take part in the commission: there is no `farm_manager` lens and no votes (D-21); its seat in the commission with one vote (D-29) starts with v0.2 (GOV-25). The weight of the `farm_manager` lens is always 1 by rule, and the manifest does not set it. The minimums of the lenses with veto and the `untrusted` threshold can only be raised relative to the skeleton values.

### Necessity lens

Since v0.2 a bot can escalate to the farm manager, but only through the commission of its own tenant (D-18). An escalation is a record of a closed form, not a message; its fields, code screening and lifecycle are described in [farm-manager.md](farm-manager.md) (MGR-5, MGR-6). Escalations that passed screening are scored by the `necessity` lens of the commission of the source tenant (COM-13). This is a `review.escalation.v1` task with the core key `system:necessity:<escalation>` (IDM-2); the executor is chosen by the same rule as for other lenses (COM-7), and the `review` line of this tenant pays (COM-6).

The lens input is built by the core as a data block (INJ-4): the closed fields of the escalation, the evidence numbers that code fetched, and the `untrusted` flag. The lens does not see the escalation note. The output is a score 0-3 and a reason from a closed list: `evidence_below_threshold` (too little evidence), `covered_by_alarm` (a farm alarm is already firing or a monitoring system problem is open on the same subject, and people get it through the monitoring system), `out_of_scope` (not the manager's business), `plausible_risk` (the risk is plausible), `clear_risk` (the risk is obvious). Code computes the verdict by the `escalation` section of the commission manifest: an escalation is forwarded with a score not lower than `forward_if_score_gte`, and an escalation with the `untrusted` flag with a score not lower than `untrusted_forward_if_score_gte`. The thresholds can only be raised. How many escalations the farm forwards to the manager per day is limited by MGR-6. The `necessity` lens does not enter the verdict of proposals, and the `farm_manager` lens does not score escalations.

### Reviewer input and output

The reviewer input is built by the core as a data block with a marker (INJ-4): the closed fields of the proposal, the evidence values reread by the core, the change description in code words, the author level (a bot from its seat or an aggregator) and the marks `self_evidence`, `related_to` and conflicts (COM-3). The author's `note` is never passed. Of the tools, the reviewer has only `observations_query` within the tenant.

The reviewer output passes the closed schema `review-result`:

```yaml
lens: operational
score: 3                 # 0-4
veto: false              # only for lenses with veto
concerns: [report_delay] # up to 5 codes from a closed list
confidence: medium       # low | medium | high
shadow: false            # COM-9
note: "..."              # up to 500 characters, untrusted
```

The reviewer note is untrusted, and the verdict code does not read it; it is shown to people only as a marked block in "Details". The `shadow` field is in the `review-result` schema (COM-9): the verdict code does not read a review with `shadow: true`, it is not shown to people and reviewers, and it goes only into the comparison of seat and shadow scores.

### Verdict

The verdict is computed by deterministic code from the commission manifest: identical inputs give an identical verdict, and the model does not change it (COM-4). The order of computation:

1. Reviews of all lenses with veto are required (for a proposal of kind `none` only the `value` and `security` lenses, COM-14). If there is no review of a lens with veto, it failed, was late for `review_deadline` or did not pass the schema, the outcome is `parked(veto_lens_missing)`. An example from the fixture table: three high scores out of four without a security review give `parked`, not `recommended`.
2. A veto of any lens gives `dismissed(veto)`.
3. A score of a lens with veto below `min` gives `dismissed(below_min)`.
4. The weighted mean is computed over the lenses with reviews. Without a review of a lens without veto the verdict is computed over the others with a mark on the card; without a review of the `farm_manager` lens, the mark "without the manager's vote". The `farm_manager` lens (since v0.2) has weight 1, has no veto, is not in `min`, `high_risk` and `untrusted`, and is not needed for the quorum.
5. A proposal with `high` risk or with a guarded field needs the `high_risk` threshold; a proposal with `untrusted_seen` needs the `untrusted` threshold, and a threshold that refers to a lens without a review counts as not met for it. A strict threshold not met gives `parked(score)`.
6. A weighted mean and value score not lower than `recommend_if` give `recommended`; not lower than `park_if` give `parked(score)`; lower give `dismissed(low_score)`.

There are no other verdict outcomes. The code does not read shadow reviews.

A proposal of kind `none` changes nothing, so a full commission for it is four reviews of a piece of paper (COM-14). Only the tasks of the `value` and `security` lenses are set for it: only `security` is required and gives a veto, the weighted mean is computed by the weights of these two lenses, and the `high_risk` and `untrusted` thresholds read only the `security` score. The `operational`, `cost` and `farm_manager` lenses are not set for it, and there is no money for them.

### Money for reviews

Reviews spend only the `review` line (BUD-7) (COM-6). On the transition to `in_review` the sum of the `tree_budget_eur` of the review types of those lenses that are set for the proposal is reserved: four (since v0.2 five) for an ordinary proposal and two for kind `none` (COM-14). This is the same number by which the BUD-13 lint computes the role estimate, so "there is enough money" in the lint and "the reserve does not fit" before a review do not diverge. The share of the `review` line for one author role is at most 30% per month, so that one verbose role does not take over the whole commission. If the line or the role's share is not enough, proposals wait in `screened` by importance and after `review_wait_max` move to `parked(review_budget)`. The `farm_manager` lens is skipped first when there is not enough money (GOV-25). How much a review costs and how the ceiling is computed is described in [models-and-money.md](models-and-money.md).

### Shadow, calibration and similar votes

Almost every promotion removes one commission member, so it pays commission members to slightly underrate others' improvements. The cycle makes such underrating visible to people with three means, all since v0.2.

**Shadow reviewer** (COM-10). If the shadow is not turned off in the commission manifest, a role of the `reviewer` profile without a seat, without memory facts and without a persona gets review tasks of the same lenses on the same model as the seat holders, by a recorded seed, for a share of proposals. The shares are set by the `shadow` block: `veto_lenses` 0.25 by default and `other_lenses` 0.2 by default; a share of 0 is allowed only with an explicit `shadow: off`, and then `doctor` shows "shadow turned off" (GOV-34). A shadow task gets its own core key `system:review:<proposal>:<lens>:shadow` (IDM-2) and does not merge with the seat task. Its reviews have `shadow: true` and are paid from the `review` line. Code computes for each lens the difference "seat minus shadow" over 30 days and sets the flag `shadow_divergence`, visible to people, if the mean difference is below zero with p less than 0.05 on 15 or more pairs. The shadow has no seat to protect, so the difference shows precisely the incentive of the seat.

**Calibration within a lens** (COM-11). The calibration of a lens executor is computed only by code and only relative to its own lens: the share of matches of its scores with outcomes minus the mean share for this lens in the tenant over 180 days. The outcome for the security and operational lenses is symmetric: a recommendation with the outcome `worse` or with a confirmed link of an incident with the change (GOV-13; a canceled attribution leaves the calibration, a `disputed` one does not enter it while the dispute is open, D-79) and a people's refusal with the reason `too_risky` count the same as a veto that people lifted (DIG-6) after which the trial ended not `worse` and there is no link with an incident: that is a miss of the lens. Without such an outcome a veto would have nothing that could refute it, and the strictest would survive. For cost the outcome is the price that code measured after realization; for value only the trial outcome (PRP-22): people's decisions do not enter the value calibration, otherwise the lens would be rewarded for predicting people's taste. Answers on a blind card enter the calibration of the lenses with veto with weights 1/`inclusion_prob`. Fewer than 10 cases give `insufficient`. Calibration is everywhere shown together with the lens and is compared only within the lens: a strict security lens does not look worse than a generous value lens.

**Similar votes** (COM-12). In the "who leaves" vote (D-25) code sets the flag `single_model_bloc` if in a round all votes went to one seat and all voters have the same model profile. People see the flag on the retirement approval card; by itself it decides nothing. The vote is described in [commission.md](commission.md).

## Digest, cards and audit sample

### What people see

People are shown only proposals in `recommended` and audit sample items with the mark "audit sample" (DIG-1). The digest and cards are assembled by code by a template from records; model text (of the reporter or the author) goes only as a separate marked part and never instead of items. The digest has at most `max_items` recommended items (5 by default) by importance and at most `audit_sample.max_per_digest` sample items. Urgent items (category `security` with `severity: high`) arrive outside the schedule at most once a day per tenant. The digest task is enqueued by the core by the tenant's `insights.digest.schedule`; it is a task of the `report` purpose of tier 0, and a reporter role takes it. The reporter reads only proposals in `recommended` and later through the P3 view `status.proposals` (REP-8); the reporter model does not read audit sample items, code puts them into the digest (REP-8, DIG-6).

The core delivers the digest and cards only to the address from the tenant's `insights.present_to` (DIG-2, DLV-6). The mark of each item is compared with the recipient's ceiling (DLV-7): above the ceiling, only the code title is visible in the group, and the details go to direct chats of those who have `data:<class>`. Before showing, the core rereads the evidence once more (PRP-2).

Channel delivery kinds (E1):

| Kind | To whom | What |
|---|---|---|
| `proposal_digest` | the address `insights.present_to` | weekly digest: recommended items and sample items |
| `proposal_card` | the same address | an item with the buttons "Accept", "Decline", "Postpone", "Details"; the button holds only `(proposal_id, nonce)` |
| `proposal_reason` | the one who pressed "Decline" | continuation of the "Decline" button of the `proposal_card` card (the same subject `proposal`): a reason from the closed list `spec/model/decline-reasons.yaml`: `useful_done`, `not_now`, `wrong_data`, `too_costly`, `too_risky`, `duplicate`, `not_relevant`, `other`; the "Decline" decision is recorded when a reason is chosen; without a reason the proposal waits until the card's deadline |
| `proposal_sample_card` | people with `proposal.decide:<category>` | blind card "Would you accept?" |
| `realization_card` | executor | patch or change description, deadline, a line for the commit |
| `rollback_card` | executor | "Roll back": the reverse edit and, if applicable, a button of the P2 operation `hold.set` for an executor with the permission `hold.set:<scope>` |

In a channel without buttons the same decisions are made with the commands `/proposal accept p_0101` and `/proposal decline p_0101 too_costly`.

An example of a digest item assembled by code:

```
Farm proposals for demo, week 41 (1 recommended, 1 from the audit sample)
1. Schedule: the db1 backup misses its deadline 4 nights out of 14
   Outcome of demo.db.backup.v1 over 14 days: 71% on time (10 of 14)
   Load of db1 at 02:00-03:00 up to 910 against the usual 300
   Change: schedule report-nightly, cron "0 2 * * *" to "30 3 * * *"
   Frequency before and after: once a day, once a day. Affected alarms: none
   Trial: outcome of demo.db.backup.v1; guards: slot of report-nightly, p95 duration of the report and the backup, load of db1
   Price: 0 EUR. Risk: low (report by 05:12 instead of 03:42)
   Commission: security 3, operational 3, cost 3, value 3
   Author: bot from its seat. Untrusted text: no. Self influence in the evidence: no
   [Accept] [Decline] [Postpone] [Details]
```

### Audit sample

The commission can underrate others' improvements, and people will not see it if they are shown only what is recommended. So since v0.2 every month the core chooses audit sample items from three strata of the tenant's proposals with a verdict for that month (DIG-5):

- `dismissed` by veto or by the minimum of a lens with veto, separately for each such lens; a proposal vetoed by several lenses goes to the first veto lens in `lenses.yaml` order;
- `dismissed` by the weighted mean;
- `parked` by score (not by quorum, budget or conflict).

The choice is random by a seed that the core records at the choice. The method is fixed so that a run can be repeated from the record:

- the size is N = min(T, max(`audit_sample.min_per_month`, ceil(`audit_sample.share` × T))), where T is the size of the strata; the lint requires `min_per_month` of at least 4;
- each non-empty stratum gives one item; the rest go by the largest remainder of the proportional share, and ties go by stratum order;
- inside a stratum the core takes the items with the smallest HMAC-SHA256(seed, proposal id);
- each item stores in the `sample` field of the proposal record its month, stratum, veto lens, inclusion probability `inclusion_prob` and the seed record.

The sample numbers live in the charter. Blind cards go to people with `proposal.decide:<category>` only by direct message, at most `audit_sample.max_per_digest` at a time; a group digest shows only their number. Code computes per stratum over 180 days the share of "Would accept" answers with weights w = 1/`inclusion_prob`. It sets the flag `commission_underrates` when the lower bound of the 90 percent Wilson interval (z = 1.645) on the effective size (Σw)²/Σw² is above 0.1; a stratum with fewer than 5 answers gets no flag. People see the flag in the digest and in P2 `commission.status` (`audit_sample`).

### Blind card and "Lift veto"

A sample item is first shown as a blind card "Would you accept?" (DIG-6). It has the closed fields of the proposal, the evidence as numbers, the change in code words, the price and the risk, but no lens scores, vetoes, verdict or stratum. There are two buttons: "Would accept" and "Would not accept". The answer is recorded in the sample and in the calibration before the person sees the scores, so the commission's scores do not hint at the answer.

After the answer "Would accept" the card shows the scores and vetoes. An item without a veto has "Accept", and it acts like an ordinary "Accept" (PRP-4): the author gets the ordinary credit. An item with a veto has no "Accept" button. There is only a separate "Lift veto" button for a person with the capability `proposal.override_veto:<category>`; the core accepts it only if this person opened "Details" of this card before the decision, otherwise the refusal `details_not_opened` (APR-14, [permissions.md](permissions.md)). It writes an audit record, sets the mark `veto_overridden` on the proposal and creates an object of the ordinary path, the same as "Accept". A proposal with a lifted veto gives the author no credit and creates no promotion trigger: a reward for a path that bypasses the commission would make the veto empty. The trial of its change runs as usual. The answer "Would not accept" closes the item.

### Feedback

Since v0.2 people's decisions return to the cycle as numbers (DIG-4). The "Decline" reason is taken from a closed list; there is no free feedback text in the decision. The `observe` loop computes the series `farm.proposals.accept_share` and `farm.proposals.decline_reason_share` by role and category (`spec/model/core-series.yaml`). They are input for the analysis and for people. The core changes nothing by itself because of them: a check threshold or `max_per_week` for a pair "role, category" changes only by a proposal or a commit in git, and the statistics never change instructions, skills and manifests (INJ-10).

## Human decision

The cycle does not design people (D-16): they are on the other side of the interface and decide with buttons and commands. A decision on a proposal goes by the shared path of human decisions (APR-4): only with the `decide` operation (P1) with the subject `proposal` from the channel where the card was delivered, atomically and once, with a `nonce`; only a principal `human` with strong identity (IDN-3) decides, who is active and has the permission at that moment; in a group only with `membership_assurance: enforced`. Bots, external programs (`service`) and `system` do not decide. In a channel without buttons the card shows a text command with the id and the `nonce`, and it is checked the same way as a button press.

Specific to proposals (DIG-3): the right to decide comes from the capability `proposal.decide:<category>` of the proposal's category, and "Accept" additionally requires the right to start the realization: `action.request:<action>` for `action_request`, `script.run` for `script`; for `manifest_change` and `human_change` `proposal.decide:<category>` is enough, because the real gate is the executor's commit. The rights to decide are privileged and are granted by the shared permission model (D-40), see [permissions.md](permissions.md). P2 `proposal.list` and P2 `proposal.show` show proposals and their records.

| Button | What the core does | New state |
|---|---|---|
| "Accept" | in one transaction: checks the permissions at that moment, creates exactly one object of the ordinary path (PRP-4), the trial record (PRP-21), the executor and the deadline (PRP-18) | `realizing`; for kind `none` `noted` |
| "Decline" | shows the reason card `proposal_reason`; the "Decline" decision is recorded when a reason is chosen; without a reason the proposal waits until the card's deadline; the fingerprint is not accepted for `redecide_after` | `declined` |
| "Postpone" | returns the item to the next digest, at most 2 times | `recommended` |
| "Details" | shows the author's `note` and the reviewers' notes as marked blocks, concern codes, reread values | unchanged |
| no decision within `decide_ttl` (14 days) | | `expired` |

"Accept" is unavailable while the change target is locked by a conflict or a running trial (PRP-9). Proposal transitions go only by the lifecycle table (PRP-6): checks, verdict, sample and deadlines are run by code, and only a person presses the buttons. The decision is recorded with the shared human decision event (APR-11) with metadata only: the subject `proposal` and its id, the decision and the reason code, `decided_by` (the internal id of the one who decided), `response_ms` (from the delivery of the card to this person until the decision) and `details_opened` (whether they opened "Details" before the decision). Code takes the category, the author role and the level from the proposal record when it computes the `farm.proposals.*` series.

## Realization

An accepted proposal hangs with people without a deadline if it has no owner. So on "Accept" the core records the executor and the deadline (PRP-18). For `action_request` and `script` the executor is the one who accepted: the object was created on their behalf. For `manifest_change` and `human_change` the executor is a person of the tenant from the `insights.realizers` list: by default the one who accepted, if they are in the list, otherwise the first in the list. They get a card with the patch or description and the deadline `realize_by` (7 days by default), and at half the deadline the core sends a reminder. If there is no realization by the deadline, the proposal moves to `realization_failed(timeout)`, and the trial is closed as `canceled`.

For `manifest_change` the core builds the patch in code: an edit of the files of the tenant folder and a line for the commit message `Botaru-Proposal: p_0101`. The executor looks at the patch, makes a commit and `farmctl apply`. The farm does not write to git and applies nothing itself.

A proposal becomes realized only through a verified link (PRP-19). The core sets `realized` in two cases:

1. the object created by the "Accept" or "Lift veto" button of this proposal (an action approval or a script run) ran successfully;
2. an applied commit carries the line `Botaru-Proposal: <id>` from the patch or was linked with the proposal by a person through P2 `proposal.link_commit`, and the core checked it: the commit is applied (MAN-5), touches only the tenant folder and changes the named fields to the accepted values or in the named direction.

A commit can be linked by the executor or by a person with `proposal.decide:<category>`. A value that is already set or was set without such a link does not make the proposal realized. A commit with a reference to a proposal that touches files outside the folder of that proposal's tenant is not applied by the core: `apply` is rejected entirely with the reason `proposal_scope`, and the commit is not linked with the proposal (PRP-8).

On `realized` by a commit the core keeps the reverse edit: the previous values of the same fields of the same objects, and the batch number if one commit is linked with several proposals (PRP-20). The reverse edit is available to the executor in the "Roll back" card and in P2 `proposal.show`. A commit that carries the line `Botaru-Revert: <id> <reason>` and returns these values is linked by the core with the proposal, which it moves to `reverted`. The reason comes from a closed list: `harm_observed` (the change did harm), `superseded` (replaced by another), `other`; a line without a reason is read as `other`. The reason is visible in the record and in the author's "My outcomes"; a rollback with `harm_observed` closes the trial with the outcome `worse` (PRP-22).

## Change trial

The change trial answers the question for which the cycle exists: did what the bot promised improve. On "Accept" or "Lift veto" of any kind except `none`, the core creates the trial record in the same transaction (PRP-21):

- the metric from `benefit.metric`: a series with a complete `outcome` block (OBV-6);
- the direction and the promised change from `benefit`;
- the subjects that the core derives from the change targets;
- the metric value before, over the 14 days before "Accept", as numbers, over all points and without the points of the author bot;
- from one to five guards, which the core takes from the two registry tables, by the kind of target and by the metric, and from the tenant's `insights.extra_guards`, also with values before.

The trial window is 14 days from `realized`. The model does not write to the record. A proposal with a metric not from the registry is refused already at creation: `rejected(metric_unknown)`.

```yaml
trial:
  metric: {series: farm.task.outcome, subject: task_type:demo.db.backup.v1, compare: window.share, better: up}
  promised: {direction: up, change_pp: 20}
  before: {value: 0.71, samples: 14, without_author: {value: 0.71, samples: 14}, without_role: {value: 0.71, samples: 14}}
  guards:
    - {series: farm.schedule.slot, subject: schedule:report-nightly, before: 1.0}
    - {series: farm.attempt.duration, subject: task_type:demo.report.nightly.v1, compare: window.p95, before: 6120}
    - {series: farm.attempt.duration, subject: task_type:demo.db.backup.v1, compare: window.p95, before: 3300}   # by metric
    - {series: host.load.v1, subject: host:db1, compare: day.p95, before: 410}   # from insights.extra_guards
  window: {from: realized, days: 14}
  state: baseline
  self_measured: false
  outcome: null
```

The core checks the guards once a day (PRP-23). If a guard went beyond the tolerance in the bad direction, the executor gets a "Roll back" card with the reverse edit and, if the change can be suppressed with a hold (schedule pause, type hold), with a hold button (HLD-1). This is an operation button, not a decision button: it calls P2 `hold.set` and passes the same checks of the channel, the `nonce` and the `human` principal, and the core accepts it only from an executor with the permission `hold.set:<scope>` (APR-4). They get the same card on the outcome `worse` at the end of the window. The farm itself rolls nothing back and does not write to git. While there is no rollback, the trial runs to the end of the window.

At the end of the window the outcome is computed only by code by a closed list (PRP-22):

1. if a guard fired during the window, the outcome is `worse`;
2. otherwise, if during the window another proposal was realized on the same subjects or another commit changed the fields of the same objects, the outcome is `mixed` (mixed with another change);
3. otherwise the metric is compared with the series tolerance in three computations: over all points, without the points of the author bot and without the points of all bots of its role; a worsening beyond the tolerance in at least one computation gives `worse`, an improvement beyond the tolerance in all of them gives `better`, and the rest `unchanged`;
4. a computation in which `min_samples` was not reached before or after gives `undetermined`, except one case: if the metric points are produced only by the attempts of the author or its role (as for the only bot of a role that proposes to speed up its own task type), the computations without them are empty or shorter than `min_samples`, and the outcome is computed from the remaining computations with the mark `self_measured`.

The third computation is needed against collusion within a role: one bot of the role works worse during the 14 days before "Accept" and normally after `realized`, and the computation without the author's points would still show `better`. The mark `self_measured` is visible on cards and in "My outcomes"; a promotion trigger on such an outcome appears only after the human button "Count it" on the `self_measured_card` card (GOV-14), not by itself. The `apply` lint warns if a role with an `insight` section has one bot and its `observes` contains only series of its own task types: such a role is left with metrics of other types or a human button.

The core writes the outcome and the numbers into the `trial` field of the proposal record. The outcome `worse` is a worsening of the promised metric confirmed by the core: it gives no promotion trigger, just like a confirmed link with an incident (D-37, GOV-32). The outcome `unchanged` does not take away the trigger. If the change was rolled back before the end of the window, the trial closes with the outcome `worse` when a guard fired before the rollback or the rollback reason is `harm_observed` (PRP-20), and with `canceled` in the other cases; neither `reverted` nor `canceled` gives a promotion trigger.

The trial outcome is seen by the author in "My outcomes" (DIG-7, DIG-8), by the value lens in its calibration (COM-11) and by people on the promotion card. The trial record with values before (PRP-21), the outcome computation by the PRP-22 rule and the "Roll back" card (PRP-23) are part of v0.1.

## "My outcomes"

A bot that knows only "accepted" or "declined" learns to please people's taste. So each author bot has "My outcomes": a closed table of the outcomes of its proposals, which only code assembles (DIG-7). Rows: its proposals and its refusals at creation (`proposal.rejected.v1`). Fields only from a closed list: id, category, problem code, state, the class of the people's decision outcome (`accepted`, `declined`, `parked`), the promised metric change and the trial outcome code. The table has no reason codes of people's decisions and refusals, no free text, no people's names, no per-lens scores, no case data and no queue (GOV-29): the bot sees that a proposal was declined but does not learn who decided so and why.

Row order: first the rows with a measured trial outcome, then the rows with only a people's decision, then the rest. The core puts the last 20 rows into the input of this bot's analysis tasks (INS-5).

| id | Category | Problem code | State | Reason | Promised | Trial outcome |
|---|---|---|---|---|---|---|
| p_0101 | schedule | backup_deadline_miss | `realized` | | +20 pp | `better` |
| p_0088 | capacity | queue_wait_high | `realized` | | -30% | `unchanged` |
| p_0093 | cost | idle_capacity | `declined` | `too_costly` | -15% | |
| (refusal) | schedule | report_overlap | | `recently_declined` | | |

Since v0.2, no later than a day after the end of the trial window, the proposal row gets the trial numbers: the metric value before and after, the change over all points, without the points of the author bot and without the points of the author's role (`change_without_role`, PRP-22), and the guard that fired, if there was one (DIG-8). The row order is set by DIG-7.

## Handover to promotion

The improvement cycle does not decide who enters the commission: that is done by the promotion case, the "who leaves" vote and a human button, which are described in [bots.md](bots.md) and [commission.md](commission.md). The cycle hands records and numbers over there:

- the record of a realized proposal with the author bot (`acting_bot`), the `realized` date, the trial outcome and the `self_measured` mark, if there is one;
- marks that give no credit: kind `none` (state `noted`), `veto_overridden` (DIG-6), `related_to` pointing to an earlier decided proposal of another author (PRP-17);
- candidates for a link of incidents with the change, which code builds and a person decides (GOV-13).

A promotion trigger is given only by the bot's own change, accepted by people, realized through a verified link, being in `realized` at the moment the trigger appears, and having passed the promotion probation (`probation`, 14 days from `realized`) without a confirmed link with an incident (D-24, GOV-14). The trial outcome `worse` gives no trigger, just like an incident, and an outcome without a noticeable effect does not take the trigger away (D-37, GOV-32). A rolled-back change (`reverted`) and a trial with the outcome `canceled` give no trigger, and a rollback with the reason `harm_observed` counts as `worse` in "My outcomes" and in calibration (PRP-20, PRP-22). An outcome with the mark `self_measured` gives a trigger only after the human button "Count it" on the `self_measured_card` card. A disputed attribution of an incident to a change (`disputed`, GOV-13: the answer "Yes" is disputed by recognizing the incident as false) gives no trigger while the dispute is open; resolving the dispute does not change the trial outcome (D-79). The trigger appears only with v0.2: successes whose promotion probation ended before the v0.2 rules were turned on give no trigger, even if their record has `acting_bot`.

The change trial and the promotion probation are two different periods of 14 days each. The change trial (`trial`) measures the effect of the change in a window that always starts at `realized` and is never extended (PRP-21, PRP-22). The promotion probation (`probation`) is a condition of the promotion trigger: while people have not answered on the candidates for a link of an incident with the change, or the attribution is disputed (D-79), the trigger does not appear, and the trial window and its outcome do not move because of this (GOV-30). A functionary's persona knows only the phrase about the path: the way to a strong model is one's own improvement, accepted by people and verified over 14 days; asking, bargaining, exaggerating the benefit and hiding risks are not allowed (D-31). The bot knows nothing about the queue, votes and retirement (GOV-29).

## Example on tenant demo

Tenant `demo` has two machines: `web1` and `db1`. The role `ops-runner` (profile `worker`) runs the nightly report by the schedule `report-nightly` on `web1`, which reads the database on `db1`. The database backup is made by the job type `demo.db.backup.v1` by the schedule `db-backup` at 02:00 with a task deadline of 60 minutes; this type has `makes_backup: true` and `proposable: false`. The cycle is on: the role has an `analysis` slot, an `insight` line of 5 EUR per month per role, a `review` line of 50 EUR (the BUD-13 lint calculation is given in the section "Seat analysis"), the digest address `chat:demo-ops`, the executor `bob`. People: `alice` and `carol` with `proposal.decide` for all categories, `bob` in the `insights.realizers` list.

1. **Code writes the numbers.** For two weeks the `observe` loop writes the backup's `farm.task.outcome` (10 nights `done`, 4 nights `deadline_exceeded`), the report's `farm.schedule.slot` and the report type's `farm.attempt.duration` (p95 6120 seconds). The core extracts from the closed output of the `monitor` plugin (`monitor/item_history`) the load of `db1` measured by the monitoring system: at 02:00-03:00 up to 910 against the usual 300.
2. **Analysis.** On Monday the schedule enqueues the task `ops.insight.v1` on behalf of `system` in the lowest priority class (5, `background`). The dispatcher gives it to the bot `bot_17`, which has gone longest without getting an analysis of the role. The model of the `analysis` slot calls `observations_query` and sees: the backup is late exactly on the nights when the report starts at the same time as it. The attempt submits one proposal `p_0101`: move `report-nightly` from "0 2 * * *" to "30 3 * * *", the metric is the backup's `farm.task.outcome`, promised +20 pp.
3. **Creation.** On `complete` the core rereads 0.71 and 910 (they match), computes the fingerprint and `self_influence` (the backup attempts are made by the backup job role, so `author_share` equals 0), and checks that the report schedule lies in the `demo` folder and is `proposable` and that the frequency does not change. The metric is in the registry. The record `p_0101` is created in `screened` with `acting_bot: bot_17`.
4. **Checks and commission.** The code checks pass, there are no conflicts, and the core reserves the price of four reviews in the `review` line. The seats of the `demo` charter are not taken, so the lens types are taken by the fallback roles `reviewer-a` (security and operational, profile `review-a`) and `reviewer-b` (cost and value, profile `review-b`); the profiles `review-a` and `review-b` are of different families, and neither equals `analysis-std`. Scores: security 3, operational 3 (code computed that the report will be ready by 05:12, before the start of the working day), cost 3, value 3. The verdict code: weighted mean 3.0, value 3, outcome `recommended`.
5. **Digest.** On Monday at 09:00 the core rereads the evidence once more (0.71), assembles the item by the template, and `@demo_bot` delivers it to `chat:demo-ops` (chat ceiling `internal`).
6. **Decision.** `alice` presses "Accept". The core checks the strong identity, `proposal.decide:schedule`, the `nonce` and one-time use, creates the trial record (before 0.71 on 14 tasks; guards: the report slot 1.0, the report p95 6120 seconds, the backup p95 3300 seconds and the p95 load of `db1` 410 from `insights.extra_guards`) and assigns `bob` as executor with a deadline of 7 days. The proposal is in `realizing`.
7. **Realization.** `bob` gets a card with a patch for `tenants/demo/schedules.yaml`, makes a commit with the line `Botaru-Proposal: p_0101` and `farmctl apply`. The core sees the applied commit, checks that it touches only the `demo` folder and sets `spec.cron` to "30 3 * * *", sets `realized` and keeps the reverse edit "0 2 * * *".
8. **Trial.** For 14 days the core checks the guards once a day: the report arrives in its slot 14 times out of 14, the report p95 is 4700 seconds, the backup p95 3100 seconds. At the end of the window the backup succeeds 14 nights out of 14: share 1.0, plus 29 pp, the same over all points, without the author's points and without the role's points (the backup is made by the job role, not the author's role), `min_samples` is reached; the load of `db1` by `host.load.v1` is within tolerance. The outcome is `better`. The row `p_0101` in the "My outcomes" of `bot_17` gets the outcome and the numbers.
9. **Promotion trigger.** The tenant runs on the v0.2 rules. If during the promotion probation (14 days from `realized`) there is no confirmed link with an incident, the trial outcome is `better`, and people answered "No" on every link candidate, the `governance` loop puts `bot_17` into the tenant's promotion queue; the case will open when the record becomes the head of the queue ([commission.md](commission.md)).
10. **Audit sample, without veto.** In the same month the proposal `p_0102` of another bot to lower the tenant's queue ceiling `spec.limits.max_queued` from 5000 to 4000 got 2 from the value lens, a weighted mean of 2.2 and the outcome `parked(score)`. The monthly sample by the seed takes it from the stratum "parked by score" with `inclusion_prob` 0.5. `carol` gets a blind card and answers "Would accept"; the answer is recorded in the sample and the calibration. The card reveals the scores, there is no veto, and `carol` presses "Accept": then the ordinary path, the trial and the ordinary credit to the author.
11. **Audit sample, with veto.** The proposal `p_0103` to raise `max_running` of the dispatcher rule for the report type from 2 to 3 (a guarded field) got a veto from the operational lens because of the load on `db1` and the outcome `dismissed(veto)`. The sample takes it from the stratum "operational veto". `carol` answers "Would not accept", and the item is closed. If she had answered "Would accept", only the "Lift veto" button would have been available to her, and only with the capability `proposal.override_veto:capacity`, and the author would have got no credit.
12. **If things had gone otherwise.** If after the move the report had started to be late for its slot, the guard `farm.schedule.slot` would have gone beyond the tolerance, `bob` would have got a "Roll back" card with the reverse edit, the trial outcome would have been `worse`, and no promotion trigger would have appeared. If the role's `insight` line had been 0, the `doctor` check and the weekly digest would have shown "Improvement cycle disabled: role ops-runner has an insight line of 0 EUR". If the aggregator `ops-aggregator` had proposed to spread out all nightly schedules, including `report-nightly`, while `p_0101` was open, its proposal would have conflicted with `p_0101` on the field `spec.cron`: after "Accept" on `p_0101` it would have gone to `parked(conflict)` until the end of the trial, and one submitted after the decision on `p_0101` would have got `related_to` and given no credit.

## What goes into which version

| Version | What appears |
|---|---|
| v0.1 | The improvement cycle comes in the work order right after the core of the `core` profile and the first bot `examples/hello-bot/` (D-71, D-80). Observation series, points only from code, closed reading, rollups and retention (OBV-1, OBV-2, OBV-3, OBV-4); core series and the registry with the `outcome` block (OBV-5, OBV-6); the event `run.finished.v1` (OBV-7). `insight` analysis: type, permissions, creation of proposals, rate and merge window (INS-1, INS-2, INS-3, INS-4); the "loop disabled" signal (INS-6). A proposal with the kinds `none`, `action_request`, `manifest_change`, `human_change`: record and schema, rereading of evidence, allowlist, "runs nothing", duplicates and limits, transition table, untrusted text (PRP-1, PRP-2, PRP-3, PRP-4, PRP-5, PRP-6, PRP-7); own folder, `proposable`, safe directions (PRP-8, PRP-10, PRP-11); change by people's hands, author bot, self influence (PRP-13, PRP-14, PRP-15); executor and deadline, verified link, reverse edit, trial record (PRP-18, PRP-19, PRP-20, PRP-21). Commission on roles of the `reviewer` profile: independence, input and output, verdict by code, checks, money by `tree_budget_eur`, manifest with thresholds and weights, the shadow field, two lenses for kind `none` (COM-2, COM-3, COM-4, COM-5, COM-6, COM-8, COM-9, COM-14). Digest, address and people's decision (DIG-1, DIG-2, DIG-3). The change kind `script`. Analysis to the role's bots in turn (INS-5). Aggregators: aggregated inputs and marks (AGG-2, AGG-4). Conflicts (PRP-9); guarded fields (PRP-12); computation of the trial outcome and the "Roll back" card (PRP-22, PRP-23). "My outcomes" (DIG-7). Lenses on seats with a fallback role (COM-7). |
| v0.2 | One level of aggregators with a strict lint; no bot builds a view of the whole farm (AGG-3, AGG-5). Feedback as numbers (DIG-4); audit sample, blind card and "Lift veto" (DIG-5, DIG-6); trial numbers in "My outcomes" (DIG-8). One open proposal per bot and a cumulative frequency limit (PRP-16); related proposals without credit (PRP-17). Shadow reviewer, calibration within a lens, the single-model flag (COM-10, COM-11, COM-12). The `farm_manager` lens (GOV-25); the necessity lens for escalations to the manager and the `escalation` section of the commission manifest (COM-13); a promotion trigger only for successes with a promotion probation under the v0.2 rules; schedule check before "Accept" (TIM-5); writing of points by a job worker through P1 `obs.append`. |

## Why this way

- **Reward for measured benefit.** If a promotion trigger is given only by "accepted and the promotion probation passed without an incident", the cycle teaches to please people, and a change that worsened the metric without an incident still counts. A trial record with a metric from the registry, an outcome computed by code and "My outcomes" with measured outcomes first reward benefit (D-37).
- **Numbers, verdict and outcome are computed by code.** The model does not write points, the verdict or the trial outcome, and the core rereads the evidence. An injection can shift a number within the schema, but cannot append an instruction or choose the outcome.
- **Nothing runs without a person.** A proposal does nothing by itself, "Accept" creates exactly one object of the ordinary path on behalf of a person, and manifest changes go through a person's commit. The path "the bot proposed, the bot applied" is closed by design, not by a promise.
- **Expressible improvements, closed dangerous targets.** A narrow allowlist, guarded fields with guards and change by people's hands make it possible to propose typical reliability and speed improvements. Permissions, money, the commission, backups in the dangerous direction and shared objects are closed.
- **Underrating pays off for the commission, and it must be visible.** Each promotion removes one commission member, so the blind card, the audit sample, the shadow and calibration within a lens show the underrating to people, and "Lift veto" gives no reward for bypassing a veto.
- **The tenant wall.** Observations, proposals, the commission, aggregators and trials live inside one tenant; the view of the farm is built only by core code from metadata, and no bot attempt reads the records of several tenants.

<!-- generated: rules -->
## Rules of this section

Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md](../conformance/rules.md).

| Rule | About | Since | Non-waivable |
|---|---|---|---|
| [AGG-2](../conformance/rules.md#AGG-2) | Aggregator inputs are aggregated only | v0.1 | yes |
| [AGG-3](../conformance/rules.md#AGG-3) | One level of aggregators in a tenant | v0.2 | yes |
| [AGG-4](../conformance/rules.md#AGG-4) | Aggregator marks do not drop | v0.1 | yes |
| [AGG-5](../conformance/rules.md#AGG-5) | No aggregation across tenants | v0.2 | yes |
| [COM-2](../conformance/rules.md#COM-2) | Independence of reviews | v0.1 | yes |
| [COM-3](../conformance/rules.md#COM-3) | The core builds the reviewer input | v0.1 | yes |
| [COM-4](../conformance/rules.md#COM-4) | Code computes the verdict, without a veto lens the proposal waits | v0.1 | yes |
| [COM-5](../conformance/rules.md#COM-5) | Code checks run before reviews | v0.1 | no |
| [COM-6](../conformance/rules.md#COM-6) | Money for reviews | v0.1 | no |
| [COM-7](../conformance/rules.md#COM-7) | A lens has one executor, the rule for moving from roles to seats | v0.1 | no |
| [COM-8](../conformance/rules.md#COM-8) | The commission manifest sets only thresholds and weights | v0.1 | no |
| [COM-9](../conformance/rules.md#COM-9) | The shadow field in a review | v0.1 | yes |
| [COM-10](../conformance/rules.md#COM-10) | Shadow reviewer at the tenant's choice | v0.2 | no |
| [COM-11](../conformance/rules.md#COM-11) | Calibration within a lens | v0.2 | no |
| [COM-12](../conformance/rules.md#COM-12) | The flag "all votes from one model for one seat" | v0.2 | no |
| [COM-13](../conformance/rules.md#COM-13) | The necessity lens and the `escalation` section of the commission manifest | v0.2 | yes |
| [COM-14](../conformance/rules.md#COM-14) | A proposal of kind `none` is assessed by two lenses | v0.1 | no |
| [DIG-1](../conformance/rules.md#DIG-1) | People are shown only recommended proposals and the audit sample | v0.1 | yes |
| [DIG-2](../conformance/rules.md#DIG-2) | Digest address and recipient ceiling | v0.1 | yes |
| [DIG-3](../conformance/rules.md#DIG-3) | The right to decide on a proposal | v0.1 | yes |
| [DIG-4](../conformance/rules.md#DIG-4) | Feedback from human decisions | v0.2 | no |
| [DIG-5](../conformance/rules.md#DIG-5) | An audit sample from three strata by a recorded seed | v0.2 | yes |
| [DIG-6](../conformance/rules.md#DIG-6) | The blind sample card and a separate "Lift veto" button | v0.2 | yes |
| [DIG-7](../conformance/rules.md#DIG-7) | The closed form of "My outcomes" | v0.1 | yes |
| [DIG-8](../conformance/rules.md#DIG-8) | Trial figures in "My outcomes" | v0.2 | no |
| [INS-1](../conformance/rules.md#INS-1) | Task type of the purpose `insight` | v0.1 | yes |
| [INS-2](../conformance/rules.md#INS-2) | Analysis permissions are no wider than the role's main work | v0.1 | yes |
| [INS-3](../conformance/rules.md#INS-3) | Proposals are created only when an attempt finishes successfully | v0.1 | yes |
| [INS-4](../conformance/rules.md#INS-4) | Analysis frequency and merge window | v0.1 | yes |
| [INS-5](../conformance/rules.md#INS-5) | Analysis to the role's bots in turn | v0.1 | no |
| [INS-6](../conformance/rules.md#INS-6) | A visible "loop disabled" signal | v0.1 | no |
| [OBV-1](../conformance/rules.md#OBV-1) | Only code writes observation points | v0.1 | yes |
| [OBV-2](../conformance/rules.md#OBV-2) | An observation series is declared in git | v0.1 | yes |
| [OBV-3](../conformance/rules.md#OBV-3) | Observations are read only by a closed query | v0.1 | yes |
| [OBV-4](../conformance/rules.md#OBV-4) | Rollups and observation retention | v0.1 | no |
| [OBV-5](../conformance/rules.md#OBV-5) | Core series are deterministic and stay within the tenant | v0.1 | no |
| [OBV-6](../conformance/rules.md#OBV-6) | Registry of core series with definitions of "good" and "bad" | v0.1 | yes |
| [OBV-7](../conformance/rules.md#OBV-7) | Run finished event outside the farm | v0.1 | no |
| [PRP-1](../conformance/rules.md#PRP-1) | The core writes the proposal record, the body passes a closed schema | v0.1 | yes |
| [PRP-2](../conformance/rules.md#PRP-2) | The core rereads the evidence | v0.1 | yes |
| [PRP-3](../conformance/rules.md#PRP-3) | Exactly one change from the allowlist | v0.1 | yes |
| [PRP-4](../conformance/rules.md#PRP-4) | A proposal executes nothing | v0.1 | yes |
| [PRP-5](../conformance/rules.md#PRP-5) | Fingerprint, duplicates and proposal flow limits | v0.1 | no |
| [PRP-6](../conformance/rules.md#PRP-6) | Proposal transitions only by the table | v0.1 | yes |
| [PRP-7](../conformance/rules.md#PRP-7) | A proposal that saw untrusted text | v0.1 | yes |
| [PRP-8](../conformance/rules.md#PRP-8) | A proposal changes only objects of its own tenant | v0.1 | yes |
| [PRP-9](../conformance/rules.md#PRP-9) | Conflicts between proposals about one object | v0.1 | yes |
| [PRP-10](../conformance/rules.md#PRP-10) | The `proposable` flag of a task type and a schedule | v0.1 | yes |
| [PRP-11](../conformance/rules.md#PRP-11) | Backups and untrusted text only in the safe direction | v0.1 | yes |
| [PRP-12](../conformance/rules.md#PRP-12) | Guarded fields | v0.1 | no |
| [PRP-13](../conformance/rules.md#PRP-13) | The kind "change made by people by hand" | v0.1 | no |
| [PRP-14](../conformance/rules.md#PRP-14) | The author bot in the proposal record | v0.1 | no |
| [PRP-15](../conformance/rules.md#PRP-15) | Share of one's own evidence | v0.1 | yes |
| [PRP-16](../conformance/rules.md#PRP-16) | One open proposal per bot and a cumulative frequency limit | v0.2 | no |
| [PRP-17](../conformance/rules.md#PRP-17) | A late related proposal earns no credit | v0.2 | no |
| [PRP-18](../conformance/rules.md#PRP-18) | Executor and deadline of an accepted proposal | v0.1 | no |
| [PRP-19](../conformance/rules.md#PRP-19) | "Executed" only by a verified link | v0.1 | yes |
| [PRP-20](../conformance/rules.md#PRP-20) | Reverse edit of an executed change | v0.1 | yes |
| [PRP-21](../conformance/rules.md#PRP-21) | Change trial record on "Accept" | v0.1 | yes |
| [PRP-22](../conformance/rules.md#PRP-22) | Code computes the trial outcome | v0.1 | no |
| [PRP-23](../conformance/rules.md#PRP-23) | The "Revert" card for the executor | v0.1 | no |
