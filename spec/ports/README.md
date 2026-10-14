# Ports

## In short

A port is a boundary at which one part of the farm meets another. Contracts with checks exist only at the boundaries with foreign code (C10, D-114): workers (P1) and channels (E1), tool plugins (P3 together with E3, the MCP boundary), the model provider (P4) and the monitoring system (E5). Their sheets hold the operations, refusals, guarantees and the rules the boundary holds, and the conformance suites check them. Every other page in this folder describes how the core is built: the admin API P2 that our own `farmctl` and channels use, the parts behind the core C1-C4, the sink E2, the agent runtime E4, the test-control port, the consent adapter and the background loops. Those pages say how it works and which rules it serves, without request and response tables. HTTP paths, SQL, column names and status codes are not part of a page: they live in the bindings `spec/bindings/`. Work is pull-based: a worker takes work itself through P1, the core pushes work to nobody.

## Contract boundaries

| Boundary | Sheet | Who is on the other side | Reference | Main rules |
|---|---|---|---|---|
| P1 WorkerProtocol | `p1-worker-protocol.md` | every worker: agents, jobs, toolhosts, channels, sinks, monitoring adapters; HTTP binding v1, OpenAPI 3.1 description `spec/bindings/http/p1.openapi.json` (MAN-12) | core | TEN-2..4, TEN-7, IDN-1..3, IDN-7, FEN-1..4, IDM-1..5, DLV-1, PRM-9, SEC-3, MSG-9 |
| E1 Channel | `e1-channel.md` | the messenger and the people in it | Telegram | IDN-3, IDN-7, APR-3, APR-4, APR-12, DLV-5..7, INJ-11, TEN-4 |
| P3 ToolGateway and E3 ToolHost | `p3-tool-gateway.md`, `e3-tool-host.md` | tool plugins (MCP servers) | core; toolhost with MCP plugins | APR-1, APR-18, PRM-3..5, INJ-1..6, INJ-13..15, TEN-3a, SEC-3, DEP-1..10, RT-4, MON-8 |
| P4 ModelGateway | `p4-model-gateway.md` | the model provider | core; a field allowlist per provider | GW-1..3, GW-7, BUD-2, BUD-3, TEN-5, CAP-2 |
| E5 Monitor | `e5-monitor.md` | the organization's monitoring system, for example Zabbix | monitoring adapter | MON-1..11, AUD-6, TEN-4, DEP-11 |

The sheet of an edge boundary describes both sides: which P1 operations the adapter calls and what it must do with the work it received.

## How the core is built

| Page | What it describes | Reference | Main rules |
|---|---|---|---|
| `p2-admin-api.md` | P2 AdminAPI for `farmctl` and channel commands on behalf of a person; its schemas are implementation schemas (`contract: false` in `operations.yaml`), the operator operations are the rows with `operator_only` (PRM-14) | core and `farmctl` | PRM-7, PRM-13, PRM-14, IDN-7, AUD-1, MAN-5, HLD-1 |
| `c1-task-store.md` | C1 TaskStore: tasks, leases, transitions in one transaction | PostgreSQL | FEN-1..3, FEN-5, IDM-1..4, DLV-2, AUD-2, AUD-3, TEN-1, RET-3, RET-6 |
| `c2-authorizer.md` | C2 Authorizer: one decision with a reason at every check point; the permission table fixture | a function in the transition transaction | PRM-1..10, TEN-2, APR-4, HLD-1 |
| `c3-session-store.md` | C3 SessionStore: conversation transcripts | a table in the farm store | TEN-5, MEM-1, MEM-4, INJ-13, RT-1 |
| `c4-memory-store.md` | C4 MemoryStore: person memory and verified bot facts | a table in the farm store | TEN-5, MEM-1..4, MEM-6..9, RT-2 |
| `e2-sink.md` | E2 Sink: a worker that writes artifacts outside the farm | disk (JSON and Markdown) | DLV-3, DLV-6, DLV-17, RET-4, RET-7 |
| `e4-agent-runtime.md` | E4 AgentRuntime: the model loop inside an agent worker and what the core requires of any runtime | own loop in Go (D-93) | INJ-9, RT-1..5, FEN-4, MEM-4, DEP-5 |
| `test-control.md` | the test-control port of the check runner, only on a test bench (APR-9) | core | APR-9 |
| `consent-adapter.md` | the consent adapter that connects a person's own account (SEC-7) | OAuth 2.0 with PKCE | SEC-7 |
| `background-loops.md` | the background loops of the core | core | OBS-4, MON-2 |

