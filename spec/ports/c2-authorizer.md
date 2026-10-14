# C2 Authorizer

How the core is built, not a contract: the check runs inside the core, so this page describes the construction (C10, D-114).

## In short

C2 gives one "allowed or not" decision with a reason, and the list of what a task may use. The request has the shape principal, action, resource, context, like Cedar; this is only a shape, not a promise to replace the engine. Deny by default, deny beats allow. The check runs in the same transaction as the transition: there is no separate permission service, and decisions are not cached between check points (PRM-3).

## How it works

- The core calls `authorize` at every check point: enqueue, claim, a tool call, an action request, a button press, an action execution, a delivery, reading a task and a result, memory, messages and control operations. `catalog` gives P3 the tools and closed resource lists of a task (PRM-4, PRM-5); `explain` tells tests and `doctor` which entries decided (PRM-8).
- The order is the same everywhere (PRM-2): holds first (HLD-1); then the non-waivable prohibitions (tenants are equal, write tools are not for the model except the two tools of APR-18, tier 3 only through approval, only the core enqueues `action.*`, `admin:*` is not a tool); then the intersection of the role, the groups of every initiator of the attempt, the chat ceiling and the budget. The initiators include the `origininitiators` of every message the attempt read (MSG-7).
- No allowing entry means not allowed (PRM-1). A bot identity, a model profile or a bot group gives no permission (IDN-9, BGR-2). Only a `human` subject decides on cards (APR-4).
- The model gets a short "no permission"; the full reason goes to the audit (PRM-8).

## Where the details are

The permission table `conformance/fixtures/authorize/` (with `authorize/holds/`) is the check every implementation passes. Its rows use the core test schemas `spec/model/authz-request.schema.json` and `authz-decision.schema.json`, which the fixtures check; they are not a port contract. Capability shapes, tiers and class order are in `spec/model/capabilities.yaml`, holds in `spec/model/holds.yaml`.
