# Lifecycle: fact offer

Kind: `memory` (the kinds are listed in [the lifecycle index](README.md)).

## In short

A fact offer (`FactOffer`) is created by the core when an identity retires or changes its binding: it contains the active verified facts of this identity and exactly one recipient, its successor: the new identity of the same worker, else the first identity enrolled in the same role, and for a seat holder the next holder of the seat. When the successor exists, code rechecks each fact with the successor's permissions and copies those that pass (MEM-7). Neither a person nor a model chooses the facts, and the retiring identity takes no part. The farm manager gets a separate offer for the section of each tenant. Schema: `spec/model/fact-offer.schema.json`; only the core writes the record.

## States

Offer:

| State | Meaning | Terminal |
|---|---|---|
| `open` | The offer waits for the successor, no longer than `open_until` (the end of the seal of the retiring identity) | no |
| `closed` | The facts were copied, or the deadline passed | yes |

Recipient within the offer:

| State | Meaning | Terminal |
|---|---|---|
| `waiting` | There is no successor yet: the seat is free or the worker is not enrolled yet | no |
| `copied` | The core rechecked the facts and copied those that passed, with `inherited` (possibly none) | yes |
| `expired` | The successor did not appear before `open_until` | yes |

## Transitions

Offer:

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `open` | the core in the transaction of an identity's retirement or of a change of its binding | there are active facts; each one is rechecked; at most `offer.max` (100) | MEM-7, IDN-11 |
| `open` | `closed` | the core or the `identity` loop | the recipient is in a terminal state or `open_until` has passed; the facts of the retiring identity are wiped together with the seal | MEM-7, MEM-9 |

Recipient within the offer:

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | recipient `waiting` | the core | the successor under MEM-7 does not exist yet | MEM-7 |
| (none), recipient `waiting` | recipient `copied` | the core, in the transaction where the successor exists or appears | the successor exists or appeared before `open_until`; every copied fact passed the recheck with the successor's permissions and fits the free room of its space | MEM-7 |
| recipient `waiting` | recipient `expired` | the `identity` loop | `open_until` passed without a successor | MEM-7 |

## Invariants

- There is one recipient, the successor under MEM-7; other identities of the same role do not get the facts.
- The offer and the recipient are always in one tenant; facts do not cross tenants (GOV-16).
- An offer contains only facts: core records, the track record, sessions and the inbox are never offered.
- A copy keeps `untrusted_origin` of its fact; the successor gets at most the free room in its own space, the most recently used facts first.
- No model takes part: there is no choice task.
- People see only numbers in P2 and in the summary: how many were offered, how many were copied.

## Versions

| Version | What appears |
|---|---|
| v0.1 | The whole cycle of the offer and the copy |
