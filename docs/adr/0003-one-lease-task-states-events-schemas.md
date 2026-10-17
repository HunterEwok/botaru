---
status: accepted
date: 2026-10-17
decision-makers: skeleton owner
---

# One lease for all work; task states; events; schemas

## Context and Problem Statement

The core hands out four kinds of work through P1 `claim`: a task attempt, a tool call, a delivery and a step of an erasure or search request at an edge store (`spec/lifecycles/lease.md`, `spec/ports/p1-worker-protocol.md`; FEN-1 states the lease rule for the first three). Each needs an answer to "who holds it now", and a late holder must be harmless. The farm also needs names for task states, an event envelope and a schema language for data and the P1 wire format. This decision has no row of its own in [decisions.md](../decisions.md): it is written in the skeleton text, in [architecture.md](../architecture.md) ("Boundaries and descriptions", "Contract formats and versions"), [tasks-and-delivery.md](../tasks-and-delivery.md), [task-language.md](../task-language.md) and the lifecycles `spec/lifecycles/lease.md` and `spec/lifecycles/task.md`.

## Decision Drivers

- a process can sleep through a pause longer than its lease and wake without knowing ([tasks-and-delivery.md](../tasks-and-delivery.md), "Fencing");
- the toolhost, the channel and the sink speak the same protocol as the agent (`spec/ports/p1-worker-protocol.md`);
- the farm borrows building blocks rather than inventing a language ([task-language.md](../task-language.md));
- one source of types: JSON Schema ([architecture.md](../architecture.md), "Contract formats and versions").

## Considered Options

- one handout mechanism for all work kinds: a lease with a fencing token, pulled through `claim`, with A2A state names, CloudEvents 1.0, JSON Schema 2020-12 and OpenAPI 3.1 for P1;
- waiting states of A2A (`input_required`, `auth_required`): not recorded as compared; the skeleton text excludes them: "There are no `input_required` or `auth_required` states" ([tasks-and-delivery.md](../tasks-and-delivery.md), CNT-4, D-41);
- pipeline languages (CWL, Nextflow, Snakemake) as the task language: [task-language.md](../task-language.md) names and rejects them, because they "describe a chain known in advance, not a goal for a model".

## Decision Outcome

Chosen option: "one lease for all work, A2A state names without waiting states, CloudEvents 1.0, JSON Schema 2020-12 and OpenAPI 3.1 for P1", because one mechanism makes every edge speak the same protocol ([architecture.md](../architecture.md)) and the formats are borrowed building blocks ([task-language.md](../task-language.md)). Every `claim` creates a lease with a growing `lease_no` and a token `(epoch, work_id, lease_no, worker_id)`; the token contains `worker_id` and the core's signature, and the core checks `worker_id` against the worker from `Authorization` (D-86 (2)). The store answers `fenced` and writes nothing for a stale token. Task states are `submitted`, `working`, `completed`, `failed`, `canceled`, `rejected`; a final task is never restarted. Events are CloudEvents 1.0 with the catalog `spec/bindings/events/catalog.yaml`; data, bodies and manifests are JSON Schema 2020-12; P1 over HTTP is OpenAPI 3.1 generated from the operation map `spec/ports/operations.yaml`.

### Consequences

- Good, because after `kill -9` the second attempt finishes and the first one's late `complete` is fenced ([tasks-and-delivery.md](../tasks-and-delivery.md)).
- Good, because the worker needs no inbound port, and the core does not need to know or trust the worker's capacity ([coordination.md](../coordination.md), "The pool and the dispatcher").
- Good, because an attempt never waits: waiting lives in core records (CNT-4).
- Bad, because fencing protects only writes to the farm store; an external write needs a work id of its own (FEN-7).
- Bad, because a bot that needs a person's answer must end and be continued by a new task (D-41, see [0004](0004-deployment-api-version-idempotency.md)).

### Confirmation

- FEN-1, a lease with a growing number and a token: black-box `blackbox:core/lease-counters`, `blackbox:core/ten-thousand-tasks`.
- FEN-2, a stale token is fenced without writes: black-box and impl, `lease/stale-complete`, `blackbox:core/kill-worker`, `impl:internal/core/TestFailRetriesThenExhausts`.
- FEN-4, cancellation through `heartbeat`: black-box `blackbox:core/cancel-via-heartbeat`.
- FEN-7, an external write carries the work id: lint, adapter and black-box, fixture `fx-fen-01`.
- CNT-4, an attempt never waits: black-box `blackbox:core/stall`.
- IDM-4, an event is unique by `source` and `id`, which the core sets: black-box `blackbox:core/event-source-id`.
- MSG-1, the message envelope is a CloudEvents 1.0 event: fixture and black-box `fx-msg-01`.
- MAN-3, only additions inside a version of a type, event or schema: lint `lint:manifests/additive`, `ci/check_additive.py`.

`ci/check_schemas.py` validates the schemas, and `ci/gen_openapi.py --check` keeps the OpenAPI description equal to the operation map.

## Pros and Cons of the Options

### One lease for all work

- Good, because the toolhost, the channel and the sink speak the same protocol as the agent (`spec/ports/p1-worker-protocol.md`).
- Good, because "who holds the work" is answered by the lease and fencing ([coordination.md](../coordination.md)).

### Waiting states

- Bad, because an attempt would wait for a person, which CNT-4 forbids: waiting lives only in core records.

### Pipeline languages

- Bad, because they describe a chain known in advance, not a goal for a model; the farm runs such pipelines as programs ([task-language.md](../task-language.md)).

## More Information

Skeleton text: [architecture.md](../architecture.md), [tasks-and-delivery.md](../tasks-and-delivery.md), [coordination.md](../coordination.md) ("The pool and the dispatcher"), `spec/ports/p1-worker-protocol.md`, [spec/versions.md](../../spec/versions.md). Related decisions D-41 (no `input_required`), D-43 (idempotency) and D-86 (2) (the lease token).
