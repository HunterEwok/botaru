# impl: the reference implementation in Go

This folder holds the reference implementation of the botaru core (D-91, D-92 in
[docs/decisions.md](../docs/decisions.md)). The implementation claims skeleton 0.3 (unreleased: `spec/VERSION` is 0.3.0
and there is no tag yet) with the profiles `core`, `monitor`, `insights`, `governance`, `manager`, `messaging`,
`campaign`, `knowledge` and `ingress`; the eval rules of the claim are not run (see Checks, "Eval rules not run"). It
runs the first bot: the hello-bot sequence of [examples/hello-bot/](../examples/hello-bot/) works end to end over the
wire, from a Telegram message to the model answer in the same chat. How to run it on a server: [DEPLOY.md](DEPLOY.md).
How each subsystem behaves: [ARCHITECTURE.md](ARCHITECTURE.md).

## Rules for this folder

- The implementation does not change skeleton files outside `impl/`. A change of a contract goes through the skeleton
  rules: additive only inside a version (MAN-3), a new schema file `.v(N+1)` for a breaking change.
- It passes the `conformance/` suite and the evals of the skeleton; a rule with `waivable: false` cannot go into its
  list of expected failures.
- The agent runtime is our own loop over P4 (D-93); conversation memory, sessions and transcript compression live in the
  core stores.
- Money is an integer number of micro-euros `eur_micros` (D-87), never a floating-point number.
- Builds are static (`CGO_ENABLED=0`) for linux/amd64 and linux/arm64. Installation files (tenants, keys, machines,
  people) never go into this repository.
- Every Go file starts with `// SPDX-License-Identifier: AGPL-3.0-only`.

## Programs

| Command | What it is |
|---|---|
| `botaru-core` | the core: the P1 listener with the P4 path, the separate P2 listener, the background loops (reaper, deadlines) over a PostgreSQL store |
| `farmctl` | the operator's tool for P2: `apply`, people (including `principal offboard`), memberships, `worker enroll`, `health`, `audit verify` |
| `botaru-agent` | the agent worker of one role (E4 adapter and its own runtime loop, D-93); calls the model only through P4 |
| `botaru-telegram` | the E1 Telegram channel of one farm channel: `getUpdates` polling and `sendMessage` deliveries |
| `botaru-toolhost` | the E3 tool execution adapter of one machine and tenant: claims tool calls over P1 and runs each in a fresh child process of its plugin |
| `botaru-kbjob` | the ingest job of the knowledge corpora (`kb.ingest.v1`, profile `knowledge`): a job worker without a model that reads a corpus document with P1 `file.get`, cuts it into fragments and writes them with P1 `kb.index_put` |
| `botaru-zabbix` | the E5 monitoring adapter of one tenant for Zabbix 7.0 and later: alarm and audit-head export, tick, problem webhook, script lists, script runs and code notes |
| `botaru-ingress` | the webhook driver of one source channel (profile `ingress`, ING-1 to ING-4): a TLS listener under its own OS user and secrets that checks the signature or token, the limits and the delivery id, then puts the body as a file and submits the mapped event over P1 |
| `botaru-plugin` | the own plugins `cmd`, `logs`, `checks`, `service`, `queue` and `monitor` as MCP servers on the standard streams; the toolhost starts them |
| `botaru-eval` | the eval runner: stands up a test bench in its own process, runs the cases of `evals/cases/` with the mock model or a real provider, writes JUnit and EvalReports entries (see [EVALS.md](EVALS.md)) |
| `botaru-mockmodel` | the mock model provider of the test bench (`spec/bindings/model-providers/mock.md`) |

## Packages

