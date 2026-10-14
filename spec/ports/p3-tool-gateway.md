# P3 ToolGateway

Sheet version: 0.1.0.

## In short

P3 is the only source of tools for the agent runtime. The core itself serves it (D-45): for the runtime it is the only MCP server, and the tool handle goes in the authorization header of every request. The core builds the `tools/list` catalog by handle on every request from the manifests, not from the live plugin server: the model sees only the read tools of its role with closed parameters and the built-in tools of its profile. The core checks a tool call, computes its target and creates a call record, which the right toolhost takes through P1. So the runtime has no worker token, and write tools are never visible to the model.

## Who calls and who implements

- **Implemented by** the core. The north side speaks the MCP tool protocol; the revision, the sessionless mode, passing the client version and capabilities in the `_meta` of every request and the server discovery operation are set by the binding `spec/bindings/mcp/README.md` (gateway profile), not by the sheet and not by the rules.
- **Called by** the agent runtime with the tool handle of its attempt. The handle is issued in `claim` and lives while the lease is alive (SEC-3).
- **South side:** the core records a plugin call as work of kind `toolcall` with a `target`; a toolhost takes it through P1 `claim` (sheet E3).

## Operations

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| server discovery (`server/discover` in the MCP binding) | runtime | The capabilities of the P3 server and the version of the tool protocol profile | `handle_invalid` | v0.1 | SEC-3 |
| `tools/list` | runtime | The tool catalog by handle, built by the assembly steps below, in a stable order | `handle_invalid`, `fenced` | v0.1 | PRM-4, APR-1, INJ-2, RT-4 |
| `tools/call` (plugin tool) | runtime | The core checks holds, permissions, tier, output class and the argument schema, computes the `target` (tenant, plugin, placement, resource machine), creates the call and waits for its result. The core takes the target machine from the argument named in the tool manifest's `target_from`, and a tool with `target: any` goes to any toolhost of the tenant with this plugin and placement; `apply` rejects a manifest with neither. The output comes back as a separate tool result block, the core searches it for secrets once more and sets `untrusted_seen` on the attempt. A call to a plugin whose toolhost is `down` immediately gets `plugin_unavailable`; a call that no toolhost took within 30 seconds (`start_by`) ends with `error(no_executor)` | a short "no permission" to the model (`held` and permissions); `handle_invalid`, `fenced`, `plugin_unavailable`, `no_executor` | v0.1 | PRM-3, PRM-4, PRM-5, INJ-1, INJ-4, INJ-13, TEN-3a, HLD-1, CNT-4, OBS-10 |
| `tools/call` (built-in tool) | runtime | Built-in tools from the `builtin-tools.yaml` registry; what they do is said in the sheets and documents of their parts | as for a plugin call | v0.1 | PRF-1 |
| `tools/call` (`service/control`, `monitor/script_run`) | runtime | Two built-in write tools with arguments only from closed lists of the tenant's catalog. The model calls them as P3 catalog tools, but the core executes them, not the gateway: the core checks holds, the allowed list (the service in the catalog and in the role's `service_control`, `monitor_scripts` of the role), the limiters and the attempt's `untrusted_seen`. Only a restart goes without a card: a verb from the catalog's `services[].no_card` (default `[restart]`) in a task without untrusted text, and also `monitor/script_run` from the role's list; then the core writes a `service_grant` audit record and creates one write call: for a `via: host` service a call to the action toolhost on the service machine (E3), for a `via: monitor` service and for a script a `monitor_script` delivery to the E5 adapter. `start` and `stop`, and also any call in an attempt with untrusted text, go through approval and a card, as for `request_action` (D-74, D-70). A person or an external program among the task's initiators needs their own capability `service.control:<service>` or `monitor.script_run` (D-73). The result returns to the model only as closed fields (the result code, the service state after the check, the `service_grant` or `approval_id` number); the core stores the script output separately as a task journal file with the untrusted text mark | a short "no permission" to the model (`held` and permissions); `service_rate_limited` (farm limiters on services and scripts) | v0.1 | MON-8, MON-10, APR-18, INJ-15, HLD-1 |

**`tools/list` assembly steps.** Steps 1, 2 and 5 are always required. Steps 3, 4 and 6 are required under `catalog: per_attempt` and recommended (SHOULD) under `catalog: per_role`; in the second case everything that depends on the task is checked at call time (RT-4, PRM-3).

