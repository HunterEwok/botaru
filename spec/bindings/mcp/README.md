# Tool protocol binding: MCP

## In short

Sheets P3 and E3 and the rules say "tool protocol" and name no revision; this file says that it is the Model Context Protocol, which revision is required and what of the protocol the farm uses and what it drops. The binding is needed in two places with different roles: on the north side of P3 the core is the MCP server for the agent runtime (gateway profile), and the toolhost is the MCP client for plugins (plugin profile). A runtime or plugin with another revision connects through a new profile in this folder, not through an edit of the sheet.

## Revision and general points

- Protocol revision 2026-07-28 or later; `tools/list` and `tools/call` follow this revision, argument and result schemas use the JSON Schema 2020-12 dialect (like all skeleton schemas).
- Sessionless mode: every request is self-contained, the core keeps no connection state; on the north side the credentials are the tool handle in the authorization header of every request (SEC-3), on the plugin side an authenticated toolhost.
- The `_meta` of every request carries the client version and capabilities and the trace context; `_meta` does not affect the decisions of the core and the plugin (P3, E3).
- The server discovery operation (`server/discover`): server capabilities and the profile version; for P3 this is the first operation of the sheet.
- Not used and not declared from the protocol: `sampling` (the server asks the model), `elicitation` (the server asks a person), `roots` (the server sees the client's files), resource subscriptions, server prompts. A plugin answer "input needed" counts as a call error (INJ-5, E3).

## Gateway profile (north side of P3)

| What | How in the binding | Sheet |
|---|---|---|
| `server/discover` | P3 capabilities (only `tools`), profile version `botaru-tool-gateway/1` | P3 |
| `tools/list` | the catalog by handle from the manifests, in a stable order; description and schema from the manifest, not from the live plugin; "which resource" parameters have an `enum` from the tenant's catalog | P3 (INJ-2, PRM-4) |
| `tools/call` | the core checks holds, permissions, tier, class and schema, creates the call and waits for the toolhost result; the result as a separate `content` block with the untrusted mark; `isError: true` with a reason from `reasons.yaml`, the model gets only a short "no permission" | P3 (PRM-3, PRM-8, INJ-4, INJ-13) |
| cancellation | `notifications/cancelled` from the runtime stops the waiting; a call that lost the attempt lease is refused | P3 (FEN-4) |
| handle | the authorization header, at least 128 bits, lives with the lease; the handle is not in the request body or in `_meta` | SEC-3 |

Built-in tools (`request_action`, `service/control`, `monitor/script_run` and the others from `builtin-tools.yaml`) look to the runtime like ordinary catalog tools; the core executes them, not the toolhost.

## Plugin profile (toolhost to plugin, E3)

| What | How in the binding | Sheet |
|---|---|---|
| connection | the plugin is an MCP server in a child process of the toolhost (stdio) or in its own container (sessionless HTTP); the toolhost has no incoming ports | E3 (DEP-6, DEP-7, DEP-10) |
| `tools/list` on `hello` | the toolhost takes a fingerprint of the live list and compares it with the `tool.yaml` manifest; a mismatch gives `blocked`; from v0.2 only recorded "image digest plus fingerprint" pairs are accepted | E3 (INJ-2, LCY-5) |
| tool annotations | `readOnlyHint`, `destructiveHint`, `idempotentHint` do not contradict the manifest's `access`, otherwise `apply` rejects | INJ-3 |
| headers | parameters with `x-mcp-header` are forbidden by the manifest schema | INJ-5 |
| `tools/call` | arguments already checked by the core; `_meta` carries the trace context and the `approval_id` or `grant_id` number as the idempotency key of the write call; the toolhost does not declare `sampling`, `elicitation`, `roots` | E3 (APR-7, APR-18) |
| output | the toolhost redacts secrets by the manifest, cuts by the tool limit and hands in to P1 `complete`; the core checks again | INJ-4 |
| cancellation and termination | on `abandoned`, `lease_expired` and self-stop the toolhost sends the plugin `notifications/cancelled` and terminates the process after `kill_after` (10 s) | FEN-6 |

## Versions

| Version | What exists |
|---|---|
| v0.1 | Both profiles: `server/discover`, `tools/list`, `tools/call`, cancellation, the `tools/list` fingerprint on `hello`, output redaction |
| v0.2 | Accepted "digest plus fingerprint" pairs for a plugin; plugin health report in `hello` and `heartbeat` |

## Checks

- `conformance/suites/core-blackbox` (P3 section): the catalog by handle, refusal of a handle of another tenant, the short "no permission", a secret canary in the output does not reach the runtime.
- `conformance/suites/toolhost`: a fake plugin with an extra tool gives `blocked`; a plugin that declared `sampling` gets a call without that capability and its input request ends in an error; cancellation on an expired lease terminates the plugin process.