| Package | What it holds |
|---|---|
| `internal/ijson`, `internal/jcs` | strict I-JSON parsing (RFC 7493) and the canonical form with hashes (RFC 8785, `spec/hashing.md`) |
| `internal/spec` | the skeleton read at run time: schemas of `spec/model` and `spec/manifests`, registries |
| `internal/store` | the C1 store on PostgreSQL: migrations, the store clock `botaru_now()`, transactions |
| `internal/core` | the operations of P1 and P2 over the store, with the permission check in the transaction of the transition |
| `internal/api` | the HTTP binding v1: paths, credentials, problem details, the operation map |
| `internal/gateway` | P4 with the Anthropic Messages binding: allowlist, model of the slot, reserve and charge |
| `internal/mcp`, `internal/toolspec`, `internal/redact` | MCP messages and the newline-delimited stream; ToolPlugin manifests (tool names, input schemas, the `tools/list` fingerprint, the checks of program commands); masking and cutting of tool output |
| `internal/toolhost`, `internal/plugins` | the toolhost and the own plugins |
| `internal/ingress` | the webhook driver: Standard Webhooks signatures, the TLS token check, rate and body limits, the mapping of closed fields |
| `internal/pluginmain` | the flags and start of the own plugins, shared by `botaru-plugin` and `botaru-eval plugin` |
| `internal/evals` | the eval runner: cases, plan and estimate, generators, seed, bench, checks and reports |
| `internal/agent`, `internal/channel/telegram`, `internal/kbjob`, `internal/p1client` | the workers and their P1 client |
| `internal/e2e` | the hello-bot sequence and a plugin tool call over the wire with the mock provider, a fake Bot API server and a real toolhost |
| `internal/conform` | the conformance runner (`conformance/runner.md`): it runs the behaviour fixtures on fresh test benches over HTTP and reports per case and per rule |

## Key implementation decisions

These are decisions of this implementation where the skeleton leaves a choice; none changes a contract. The behaviour of
each subsystem, with the rules it supports, is in [ARCHITECTURE.md](ARCHITECTURE.md):

| Section of ARCHITECTURE.md | What it covers |
|---|---|
| Core, Store & Clock | the store clock, the skeleton at run time, defaults, the attempt record, the set lint, health, core keys |
| Dispatcher, Leases, Holds & Alarms | dispatch order and ceilings, leases, holds, alarms, children and continuations, schedules, programs on P1 |
| Tools (P3/E3), Plugins & Output Redaction | the tool catalog, plugin checks, the toolhost, output masking, own plugins, the tool cache, service control |
| Model Gateway (P4), Providers & Money (`eur_micros`) | providers and their pairs, fallback profiles, terms and regions, budget lines, reconciliation |
| Channels, Cards, Commands & Outbound Privacy | texts and locales, outbound privacy, approvals and cards, commands, group chats, messages, reporters, ingress |
| Sessions, Memory, Knowledge & Erasure | sessions, facts, files, knowledge corpora and skills, erasure, retention, export |
| Monitor (E5), Incidents & Farm Manager | the monitoring export and adapter, incidents, the farm manager seat, control operations, grants, escalations |
| Observations, Proposals, Commission & Governance | observations, analysis, commission, digest, trials, identities and seats, the quality gate, accountability |
| Campaigns, Scripts & Translator | campaigns, scripts, the translator and its eval gate, script links and the role hold |
| People, Rights and P2 Views | operator credentials, invitations, privileged groups, task and object views |
| Workers and Deployment | packaging, the deployment description, secrets, drain, backups, restore, the tenant lifecycle |
| Evals and the conformance runner | the eval runner, the test-control port, the conformance runner |

### Store

PostgreSQL 16 or later through pgx is the C1 store. The store clock is `botaru_now()` (the database time plus the test
bench offset), so every deadline is counted by one clock (TIM-1). All core state is in the store per transaction, and
the permission check of an operation runs in the transaction of its transition. The skeleton is read at run time from a
checkout (`spec_root`, or the folder the program runs in), not copied into the binaries, so the schemas the core checks
are the ones in git.

### Runtime

The agent runtime is our own loop over P4 (D-93): `botaru-agent` calls the model only through P4 with the handle of its
attempt, and keeps no working files, caches, transcripts or logs with content after an attempt. The result schema
reaches the runtime as the extension field `payload.result_schema` of the attempt (MAN-3), so the instruction stays
exactly the trusted text from git. Sessions and memory are core stores; the runtime gets skills only as files (SKL-2).

### Secrets

Worker tokens, handles and driver secrets are stored as HMAC-SHA256 with the core token key; lease tokens are signed
with the same key (`key_id`, `sig`). Operator credentials are scoped to one tenant each, because the bodies of the P2
people operations carry no tenant (TEN-2), and the settings file holds only the SHA-256 of each credential. Token keys
rotate with an overlap (SEC-5), and `secret.revoke` holds every scope a secret reaches (SEC-6).

