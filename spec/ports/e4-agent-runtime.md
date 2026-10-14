# E4 AgentRuntime

How the core is built, not a contract: the agent is a worker of the farm, and its boundary with the core is P1, with tools only through P3 and the model only through P4 (C10, D-114). The reference runtime is our own loop in Go over P4 (D-93).

## In short

An agent worker has two parts. The adapter holds the worker token, takes an attempt with P1 `claim(attempt)`, renews it with `heartbeat` and hands in the result with `complete` or `fail`. The runtime runs the model loop with only what the attempt work item gives it: the task body, the trusted instruction, the handles for P3 and P4, and from v0.2 the role's skills and `reply_locale` (SKL-2, DLV-18). It has neither the worker token nor the provider key (DEP-5).

## What the core requires of any runtime

The agent worker declares in the `runtime` field of P1 `hello` how its runtime performs the farm mechanisms, with values only from `spec/model/runtime-deviations.yaml`; any value except the first turns on its own non-waivable rule, and the core checks what it can see:

| Field | Values | What the core requires |
|---|---|---|
| `session` | `farm`, `runtime_internal`, `none` | `runtime_internal` only with `erasure: [erase, match]`, otherwise `blocked` (RT-1, RT-5) |
| `memory` | `farm`, `bridge`, `runtime_internal`, `none` | `runtime_internal` only in scope `person`, with record approval, without self-learning and with `erasure: [erase, match]` (RT-2, RT-5) |
| `model_attribution` | `per_attempt`, `per_profile` | `per_profile`: capacity 1, the handle lives only with a lease (RT-3) |
| `catalog` | `per_attempt`, `per_role` | `per_role`: the core checks everything task-dependent at every call (RT-4) |
| `built_in_tools` | only `[]` | anything else is `blocked`; P4 strips provider server tools independently (INJ-9, GW-1) |
| `skills` (from v0.2) | `farm`, `runtime_files` | `runtime_files`: a read-only skill folder with only the role's skills whose hash matches (SKL-2) |

Beyond the declaration:

- The runtime takes tools only from P3 and reaches the model only through P4, without fallback providers; it does not retry a P4 error, the core retries the task (GW-12).
- It stops on `cancel_requested` from `heartbeat` before the next heartbeat (FEN-4), and its result is tied to the attempt lease (FEN-1).
- After an attempt no transcript stays on its disk except under a declared `runtime_internal` session (MEM-4). From v0.2 every runtime that ran an attempt of a tenant takes `erasure` work units for its working files, caches and logs with content (RET-11). The erase operation of a runtime is called `purge` (a work unit with `op: erase`), the search `match`.
- A separate runtime process lives in its own container: a read-only config, no container runtime socket, no host volumes, network only to P3, P4 and its adapter (DEP-5). Telemetry goes only through the adapter by the allowlist, without content (OBS-1); handles do not get into the model context (SEC-3).
- It does not change instructions, skills, the catalog or manifests at the model's request (INJ-10).

## Where the details are

The attempt work item and the `hello` request in `spec/model/work-item.schema.json` and `p1-hello-request.schema.json`; the rows `E4.*` of `spec/ports/operations.yaml`; the runtime checks of the `core-blackbox` suite with a mock model behind P4. Another runtime replaces the reference only after an ADR and the comparison in `docs/quality.md`.
