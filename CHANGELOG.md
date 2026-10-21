# Changelog

Skeleton versions: v0.1, v0.2, v0.3, v0.4. A skeleton version states which rules the conformance suite checks
from that version on; what goes into each version is stated in the section "What is in which version" of every
document.

Entries are grouped by area; within a group the newest entry comes first.

## Unreleased

### Skeleton & Contracts

- Safe evolution of the scaffold (D-120, rules EVO-1..EVO-10, additions to v0.3; a tenant without the opt-ins
  `insights.evolution.enabled` and `insights.gate.required` keeps the behaviour of 0.3.0, except that a revert may
  also take out a field the change added): the output coupling graph
  of the manifests and the field lock over direct neighbours (EVO-1); the merge gate of a
  manifest change with its diff size and protected path limits (EVO-2), hidden holdout cases (EVO-7), the
  anti-gaming guard (EVO-8) and the opt-in `Tenant.spec.insights.gate` (`required`, `max_fields`,
  `max_diff_lines`); P2 `proposal.gate` with the capability `proposal.gate:<category>`, the record field `gate`, the
  proposal transition `recommended`/`presented` → `dismissed(gate_failed)`, the gate before "Lift veto" of the
  audit sample and the gate codes in `reasons.yaml` (EVO-9); review rules of a role from refused, reverted and worse changes with `rejected(review_rule)` and P2
  `review_rule.list|drop` (EVO-3); the variant archive with lineage, P2 `variant.list|show` and the analysis block
  `evolution` (`insight-evolution.schema.json`) without reason codes (EVO-4..EVO-6, GOV-29); the revert in one
  command, with the realization field `revert_absent` for the fields a change added (EVO-10). The eval check
  `tools_allowed` takes `tools_required`.
- The P4 request allowlist (`p4-anthropic-messages-request.schema.json`) accepts `caller` of type `direct` on a
  `tool_use` block of the history: the Messages API marks every tool call of the model with it, and a runtime
  that sends the history back was refused at its second model call; a call made by a provider server tool still
  does not pass (GW-1).
- The demo catalog and `examples/README.md` give trigger 4401 the monitoring class `disk_free` of
  `spec/model/monitor-map.yaml` instead of `disk_full`, which is an incident class, not a monitoring class: the
  free space problem now raises `alarm.disk.free.low`.

### Reference Implementation (impl/)

- The stand's own roles (impl/SIM.md, "What runs"): the script translator `translator` with the task type
  `script.translate.v1` on `/script`, and the campaign operator `ops-operator` (profile `manager`, read and action
  campaigns) with `ops.rollout.v1` on `/rollout`; `alice` runs scripts (`script.run` through the seed group
  `runners`); `farm-manager` claims `manager.postmortem.v1` on the stand; the
  `standard` farm runs 39 agent workers. `BenchConfig.Roles` also takes a role file path.
- `botaru-sim -farm smoke|standard` (impl/SIM.md, "Farm profiles"): a farm profile names the tenants of the stand,
  their machines and the workers of each bot role per tenant. `smoke`, the default, is the stand as it was;
  `standard` runs the four tenants of the cloud load test on sixteen machines with 35 agent workers. The eval bench
  (`evals.BenchConfig.Tenants`, `EditTenants`) gives each tenant a copy of the demo manifests, its own Telegram
  channel and agents, and runs each worker under a context of its own (`StopWorker`); the world, the fake Zabbix
  API and the physics are per machine and tenant. `botaru-eval` and its cases are unchanged.
- An analysis of a role's own results (`purpose: insight`) and an aggregate run on the role's `analysis` slot, as
  the apply lint already priced them (PRF-1), and their calls are held to the position's analysis ceiling (GW-7):
  the claim took the main slot `base` before, so the analysis of `ops-reader` ran on the profile of its base work.
  Such a task waits for the provider pair of the analysis profile. Upgrade: a tenant whose `models_allowed` lacks a
  role's analysis profile, or a charter whose `analysis_max_rank` is below that profile's rank, now refuses these
  calls; check both before upgrading.
