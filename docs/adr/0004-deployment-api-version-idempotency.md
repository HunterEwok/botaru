---
status: accepted
date: 2026-10-07
decision-makers: skeleton owner
---

# Deployment; `apiVersion`; idempotency scope; no `input_required`

## Context and Problem Statement

Four small decisions shape every installation and every client of the farm: how the farm is packaged (D-08), which version manifests carry before release 1.0.0 (D-44), in which scope an idempotency key is unique (D-43), and what a bot does when it needs an answer from a person (D-41). They touch the deployment description, the manifest wrapper, P1 `enqueue` and the task lifecycle.

## Decision Drivers

- the deployment description is data the lint checks ([architecture.md](../architecture.md), DEP-10);
- the fields of `botaru/v1alpha1` manifests may change in any 0.y.z release, visible in the changelog ([spec/versions.md](../../spec/versions.md));
- a repeat of the same request does not create a second task but returns the first one (D-43);
- an attempt never waits (CNT-4, see [0003](0003-one-lease-task-states-events-schemas.md)).

## Considered Options

- packaging in containers with docker compose (D-08); no alternative is recorded;
- manifests at `botaru/v1alpha1` before release 1.0.0 and `botaru/v1` after it (D-44); no alternative is recorded;
- the idempotency key unique in `(tenant, idempotency_key)`, a repeat returned only to the same initiator with the same body (D-43); no alternative is recorded;
- a clarification as a new task in the same conversation (D-41); the alternative, an `input_required` mode in which the attempt waits, is excluded by D-41 itself: "There is no `input_required` mode".

## Decision Outcome

Chosen option: "containers and compose; `botaru/v1alpha1`; `(tenant, idempotency_key)`; a clarification is a new task", because these are the decisions D-08, D-44, D-43 and D-41. The topology is data in `deploy/compose.example.yaml`, which the lint checks. A repeat with the same key gives the first task only to the same initiator with the same body; otherwise `idempotency_conflict` without any sign of the other task (IDM-1), and a deleted task leaves a tombstone so the key stays taken (RET-3). A bot that needs something from a person answers with a question and ends ([tasks-and-delivery.md](../tasks-and-delivery.md)); the person's reply becomes a new task linked by `continues` (CNT-5).

### Consequences

- Good, because the deployment is checked by lint, not by a reviewer (DEP-10).
- Good, because a key reveals nothing about another initiator's task (IDM-1).
- Good, because no attempt holds a lease while a person thinks (CNT-4).
- Bad, because the manifest fields are an alpha version and may change before 1.0.0 ([spec/versions.md](../../spec/versions.md)).

### Confirmation

- DEP-10, the deployment is described by data and checked by lint: lint `lint:deploy/two-tenants-one-host`, `lint:deploy/toolhost-on-farm-host`, fixture `manifests/deploy`.
- MAN-1, the wrapper and `apiVersion` `botaru/v1alpha1`: fixture and lint `lint:manifests/wrapper`.
- IDM-1, the task idempotency key: black-box `blackbox:core/idempotency-key`.
- RET-3, tombstones keep uniqueness: black-box `blackbox:core/tombstone`.
- CNT-4, an attempt never waits and there is no `input_required`: black-box `blackbox:core/stall`.
- CNT-5, a person's answer is linked by a reply: fixture and black-box `blackbox:channel/followup-window`.

## Pros and Cons of the Options

### Containers and compose

- Good, because the topology is one data file, `deploy/compose.example.yaml`, with the OS user, networks, volumes and named secrets of each process, which the lint checks (DEP-10).

### `botaru/v1alpha1` until 1.0.0

- Good, because the version says that the fields are not yet covered by the compatibility promise, and `botaru/v1` after 1.0.0 follows the same addition rule ([spec/versions.md](../../spec/versions.md)).
- Bad, because a change of manifest fields is possible in any 0.y.z release ([spec/versions.md](../../spec/versions.md)).

### `(tenant, idempotency_key)`

- Good, because a repeat does not create a second task but returns the first one (D-43), and only to the same initiator with the same body (IDM-1).
- Good, because deletion does not release the key (RET-3).

### A clarification is a new task

- Good, because an attempt never waits; waiting lives in core records, and the core enqueues the continuation ([tasks-and-delivery.md](../tasks-and-delivery.md)).
- Bad, because the reply is linked only if the conversation and tenant are the same and the person replying is the initiator or may read the task (CNT-5).

## More Information

Decisions D-08, D-44, D-43, D-41. Documents: [tasks-and-delivery.md](../tasks-and-delivery.md), [operations.md](../operations.md), [spec/versions.md](../../spec/versions.md), `spec/lifecycles/task.md`.
