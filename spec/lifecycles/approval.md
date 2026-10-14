# Lifecycle: action approval

Kind: `human-decision` (the kinds are listed in [the lifecycle index](README.md)).

## In short

An approval (`Approval`) is a recorded request to change the world that waits for a human decision. Only the core creates it: on `request_action` from an attempt, on the `/run` command for a pipeline that changes working machines, on a campaign proposal with an action, on a script step of kind `action`, on a `service/control` call with the verb `start` or `stop` (D-74), and on a `service/control` or `monitor/script_run` call in a task with `untrusted_seen` (INJ-15). Restarting a service from the allowed list and a monitoring script in a task without `untrusted_seen` create no approval: the core executes them by a `service_grant` record if both the role and the initiator have the right (APR-18, D-73); a person's `/service restart <service>` command also goes by a `service_grant` record without approval, while `/service start` and `/service stop` go by approval (PRM-18); a routine pipeline run by `/run` goes by a `service_grant` record authored by the initiator without approval (APR-17, D-78). The card carries a `context` line made of closed code fields, and the initiator gets a copy of the card without decision buttons, with a "Remind approvers" button (APR-19). The record keeps a snapshot of the arguments and its hash, the `untrusted_seen` flag with its source, the action risk, the deadline and a random `nonce`. Only a person decides, with a card button through the common P1 `decide` (APR-4); an approved action is executed at most once, by one write call from the snapshot (APR-7). An approval is a special case of the general human decision (`spec/lifecycles/decision.md`): the `pending` state is the same here, and the decided state is expanded into execution.

## States

