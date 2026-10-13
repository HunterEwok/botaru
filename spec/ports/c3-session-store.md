# C3 SessionStore

Sheet version: 0.1.0.

## In short

C3 stores runtime conversation transcripts under a key that the core computes and that always includes the tenant. The port is thin: one page, one reference adapter (a table in the farm store), a minimal set of checks; without a second implementation by version 0.3 it folds into the prose of the core sheet (D-46). The operations exist from v0.1 in the reference store table; the sheet as the contract of a replaceable adapter appears in v0.2. A runtime that keeps the session itself declares `session: runtime_internal` and follows the substitute rule RT-1.

## Who calls and who implements

- **Called by** the core on P1 `session.load` and `session.append` from the E4 adapter, and by the background loops `retention` and `erasure`.
- **Implemented by** the session store adapter registered by name in the core deployment configuration.

## Operations

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| `append` | core | Appends session entries; idempotent by `entry.uuid` | `handle_invalid` | v0.1 | MEM-1 |
| `load` | core | Reads a session by key | `not_found` | v0.1 | MEM-1, TEN-5 |
| `list` | core | Sessions of a namespace | | v0.1 | TEN-5 |
| `delete` | `erasure` loop on an erasure request (`forget`) | Deletes a session by id; the result goes into the erasure request | | v0.1 | MEM-4, RET-7 |
| `purge` | `erasure` loop on an erasure request (`forget`, person offboarding, tenant deletion, `bot_retired`) | Deletes all sessions of the scope `(tenant, subject)` (the subject's sessions and the group sessions whose key contains the subject) or by a list of ids; the result goes into the erasure request. Also the sealed sessions of a bot identity (the identity id scope, a `bot_retired` request at the end of the seal, MEM-9); from v0.2 the scope `(tenant)` together with tenant deletion and the `erasure` suite | | v0.1 | MEM-4, MEM-9, RET-7, RET-4 |
| `prune` | `retention` loop | Deletes entries older than the period of the `content` class | | v0.2 | RET-1, RET-2 |

## Guarantees

- MUST: the core computes the session key; model arguments do not set it (MEM-1). The session key of a group chat includes the set of subjects who wrote in the conversation (the core adds a subject on their first activity), so `purge` by the scope `(tenant, subject)` also finds the group sessions in which the person took part (RET-7, SEC-11).
- MUST: the core maintains the session's `untrusted_seen` mark; only a new conversation or `forget` clears it (INJ-13).
- MUST: deletion is real in all copies of the store (MEM-4, RET-7); from v0.2 the adapter passes the `erasure` suite (RET-4).

## Bindings

`spec/model/session-entry.schema.json`; the suite `conformance/suites/session` (v0.2).

## What the port does not do

It does not decide which session an attempt sees, and it does not store sessions of one tenant next to the keys of another.
