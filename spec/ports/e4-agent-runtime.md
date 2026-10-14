# E4 AgentRuntime

Sheet version: 0.1.0.

## In short

E4 describes the model loop inside a worker of kind `agent`. An agent has two parts. The E4 adapter holds the worker token, does `claim` and `heartbeat` and gives the runtime only the task body, the trusted instruction and the handles. The runtime runs the model loop: it takes tools only from P3, goes to the model only through P4 and has neither the worker token nor the provider key. The reference runtime is our own loop in Go over P4 (D-93); any other runtime connects through this same port. Where the runtime differs from the farm model (where the session is, where the memory is, what the model handle is issued for, how the catalog is built), it declares from a closed list, and each deviation turns on its own non-waivable substitute RT rule.

## Who calls and who implements

- **Implemented by** the E4 adapter plus the runtime. If the runtime is a separate program, it lives in its own container: without the provider key and the worker token, with a read-only config, without the container runtime socket and host volumes, with network only to P3, P4 and its adapter (DEP-5).
- **Calls**: the adapter calls P1 `hello`, `claim(attempt)`, `heartbeat`, `complete`, `fail`, `session.*`, `memory.*`, `file.get`; the runtime calls P3 and P4 with the attempt's handles. P4 computes the model answer limits (the deadline of one call) from the model profile, the runtime does not set them.

## Operations

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| `describe` | adapter at start | Declares how the runtime performs the four mechanisms; values only from `spec/model/runtime-deviations.yaml` | | v0.1 | RT-1, RT-2, RT-3, RT-4, INJ-9 |
| `run` | adapter after `claim` | Input `RunRequest`; output `completed{artifacts, usage}` or `failed{reason, retryable, usage}`; `usage` is for reference | | v0.1 | FEN-4, BUD-3 |
| `cancel` | adapter on `cancel_requested` from `heartbeat` | Stops the work before the next heartbeat | | v0.1 | FEN-4 |
| `purge` | adapter on an erasure request from the `erasure` loop; the adapter gets the step as an `erasure` work unit (the adapter takes it with P1 `claim` and hands in the result with `complete`) and passes it to the runtime | Deletes runtime data by scope (the session and memory profile under `runtime_internal`) or by a list of ids and hands the result into the erasure request; required from v0.1 for runtimes that declared `runtime_internal`, from v0.2 for all: working files, caches and logs with content of every runtime that ran an attempt of the tenant, together with cleanup when a worker is retired (RET-11) | | v0.1 | RT-1, RT-2, MEM-4, RET-7, RET-4, RET-11 |
| `match(tenant, subject, needles)` | adapter on a match search from the `erasure` loop; the adapter gets the step as an `erasure` work unit (the adapter takes it with P1 `claim` and hands in the result with `complete`) and passes it to the runtime | Searches the transcripts and memory inside the runtime for the subject's exact strings and answers only with a list of ids, without content; required from v0.1 for runtimes with `session: runtime_internal` or `memory: runtime_internal`, otherwise `describe` blocks the worker at `hello` | `blocked` | v0.1 | RT-5, RET-7, SEC-11 |

`RunRequest` (sketch):

```json
{
  "run_id": "t_01JB...:2",
  "instruction": "text of the role and task type (trusted)",
  "body": { "type": "ops.investigate.v1", "goal": "...", "params": {}, "attachments": [] },
  "tools":  { "mcp_endpoint": "...", "handle": "<handle>" },
  "model":  { "endpoint": "...", "handle": "<handle>" },
  "session": null,
  "memory": null,
  "skills": [],
  "limits": { "max_turns": 20, "run_max_input_tokens": 400000, "deadline": "2026-10-05T16:45:00Z" }
}
```

`run_id` of the form `<task id>:<lease_no>` (one runtime run per attempt lease, FEN-1) is the idempotency key. `skills` are references to skills from the repository with the skill tree hash (`spec/hashing.md`) by `run-request.schema.json#/$defs/skill_ref`. The limits in `limits` are `max_turns`, `run_max_input_tokens` and `deadline`, as in `docs/models-and-money.md`; the output limit of one call is the model profile's `max_output_tokens`, P4 sets it, and the money of the whole task tree is limited by the task type's `tree_budget_eur` (BUD-9).

**Runtime deviations** (`describe()`). Any value except the first turns on a substitute rule, and that rule is non-waivable.

| Field | Values | Substitute rules |
|---|---|---|
| `session` | `farm`, `runtime_internal`, `none` | `runtime_internal`: RT-1 and RT-5 (`match`) |
| `memory` | `farm`, `bridge`, `runtime_internal`, `none` | `bridge` as `farm`; `runtime_internal`: RT-2 and RT-5 (`match`) |
| `model_attribution` | `per_attempt`, `per_profile` | `per_profile`: RT-3 |
| `catalog` | `per_attempt`, `per_role` | `per_role`: RT-4 |
| `built_in_tools` | only `[]` | no deviations (INJ-9) |
| `skills` (from v0.2; absent means `farm`) | `farm`, `runtime_files` | `runtime_files`: SKL-2 and RT-2 (a read-only skill folder with only the role's skills whose tree hash matches) |

## Guarantees

Every implementation must (MUST):

- take tools only from P3; keep built-in shell, files, web, browser, provider server tools and background self-learning turned off (INJ-9); P4 checks this independently (GW-1);
- go to the model only through P4, without fallback or auxiliary providers bypassing it;
- not retry a P4 error: the run ends with the reason P4 returned, and the core retries the task (GW-12);
- not write memory without the approval of the person whose memory it is, and not turn on self-learning without approval (MEM-3);
- stop working on cancellation before the next heartbeat (FEN-4);
- use `run_id` as the idempotency key;
- not leave a transcript on its disk after `run`, except under a declared `session: runtime_internal` (MEM-4);
- delete data by scope through the adapter with the `purge` operation on an erasure request: from v0.1 under `runtime_internal` (RT-1, RT-2, RET-7), from v0.2 for all runtimes (RET-4, RET-11); under `runtime_internal`, answer `match` with a list of ids where the subject's strings occur (RT-5);
- send telemetry only through the adapter and only by the allowlist, without content (OBS-1);
- not set the region of a model call: P4 sets it;
- not see the worker token and the provider key (DEP-5); handles do not get into the model context (SEC-3).

## Bindings

- `spec/model/run-request.schema.json`, `run-outcome.schema.json`, `runtime-deviations.yaml`, `e4-describe-response.schema.json`, `e4-cancel-request.schema.json`; the map `spec/ports/operations.yaml` (rows `E4.*`).
- The `conformance/suites/runtime` suite with a mock model behind P4 and the `erasure` suite for runtimes with `runtime_internal` (`match` finds the subject's canary, `purge` by id removes it); then a full eval next to the current runtime. The adapter's expected failures may hold only rules with `waivable: true`.
- The reference runtime is our own loop in Go over P4 (D-93). Any other runtime gets an ADR with checks of the candidate: does the runtime work with a read-only config; does approval of memory records work; can a model handle be passed for each attempt; does the runtime read `tools/list` on each attempt; how can the adapter learn the list of the runtime's enabled tools.

## What the port does not do

- It does not give the runtime the worker token, the provider key or direct access to the store.
- It does not change instructions, skills, the catalog and manifests at the model's request (INJ-10).
- It does not open networks to the runtime other than P3, P4 and the adapter.