- The runtime gets the full result schema and fixes its answer before `complete` (impl/ARCHITECTURE.md "The
  attempt record"): the claim payload carries `full_result_schema` beside the unchanged `result_schema`, with the
  `enum_from` values of the attempt and the patterns of `x-botaru-format`; `botaru-agent` shows it to the model with
  the rule to omit unknown optional fields, drops a `null` of an optional field, checks the answer locally and gives
  a failing one a single repair turn with the failed paths, so a format error no longer spends the attempt (OUT-1).
- `observations_query` lists the subjects of its series as a closed list and says which kind each series takes, and
  an analysis task carries `insight_context` (the subjects of the role's work types and, with the evolution on, the
  manifest fields a proposal may change with their current values), so a real model reads the series that have
  points and proposes only changes the core takes.
- `botaru-sim` for real models: the world samples the traffic items of `eth0` too, so every field of
  `monitor/item_history` has points on every machine; `incident-and-commission` and `scaffold-evolution` seed the
  core series of the investigations with a history of repeated long attempts before the analysis; the evolution
  mode (scenario and console command `evolve`) also writes a load check into the instruction of `ops.insight.v1` in
  the farm git, and `accepted_and_realized` takes any lower `max_queued`.
- Fixes found by the first `botaru-sim -real` runs on Claude Haiku 4.5 (the mock had hidden them): the built-in
  tool `request_action` has no `oneOf` at the top of its input schema, which the Messages API refuses (the
  per-action argument schemas are now branches of `anyOf` under `args`; `requestAction` checks the pairing as
  before); the claim payload carries the core's own result schema for a type that declares none (the analysis
  envelope `insight-result`, the lens `review-result` or the necessity output), with every `$ref` inlined; the
  runtime passes the task's params beside the goal (the incident view of `manager.incident.respond.v1` never
  reached the model) and the current time, gives a model that answered in prose one more turn for the result
  object, and takes a plain answer as `{answer}` for a type with no result schema. The sim mock refuses a tool
  with a top-level `oneOf`/`allOf`/`anyOf` and marks its tool calls with `caller` like the Messages API, the
  console prints a real model's final answer, and the `geo_described` check also takes "GeoTIFF".
- `botaru-sim` runs the scaffold evolution (D-120, [impl/SIM.md](impl/SIM.md)): the stand keeps its set in a
  farm git under `<work>/repo` and applies every change as a commit of it, `demo` turns on
  `insights.evolution` and `insights.gate.required`, `bob` holds `proposal.gate:reliability`, and hidden cases of
  the stand's own gate suite lie under `<work>/holdout`. The seventh scenario `scaffold-evolution`: the mock
  model's analysis proposes to lower `spec.limits.max_queued` of `demo`, the commission recommends it, the merge
  gate passes it on the farm git, `bob` accepts it, the patch with `Botaru-Proposal` is applied, then reverted in
  one step for harm; the variant gets the outcome `reverted` and a review rule refuses the same approach with
  `rejected(review_rule)`. Console commands `gate`, `realize`, `revert`, `variants`, `rules` and `evolve`; the
  event kind `FARM GIT`. The eval bench gains `BenchConfig.Commit`, `PersonOperators`, `P2As`, `ApplySet` and
  `PlanSet`.
- The scaffold evolution contour (impl/ARCHITECTURE.md "Safe scaffold evolution"): `internal/manifest/coupling.go`,
  the package `internal/gate` (temporary git worktree, YAML line editor, the steps diff, lint, coupling, regression
  and holdout, `Patch`, `Revert`, `Checkout`), `internal/evals/holdout.go` (`CheckRewardHacking`), migration
  `0080_scaffold_evolution` (`scaffold_variants`, `review_rules`), and in farmctl `proposal gate|patch|revert`,
  `variant list|show|checkout` and `review-rule list|drop`. `botaru-eval -holdout-dir` runs hidden cases from a
  folder outside the root; `results.jsonl` adds seconds, tool calls, finding kinds and security findings.
