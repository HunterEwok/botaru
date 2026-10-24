# Provider binding: Anthropic Messages API

## In short

The reference P4 binding of v0.1 (lead decision 2 of the 2026-10-12 review). The runtime talks to P4 in the Anthropic Messages API format, P4 checks the request against the allowlist, itself sets the model, the output limit, the reasoning effort, the region and the cache points, calls the provider with the key of the tenant's workspace and computes usage from the `usage` field of the answer. The content of a binding is set by `README.md` of this folder; this file holds only what is specific to Anthropic.

## Version

| What | Value |
|---|---|
| Provider API | Messages API, `POST /v1/messages`, header `anthropic-version: 2023-06-01` |
| Provider address | only from the closed list of this binding: `https://api.anthropic.com`. Another address (the mock provider `mock.md`) is allowed only on the test bench; the core on a farm where the P4 address is not from the list does not start (`docs/quality.md`, `stand/prod-sees-eval`) |
| Binding paths | one: `POST /v1/messages`. P4 does not open other provider paths (batches, files, models, token counting): a request to them gets `404 not_found` before the provider is contacted |
| HTTP binding path | `POST /v1/p4/anthropic/v1/messages` on the P1 listener (`spec/bindings/http/README.md`), the model handle in `Authorization: Bearer` |
| Request schema | `spec/model/p4-anthropic-messages-request.schema.json` (allowlist) |
| Response schema | `spec/model/p4-anthropic-messages-response.schema.json` |
| Date checked | 2026-10-12 |
| `anthropic-beta` headers | not passed: neither from the runtime nor from P4 |

The binding is not a transparent proxy. Only P4 sets the headers of the provider call, by a closed list: `x-api-key`, `anthropic-version`, `content-type`, `accept`. No runtime header, including `traceparent`, `tracestate`, `user-agent` and any `anthropic-*`, is passed to the provider; the runtime has no key (GW-3). The runtime request body is I-JSON (`spec/hashing.md`, section "Rules", row "Input"): a repeated key gives a refusal before the allowlist check.

## Allowlist

A runtime request passes only if it is valid by the `p4-anthropic-messages-request` schema; any field outside it gives `input_schema_invalid` before the provider is contacted (GW-1, INJ-9). The fields fall into three closed lists (D-87 (4)):

| Passed to the provider | Accepted and dropped | Rejected |
|---|---|---|
| `model`, `max_tokens`, `messages`, `system`, `tools`, `tool_choice` (`auto` or `none`, `disable_parallel_tool_use`), `stop_sequences`, `temperature`, `top_p`, `top_k`, `thinking`, `output_config` (`effort`, `format` by a JSON schema), `stream` | `metadata`: P4 removes it before the provider call, this is not a refusal (`x-botaru-dropped: true` in the schema) | `mcp_servers`, `container`, provider server tools (any `tools` element with a `type` field), `inference_geo` and any other region field, `service_tier`, `context_management`, `tool_choice` of kind `any` or `tool`, references to provider files and sources by address, any field outside the schema |

- Content blocks of `messages` only of the kinds `text`, `image` and `document` (bytes in base64 or text), `tool_use`, `tool_result`, `thinking`, `redacted_thinking`. Blocks of server tools, containers and external MCP servers do not pass.
- Ready-made runtimes and SDKs send `metadata` in every request, so P4 accepts it but does not pass it to the provider: the provider sees neither the subject, nor the tenant, nor the task id. The sampling fields (`temperature`, `top_p`, `top_k`) do not change access, region, tools and money and are passed as they are.
- Forced tool choice (`any`, `tool`) is not included: some reasoning models do not accept it, and a runtime that needs a call asks for it by instruction.

## Model

The core chooses the model by the handle slot, not the request (GW-7). The `model` field of the request is required by the provider's shape, but chooses nothing: P4 always puts into the body of the provider call the `model` field of the slot profile that the core bound to the handle, and on a switch to the fallback profile (GW-5) the model of the fallback profile. The runtime sees the actual model in the `model` field of the answer.

## Tools

The request's `tools` are compared with the P3 catalog of this handle: the same names in the same order, and for each tool the hash of `{name, description, input_schema}` by `spec/hashing.md` equals the hash in the catalog. Any difference, an extra or a missing tool gives `input_schema_invalid` (GW-1). The catalog of a translator handle is a single `propose_script` (PRF-1).

