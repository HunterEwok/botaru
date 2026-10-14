# Lifecycle: personal memory record

Kind: `memory` (the kinds are listed in [the lifecycle index](README.md)).

## In short

A memory record of the `person` scope (`MemoryItem`) keeps what the bot remembered about a person, for example a preference for the answer format of a personal assistant. The model only proposes a record, and the decision of the person whose memory it is makes it active (MEM-3). Such memory exists only for roles without untrusted input (MEM-2). Deletion is real: `forget` deletes the record in all stores (MEM-4). Facts of bot memory live by their own cycle (`fact.md`).

## States

| State | Meaning | Terminal |
|---|---|---|
| `proposed` | The model proposed a record; the person has not decided yet | no |
| `active` | The person approved the record; the tasks of this role for this person read it | no |
| `discarded` | The person rejected the record or stayed silent until the card's deadline | yes |
| `superseded` | The person approved a new record with the same key | yes |
| `forgotten` | The record is really deleted | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `proposed` | P1 `memory.propose` | a role with `person` memory; the attempt has no `untrusted_seen`, otherwise the refusal `memory_untrusted` | MEM-2, MEM-3 |
| `proposed` | `active` | P1 `decide` with the subject `memory_record`: a button on the card in the person's direct chat | only the person whose memory it is decides | MEM-3, APR-4 |
| `proposed` | `discarded` | P1 `decide` with a refusal, or the `approval-expiry` loop at the card's deadline | the default deadline is 7 days, the outcome on silence comes from the decision registry row (APR-12) | MEM-3, APR-12 |
| `active` | `superseded` | approval of a new record with the same key | | MEM-3 |
| `proposed`, `active` | `forgotten` | a person's `/forget`, an erasure request (including on a person's offboarding and on tenant deletion), from v0.2 the tenant's `memory` period, if it sets one (the `erasure` and `retention` loops, RET-9, RET-10) | deletion in all stores, including runtime stores | MEM-4, RET-7, RET-10 |

## Invariants

- The core computes the space `(tenant, person, subject)`; the model's arguments do not set it (MEM-1).
- There is no background self-learning without approval; a skill written by the model goes into work only after a review by a person (MEM-3).
- A person's memory record never passes to another bot and never gets into a fact offer (MEM-7).
- A record carries the flag `personal_seen` (AUD-5): the core sets it if the attempt that proposed the record carried the flag, and an attempt that read such a record gets the flag itself. The model neither sees nor removes the flag.
- Early deletion goes only through an erasure request (RET-7).

## Versions

| Version | What appears |
|---|---|
| v0.1 | The whole cycle, except deletion by the retention class period and tenant deletion |
| v0.2 | Deletion by the tenant's `memory` period by the `retention` loop (RET-9, RET-10); tenant deletion (P2 `tenant.delete`) |
