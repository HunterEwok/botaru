# Lifecycle: tool call

## In short

A tool call (`ToolCall`) is a core record of one call to a plugin tool. A read call is created by P3 `tools/call` from a live agent attempt; a write call is created only by the core: in the transaction of an approval, from the argument snapshot (APR-7), or on a call of the built-in tool P3 `service/control` for a service with `via: host`, together with the audit record `service_grant` (APR-18). The core checks the call, computes its target `target` (the tenant, the plugin, the placement, the machine from the `target_from` argument of the tool manifest, or any toolhost with `target: any`) and queues it; the toolhost of this target executes it, picking the call up through P1 `claim` as work of kind `toolcall` (TEN-3a). A call does not wait for an executor longer than `start_by` (30 s): without a toolhost it ends with `error(no_executor)` (DLV-16). The core never retries a call: the model sees a read error and decides itself, and a write call is executed at most once (DLV-1, APR-7). The model does not see write tools at all (APR-1).

## States

| State | Meaning | Terminal |
|---|---|---|
| `requested` | The core accepted the call and checks it | no |
| `denied` | A check failed: a hold, the rights, the tier, the output class, an argument outside the closed schema. The model gets a short "no rights", the full reason is written to the audit | yes |
| `queued` | The target is computed, the call waits for the toolhost of this target no longer than `start_by` (a ceiling of 30 s, the type can decrease it with the field `timeouts.tool_start_by`) | no |
| `leased` | A toolhost took the call under a lease and executes it with the plugin | no |
| `done` | The plugin returned an outcome; for a read, the output is redacted and given to the model as a separate tool result block | yes |
| `error` | The plugin returned an error, asked for input (`InputRequiredResult`), the call's deadline passed, the read lease expired, nobody took the call within `start_by` (`no_executor`), or the target has no live toolhost (`plugin_unavailable`). The model sees the error; for a write call the approval becomes `exec_failed` with the same reason | yes |
| `exec_unknown` | Only a write call: the lease expired without an outcome or the store epoch grew. It is unknown whether the action was executed; there is no retry | yes |
| `abandoned` | Only a read: the attempt that owns the call lost its lease or the task was canceled. The result is given to nobody | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `requested` | P3 `tools/call` with the tools handle of a live attempt | a read call; the handle is valid and belongs to the attempt's tenant | SEC-3, TEN-2 |
| (none) | `requested` | the core in the transaction of the approval transition `approved` -> `executing` | exactly one write call, from the argument snapshot, with `approval_id` and the argument hash; the catalog value in the arguments is still in the tenant catalog | APR-7, MAN-10 |
| (none) | `requested` | the `campaign` loop at the release of an item of a campaign with an action | the work id `<approval_id>:<n>`; the plan hash matched; with `exclusive: resource` at most one released write call per resource | ORC-3, ORC-12 |
| (none) | `requested` | the core on a call of the built-in tool P3 `service/control` | a service with `via: host`; the role's right `service_control` and the task initiator's own right `service.control:<service>` (D-73), a verb from the service's `control` and `no_card` (without a card only `restart`, D-74), the limiters, `exclusive: resource` and holds passed; the task has no `untrusted_seen` (otherwise, as with `start` and `stop`, an approval is created instead of the call, INJ-15); a `service_grant` record in the same transaction; the OS mechanism `unit` | APR-18, MON-8 |
| `requested` | `denied` | the core (C2 in the same transaction) | a hold in the scope (`held`); the tool is not in the role's `tools`, `access` is not `read`, `tier` is above `max_tier`, the output class is above `data_mark`; an argument outside the closed schema or outside the catalog `enum` | HLD-1, PRM-3, PRM-4, PRM-5, PRM-8, INJ-1 |
| `requested` | `queued` | the core | the rights and the schema passed; `target` is computed from the `target_from` argument (a machine from `catalog.hosts` or the machine of the resource the value belongs to) or without a machine with `target: any`; the call carries a minimum of fields: the tool, the checked arguments, the tenant, `target`, the call id, for a write also `approval_id` and the argument hash | TEN-3a, SEC-4 |
| `requested` | `error` | the core | the plugin is in the `down` state or the target has no toolhost with `last_seen` fresher than the threshold: `plugin_unavailable` at once (v0.1) | DLV-16, OBS-10 |
| `queued` | `error` | the `deadline` loop | no toolhost of the target took the call within `start_by`: `error(no_executor)`; the model sees the error, a write call gives the approval `exec_failed(no_executor)` and the action task `failed(exec_failed)` | DLV-16, CNT-7 |
| `queued` | `leased` | P1 `claim` with the kind `toolcall` from a toolhost | the tenant, the plugins, the placement and the machine of the toolhost recorded at `enroll` matched `target` | TEN-3a, FEN-1, DEP-2 |
| `queued` | `denied` | P2 `hold.set` with `no_tool` on the plugin or `no_action` on the action | the hold was set before hand-out: `denied(held)`, the toolhost does not get the call; for a write call the approval ends with `exec_failed` with the reason `held` | HLD-3 |
| `leased` | `done` | P1 `complete` from the toolhost | the plugin's outcome; for a read, the output is redacted on the toolhost by `redact` and checked again by the core, the attempt gets `untrusted_seen` with `untrusted_output: true`; for a write, the toolhost checked `approval_id` (for `service/control` the `service_grant` number in the `grant_id` field), the tenant and the argument hash, the approval becomes `executed`, and the `action.<a>` task `completed` (CNT-7); code writes the outcome of `service/control` to the task journal and checks the state of the service after 2 minutes | INJ-4, INJ-13, APR-7, CNT-7 |
| `leased` | `error` | P1 `fail` from the toolhost | a plugin error, `InputRequiredResult` (this is a call error), the `timeout_s` deadline; for a write the approval becomes `exec_failed`, the `action.<a>` task `failed(exec_failed)` | INJ-5, APR-7, CNT-7 |
| `leased` | `error` | the `reaper` loop | only a read: the toolhost lease expired, `error(lease_expired)`; if it expired during core downtime and was not extended after readiness, `error(core_unavailable)` | DLV-1, FEN-5 |
| `leased` | `exec_unknown` | the `reaper` loop | only a write: the lease expired without an outcome; there is no retry, the approval becomes `exec_unknown`, the `action.<a>` task `failed(exec_unknown)`, the card is updated | APR-7, FEN-5, CNT-7 |
| `queued`, `leased` | `exec_unknown` | C1 `epoch.advance` on restore from backup | only a write; actions are not repeated | RET-6 |
| `queued`, `leased` | `abandoned` | the core on loss of the lease by the attempt that owns the call, on task cancellation, on lease revocation | only a read; the result is given to nobody; the toolhost sends the plugin an MCP cancellation and after `kill_after` ends its process | FEN-4, FEN-6 |

