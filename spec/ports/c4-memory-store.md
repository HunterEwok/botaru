# C4 MemoryStore

How the core is built, not a contract (C10, D-114).

## In short

C4 keeps long-term memory in the farm store by namespaces `(tenant, scope, subject)`, which the core computes (MEM-1). The `person` scope is a person's memory: a record becomes active only after that person's decision (MEM-3). The `bot` scope holds only core records and verified facts (D-35): the model proposes a fact (kind, predicate, subject), and the core computes the value and the evidence (MEM-6). A runtime that keeps memory itself declares it in P1 `hello` and follows RT-2.

## How it works

- `search` and `list` read by namespace; `propose` adds a `person` record, refused under `untrusted_seen` (MEM-3); `commit` and `discard` follow the person's decision, silence discards.
- `fact.verify` builds a fact of the closed schema from the registries and the evidence the core collects itself (MEM-6).
- When a bot leaves, `offer` and `take` copy the rechecked facts to the successor with the `inherited` mark (MEM-7); `seal` seals the memory of a retired identity for `seal_ttl` (MEM-4).
- `erase` deletes records and facts for real by a list of ids, by `(tenant, subject)`, by a retired identity (MEM-9) and from v0.2 by `(tenant)`, with a result in the erasure request (RET-7).
- `person` memory exists only for roles without untrusted input (MEM-2); facts do not cross tenants and their mark is not above the namespace ceiling (TEN-5).

## Where the details are

`spec/model/memory-item.schema.json`, `fact.schema.json`, `fact-offer.schema.json`, `fact-kinds.yaml`, `fact-predicates.yaml`; fixtures `conformance/fixtures/memory/`.
