# C2 Authorizer

Sheet version: 0.1.0.

## In short

C2 gives one "allowed or not" decision with a reason, and the list of what is allowed for a task. The request shape is like Cedar: principal, action, resource, context; deny by default, deny beats allow. This is a model for the shape, not a promise to replace the engine: the check runs in the same transaction as the transition, there is no separate permission service. The order of the decision is the same at every check point: first holds, then non-waivable prohibitions, then the intersection of the role, the groups of all initiators, the chat ceiling and the budget. The permission table is a fixture that every implementation passes.

## Who calls and who implements

- **Called by** the core at every check point: enqueue, claim, tool call, action request, button press, action execution, delivery, reading a task and a result, reading and writing memory, sending and reading messages, control operations.
- **Implemented by** a function inside the core in the transition transaction; the reference is a store function.

## Operations

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| `authorize` | core | `authorize(principal{kind, id, tenant}, action, resource{tenant, kind, id}, context{task: type, tier, data_mark, depth, parent, untrusted_seen, initiators; chat: kind, ceiling; budget}) -> {allow, deny, reason}` | `deny` with a reason from `reasons.yaml` (including `held`) | v0.1 | PRM-1, PRM-2, PRM-3, HLD-1, TEN-2 |
| `catalog` | core (P3) | `catalog(principal, task)` -> the task's tools and closed resource lists | | v0.1 | PRM-4, PRM-5 |
| `explain` | only tests and `doctor` | Why the decision is what it is: which entries allowed or denied | | v0.1 | PRM-8 |

## Guarantees

- MUST: no allowing entry means not allowed (PRM-1).
- MUST: holds are checked first (HLD-1); then non-waivable prohibitions: tenants are equal, write tools are not for the model (except `service/control` and `monitor/script_run` from the role's allowed list, APR-18), tier 3 only through approval, only the core enqueues `action.*`, `admin:*` is not a tool; then the intersection of the role, the groups of all initiators of the attempt, the chat ceiling and the budget (PRM-2).
- MUST: the initiator set includes the `origininitiators` of all messages the attempt read; permissions are intersected with each, a flag only narrows (MSG-7).
- MUST: decisions are not cached between check points; a revoked permission gives `canceled(rights_revoked)` (PRM-3).
- MUST: the permission table does not depend on the bot identity, its model profile or bot groups: an identity and a bot group give no permissions (IDN-9, BGR-2).
- MUST: only a subject of kind `human` may decide on cards; permissions to decide are privileged (APR-4).
- MUST: the model gets a short "no permission", the full reason is written to the audit (PRM-8).
- MUST: every C2 implementation passes the permission table `conformance/fixtures/authorize/`.

## Bindings

- `spec/model/authz-request.schema.json`, `authz-decision.schema.json`, `spec/model/capabilities.yaml` (capability shapes, tiers, class order), `spec/model/holds.yaml`.
- Fixtures: `conformance/fixtures/authorize/` and `authorize/holds/`.

## What the port does not do

- It does not keep permissions in process memory between checks.
- It does not grant permissions: P2 and `apply` change groups and memberships.
- It does not promise a replacement of the permission engine; the Cedar shape is only the request shape.