- `botaru-sim`, the farm simulator ([impl/SIM.md](impl/SIM.md)): the eval test bench with the monitor profile on,
  the reference bot `ops-reader`, toolhosts of `web1`, `db1` and `ctd1`, the oneshot executor `pipeline` and the
  monitoring adapter `botaru-zabbix`, whose tools work on a virtual world under the run's directory (machines,
  disks, services, monitoring problems and series, live logs, check results). Stubs of `systemctl`, `df` and
  `journalctl` answer from the world, so no tool call touches the host. A fake Zabbix 7.0 API serves the world
  and sends the problem webhook. A director runs the scenarios `nginx-down`, `slow-site`, `disk-filling` and
  `prompt-injection` with their checks, or a continuous mode with a console; virtual approvers press the cards by
  `-auto-approve none|safe|all|random`. The built-in mock model plays a plain operator without a key; `-real`
  calls a provider with `-model`. The money fuse `-max-eur` refuses a model call whose reserved worst case would
  reach the cap, holds every tenant with `no_enqueue`, `no_claim` and `cancel_running`, refuses further model calls
  and stops the run. Every event goes to the console and to `sim-events.jsonl`.
- `botaru-sim` runs every role of `examples/roles` except the deferred `analyst` (sandbox, tier 2) and
  `assistant` (DSP-10): the data bots with plugins served from the world's data files,
  the job worker of `data-job` for its schedule tasks, the commission reviewers and the farm manager with its seat
  assigned. The scenario `incident-and-commission` plays an incident answered by the farm manager and a proposal
  of `ops-reader`'s analysis through the commission to a person's card; `lab-requests` asks the data bots.
- The eval bench sends the persona texts with apply, so a set with a commission charter applies, and can apply
  its set again while it runs.
- The own plugin `cmd` takes `-program-root` (a test stand only): the manifest's absolute programs run from under
  that directory.
- The eval bench can carry another program's stand: further roles, an edit of the set and the seed, the skill
  folders of apply, its own toolhosts and monitoring API, core settings, observers of the Bot API and of the model
  calls, and a gate in front of the provider; a monitoring export no longer counts as pending work of a case.

### CI & Checks

- The workflow `claude-review.yml`: a comment with `@claude` in the conversation of a pull request by a person
  with write access runs a Claude review there; it never runs on a push and cannot push (`ci/README.md`, "Claude review").

## 0.3.0 - 2026-10-17

### Skeleton & Contracts

- Skeleton release 0.3.0: this block, until now "Unreleased", is the release 0.3.0 of skeleton v0.3, the first
  published release; the compatibility promise of `spec/versions.md` holds from its tag `v0.3.0`.
- Decision D-119: the defaults of the skeleton questions in `docs/open-questions.md` are accepted as rules of
  the skeleton for 0.3.0, and the tables of the skeleton questions are empty; the part "What every
  installation decides" stays as the checklist of an installation's own repository.
- The format `date` (a calendar date, RFC 3339 full-date) in `spec/model/formats.yaml`; its pattern takes only
  the days that exist, which `ci/check_schemas.py` and `impl/internal/spec` check. `report.sensor.v1` gets the
  optional parameter `date`.
- `Schedule.spec.paused` (v0.2, TIM-5) and the `schedule.skipped` reason `paused`.
- OBS-11, CAP-6 and DEP-10 describe only what the schemas hold: the alarm path lines are `doctor` checks and
  `status` lists the tenants with the farm manager off under `farm_manager_disabled`; the volume free space
  series is a tenant `ObservationSeries` written by the installation's `job` role; the deployment lint checks
  the facts that `deploy-topology.schema.json` has fields for, so the agent's network (DEP-5) and the machine of
  the monitoring system (MON-2) are not linted in 0.3; MON-11 names `health` and `doctor`; TIM-3 and TIM-5 define
  the skip of a paused schedule.
- The ten accepted architecture decisions of the table "Decisions (ADR)" in `docs/architecture.md`, which D-86
  (13) said would appear with the implementation, are files 0001 to 0010 in `docs/adr/` in MADR 4.0.0
  form with the rules that confirm them; the next ADR number is 0011.
- Skeleton release 0.3.0: `spec/VERSION` and the contract sheets of `spec/ports/` carry 0.3.0; until the tag,
  forms changed in place with an entry in `spec/removed.yaml` (`spec/versions.md`). `examples/roles/assistant`
  has `memory: none` while the personal assistant is deferred (DSP-10, `person_memory_deferred`, MEM-2), and
  flow 8 drops its `memory.search` step. The manifest fixtures
  allow `person_memory_deferred` beside the memory lints, `approvals: 2` for a risk other than routine passes
  from v0.2, and the INJ-12 output coverage of `ci/check_evals.py` skips `monitor_note` of the deferred MON-7.