## Output limit

P4 sets `max_tokens` to the smaller of the request value and the profile's `max_output_tokens` (GW-2). If `thinking.budget_tokens` is in the request, it must be smaller than the resulting `max_tokens`, otherwise `input_schema_invalid`.

## Reasoning effort

If the profile has `effort`, P4 sets `output_config.effort` to it, in place of any value from the runtime, and keeps the other fields of `output_config`; without it the request goes as it came and the provider's default holds (GW-19). Not every model takes the field: a model that does not answers with `400`, which is `model_error`, so a person sets `effort` only on a profile whose model takes it.

## Structured output

The allowlist lets `output_config.format` through: a JSON schema closed by the `result_schema` of the attempt's task type. The reference loop (D-93) does not send it. The list of JSON Schema keywords that the binding removes from this projection is filled from v0.2, when a runtime uses it (OUT-2). In every case the core checks the result against the type's full `result_schema` at `complete` (`output_schema_invalid`, OUT-1, OUT-2).

## Region

The binding's list of regions from which `privacy.subprocessors[].region` of the `anthropic` provider record is taken:

| Region in the manifest | What P4 sets |
|---|---|
| `us` | the field `inference_geo: "us"` in the body of the provider call |
| `global` | the field `inference_geo: "global"` |

Both regions are outside the EU. A production tenant (`kind: production`) whose `privacy.subprocessors` has the `anthropic` provider with such a region passes `apply` only if the tenant has a transfer basis `spec.model_transfer` set for `anthropic` or for this region (`scc`, `adequacy` or `consent` with a check date, D-87 (2)); without it `apply` rejects the profile for this tenant. Each tenant has its own basis: the contract with the provider is already in its `privacy.subprocessors[].agreement_ref`.

Only P4 sets the field; the same field from the runtime is rejected (GW-14). A region that is not in the table fails `apply` for a profile of this provider. The table changes only with a new version of this file after a check with the provider.

## Cache

- The allowlist has one cache field: `cache_control` of the form `{"type": "ephemeral"}` without `ttl` on `tools` elements, `system` blocks and `messages` blocks. The cache period is the provider's default period, because a profile has one cache write price.
- P4 itself sets the cache point on the last `system` block: it closes the unchanging part, the tool catalog and the instruction, which the provider reads in this order (GW-16). If `system` in the request is a string, P4 turns it into one text block.
- From the second call of a run on (the request has more than one message), P4 also sets a point on the last block of the last message, unless it is a reasoning block: the next call of the run, which sends the same messages and its new ones, reads the conversation so far from the cache instead of paying the full input price for it again (GW-18). If the content of that message is a string, P4 turns it into one text block. P4 sets this point only while the call has fewer than four.
- There are no more than four points in a call in total, counting the P4 points. A request with more gets `input_schema_invalid`.
- For a profile without `cache_write` and `cache_read` prices P4 sets no point and rejects `cache_control` from the runtime: an unpaid cache would not be part of the reserve (BUD-9).
- The cache lives in the tenant's workspace and is not shared between tenants (GW-3).

## Usage

| `usage` field of the answer | Token kind of the usage line | Profile price |
|---|---|---|
| `input_tokens` | `input` | `price.input` |
| `output_tokens` | `output` | `price.output` |
| `cache_creation_input_tokens` | `cache_write` | `price.cache_write` |
| `cache_read_input_tokens` | `cache_read` | `price.cache_read` |

- Price tier: if the profile has `price.above` and the input of the call, `input_tokens`, `cache_creation_input_tokens` and `cache_read_input_tokens` together, is above its `input_tokens`, every line of the call is priced by the tier; a cache price the tier leaves out is the base cache price times the ratio of the tier's input price to the base input price (BUD-17).

