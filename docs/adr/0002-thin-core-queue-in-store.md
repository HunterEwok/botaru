---
status: accepted
date: 2026-10-04
decision-makers: skeleton owner
---

# A thin core, the queue in the store

## Context and Problem Statement

The farm hands work to agents, jobs, toolhosts, channels and sinks for several tenants. It needs one place where "who holds the work", permissions, money and the audit are decided atomically. The question is where the task queue lives and how heavy the core is. It touches C1 TaskStore, P1 `claim`, the dispatcher and the message inboxes.

## Decision Drivers

- a task transition, its event, deliveries, continuations and usage must be one transaction;
- the core must not trust a worker's report of its capacity or liveness;
- one or two people operate the farm, so every extra stateful system is a cost;
- the store engine stays replaceable behind C1.

## Considered Options

- a thin core with the task queue in the store, reference PostgreSQL;
- a NATS message bus between the core and the workers;
- a farm of separate agent gateways, each holding its own work.

These three were compared for D-03.

## Decision Outcome

Chosen option: "a thin core with the queue in the store", because the queue then sits in the same transaction as the lease, the permission check and the usage line, and there is no second stateful system to run. Tasks in `submitted` in C1 are the pool, partitioned by tenant; workers pull work through P1 `claim`, and the dispatcher lives inside `claim` ([coordination.md](../coordination.md)). Bot messages are published in the same transaction as a successful `complete`. The core is thin: it decides, and everything else is a worker at the edge.

### Consequences

- Good, because a transition and its consequences are one store transaction, with no lost or doubled work.
- Good, because workers need no inbound port, and the core needs no knowledge of their capacity.
- Good, because the store clock is the only clock for deadlines.
- Bad, because the store is a single point the whole farm depends on; its loss means `not_ready`, and backups and restore need their own rules.
- Bad, because there is no broker: events are the CloudEvents catalog plus P1 `emit` and C1 `event.*`, and a separate EventStream port appears only together with a broker ([spec/ports/README.md](../../spec/ports/README.md)).

### Confirmation

- DSP-1, the pool is the C1 queue and work goes only through `claim`: lint and black-box, fixture `fx-dsp-09`.
- DLV-2, a transition and its consequences in one transaction: black-box `blackbox:core/transition-one-tx`.
- MSG-9, publishing and acknowledgement together with `complete`: black-box, fixtures `fx-msg-03`, `fx-msg-04`.
- STA-2, a self-report is informational and liveness is the core's: fixture and black-box `fx-sta-01`.
- TIM-1, one clock, the store clock: black-box, fixtures `time/`.
- DEP-8, only the core has the store: lint and black-box `blackbox:core/job-no-store`.
- TEN-3, only its own tenant takes work: black-box `blackbox:core/claim-own-tenant`.

## Pros and Cons of the Options

### Queue in the store

- Good, because one transaction covers the queue and every decision.

### NATS bus

- Bad, because the lease, the permission check and the usage line would span two systems; [coordination.md](../coordination.md) ("Why it works this way") keeps the dispatcher inside `claim`: "no broker, no second stateful system".

### Separate agent gateways

- Bad, because "who holds the work" and the money would be decided outside the core, while [architecture.md](../architecture.md) makes the core "the only place where decisions are made".

## More Information

Decision D-03, refined by D-38; related D-14 (the pool and the dispatcher). Documents: [architecture.md](../architecture.md), [coordination.md](../coordination.md), `spec/ports/c1-task-store.md`, [spec/ports/README.md](../../spec/ports/README.md) ("What is deliberately not a port").