- Decision D-118, skeleton gaps before a v0.3 claim: P2 `seat.resume` names exactly one of `incident` and
  `tenant` (GOV-22, GOV-47, MGR-47); `commission.status` gets `governance_v03_since`, `accountability` and per
  seat `accuracy`, `scored_forecasts` and `missed_forecasts` (GOV-36, COM-17); `attention_notice` closes
  `proposal.op` to the control operations a core may serve to the seat (all of `control-ops.yaml` but
  `notify`) and `service_start`, `service_stop`, `service_restart` (breaking, recorded in `spec/removed.yaml`,
  MGR-52); `proposal.rejected` gets `resubmit_too_early` and `manager_restricted`; promotion `removed_reason`
  gets `parked` (GOV-42); the core series `farm.takeover.count` (MGR-48); the notice `commission_weekly` with
  its code texts (COM-17, GOV-36); the incident `cause` and the P2 operation `incident.cause` (GOV-38,
  GOV-41); `x-botaru-version: v0.3` on the v0.3 manifest fields of `Governance` and `IncidentClass`; the
  Catalog field `role_incident_hold` that only makes HLD-9 stricter; the source channel binding
  `spec/bindings/channels/webhook.md` (ING-1 to ING-4) with an `ext.*` event type and up to 32 data fields in
  the activity, and the lints `ingress_reserved_source`, `ingress_source_too_long` and
  `ingress_source_duplicate`; the `facts` of the `ops.investigate.v1` result that the eval cases check, and
  injected eval lines inside the `logs/tail` window; the examples `kb.ingest.v1`, `manager.postmortem.v1`, two
  corpora, a role skill, a source channel and the v0.3 incident class fields.
- The code text catalog gets `cmd.cancel.finished` and the `cmd.profile.*` lines, and `cmd.cancel.confirm`
  names the confirmation word `{nonce}`.
- The card schema gets the optional fields `state` (the state of the subject when the card was drawn, for an
  update in place) and `reminder` (a reminder card); the code text catalog gets the `card.*` texts of an
  action approval card, its buttons, its answers and its "Details" view.
- The events catalog gets `alarm.provider.config.v1`, the alarm by code of GW-13 for a provider configuration
  error, which had no catalog entry.
- The P1 `hello` and `heartbeat` request schemas get the optional field `health`, a list of `health-report`
  records, so a toolhost can report the state of each plugin as OBS-10 requires; the rule had no field to
  carry the report.
- The activity schema gets the optional fields `forwarded_from` and `quoted_from`: the identity of the author
  of forwarded or quoted text when the messenger shows a person, so the core can record that person in
  `personal_subjects` (AUD-5). The Telegram binding fills them from `forward_origin`, the replied message of a
  `quote` and `external_reply`.
- The code text catalog gets `delivery.withheld.channel_ceiling`, the text a person gets in place of an answer
  above the ceiling of the channel (SEC-9).
- The code text catalog gets `notice.alarm`, the text of the `alarm_notice` delivery to admins in an
  installation without the monitoring system (MON-13).
- Owner decision D-117, commission lenses: every member of the commission represents one lens with its own
  persona addendum in the tenant charter (`commission.lenses.<lens>.persona`), and the independence of the
  lenses rests on these personas and blind reviews, not on model families: the rule on different model
  families is retired and the reason `veto_lenses_same_family` is kept only as a retired entry. The skeleton
  adds the lenses `speed` and `clarity` without a veto (lenses registry version 3), and a tenant adds its own
  lenses `x_<name>` without a veto, with a meaning and a persona addendum, reviewed by the type
  `review.lens.v1` (the new rule COM-19, the apply reasons `lens_persona_missing` and `lens_unknown`). Example
  addenda are in `examples/personas/`.
