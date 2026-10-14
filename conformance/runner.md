# Runner contract

The runner is an implementation's program that checks the core as a black box: it knows the core's address and the test bench key and knows nothing about how the implementation is built. The skeleton has no runner; this page records what the runner reads, how it executes fixtures and how it controls the test bench. The input, the output and the expected failures file are described in [docs/quality.md](../docs/quality.md), section "Runner"; a sample expected failures file is in `conformance/expected-failures.example.yaml`.

## What the runner reads

| Where | Form | Who executes it |
|---|---|---|
| `conformance/fixtures/schemas/valid`, `schemas/invalid` | `schema` and a list of `cases` with bodies that the schema accepts or rejects | the skeleton CI, `ci/check_schemas.py` |
| `conformance/fixtures/manifests/` | `cases` with manifests and the expectation `expect: ok` or a lint error code | the skeleton CI, `ci/check_manifests.py` |
| `conformance/fixtures/state-machine/` | lists of `allowed` and `forbidden` transitions generated from `spec/lifecycles/*.md` | the implementation's runner |
| `conformance/fixtures/authorize/table.yaml` | the behaviour fixture envelope, kind "decision table" (section [Decision table](#decision-table)) | the implementation's runner, with the test bench operation `authorize.eval` |
| the other folders of `conformance/fixtures/` | the behaviour fixture envelope, section below | the implementation's runner |
| `conformance/suites/` | descriptions of checks (what is given, what is done, what is expected) | the implementation's runner; the implementation writes the check code |
| `conformance/requirements.yaml` | rules with profile, version and the `waivable` flag | the implementation's runner, to select checks and for the report |

The runner takes the rule checks of the profiles the implementation claimed in `conforms_to`, and only rules whose `version` is not newer than the claimed skeleton version and whose `until`, when set, is not older than it: a rule with `until: v0.2` is checked for a v0.1 or v0.2 claim and skipped for a v0.3 claim, where the rules of its `superseded_by` take over (MAN-8, D-113). A fixture case whose `version` is newer than the claimed one is skipped by the runner and reported as skipped, not as passed.

## Behaviour fixture envelope

The form is set by the schema [fixtures/fixture.schema.json](fixtures/fixture.schema.json). A fixture is a YAML document holding a mapping:

| Field | Meaning |
|---|---|
| `id` | The name of the fixture or case; unique across all of `conformance/` and `evals/` (`ci/check_fixtures.py`). |
| `rules` | Numbers of the rules from `conformance/requirements.yaml` that the fixture or case checks. |
| `version` | The skeleton version from which the case applies, for example `v0.2`. Without the field the case applies from the rule's version. |
| `title`, `description` | Text for a human. `description` is required for a fixture. |
| `given` | The initial state of the test bench, section [Initial state](#initial-state). |
| `steps` | Steps in order, section [Steps](#steps). A fixture or case without `steps` checks what results from seeding `given`. |
| `expect` | The expectation after the last step, section [Expectations](#expectations). |
| `cases` | Cases. Each case is built like a fixture and may have its own `cases`. |

A case inherits the parent's `given`: the runner seeds the parent's `given`, then the case's `given`. A case without its own `steps` runs the parent's `steps` and checks the parent's `expect`. A key of the case replaces the parent's key of the same name, and in the `people` list a person's entry replaces the parent's entry about the same person. The runner executes every top-level case on a fresh test bench or after rolling the test bench back to the parent's seed; cases do not share state unless the case has an `after` field in `given` (then the case continues the state after the case with that `id`).

YAML comments at the start of a file explain the set of rows and carry no data.

## Initial state

`given` is the seed data. Closed keys:

- `tenant`: the tenant on whose behalf the steps run, by name (`demo`) or as an entry `{name, kind, ...}` with the fields of the tenant manifest;
- `people`: the people and principals of the test bench as a list `{principal, groups, caps, state, tenant}`; `groups` are groups of the tenant, `caps` are individual permissions from permission strings (`control.approve@demo`, `action.request:cleanup_tmp`), and `state` is the principal's state per the `state` field of the schema `spec/model/principal.schema.json` (`active`, `off`, `offboarded`).

The other keys (`manifest`, `manifests`, `holds`, `seat`, `campaign`, `task`, `workers`, `records` and others) name records and manifests that must exist before the first step, using the fields of their schemas in `spec/model/` and `spec/manifests/`. The runner seeds them by the ordinary paths: manifests through P2 `apply`, records through the P1 and P2 operations that create them, and time through the test bench port. The runner applies a `Tenant` manifest from `examples/` with `kind: production` on the test bench with `kind: test` and the other fields as they are: on the test bench all tenants are test tenants (`seed.yaml` works the same way; `docs/quality.md`, the test bench table, the row "Store"). Fields and kinds of a version later than the one the implementation claims (MAN-11) the runner removes before `apply`; the other fields stay as they are. The runner has no direct write access to the store. A record that cannot be created by an ordinary path (for example, a hold set by the core's auto-stop) the runner obtains through the action that produces it, and chooses the path itself.

The training people and programs are described in [evals/fixtures/seed.yaml](../evals/fixtures/seed.yaml): the tenants `farm`, `demo` and `demo2` (all `kind: test`), the people `alice`, `bob`, `carol`, `dave`, `erin`, and the program `report-client`. The groups from `seed.yaml` apply until `given` replaces them. The runner seeds people with the P2 calls `principal.add`, `principal.set_identity` and `group.*` using the values from `seed.yaml`: the principal id, the channel identity (`telegram:1001` for `alice`, `telegram:1002` for `bob`, `telegram:1003` for `carol`, `telegram:1005` for `dave`; `erin` has no principal record, and her identity is `telegram:1004`) and the groups. The runner binds the group chats from `seed.yaml` with P2 `chat.bind` using their external id (`external_id`); personal chats need no binding, they are checked by the person's identity (D-87 (3)), and the `Activity` body in a fixture carries the same values: the person's `sender.identity` and the chat's `chat.external_id`.

## Actor

The `by` field of a step and the `principal` field in `given.people` follow this grammar:

| Notation | Who it is |
|---|---|
| `human:<handle>` | A person from `seed.yaml` or `given.people` by training handle (`human:alice`). The runner substitutes the opaque principal id `human:p_<ULID>` from `seed.yaml` (the `id` field) or the one the core issued during seeding. Core records hold no handle; the core writes only the id (lead decision 1, `principal_ref` in `spec/model/common.schema.json`). |
| `human:p_<ULID>` | A person by id, if the fixture needs the id itself. |
| `service:<name>` | An external program with its own P2 credentials. |
| `bot:<id>`, `worker:<id>` | A bot identity or a test bench worker; P1 calls go with the token of that worker. |
| `attempt`, `attempt:<n>` | The current attempt of the fixture's task or the attempt with number `n`; the call goes under its lease. |
| `attempt:<type>` | An attempt of a task of this type, for example `attempt:data.describe.v1`. |
| `role:<name>` | An attempt of a worker with this role in the tenant `given.tenant`. |
| `channel:<name>` | A channel process; it acts on behalf of a person with the headers `Botaru-On-Behalf` and `Botaru-Chat`. |
| `seat` | The holder of the farm manager seat with the current epoch. |
| `system` | The core's service principal. |
| `operator` | The farm operator through `farmctl` (PRM-14). |
| `schedule`, `route`, `model` | A schedule, a route and a model as call sources. They have no permissions of their own; such steps check that a source without a permission is refused. |

A step without `by` is made by whoever naturally makes that operation: the channel for `submit_activity` and `decide`, the current attempt for operations under a lease, `operator` for P2 operations without a person.

## Steps

A step contains exactly one verb from a closed list, plus modifiers.

| Verb | What the runner does |
|---|---|
| `call: {port, op, request}` | Calls a port operation per the map `spec/ports/operations.yaml` (`port` is `P1`..`P4`, `C1`..`C4`, `E1`..`E5`). `request` is the body per the operation's request schema. C and E operations that are off the wire (inside the core) the runner checks through the observable path that invokes them, for example C2 `authorize` through a call that needs a permission, or through P2 `dispatch.explain`. |
| `stand: {op, ...}` | A test bench action through port T, section [Test bench](#test-bench). |
| `model: {...}` | What the mock model behind P4 returns for the next `model.call` in this case (`text`, tool calls, other response fields), per `spec/bindings/model-providers/mock.md`. In tests without a model, the model at once does what an attacker would want. |
| `lint: {...}` | Manifest fragments (`tenant_yaml`, `role_yaml`, `deploy_yaml` and others) that the runner submits to P2 `apply` on top of the seed; the expectation is usually `stage: lint` or `stage: apply`. |
| `occur: {...}` | A core event that the fixture states as an outcome rather than a call, for example "a child task closed" or "an incident of a class opened". The runner brings it about with ordinary P1 and P2 calls and test bench actions; the runner chooses the path. |
| `check: ...` | An observation point without an action: what the runner reads (a record, a delivery, an attempt outcome) before comparing the step's `expect`. |

Step modifiers:

- `by`: the actor, section [Actor](#actor);
- `chat`: the chat from which the person acts;
- `card`: the card kind from `spec/model/decision-points.yaml` whose button made the step;
- `on_behalf`: the identity that the channel passes in the `Botaru-On-Behalf` header instead of the identity of the person in `by` (for example `telegram:1001` for the actor `channel:<name>` with a different driver); without the field the runner takes the person's identity from the seed;
- `message`: the channel message for the `Botaru-Message` header of a button press (D-86 (1)); without the field the runner takes the `external_id` of the delivery of this card to this chat, and `other` means another message of the same chat;
- `credential`: foreign or invalid credentials instead of the actor's usual ones (`worker_token:w_m1`);
- `lease`: the lease token that the runner sends in the `Botaru-Lease` header (`spec/bindings/http/README.md`): a token object (`epoch`, `work_id`, `lease_no`, `worker_id`) or the handle `current` (the actor's current lease) or `stale` (a previous lease of the same work); fields missing from the object the runner takes from the actor's current lease (so `lease: {seat_epoch: 9}` means a token with an old seat epoch); without the field the runner takes the actor's current lease, and the request body carries no token. If the core signs tokens (`key_id` and `sig` are optional, D-87 (5)), the runner does not compute signatures: for a `lease` object it picks, from the tokens the core issued in this run (`claim` responses and leases from `given` that the test bench seeded through the core), the one whose fields match; for an unsigned token the runner assembles it from the fields. If there is no signed token with such fields, the runner sends the fields of the object with the signature of the actor's current lease; only steps that expect `lease_mismatch` are written this way (a forged field or a foreign `worker_id`, D-86 (2)), and with any other expectation this is a fixture error;
- `context`: conditions of the step that are not in the request body: fields of the attempt, the task, the actor or a record (the initiator, the task type, untrusted text in the context, a data mark, the call path `via: farmctl`). The keys name fields of the schemas in `spec/model/`; the runner establishes them before the step through the seed and earlier steps, like the keys of `given`, and does not put them into the request body;
- `at`: when the step is made, as a UTC instant (`2026-10-12T03:30:00Z`) or an offset from the start of the case (`7d`); the runner sets the clock with the action `clock.set` or `clock.advance` before the step;
- `note`: an explanation for a human;
- `expect`: the expectation right after this step.

### Body of a call step

`request` is the body per the request schema of the operation from the map `spec/ports/operations.yaml` (the `request` field of the row), and the skeleton CI checks every body against that schema (lead decision 11 of the review of 2026-10-12). The check allows only the notations of this section and of the section [Short ids and references](#short-ids-and-references): a short id, a reference, a training handle `human:<handle>` and an omitted `nonce`. A step whose expectation contains `stage: schema` or `reason: input_schema_invalid` deliberately sends a body outside the schema, and the check skips it. What is not part of the body (who calls, under which lease, in which attempt context) is written with the modifiers `by`, `lease` and `context`. An event that a fixture states as an outcome rather than a body is written with the verb `occur`, not `call`. A card button that is an operation per `spec/model/decision-points.yaml` (for example, "Roll back" on `rollback_card` is P2 `hold.set`) is written as a call of that operation with the `card` modifier.

### Body of decide

The body of P1 `decide` is written with the fields of the schema `p1-decide-request`: `subject_kind`, `id`, `nonce`, `decision`, `reason_code`. Who pressed and from which chat are set by the modifiers `by` and `chat`, not by the body. A fixture omits the `nonce` field if it is not checking it: the runner takes the one-time value from the delivered card. The value `$stale` means the one-time value of a previous version of the same card. The bodies of P1 `card.details`, `card.remind`, `campaign.control` and `script.control` are written the same way.

### Short ids and references

Ids of core objects in fixtures can be shortened to the form `<prefix>_<number>` (`i_311`, `t_501`, `h_auto_1`, `bot_41`). The runner expands a short form to an id per `object_id` (prefix, `_`, ULID) in the same way throughout a case: one short form means one object. The hyphen form (`i-311`) is not used. The runner passes full ids `t_01JB...` as they are.

In the id field of a body (`id` of `decide`, `card.*`, `campaign.control` and `hold.release`, `file_ref` of `file.get`, the elements of `handles` of `msg.ack` and `msg.nack`) a reference can be written:

- `$last`: the last subject of the kind the field points to that was created or issued in this case: the kind is the `subject_kind` of the body, and without it the kind of the operation (a hold for `hold.release`, a campaign for `campaign.control`, a file for `file.get`, the handle of an inbox entry issued to the actor for `msg.ack` and `msg.nack`);
- `$from:<case id>`: a subject of the same kind created in the case with that `id`.

References to files are written in the full `file_ref` form (`file:` and 22 characters); a training value in a fixture links the step with a file from `given`.

## Expectations

An expectation is a mapping of data; there is no bare string. Closed fields:

| Field | Values |
|---|---|
| `result` | `ok`; `rejected` (a refusal by a rule or an input check); `denied` (a C2 permission refusal). |
| `reason` | A code from `spec/model/reasons.yaml`: the `reason` member of a problem details response, the reason an attempt ended, or the reason of an outcome. `reason` without `result` means a refusal or an outcome with this reason, and the kind of refusal does not matter to the fixture. |
| `stage` | Where the refusal happens: `schema` (the schema of the body or manifest), `lint` (the manifest lint), `apply` (applying the set), `hold` (holds in the C2 decision order), `ban` (an unconditional ban in the C2 decision order). |
| `outcome` | An outcome that is neither `ok` nor a refusal: `staged`, `enqueued`, `skipped`, a trial result and others. |
| `state` | The state of the subject after the step per its lifecycle in `spec/lifecycles/`. |
| `error` | A stable lint or `apply` error code for a refusal with `stage: lint` or `stage: apply`; the lint codes of the skeleton's manifests are listed in `ci/lint_codes.py` (table in `ci/README.md`). |
| `note` | An explanation for a human; the runner does not compare it. |

The other keys are observable facts: the name of a record, field or counter, and the expected value (`tasks_created: 1`, `approval: {state: pending}`, `deliveries: 0`). The runner compares a fact against the record, event, audit entry or operation response of the same name. A fact the runner cannot check it marks in the report as unchecked, not as passed; a case with an unchecked fact does not count as covering the rule.

## Test bench

A test bench is a farm whose store was created as a test bench store from the first start, whose deployment settings set the test bench key, and which never had a tenant other than `kind: test`, including a retired one (APR-9, lead decision 4 of the review of 2026-10-12). The service tenant `farm` on a test bench is also `kind: test`. An empty farm is not a test bench: the store sets the flag, not the list of tenants. Everything that cannot be done through P1 and P2 the runner does through the test bench port [spec/ports/test-control.md](../spec/ports/test-control.md) (lead decision 4): a separate core listener, the path `/v1/test/<operation>`, the test bench key in `Authorization: Bearer`. A production farm has no such port, and a core with a test bench key on a store without the test bench flag does not start; a fixture that needs the `stand` verb is not executed on such a farm.

| Action `stand.op` | Arguments in the fixture | Request schema (a core test schema, not a port contract) |
|---|---|---|
| `clock.set` | `at`: a UTC instant; the clock only moves forward | `test-clock-request` |
| `clock.advance` | `by`: a duration (`15m`) or the name of a period from a manifest or a rule (`lease_ttl`, `bake`), which the runner converts to seconds `by_s` | `test-clock-request` |
| `loop.tick` | `loop`: a loop name from `spec/ports/background-loops.md`; `copies: 2` and `concurrent: true` for two ticks at the same time; other fields (`point`, `day`) specify which deadline of the loop has come | `test-loop-tick-request` |
| `process.stop` | `process`: a test bench worker or the core; `signal: 9` means `kill`, without the field `graceful`; `during` and `item` name the moment of the stop | `test-process-stop-request` |
| `store.fault` | `point`: `before_commit`, `unavailable`, `low_space` or `clear`; `count` for `before_commit` | none: no fixture uses it yet (D-114) |
| `store.snapshot` | `label`: the backup label; the backup is taken in the same way as by the external backup process (RET-5) | `test-store-snapshot-request` |
| `store.restore` | `label`: the backup label; the restore follows the real path: the store epoch grows, and a restore record is written into the `farm` chain (RET-6, AUD-6) | `test-store-restore-request` |
| `authorize.eval` | `request` (an `authz-request` body) and `tenant_kind` (`test` or `production`, `test` by default); the response is an `authz-decision` without a record and without side effects | the `authz-request` of the row |
| `function.eval` | `function`: the name of a pure core function; `input`: its input | `test-function-eval-request` |
| `event.inject` | `event`: an incoming `Activity` of a test bench channel or monitoring adapter; `via`: the process name | none: no fixture uses it yet (D-114) |

The test bench port seeds no data and does not change records of tasks, decisions, principals or manifests; `authorize.eval` and `function.eval` take their input in the request and write nothing: seeding goes through P2 `apply` and P1 and P2 operations. The mock model sits behind P4 per `spec/bindings/model-providers/mock.md`, not behind the test bench port.

## Decision table

A decision table is a kind of behaviour fixture that checks one function of the core: each case sets the input of the function (`given`, a `call` or `lint` step) and its expected decision (`expect`). The runner executes a `call` step of C2 `authorize` with the test bench operation `authorize.eval`: the step body goes into the `request` field, and a `lint` step goes through P2 `apply` with `dry_run`; the table leaves no records and no side effects, it has no `stand` steps, and the cases do not share state. The production tenant kind of a row (`kind: production`; the kind is written in the file header or in the entry `given.tenant`) is passed by the runner in the `tenant_kind` field of the `authorize.eval` request instead of creating a production tenant, so rows that depend on the production tenant kind (for example, the self-approval ban, APR-9) are checked this way, and the APR-9 test bench restriction is not violated. In v0.1 there is one decision table: [fixtures/authorize/table.yaml](fixtures/authorize/table.yaml), for the C2 function `authorize` and the `apply` lint (PRM-2). From v0.2 the runner executes the decision tables of pure core functions with the test bench operation `function.eval`: the case's `given` without `tenant` goes into the `input` field, and the `output` is compared with the case's `expect`. These tables are [fixtures/commission/com-05.yaml](fixtures/commission/com-05.yaml) (`commission.accuracy`, COM-17, from v0.3) and [fixtures/reporters/fx-rep-no-numbers.yaml](fixtures/reporters/fx-rep-no-numbers.yaml) (`reporter.number_check`, REP-4). The behaviour fixtures `dig-06` and `fx-time-schedule-proposal-check` keep their steps; the runner may also check their pick and their disappearing and repeating times with `function.eval` (`digest.sample`, `schedule.check`). The kind of the fixture and the function are written in a comment at the start of the file.

## Report and expected failures

The runner writes a JUnit XML report: one test per fixture case or suite check, with the rule number and the case `id` in the test name. The result per profile is computed from the rules of the claimed profiles. An unexpected failure, an entry in the expected failures that started to pass, and a non-waivable rule (`waivable: false`) in the expected failures file fail the run (`docs/quality.md`, section "Runner").
