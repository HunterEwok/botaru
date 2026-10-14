# Lifecycle: fact

## In short

A fact (`Fact`) is a verified record about the farm in a bot's memory of the `bot` scope, for example "the task type `pipeline.qc.v1` on `web1` runs 130 minutes at the 95th percentile". The model only proposes what to remember: the kind, the predicate and the subject from closed registries; the core chooses and computes the window, the evidence and the value (MEM-6). A fact has no free strings, so an instruction cannot be put into it. A fact is the only thing that passes from a retired bot to its successor (MEM-7). A person does not approve facts.

## States

| State | Meaning | Terminal |
|---|---|---|
| `active` | The fact is verified by code; the identity's tasks with `memory.read: bot` read it, and it can be offered to the successor | no |
| `stale` | The evidence was wiped or a recomputation disagreed with the value: the fact is neither read nor offered | no |
| `sealed` | The owning identity retired: no bot reads the fact, except for the recheck for the offer to the successor | no |
| `erased` | The fact is wiped in all stores | yes |

A refusal at creation (`fact_insufficient`, a schema error, a subject not from this tenant) creates no record: only the `fact.rejected` event remains.

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `active` | P1 `complete` of an attempt whose type declared `memory.facts: propose` | the schema, a subject of this tenant, at least `min_samples` pieces of evidence; at most 3 facts from an attempt; the attempt did not fail, was not canceled and is not `fenced`; with the attempt's `untrusted_seen` the fact gets `untrusted_origin: true` | MEM-6, MEM-3 |
| (none) | `active`, a copy with `inherited` | the core when the successor of a fact offer exists | the recheck against the successor's rights passed; `untrusted_origin` is kept | MEM-7 |
| `active` | `stale` | the core at a recheck (creating an offer, copying) | the evidence was wiped | MEM-6 |
| `active` | `stale` | the core at a recheck | the recomputed value disagrees with the record (`fact_mismatch`); no copy is made | MEM-6 |
| `active` | `erased` | the core when writing a new fact | the space has more than 100 active facts; the fact with the oldest `last_used_at` is evicted | MEM-6 |
| `active`, `stale` | `sealed` | the core in the transaction of an identity's retirement | IDN-11 | MEM-4 |
| `stale` | `erased` | the `retention` loop | the retention class period | MEM-9, RET-2 |
| `sealed` | `erased` | the `identity` loop enqueues an erasure request `bot_retired`, the `erasure` loop wipes | `seal_ttl` has passed; from v0.2 earlier, if the retention class period ended earlier (the `retention` loop, RET-10, MEM-10) | MEM-9, MEM-10, RET-7 |
| any | `erased` | the `erasure` loop | tenant deletion (P2 `tenant.delete`, the reason `tenant_delete`) or an erasure request for its scope | MEM-4, RET-7 |

## Invariants

- The model sets only the kind, the predicate and the subject; the core writes the window, the evidence, the value, the mark and the time (MEM-6).
- The subject is only a catalog resource, a task type, a dataset or a schedule of the same tenant; an identity, a seat, a principal and a proposal are never a subject.
- A fact does not cross tenants; the fact's mark equals the highest mark of its evidence, and the fact gets only into a task with a mark that is not lower (PRM-5).
- Reading a fact does not set `untrusted_seen`; tasks of the purposes `review` and `manage` get only facts with `untrusted_origin: false`.
- The facts block in a task's input shows the kind, the predicate, the subject, the value, the unit, the window and the age; the model does not see whom the fact was taken from (GOV-29).
- A commission seat holder does not write new facts (PRF-1), so from the opening of a case nobody from L can prepare a one-sided set of facts (GOV-12).
- A fact carries the flag `personal_seen` (AUD-5): the core sets it if the attempt that proposed the fact carried the flag; the flag stays with the fact also when it is handed to the successor, and an attempt that read such a fact gets the flag itself. The model neither sees nor removes the flag.
- A new kind or predicate is introduced only through an ADR.

## Versions

| Version | What appears |
|---|---|
| v0.1 | The whole cycle: creation, recheck, `stale`, the seal, the choice by the successor, wiping |
| v0.2 | Wiping of `stale` and sealed facts by the retention class period by the `retention` loop (RET-10, MEM-10) |