- Owner decisions D-109 to D-113, the rules of v0.3: the commission answers for its outcomes (forecasts scored
  by code, accuracy, a reward for the most accurate member, links of incidents to rejected proposals confirmed
  by a person, parking of a member who answers for a link, who leaves by accuracy), first in shadow and
  switched on by data; the farm manager is no longer stopped by an incident it could have prevented, but pages
  people and is restricted, and gets takeover on its model, notices from closed fields, an analysis after each
  incident and a monthly line; new personas with five anchors for the manager and accuracy paragraphs for the
  reporter and the translator; links of incidents to script runs, tenant check cases and a translator pause;
  resubmission and replay of rejected proposals and farm guards in every trial. 40 new rules; 13 rules of v0.2
  keep their rows with `until: v0.2` and name their v0.3 replacements in `superseded_by` (D-113). New schemas
  `review-result.v2`, `attribution.v2`, the tools `link_hint` and `attention_notice`, the P1 operation
  `card.feedback`, the registry `spec/model/manager-attention.yaml`, 11 new card and notice kinds and 38 suite
  checks in `docs/quality.md`.
- Owner decision D-94: five rules of v0.2 that change what the core writes for every task move to v0.1: the
  run fingerprint (PRV-1), artifact provenance (PRV-2), a reserve and a unit for every consumable meter
  (BUD-5, the meter registry is now version 2), one trace per task tree and external tracing context that
  decides nothing (OBS-2, OBS-3). Five new suite checks in `docs/quality.md`; the `trace` field of the task
  record and `traceparent` of the message envelope are v0.1 fields.
- Owner decisions D-88 and D-89: a farm on one server and without a monitoring system (the conformance profile
  `monitor` for the E5 export, alarms to people as the code text `alarm_notice` without it, the watchdog
  `kind: watch`, backups encrypted to external storage, the example `examples/deploy/single.yaml`, the lints
  `watch_missing` and `backup_external_required`, the rules MON-12 and MON-13); the Contributor License
  Agreement `CLA.md` (draft), external pull requests after signing, signature check by a CLA bot.
- Decisions of the fourth review of the third layer (D-87): call usage in micro-euros `eur_micros` and one
  daily reconciliation with the provider; the transfer basis `Tenant.spec.model_transfer` instead of
  `transfer_basis` of the profile; the P2 secret of a channel held by the driver (`p2_token` in the
  `worker.enroll` response), a personal chat needs no binding; the P4 allowlist with `temperature`, `top_p`,
  `top_k` and a dropped `metadata`; the lease token signature is optional; checking your own bot and channel
  from your own folder (`--seed`, `--driver-schemas`, skipping files that are not manifests); `hello-001` on a
  separate person `dave`, `origin.chat` as an object in `examples/hello-bot/sequence.yaml`; the section
  "Before opening the repository" in `ci/README.md`.
- Decisions of the third review of the third layer (D-86): a Telegram button with `callback_data` up to 64
  bytes; `worker_id` and the core signature in the lease token, the refusal `lease_mismatch`;
  `Botaru-On-Behalf` only from the driver of a person's channel; the condition of the test-control port (the
  test bench key, a store flag, no tenant other than `kind: test` over the whole life of the store); P4 is not
  a transparent proxy, JSON on inputs follows I-JSON; `transfer_basis` on the model profile; the audit head
  with `epoch` and `key_id`; erasure with `needles`; rounding usage up to the euro cent and a daily
  reconciliation; hello-bot follows the fixture envelope with `ai_notice` and `privacy_notice`; three
  readiness numbers; your own additions are checked from your own folder; an ADR for a change of a port or a
  contract; training examples without buoys, sequencing and protected areas.
- Decisions of the second review of the third layer (D-85): a principal in core records is the opaque id
  `human:p_<ULID>`; the reference binding of P4 to the Anthropic Messages API; the audit head signature
  Ed25519 and SHA-256 hashes over JSON in RFC 8785 form (`spec/hashing.md`); the test bench control port
  `spec/ports/test-control.md`; suites stay descriptions of checks; README and the short part of CONTRIBUTING
  in English, the "English" column in the glossary; the default time zone `UTC`, the training time zone of
  examples `Europe/Berlin`.
- CONTRIBUTING: the table "what I add, where I put it, what I run" with the `ci/` commands, the fields of a
  rule row, the contribution terms (external pull requests after the owner publishes the terms, issues until
  then). The folder `docs/adr/` with the MADR 4.0.0 template.
- The third layer of v0.1: JSON Schema files in `spec/model/` and `spec/manifests/`, closed registries, the
  generated lifecycles `spec/lifecycles/*.yaml`, the fixtures `conformance/fixtures/`, the suites
  `conformance/suites/`, eval cases and examples on the tenants `demo` and `demo2`.