### Versions, profiles and deferred rules

The core claims skeleton `v0.3` (`spec.Version`) with the profiles of `spec.Profiles` (MAN-8). P2 `status` shows the
claim as `conforms_to`: `skeleton` is "0.3.0", the number of `spec/VERSION`, although that release is untagged
(unreleased), and the profiles are those the installation claims, so `monitor` only with the monitoring system
configured (D-88).

- `apply` refuses with `version_unsupported` and the path a kind whose `since` is later, a field marked with a later
  `x-botaru-version`, the fields of the unclaimed profiles `connections`, `debug` and `sandbox` and tier 2 (MAN-11), and
  the kinds and fields of deferred rules.
- `spec.Claims` reads `spec.Profiles`; a test sets the exact claimed list with `spec.SetProfileOverride` (the list
  replaces the claim, it does not add to it; an empty call goes back to `spec.Profiles`).
- `spec.Ahead`, the list of fields built ahead of the claimed version, is empty: every v0.2 and v0.3 field the build
  serves is of the claimed version, and the core runs the v0.3 forms only. The Catalog fields `owner` and `terms` and
  the ModelProfile field `forced_schema` are accepted as descriptive fields of the claimed version; the Catalog field
  `role_incident_hold` (HLD-9) and the hold scope `sink` are of the claimed version.
- The example tenant and channel may carry fields the core refuses; the test bench and `farmctl apply -drop-unsupported`
  leave them out, the second with a warning for each. Only a whole optional field is left out. A later kind, a required
  field, a list element, or a value that gives a field its meaning (an enum or const value of the field, such as a
  channel direction, a catalog resource kind, a placement, a tier, the sink driver `webhook` or a `commission.shadow`
  other than `off`) stops with an error that names the path, because the manifest would then say something else.

Deferred rules (D-114, MAN-11): `spec.Deferred` (`internal/spec/deferred.go`) lists, for every rule of
`conformance/requirements.yaml` with `status: deferred` whose mechanism is still in the skeleton, the surface by which a
caller could reach it: a manifest pointer (kind and JSON pointer, with a value when only that value is deferred), a P1
or P2 operation or a field of its body, a built-in tool, or a field of the P1 `hello` body. A rule without a surface has
an entry with a note that says why (for example STA-3, which `status.workers` simply does not show, or RET-9, whose
retention fields stay served for RET-10). `spec.Offered(rule)` is false for every rule in the list.

- `apply` refuses a deferred kind or field with `version_unsupported` and the path; P1 and P2 refuse a deferred
  operation before its body schema and a deferred body field after it, with `version_unsupported` and a detail naming
  the path and the rule.
- P3 refuses a call of a deferred built-in tool (`link_hint`, `skill_read`) with `error: version_unsupported`, and a
  deferred argument field of a served tool is refused by the tool in its own outcome (`propose_campaign` rollout `waves`
  and `rate_per_min`, ORC-15; `control_ops` `pin_task`, MGR-12, also on P2 `control.apply`); `hello` with
  `runtime.skills: farm` is refused (SKL-4).
- A rule whose mechanism is removed (`mechanism_removed: true`) has no entry, since nothing may name it.
- A Go test checks that every deferred row has an entry, that every entry names a deferred row, and that its operations
  and tools exist in `spec/ports/operations.yaml` and `spec/model/builtin-tools.yaml`.

### Money

Every amount is an integer of micro-euros (`eur_micros`, D-87); a card shows euro cents. Every usage line and reserve
carries the budget line of the type's purpose, and a reserve is taken before a model or tool call and settled after it
(BUD-1, BUD-2). The model gateway reconciles its estimates with the provider's daily cost report (BUD-10).

## Not in this version

The following parts of the claimed rules are not built yet. The deferred rules of `spec.Deferred` are refused as
described above and are not repeated here, except where a built mechanism stops short because of them.

Core, ports and people:

- P2 on behalf of a person: P2 serves operator credentials only, so the person operations on P2 (holds of one's own
  scopes and their rank rules, PRM-15; incident decisions) are not built, a channel credential has no P2 path, and the
  PRM-14 refusal `operator_only` of a channel credential has none either. P2 with a channel driver's secret on behalf of
  a verified person (IDN-7) needs the driver's P2 secret, the binding of the verified person and the person checks of
  each operation. A P2 operation the core does not serve answers `404 not_found`.
- Streaming on P4 (a request with `stream: true` is refused).
- The built-in tools `messages_digest`, `proposals_query`, `incident_view`, `incident_close`, `file_read` and
  `escalations_take` are not served.
- The `sandbox` placement and tier 2 (the profile `sandbox` is not claimed), and plugins with secrets or network egress
  other than the own `monitor` plugin: such a plugin runs in its own container, which this build does not start (DEP-7).
- A pause of a schedule (TIM-5): `Schedule` has no pause field.
- The chat command `/invite`, and the notice `approve_unread_digest` of the excess cards (APR-13); merging a daily
  digest of preferences into one message.
- A `proposal.rejected` event with `manager_restricted` for the dropped proposals of a restricted seat (MGR-47, D-118).
- Attempt journals (AUD-4, profile `debug`) and the purge of `runtime_internal` sessions by period (RET-11).

Monitoring:

- Services with `via: monitor`, and the reconciliation of a service state after a service action (MON-8).
- A route task with a model for a monitoring problem, the history number of the catalog item, and a card for a
  monitoring script after untrusted text.
- The free space series of a volume written by a `job` role from the monitoring system history (CAP-6):
  `spec/model/core-series.yaml` names no such series, so it is a tenant's `ObservationSeries` with a job source, and
  this build has no job program that reads the monitoring history.
- A monitoring line in `status` (OBS-11: the status schema has none, the account line is in `doctor`).

Privacy, export and connections:

- People's connections (SEC-7, the profile `connections` is not claimed), so `secret.revoke` with `connections`, the
  revocation of connections at `tenant.offboard` and the exclusion of connection records from an export have nothing to
  act on (the bulk form answers `not_found`).
- The period of class `content` for a `self` sink, and the content of files in an export (only their records).

Deployment (DEP-10):

- The agent process kind (the schema has none, so DEP-5, the agent's network, is not linted; the compose agent is on the
  core network only).
- The comparison of the OS mechanism of the machine with the catalog (DEP-4 at doctor), plugin image digests and the
  docker group or socket (no schema fields), and the monitoring system's host against the core machine (no host address
  in the schema).
- The reading of the workspace ceilings at the provider by doctor (GW-9: the provider binding has no such call; apply
  checks the ceilings the settings describe).

Governance and the farm manager:

- The function `commission.accuracy` of the test-control port (the accuracy of COM-17 is computed from the scored
  forecasts in the store, not by a pure function, so `function.eval` answers that this build has none).
- The GOV-46 eval (an eval rule, not run), and the deferred PRP-25, COM-18, COM-10 and the GOV-39 hints, which leave
  their parts of the accountability rules without input.
- The parts of the farm manager, promotion and attribution that ARCHITECTURE.md lists as not built in their sections.

Checks and tools:

- The PRM-13 raise check in the skeleton lint: `ci/check_manifests.py` lints one state of the git and has no stored set
  to compare with; the core checks it at `apply`.
- A registry of programs whose output is text from outside (INJ-14 names the journal as its example).
- The lint of FEN-7 for side effects without an idempotent key (the core cannot see an external write).
- The stop of a toolhost or worker process by the conformance runner (it runs no worker processes).

Webhook source channels (ING-1 to ING-4): the driver's heartbeat health (OBS-10), a conformance runner case for the
ingress adapter suite (the Go tests of `internal/ingress` and `internal/e2e` cover it), route `deliver_to` entries in
map form, and a delivery-id format check other than the body hash for a TLS token source.

Of the eval runner these are not built: the roles other than `hello` and `ops-reader` on the bench, inbox, system, task
and group chat inputs, the `monitor` and `corpus` injection carriers, follow-ups that need the store clock to move,
`drift` and `rate` checks, parallel cases, and the judge. A case that needs one of them is reported `not_run` with the
reason (EVALS.md).

