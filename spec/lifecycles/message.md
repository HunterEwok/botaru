# Lifecycle: message (inbox entry)

Kind: `message` (the kinds are listed in [the lifecycle index](README.md)).

## In short

A bot message lives in an inbox inside the core as an inbox entry: one message in one inbox of one tenant, and with the `all` delivery a separate copy in the inbox of every recipient worker, with its own cycle (MSG-14). An entry of the `report` genus (a work report) goes this way: `staged` (sent from an attempt, waits for its end), `queued` (published), `leased` (handed to a reading attempt under its lease) and `acked` (the reading attempt wrote `complete`); the terminal outcomes without processing are `discarded`, `dead` and `expired`. An entry of the `state` genus (worker state) has its own short cycle: the latest value `current`, an outdated `stale`, a replaced `replaced` and a removed `removed`; it has no lease and no acknowledgement. A refusal at sending creates no record: it is an answer to the sender with a reason, not a stored state. Only the core makes all transitions, each in one transaction of the C1 store with an event made of metadata only and an audit record (MSG-3, DLV-2). Publishing an attempt's messages and acknowledging what it read happen in the transaction of this attempt's `complete` (MSG-9).

## States

| State | Meaning | Terminal |
|---|---|---|
| `staged` | an entry of the `report` genus was sent from an attempt and stays with the core until the outcome of this attempt; it is not visible to the recipient but already takes space in the inbox and in the sender's share | no |
| `queued` | the entry is published in the inbox and waits for a reader | no |
| `leased` | the entry is handed to a reading attempt and leased by it until its outcome; it is not handed to other readers | no |
| `acked` | processed: the attempt that read it wrote `complete` | yes |
| `discarded` | removed without processing: the sending attempt ended without `complete`, a hold with the effect `no_message` was in force at `complete` (reason `held`), or the recipient disappeared (reason `recipient_gone`) | yes |
| `dead` | the entry returned to the inbox three times without acknowledgement (`delivery_exhausted`) | yes |
| `expired` | the entry's `expiresat` period ended before acknowledgement; the content is deleted by the `content` retention class | yes |
| `current` | the `state` genus: the latest value of this kind from this worker in this inbox | no |
| `stale` | the `state` genus: the value is older than the kind's `stale_after` (at most an hour); it is read with an "outdated" mark | no |
| `replaced` | the `state` genus: the value was replaced by a new one from the same worker | yes |
| `removed` | the `state` genus: the sending worker was revoked, or the kind or the receiving role was removed from the manifests | yes |
| `rejected` | a refusal at sending; there is no record, the idempotency key is not taken, the sender sees the reason, the refusal is written to the audit | yes (no record) |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `staged` | P3 `send_message` from an agent attempt; P1 `msg.send` with a `job` attempt token | the attempt is alive and holds a lease; a kind of the `report` genus with the needed `producers`; the pair (kind, address) is in the role's `messages.send`, the initiator has `msg.send:<kind>`; the address is allowed inside the tenant; the fields passed the kind's closed schema, the size and the note are within limits; the kind's `data_class` is not below the attempt's mark; at most 3 sends per attempt, the kind's rate; `hops` at most 5; the inbox has enough space counting `staged`, and the sending role's share is not exceeded; there is no hold with `no_message` | MSG-1, MSG-2, MSG-4, MSG-5, MSG-6, MSG-15, MSG-11, MSG-13 |
| (none) | `rejected` | P3 `send_message`, P1 `msg.send` | any check above failed: `address_invalid`, `msg_type_not_allowed`, `data_schema_invalid`, `note_not_allowed`, `data_class_exceeded`, `inbox_full`, `sender_quota`, `hops_exceeded`, `service_rate_limited`, `held`, `fenced`; the model gets a short "no" | MSG-4, MSG-5, MSG-15, PRM-8, HLD-3, FEN-4 |
| `staged` | `queued` | C1 `complete` of the sending attempt (the same transaction) | there is no hold with `no_message` on the sender's scope at this moment; for the `all` delivery the copies in the inboxes of the recipient's active workers are created here too, at most 50, and a copy that has no space in a worker's inbox is not created, and the core writes an event | MSG-9, MSG-14, DLV-2 |
| `staged` | `discarded` | C1 `fail` of the sending attempt, its cancellation, preemption or lost lease; C1 `complete` under a hold with `no_message` | the attempt ended without `complete`, or a hold forbids publication (reason `held`); a retry of the task by a new attempt sends its own entries and does not duplicate the key `system:msg:<attempt>:<n>` | MSG-9, FEN-4, HLD-3 |
| (none) | `queued` | the core in the transaction of a task's terminal transition (a route `on: task_done` with `mode: report`); the core for its own message to a specific worker | an auto-report with the `summary_fields` fields, the task's mark and flag, the key `system:report:<task>:<route>`; only the core writes the address `worker:<id>`; an auto-report that has no space is not created, the core writes a `message.rejected` event, and the task's transition goes through | MSG-4, MSG-16, DLV-2 |
| `queued` | `leased` | P3 `inbox_read`; P1 `msg.receive` with a `job` attempt token | the task type of the reading attempt declared `inbox.read` with this kind, the kind is in the role's `receive`; the entry's mark is not above the attempt's mark (otherwise the entry is not handed out and is counted in `hidden`); a worker reads the inbox of its role, the inboxes of its bot groups and, if the role declared `instance_inbox`, its own inbox | MSG-6, MSG-7, MSG-8, MSG-11 |
| `leased` | `acked` | C1 `complete` of the reading attempt (the same transaction) | all entries that the attempt read and did not return with `msg.nack`; P1 `msg.ack` only marks the entry, the acknowledgement still happens here | MSG-9, DLV-2 |
| `leased` | `queued` | C1 `fail` of the reading attempt; loss of its lease (the `reaper` loop); P1 `msg.nack` | the entry's period has not ended; the hand-out count plus one, and after that it is less than 3 | MSG-9 |
| `leased` | `queued` (without counting a hand-out) | cancellation or preemption of the reading attempt; a lease that expired while the core was not working | the entry's period has not ended; the hand-out count does not change | MSG-9, FEN-5 |
| `leased` | `dead` | C1 `fail` of the reading attempt; loss of its lease; P1 `msg.nack` | this is the third return with a hand-out count (`delivery_exhausted`); the core writes a `message.dead` event | MSG-9 |
| `leased` | `expired` | any return of the entry to the inbox | the entry's period ended while it was leased | MSG-12 |
| `queued` | `expired` | the background loop `inbox` | `expiresat` has passed; the entry is no longer handed out, wakes nobody and creates no tasks | MSG-12 |
| `queued` | `discarded` | `apply` removed the role, the bot group or the `receive` of this kind; P2 `worker.revoke` for a worker inbox | the recipient no longer exists (reason `recipient_gone`); the core writes an event, the entry is not lost silently | MSG-3, MSG-15 |
| (none) | `current` | P1 `msg.send` with a worker token (code outside an attempt) | a kind of the `state` genus with `producers: [code]`; the sender is the worker from the credentials; at most once a minute per (worker, kind), plus on a change of `phase` | STA-1, MSG-14 |
| `current` | `replaced` | P1 `msg.send` of the same worker with the same kind | a new value in the same inbox; in the same transaction it becomes `current` | MSG-9, MSG-14 |
| `current` | `stale` | the background loop `inbox` | the value is older than the kind's `stale_after` | MSG-12 |
| `stale` | `replaced` | P1 `msg.send` of the same worker with the same kind | the new value becomes `current` | MSG-14 |
| `current`, `stale` | `removed` | P2 `worker.revoke`; `apply` removed the kind or the receiving role | the value of the revoked worker is no longer shown as a state | STA-2, MSG-14 |