1. Tools from the role's `tools`.
2. Only `access: read`; write tools of plugins are never shown (APR-1). The only write tools in the catalog are the built-in `service/control` and `monitor/script_run` at step 7 (APR-18).
3. `tier` not above the task's `max_tier` (PRM-4).
4. The output class (the highest of the tool's `output_class` and the resource class) not above the task's `data_mark` (PRM-5).
5. Description and schema from the manifest, not from the live server (INJ-2).
6. "Which resource" parameters are rebuilt into an `enum` from the tenant's catalog, filtered by the task mark. A parameter with `enum_from: task.attachments` gets an `enum` from the ids of this task's attachments (the initiator's files, for example a CTD file for `data/describe` of the `data` plugin, D-72); a foreign or nonexistent id does not get into the `enum` (TEN-7).
7. Built-in tools from `builtin-tools.yaml` are added that are available to the role profile and the task conditions (for example `request_action` if the role may request actions and the initiator has `action.request` for at least one; `inbox_read` only if the type declared `inbox.read`; `service/control` if the role has `service_control` and the catalog has such services, with an `enum` of services and verbs from the catalog, and `monitor/script_run` if the role has a non-empty list `monitor_scripts: [<name>...]` (`["*"]` means all scripts of the catalog), with an `enum` of resources with `monitor_host` and of script names from the catalog's `hosts[].monitor_scripts_allowed` whose `scriptid` is in the adapter's latest list; names from the monitoring system API do not get into the `enum`).
8. A stable order, so that the provider's prompt cache works.

**Built-in tools by version.**

| Version | Tools |
|---|---|
| v0.1 | `request_action`, `status.tasks`, `status.workers`, `enqueue_child`; `send_message`, `inbox_read` (messages between bots, D-71), `status.proposals`, `observations_query` (improvement cycle); `service/control`, `monitor/script_run` (write tools, APR-18); `messages_digest`, `proposals_query` (aggregator); `status.campaigns`, `propose_campaign`, `campaign_status`, `campaign_results` (only in a continuation task), `propose_script` (no more than 3 calls per attempt); tools of the farm manager seat: operations from `control-ops.yaml` (a C1 `control.apply` call with the attempt's epoch; refusals `fenced`, `held`, `target_not_implicated`, `target_excluded`, `protected_target`, `control_rate_limited`), `inspect_request` (a read request to the reading bot), `incident_close` (closing an incident from `resolved`), viewing an incident and the tenant state; `file_read` of the manager's reading bot |
| v0.2 | `escalate`, `request_model_grant`, `grant_decide`, `escalations_take`, `grants_pending`, `takeover_request`; `skill_read` (a skill file of the role by `enum`, only for runtimes that declare `skills: farm`, only for the `worker` and `manager` profiles, SKL-4); `kb_search` and `kb_read` with the word search of KNW-6 |

Which profiles can use each built-in tool is set by the profile table (PRF-1): for example `propose_script` exists only for `translator`, `escalate` and `request_model_grant` only for `worker` and `manager`, `messages_digest` and `proposals_query` only for `aggregator`, `skill_read` (from v0.2) only for `worker` and `manager`. In `insight` tasks the `worker` and `manager` profiles get only `observations_query` of the built-in tools (INS-1, OBV-3); the `reviewer` profile in `review` tasks also only `observations_query`. In `governance` tasks no profile has built-in tools (GOV-16).

## Guarantees

- MUST: the catalog is computed by handle on every request, not per connection; a handle of one tenant in the work of another is refused (TEN-2).
- MUST: write tools of plugins are not in the catalog; the model can change the world only through `request_action` and a person's approval (APR-1, APR-2). There is one exception: a service restart (`service/control` with a verb from `services[].no_card`, default only `restart`) and `monitor/script_run` from the role's allowed list are executed without a card under a `service_grant` audit record if the attempt did not see untrusted text; a start, a stop and any call after untrusted text go through a card (D-68, D-70, D-74, APR-18, INJ-15).
- MUST: holds are checked before permissions; on a hold and on a permission refusal the model gets the same short "no permission", the full reason is written to the audit (HLD-1, PRM-8).
- MUST: permissions are checked on every call in the same transaction, decisions are not cached (PRM-3).
- MUST: tool output is redacted on the toolhost and checked once more by the core, and is returned as a separate tool result block; where text is needed, it is wrapped in a mark with a random value per call (INJ-4).
- MUST: the first untrusted output sets `untrusted_seen` on the attempt (INJ-13).
- MUST: a call that lost the attempt lease is refused, its result is given to nobody (FEN-4).
- MUST: the tool handle does not get into the model context or the logs (SEC-3). The trace context in `_meta` does not affect decisions.
- MUST: arguments are checked by the closed schema of the manifest; free input exists only for `placement: sandbox` (INJ-1).

## Bindings

- `spec/bindings/mcp/README.md` (gateway profile): the north side of P3, the MCP revision (2026-07-28 or later), the sessionless mode, what of the protocol is used.
- `spec/model/builtin-tools.yaml`: built-in tools, the profiles that can use them, conditions.
- Manifest schemas `ToolPlugin`, `Role`, `TaskType`; `spec/model/formats.yaml`.
- Checks: the `conformance/suites/core-blackbox` suite (P3 section), catalog fixtures and secret canaries.

## What the port does not do

- It does not execute tools itself and does not hold connections to plugins: the toolhost executes (E3).
- It does not show the model write tools, addresses, hosts, paths and free commands.
- It does not take descriptions and schemas from the live plugin server.
- It does not give the model provider server tools: they are cut off already in P4 (GW-1).
- It does not retry a plugin call by itself: when a read lease is lost, the model gets an error and decides itself (DLV-1).
