---
status: accepted
date: 2026-10-07
decision-makers: skeleton owner
---

# Deployment; `apiVersion`; idempotency scope; no `input_required`

## Context and Problem Statement

Four small decisions shape every installation and every client of the farm. How the farm is packaged, which version manifests carry before release 1.0.0, in which scope an idempotency key is unique, and what a bot does when it needs an answer from a person. They touch the deployment description, the manifest wrapper, P1 `enqueue` and the task lifecycle.

## Decision Drivers

- one or two people operate the farm, on one server or a few;
- manifest fields may still change before 1.0.0, and this must be visible;
- a repeat of a request must not create a second task, and must not reveal another tenant's or person's task;
- an attempt never waits (see [0003](0003-one-lease-task-states-events-schemas.md)).

## Considered Options

- packaging in containers with docker compose (D-08), or processes installed on the hosts without containers;
- manifests at `botaru/v1alpha1` until 1.0.0 and `botaru/v1` after it (D-44), or `botaru/v1` from the start;
- the idempotency key unique in `(tenant, idempotency_key)` and returned only to the same initiator with the same body (D-43), or a key unique per initiator;
- a clarification as a new task in the same conversation (D-41), or an `input_required` state in which the attempt waits.

## Decision Outcome

Chosen option: "containers and compose; `botaru/v1alpha1`; `(tenant, idempotency_key)`; a clarification is a new task", because each of them keeps a rule simple for the core and safe for the sender. The topology is data in `deploy/compose.example.yaml`, which the lint checks. A repeat with the same key gives the first task only to the same initiator with the same body; otherwise `idempotency_conflict` with no sign of the other task, and a deleted task leaves a tombstone so the key stays taken. A bot that needs something from a person answers with a question and ends; the person's reply becomes a new task linked by `continues`.

### Consequences

- Good, because the deployment description is data the lint checks, not a runbook.
- Good, because the alpha `apiVersion` tells implementers that manifest fields may change, with a changelog entry.
- Good, because a key cannot be used to probe for another person's tasks.
- Good, because no attempt holds a lease while a person thinks.
- Bad, because a conversation of several turns is several tasks, and context is carried by the session and `refs`.
- Bad, because the move to `botaru/v1` at 1.0.0 is a one-time change of every manifest.

### Confirmation

- DEP-10, the deployment is described by data and checked by lint: lint `lint:deploy/two-tenants-one-host`, `lint:deploy/toolhost-on-farm-host`, fixture `manifests/deploy`.
- MAN-1, the wrapper and `apiVersion` `botaru/v1alpha1`: fixture and lint `lint:manifests/wrapper`.
- IDM-1, the task idempotency key: black-box `blackbox:core/idempotency-key`.
- RET-3, tombstones keep uniqueness: black-box `blackbox:core/tombstone`.
- CNT-4, an attempt never waits and there is no `input_required`: black-box `blackbox:core/stall`.
- CNT-5, a person's answer is linked by a reply: fixture and black-box `blackbox:channel/followup-window`.

## Pros and Cons of the Options

### Containers and compose

- Good, because one file describes the processes, networks, volumes and secrets per process.
- Bad, because larger installations will want an orchestrator, which the skeleton leaves to the deployment.

### `botaru/v1` from the start

- Bad, because it would promise stability of fields that are still being assembled ([spec/versions.md](../../spec/versions.md)).

### A key per initiator

- Bad, because the scope of uniqueness would depend on who asks, and the tenant wall would not bound it.

### An `input_required` state

- Good, because it matches a state that A2A defines.
- Bad, because the attempt, its lease and its runtime would wait for an unbounded time.

## More Information

Decisions D-08, D-44, D-43, D-41. Documents: [tasks-and-delivery.md](../tasks-and-delivery.md), [operations.md](../operations.md), [spec/versions.md](../../spec/versions.md), `spec/lifecycles/task.md`.