## Invariants

- Only the core changes the state of an entry. Neither the model, nor worker code, nor an external program creates entries bypassing the sending checks or changes an entry's state, period, address, mark, untrusted flag and core fields (MSG-1).
- The envelope does not change after publication: only the state, the hand-out count and the lease change. The note lives only in the deletable `content` class until the end of the entry's period and does not get into logs (MSG-3, MSG-13).
- An entry lies in exactly one inbox of one tenant; the copies of the `all` delivery are separate entries (MSG-4, MSG-14).
- An attempt's messages become visible to recipients only after its `complete`, and the entries it read are acknowledged in the same transaction; an attempt without `complete` publishes nothing (MSG-9).
- An entry of the `report` genus is leased by at most one attempt at any moment. After `acked`, `dead`, `expired` or `discarded` it is never handed out.
- There are at most three returns with a hand-out count; the third makes the entry `dead` (MSG-9).
- The `staged`, `queued` and `leased` entries take space in the inbox and in the sender's share, so the check at sending still holds at publication; old entries are not evicted silently (MSG-15).
- An entry above the mark of the reading attempt is not handed out; reading does not lower the mark and carries the untrusted flag over to the reader (MSG-6).
- The content of an entry reaches the model only as the output of a reading tool that the model called itself, in the untrusted text wrapper; an entry is not inserted into a running attempt (MSG-8).
- An expired entry is not handed out, wakes nobody and creates no tasks and notifications (MSG-12).
- For the `state` genus every inbox has at most one `current` entry per pair (sending worker, kind); it does not affect work hand-out, leases, holds, budgets and alarms (MSG-14, STA-2).
- Viewing inboxes by the farm manager's reading bot does not lease an entry, does not acknowledge it, does not count as a hand-out and wakes nobody; every such read is written to the read log (MGR-25, MGR-28).

## Versions

Messages between bots are part of v0.1 and in the order of work come after the core and the first bot (D-71, D-80). The `core` profile does not keep inboxes: an implementation that claims only that profile creates no states of this cycle.

| Version | What appears |
|---|---|
| v0.1 | all states and transitions above; P1 `msg.send`, `msg.receive`, `msg.ack`, `msg.nack`; P3 `send_message`, `inbox_read`; C1 `msg.stage`, `msg.publish`, `msg.lease`, `msg.ack`, `msg.nack`, `msg.expire`, `state.put`; the background loop `inbox`; the sender's bot identity in the store record; viewing inboxes by the farm manager's reading bot; a hold of the `bot_group` scope with the effect `no_message` for the group's inboxes; `traceparent` in the envelope (OBS-2, D-94) |
| v0.2 | P2 `msg.list`, `msg.show`, `inbox.status` (read only, they create no transitions) |
