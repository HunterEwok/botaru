# How the farm gets better

## In short

The farm gets better because its bots propose changes to their own work and only measured benefit counts (D-16). The core writes the numbers. A functionary looks at them in an analysis task and proposes at most three changes, each with exactly one change from an allowlist. Code checks the proposal, a blind commission of bots of the same tenant scores it through lenses (D-23), and code computes the verdict. People see only recommended proposals and a random audit sample of rejected ones, and only a person presses "Accept". The change then goes the farm's ordinary way: an action approval, a script run or a person's commit. For 14 days the core measures whether the promised metric improved, with guards that must not get worse. The outcome reaches the author in "My outcomes", the commission in its accuracy and people on the promotion card.

A functionary whose own accepted change was realized, did not make the metric worse and lived through 14 days without a confirmed link to an incident enters its tenant's promotion queue (D-24, D-37). The head of the queue gets a case; if the commission is full, one member leaves. Code proposes who leaves by the accuracy of forecasts, and while there are not enough scored forecasts a person chooses on the card (D-115). Nothing takes effect without a person's button, and no bot knows about the queue, cases or departures (GOV-29). From v0.3 the commission also answers for its outcomes, first in shadow (D-109).

## How it works

### The path of one change

| Step | Who does it | What comes out |
|---|---|---|
| Observations | the core, job workers, the core from tool output | points of numeric series |
| Analysis | a functionary on the `analysis` model slot, or an aggregator | up to three proposals or `nothing_found` |
| Proposal | the core makes a record from the body the bot submitted | a record in `screened` |
| Code checks and commission | code, then the lenses; code computes the verdict | `recommended`, `parked` or `dismissed` |
| Digest and cards | the core assembles, a reporter delivers | items for people and an audit sample |
| Decision | a person with the permission | "Accept", "Decline", "Postpone" |
| Realization | the executor, or the object of the ordinary path | `realized` through a verified link |
| Change trial | the core, 14 days | `better`, `unchanged`, `worse`, `undetermined`, `mixed` |
| Promotion | the `governance` loop, then a person | a seat on the commission |

On this path a model writes only the proposal body, the lens scores with their forecasts and the reporter's optional introduction. The numbers, checks, verdict, digest, realization, trial outcome and case are made by code and people. The transitions are tables in [proposal.md](../spec/lifecycles/proposal.md), [trial.md](../spec/lifecycles/trial.md), [attribution.md](../spec/lifecycles/attribution.md), [promotion-entry.md](../spec/lifecycles/promotion-entry.md) and [case.md](../spec/lifecycles/case.md).

### Observations

An observation is a number or a value from a closed list that a bot sees from its seat: attempt duration, queue wait, machine load by hour, the age of the last backup. Analysis, evidence and the change trial stand on these series; people rarely read them directly.