## Invariants

- The core does not retry a tool call. The model gets a read error and decides itself; a repeated call is a new call and new spending (DLV-1). A call does not hang without a reason: in `queued` no longer than `start_by`, in `leased` no longer than `timeout_s` and the lease (DLV-16).
- Write tools never get into the model's `tools/list`; only the core creates a write call, from the snapshot of an approved approval or, for `service/control`, from a `service_grant` record, and at most once; `redrive` is forbidden for `action.*` (APR-1, APR-7).
- A write call passes the plugin a work id (`approval_id`, for a campaign item `<approval_id>:<n>`, for `service/control` the `service_grant` number in the `grant_id` field), by which a second executor recognizes what was done (FEN-7).
- The core computes the target from the `target_from` argument of the manifest, or without a machine with `target: any`; a tool without one of the two fields does not pass `apply`; a toolhost of another machine, another placement or another tenant does not get the call (TEN-3a).
- A running call does not count as progress of the attempt: the CNT-4 stall is counted from the last finished call, and the deadline of a running one is held by `start_by` and `timeout_s` (CNT-4, DLV-16).
- Holds are checked before rights; for a hold and for a refusal on rights the model gets the same short answer (HLD-1, PRM-8).
- The arguments pass the closed schema of the manifest; a free string is allowed only for tools with `placement: sandbox` (INJ-1).
- The schema of a write tool is the schema of the action by the `args_from` reference; there is no second copy (MAN-9).
- A plugin asks neither the model nor people: `sampling`, `elicitation` and `roots` are not declared, a request for input is an error (INJ-5).
- The toolhost stops execution by its own clock when the lease has ended, and after that writes nothing (FEN-6).

## Versions

| Version | What appears |
|---|---|
| v0.1 | all states; reads through P3 `tools/call`, writes from an approval; the target from `target_from` or `target: any`; the hand-out deadline `start_by` and `error(no_executor)`; `error(plugin_unavailable)` by plugin health; `error(core_unavailable)`; the MCP cancellation and `kill_after` for `abandoned`; the outcome of a write call moves the `action.<a>` task; after the core and the first bot (D-71, D-80): write calls of campaign items with the id `<approval_id>:<n>` and `exclusive: resource`; the `service/control` write call by a `service_grant` record with the `unit` mechanism (without a card only `restart` by the initiator's right, D-73, D-74); the write call of a `routine` class action by the initiator's `service_grant` (D-78); `denied(held)` by holds with `no_tool` and `no_action` |
| v0.2 | nothing new |
