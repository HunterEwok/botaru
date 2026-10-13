# Lifecycle: session

## In short

A session is the transcript of a runtime conversation under the key `(tenant, role, subject, conversation)`, which the core computes (MEM-1). Session records are only appended, and compaction appends a summary record. The core does not read the content of a session: it keeps only the key, the `untrusted_seen` mark and the periods. A session does not pass to another bot: when the identity that kept it retires, the session is sealed, and the conversation continues in a new session. A runtime that keeps the session itself declares `session: runtime_internal` and obeys RT-1.

## States

| State | Meaning | Terminal |
|---|---|---|
| `open` | The tasks of the conversation append to the session | no |
| `sealed` | The identity that wrote to the session retired: no bot reads the session, people with `admin:governance` see only its metadata | no |
| `deleted` | The session is deleted in all copies of the store | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `open` | P1 `session.append` from the runtime adapter in the first task of the conversation | the core computes the key; the conversation's `untrusted_seen` mark, if it already exists, carries over into the new session | MEM-1 |
| `open` | `open` | P1 `session.append`; runtime compaction appends a summary | only appending; with untrusted output the core sets the `untrusted_seen` mark | MEM-1, INJ-13 |
| `open` | `sealed` | the core in the transaction of an identity's retirement | the identity wrote to this session; the next task with the same key starts a new session | MEM-4, IDN-11 |
| `open`, `sealed` | `deleted` | the `retention` loop | the retention class period has passed; the seal does not extend the period | MEM-9, RET-2 |
| `sealed` | `deleted` | the `identity` loop enqueues an erasure request, the `erasure` loop deletes | `seal_ttl` has passed | MEM-9, RET-7 |
| `open`, `sealed` | `deleted` | `forget`, a person's erasure request, a person's offboarding, tenant deletion (the `erasure` loop) | real deletion, an outcome for every store | MEM-4, RET-7 |
| `open` | `deleted` | an erasure request `bot_retired` that the core enqueues in the transaction of an identity's retirement; the runtime adapter executes it with the E4 operation `purge` | the session is `session: runtime_internal`: the core cannot seal it | MEM-9, RT-1, RET-4, RET-7 |

## Invariants

- The core computes the session key, and it always includes the tenant; the model's arguments do not set it (MEM-1).
- Only a new conversation or `forget` removes the `untrusted_seen` mark; a new session of the same conversation keeps the mark (INJ-13).
- Records are only appended; the core does not read the content.
- A session is not handed to a successor and does not get into a fact offer (MEM-7).
- From v0.2 a sealed session lives no longer than the period of its class (MEM-9, RET-2).
- With `conversation_concurrency: serial` one role has at most one attempt in one conversation, and tasks are taken in the order they were enqueued (MEM-5, dispatcher condition D17).

## Versions

| Version | What appears |
|---|---|
| v0.1 | `open` and `deleted`: appending, compaction, `forget`, a person's offboarding (P2 `principal.offboard`); `sealed` on an identity's retirement; deletion of a `runtime_internal` session on an identity's retirement by a `bot_retired` request through E4 `purge` |
| v0.2 | Deletion by the class period by the `retention` loop (RET-2), and the seal does not extend this period (MEM-9); tenant deletion (P2 `tenant.delete`); the order of tasks in a conversation (MEM-5) |
