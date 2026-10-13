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
| `farmctl` | the operator's tool for P2: `apply`, people, memberships, `worker enroll`, `health`, `audit verify` |
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

## Implementation choices

These are decisions of this implementation where the skeleton leaves a choice; none changes a contract.

- **Store.** PostgreSQL 16 or later through pgx. The store clock is `botaru_now()` (the database time plus the test bench offset), so every deadline is counted by one clock (TIM-1).
- **The skeleton is read at run time** from a checkout (`spec_root`, or the folder the program runs in), not copied into the binaries, so the schemas the core checks are the ones in git.
- **Operator credentials** are scoped to one tenant each, because the bodies of the P2 people operations carry no tenant (TEN-2). The settings file holds only the SHA-256 of each credential.
- **Secrets.** Worker tokens, handles and driver secrets are stored as HMAC-SHA256 with the core token key; lease tokens are signed with the same key (`key_id`, `sig`).
- **The result schema reaches the runtime** as the extension field `payload.result_schema` of the attempt (MAN-3), so the instruction stays exactly the trusted text from git; the runtime appends the request for a JSON answer to the system prompt.
- **The recipient's locale** reaches the channel as the extension field `payload.locale` of a delivery. The code text catalog ships in `internal/texts/en.yaml`, because the skeleton has no channel text catalog yet (DLV-9 names one); other locales are `<locale>.yaml` files in the installation's text folder.
- **Model providers.** The `provider` of a model profile is a name that the deployment settings map to a binding, an address and the key of each tenant's workspace. On the test bench the provider of the examples (`provider-a`) maps to the mock provider.
- **A bare command is its own goal.** `goal` is required by `task-body`, so `/hello` without text gets the goal `/hello`.
- **Defaults** where a type sets nothing: lease 60 s, heartbeat 20 s, 2 attempts, backoff 10 s doubling to 5 min, task deadline 1 h, 5 delivery attempts, priority class 3 (2 for an interactive type from a person).

## Not in this version

v0.1 of the implementation stops at the first working bot; the following parts of the v0.1 rules are not built yet: P3 and tools, sessions and memory, approvals, cards and buttons, holds, messages between bots, routes and schedules, the monitoring adapter, erasure, the test-control port, streaming on P4, the circuit breaker and the pool limiter of P4, files, group chats (`chat.bind`; activity from a group is ignored), and the verification code for privileged groups (PRM-13): `grant` of a privileged group answers `privileged_group`. The P2 operations outside `apply`, `principal.*`, `grant`, `ungrant`, `worker.enroll`, `health` and `audit.verify` answer `404 not_found`.

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

The store tests need PostgreSQL: either `BOTARU_TEST_DSN` names a server where the tests may create and drop databases, or `initdb` of PostgreSQL 16 or later is installed (in `PATH` or `/usr/lib/postgresql/*/bin`) and the tests start a private server; they refuse to do that as root. Without PostgreSQL the store tests are skipped, unless `BOTARU_TEST_PG_REQUIRED=1`, as in CI.
