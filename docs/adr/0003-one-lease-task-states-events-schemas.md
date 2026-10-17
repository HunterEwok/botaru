---
status: accepted
date: 2026-10-17
decision-makers: skeleton owner
---

# One lease for all work; task states; events; schemas

## Context and Problem Statement

The core hands out four kinds of work through P1 `claim`: a task attempt, a tool call, a delivery and an erasure step. Each needs an answer to "who holds it now", and a late holder must be harmless. The farm also needs names for task states, an event envelope and a schema language for data and the P1 wire format. This decision has no row of its own in [decisions.md](../decisions.md): it is written in the skeleton text, in [architecture.md](../architecture.md) ("Boundaries and descriptions", "Contract formats and versions"), [tasks-and-delivery.md](../tasks-and-delivery.md), [task-language.md](../task-language.md) and the lifecycles `spec/lifecycles/lease.md` and `spec/lifecycles/task.md`.

## Decision Drivers

- a process can sleep through a pause longer than its lease and wake without knowing;
- the toolhost, the channel and the sink should speak the same protocol as the agent;
- borrow building blocks rather than invent a language ([task-language.md](../task-language.md));
- one source of types, checked by `ci/`.

## Considered Options

- one handout mechanism for all work kinds: a lease with a fencing token, pulled through `claim`;
- a separate mechanism per kind, with the core pushing tool calls and deliveries to their executors;
- task states with waiting states (`input_required`, `auth_required`) next to the A2A terminal states;
- an own event envelope and own schema notation instead of CloudEvents 1.0, JSON Schema 2020-12 and OpenAPI 3.1.

## Decision Outcome

Chosen option: "one lease for all work, A2A state names without waiting states, CloudEvents 1.0, JSON Schema 2020-12 and OpenAPI 3.1 for P1", because one mechanism makes every edge speak the same protocol and standard formats need no farm-specific tooling. Every `claim` creates a lease with a growing `lease_no` and a token `(epoch, work_id, lease_no, worker_id)`; the store answers `fenced` and writes nothing for a stale token. Task states are `submitted`, `working`, `completed`, `failed`, `canceled`, `rejected`; a final task is never restarted. Events are CloudEvents 1.0 with the catalog `spec/bindings/events/catalog.yaml`; data, bodies and manifests are JSON Schema 2020-12; P1 over HTTP is OpenAPI 3.1 generated from the operation map `spec/ports/operations.yaml`.

### Consequences

- Good, because a late `complete` after `kill -9` is fenced, and the second attempt finishes.
- Good, because workers need no inbound port, and the core needs no trust in their capacity.
- Good, because an attempt never waits: waiting lives in core records, which keeps leases short.
- Bad, because fencing protects only the farm store; an external write needs a work id of its own.
- Bad, because a bot that needs a person's answer must end and be continued by a new task (see [0004](0004-deployment-api-version-idempotency.md)).

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

- Good, because one fencing rule covers attempts, tool calls, deliveries and erasure steps.
- Bad, because every edge process must long-poll `claim` and renew its lease.

### A mechanism per kind

- Good, because a push to a toolhost can be faster than a poll.
- Bad, because each executor would need an inbound port and its own "who holds it" rule.

### Waiting states

- Good, because A2A clients know `input_required`.
- Bad, because a waiting attempt holds a lease and a runtime for an unbounded time.

### Own envelope and notation

- Bad, because every tool around the farm would need a farm-specific parser.

## More Information

Skeleton text: [architecture.md](../architecture.md), [tasks-and-delivery.md](../tasks-and-delivery.md), [coordination.md](../coordination.md) ("The pool and the dispatcher"), `spec/ports/p1-worker-protocol.md`, [spec/versions.md](../../spec/versions.md). Related decisions D-41 (no `input_required`) and D-43 (idempotency).
