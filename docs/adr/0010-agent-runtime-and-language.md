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
- sessions, memory and transcript compression should stay in core stores, under erasure and the tenant wall;
- other runtimes must stay possible behind E4;
- the implementation must not change skeleton files, and contract changes go through the skeleton rules.

## Considered Options

- an own model loop over the provider API through P4, without a ready-made agent (D-93);
- a ready-made agent framework as the runtime, declaring its deviations from the farm model in `hello`;
- the core in Go (D-91), or in another language;
- the implementation in this repository under `impl/` (D-92), or in a separate repository.

## Decision Outcome

Chosen option: "an own loop in Go over P4; the core in Go in `impl/`", because an own loop needs no runtime deviations: sessions and memory are kept by the core in C3 and C4, there are no built-in tools to turn off, and P4 sees every call. The E4 port stays open: another runtime declares its deviations with values from `spec/model/runtime-deviations.yaml`, and each deviation turns on its non-waivable replacement rule. Ports, plugins, channel drivers and data programs may use any language, because they talk to the core only through P1, P2 and MCP (D-91). The reference implementation lives in `impl/` under the AGPL, does not edit skeleton files, and passes the `conformance/` suite (D-92).

### Consequences

- Good, because the reference agent keeps the farm model for `session`, `memory`, `catalog` and `built_in_tools` without exceptions.
- Good, because a change of the skeleton and of the implementation can be reviewed together, while the skeleton rules still govern contracts.
- Bad, because an own loop has to implement tool use, transcript compression and stopping by limits itself.
- Bad, because a second runtime is checked only through its `hello` declaration and the black-box suite, not by the reference.

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

- Good, because nothing in the runtime can bypass P3 or P4.
- Bad, because the loop is code the project maintains.

### A ready-made agent framework

- Good, because tool use and compression come ready.
- Bad, because its own sessions, memory and built-in tools would be deviations, each with an erasure duty and a rule, and built-in tools must be off.

### Go or another language

- Good, because the core, `farmctl`, the black-box runner, the channel adapters and the runtime share one language and one test suite.
- Bad, because a contributor to the core must work in Go; ports, plugins and drivers may still use any language (D-91).

### A separate repository

- Bad, because skeleton and implementation would drift, and a contract change would be reviewed in two places.

## More Information

Decisions D-93, D-91, D-92, also D-45, D-83, D-89. Documents: [architecture.md](../architecture.md) ("The agent runtime"), `spec/ports/e4-agent-runtime.md`, [impl/README.md](../../impl/README.md). Related: [0005](0005-gateways-in-core-internal-ports.md).
