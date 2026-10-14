# C4 MemoryStore

Sheet version: 0.1.0.

## In short

C4 stores long-term memory by namespaces `(tenant, scope, subject)`. There are two scopes. The `person` scope: the memory of a person (for example of their personal assistant); a record becomes active only after the person's decision. The `bot` scope: the memory of a bot, which holds only core records and verified facts (D-35); there are no free-form model notes. The model only proposes a fact (kind, predicate, subject), and the core computes the value and the evidence. The port is thin (D-46). The operations of the `person` scope exist in the reference table of the farm store from the start of v0.1; the sheet as the contract of a replaceable adapter and the fact operations enter v0.1 after the core and the first bot (D-71). A runtime with memory inside declares `memory: runtime_internal` and follows RT-2.

## Who calls and who implements

- **Called by** the core: on P1 `memory.search` and `memory.propose` from the E4 adapter, on `complete` (facts from the attempt result), in the fact picking task `memory.pick.v1`, in the `retention` and `erasure` loops.
- **Implemented by** the memory store adapter registered by name in the core deployment. Roles choose a memory scope, not a store.

## Operations

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| `search`, `list` | core | Search and list by namespace; the core computes the namespace | `data_class_exceeded` | v0.1 | MEM-1, TEN-5 |
| `propose` | core | Proposal of a `person` scope record; refused under `untrusted_seen` | `memory_untrusted` | v0.1 | MEM-3 |
| `commit`, `discard` | core on the decision of the person whose memory it is (P1 `decide`, subject `memory_record`) | The record becomes `active` or is discarded; if the person stays silent until the deadline, it is discarded | | v0.1 | MEM-3, APR-4 |
| `erase` | `erasure` loop on an erasure request (`forget`, person offboarding, a person's request) | Really deletes records and facts by a list of ids or by the scope `(tenant, subject)`, including sealed ones; the result goes into the erasure request. From v0.2 also the scope `(tenant)` and the `erasure` suite | | v0.1 | MEM-4, RET-7, RET-4 |
| `fact.verify` | core | Builds a fact of the closed schema: checks the kind, predicate and subject against the registries, collects the evidence itself and computes the value; no more than 3 facts from an attempt and 100 active in a namespace | `fact_insufficient`, `fact_mismatch` | v0.1 | MEM-6 |
| `offer` | core when a bot leaves | Offers active re-verified facts only to the successor; for the manager separately by the section of each tenant | | v0.1 | MEM-7 |
| `take` | core on the result of the `memory.pick.v1` task | Copies the chosen facts to the successor with the `inherited` mark after re-verification against the successor's permissions | `stale`, `fact_mismatch` | v0.1 | MEM-7 |
| `seal` | core on `retired` | Seals the memory of the retired identity for `seal_ttl` (30 days); only people with `admin:governance` read it | | v0.1 | MEM-4 |
| `erase` (identity id scope) | `erasure` loop on a `bot_retired` erasure request, which the `identity` loop enqueues at the end of `seal_ttl` | Erases what is sealed for the retired identity: records, facts and the track record; a tombstone remains | | v0.1 | MEM-9, RET-3, RET-7 |

## Guarantees

- MUST: the core computes the namespace `(tenant, scope, subject)`; model arguments do not set it (MEM-1).
- MUST: `person` memory exists only for roles without untrusted input; roles that read untrusted text work without it (MEM-2).
- MUST: a `person` record is active only after the decision of the person whose memory it is; there is no background self-learning (MEM-3).
- MUST: facts do not cross tenants, and their mark is not above the namespace ceiling (TEN-5).
- MUST: deletion is real in all stores, including runtime stores, and goes only through an erasure request (MEM-4, RET-7).

## Bindings

`spec/model/memory-item.schema.json`, `fact.schema.json`, `fact-offer.schema.json`, `fact-kinds.yaml`, `fact-predicates.yaml`; transfer of records in the `memory-item` format (JSON Lines); the suite `conformance/suites/memory` (v0.2).

## What the port does not do

It does not store model notes about the bot, sessions, correspondence and the track record as memory that passes to the successor; it does not accept fact values from the model.
