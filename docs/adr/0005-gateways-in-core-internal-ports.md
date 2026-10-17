---
status: accepted
date: 2026-10-14
decision-makers: skeleton owner
---

# P3 and P4 inside the core; internal ports as descriptions

## Context and Problem Statement

The agent runtime needs tools and a model, and both carry risk: a tool can write on a production machine, and a model call spends money on a provider key. The question is where the tool gateway P3 and the model gateway P4 live, and what happens to ports that have only one implementation. It touches P3, P4, E4, the trust boundary and the list of contract boundaries.

## Decision Drivers

- the runtime must hold neither the worker token nor the provider key;
- money and the tool catalog are decided by code in the trusted boundary;
- a port without a second implementation should not carry a contract nobody tests.

## Considered Options

- P3 and P4 as part of the core, with no intermediary between the runtime and the core (D-45);
- a separate proxy between the farm and the providers, which D-45 records as not needed;
- every port as a mandatory checked contract, or a port without a second implementation as a description (D-46, applied by D-114 (4)).

## Decision Outcome

Chosen option: "P3 and P4 inside the core; a port that is not a boundary with foreign code is a description", because the gateways must sit in the same transaction as the reserve, the charge and the catalog check, and a contract is worth checking only where foreign code is on the other side. P3 is the only source of tools for the runtime, an MCP server inside the core; P4 holds the provider keys and reserves and charges every call. The runtime reaches only P3, P4 and its adapter. P2, C1 to C4, E2, E4, the test-control port, the consent adapter and the background loops are descriptions; P1, E1, P3 with E3, P4 and E5 keep checked contracts ([spec/ports/README.md](../../spec/ports/README.md)).

### Consequences

- Good, because a runtime that "forgot" to turn off a built-in tool fails at the gateway instead of gaining a capability.
- Good, because spending is reserved and charged whatever the runtime does.
- Good, because only P4 holds the provider key (GW-3), and the runtime has neither it nor the worker token (DEP-5).
- Bad, because a provider beyond the reference needs its own binding (`spec/bindings/model-providers/README.md`, D-85).
- Bad, because a description is not checked by a suite; a second implementation of such a part is checked only through the boundaries.

### Confirmation

- GW-1, the model request matches the P3 catalog and the field allowlist: black-box `blackbox:model/provider-server-tools`, schema fixtures `schemas/valid/security/p4-anthropic-messages-request.yaml` and `schemas/invalid/security/p4-anthropic-messages-request.yaml`.
- GW-2, worst-case reserve and charging by the provider response: black-box `blackbox:core/usage-rows`.
- GW-3, only P4 has the provider key: black-box and lint.
- INJ-9, the runtime has only P3 tools: black-box `blackbox:core/runtime-only-p3-tools`.
- DEP-5, a runtime without keys or extra paths: lint and black-box.
- MAN-8, the implementation declares the profiles that the runner checks: lint `lint:manifests/conforms-to`.

`ci/check_crossrefs.py` checks that every operation of a boundary sheet is in `spec/ports/operations.yaml`.

## Pros and Cons of the Options

### Gateways inside the core

- Good, because P4 reserves and charges every call whatever the runtime does, and rejects a request with a tool outside the P3 catalog ([architecture.md](../architecture.md), "The agent runtime").

### A separate proxy

- Bad, because D-45 records it as not needed: the gateways are part of the core, and only P4 holds the provider key (GW-3).

### Every port a checked contract

- Bad, because a port without a second implementation carries a contract nobody else implements (D-46); a schema of an internal page that nothing checked was removed ([spec/ports/README.md](../../spec/ports/README.md)). D-80 placed the turn in the next release after v0.2, and D-114 applied it in v0.4.

## More Information

Decisions D-45, D-46, D-80, D-114. Documents: [architecture.md](../architecture.md), [models-and-money.md](../models-and-money.md), `spec/ports/p3-tool-gateway.md`, `spec/ports/p4-model-gateway.md`, `spec/ports/e4-agent-runtime.md`. Related: [0001](0001-skeleton-from-the-model.md), [0010](0010-agent-runtime-and-language.md).