| State | Meaning | Terminal |
|---|---|---|
| `pending` | the record is created, cards are being delivered to the approvers, there is no decision yet; with `approvals: 2` the approvals of different people accumulate here | no |
| `approved` | a person approved; the state lives only inside the transaction of the transition to `executing` | no |
| `executing` | an `action.<a>` task with `approval_id` is enqueued, exactly one write call from the snapshot is created; for a campaign, the items run | no |
| `executed` | the write call returned success; for a campaign, the campaign became terminal | yes |
| `exec_failed` | the write call returned an execution error, or a hold rejected it before it was handed to the toolhost (reason `held`) | yes |
| `exec_unknown` | the lease of the write call expired without an outcome; there is no retry | yes |
| `rejected` | a person rejected | yes |
| `expired` | the deadline passed without a decision; the action was not executed and will not be executed | yes |
| `canceled` | the core canceled a pending approval; the reason is in the record: `person_inactive`, `rights_revoked`, `held`, `restored`, `seat_changed`, `admin`, `initiator`, `ref_invalid` | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `pending` | P3 `request_action` from an attempt | the action is in the role's `actions_may_request`, every initiator has `action.request:<a>`, the arguments after substituting default values passed the closed schema, at most 3 requests per task; an identical pending request returns the existing number | APR-1, APR-2, INJ-1 |
| (none) | `pending` | P3 `service/control` or `monitor/script_run` from an attempt | the task has `untrusted_seen`, or the verb is `start` or `stop` (D-74); the role has `service_control` for this service, or the script name is in the role's `monitor_scripts` and in the catalog's `monitor_scripts_allowed`; every initiator has `service.control:<service>` or `monitor.script_run` (D-73); the arguments are from closed catalog lists; holds and rate limits let it through; the approval counts toward the 3 per task and toward `cards_per_tenant_hour` (APR-2); the approver has `action.approve:service.control` or `action.approve:monitor.script_run`, and at decision time the core checks the role's right and the initiator's right | APR-18, INJ-15, MON-8 |
| (none) | `pending` | the `/service start` or `/service stop` command (P1 `submit_activity`) | the person has `service.control:<service>`; the verb is not in the service's `no_card`; the approval is on behalf of this person, and a person with `action.approve:service.control:<service>` decides | PRM-18, D-74 |
| (none) | `pending` | the `/run` command (P1 `submit_activity`) | the pipeline did not declare `run.approval: none`, or `apply` did not confirm that it only reads the catalog and writes to the farm file store, and the action is not of class `routine`; the initiator has `action.request:<a>`. An action with `risk: routine` creates no approval: a `service_grant` record authored by the initiator, an `action.<a>` task with `grant_id` and one write call (D-78) | APR-17, IDN-6 |
| (none) | `pending` (subject `campaign`) | a P3 campaign proposal with an action | a snapshot with the action, the items, the rollout policy and `plan_hash`; rights by ORC-5 and ORC-6 | ORC-2, APR-2 |
| (none) | `pending` | release of a script step of kind `action` | the run is in `running`, the step reached its release; the source on the card is "script <plan>, step k" | APR-3, APR-5 |
| `pending` | `pending` | P1 `card.remind(subject_kind, id, nonce)` from the channel of the initiator copy | the initiator pressed it; at most once every 5 minutes per approval, otherwise `service_rate_limited`; approvers who have not decided get a `card_update` | APR-19 |
| `pending` | `pending` | the background loop `approval-expiry` at half of the deadline | a one-time `card_update` reminder to the approvers who have not decided | APR-19 |
| `pending` | `pending` | P2 `hold.set` with the effect `pause_action` on the approval's scope, by a person or a maintenance window (v0.2) | the deadline is extended by the time the approval was pending under the hold, the card gets a `card_update` with a pause mark, no decision is accepted until the hold ends; the pauses extend one approval by at most 7 days in total, then the `approval-expiry` loop expires it; `no_action` wins | APR-19, HLD-3, HLD-8 |
| `pending` | `pending` | P1 `decide(approve)` from the first of two approvers | `approvals: 2` (v0.2; in v0.1 `apply` rejects `approvals: 2` with the code `not_in_version`); the vote is recorded separately; the initiator is active and has `action.request:<a>` | APR-16 |
| `pending` | `approved` | P1 `decide(approve)` from the card's channel | a person with strong identity and `action.approve:<a>`; the `nonce` and the deadline are valid; not the initiator when `self_approve: false`; the initiator is active; with `approvals: 2` this is the second, different approver | APR-4, APR-9, APR-16 |
| `pending` | `rejected` | P1 `decide(reject)` from the card's channel | the same right as for approval; one "Reject" closes the request also with `approvals: 2` | APR-4, APR-16 |
| `pending` | `canceled` | P1 `decide(approve)` from the card's channel | the argument value is no longer in the tenant catalog: the core checks it at approval and when creating the write call in the same transaction; reason `ref_invalid`, no write call | APR-6, MAN-10 |
| `pending` | `expired` | the background loop `approval-expiry` | `expires_at` has passed | APR-12 |
| `pending` | `canceled` | the core in the transaction of the cause: P2 `principal.off`, P2 `ungrant`, P2 `hold.set` with `no_action`, restore from backup, P2 `seat.assign` or `seat.vacate` for the seat that asked, cancellation by an admin or the operator, P2 `task.cancel` of the source task by its initiator (`/cancel`, reason `initiator`) | the reason is written into the approval | APR-6, PRM-3, PRM-17, HLD-3 |
| `approved` | `executing` | the core in the same transaction | an `action.<a>` task is enqueued directly in `working`, without an attempt and a lease, with the key `system:approval:<id>` (CNT-7), one write call from the snapshot is created; for a campaign, the release of items starts with a `plan_hash` check | APR-5, APR-7, ORC-3 |
| `executing` | `executed` | P1 `complete` of the write call from the target's toolhost | the toolhost checked `approval_id`, the tenant and the argument hash; for a campaign, all items are terminal | APR-7 |
| `executing` | `exec_failed` | P1 `fail` of the write call | an execution error; the core does not retry | APR-7 |
| `executing` | `exec_failed` | the core: a hold with `no_tool` or `no_action` on the write call's scope | the hold was set before the call was handed to the toolhost: the write call gets `denied(held)`, the approval ends with the reason `held`; nothing is executed late | APR-7, HLD-3 |
| `executing` | `exec_unknown` | the background loop `reaper` | the lease of the write call expired without an outcome | APR-7 |
| `executing` | `exec_unknown` | C1 `epoch.advance` on restore from backup | the write call was in `queued` or `leased`; the action is not repeated | RET-6 |