The schemas of these pages are of two kinds: implementation schemas of the core and `farmctl` (P2) and core test schemas that fixtures check (the permission table `authz-request` and `authz-decision`, the stand steps of the test-control port). A schema of an internal page that nothing checked was removed (`spec/removed.yaml`).

Within v0.1, by D-71, the core and the first bot come first, and then the operations of messages between bots (`msg.*`, `inbox_read`, `send_message`), the improvement cycle (`obs.*`, `proposal.*`, `commission.status`, subject `proposal`), reporters, campaigns, scripts and the manager; in the sheets they are also marked v0.1 (D-80).

## What is deliberately not a port

| Candidate | Why it is not a port |
|---|---|
| EventStream | Events are a contract (the CloudEvents catalog) plus `emit` in P1 and `event.*` in C1. A separate port will appear together with a broker |
| Scheduler | Schedules are run by the core background loop `schedule`. An external timer, if the deployment needs one, calls P1 `enqueue` with the slot key |
| IdentityProvider | The step "credentials -> subject" is a required part of P1 and P2, not a replaceable part. An external program is a subject of kind `service` (D-47) |
| SecretStore | Secrets are named by name in manifests, the deployment gives the values. The names of a global plugin are resolved in the tenant's namespace (TEN-6, SEC-1, SEC-2) |
| Meter, FileStore, AuditLog | Parts of C1. Only P4 reserves and charges model usage, workers only report usage (BUD-3) |
| ToolPlugin | A plugin is an external MCP server; its contract is the plugin profile in the binding `spec/bindings/mcp/README.md` and sheet E3 |
| Telemetry | This is a binding contract (`spec/bindings/telemetry/`) and a rule about what may be sent out (OBS-1). The organization's monitoring system is not telemetry: it has its own port E5, because alarms to people and core liveness go through it (MON-1, MON-2) |
| ConsentAdapter | An edge process that connects a person's own account at an external service: the consent screen, the token in the deployment secret store and the P2 operations `connection.register` and `connection.revoke` on behalf of the person. The core calls nothing in it, so it adds no operation; its sheet is `consent-adapter.md` (SEC-7) |
| Watchdog | The farm has no separate liveness check process: the monitoring system checks core liveness by polling P2 `health` (D-60, MON-2) |

## Common to all ports