- The license GNU AGPL-3.0-only (D-83) instead of Apache 2.0.
- The first consolidated text of the skeleton: the overview and the architecture, 13 port sheets, lifecycles,
  the rule registry, the glossary, documents on the parts of the farm, examples on the tenant `demo` and a
  description of the bot quality checks.
- The rule registry `conformance/requirements.yaml` became the only place where rules are written; the rule
  lists in the documents are generated from it.

### Reference Implementation (impl/)

- impl: P2 `status` lists under `farm_manager_disabled` only the tenants with `farm_manager.enabled: false`
  (D-77) instead of every tenant; each proposal dropped from a restricted farm manager seat writes
  `proposal.rejected.v1` with `manager_restricted` (MGR-47, D-118); a schedule with `paused: true` skips its
  slots (TIM-5); the matching items leave "Not in this version".
- impl: the files of `impl/internal/core` named by version or stage get domain names, and the largest files
  are split by responsibility; declarations and tests move verbatim, nothing is renamed.
- impl: `impl/README.md` is a short overview (programs, packages, key decisions, what is not built, checks),
  and the subsystem notes move to `impl/ARCHITECTURE.md`, grouped by domain and written as current behaviour;
  statements left from the v0.1 core are corrected.
- impl: the reference core claims 0.3 (unreleased) with the profiles `core`, `monitor`, `insights`,
  `governance`, `manager`, `messaging`, `campaign`, `knowledge` and `ingress`; `spec.Ahead` is empty and the
  v0.1 and v0.2 paths are gone: no incident stops the farm manager seat (MGR-55, GOV-47), `seat.resume` takes
  a `tenant` only (and `farmctl` has no form by an incident), a takeover is paid from the manager line
  (MGR-48), reviews hand in `review-result.v2` (COM-15). `stops_manager` and `incident_stop` apply with a
  warning and are not used. The list `spec.Deferred` names the surface of every deferred rule whose mechanism
  is still in the skeleton: `apply` refuses its kinds and fields, P1 and P2 its operations and body fields, P3
  its built-in tools, and `hello` `runtime.skills: farm`, all with `version_unsupported` and the path (D-114,
  MAN-11). The conformance runner checks the core's own claim, and reports a failing case that names a rule
  ended before the claim as unchecked (D-113): 74 passing cases, against 69 under the v0.1 claim. No paid eval
  run was made: the 22 eval rules of the claim are listed as not run in `impl/README.md`. Upgrade notes are in
  `impl/DEPLOY.md`, "Upgrading from the first version".
- Upgrade note: a tenant that applied v0.3 fields on a v0.1 core before `spec.Ahead` became version-aware now
  gets `version_unsupported` (MAN-11); apply it with `-drop-unsupported` or remove those fields
  (`impl/DEPLOY.md`, "Upgrading from the first version").
- impl (D-118): a parked holder's promotion entry leaves with `parked`; `spec.Ahead` lists the v0.3 fields the
  core builds with their version, and apply accepts them only while the effective claim is v0.3, so a v0.1
  core still refuses them (MAN-11).
- impl: the initiator commands `/tasks`, `/status`, `/cancel` (with confirmation, the tree and its waiting
  approvals canceled with the reason `initiator`), `/help`, `/me`, `/lang`, `/team` and `/about`, answered by
  code (PRM-17, REP-12).
- impl: service control without a card for a restart from the catalog's `no_card` under a `service_grant`, by
  card for start, stop and after untrusted text, with the initiator's own right, holds, the hourly limits with
  `alarm.service.control.rate` and one operation per service at a time (MON-8, APR-18, INJ-15); the commands
  `/service` and `/run` with routine pipelines (PRM-18, APR-17); the plugins `service` (systemctl) and `queue`
  (the oneshot queue) and `botaru-plugin polkit`, which prints the polkit rule of a machine from the catalog
  (DEP-4).
- impl: actions by manifest with approval cards: the built-in tool `request_action`, approvals, cards drawn
  from the record, P1 `decide`, `card.details` and `card.remind`, the action task with its write call executed
  at most once, reminders, expiry and cancellation by holds, offboarding and revoked rights (APR-1 to APR-5,
  APR-7, APR-10, APR-11, APR-19, CNT-7, MAN-9); Telegram cards with inline buttons and edits in place.
