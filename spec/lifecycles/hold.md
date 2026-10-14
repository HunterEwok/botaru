# Lifecycle: hold

Kind: `hold` (the kinds are listed in [the lifecycle index](README.md)).

## In short

A hold (`Hold`) is a recorded object that forbids part of the farm's work: a scope (for example, a task type in the tenant `demo`), effects from a closed list (for example, `no_claim`), a reason, an `until` deadline or "until released", an author. A hold only narrows: it grants no rights, deletes no data and changes no manifests. While a hold is `active`, C2 checks it at every check point before the rights formula (HLD-1). A hold of the scopes `farm` and `seat` is a record of the service tenant `farm`, a hold of any other scope is a record of the tenant of its scope (TEN-1). A hold ends in two ways: it expires by its deadline, or someone who is allowed releases it, that is, a principal with `hold.release` on the scope and not below the hold's author in rank (HLD-2). Terminal holds remain as records of the `metadata` class for audit and analysis.

## States

| State | Meaning | Terminal |
|---|---|---|
| `active` | the hold is in force: the core refuses everything that its effects forbid in its scope (HLD-3) | no |
| `expired` | the `until` deadline arrived by the store clock; the effects no longer apply | yes |
| `released` | the hold was released; the effects no longer apply | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `active` | P2 `hold.set` | the operator through `farmctl` (any scope; the scopes `farm` and `seat` only the operator) or a person of their own tenant with `hold.set:<scope>` on scopes of their tenant, through `farmctl` or with a command in a direct chat with the bot; a hold of the `governance` scope (v0.2) is set by a person with `admin:governance` in the case's tenant; the scope and the effects are in the closed lists of the current version | HLD-1, HLD-2, PRM-14, PRM-15, GOV-14 |
| (none) | `active` | `farmctl hold farm` (a write directly to the store, without the core process and without a channel) | the operator on the core machine; `farmctl` with the core's credentials is part of the core; the scope `farm` | HLD-1, DEP-8 |
| (none) | `active` | the background loop `hold` by an auto-stop rule | the condition of a rule from `spec/model/holds.yaml` is met; the author is `contain_auto`; no deadline (HLD-6, CAP-4) or one hour (HLD-7 `runaway_spend`) | HLD-2, HLD-6, HLD-7, CAP-4 |
| (none) | `active` | the core when an incident is opened | the first measure of the incident class from git with its own deadline; the author is `contain_auto` | HLD-2, MGR-7 |
| (none) | `active` | C1 `control.apply`, the `hold` operation | the current holder of the farm manager seat with the current epoch; the scope `worker`, `bot`, `bot_group`, `role`, `tasktype` or `resource`; a target from the list that code built; a deadline of at most `hold_ttl_max`; the seat limits are not exceeded | HLD-2, MGR-9 |
| (none) | `active` | the seat safeguard | the limits of control operations are exceeded; in one transaction a `no_control` hold on the `seat` scope without a deadline, an increment of the seat epoch and cancellation of the holder's running attempts in all tenants | MGR-14 |
| (none) | `active` | a P2 hold on the `seat` scope (the farm manager kill switch) | only the operator with `admin:farm` through `farmctl`; in one transaction an increment of the seat epoch and cancellation of the holder's running attempts in all tenants | MGR-15, PRM-14 |
| (none) | `active` | the background loop `hold` by a maintenance window of the monitoring system (the E5 adapter polls the maintenance status, v0.2) | the first `active` event of a window on a node that the tenant catalog linked to its resource (`Catalog.spec.hosts[].monitor_host`); the scope `host` (the catalog host whose `monitor_host` is the node); the effects `no_tool` and `pause_action` (with `hosts[].maintenance_hold: true` the effect `no_action` instead of `pause_action`); the reason `maintenance`; the author `contain_auto`, the source `system:monitor`; `until` from the event, the poll time plus two poll intervals; no new hold for a window whose hold a person released | HLD-2, MON-9 |
| (none) | `active` | P2 `tenant.suspend` | the operator; a hold of the `tenant` scope with the reason `suspended` | LCY-1 |
| (none) | `active` | P2 `secret.revoke` (`farmctl secret revoke --compromised`) | the operator; in the same operation a hold without `until` on every scope where the deployment description uses the secret, the reason `compromised` | SEC-6 |
| `active` | `expired` | the background loop `hold` | `until` arrived | HLD-1, TIM-1 |
| `active` | `released` | P2 `hold.release` | a principal with `hold.release` on the hold's scope and not below the author in rank (the table "Who sets, who releases" below); otherwise `forbidden`; a hold of the `governance` scope is released by a person with `admin:governance` in the case's tenant | HLD-2, HLD-1, PRM-14, GOV-14 |
| `active` | `released` | C1 `control.apply`, releasing a hold | the seat holder releases only its own holds | HLD-2, MGR-9 |
| `active` | `released` | P2 `control.revert` with `since` | only the operator with `admin:farm`; the holds that the seat holder set after `since` are released | MGR-15, PRM-14 |
| `active` | `released` | P2 `control.revert` with `incident` | the operator or a person of the tenant with `hold.release` in this tenant (the command `/control revert <number>` in a direct chat with the bot); only the seat holder's holds for this incident are released | MGR-15, PRM-15 |
| `active` | `active` | the background loop `hold` by a later `active` event of the same maintenance window (v0.2) | the core moves `until` of the maintenance window hold forward to the event's `until`, never back; nothing else of the hold changes | HLD-2, MON-9 |
| `active` | `active` | P2 `seat.assign` (replacement of the seat holder) | for the holds of the previous holder, `until` is shortened to 15 minutes after the replacement if it was further away | HLD-2, MGR-1 |

