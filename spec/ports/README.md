# Ports

## In short

A port is a boundary at which one part of the farm can be replaced or checked separately from the rest. The skeleton has thirteen ports of three kinds: four core protocols (P1-P4), four ports behind the core (C1-C4) and five edge ports (E1-E5) (D-38, D-66). Each port has a sheet in this folder: the meaning of the operations, who calls them, refusals, guarantees and the rules the port holds. HTTP paths, SQL, column names and status codes are not part of a sheet: they live in the bindings `spec/bindings/`. Work is pull-based: a worker takes work itself through P1, the core pushes work to nobody. The background loops of the core are not a port; their shared contract is described in `background-loops.md`.

## Three kinds of ports

| Kind | Who calls | Where the boundary is | Ports |
|---|---|---|---|
| Core protocol | workers, the runtime, people through `farmctl` and a channel call the core | network: HTTP, MCP | P1, P2, P3, P4 |
| Port behind the core | the core calls a replaceable part | inside the core or its binding | C1, C2, C3, C4 |
| Edge port | an adapter takes work through P1 and talks to the outside world | inside the edge process | E1, E2, E3, E4, E5 |

The sheet of an edge port describes both sides: which P1 operations the adapter calls and what it must do with the work it received.

## Summary table

A "thin" port: one page, one reference adapter, a minimal set of checks. A thin port (C3, C4, E2) that has no second implementation and no open decision by the release after v0.2 folds into the prose of the core sheet (D-46).

| Port | Sheet | Kind | Sheet since | Thin | Reference | Main rules |
|---|---|---|---|---|---|---|
| P1 WorkerProtocol | `p1-worker-protocol.md` | core protocol | v0.1; HTTP binding v1 from v0.1, OpenAPI 3.1 description `spec/bindings/http/p1.openapi.json` from v0.2 (MAN-12) | no | core; the deployment chooses the transport | TEN-2..4, TEN-7, IDN-1..3, IDN-7, FEN-1..4, IDM-1..5, DLV-1, PRM-9, SEC-3, MSG-9 |
| P2 AdminAPI | `p2-admin-api.md` | core protocol | v0.1; HTTP binding v1 (`admin.md`) from v0.1, OpenAPI will appear with the second client | no | `farmctl`; commands of people in a channel on behalf of the verified person | PRM-7, PRM-13, PRM-14, IDN-7, AUD-1, MAN-5, HLD-1 |
| P3 ToolGateway | `p3-tool-gateway.md` | core protocol | v0.1 | no | core; the MCP tool protocol, revision in the binding | APR-1, APR-18, PRM-3..5, INJ-2, INJ-4, INJ-13, INJ-15, TEN-3a, SEC-3, RT-4 |
| P4 ModelGateway | `p4-model-gateway.md` | core protocol | v0.1 | no | core; a field allowlist per provider | GW-1..3, GW-7, BUD-2, BUD-3, TEN-5, CAP-2 |
| C1 TaskStore | `c1-task-store.md` | behind the core | v0.1 | no | PostgreSQL | FEN-1..3, FEN-5, IDM-1..4, DLV-2, AUD-2, AUD-3, TEN-1, RET-3, RET-6 |
| C2 Authorizer | `c2-authorizer.md` | behind the core | v0.1 (the permission table as a fixture) | no | a function in the same transaction as the transition | PRM-1..10, TEN-2, APR-4, HLD-1 |
| C3 SessionStore | `c3-session-store.md` | behind the core | v0.2 | yes | a table in the farm store | TEN-5, MEM-1, MEM-4, INJ-13, RT-1, RT-5 |
| C4 MemoryStore | `c4-memory-store.md` | behind the core | v0.1 | yes | a table in the farm store | TEN-5, MEM-1..4, MEM-6..8, RT-2 |
| E1 Channel | `e1-channel.md` | edge | v0.1 | no | Telegram | IDN-3, IDN-7, APR-3, APR-4, APR-12, DLV-5..7, INJ-11, TEN-4 |
| E2 Sink | `e2-sink.md` | edge | v0.2 (`match` and `erase` of the reference from v0.1) | yes | disk (JSON and Markdown) | DLV-3, DLV-6, RET-4, RET-7 |
| E3 ToolHost | `e3-tool-host.md` | edge | v0.1 | no | toolhost with MCP plugins | TEN-3a, INJ-1..6, INJ-14, DEP-1..10, APR-7, APR-18, MON-8 |
| E4 AgentRuntime | `e4-agent-runtime.md` | edge | v0.1 | no | own loop in Go (D-93) | INJ-9, RT-1..5, FEN-4, MEM-4, DEP-5 |
| E5 Monitor | `e5-monitor.md` | edge | v0.1 (`match` and `erase` of notes from v0.1, D-76) | no | the organization's monitoring system, for example Zabbix | MON-1..11, AUD-6, TEN-4, DEP-11 |

The test-control port `test-control.md` is not one of the thirteen ports: it is the input of the check runner, which exists only with a test bench key in the settings and a store created as a test bench store that never had a tenant other than `kind: test` (APR-9); a production farm does not have it, and the core of a production farm with a test bench key does not start.

In v0.1 the sheets P1, P2, P3, C1, E1 and E5 hold only v0.1 operations. Operations of later skeleton versions enter a sheet as a minor version of the sheet together with their skeleton version; the "Version" column in the operation tables shows from which version an operation exists. Within v0.1, by D-71, the core and the first bot come first, and then the operations of messages between bots (`msg.*`, `inbox_read`, `send_message`), the improvement cycle (`obs.*`, `proposal.*`, `commission.status`, subject `proposal`), reporters, campaigns, scripts and the manager; in the sheets they are also marked v0.1 (D-80).

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
- **Versions.** Each sheet has its own SemVer version; a new operation is a minor version of the sheet. Bindings have their own versions: the `/v1` path in HTTP, the `.vN` suffix of task and event types, `apiVersion` of manifests, the tool protocol revision and the OS version in `spec/bindings/` (see `spec/bindings/README.md`). The version map is the single file `spec/versions.md`. The operation map of all sheets with request and response schemas and rules is `spec/ports/operations.yaml`: every operation from the sheet tables is in the map, and every schema of the map is in `spec/model`. The `profiles` field of a map row names the conformance profiles that need the operation: an implementation that claims only `core` takes the rows that contain `core`.

## Sheet template

Every sheet has the same structure.

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