A tenant declares a series in git as `ObservationSeries` ([OBV-2](../conformance/rules.md#OBV-2)). The `outcome` block is needed only if the series is to be a trial metric or guard.

```yaml
# tenants/demo/observations/host.load.v1.yaml
kind: ObservationSeries
metadata: {name: host.load.v1, tenant: demo}
spec:
  catalog_kind: host
  value: {type: number, minimum: 0, maximum: 10000}
  unit: load_x100
  data_class: internal                   # not lower than the tool's output_class
  source: {kind: tool, tool: monitor/item_history, field: cpu_load}   # monitor plugin (MON-6)
  resolution: 5m
  rollups: [hour.max, hour.mean, day.p95] # from spec/model/rollups.yaml
  retention: {raw: 14d, rollup: 93d}
  outcome: {compare: day.p95, better: down, tolerance_pct: 10, min_samples: 10}
```

Only code writes points ([OBV-1](../conformance/rules.md#OBV-1)). There are three sources. The core loop `observe` computes `core` series from records of the same tenant ([OBV-5](../conformance/rules.md#OBV-5)). A job worker can write into the series of its task type through P1 `obs.append` (OBV-8, deferred). And the core extracts a closed field from tool output, as with `host.load.v1`: the monitoring system measures the load, and the `monitor` plugin only reads it. An agent attempt has no operation for writing a point, so the data people later decide on cannot be poisoned by model text.

Reading goes only through a closed query ([OBV-3](../conformance/rules.md#OBV-3)): the built-in tool `observations_query` for bots, P1 `obs.query` for a job worker without a model, P2 `obs.series` for people. The answer holds only numbers and `enum` values, so reading does not set `untrusted_seen`: an instruction cannot hide in a number. A number can still be distorted by the world it measures, which is why numbers alone never decide. Rollups are computed by code and kept by the retention class `observation`, 14 days for points and 93 days for rollups ([OBV-4](../conformance/rules.md#OBV-4)).

**What "better" means.** The skeleton keeps the registry `spec/model/core-series.yaml`: `farm.attempt.duration`, `farm.task.outcome`, `farm.task.attempts`, `farm.queue.wait`, `farm.task.cost`, `farm.schedule.slot`, `farm.backup.age` and the `farm.external.*` series, each with an `outcome` block: the rollup to compare, the good direction, the tolerance inside which nothing changed, and `min_samples` ([OBV-6](../conformance/rules.md#OBV-6)). Without one shared definition of "slow" and "reliable" a bot either stays silent or draws conclusions from noise, and a trial has nothing to measure with.

**Guards.** A guard is a series that must not get worse while the benefit is measured. The core takes one to five of them from two tables of the registry ([PRP-21](../conformance/rules.md#PRP-21)). The first goes by the kind of change target:

| Change target | Guards |
|---|---|
| a schedule | `farm.schedule.slot` of the schedule, `farm.attempt.duration` of its type |
| a backup schedule window | `farm.backup.age` of the dataset (required), `farm.schedule.slot` |
| a tenant dispatcher rule | `farm.task.outcome` and `farm.queue.wait` of the type, `farm.task.cost` |
| a tenant limit | `farm.queue.wait` and `farm.task.outcome` of the affected types |
| a dataset's backup reference | `farm.backup.age` of the dataset |
| `action_request`, `script`, `human_change` | `farm.task.outcome` of the affected types |

The second goes by the promised metric. Without it a bot could double the attempt deadline, raise the share of `done` and deliver the report after the working day starts.

| Trial metric | Guards |
|---|---|
| `farm.task.outcome` | `farm.attempt.duration` of the same type |
| `farm.attempt.duration` | `farm.task.outcome` of the same type |
| `farm.task.attempts` | `farm.attempt.duration` and `farm.task.cost` |
| `farm.queue.wait` | `farm.task.cost` and the load series of the subjects, if declared with an `outcome` block |
| `farm.task.cost` | `farm.task.outcome` and `farm.attempt.duration` |
| `farm.schedule.slot` | `farm.attempt.duration` of the schedule's type |
| `farm.backup.age` | `farm.schedule.slot` of the backup schedule |
| `farm.external.*` | the other `farm.external.*` series of the type |

A tenant can only add guards, with `insights.extra_guards`. From v0.3 every trial also carries two farm guards that a tenant cannot remove: the tenant's share of successful tasks and the 95th percentile of queue wait ([PRP-26](../conformance/rules.md#PRP-26)).

**Work that ends outside the farm.** An external pipeline or a backup outside the manifests reports its end with the event `run.finished.v1`, submitted by a job worker through P1 `emit` ([OBV-7](../conformance/rules.md#OBV-7)). The core writes the `farm.external.*` points from it.

```yaml
type: run.finished.v1
data:
  run_id: r_7f3a
  approval_id: 4813       # optional: the approval that started the run
  subject: ctd-stations  # id from its own tenant's catalog
  duration_s: 2710
  retries: 1
  exit_code: 0
  outcome: done          # done | failed | timeout | canceled
```

### Analysis

Seat analysis is a task type of the `insight` purpose of a functional role; only roles of the `worker` and `manager` profiles declare it (PRF-1).

```yaml
# roles/ops-runner/role.yaml (fragment)
spec:
  profile: worker
  models: {base: base-std, analysis: analysis-std}   # analysis only for insight and aggregate
  insight:
    types: [ops.insight.v1]
    observes: [farm.task.outcome, farm.schedule.slot, farm.attempt.duration, farm.backup.age, host.load.v1]
    tools: [monitor/item_history]   # tier at most 1, numbers only
    max_data: internal
    max_runs: {per: week, count: 2}
    max_event_runs: {per: day, count: 1}
    merge_window: 1h
    max_open_proposals: 3
    max_per_week: 3
---
# tenants/demo/tenant.yaml (spec fragment)
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
  redecide_after: 30d
  extra_guards:
    - {metric: farm.task.outcome, guards: [host.load.v1]}
```

The analysis model comes only from the role's `analysis` slot (GW-7), so the model that does the work and the model that judges it differ, as D-16 asks. Analysis is paid from the `insight` line, which is 0 by default, and runs in the lowest dispatcher class, so it is turned on per role with money and never crowds out the main work (BUD-7, BUD-8).

A type of the `insight` purpose has tier at most 1 and no tools that act or send ([INS-1](../conformance/rules.md#INS-1)). Its permissions are the intersection of `insight.tools`, the `system-ops` group, tier 1 and the `max_data` mark, never wider than the role's main work ([INS-2](../conformance/rules.md#INS-2)). Analysis starts only from a schedule or from `obs.*` and `alarm.*` events through a merge window of at least an hour, within the run limits ([INS-4](../conformance/rules.md#INS-4)); otherwise whoever writes to the logs could raise alarms and burn the `insight` and `review` lines. The output is a task result with at most three proposals; the core creates them only on a successful `complete`, and they inherit the attempt's data mark and `untrusted_seen` ([INS-3](../conformance/rules.md#INS-3)). Analysis can also be done by a job type without a model, which is the preferred path for anything a rule can check.

The dispatcher hands analysis to the role's bots in turn, and the input carries the bot's own attempts and the last rows of its "My outcomes" (INS-5, deferred). This keeps "one's own improvement" from becoming a lottery of task dispatch.

**"Loop disabled".** Because the money lines are 0 by default, the cycle is easy to turn off without noticing. The core calls a tenant's cycle disabled for any reason from a closed list, such as a role with an `insight` line of 0, a `review` line of 0, no `present_to` address or no person with `proposal.decide`, and `doctor`, the `apply` lint and the weekly digest print the reason ([INS-6](../conformance/rules.md#INS-6)).

**Money for the cycle.** The `apply` lint checks that the `review` line can pay for the reviews of the proposal ceiling of every role with analysis (BUD-13). The price of one review is the `tree_budget_eur` of its review type, the same number the core reserves before a review ([COM-6](../conformance/rules.md#COM-6)). In `demo`: `ops-runner` runs analysis up to 9 times in 31 days with at most 3 proposals each, so up to 27 proposals a month. Four lenses at 0.08 EUR and the farm manager's lens at 0.20 EUR give 0.51 EUR per proposal, about 13.9 EUR a month. One author role may use 30% of the `review` line, 15 EUR of 50, and the lint passes. With a 40 EUR line the role's share would be 12 EUR, and `apply` would reject the manifests with `doctor` showing the missing 1.9 EUR.

### Aggregators

An aggregator is a bot of the `aggregator` profile that looks at a bot group or the whole tenant and proposes changes one level up, for example "three machines out of four have the same nightly peak" (D-17). It reads only aggregated inputs: series rollups, the `data` field of reports, closed fields of proposals people already decided on, and decision statistics; never logs, files, notes or tool output ([AGG-2](../conformance/rules.md#AGG-2)). An injection through a bot report can only shift a number inside a schema.

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
      message_kinds: [report.ops.v1]
    max_sources: 50
```

A tenant has one level of aggregators: one does not read another ([AGG-3](../conformance/rules.md#AGG-3)). Marks do not drop: a rollup and a proposal carry the highest mark and the OR of `untrusted_seen` of their inputs ([AGG-4](../conformance/rules.md#AGG-4)). There is no aggregation across tenants and no bot with a view of the whole farm ([AGG-5](../conformance/rules.md#AGG-5)); operators see the farm through metrics and the manager's daily summary. An aggregator's proposal is scored like any other and takes no credit from a bot that proposed the same earlier.

### The proposal

A proposal is a closed record. The bot submits only the body, which passes the schema `botaru.proposal/v1`; the record fields are written only by the core ([PRP-1](../conformance/rules.md#PRP-1)).

```yaml
# the body the bot submits (schema botaru.proposal/v1)
category: schedule
problem:
  code: backup_deadline_miss
  subjects: [task_type:demo.db.backup.v1, schedule:report-nightly, host:db1]
  severity: medium
evidence:                     # at most 10 references; the core rereads the values
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
note: "The report and the backup run in the same window on db1..."   # untrusted
---
# the record that only the core writes
id: p_0101
tenant: demo
origin: {role: ops-runner, worker: w_0007, acting_bot: bot_17, bot_epoch: 1, task: t_5521, level: 0}
fingerprint: "sha256:..."
data_mark: internal
untrusted_seen: {value: false, sources: []}
self_influence: [{series: farm.task.outcome, author_share: 0.0, role_share: 0.0}]
state: screened
```

The `note` is always untrusted: code does not read it, reviewers never get it, and people see it only as a marked block behind "Details". The author is the bot whose attempt submitted the result ([PRP-14](../conformance/rules.md#PRP-14)).

**Change kinds.** Exactly one change from the allowlist ([PRP-3](../conformance/rules.md#PRP-3)):

| `change.kind` | What "Accept" creates | Who changes the world |
|---|---|---|
| `none` ("for information") | nothing; the proposal moves to `noted` | nobody |
| `action_request` | an action approval on behalf of the person who accepted; also `service/control` and `monitor/script_run` within the author role's permissions | a person who approves, then the toolhost |
| `script` | a script run in `proposed` ([coordination.md](coordination.md)) | the same people and buttons |
| `manifest_change` | up to 10 `set` operations or `create_from_template`; the core builds a patch | the executor, with a commit |
| `human_change` | a card with a closed description: object, field from `spec/model/change-paths.yaml`, direction, value ([PRP-13](../conformance/rules.md#PRP-13)) | the executor, by hand and with a commit |

A proposal runs nothing by itself ([PRP-4](../conformance/rules.md#PRP-4)): only the human buttons "Accept" and "Lift veto" create an object of the ordinary path, and the farm never writes to git.

**What can be changed.** A proposal changes only objects in its own tenant's folder ([PRP-8](../conformance/rules.md#PRP-8)); a shared object (role, task type, plugin, action) affects every tenant, so it gets only the kind `none`. The allowlist `spec/model/proposable-fields.yaml` names the fields, and a tenant can only narrow it.

| Object | Allowed | Never |
|---|---|---|
| the tenant's `Schedule` | `spec.cron` as a window shift without halving the frequency, `spec.timezone`, `create_from_template` | pause (only kind `none`), `deliver_to`, the task type |
| the tenant's `Catalog` | add or tighten a dataset's backup reference; labels that nothing selects on | remove a backup reference, rarer backups, new addresses, egress |
| `Tenant` | lower `limits.*`, `campaigns.*`, `scripts.*` | budgets up, budget lines, retention, `self_approve`, `models_allowed` |
| a tenant dispatcher rule | guarded fields | the rest |
| `TaskType`, `Role`, `ToolPlugin`, `Action` | only kind `none` | everything |
| groups, channels, model profiles, the commission and charter, seats, incident classes, series | nothing | everything |

Permissions, money upward, the commission, the farm manager's limits, consent groups and the protected set are closed: such a target gives `rejected(forbidden_change)`, and a security idea goes as kind `none` for people to act on. A task type or schedule with `proposable: false` is protected ([PRP-10](../conformance/rules.md#PRP-10)); the lint requires this for alarm, backup and system types, and the farm manager uses the same protected set. Backups change only in the safe direction, and a proposal that saw untrusted text changes only fields in their safe direction ([PRP-11](../conformance/rules.md#PRP-11)). From v0.2 the frequency limit counts cumulatively over 90 days, so a series of small "a bit less often" proposals cannot get around it, and each bot has at most one open proposal ([PRP-16](../conformance/rules.md#PRP-16)). The card shows the local times that disappear or repeat at clock changes before "Accept" (TIM-5).

**Guarded fields.** The usual reliability levers, attempt deadline, retries, concurrency and the backup window, are dangerous without supervision. They change only within limits and with required guards, and a proposal with them is recommended only when both veto lenses gave at least 3 ([PRP-12](../conformance/rules.md#PRP-12)). They live in the tenant dispatcher rule, not in the shared task type ([coordination.md](coordination.md)).

**Evidence and self influence.** Evidence is at most 10 references to objects the author could read. The core rereads the values at creation and again before showing; a mismatch gives `rejected(evidence_mismatch)` or `parked(evidence_drift)` ([PRP-2](../conformance/rules.md#PRP-2)). People see the core's numbers, not the bot's. A bot whose own attempts produce the numbers could create a problem and then "fix" it, so the core records `self_influence`, marks the card `self_evidence` when the author's share is 0.3 or more, and computes the trial three times: over all points, without the author's and without its role's ([PRP-22](../conformance/rules.md#PRP-22)).

**Duplicates, limits and conflicts.** The core fingerprints the category, problem, subjects and targets; an open duplicate only raises `seen_count`, and a fingerprint people declined is refused for `redecide_after` ([PRP-5](../conformance/rules.md#PRP-5)). A proposal similar to one people decided on in 90 days is marked `related_to` and gives no credit (PRP-17, deferred). Two accepted changes of one field would overwrite each other and the trial would measure someone else's value, so a field has at most one running trial; conflicting open proposals wait in `parked(conflict)` ([PRP-9](../conformance/rules.md#PRP-9)).

**Untrusted text.** A proposal whose analysis saw a log, plugin output or a message note carries `untrusted_seen` with its source on every card ([PRP-7](../conformance/rules.md#PRP-7)). Its change can only be `none` or a safe-direction `manifest_change`, and the strict `untrusted` verdict threshold applies.

A refusal at creation makes no record, only the event `proposal.rejected.v1` with a reason from a closed list; the author sees it in "My outcomes" and stops spending money on dead repeats.

### Code checks and the commission

Code checks run first, in a fixed order: schema, reread evidence, duplicate, empty change, conflict, feasibility (someone can accept and realize), money ([COM-5](../conformance/rules.md#COM-5)). A failure ends in `dismissed` with a reason before any review money is spent.

**Lenses.** A lens is a point of view on benefit, cost or risk, and every member of the commission represents one lens. The skeleton sets its lenses in `spec/model/lenses.yaml`, and a tenant adds its own (below):

| Lens | What it scores | Veto |
|---|---|---|
| `security` | permissions, data, leaks, untrusted text | yes |
| `operational` | reliability, deadlines, load, rollback | yes |
| `cost` | proportionality of price and benefit; code computes the price | no |
| `value` | benefit by the numbers | no |
| `speed` | how fast people and tasks get their result | no |
| `clarity` | whether people understand what the farm does and tells them | no |
| `farm_manager` | the farm manager's advisory view, weight 1 (v0.2) | no |
| `necessity` | only bot escalations to the farm manager (v0.2) | no |

A proposal of kind `none` changes nothing, so only `value` and `security` score it ([COM-14](../conformance/rules.md#COM-14)).

**A persona per lens, and the tenant's own lenses** ([COM-19](../conformance/rules.md#COM-19), D-117). Each lens has its own persona addendum in the charter, appended after the reviewer persona for every review of that lens, so a member argues for its own aspect: the security member looks at the change as an attacker would, the reliability member asks what happens in the worst hour, and so on. The independence of the lenses rests on these personas and on blindness (a reviewer sees neither the author nor the other scores), not on model families, so all seats may run on one model profile. People extend the list in git: a tenant lens has an id `x_<name>`, a one- or two-sentence `meaning` and its persona addendum. It has no veto, it is reviewed by the skeleton type `review.lens.v1`, and the commission manifest sets its weight like any other. A tenant that also wants speed, clarity and its own reproducibility lens adds three seats and three addenda:

```yaml
# tenants/<t>/governance.yaml (fragment)
    seats:
      - {id: 5, lens: speed,             fallback_role: reviewer-b}
      - {id: 6, lens: clarity,           fallback_role: reviewer-b}
      - {id: 7, lens: x_reproducibility, fallback_role: reviewer-b}
    lenses:
      speed:   {persona: {ref: personas/lens-speed.md, sha256: "..."}}
      clarity: {persona: {ref: personas/lens-clarity.md, sha256: "..."}}
      x_reproducibility:
        meaning: Whether a result can be produced again from the same data and settings.
        persona: {ref: personas/lens-reproducibility.md, sha256: "..."}
```

The example persona addenda of the skeleton lenses are in `examples/personas/`.

**The charter.** Only the tenant charter `tenants/<t>/governance.yaml` declares commission seats ([GOV-33](../conformance/rules.md#GOV-33)). The commission manifest `Commission` sets only thresholds and weights per lens ([COM-8](../conformance/rules.md#COM-8)).

```yaml
# tenants/demo/governance.yaml (fragment)
spec:
  commission:
    seat_role: commission-member         # a reviewer role: its workers run the seats' tasks
    model_profile: seat-std
    seats:                               # one lens per seat
      - {id: 1, lens: security,    fallback_role: reviewer-a}
      - {id: 2, lens: operational, fallback_role: reviewer-a}
      - {id: 3, lens: cost,        fallback_role: reviewer-b}
      - {id: 4, lens: value,       fallback_role: reviewer-b}
    lenses:                              # a persona addendum for every seat lens (COM-19)
      security:    {persona: {ref: personas/lens-security.md, sha256: "..."}}
      operational: {persona: {ref: personas/lens-operational.md, sha256: "..."}}
      cost:        {persona: {ref: personas/lens-cost.md, sha256: "..."}}
      value:       {persona: {ref: personas/lens-value.md, sha256: "..."}}
    audit_sample: {min_per_month: 4, share: 0.25, max_per_digest: 2}
  promotion: {probation: 14d, exclude_change_kinds: [none], min_tenure: 30d, swap_cooldown: 30d}
  removal: {protect_new: 60d}
  confirmation: {ttl: 7d}
  attribution: {window: 14d}
---
# tenants/demo/commission.yaml
kind: Commission
metadata: {name: default, tenant: demo}
spec:
  review_deadline: 24h
  min: {security: 2, operational: 2}          # can only be raised
  weights: {value: 2, cost: 1, operational: 1, security: 1}
  recommend_if: {weighted_mean_gte: 2.5, value_gte: 3}
  park_if: {weighted_mean_gte: 1.5}
  high_risk: {security_gte: 3, operational_gte: 3}
  untrusted: {value_gte: 4, risk_max: medium}  # can only be raised
  escalation: {forward_if_score_gte: 2, untrusted_forward_if_score_gte: 3}   # v0.2
```

Every charter number moves only in its safe direction: deadlines up, memory limits down. Each lens adds one review to every proposal, so a longer list of lenses costs more: the `review` line must cover it ([BUD-13](../conformance/rules.md#BUD-13)).

**Who runs a lens.** At any moment a lens has exactly one source of executors: the holders of its seats if at least one is taken, otherwise the charter's `fallback_role` ([COM-7](../conformance/rules.md#COM-7)). A seat whose holder is under a person's hold counts as free. This way empty seats never stall the cycle, and the move from reviewer roles to promoted seat holders happens seat by seat. A seat holder is always a former functionary of its own tenant (D-22); it reviews through its lens, writes no new facts and has no path beyond the commission.

**Independence.** The reviewer role is not the author's, the review model is not the author's `analysis` profile, and a reviewer sees neither the author nor other lenses' scores; one task carries one proposal ([COM-2](../conformance/rules.md#COM-2)). A holder does not review its own proposals, and the farm manager does not review proposals about itself ([GOV-17](../conformance/rules.md#GOV-17)). The core builds the reviewer input as a data block of closed fields, reread values and the change in code words ([COM-3](../conformance/rules.md#COM-3)); from v0.3 the input also drops the author level and the review carries a forecast of its lens's own outcome ([COM-15](../conformance/rules.md#COM-15)):

```yaml
lens: operational
score: 1                       # 0-4
veto: false                    # veto lenses only
concerns: [report_delay]       # codes from a closed list
confidence: medium
forecast: {bad: 0.7, ok: 0.3}  # value: better/unchanged/worse; cost: over/within
note: "..."                    # untrusted, never read by code
```

**The farm manager's lens.** From v0.2 the farm manager is an advisory member of every commission whose tenant has not opted out of it: weight 1, no veto, not in `min`, `high_risk` or `untrusted`, not needed for a quorum, and skipped first when the `review` line runs short ([GOV-25](../conformance/rules.md#GOV-25), D-29). It runs on the tenant's reviewer worker with the seat profile and the tenant's money. It takes no part in choosing who leaves. The **necessity lens** scores only bot escalations that code screening did not settle; a forwarded escalation reaches the farm manager ([COM-13](../conformance/rules.md#COM-13), [farm-manager.md](farm-manager.md)).

**Verdict.** Deterministic code computes it from the manifest ([COM-4](../conformance/rules.md#COM-4)): a missing veto-lens review gives `parked(veto_lens_missing)`; a veto gives `dismissed(veto)`; a veto lens below `min` gives `dismissed(below_min)`; high risk or a guarded field needs `high_risk`, and untrusted text needs `untrusted`; then the weighted mean and the value score decide between `recommended`, `parked(score)` and `dismissed(low_score)`. From v0.3 a score of 3 or more with a bad-outcome forecast of 0.5 or more contradicts itself and counts as missing. There are no other outcomes.

**Money.** Reviews spend only the `review` line, reserved in full on the move to `in_review` ([COM-6](../conformance/rules.md#COM-6)). One author role gets at most 30% of the line a month, so a verbose role cannot take over the commission. Without money a proposal waits and then moves to `parked(review_budget)`.

**Shadow reviewer.** Every promotion removes a commission member, so it pays members to underrate others' improvements a little. A tenant can turn on a shadow: the fallback role reviews a random share of proposals with the seat's profile, the review is never read by the verdict, and a sign test on "seat below shadow" sets the flag `shadow_divergence` (COM-9, COM-10, deferred; D-103).

### The digest and people's decision

People are shown only `recommended` proposals and audit sample items ([DIG-1](../conformance/rules.md#DIG-1)). Code assembles the weekly digest and cards from records; a reporter's text goes only as a separate marked part. The digest goes only to `insights.present_to`, and an item above a recipient's ceiling shows only its code title in a group ([DIG-2](../conformance/rules.md#DIG-2)). Before showing, the core rereads the evidence once more.

```
Farm proposals for demo, week 41 (1 recommended, 1 from the audit sample)
1. Schedule: the db1 backup misses its deadline 4 nights out of 14
   Outcome of demo.db.backup.v1 over 14 days: 71% on time (10 of 14)
   Load of db1 at 02:00-03:00 up to 910 against the usual 300
   Change: schedule report-nightly, cron "0 2 * * *" to "30 3 * * *"
   Trial: outcome of demo.db.backup.v1; guards: slot of report-nightly, p95 of the report and the backup, load of db1
   Price: 0 EUR. Risk: low (report by 05:12 instead of 03:42)
   Commission: security 3, operational 3, cost 3, value 3
   Author: bot from its seat. Untrusted text: no. Self influence: no
   [Accept] [Decline] [Postpone] [Details]
```

A decision goes by the shared path of human decisions: the `decide` operation from the card's channel, a `nonce`, once, by an active `human` with strong identity (APR-4). The right comes from `proposal.decide:<category>`, and "Accept" also needs the right to start the realization, `action.request:<action>` or `script.run` ([DIG-3](../conformance/rules.md#DIG-3), [permissions.md](permissions.md)). Transitions follow only the lifecycle table ([PRP-6](../conformance/rules.md#PRP-6)).

| Button | What the core does | New state |
|---|---|---|
| "Accept" | creates exactly one object of the ordinary path, the trial record, the executor and the deadline | `realizing`; `noted` for kind `none` |
| "Decline" | asks for a reason from `spec/model/decline-reasons.yaml`; the fingerprint is refused for `redecide_after` | `declined` |
| "Postpone" | back to the next digest, at most twice | `recommended` |
| "Details" | shows the notes as marked blocks, concern codes, reread values | unchanged |
| silence for `decide_ttl` | | `expired` |

People's decisions come back to the cycle as numbers: the series `farm.proposals.accept_share` and `farm.proposals.decline_reason_share` by role and category ([DIG-4](../conformance/rules.md#DIG-4)). They feed analysis; nothing changes by itself because of them.

**Audit sample.** A commission that underrates others' work would stay invisible if people saw only what it recommends. So every month the core draws, by a recorded seed, a uniform random sample of the proposals that got a verdict and were not recommended: `dismissed`, and `parked` by score. The size is N = min(T, max(`min_per_month`, ceil(`share` × T))), and each item keeps its `inclusion_prob` ([DIG-5](../conformance/rules.md#DIG-5)). An item goes first as a blind card "Would you accept?", with the numbers, the change, the price and the risk but no scores, vetoes or verdict ([DIG-6](../conformance/rules.md#DIG-6)). The answer is recorded before the person sees the scores. After "Would accept" an item without a veto gets an ordinary "Accept", and an item with a veto only "Lift veto", for a person with `proposal.override_veto:<category>` who opened "Details" first. A lifted veto gives the author no credit and no promotion trigger, so a path around the veto earns nothing. A weighted share of "Would accept" of 0.3 or more over at least 10 answers sets `commission_underrates`.

A rejected change can also come back. A proposal with the fingerprint of one the commission dismissed or parked by score may be resubmitted by any bot after 30 days, at most once in 90 days ([PRP-24](../conformance/rules.md#PRP-24)); the reviewers do not know it is a resubmission. A rejected change of replayable fields can be replayed by code over 30 days of history, and a clear benefit reaches people as one digest line a week (PRP-25, DIG-9, deferred).

### Realization and the change trial

An accepted proposal with no owner would hang forever, so "Accept" also names the executor and the deadline ([PRP-18](../conformance/rules.md#PRP-18)): the person who accepted for an action or a script, a person from `insights.realizers` for a manifest or a manual change, with `realize_by` of 7 days. For `manifest_change` the core builds the patch with the line `Botaru-Proposal: p_0101`; the executor commits and runs `farmctl apply`.

A proposal becomes `realized` only through a verified link ([PRP-19](../conformance/rules.md#PRP-19)): the object its button created ran successfully, or an applied commit carries the line (or a person linked it with P2 `proposal.link_commit`) and the core checked that it touches only the tenant folder and sets the named fields. A value already set without such a link does not count. The core keeps the reverse edit; a commit with `Botaru-Revert: <id> <reason>` moves the proposal to `reverted`, and the reason `harm_observed` closes the trial as `worse` ([PRP-20](../conformance/rules.md#PRP-20)).

**The trial.** On "Accept" the core writes the trial record in the same transaction ([PRP-21](../conformance/rules.md#PRP-21)): the metric from `benefit`, the subjects, the values before over 14 days (over all points, without the author's and without its role's) and the guards. A metric outside the registry is refused at creation with `rejected(metric_unknown)`.

```yaml
trial:
  metric: {series: farm.task.outcome, subject: task_type:demo.db.backup.v1, compare: window.share, better: up}
  promised: {direction: up, change_pp: 20}
  before: {value: 0.71, samples: 14, without_author: {value: 0.71, samples: 14}, without_role: {value: 0.71, samples: 14}}
  guards:
    - {series: farm.schedule.slot, subject: schedule:report-nightly, before: 1.0}
    - {series: farm.attempt.duration, subject: task_type:demo.report.nightly.v1, compare: window.p95, before: 6120}
    - {series: farm.attempt.duration, subject: task_type:demo.db.backup.v1, compare: window.p95, before: 3300}
    - {series: host.load.v1, subject: host:db1, compare: day.p95, before: 410}
  window: {from: realized, days: 14}
```

The core checks the guards once a day. A guard beyond tolerance in the bad direction sends the executor a "Roll back" card with the reverse edit and, where a hold can suppress the change, a P2 `hold.set` button ([PRP-23](../conformance/rules.md#PRP-23)). The farm itself rolls nothing back.

At the end of the window code computes the outcome ([PRP-22](../conformance/rules.md#PRP-22)): a fired guard gives `worse`; another change on the same subjects gives `mixed`; otherwise the metric is compared in all three computations, and a worsening in any gives `worse`, an improvement in all gives `better`, the rest `unchanged`; too few samples give `undetermined`. The third computation stops two bots of one role from making things worse before "Accept" and better after. When the metric comes only from the author's own attempts, the remaining computation is marked `self_measured`, and a promotion trigger needs a person's "Count it".

### Safe evolution of the scaffold from v0.3

The model stays frozen; the farm gets better only through versioned manifests, and code guards every step (D-120).

**Coupling.** A change to one object can break the objects that read it, so code builds the graph of references between the manifests, from the callee to the caller: a task type feeds the roles, schedules, routes and dispatcher rules that use it, a role feeds its bot groups, a plugin, a skill, an action and a catalog feed their callers ([EVO-1](../conformance/rules.md#EVO-1)). The field lock of PRP-9 also holds the direct neighbours of a locked object, and a batch of changes goes from callee to caller.

**The merge gate.** A tenant with `insights.gate.required: true` shows a manifest change to people only after a gate at the commit the core applied ([EVO-9](../conformance/rules.md#EVO-9)). A person with `proposal.gate:<category>` runs `farmctl proposal gate <id>` (or `-pending` from a timer); Go code checks the change in a temporary git worktree, in this order, and stops at the first failure:

| Step | What it checks | Code |
|---|---|---|
| diff | at most `max_fields` fields (1), `max_diff_lines` lines (40) and one file; no path of the skeleton, no symbolic link, no protected kind or field; the file read back differs by exactly the fields ([EVO-2](../conformance/rules.md#EVO-2)) | `diff_size_exceeded`, `forbidden_path_modified`, `diff_unrenderable` |
| lint | an `apply` plan of the whole candidate set | `lint_failed` |
| coupling | no broken reference near a changed object | `coupling_broken` |
| regression | the visible eval cases of the affected roles, on the base and on the candidate | `regression_failed` |
| holdout | the hidden cases and the anti-gaming guard ([EVO-7](../conformance/rules.md#EVO-7), [EVO-8](../conformance/rules.md#EVO-8)) | `holdout_regression`, `reward_hacking_detected`, `shortcut_gaming_detected`, `audit_tampering_detected` |

A step without what it needs (no eval binary, no hidden folder, no case rated on both sides with the mock model) is `not_run`, shown, and never counted as passed. A failed gate moves the proposal to `dismissed(gate_failed)`; nobody is asked. A new apply makes a passed gate stale: the digest waits and "Accept" is refused until the gate runs again. A tenant without `required` keeps the behaviour of 0.3.0.

**Hidden cases.** The holdout cases have the format of `evals/cases/` and live in a folder outside the repository and outside every bot's reach. Only the step status, `holdout_passed` and the code leave the gate, for people; a bot sees only `rejected_at_gate`.

**Lessons and variants.** A decline by people for the change itself, a veto, a revert for harm and a worse trial become a review rule of the role: the same approach to the same problem and target is refused at creation with `rejected(review_rule)` until a person drops the rule with `farmctl review-rule drop` ([EVO-3](../conformance/rules.md#EVO-3)). Every proposed change of an object is kept in the variant archive with its parent, fields, outcome and the success of its children ([EVO-4](../conformance/rules.md#EVO-4)). An analysis sees up to five variants with different approaches, not only the last one, and its role's rules, as outcome classes without reason codes ([EVO-5](../conformance/rules.md#EVO-5)). People read the archive with `farmctl variant list|show` and write a variant back as a manifest file with `farmctl variant checkout` ([EVO-6](../conformance/rules.md#EVO-6)).

**Revert in one command.** `farmctl proposal revert <id> -reason harm_observed` writes the reverse edit into a temporary worktree, commits it with `Botaru-Revert`, moves the person's branch forward and applies the set; the proposal becomes `reverted`, the trial ends and the object has its hash from before the change ([EVO-10](../conformance/rules.md#EVO-10)). The core itself still never writes git.

### "My outcomes"

A bot that only learns "accepted" or "declined" learns to please. So each author bot has "My outcomes": a closed table that only code assembles ([DIG-7](../conformance/rules.md#DIG-7)). It holds its proposals and its refusals at creation: id, category, problem code, state, the class of people's decision, the promised change and the trial outcome, measured outcomes first. It has no names, no reasons for people's decisions, no lens scores and nothing about cases or the queue. From v0.2 a row also gets the trial numbers before and after, with and without the author and the role, and the guard that fired ([DIG-8](../conformance/rules.md#DIG-8)). The last 20 rows go into the bot's next analysis.

| id | Category | Problem code | State | Promised | Trial outcome |
|---|---|---|---|---|---|
| p_0101 | schedule | backup_deadline_miss | `realized` | +20 pp | `better` |
| p_0088 | capacity | queue_wait_high | `realized` | -30% | `unchanged` |
| p_0093 | cost | idle_capacity | `declined` | -15% | |

### From a trial to a seat

**The trigger.** There is one way into the commission ([GOV-14](../conformance/rules.md#GOV-14)): the functionary's own proposal, accepted by a person, realized through a verified link, still `realized`, of a kind outside `exclude_change_kinds` (`none` always), whose promotion probation of 14 days from `realized` has passed and whose every attribution candidate a person answered "No". The trial outcome `worse` removes the trigger; `better`, `unchanged`, `undetermined` and `mixed` keep it ([GOV-32](../conformance/rules.md#GOV-32), D-81). The change trial and the probation are two separate 14-day periods: the trial window never moves, while the probation waits as long as people have not answered.

**Attribution.** An attribution candidate is a possible link between an incident and a realized change. Only code creates it, by the rules `direct` and `dependency_1` of `spec/model/attribution-rules.yaml`, for an incident of a `change_sensitive` class opened within the window; only a person with `governance.attribute` decides ([GOV-13](../conformance/rules.md#GOV-13)). "Yes" and "No" are accepted only after "Details", because "No" is the answer that gives a trigger and would otherwise be pressed for quiet (APR-14). Silence never expires: an unanswered candidate stays `unknown` and the trigger waits ([GOV-30](../conformance/rules.md#GOV-30)). If people later declare the incident false, candidates without "Yes" are canceled, and a "Yes" turns into a dispute that its author settles (D-57, D-79). No bot creates, confirms or hides an attribution, an incident or an alarm.

```
Is the incident linked to the change? (candidate at_0042)
Incident: i_0913, class backup_late, db1, opened 11-18 02:40
Change: p_0101, moved report-nightly from 02:00 to 03:30, executed 11-10
Link by rule dependency_1: report-nightly reads the database on db1
[Yes] [No] [Don't know] [Details]
```

**Queue and case.** The `governance` loop puts the author in its tenant's queue, one entry per bot, ordered by trigger time, seen by no bot. A case opens for the head of the queue when the tenant has no open case and no `governance` hold. Only code opens a case; no P2 operation or bot tool does ([GOV-6](../conformance/rules.md#GOV-6)). A case lives only in its tenant and has no free text ([GOV-16](../conformance/rules.md#GOV-16)). Eligibility is checked at opening: active identity, `min_tenure` of 30 days, no confirmed attribution in 90 days. With a vacant seat the case is in mode `vacancy` and nobody leaves. Otherwise it is in mode `removal`, and it needs `swap_cooldown` (30 days) since the last freeing of a seat.

### Who leaves

When a newcomer enters a full commission, exactly one member leaves, and only a person's button makes it leave (D-25, D-115). At opening the core records the list L: the seats with a holder, minus holders protected for `protect_new` (60 days), never the farm manager seat ([GOV-12](../conformance/rules.md#GOV-12)). For each seat the card shows the lens, the tenure, how many times the holder recommended and vetoed over 180 days, how many of these were confirmed and how many refuted, and from v0.3 its accuracy. The symmetry matters: a refuted veto counts as much as a refuted recommendation, otherwise the strictest holder would always look best and the cycle would strangle itself.

Code proposes the leaver by accuracy, and a person chooses when code cannot ([GOV-43](../conformance/rules.md#GOV-43)). With `commission.accountability: on` code proposes the holder with the worst accuracy and marks it on the card. In every other case, under v0.2, in `shadow`, when the two worst are within one standard error, or when no holder has enough scored forecasts, code proposes nobody and the person picks a seat from L. "Confirm" carries the chosen seat; a seat outside L, or other than the proposed one, is refused with `leaver_not_offered`. A person who disagrees presses "Not now". No bot votes, and no bot learns about the choice.

```
Promotion to the demo commission: please confirm (case gc_0007)
Candidate: bot_17 (role ops-runner), functionary for 31 days
Basis: p_0101 "move report-nightly to 03:30", executed 11-10, change trial outcome: better
Incidents on changed objects since execution: 1 (i_0913), link rejected: "No" after 4 min, "Details" opened
Who leaves (choose one):
  seat 1 (security), bot_05, 300 days: recommended 12, vetoed 9; confirmed 15, refuted 2
  seat 2 (operational), bot_07, 150 days: recommended 20, vetoed 4; confirmed 14, refuted 5
  seat 3 (cost), bot_09, 210 days: recommended 41, vetoed 0; confirmed 18, refuted 6
Cannot leave (newcomers): seat 4 (40 days)
[Confirm: seat 1] [Confirm: seat 2] [Confirm: seat 3] [Not now] [Wrong candidate] [Details]
```

The outcome takes effect only after a decision of a person with `governance.confirm:commission`, after "Details", within 7 days ([GOV-9](../conformance/rules.md#GOV-9)). "Not now" leaves the entry at the head and waits `swap_cooldown`; the first "Wrong candidate" gives the entry its own 30-day pause while others go ahead, and the second removes it until the bot's next success ([GOV-31](../conformance/rules.md#GOV-31), D-101). Silence gives `expired`. Code sets case flags only from `spec/model/governance-flags.yaml`, such as `evicts_upheld_veto`; flags inform people and decide nothing ([GOV-19](../conformance/rules.md#GOV-19)).

**Changeover.** After "Confirm" the core does everything in one transaction: the candidate takes the seat with a new seat epoch, the leaver retires with `removed_by_vote` (a registry name kept from the old vote), the candidate's worker gets a new functionary identity so the work does not stop, the leaver's verified facts are offered only to the new holder, and its memory is sealed for 30 days ([bots.md](bots.md)). The audit holds only metadata under the names of `spec/model/governance-events.yaml` ([GOV-21](../conformance/rules.md#GOV-21)). From the new epoch the seat's tasks run on the seat model, so the newcomer gets a stronger model without any grant.

Before v0.2 there are no cases: a person with `admin:governance` places and removes holders with P2 `seat.assign` and P2 `seat.vacate`, only through `farmctl` (PRM-14, D-21). Successes before v0.2 stay only in the track record (D-51). A commission that keeps underrating gets an addressee: a persistent flag sends the tenant admins a `commission_review` card with ready `farmctl` commands (GOV-35, deferred).

### Accountability from v0.3

From v0.3 the commission answers for its outcomes (D-109). It starts in `shadow`: code finds links, scores forecasts and computes accuracy, and shows it all only to people; nobody leaves for a link or by accuracy, and a person chooses who leaves as in v0.2 ([GOV-36](../conformance/rules.md#GOV-36)). When every seat is held and every holder has `commission.accountability_min_forecasts` scored forecasts (20 by default, never below 8, D-116), the admins get a card, and `apply` accepts `accountability: on`. Nothing acts on records from before v0.3.

**Forecasts and accuracy.** Code scores each forecast with the Brier score when the outcome is known ([COM-16](../conformance/rules.md#COM-16)). The value lens is scored by the trial outcome alone; the veto lenses by a "too risky" decline, a confirmed attribution or `worse`; cost by the measured spend of the change subjects. A rejected proposal is scored through the audit sample, a resubmission or a replay. Accuracy is 1 - B / B_ref over 180 days, where B_ref is a forecast from the lens's base rates, so a strict security lens is never compared with a generous value lens ([COM-17](../conformance/rules.md#COM-17)). A warning that contradicts its own forecast counts as a missed forecast. People see accuracy from 10 scored forecasts; no bot sees it. The most accurate holder can get a stronger model for hard proposals and protection from leaving by accuracy, within `reward_cap_eur_month`, which is 0 by default (COM-18, deferred).

**A link to a rejected proposal.** Only code finds that a proposal the commission dismissed or parked by score in the 90 days before an incident could have prevented it: the incident is on the change target or one step away, and the problem code matches the incident class ([GOV-38](../conformance/rules.md#GOV-38)). External causes, untrusted proposals, the manager's proposals and self-written alarm series never give a candidate. The farm manager may only hint (GOV-39, deferred). A person confirms on a card after "Details" ([GOV-40](../conformance/rules.md#GOV-40)).

### Who answers, and what happens to them

For a confirmed link to a rejected proposal, the holders who were against it answer, except a veto, which never counts against its holder; nobody answers if people themselves rejected it. For a confirmed attribution to a recommended change, only the holders who were for it and whose lens covers the incident area answer ([GOV-41](../conformance/rules.md#GOV-41)). With `on` such a holder leaves its seat and is parked, not deleted: it keeps its memory, waits for a worker of its former role and gets no promotion trigger for 180 days ([GOV-42](../conformance/rules.md#GOV-42)). At most one such departure per tenant in 30 days, and the last holder of a veto lens never leaves this way.

## Why it works this way

- **A reward for measured benefit.** If people's approval led to a seat, the farm would teach bots to please. Only one's own change that the core measured and that did no confirmed harm gives a trigger (D-24, D-37), and "My outcomes" puts measured outcomes first.
- **Numbers, verdict and outcome are code.** The model does not write points, verdicts or trial outcomes, and the core rereads the evidence. An injection can shift a number inside a schema but cannot add an instruction or choose an outcome.
- **Nothing runs without a person.** A proposal does nothing by itself, "Accept" creates one object of the ordinary path on a person's behalf, and manifests change only by a person's commit. "The bot proposed, the bot applied" is closed by design.
- **Typical improvements are expressible, dangerous targets closed.** A narrow allowlist, guarded fields with guards and manual changes let bots propose real reliability and speed fixes, while permissions, money, the commission and backups in the unsafe direction stay closed.
- **Underrating must be visible.** Each promotion removes a member, so members have a reason to underrate. The blind sample, symmetric figures and accuracy within a lens show it, and "Lift veto" earns nothing.
- **Members do not judge each other.** A vote among members invites bargaining and refusals. Code proposes the leaver by measured accuracy, a person chooses while data is short, and the person's button stops a mistake (D-115). Bots know nothing about cases, so there is nothing to campaign for.
- **The tenant wall.** Observations, proposals, the commission, cases and trials live inside one tenant, and no bot reads several tenants.

## Example on `demo`

Tenant `demo` has `web1` and `db1`. The role `ops-runner` runs the nightly report `report-nightly` on `web1`, which reads the database on `db1`. The backup job `demo.db.backup.v1` (`makes_backup: true`, `proposable: false`) runs at 02:00 with a 60-minute deadline. The cycle is on, with the money from "Analysis". `alice` and `carol` decide proposals and confirm cases, `bob` realizes changes. Commission holders: seat 1 (security) `bot_05`, 300 days; seat 2 (operational) `bot_07`, 150 days; seat 3 (cost) `bot_09`, 210 days; seat 4 (value) `bot_12`, 40 days.

1. **Numbers.** For two weeks `observe` records the backup's `farm.task.outcome`: 10 nights `done`, 4 `deadline_exceeded`. From `monitor/item_history` the core extracts the load of `db1`: up to 910 at 02:00-03:00 against the usual 300.
2. **Proposal.** On Monday the schedule enqueues `ops.insight.v1`, and `bot_17` gets it. It sees that the backup is late on the nights when the report runs at the same time and submits `p_0101`: move `report-nightly` to "30 3 * * *", metric the backup's `farm.task.outcome`, +20 pp. The core rereads 0.71 and 910, finds `author_share` 0 (the backup is written by the job role), checks the folder, `proposable` and the frequency, and creates the record in `screened`.
3. **Commission.** The checks pass and the core reserves the price of five reviews. The holders score blind: security 3, operational 3 (code computed that the report will be ready by 05:12), cost 3, value 3; the farm manager's lens 3. Weighted mean 3.0, value 3: `recommended`.
4. **Card.** At 09:00 the core rereads 0.71 and sends the digest item above to `chat:demo-ops`. `alice` opens "Details" and presses "Accept". The core checks `proposal.decide:schedule`, writes the trial record with four guards and names `bob` with a 7-day deadline.
5. **Realization.** `bob` commits the patch with `Botaru-Proposal: p_0101` and runs `farmctl apply`. The core checks that only `spec.cron` in the `demo` folder changed, sets `realized` on 11-10 and keeps "0 2 * * *" as the reverse edit.
6. **Trial.** The guards hold for 14 days: the report lands in its slot every night, its p95 drops to 4700 seconds, `db1` load stays within tolerance. The backup succeeds 14 of 14 nights, +29 pp in all three computations: `better`. The row in `bot_17`'s "My outcomes" gets the numbers.
7. **Attribution.** On 11-18 code opens `i_0913` of class `backup_late` on `db1` and, by `dependency_1`, an attribution candidate to `p_0101`. `alice` opens "Details", sees a busy disk, and answers "No".
8. **Queue and case.** On 11-24 the probation has passed, so `bot_17` enters the queue. On 12-03 the loop opens `gc_0007` in mode `removal`: `bot_17` has 31 days of tenure, L is seats 1, 2 and 3 (seat 4 is protected). The tenant is on v0.2, so code proposes nobody.
9. **Who leaves.** `alice` gets the promotion card above, opens "Details", sees that seat 3 was refuted 6 times in 41 recommendations, and presses "Confirm: seat 3". `bot_17` takes seat 3 on the seat model, `bot_09` retires, `bot_17`'s worker on `web1` continues as a new identity `bot_31`, and the verified facts of `bot_09` are offered to `bot_17`. No bot learned about the case.
10. **The same case under v0.3.** With `accountability: on` and every holder past 20 scored forecasts, code would mark the worst-accuracy seat, say seat 2, and "Confirm" would accept only seat 2. If `alice` disagreed she would press "Not now".
11. **Audit sample.** That month `p_0103` to raise `max_running` of the report rule from 2 to 3 got an operational veto for the load on `db1`. The sample draws it, and `carol` answers "Would not accept" on the blind card; the item closes. Had she answered "Would accept", she could only "Lift veto", and the author would get no credit.
12. **If things had gone otherwise.** Had the report started missing its slot, the guard would have fired, `bob` would have got a "Roll back" card, the outcome would have been `worse`, and no trigger would have appeared. Had `alice` never answered the attribution card, the trigger would still be waiting.

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
| [COM-3](../conformance/rules.md#COM-3) | The core builds the reviewer input | v0.1 until v0.2 | yes |
| [COM-4](../conformance/rules.md#COM-4) | Code computes the verdict, without a veto lens the proposal waits | v0.1 | yes |
| [COM-5](../conformance/rules.md#COM-5) | Code checks run before reviews | v0.1 | no |
| [COM-6](../conformance/rules.md#COM-6) | Money for reviews | v0.1 | no |
| [COM-7](../conformance/rules.md#COM-7) | A lens has one executor, the rule for moving from roles to seats | v0.1 | no |
| [COM-8](../conformance/rules.md#COM-8) | The commission manifest sets only thresholds and weights | v0.1 | no |
| [COM-13](../conformance/rules.md#COM-13) | The necessity lens and the `escalation` section of the commission manifest | v0.2 | yes |
| [COM-14](../conformance/rules.md#COM-14) | A proposal of kind `none` is assessed by two lenses | v0.1 | no |
| [COM-15](../conformance/rules.md#COM-15) | The core builds the reviewer input, and a review carries a forecast | v0.3 | yes |
| [COM-16](../conformance/rules.md#COM-16) | Every review is a forecast scored by code | v0.3 | no |
| [COM-17](../conformance/rules.md#COM-17) | Accuracy of a commission member | v0.3 | no |
| [COM-19](../conformance/rules.md#COM-19) | Every lens has its persona, and a tenant adds its own lenses | v0.1 | yes |
| [DIG-1](../conformance/rules.md#DIG-1) | People are shown only recommended proposals and the audit sample | v0.1 | yes |
| [DIG-2](../conformance/rules.md#DIG-2) | Digest address and recipient ceiling | v0.1 | yes |
| [DIG-3](../conformance/rules.md#DIG-3) | The right to decide on a proposal | v0.1 | yes |
| [DIG-4](../conformance/rules.md#DIG-4) | Feedback from human decisions | v0.2 | no |
| [DIG-5](../conformance/rules.md#DIG-5) | A random audit sample by a recorded seed | v0.2 | yes |
| [DIG-6](../conformance/rules.md#DIG-6) | The blind sample card and a separate "Lift veto" button | v0.2 | yes |
| [DIG-7](../conformance/rules.md#DIG-7) | The closed form of "My outcomes" | v0.1 | yes |
| [DIG-8](../conformance/rules.md#DIG-8) | Trial figures in "My outcomes" | v0.2 | no |
| [EVO-1](../conformance/rules.md#EVO-1) | The output coupling graph of the manifests | v0.3 | yes |
| [EVO-2](../conformance/rules.md#EVO-2) | A scaffold change stays small and away from the skeleton | v0.3 | yes |
| [EVO-3](../conformance/rules.md#EVO-3) | Lessons of review become rules of the role | v0.3 | yes |
| [EVO-4](../conformance/rules.md#EVO-4) | The variant archive and the lineage of the scaffold | v0.3 | yes |
| [EVO-5](../conformance/rules.md#EVO-5) | Analyses see diverse variants, not only the last one | v0.3 | yes |
| [EVO-6](../conformance/rules.md#EVO-6) | People read and reuse the archive | v0.3 | no |
| [EVO-7](../conformance/rules.md#EVO-7) | Hidden holdout cases stay hidden | v0.3 | yes |
| [EVO-8](../conformance/rules.md#EVO-8) | The anti-gaming guard | v0.3 | yes |
| [EVO-9](../conformance/rules.md#EVO-9) | The merge gate of a manifest change | v0.3 | yes |
| [EVO-10](../conformance/rules.md#EVO-10) | A revert in one command | v0.3 | yes |
| [GOV-6](../conformance/rules.md#GOV-6) | Only the core opens a case | v0.1 | yes |
| [GOV-9](../conformance/rules.md#GOV-9) | The case outcome after a person's button | v0.2 | yes |
| [GOV-12](../conformance/rules.md#GOV-12) | The seats that can leave and the person's choice | v0.2 | yes |
| [GOV-13](../conformance/rules.md#GOV-13) | Code creates the incident attribution, a person decides | v0.1 | yes |
| [GOV-14](../conformance/rules.md#GOV-14) | Trigger, queue and eligibility for promotion | v0.2 until v0.2 | no |
| [GOV-16](../conformance/rules.md#GOV-16) | Governance lives in the tenant | v0.1 | yes |
| [GOV-17](../conformance/rules.md#GOV-17) | Recusal and blindness | v0.2 | yes |
| [GOV-19](../conformance/rules.md#GOV-19) | Case flags | v0.2 | no |
| [GOV-21](../conformance/rules.md#GOV-21) | Case audit with metadata only | v0.2 | yes |
| [GOV-25](../conformance/rules.md#GOV-25) | The manager's advisory lens in every commission | v0.2 | yes |
| [GOV-30](../conformance/rules.md#GOV-30) | Silence on an attribution does not expire | v0.2 | no |
| [GOV-31](../conformance/rules.md#GOV-31) | "Not now" and "Wrong candidate" on the promotion card | v0.2 | no |
| [GOV-32](../conformance/rules.md#GOV-32) | A measured deterioration removes the trigger | v0.2 | no |
| [GOV-33](../conformance/rules.md#GOV-33) | Only the charter declares seats | v0.1 | yes |
| [GOV-36](../conformance/rules.md#GOV-36) | Accountability runs in shadow until people switch it on, without retroactive effect | v0.3 | no |
| [GOV-38](../conformance/rules.md#GOV-38) | Code finds a link between an incident and a rejected proposal | v0.3 | yes |
| [GOV-40](../conformance/rules.md#GOV-40) | A person confirms a link on a card | v0.3 | yes |
| [GOV-41](../conformance/rules.md#GOV-41) | Who answers for a link | v0.3 | yes |
| [GOV-42](../conformance/rules.md#GOV-42) | A member who answers for a link returns to the functionaries, and promotion from v0.3 | v0.3 | yes |
| [GOV-43](../conformance/rules.md#GOV-43) | Code proposes who leaves when a newcomer enters | v0.3 | yes |
| [INS-1](../conformance/rules.md#INS-1) | Task type of the purpose `insight` | v0.1 | yes |
| [INS-2](../conformance/rules.md#INS-2) | Analysis permissions are no wider than the role's main work | v0.1 | yes |
| [INS-3](../conformance/rules.md#INS-3) | Proposals are created only when an attempt finishes successfully | v0.1 | yes |
| [INS-4](../conformance/rules.md#INS-4) | Analysis frequency and merge window | v0.1 | yes |
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
| [PRP-9](../conformance/rules.md#PRP-9) | One running trial per field of an object | v0.1 | yes |
| [PRP-10](../conformance/rules.md#PRP-10) | The `proposable` flag of a task type and a schedule | v0.1 | yes |
| [PRP-11](../conformance/rules.md#PRP-11) | Backups and untrusted text only in the safe direction | v0.1 | yes |
| [PRP-12](../conformance/rules.md#PRP-12) | Guarded fields | v0.1 | no |
| [PRP-13](../conformance/rules.md#PRP-13) | The kind "change made by people by hand" | v0.1 | no |
| [PRP-14](../conformance/rules.md#PRP-14) | The author bot in the proposal record | v0.1 | no |
| [PRP-16](../conformance/rules.md#PRP-16) | One open proposal per bot and a cumulative frequency limit | v0.2 | no |
| [PRP-18](../conformance/rules.md#PRP-18) | Executor and deadline of an accepted proposal | v0.1 | no |
| [PRP-19](../conformance/rules.md#PRP-19) | "Executed" only by a verified link | v0.1 | yes |
| [PRP-20](../conformance/rules.md#PRP-20) | Reverse edit of an executed change | v0.1 | yes |
| [PRP-21](../conformance/rules.md#PRP-21) | Change trial record on "Accept" | v0.1 | yes |
| [PRP-22](../conformance/rules.md#PRP-22) | Own evidence share and the trial outcome computed by code | v0.1 | yes |
| [PRP-23](../conformance/rules.md#PRP-23) | The "Revert" card for the executor | v0.1 | no |
| [PRP-24](../conformance/rules.md#PRP-24) | A rejected proposal can be submitted again | v0.3 | no |
| [PRP-26](../conformance/rules.md#PRP-26) | Every trial guards the farm's work | v0.3 | no |