## Who sets, who releases

Ranks from top to bottom: the operator, a tenant admin (a person of the tenant with `hold.release` on the scope), the holder of the farm manager seat. Only a principal with `hold.release` on the scope and not below the author in rank releases a hold (HLD-2); C2 checks this at the `hold.release` check point and answers `forbidden`.

| Hold author | Examples | Who releases |
|---|---|---|
| the operator | tenant suspension (`suspended`, LCY-1), secret revocation (`compromised`, SEC-6), a `farm` hold on suspected key theft, any hold through `farmctl` | only the operator |
| code inside the tenant, author `contain_auto` | the auto-stop "secret in outbound" (HLD-6), the auto-stop on the spending rate `runaway_spend` (HLD-7), a schedule pause (CAP-4), the first measure of an incident class (MGR-7), a maintenance window hold `maintenance` (MON-9) | a tenant admin or the operator |
| a person of the tenant with `hold.set:<scope>` | a hold on a plugin because of a vulnerability | a tenant admin or the operator |
| the holder of the farm manager seat (`control.apply`) | quarantine of a worker for an incident | the holder itself (only its own), a tenant admin through P2 `control.revert` with `incident`, the operator through `control.revert` with `since` |
| the seat safeguard (MGR-14) or the operator on `seat` (MGR-15) | a `no_control` hold on the farm manager seat | only the operator with `admin:farm` |
| a person with `admin:governance` | a pause of a governance case (the `governance` scope, GOV-14) | a person with `admin:governance` in the case's tenant, or the operator |

## Invariants

- A hold only narrows: it grants no rights, deletes no data, changes no manifests (HLD-2).
- The model, a worker, a route and a schedule neither set nor release holds. The only exception: the current holder of the farm manager seat with the current epoch, only on targets from the code's list and only its own holds (MGR-9).
- Only a principal not below the author in rank releases a hold; an operator hold is released only by the operator, a `contain_auto` code hold by a tenant admin or the operator (HLD-2).
- A hold set by code (an auto-stop, the first measure of an incident, the seat safeguard, a maintenance window hold) is released only by a person; otherwise it ends by `until`. The core does not release holds itself: neither when an incident is closed, nor when an alarm is resolved, nor when a maintenance window ends or is deleted in the monitoring system: a maintenance window hold ends at its `until`, at most two poll intervals later (MON-9).
- A maintenance window hold does not cancel pending approvals: with `pause_action` they wait, and their deadline is extended by the length of the hold; only `hosts[].maintenance_hold: true` gives `no_action` instead (MON-9, HLD-3).
- The rights `hold.set:<scope>` and `hold.release` are privileged and are granted only through `farmctl` (PRM-13). A hold of the scopes `farm` and `seat` and its release are operator operations (PRM-14). A hold of the `governance` scope is set and released by a person with `admin:governance` in the case's tenant (GOV-14).
- A hold of the `bot` scope set by a person moves the bot identity to `suspended`, and the release or expiry of the last such hold returns it to `active` (`spec/lifecycles/identity.md`). A hold of the same scope set by the farm manager seat holder only delays the identity's tasks: it does not change the identity's state or its eligibility for promotion (MGR-9).
- The scope, the effects, the reason and the author do not change after the hold is set. Only the deadline of an active hold can change: toward shortening when the seat holder is replaced, and toward extension only for a maintenance window hold while the monitoring adapter reports the window active (MON-9). To change the effects, a new hold is set and the old one is released.
- A hold applies from the next check, without `apply` and restarts (HLD-3).
- Deadlines are counted by the store clock (TIM-1).
- Every transition publishes an event (`hold.set`, `hold.released`, `hold.expired`) and writes an audit record (HLD-1, AUD-1).
- A hold does not silence alarms and does not delay their export: notification and silencing live in the monitoring system (MON-1).

## Versions

| Version | What appears |
|---|---|
| v0.1 | the states `active`, `expired`, `released`; the scopes `farm`, `tenant`, `role`, `tasktype`, `worker`; the effects `no_enqueue`, `no_claim`, `cancel_running`; `farmctl hold farm`; P2 `hold.set`, `hold.release`, `hold.list`; release by the author's rank, the author `contain_auto` for code holds; the scopes `plugin`, `action`, `channel`, `host`, `resource`, `bot_group`, `seat`, `bot`; the effects `no_tool`, `no_action`, `no_delivery`, `no_message` (messages between bots come in v0.1 after the core and the first bot, D-71), `no_control`; seat holder holds, the safeguard, the kill switch and the seat revert (`control.revert` with `since` and with `incident`); the identity's transition to `suspended` on a person's hold of the `bot` scope; the first measure of an incident class; the auto-stop "secret in outbound" (HLD-6); the auto-stop on the spending rate `runaway_spend` with a deadline of one hour (HLD-7) |
| v0.2 | the scopes `schedule` (a schedule pause by an auto-stop, CAP-4) and `governance` (a pause of a governance case, GOV-14); the effect `pause_action`; holds by maintenance windows of the monitoring system and the field `hosts[].maintenance_hold` (MON-9); P2 `tenant.suspend`; a hold on revocation of a compromised secret (SEC-6) |