- **Errors.** One shape for HTTP: problem details by RFC 9457 (`type`, `title`, `status`, `detail`, `instance`, media type `application/problem+json`) with an added member `reason` from the closed list `spec/model/reasons.yaml`. Inside a process the port returns the same `reason`. The reason registry says for each reason: whether it spends an attempt, whether to retry, with which outcome it ends. The model does not get the reason of a permission refusal: it gets a short "no permission", the full reason is written to the audit (PRM-8). HTTP binding v1 assigns each code to a status; the enqueue refusal `rejected{reason}` of the sheets is also problem details, with status 422 (`spec/bindings/http/README.md`).
- **The subject and the tenant** of every operation the core computes from the credentials (TEN-2). The schema rejects a tenant field in the body.
- **Handles.** Everything the runtime gets for its work (tool, model, session and memory handles) is bearer credentials, not names: at least 128 bits of randomness, they expire together with the lease, are passed in the authorization header, and do not get into the model context, logs and audit (SEC-3). A handle is bound to the store epoch: after a restore from a backup, handles of the previous epoch get `fenced` (RET-6).
- **Lease token.** The holder of work passes the lease token in one way in all operations under a lease: by a binding header (`Botaru-Lease` in HTTP), not by a body field; a call without the token or with a token of a past lease gets `fenced` (FEN-2).
- **Not ready.** While the C1 store does not answer or reconciliation after a restore has not finished, P1 answers `not_ready` with `retry_after_s`, P4 starts no new model calls, a channel does not move the external system offset (OBS-8, RET-6).
- **Idempotency.** A repeat of a changing operation with the same arguments gives the same answer, side records are made once (IDM-3). The sheet says for each operation by which key it is idempotent.
- **Erasure and search.** Every port that stores content has its own erase operation by the scope `(tenant)`, `(tenant, subject)` and by a list of ids with the result `done`, `failed` or `not_applicable` into the erasure request: C1 `file.purge` for files and `task.purge` for task content, C3 `delete` and `purge`, C4 `erase`, E1 `erase`, E2 `erase`, E4 `purge`, E5 `erase` (RET-4), and a search operation `match(tenant, subject, needles) -> ids` for erasure by match in stores the core does not read: E2, E4 and E5 from v0.1, E1 from v0.2; a store without `match` counts a search by match as `failed`. Erasure of a person's data on their request (`/mydata erase`, C1 `file.purge` and `task.purge`, E2 and E5 `erase`, `erasure.match`) is part of v0.1 and is non-waivable for a production tenant (D-76); from v0.2 E1 `erase` and `match`, the data extract and the other subject rights are added. The rest of the early versions: C3 `delete` and `purge` and C4 `erase` on `forget` and person offboarding (MEM-4), E4 `purge` and `match` for runtimes with `runtime_internal` (RT-1, RT-2, RT-5), C4 `erase` of what is sealed for a retired bot (MEM-9). A store without an erase operation answers a request with `failed`, and the request stays open with an alarm (RET-7). The result `not_applicable` is accepted only with the `basis` sign by which the store made sure that there is nothing to delete; a channel that declared `driver.stores_content: none` in `hello` closes its step with this declaration, and the core writes its result itself. Edge stores get the erasure and search steps by pull, like any work: as a work unit of kind `erasure` through P1 `claim`, the result is handed in with `complete` (`spec/bindings/http/README.md`, "Erasure and search work for edge stores").
- **Telemetry** of port processes goes only by the attribute allowlist, without content (OBS-1).
- **Versions.** Each contract sheet has its own SemVer version; a new operation is a minor version of the sheet. A page that describes the core has no version of its own. Bindings have their own versions: the `/v1` path in HTTP, the `.vN` suffix of task and event types, `apiVersion` of manifests, the tool protocol revision and the OS version in `spec/bindings/` (see `spec/bindings/README.md`). The version map is the single file `spec/versions.md`. The operation map of all sheets with request and response schemas and rules is `spec/ports/operations.yaml`: every operation from the sheet tables is in the map, and every schema of the map is in `spec/model`. The `profiles` field of a map row names the conformance profiles that need the operation: an implementation that claims only `core` takes the rows that contain `core`.

## Sheet template

Every contract sheet has the same structure. A page that describes how the core is built has only "In short", "How it works" and "Where the details are".

```
# <Port name>

## In short
Why the port exists, the main idea, what it is connected with. 3-6 sentences.

## Who calls and who implements
The sides of the port, the reference implementation, where the trust boundary runs.

## Operations
| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |

## Guarantees
MUST, SHOULD, MAY by RFC 2119: what the port promises for any implementation.

## Bindings
Where the exact shapes live: spec/model schemas, spec/bindings bindings, conformance/suites check suites.

## What the port does not do
Boundaries of responsibility, so that no second path around the rules appears.
```

A sheet is no longer than two pages. The sheet of an edge port whose reference external system is set by a decision may add the sections "Data shapes" and "Reference binding" (sheet E5 is built this way). "Who calls" names a worker kind, a subject kind or a permission; "Refusals" lists `reason` codes; "Rules" refer to registry numbers.
