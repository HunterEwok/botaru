# Lifecycle: erasure request

Kind: `erasure` (the kinds are listed in [the lifecycle index](README.md)).

## In short

An erasure request (`ErasureRequest`) is a recorded object through which every early deletion goes, that is, a deletion before the end of the retention class period: `forget` of memory, a person's offboarding, wiping the data of a retired bot, deleting a tenant, cleaning a runtime's store when a worker is revoked, a person's request, a manual deletion by the operator through `farmctl` (RET-7). The request keeps the scope, the reason, the author, the time and the outcome for every store, but does not keep content. The scope is from a closed list: `(tenant)`, `(tenant, subject)`, a bot identity id (everything sealed for it, only with the reason `bot_retired`), a worker id (the store of its runtime, only with the reason `worker_revoke`), or a list of object ids. One request can name many ids: this is how a confirmed `erase_match` deletes all found objects (SEC-11). The request gets into the erasure log before the first byte is deleted; the log is backed up separately from the store backups and at least as often (RET-5). After a restore from backup the core replays the log, so deleted data does not come back (RET-6). Deletion by period is done by the background loop `retention` without a request: after a restore it deletes the expired data again anyway. In v0.1 the loop deletes only the classes `content` and `observation` by the default periods, from v0.2 all classes by `Tenant.spec.retention` (RET-2).

## States

| State | Meaning | Terminal |
|---|---|---|
| `requested` | the request is written to the erasure log; deletion has not started yet | no |
| `running` | the background loop `erasure` sent the request to the stores and collects the outcomes | no |
| `done` | every store answered `done` or `not_applicable` | yes; after a restore from backup the request is replayed |
| `failed` | at least one store answered `failed` after retries, or the request deadline passed; code raised an alarm | no |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `requested` | P2 `erasure.request` (a `farmctl` command) | the operator; the reason `manual` or `subject_request`; the scope `(tenant)`, `(tenant, subject)` or a list of object ids | RET-7 |
| (none) | `requested` | approval of a person's request (`SubjectRequest`) through P1 `decide` | from v0.1 (D-76); a person with `admin:people`, for `erase_match` a person other than the author, and if the tenant has no second active person with `admin:people`, the farm admin through `farmctl` (D-86 (8)); the reason `subject_request`; the scope `(tenant, subject)` for `/mydata erase`, or the list of object ids that `erase_match` found | SEC-11, RET-7 |
| (none) | `requested` | a person's `/forget` (MEM-4); executed by C4 `erase` | a person about their own `person` memory, or a person with the right to that memory; the reason `forget`; the scope is a record id or `(tenant, subject)` | RET-7, MEM-4 |
| (none) | `requested` | P2 `principal.offboard` | an admin with `admin:people` for people without privileged groups, the operator through `farmctl` for people with privileged groups (PRM-14); the reason `offboarding`, the scope `(tenant, subject)` | LCY-3, RET-7, PRM-14 |
| (none) | `requested` | P2 `tenant.delete` | the operator; the export window has ended; the reason `tenant_delete`, the scope `(tenant)` | LCY-1, RET-7 |
| (none) | `requested` | the core in the transaction of a bot identity retirement | the identity's sessions inside the runtime (`runtime_internal`), which the core cannot seal; the reason `bot_retired`, the scope is the list of ids of these sessions; executed by the runtime adapter with the E4 operation `purge` | MEM-9, RET-4, RET-7 |
| (none) | `requested` | the background loop `identity` at the end of the seal of a retired bot | `seal_ttl` has passed; the reason `bot_retired`, the scope is the bot identity id; everything sealed is wiped | MEM-9, RET-7 |
| (none) | `requested` | P2 `worker.revoke` of a worker whose runtime keeps sessions or memory itself | the operator; the reason `worker_revoke`, the scope is the worker id (the store of its runtime) | LCY-4, RET-7 |
| `requested` | `running` | the background loop `erasure` | the request is in the erasure log | RET-7 |
| `running` | `done` | the background loop `erasure` | all stores answered `done` or `not_applicable` | RET-4, RET-7 |
| `running` | `failed` | the background loop `erasure` | a store answered `failed` after retries, or the deadline passed; an alarm by code | RET-7 |
| `failed` | `running` | the background loop `erasure` after a pause, or P2 `erasure.retry` | only the stores without `done` and `not_applicable` are retried | RET-7 |
| `done` | `running` | restore of a store from backup | the core replays the erasure log from its separate backup before readiness | RET-6, RET-7 |
| `failed` | `running` | restore of a store from backup | same | RET-6, RET-7 |

## Invariants

- Content can be deleted early only by a request; no process and no `farmctl` command has a direct path around the request (RET-7).
- The request reasons are a closed list: `forget`, `offboarding`, `bot_retired`, `tenant_delete`, `worker_revoke`, `subject_request`, `manual`.
- The request scopes are also a closed list: `(tenant)`, `(tenant, subject)`, a bot identity id (only with `bot_retired`), a worker id (only with `worker_revoke`), a list of object ids.
- A request gets into the erasure log before the first deletion by it.
- A request keeps only metadata: the scope, the reason, the author, the time, the outcomes. The `erase_match` search string gets neither into the request nor into the log, not even as a hash (SEC-11). The log itself belongs to the retention class `tombstone` (RET-1).
- Repeating a request is safe: what is already deleted gives `done` or `not_applicable` the second time.
- After a task is deleted a tombstone remains, and its idempotency key is not released (RET-3).
- A request does not close while any store has not answered `done` or `not_applicable`; a request open past its deadline gives an alarm. There is one exception: `failed` from E5 for a list of note ids does not keep the request open, because a note carries no people data (RET-4).
- Deleting a person's data at their request is part of v0.1 and cannot be waived on a tenant with `kind: production` (D-76).
- Every transition publishes an event (`erasure.requested`, `erasure.done`, `erasure.failed`) and writes an audit record.

## Versions

| Version | What appears |
|---|---|
| v0.1 | all states, the erasure log and its separate backup (the core sees its freshness from `backup.report` reports, RET-8), the replay after a restore; deletion by period of the classes `content` and `observation` by the `retention` loop without a request (RET-2); the scopes `(tenant)`, `(tenant, subject)` and a list of object ids; the reasons `forget`, `offboarding` (a person's offboarding deletes their memory and sessions, MEM-4), `subject_request` and `manual` from the operator; a request on an approved request of a person `/mydata erase` and on `erase_match` (SEC-11, D-76); P2 `erasure.request`, `erasure.status`, `erasure.list`, `erasure.retry`. The request reaches the stores that already have a delete operation: sessions in C3, memory in C4, runtime stores with `runtime_internal` through E4 `purge`, files through C1 `file.purge`, the sink archive through E2 `erase`, notes through E5 `erase` (RET-4). A store with content but without such an operation (attachments and the cache of the E1 channel, runtimes without `runtime_internal`) answers `failed`, and the request stays open with an alarm; the reason `bot_retired` and the scope of a bot identity id: the sessions of a retired bot inside the runtime are deleted by a request in the retirement transaction, and everything sealed at the end of the seal, so it does not come back after a restore from backup; from v0.2 the other sessions of the bot are deleted by their own period (RET-2) |
| v0.2 | E1 `erase` and `match`, E4 `purge` for every runtime and C3 `purge` by `(tenant)` (RET-11); deletion by the tenant periods in every copy without a request (RET-9, RET-10); the `/mydata export` statement (SEC-10, LCY-2); the reason `tenant_delete` together with the tenant cycle; the reason `worker_revoke` and the scope of a worker id together with worker revocation; the full subject cycle with deletion of `Identity` (LCY-3) |