## Checks

The workflow `.github/workflows/go.yml` runs on every pull request. Locally, from this folder:

```sh
gofmt -l .
go mod tidy -diff
go vet ./...
go tool staticcheck ./...
go test -race ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
go tool govulncheck ./...
```

The `go` command downloads the toolchain named in `go.mod` by itself (`GOTOOLCHAIN=auto`). staticcheck and govulncheck
are tool dependencies in `go.mod`, so their versions are pinned and Dependabot updates them.

The store tests need PostgreSQL: either `BOTARU_TEST_DSN` names a server where the tests may create and drop databases,
or `initdb` of PostgreSQL 16 or later is installed (in `PATH` or `/usr/lib/postgresql/*/bin`) and the tests start a
private server; they refuse to do that as root. Without PostgreSQL the store tests are skipped, unless
`BOTARU_TEST_PG_REQUIRED=1`, as in CI.

### Rule coverage

`TestConformance` in `internal/conform` runs every behaviour case of `conformance/fixtures/` whose rules are within the
claim of the core (`spec.Version` v0.3 and the profiles of `spec.Profiles`) on the reference core: each case on a fresh
test bench (a new database, the core with its P1, P2 and test-control listeners), reached only over HTTP.

- A case is passed, failed, skipped (a later version) or unchecked: it needs a given key, a step, an operation or a fact
  that the runner or the core cannot do yet, and the report names the first such need. An unchecked case never counts
  for its rule. A rule is done when every case that names it passes.
- `testdata/passing.txt` lists the passing cases: the test fails when a listed case stops passing, and when a case
  starts to pass without being listed (record it with `go test ./internal/conform -run TestConformance -update`).
- Decision tables run by the comment at the start of their file: a C2 `authorize` row of the permission table goes to
  `authorize.eval` with the table's tenant kind, the example task types its steps name added to the seed, its given
  narrowed to the people its steps name (the table-wide `roles`, `catalog` and `system` belong to the tool rows) and
  `given.budget.exhausted` passed as the row's budget state; a table of a pure function goes to `function.eval` with its
  given without `tenant` and the mock model's `text`, and each expected fact is compared with the output field of the
  same name (a fact the output lacks leaves the case unchecked). `TestFunctionTables` runs the function tables at a
  claim of v0.3.
- With `BOTARU_CONFORM_OUT=<folder>` the test writes `junit.xml` and the coverage table `coverage.md`; CI puts the table
  into the job summary.
- A case that names a rule whose `until` is older than the claim (D-113: its behaviour was replaced by the rules of its
  `superseded_by`) still runs with its other rules; when it fails, it is reported unchecked with the detail "names ended
  rule <ID>", since its expectation may be the ended rule's.

The run counts 74 passed, 0 failed and 1212 unchecked cases, none skipped as later. Among the passing cases are the six
`fx-rep-nn-*` cases (REP-4, numbers in a model paragraph) and `gov-fx-24-c` (a shorter bound applies). `gov-fx-24-b` (a
farm charter with `decide_within` above 7 days is refused, GOV-22, until v0.2) is unchecked, since the core does not
lint `incident_stop` (MGR-55). The cases of the profiles the core does not claim (`connections`, `debug`, `sandbox`)
pass none. The two lint cases `fx-man-com-v02#1` and `#2` (the COM-10 shadow block) are not in the lint list of
`internal/core`: the core refuses the block with `version_unsupported` before its lint, and the fixture format has one
expected code per case for the skeleton lint and the core alike, so the case cannot expect the refusal without failing
the skeleton lint, which still lints the block.

### Eval rules not run

The claim is 0.3 (unreleased), and no paid eval run was made for it: every rule of the claim with `check: eval` in
`conformance/requirements.yaml` is "eval not run", whatever its other checks show. There are 22: GOV-13, GOV-29, GOV-45,
GOV-46, INJ-12, INJ-15, KNW-2, MGR-4, MGR-6, MGR-14, MGR-17, MGR-22, MGR-23, MGR-26, MGR-52, MSG-7, ORC-9, REP-2, REP-4,
REP-8, TRN-5 and TRN-6. The mock and scripted-provider bench runs of `internal/evals` check the runner, not the models.
