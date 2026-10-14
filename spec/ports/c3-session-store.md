# C3 SessionStore

How the core is built, not a contract (C10, D-114).

## In short

C3 keeps the conversation transcripts of attempts in a table of the farm store, under a key that the core computes and that always includes the tenant (MEM-1). The agent reads and writes them through P1 `session.load` and `session.append`; a runtime that keeps the session itself declares it in P1 `hello` and follows RT-1.

## How it works

- `append` adds entries, idempotent by the entry id; `load` reads a session by key; `list` lists the sessions of a namespace (TEN-5).
- The session key of a group chat includes the subjects who wrote in the conversation, so a deletion by `(tenant, subject)` also finds the group sessions in which the person took part (SEC-11).
- The core keeps the session's `untrusted_seen` mark; only a new conversation or `forget` clears it (INJ-13).
- `delete` (one session by id) and `purge` (by `(tenant, subject)`, a list of ids, a retired bot identity, and from v0.2 `(tenant)`) serve the `erasure` loop, with a result for each request (MEM-4, MEM-9, RET-7, RET-11). `prune` deletes entries older than the tenant's `content` period for the `retention` loop (RET-10).

## Where the details are

`spec/model/session-entry.schema.json`; erasure checks of the `core-blackbox` suite.