- The price of a call in micro-euros is the sum over the lines: tokens multiplied by the profile price in euros per million tokens (that is, micro-euros per token); a fraction of a micro-euro in the call sum is rounded up, the call is not rounded to a euro cent (BUD-9).
- The provider includes reasoning tokens in `output_tokens` and does not name them separately, so the `reasoning` kind in the usage line is not filled in, and reasoning is paid at the output price.
- If `usage` has `iterations`, P4 sums the tokens over all iterations and writes their number into `compaction_iterations` (GW-2).
- Reserve before a call (GW-2): the input is estimated from above by the number of UTF-8 bytes of the request body (a provider token is no shorter than a byte) plus 1024 tokens for the provider's service text for tools and reasoning; the output is the profile's `max_output_tokens`. With a cache price the input is estimated by the higher of the `input` and `cache_write` prices, of the price tier of this estimate.
- The "model answered" flag (GW-13) is set when P4 received a `200` answer with a `usage` field. With streaming (`stream: true`) this is the first `message_start` event; usage is charged by the `message_delta` event with the final `usage`.
- A dropped runtime connection does not break reading the provider stream: P4 reads the stream to `message_stop` or to the answer deadline. If there is no final `usage`, usage is counted by what was actually sent: the input from the `usage` of the `message_start` event, the output by the last `usage` of a `message_delta` event that P4 received; if even that is missing, P4 charges the reserve with `spend_estimated` (BUD-11).
- A call sent without an answer is charged by the reserve with `spend_estimated` (BUD-11).
- Euro cents appear only in limits and reports: the sum of a window is rounded up to a whole euro cent (D-87 (1)).

## Errors

| Provider answer | `reason` | Circuit (GW-11) |
|---|---|---|
| `401 authentication_error`, `403 permission_error` | `provider_config` | opens until fixed |
| `404 not_found_error` on the model | `provider_config` | opens until fixed |
| `402 billing_error`, refusal by the workspace spend ceiling | `provider_spend_cap` | no |
| `429 rate_limit_error` | `provider_rate_limited`, `retry_after_s` from `retry-after` | no |
| `500 api_error`, `529 overloaded_error`, dropped connection, answer deadline | `provider_unavailable` | counts toward the failure series |
| `400 invalid_request_error` on a body that P4 already checked | `model_error` | no |
| `413 request_too_large` | `model_error` | no |
| a `200` answer with `stop_reason: refusal` | no error: the answer is given to the runtime, the runtime decides | no |

- The SDK in the runtime is configured without retries (`max_retries` 0). P4 retries a call no more than once and only if the request did not go out, that is, there was no answer with a `request-id` header (GW-12). After the body was sent there is no retry.
- Answer deadline: `max_output_tokens` divided by the profile's `tokens_per_s`, multiplied by two, not less than 60 seconds (GW-12).
- P4 writes the `request-id` of the answer into the usage line and the run record, but not into the answer to the runtime.
- The fallback profile (GW-5) for the reason `overloaded` is a `529` answer, for the reason `unavailable` a `500` and a dropped connection, for the reason `retired` a `404` on the model.

## Reconciliation report

- The reconciliation source (BUD-10) is the provider's cost report per workspace and UTC day, exported from the provider console as a CSV file (or read with the reporting key); it has no ids of individual requests, so reconciliation uses the daily total, not line by line.
- `farmctl spend reconcile --file` reads the file and hands in each UTC day with P2 `spend.reconcile`; the core compares the sum of the workspace's usage lines for that day with the report. The mismatch tolerance is set in the deployment settings; a mismatch above the tolerance gives `alarm.spend.estimate_drift`.
- The `spend_estimated` lines of that day get the `spend_reconciled` mark; the price of individual lines does not change.
- The alarm `alarm.spend.reconcile_overdue` clears as soon as the day is reconciled.

## Key and workspace

- Each tenant has its own workspace and its own Anthropic key; only P4 holds the key, by name from the deployment settings, and it never goes to the runtime (GW-3).
- The spend ceiling and rate limits of a workspace are set in the provider console and described in the deployment for each workspace (GW-9).
- Because the rate limits belong to the tenant's workspace, a `429` answer is a wait of that tenant's pair, not a refusal (GW-10). There is no daily probe call: `farmctl doctor` reads the workspace ceilings when the reporting access allows it, and a key or model failure on a real call gives `provider_config` (GW-13).

## Checks

The mock provider `mock.md` must be able to do the following for this binding: an answer in the format of this file with `usage` in four fields; the answers `429` with `retry-after`, `529`, `401` and `404`; a dropped connection after the body was received (BUD-11); a stream that the runtime closes after the first `content_block_delta` (usage by what was actually sent); an answer that takes longer than the deadline (GW-12); a second call with a cache read above zero (GW-16).