- impl: provider pairs with a circuit breaker, the wait for a provider rate limit and the wait for a
  configuration fix, seen by the dispatcher (GW-10, GW-11, GW-13, CAP-2); the running ceiling of dispatch
  rules (DSP-7); the 80% threshold event and the waiting reserve exception (BUD-4).
- impl: P2 `status`, `doctor`, `deadletter.list`, `task.redrive`, `delivery.redrive` and `audit.query` (DLV-4,
  AUD-7, MAN-8), and plugin health from toolhost reports (OBS-10).
- impl: P2 `health` reports the tick age of each background loop from the store, the age of the oldest urgent
  task, the share of deliveries delivered within 10 minutes and the free space of the store volume; the new
  `health` loop raises their alarms, the soft threshold of the store volume refuses enqueues of classes 3-5
  and the hard one answers `not_ready` on P1, and P1 answers `not_ready` while the store is unavailable
  (OBS-4, OBS-8, OBS-9, CAP-5).
- Owner decisions D-91 to D-93: the reference implementation of the core is written in Go, lives in this
  repository under `impl/` under the AGPL, and runs its own agent loop over P4 without a ready-made agent. The
  Go module skeleton `impl/` with the workflow `.github/workflows/go.yml` (gofmt, go vet, staticcheck, tests
  with the race detector, builds for linux/amd64 and linux/arm64, govulncheck) and Dependabot for `gomod`. The
  additive-only check now also rejects any constraint on a schema that was `true` or empty, `unevaluatedItems:
  false`, `prefixItems` where there were none, a changed `if` or `not`, and a new or changed `oneOf` branch
  (matched by content) that is not provably disjoint from the others.

### Simplification (v0.4 / D-114)

- Contracts only at the boundaries (D-114, C10): contracts with checks stay only at P1, E1, P3 with E3, P4 and
  E5; the pages of P2, C1-C4, E2, E4, the test-control port, the consent adapter and the background loops
  become short descriptions of how the core is built, and `spec/ports/README.md` separates the two. The
  operation map is version 2: `contract: false` marks the rows of those ports (P2 schemas are implementation
  schemas of the core and `farmctl`), and `operator_only` on P2 rows holds the closed list of PRM-14. Fourteen
  schemas nothing checked are removed (the E4 describe answer, run request, run outcome and cancel request;
  five test-control schemas; five C1 record schemas), and the permission table and stand step schemas stay as
  core test schemas. The agent worker declares its runtime in the new `runtime` field of P1 `hello`, and the
  attempt work item carries `reply_locale` and `skills`. RT-1, RT-2 and RT-5 become core rules about what the
  core requires of any runtime. The suites `store`, `runtime` and `sink` are gone: their checks move to
  `core-blackbox` (and `model-gateway`, `toolhost`), AUD-3 and FEN-2 are also covered by Go tests of the
  reference, and the check method `store` is removed.
- Owner decisions D-115 and D-116, the commission of v0.4 (D-114): the secret mandatory vote "who leaves" is
  replaced, code proposes the leaver by accuracy (GOV-43) and otherwise a person chooses it from the list of
  seats on the promotion card with the figures of every seat and "Not now", and only a person's button makes
  it effective; ballots, their package and reasons, case rounds, vote counting, reissues, the flag "stuck" and
  the card `governance_stuck`, `wrong_leaver` and the flags of the vote are removed (five rules of the vote,
  its count, its money and its stuck rounds and the single-model flag retired; see `conformance/retired.md`
  and `spec/removed.yaml`); one accuracy measure per lens, 1 - B / B_ref, with the calibration rule merged
  into it (COM-17); the threshold of scored forecasts for switching accountability on is the charter field
  `commission.accountability_min_forecasts` (20 by default, never below 8, D-116), and forecasts are scored
  from the first `apply` on v0.3; the audit sample is a uniform draw by a recorded seed (DIG-5); one running
  trial per field of an object (PRP-9); the farm manager is an advisory lens that takes no part in choosing
  who leaves (GOV-25); the necessity lens sees only borderline escalations (COM-13).
