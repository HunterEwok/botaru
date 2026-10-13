# Mock model provider

## In short

The mock provider sits behind P4 instead of a real provider on the test bench and in the black-box runner. It answers by a recorded scenario in the format of the provider binding (in v0.1 this is `anthropic.md`), so P4 checks the request, sets the model, region and cache and computes usage the same way as in production; only the provider address in the test bench deployment settings changes. The mock provider is allowed only on the test bench: on a farm where the P4 address is not from the closed list of the binding, the core does not start. The mock provider decides nothing for the core: it only answers and records what it received. A scenario is a YAML file; example `examples/hello-bot/mock-model.yaml`.

## Scenario

| Field | Meaning |
|---|---|
| `id` | the scenario name, for example `hello-bot/mock-model` |
| `rules` | the numbers of the rules the scenario checks |
| `description` | what happens, in full sentences |
| `profile` | the name of the `ModelProfile` on which a call is expected |
| `turns[]` | turns in order: each P4 call to the provider takes the next turn |

A turn:

| Field | Meaning |
|---|---|
| `expect_request.slot` | the handle slot on which a call is expected (`base`, `analysis`, `seat`) |
| `expect_request.tools` | the names of the tools that must arrive in `tools`, in order; `[]` means no tools |
| `expect_request.instruction_contains` | a string that must be in `system` |
| `expect_request.cache_points` | how many cache points are expected in the request (optional) |
| `respond.stop_reason` | `end_turn`, `tool_use`, `max_tokens` or `refusal` |
| `respond.result` | the task result: the mock provider returns it as a JSON text block |
| `respond.text` | an ordinary text answer |
| `respond.tool_use` | a tool call: `{name, input}`; the next turn expects `tool_result` |
| `respond.usage` | response tokens: `input_tokens`, `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens` |
| `fault` | a refusal instead of an answer, see below |

If the request does not match `expect_request`, the mock provider answers `400 invalid_request_error` with the text of the mismatch, and the check fails: a mismatch means that P4 let through what it should have rejected or changed.

## Faults

| `fault.kind` | What the mock provider does | For which rules |
|---|---|---|
| `status` | answers with code `fault.status` and error type `fault.error_type`, on `429` with a `retry-after` header from `fault.retry_after_s` | GW-11, GW-13, CAP-2 |
| `status_for` | answers with code `fault.status` to every call for `fault.for_s` seconds, for example `529` for 30 minutes | GW-11 |
| `drop_after_body` | accepts the body, sends the `request-id` header and drops the connection without an answer | BUD-11, GW-12 |
| `delay` | answers after `fault.delay_s` seconds, longer than the P4 response deadline | GW-12 |
| `client_close` | sends the stream (`stream: true`) to the end, while the runner on the runtime side closes the connection after the first `content_block_delta`; the check expects that P4 read the stream to the end and charged usage by what was actually sent | GW-2, BUD-2 |
| `spend_cap` | answers with a refusal by the workspace spend ceiling | GW-9 |

## What the mock provider records

- Every accepted call: time, workspace (by key), model, region, number of cache points, tool names and the body hash by `spec/hashing.md`. The whole body is not stored.
- The number of calls: the GW-2 check compares it with the number of usage lines, the GW-11 check with the number of probes, the GW-12 check expects exactly one call.
- The key of every call: the GW-3 and TEN-5 check expects that calls of `demo` tenant tasks came with the `demo` key, and that the `demo` canary did not appear in any `demo2` call.
- Cache: the mock provider counts the cache as read if the prefix up to the cache point matched the prefix of the previous call of the same workspace, and then returns `cache_read_input_tokens` greater than zero (GW-16).
