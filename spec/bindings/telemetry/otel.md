# Telemetry binding: OpenTelemetry

## In short

This binding holds rule OBS-1 for the telemetry of farm processes: traces, metrics, process logs and health reports carry only ids, sizes, hashes, times, codes and names from closed lists. The farm telemetry collector lets through only attributes from the allowlist below and drops every attribute outside the list. The content attributes of the OpenTelemetry GenAI conventions are not enabled in any farm process. The deployment chooses which collector runs; the contract is the same for all.

## What it implements

| What | Source | Since |
|---|---|---|
| the allowlist of telemetry attributes and names | OBS-1, AUD-2 | v0.1 |
| prohibition of GenAI content attributes | OBS-1 | v0.1 |
| metric labels only from closed sets | OBS-1, the `metrics.yaml` catalog in this folder | v0.1 (metric catalog from v0.2) |
| dropping external `baggage` and `tracestate` at the core boundary | OBS-3 | v0.1 |
| runtime telemetry only through the E4 adapter | OBS-1 | v0.1 |

## Conventions version

The binding pins one version of the OpenTelemetry semantic conventions, and the collector is checked only against it (the binding rule in `spec/bindings/README.md`). The skeleton text does not name the version number yet: it is pinned by a decision on the skeleton architecture together with the exact attribute names of the allowlist. Until then the list of categories below applies, and everything that does not fall into it is dropped.

## Allowlist

Only attributes of these kinds are let through:

| Kind | Examples | Source of the value |
|---|---|---|
| ids of core objects | id of the task, the attempt, the tree root (`correlationid`), the direct cause (`causationid`), the worker | core |
| trace context | the W3C `traceparent` of the root in the `trace` field of the task record; a foreign `traceparent` only as a link | core (OBS-2, OBS-3) |
| names from closed lists | tenant, task type, role, plugin, tool, reason from `spec/model/reasons.yaml`, loop state | manifests and registries |
| numbers | sizes, durations, counters, tokens by kind, price in micro-euros | core and P4 |
| hashes | tool schema hash, run fingerprint | core |
| time | a UTC instant in `datetime` format | store clock (TIM-1) |

Metric labels come only from the sets `tenant`, `task_type`, `role`, `plugin`, `tool`, `reason`, `state` (`label_sets` in `metrics.yaml`). Ids of people, chats and external systems are not labels.

## What is always dropped

- Content attributes of the GenAI conventions: input and output messages, system instructions, arguments and results of tool calls. In the OpenTelemetry conventions these are attributes like `gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions`, `gen_ai.tool.call.arguments` and `gen_ai.tool.call.result`; the exact names are checked against the pinned conventions version. Ready-made instrumentation enables them with one setting, so the collector drops them even if a process sent them.
- `baggage` and `tracestate` coming from outside: W3C Baggage carries arbitrary key-value pairs (OBS-3).
- Any lines from logs with content: runtime transcripts, plugin stderr, channel debug output. Such a log is a store of the `content` class (RET-1), and only metadata leaves it.
- Error texts of external systems: health reports hold only check codes (OBS-10).

## Checks

- A `conformance/suites/` suite with a mock telemetry collector: the canaries `CANARY-NOT-A-SECRET-*` in the task body, the plugin output, the mock model answer and the attachment file name do not appear in the exported telemetry (OBS-1).
- Fixtures `conformance/fixtures/canaries/telemetry/`: where the canaries are and which attributes the collector must drop.
- Lint of deployment configs: the instrumentation configuration has no enabled GenAI content capture, and the collector runs with this allowlist.
