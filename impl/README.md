# impl: the reference implementation in Go

This folder holds the reference implementation of the botaru core (D-91, D-92 in [docs/decisions.md](../docs/decisions.md)). Version 0.1 of the implementation runs the first bot: the hello-bot sequence of [examples/hello-bot/](../examples/hello-bot/) works end to end over the wire, from a Telegram message to the model answer in the same chat. How to run it on a server: [DEPLOY.md](DEPLOY.md).

## Rules for this folder

- The implementation does not change skeleton files outside `impl/`. A change of a contract goes through the skeleton rules: additive only inside a version (MAN-3), a new schema file `.v(N+1)` for a breaking change.
- It passes the `conformance/` suite and the evals of the skeleton; a rule with `waivable: false` cannot go into its list of expected failures.
- The agent runtime is our own loop over P4 (D-93); conversation memory, sessions and transcript compression live in the core stores.
- Money is an integer number of micro-euros `eur_micros` (D-87), never a floating-point number.
- Builds are static (`CGO_ENABLED=0`) for linux/amd64 and linux/arm64. Installation files (tenants, keys, machines, people) never go into this repository.
- Every Go file starts with `// SPDX-License-Identifier: AGPL-3.0-only`.

## Programs

| Command | What it is |
|---|---|
| `botaru-core` | the core: the P1 listener with the P4 path, the separate P2 listener, the background loops (reaper, deadlines) over a PostgreSQL store |
| `farmctl` | the operator's tool for P2: `apply`, people (including `principal offboard`), memberships, `worker enroll`, `health`, `audit verify` |
| `botaru-agent` | the agent worker of one role (E4 adapter and its own runtime loop, D-93); calls the model only through P4 |
| `botaru-telegram` | the E1 Telegram channel of one farm channel: `getUpdates` polling and `sendMessage` deliveries |
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
| `internal/agent`, `internal/channel/telegram`, `internal/p1client` | the workers and their P1 client |
| `internal/e2e` | the hello-bot sequence over the wire with the mock provider and a fake Bot API server |
| `internal/conform` | the conformance runner (`conformance/runner.md`): it runs the behaviour fixtures on fresh test benches over HTTP and reports per case and per rule |

## Implementation choices

These are decisions of this implementation where the skeleton leaves a choice; none changes a contract.

- **Store.** PostgreSQL 16 or later through pgx. The store clock is `botaru_now()` (the database time plus the test bench offset), so every deadline is counted by one clock (TIM-1).
- **The skeleton is read at run time** from a checkout (`spec_root`, or the folder the program runs in), not copied into the binaries, so the schemas the core checks are the ones in git.
- **Operator credentials** are scoped to one tenant each, because the bodies of the P2 people operations carry no tenant (TEN-2). The settings file holds only the SHA-256 of each credential.
- **Secrets.** Worker tokens, handles and driver secrets are stored as HMAC-SHA256 with the core token key; lease tokens are signed with the same key (`key_id`, `sig`).
- **The result schema reaches the runtime** as the extension field `payload.result_schema` of the attempt (MAN-3), so the instruction stays exactly the trusted text from git; the runtime appends the request for a JSON answer to the system prompt.
- **Code texts** come from the skeleton message catalog `spec/bindings/channels/messages/en.yaml`, read at run time like the other skeleton files (DLV-9); other locales are `<locale>.yaml` files in the installation's text folder, and a key they lack takes the `en` text. The recipient's locale reaches the channel as the extension field `payload.locale` of a delivery; without it the channel takes `en` (the tenant's `locale` is a field of v0.2). Code fills the values of `notice.ai` (the tenant, and the operator from the deployment setting `operator`) and of `notice.privacy` (the tenant's privacy block and the default periods of `spec/model/retention-classes.yaml`); a value the installation does not set gets a fallback (the operator: "the operator of this farm"; the controller: the tenant name; the contact: the notice link; anything else: "-"), and the core logs a warning for each at start and after `apply`. The channel renders a parameter it does not get as "-", so a person never sees a raw placeholder. Notices planned by the first version of the implementation get the new keys from store migration 0002 and their new values from the core at start.
- **Later versions and profiles.** The core claims skeleton `v0.1` with the profile `core` (`internal/spec`). `apply` refuses with `version_unsupported` and the path a kind whose `since` is later, a field marked with a later `x-botaru-version`, and the fields of the profiles `knowledge`, `ingress`, `connections`, `debug` and `sandbox` and tier 2 (MAN-11). The example tenant and channel carry such fields; the test bench and `farmctl apply -drop-unsupported` leave them out, the second with a warning for each. Only a whole optional field is left out. A later kind, a required field, a list element, or a value that gives a field its meaning (an enum or const value of the field, such as a channel direction, a catalog resource kind, a placement or a tier) stops with an error that names the path, because the manifest would then say something else. `farmctl` reads the schemas for this from the checkout (`-spec-root` or `BOTARU_SPEC_ROOT`, else a parent of the working directory).
- **The attempt record.** At `claim` the core writes into the lease of the attempt the run fingerprint with its part hashes (PRV-1) and, for the full result check at `complete` (OUT-1), the version of the tenant catalog and the `enum_from` values of the result schema filtered by the task's mark. P1 `hello` carries no `describe()` values, so the runtime part has `describe: null`; its driver is the role's `runtime.driver`, else the implementation name from `hello`. `catalog.<list>`, `catalog.hosts` and `catalog.services` resolve from the tenant's `Catalog` manifest, `reasons.<name>` from `spec/model/<name>-reasons.yaml`, and `task.attachments` to nothing, because there are no files yet.
- **Notices to a person** (`principal_left`, IDN-13) go to the direct chat of each admin with `admin:people` through every channel of the tenant whose identity kind matches the admin's identity; the chat id of a direct chat is the external user id. The notice goes when the state changes: a repeated `off` sends none. When no such admin is reachable, the departure still goes through; the audit record of the command shows `principal_left: 0`, and the core logs a warning. An offboarded id is final: every people operation on it, `principal.add` included, answers `subject_closed`.
- **Results refused at complete** (`output_schema_invalid`, OUT-1) are stored on the lease like an accepted outcome, so a repeat of the same complete gets the same refusal (IDM-3), and any other call under the lease is fenced. A lease taken before the upgrade has no attempt record; its `enum_from` values are resolved at complete.
- **Model providers.** The `provider` of a model profile is a name that the deployment settings map to a binding, an address and the key of each tenant's workspace. On the test bench the provider of the examples (`provider-a`) maps to the mock provider.
- **A bare command is its own goal.** `goal` is required by `task-body`, so `/hello` without text gets the goal `/hello`.
- **Defaults** where a type sets nothing: lease 60 s, heartbeat 20 s, 2 attempts, backoff 10 s doubling to 5 min, task deadline 1 h, 5 delivery attempts, priority class 3 (2 for an interactive type from a person).

