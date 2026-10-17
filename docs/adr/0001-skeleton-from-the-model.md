---
status: accepted
date: 2026-10-14
decision-makers: skeleton owner
---

# A skeleton from the model; contracts only at the boundaries

## Context and Problem Statement

botaru is a skeleton: contracts and abstractions, and its files need not build by themselves (D-13). It must say what any implementation keeps without choosing the implementation. The question is what the skeleton consists of and where it needs checked contracts. Every port with a full contract costs schemas, fixtures and suites, and a port that has no second implementation turns from a mandatory contract into a description (D-46).

## Decision Drivers

- the goals and constraints of [goals.md](../goals.md) are fixed; everything else is a means that may be simplified (D-114 (1), (2));
- any implementation that passes the checks is a farm (C10);
- one source per fact, with references checked by `ci/` ([architecture.md](../architecture.md), "What the skeleton is");
- foreign code (workers, channels, tool plugins, the model provider, the monitoring system) needs a contract it can be tested against (D-114 (4)).

## Considered Options

- the model first: concepts, lifecycles and rules in a registry, and checked contracts only where foreign code meets the farm (D-114 (4));
- the model first with a checked contract for every port, the shape before D-114: 4 layers and 13 ports with rules (D-38), until D-46 turned ports without a second implementation into descriptions and D-114 applied this in v0.4 to every port that is not a boundary with foreign code.

## Decision Outcome

Chosen option: "the model first, contracts only at the boundaries", because it keeps the rules every implementation must hold in one registry while paying for checked contracts only where someone else's code is on the other side. The skeleton has four layers (model, ports, contracts and checks, implementations, see [architecture.md](../architecture.md)). Contract boundaries are P1 and E1, P3 with E3, P4 and E5; the other ports are descriptions of how the core is built (D-114 (4), applying D-46). The common edge port E5 talks of "a monitoring system" (D-66): product names live only in bindings, the E5 sheet, [operations.md](../operations.md) and examples ([architecture.md](../architecture.md), "What the skeleton does not decide"; D-66 itself says the port sheet, docs/monitoring.md and examples; that file no longer exists, and architecture.md names operations.md in its place).

### Consequences

- Good, because a rule exists once, in `conformance/requirements.yaml`, and every rule names the goal it serves (D-114 (1)).
- Good, because simplification is allowed by rule: a rule may be retired or deferred with its history kept (D-114 (2), (3)).
- Good, because an implementation that passes the suite of the profiles it claims is a farm (C10).
- Bad, because internal descriptions are not checked by a suite of their own (D-114 (4)).

### Confirmation

Rules of the registry (texts in [rules.md](../../conformance/rules.md)):

- MAN-3, only additions inside a version: lint `lint:manifests/additive` and the CI step `ci/check_additive.py`.
- MAN-8, the implementation declares the skeleton version and profiles, and the runner checks only those: lint `lint:manifests/conforms-to`.
- MAN-11, a core rejects what it does not implement: black-box `blackbox:core/apply-later-version`.
- MON-1, alarms to people along one installation path: the monitoring system with the profile `monitor`, otherwise `alarm_notice` (D-66, D-88): fixture, lint and black-box `blackbox:core/alarm-path-monitor`, `blackbox:core/alarm-notice-no-monitor`.
- MON-12, alarm export to the monitoring system through E5 (D-66): fixture, lint and black-box `blackbox:monitor/export-stalled`.
- RET-4, erase operations in every port with content (D-66): adapter and black-box `blackbox:core/erasure-v01-ports`, fixture `erasure/erasure`.

The skeleton checks hold the "one source per fact" side: `ci/check_text.py` checks rule numbers, decision numbers, links and product names in rule texts (D-66); `ci/check_crossrefs.py` checks the operation map against the port sheets and schemas and that the rules named there are active.

## Pros and Cons of the Options

### The model first, contracts only at the boundaries

- Good, because checks go where foreign code is on the other side (C10, D-114 (4)).
- Bad, because the internal descriptions are not checked by a suite of their own (D-114).

### A checked contract for every port

- Good, because every part of the core had a contract.
- Bad, because a port without a second implementation carried a contract nobody else implemented, which is why D-46 turned such a port into a description.

## More Information

Decisions D-38, D-66, D-114, also D-13 and D-46. Documents: [architecture.md](../architecture.md), [goals.md](../goals.md), [extending.md](../extending.md), [spec/versions.md](../../spec/versions.md).
