---
status: accepted
date: 2026-10-14
decision-makers: skeleton owner
---

# A skeleton from the model; contracts only at the boundaries

## Context and Problem Statement

botaru is a skeleton of a bot farm, not a product (D-13): it must say what any implementation keeps without choosing the implementation. The question is what the skeleton consists of and where it needs checked contracts. Every port with a full contract costs schemas, fixtures and suites, and most ports have exactly one implementation, the core itself.

## Decision Drivers

- the goals and constraints of [goals.md](../goals.md) are fixed; everything else is a means that may be simplified (D-114 (1), (2));
- any implementation that passes the checks is a farm (C10);
- one source per fact, with references checked by `ci/`;
- foreign code (workers, channels, tool plugins, the model provider, the monitoring system) needs a contract it can be tested against;
- extension without changing the core (C9, D-02).

## Considered Options

- the model first: concepts, lifecycles and rules in a registry, and checked contracts only where foreign code meets the farm;
- the model first with a checked contract for every port, the shape before D-114 (4 layers and 13 ports with rules, D-38);
- code first: the reference implementation defines the farm, and documents describe it afterwards.

## Decision Outcome

Chosen option: "the model first, contracts only at the boundaries", because it keeps the rules every implementation must hold in one registry while paying for checked contracts only where someone else's code is on the other side. The skeleton has four layers (model, ports, contracts and checks, implementations, see [architecture.md](../architecture.md)). Contract boundaries are P1 and E1, P3 with E3, P4 and E5; the other ports are descriptions of how the core is built (D-114 (4), applying D-46). The common edge port E5 talks of "a monitoring system", and product names stay in bindings, the E5 sheet and examples (D-66).

### Consequences

- Good, because a rule exists once, in `conformance/requirements.yaml`, and every rule names the goal it serves.
- Good, because an implementation in any language is a farm if it passes the suite of the profiles it claims.
- Good, because an internal part of the core can be rebuilt without a contract change.
- Bad, because internal descriptions are not checked by a suite, so a mistake there shows only in the boundary checks or in review.
- Bad, because a contract change still needs an ADR and an additive change inside a version, which slows quick experiments.

### Confirmation

Rules of the registry (texts in [rules.md](../../conformance/rules.md)):

- MAN-1, the manifest wrapper and `apiVersion`: fixture and lint `lint:manifests/wrapper`.
- MAN-3, only additions inside a version: lint `lint:manifests/additive` and the CI step `ci/check_additive.py`.
- MAN-4, drivers and implementation kinds do not touch permissions: lint `lint:manifests/driver-config`.
- MAN-8, the implementation declares the skeleton version and profiles: lint `lint:manifests/conforms-to`; the black-box runner checks only the claimed profiles.
- MAN-11, a core rejects what it does not implement: black-box `blackbox:core/apply-later-version`.

The skeleton checks `ci/check_text.py` (rule numbers, links, product names in rule texts) and `ci/check_crossrefs.py` (port sheets against the registry) hold the "one source per fact" side.

## Pros and Cons of the Options

### The model first, contracts only at the boundaries

- Good, because checks go where trust ends: workers, channels, plugins, the provider and the monitoring system.
- Good, because simplification is allowed by rule: a rule may be retired or deferred with its history kept (D-114 (2), (3)).
- Bad, because the line between a boundary and a description must be kept by hand in [spec/ports/README.md](../../spec/ports/README.md).

### A checked contract for every port

- Good, because every part of the core would be replaceable by contract.
- Bad, because ports without a second implementation carried schemas and fixtures nobody used; D-46 already turned such a port into a description.

### Code first

- Good, because there is less text to keep in step.
- Bad, because a second implementation would have no contract to meet, against D-13 and C10.

## More Information

Decisions D-38, D-66, D-114, also D-13, D-46 and D-02. Documents: [architecture.md](../architecture.md), [goals.md](../goals.md), [extending.md](../extending.md), [spec/versions.md](../../spec/versions.md).
