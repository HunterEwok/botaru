---
status: accepted
date: 2026-10-13
decision-makers: skeleton owner
---

# Agent runtime; implementation language

## Context and Problem Statement

The skeleton needed a reference implementation to prove that its contracts can be built and checked. Three choices had to be made: which agent runtime the reference agent worker uses, in which language the core is written, and where the code lives. It touches E4 AgentRuntime, P1 `hello` with its `runtime` declaration, P3, P4, C3, C4 and the `impl/` folder.

## Decision Drivers

- the runtime holds neither the worker token nor the provider key, and has only P3 tools (DEP-5, INJ-9);
- sessions, memory and transcript compression stay in core stores (D-93);
- other runtimes stay possible behind E4 (D-93);
- the implementation does not change skeleton files, and contract changes go through the skeleton rules (D-92).

## Considered Options

- an own model loop in Go over the model provider's API through P4 (D-93);
- a ready-made agent as the runtime: D-93 records the choice "without a ready-made agent", and E4 stays open to other runtimes, which declare their deviations in P1 `hello` (`spec/ports/e4-agent-runtime.md`);
- the core in Go (D-91); no other language is recorded as compared;
- the implementation in this repository under `impl/` (D-92); no other place is recorded as compared.

## Decision Outcome

Chosen option: "an own loop in Go over P4; the core in Go in `impl/`", because these are the owner's decisions D-93, D-91 and D-92. Conversation memory, sessions and transcript compression are kept by the core in its stores C3 and C4 (D-93). The E4 port stays open: another runtime declares its deviations with values from `spec/model/runtime-deviations.yaml`, and any value except the first turns on its non-waivable replacement rule ([architecture.md](../architecture.md), "The agent runtime"). Ports, plugins, channel drivers and data programs may use any language, because they talk to the core only through P1, P2 and MCP (D-91). The reference implementation lives in `impl/` under the AGPL and does not change skeleton files; a change of a contract goes through the skeleton rules (D-92). It passes the `conformance/` suite ([architecture.md](../architecture.md), "What the skeleton is").

### Consequences

- Good, because sessions and memory stay in core stores, under the erasure and tenant rules of C3 and C4 (D-93).
- Good, because a contract change made for the implementation goes through the skeleton rules, additive only inside a version (D-92, MAN-3).
- Bad, because the farm maintains its own model loop, transcript compression included (D-93).
- Bad, because a runtime with its own sessions or memory must declare them and support erasure search, or the core blocks it at `hello` (RT-1, RT-2, RT-5).

### Confirmation

- INJ-9, the runtime has only P3 tools: black-box `blackbox:core/runtime-only-p3-tools`.
- DEP-5, a runtime without keys or extra paths: lint and black-box.
- GW-1, P4 rejects a request with tools outside the P3 catalog or provider server tools: black-box `blackbox:model/provider-server-tools`.
- RT-1, a session inside the runtime only with erasure: black-box `blackbox:core/runtime-internal-purge`.
- RT-2, memory inside the runtime: black-box `blackbox:core/runtime-memory-untrusted`.
- RT-5, search in the runtime for erasure: black-box and impl, `blackbox:core/runtime-internal-match`, `impl:internal/core/TestHelloRuntimeDeclaration`.
- MAN-8, the implementation declares the skeleton version and profiles: lint `lint:manifests/conforms-to`.

The impl checks are Go tests in `impl/`; the black-box checks run against the implementation through `conformance/runner.md`.

## Pros and Cons of the Options

### An own loop over P4

- Good, because the runtime takes tools only from P3 and reaches the model only through P4 (`spec/ports/e4-agent-runtime.md`), and the core keeps sessions and memory (D-93).
- Bad, because the loop is code the project maintains (D-93).

### A ready-made agent

- Bad, because its built-in tools must be off (`built_in_tools` only `[]`, INJ-9), and its own sessions or memory would be deviations with their own non-waivable rules (RT-1, RT-2, RT-5).

### Go for the core

- Good, because ports, plugins, drivers and data programs may still use any language (D-91).

### `impl/` in this repository

- Good, because it is released under the AGPL like the skeleton, and the CLA keeps another license possible for the owner (D-92, D-89).
- Bad, because installation files (tenants, keys, machines, people) must stay outside the public repository (D-92).

## More Information

Decisions D-93, D-91, D-92, also D-45, D-83, D-89. Documents: [architecture.md](../architecture.md) ("The agent runtime"), `spec/ports/e4-agent-runtime.md`, [impl/README.md](../../impl/README.md). Related: [0005](0005-gateways-in-core-internal-ports.md).
