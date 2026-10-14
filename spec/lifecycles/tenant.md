# Lifecycle: tenant

## In short

A tenant (`Tenant`) is a wall of data and money: whose tasks, whose memory, whose key at the model provider. It is declared in git by the file `tenants/<name>/tenant.yaml`, appears at `farmctl apply` and goes through four states: working (`active`), suspended (`suspended`), offboarding (`offboarding`), deleted (`deleted`). A suspension is a hold of the `tenant` scope with the reason `suspended`; it has no separate state field. Offboarding revokes all credentials and leaves only the export. Deletion creates an erasure request across all stores, and the tenant id remains a tombstone forever (LCY-1).

## States

| State | Meaning | Terminal |
|---|---|---|
| `active` | the tenant works: enqueue, hand-out, tools, actions and deliveries are allowed by the rights; data is kept by the periods | no |
| `suspended` | a hold of the `tenant` scope with the reason `suspended` and the effects `no_enqueue`, `no_claim`, `no_tool`, `no_action`, `no_delivery` is in force: there is no new work except core tasks for deletion and export; pending approvals are canceled; deliveries get `withheld`, except messages to the admins about the hold itself (HLD-3); data is not touched; the credentials and the provider key stay valid | no |
| `offboarding` | the tenant is leaving: the credentials of workers and channels are revoked, the core forgets the provider key and stops using it, and the answer of `tenant.offboard` names the operator step to revoke the key and the workspace at the provider (D-103), every connection of the tenant's people to external services is revoked (SEC-7), pending approvals are canceled; only the export is allowed; data is not touched | no |
| `deleted` | content and memory are deleted by an erasure request `(tenant)`; metadata lives until the end of its period and is deleted in whole periods; the tenant id remains a tombstone | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `active` | P2 `apply` (`farmctl apply`) with the manifest `tenants/<name>/tenant.yaml` | the operator; the name is not taken by the tombstone of a deleted tenant | LCY-1, RET-3, PRM-14 |
| `active` | `suspended` | P2 `tenant.suspend` | the operator; the core sets a hold of the `tenant` scope with the reason `suspended` | LCY-1, HLD-1 |
| `suspended` | `active` | P2 `hold.release` of this hold or its `until` deadline (the background loop `hold`) | only the operator releases it: the operator set the hold, and a principal below the operator in rank, including a tenant admin with `hold.release`, gets `forbidden` | LCY-1, HLD-2 |
| `active` | `offboarding` | P2 `tenant.offboard` | the operator | LCY-1 |
| `suspended` | `offboarding` | P2 `tenant.offboard` | the operator | LCY-1 |
| `offboarding` | `deleted` | P2 `tenant.delete` | the operator; the export window has ended (30 days from `tenant.offboard` by default) | LCY-1, LCY-2, RET-7 |

## Invariants

- The `suspended` state is derived from the hold. A tenant is `suspended` while a hold of the `tenant` scope with the reason `suspended` is in force. Other holds of the `tenant` scope (for example, a hold by a person of the tenant with the single effect `no_enqueue`) act as ordinary holds and do not change the tenant's state. Only the operator can lift a suspension (HLD-2).
- Reading the tenant's logs and transcripts by the operator through `admin:debug` follows the field `Tenant.spec.operator_access.debug_journal`: `deny`, `notify` (the default: an audit record and a code message to the tenant admins about every read) or `allow` (only an audit record); the rule is in `docs/privacy.md`.
- A tenant can opt out of the farm manager with the flag `Tenant.spec.farm_manager.enabled`, `true` by default (MGR-42). With `enabled: false` the farm manager does not read the tenant's data, does not run a review and a lens for it, there is no `manager` line in the tenant's budget, and the tenant's incidents go only to its admins; `status` shows the line "farm manager disabled" (D-77, OBS-11). The field is changed by editing `tenant.yaml` and `apply`, like the other tenant fields.
- `apply` and the tenant operations (`tenant.suspend`, `tenant.offboard`, `tenant.delete`, `tenant.export`) are operator operations: they are not executed from a channel (PRM-14).
- There is no way back from `offboarding` and `deleted`. A client can be brought back only as a new tenant with a new name.
- A tenant id is not reused; `apply` of a tenant with the name of a deleted one is rejected (RET-3).
- Content deletion on `deleted` goes only through an erasure request `(tenant)` across all stores (RET-7) and survives a restore from backup (RET-6).
- The export writes only to the sink that the operator named, and never to a chat or to models (LCY-2).
- The service tenant `farm` is never offboarded or deleted: the farm manager seat and the recipients of alarms about the farm itself live in it.
- Every transition publishes an event (`tenant.suspended`, `tenant.resumed`, `tenant.offboarding`, `tenant.deleted`) and writes an audit record.

## Versions

| Version | What appears |
|---|---|
| v0.1 | a tenant from `apply` in the `active` state; a hold of the `tenant` scope already acts as an ordinary hold; the fields `operator_access.debug_journal` and `budgets`; the `farm_manager` field with the opt-out of the farm manager (D-77) |
| v0.2 | the states `suspended`, `offboarding`, `deleted`; P2 `tenant.suspend`, `tenant.offboard`, `tenant.delete`, `tenant.export` |