## Not in this version

v0.1 of the implementation stops at the first working bot; the following parts of the v0.1 rules are not built yet: P3 and tools, sessions and memory, approvals, cards and buttons, holds, messages between bots, routes and schedules, campaigns and scripts, the monitoring adapter and farm alarms (`alarm_notice`, MON-1, MON-13), the storage headroom (CAP-5), the `retention` loop (RET-2), erasure work (offboarding records the erasure request by the scope `(tenant, subject)` with the reason `offboarding` in the state `requested`, LCY-8, but nothing processes it yet, RET-7), the test-control operations other than `clock.set`, `clock.advance` and `loop.tick` of the `reaper` and `deadline` loops, streaming on P4, the circuit breaker and the pool limiter of P4, files and artifacts (with their provenance, PRV-2), group chats (`chat.bind`; activity from a group is ignored, so `principal_left` lists no chats), and the verification code for privileged groups (PRM-13): `grant` of a privileged group answers `privileged_group`. The core writes `fairness_key` on every task (DSP-2), but P2 `task.show` is not built. The P2 operations outside `apply`, `principal.add`, `principal.off`, `principal.on`, `principal.set_identity`, `principal.offboard`, `grant`, `ungrant`, `worker.enroll`, `health` and `audit.verify` answer `404 not_found`.

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

The `go` command downloads the toolchain named in `go.mod` by itself (`GOTOOLCHAIN=auto`). staticcheck and govulncheck are tool dependencies in `go.mod`, so their versions are pinned and Dependabot updates them.

### Rule coverage

`TestConformance` in `internal/conform` runs every behaviour case of `conformance/fixtures/` whose rules are of v0.1, all profiles, on the reference core: each case on a fresh test bench (a new database, the core with its P1, P2 and test-control listeners), reached only over HTTP. A case is passed, failed, skipped (a later version) or unchecked: it needs a given key, a step, an operation or a fact that the runner or the core cannot do yet, and the report names the first such need. An unchecked case never counts for its rule. A rule is done when every case that names it passes. `testdata/passing.txt` lists the passing cases: the test fails when a listed case stops passing, and when a case starts to pass without being listed (record it with `go test ./internal/conform -run TestConformance -update`). With `BOTARU_CONFORM_OUT=<folder>` the test writes `junit.xml` and the coverage table `coverage.md`; CI puts the table into the job summary.

The store tests need PostgreSQL: either `BOTARU_TEST_DSN` names a server where the tests may create and drop databases, or `initdb` of PostgreSQL 16 or later is installed (in `PATH` or `/usr/lib/postgresql/*/bin`) and the tests start a private server; they refuse to do that as root. Without PostgreSQL the store tests are skipped, unless `BOTARU_TEST_PG_REQUIRED=1`, as in CI.
