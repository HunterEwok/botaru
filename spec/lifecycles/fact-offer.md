# Lifecycle: fact offer

## In short

A fact offer (`FactOffer`) is created by the core when an identity retires or changes its binding: it contains the active verified facts of this identity and exactly one recipient, its successor (MEM-7). The successor itself chooses what to take, with a `memory.pick.v1` task in a clean context (MEM-8). A person does not approve the handover, and the retiring identity takes no part in the choice. The farm manager gets a separate offer for the section of each tenant. Schema: `spec/model/fact-offer.schema.json`; only the core writes the record.

## States

Offer:

| State | Meaning | Terminal |
|---|---|---|
| `open` | The offer waits for the recipient or its choice, no longer than `open_until` (the end of the seal of the retiring identity) | no |
| `closed` | The recipient took facts, declined, or the deadline passed | yes |

Recipient within the offer:

| State | Meaning | Terminal |
|---|---|---|
| `waiting` | There is no successor yet: the seat is free or the worker is not enrolled yet | no |
| `pending` | The choice task is enqueued | no |
| `picked` | The recipient took at least one fact; the copies are written with `inherited` | yes |
| `declined` | The recipient took 0 facts | yes |
| `expired` | The choice task failed after 3 retries, or the successor did not appear before `open_until` | yes |

## Transitions

Offer:

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `open` | the core in the transaction of an identity's retirement or of a change of its binding | there are active facts; each one is rechecked; at most `offer.max` (100) | MEM-7, IDN-11 |
| `open` | `closed` | the core or the `identity` loop | the recipient is in a terminal state or `open_until` has passed; the facts of the retiring identity are wiped together with the seal | MEM-7, MEM-9 |

Recipient within the offer:

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | recipient `waiting` | the core | the successor by the MEM-7 table is not determined yet | MEM-7 |
| (none), recipient `waiting` | recipient `pending` | the core enqueues `memory.pick.v1` | the successor exists or appeared before `open_until` | MEM-8 |
| recipient `pending` | recipient `picked` | P1 `complete` of the choice task | `take` is not empty; every taken fact is rechecked against the recipient's rights | MEM-8 |
| recipient `pending` | recipient `declined` | P1 `complete` of the choice task | `take` is empty | MEM-8 |
| recipient `pending` | recipient `expired` | the core | the task failed after 3 retries | MEM-8 |
| recipient `waiting` | recipient `expired` | the `identity` loop | `open_until` passed without a successor | MEM-7 |

## Invariants

- There is one recipient, the successor by a closed table; colleagues of the same role do not get the facts (MEM-7).
- The offer and the recipient are always in one tenant; facts do not cross tenants (GOV-16).
- An offer contains only facts: core records, the track record, sessions and the inbox are never offered.
- The recipient takes at most `take_max` (50) and at most the free space in its own space.
- The input of the choice task is closed: the task does not set `untrusted_seen` and does not see whom the facts come from.
- People see only numbers in P2 and in the summary: how many were offered, how many were taken.

## Versions

| Version | What appears |
|---|---|
| v0.1 | The whole cycle of the offer and the choice |