Every transition creates a `card_update` delivery so that the card shows the outcome to all recipients. A repeated `decide` with the same principal, the same decision and the same `nonce` returns the same success and changes nothing (IDM-3); a different decision or a different principal on a decided approval gets `already_decided`.

## Invariants

- Only the core creates an approval; the model, a program, `system` and a bot only ask (APR-1, APR-5).
- The transition from `pending` to `approved` or `rejected` happens exactly once; only a `human` principal decides, from the channel where the card was delivered, with a valid `nonce` and with the right at the moment of the decision (APR-4).
- The channel draws the card from the record by a code template, there is no model text on it; the button carries only `(subject_kind, id, nonce)` (APR-3). The `context` line is assembled by code from closed fields of the result of the source attempt; the initiator gets a copy without decision buttons (APR-19).
- An approval is executed at most once: one write call from the snapshot, `redrive` is forbidden for `action.*`, a non-idempotent action has one attempt (APR-7).
- The core checks an argument value against the tenant catalog at the request, at approval and when creating the write call; a value that disappeared gives `canceled(ref_invalid)` without a write call (MAN-10).
- A write call rejected by a hold before it was handed out is not executed later: the approval ends with `exec_failed(held)` (APR-7).
- The attempt that asked for an action does not wait for the decision: it gets the approval number and finishes (D-41).
- Neither an agent nor a `job` takes an `action.<a>` task: it is created directly in `working`, its only work is the write call that the target's toolhost picks up, and the outcome of that call closes the task (TEN-3a, CNT-7).
- Every decision writes a decision event with `response_ms` and `details_opened` (APR-11).
- On a production tenant the initiator does not approve their own request (APR-9); the two-person rule for `risk: high` applies from v0.2 (APR-16).

## Versions

| Version | What appears |
|---|---|
| v0.1 | all states; the sources `request_action` and `/run`; a routine pipeline run by `/run` without approval, by a `service_grant` record authored by the initiator (APR-17, D-78); the `action.<a>` task directly in `working` (CNT-7); the cancellation reasons `person_inactive`, `rights_revoked`, `held`, `restored`, `admin`, `initiator`, `ref_invalid`; the `context` line, the copy to the initiator and P1 `card.remind`, a deadline without a manifest `ttl` of 60 minutes for `risk: low` and 30 minutes for `risk: high`, a reminder at half of the deadline (APR-19); the `cards_per_tenant_hour` limit (APR-2); `exec_unknown` for write calls on restore from backup; the decision event with `response_ms` and `details_opened`; the risk field; the source `service/control` and `monitor/script_run` with `untrusted_seen` (INJ-15) and `service/control` with the verb `start` or `stop` (D-74), these approvals count toward the APR-2 limits; the initiator's right `service.control:<service>` or `monitor.script_run` (D-73); a person's `/service restart` command without approval, `/service start` and `/service stop` with approval (PRM-18); the subject `campaign` (a campaign approval covers the snapshot and becomes `executed` when the campaign is terminal); the source "script <plan>, step k" for steps of kind `action`; `exec_failed(held)` when a hold with `no_tool` or `no_action` rejected the write call before it was handed out; the reason `seat_changed` for farm manager requests; a "requester stopped" mark on the card when the farm manager seat is stopped by an incident |
| v0.2 | `approvals: 2`: votes accumulate in `pending`, the transition to `approved` on the second, different approver; extension of the deadline by a `pause_action` hold (HLD-3) |