- Simpler money and less work beside the monitoring system (D-114): money is the one reserved unit (BUD-5,
  four meter values removed, a paid tool declares `billing: external` with `price_eur`); code reconciles an
  imported provider report with a release by time of unreconciled estimates and an overdue threshold set to
  the reconciliation rhythm (BUD-10); one formula for review money (BUD-13), actual usage per line in `doctor`
  instead of the price table (BUD-15), the workspace check only at `apply` without a daily probe (GW-9, the
  loop `provider_check` is gone), a provider rate limit as a wait without a limiter of our own (GW-10); the
  coverage lint of the manager line merged into MGR-49; volume quotas belong to the file system and the
  monitoring system (CAP-6), the core raises backup alarms only without the monitoring system (RET-8), and the
  per-tenant `doctor` lines are the alarm path (OBS-11).
- Simplification of v0.4 (D-114), reports to people and scripts: a model paragraph to people carries no
  numbers at all, only names and the digest's own dates (REP-4, new fixture `fx-rep-no-numbers`); a newer
  digest over the pace ceiling replaces the waiting one (REP-10); the schedule check before "Accept" keeps
  only validity and the disappearing and repeating local times (TIM-5); a script condition keeps `eq`, `ne`,
  `in` over closed fields and step states, while rollups and `items.from` selections are deferred (SCR-3, new
  deferred SCR-15, `condition-ops` version 2); the translator pause becomes the auto-hold of any role after
  three confirmed incident links (the new HLD-9, which replaces the translator's own pause); a person's copy
  of their data goes only to them, and the separate `/mydata` report and `principal.data_report` are dropped
  (SEC-10).
- D-114, the farm manager and the bot's memory and identity simplified: the manager has two limits
  (`ops_per_hour` and `hold_ttl_max`) and the safeguard after refusals in a row, without a separate pause of
  its money (the operation `P2.manager.resume`, `paused_in` and the notice `manager_spend_paused` are
  removed); the tenant's relation to the manager is the one flag `farm_manager.enabled`; escalation rates per
  worker and per tenant only; "preventable" (MGR-45, v0.3) is measured by code on the free-space series and
  covers each critical incident class of `docs/goals.md`, stating which ones the manager cannot prevent; facts
  pass to the successor by code, rechecked and marked inherited, without a choice task, a person or the
  `handover` purpose; the bot persona lint checks the file only; the personal assistant (DSP-10) is deferred,
  and from v0.2 `apply` rejects a role with `memory: person` (`person_memory_deferred`). Removed schemas,
  fields and registry values are listed in `spec/removed.yaml`.
- Owner decision D-114, the simplification of v0.4 begins: the goals and constraints of the farm in
  `docs/goals.md` (G1 to G6, C1 to C10, the five critical incident classes); every rule names the goal or
  constraint it serves in the new required field `goal`; 41 rules not needed before the pilot are deferred
  with a reason and a condition of return (`conformance/deferred.md`), their mechanisms are still in the
  schemas and are removed later; 14 rules merged into the rule with the same behaviour (each stays as a
  retired row in `conformance/retired.md` that names its target), and the closed registries whose entries
  named them got a new version.

### CI & Checks

- Checks for the simplification of v0.4: the skeleton version v0.4; the rule status `deferred` (the full row
  stays with the date, the reason, the condition of return and whether the mechanism was removed; no version
  checks it; while its mechanism is still in the skeleton, fixtures, cases, suite checks and texts may name it
  (a note), and once `mechanism_removed: true` nothing may name it outside the decision log and the changelog;
  generated list `conformance/deferred.md`); a rule may end without a replacement (`until`, empty
  `superseded_by`, `end_reason`); before the first release a removed schema, closed registry or registry
  value, or an incompatible schema change, is accepted when recorded in `spec/removed.yaml`; the check method
  `impl`: a Go test of the reference implementation (`impl:<folder>/<TestName>`) covers a rule in readiness.
- The `ci/` checks in Python and the workflow `.github/workflows/skeleton.yml`: the rule registry, file
  generation, text and links, schemas, manifests, evals, additive-only schema changes, public safety and
  version readiness. All 380 rules of v0.1 have a described check: data that the skeleton CI runs, data for an
  implementation's runner or eval, or a description of a suite check; readiness shows these three numbers
  separately (D-85 (5), D-86 (11)).
