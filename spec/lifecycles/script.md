# Lifecycle: script run and its steps

## In short

A script run (`ScriptRun`) is a core record that appears when a draft passed acceptance, and lives until a terminal state. The script steps (`ScriptStep`, `step` in identifiers) are parts of the same record; each has its own state. A draft that did not pass acceptance leaves no record: the refusal is written only to the audit. The core makes all transitions: on a port call, on a person's button or by the background loop `script`, always in one transaction with an event and deliveries (DLV-2). Transitions go only by this table (SCR-11). Worker attempts never wait in the meantime: only the record waits (CNT-4).

## States

### Run

| State | Meaning | Terminal |
|---|---|---|
| `proposed` | the draft passed acceptance (SCR-5): the record is created, the computed fields and the frozen lists are written, the `script_confirm` card is delivered to the initiator; waits for the initiator's button | no |
| `running` | the initiator pressed "Run", the budget and tree reserve is taken; the core releases steps | no |
| `paused` | the release of new steps is stopped by the initiator's button or (from v0.2) by a hold; released steps are finished and closed, the run deadline keeps running | no |
| `completed` | all steps are in a terminal state, no failure with `on_failure: stop` happened | yes |
| `failed` | a step failure with `on_failure: stop` happened and all steps finished, or the deadline of a running run expired | yes |
| `canceled` | the run was canceled before the end; released tasks and campaigns got a cancellation | yes |
| `rejected` | the run did not start: the draft did not pass acceptance (no record), or a run in `proposed` was rejected, superseded or expired | yes |

### Step

| State | Meaning | Terminal |
|---|---|---|
| `pending` | waits until the steps from `needs` finish and the run is in `running` | no |
| `waiting` | an approval is created (for an action, a campaign with an action, or `script_gate` for `confirm`), waits for a human decision | no |
| `released` | the step's work runs: a task or a campaign is created, for `action` the approved action is being executed | no |
| `ok` | the task or the campaign finished successfully, the action was executed, for `confirm` a person answered "Continue"; for `wait` the deadline arrived, for `await` the event came | yes |
| `failed` | the task, the campaign or the action failed; the initiator's rights failed at release; the release was rejected by a hold (v0.1), or a hold canceled the pending approval; for `await` the timeout expired | yes |
| `unknown` | the outcome of the action is unknown (`exec_unknown`) | yes |
| `declined` | a person refused the approval or the decision deadline expired | yes |
| `skipped` | the `when` condition is false, `run_if` is not met, or the release was stopped by a failure with `on_failure: stop` | yes |
| `canceled` | the run was canceled or its deadline expired while the step had not finished | yes |

`failed` and `unknown` count as a step failure. `declined` does not count as a failure: dependent steps with `run_if: succeeded` are skipped, but the run does not become failed.

## Transitions

### Run

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (no record) | `proposed` | P3 `propose_script` of a translator attempt; the "Accept" button on a proposal with a `script` change (P1 `decide`, subject `proposal`) | all acceptance checks passed; the record, the steps in `pending`, the `script.drafted` event and the `script_confirm` delivery are created | SCR-5, SCR-6 |
| (no record) | `rejected` | the same calls | a check failed: `script_invalid`, `script_ref_unknown`, `no_role_cap`, `no_group_cap` and other rights reasons, `script_too_large`, `script_untrusted_limit`; there is no record, the key is not taken, the refusal is written to the audit | SCR-5, TRN-4 |
| `proposed` | `running` | the "Run" button (P1 `decide`, subject `script`) | the initiator pressed it: a `human` principal, with `script.run` if the script has actions, `confirm` or service control (SCR-6), the card's channel, a valid `nonce`, once; the deadline has not expired; `plan_hash` matched; there is no hold on the scope; the budget and tree size reserve is taken in full. The `script.started` event | SCR-6, SCR-8 |
| `proposed` | `rejected` | the "Cancel" button (`declined`); the `script` loop at the decision deadline (`expired`); the initiator being disabled (`person_inactive`); a new draft in the same conversation (`script_superseded`); the reserve did not fit at the press (`budget_exhausted`) | code updates the card; the reserve is not taken | SCR-6, SCR-11 |
| `running` | `paused` | P1 `script.control` with `pause` from the channel on the initiator's button; from v0.2 a hold whose scope touches the run (`held`) | new steps are not released; released work continues unless the hold itself stopped it | SCR-10, SCR-11 |
| `paused` | `running` | P1 `script.control` with `resume` from the channel on the initiator's button | there is no hold on the scope, the deadline has not expired, the initiator is active | SCR-10, SCR-11 |
| `running`, `paused` | `completed` | the core when closing the last step (C1 `script.step_close`) | all steps are terminal, no failure with `on_failure: stop` happened; the `script_summary` delivery | SCR-11 |
| `running`, `paused` | `failed` | the core when closing the last step | all steps are terminal and a failure with `on_failure: stop` happened; the `script_summary` delivery | SCR-11 |
| `running` | `failed` | the `script` loop at the run deadline (`deadline_exceeded`) | the deadline expired: unreleased steps become `canceled`, pending approvals are canceled, released work gets a cancellation | SCR-11 |
| `running` | `canceled` | P1 `script.control` with `cancel` from the channel on the initiator's button; P2 `script.cancel` (the initiator or a person with `admin:people`); cancellation of the anchor task; the initiator being disabled (`person_inactive`) | in the same transaction unreleased steps become `canceled`, pending approvals of steps are canceled, the tasks and campaigns of released steps get a cancellation | SCR-11 |
| `paused` | `canceled` | the same calls as for `running`; the `script` loop at the run deadline | same | SCR-10, SCR-11 |

From v0.2 a script from git (`kind: Script`) is started by a schedule directly in `running`, without `proposed` and without a button, only if it needs no `script.run`, has no `await` step and keeps within the tenant's thresholds, and only with the default values of its parameters; acceptance and the reserve are the same (SCR-12). A person starts a script from git with a command from `Tenant.spec.script_commands`: the run is created in `proposed` with the `script_confirm` card, exactly as for a draft (SCR-14).

### Step

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `pending` | C1 `script.create` together with the run | all steps of the draft | SCR-5 |
| `pending` | `skipped` | the core in the transaction that closes a predecessor; the `script` loop | all steps from `needs` are terminal, but `when` is false or `run_if` is not met; or a failure with `on_failure: stop` stopped the release, and the step has `run_if: succeeded` | SCR-3, SCR-11 |
| `pending` | `released` | C1 `script.release_step` in the transaction that closes a predecessor; the `script` loop for steps without predecessors | the run is in `running`; `needs` are terminal, `run_if` and `when` are met; the initiator's rights are rechecked; a task with the key `system:script:<run>:<step>` (`task`) or a campaign (`fanout` without an action) is created; for `wait` and `await` (v0.2) a waiting record is created | SCR-8, SCR-9 |
| `pending` | `waiting` | same | same, but the kind is `action`, `confirm` or `fanout` with an action: an approval with an exact snapshot is created (subject `approval`, `script_gate` or a campaign approval); an `action` step with `risk: routine` without untrusted text creates no approval and goes directly to `released` by a `service_grant` record authored by the initiator (D-78) | SCR-7, SCR-8 |
| `pending` | `failed` | same | the initiator's rights failed at release (`rights_revoked`, `no_group_cap`); in v0.1 a hold rejected the enqueue (`held`) | SCR-8 |
| `waiting` | `released` | P1 `decide` "Approve" on an action or a campaign | a person with `action.approve:<a>` decided by the approval rules; the core enqueues `action.<a>` tasks | SCR-7 |
| `waiting` | `ok` | P1 `decide` "Continue" on a `script_gate` | a person decided by the step's `who` | SCR-7 |
| `waiting` | `declined` | P1 `decide` "Reject" or "Stop"; the decision deadline (`expired`) | | SCR-11 |
| `waiting` | `failed` | a hold with the effect `no_action` on the step's scope | the pending approval of the step moved to `canceled(held)` (HLD-3); the step is `failed(held)` | SCR-11 |
| `released` | `ok` | the core at the end of the task, the campaign or the action; for `wait` the timer of `for` fired, for `await` the first matching event of the same tenant came (v0.2) | the task is `completed`, the campaign finished successfully, the action was executed | SCR-8, SCR-13 |
| `released` | `failed` | same; for `await` the timeout expired: `failed(await_timeout)` (v0.2) | the task is `failed`, the campaign failed, the action failed | SCR-11, SCR-13 |
| `released` | `unknown` | the core on the outcome of the action | the outcome of the write call is unknown (`exec_unknown`) | SCR-11 |
| `pending`, `waiting`, `released` | `canceled` | cancellation of the run or expiry of its deadline | the pending approval is canceled, the released task or campaign gets a cancellation | SCR-11 |

## Invariants

1. Only the core writes the record of the run and its steps; the fields computed by acceptance (`max_tier`, `data_mark`, `untrusted_seen`, the list of actions, the estimate, `plan_hash`, the frozen lists) do not change after `proposed` (SCR-5).
2. No step is released while the run is not in `running`, and nothing is released after a terminal state of the run (SCR-8, SCR-11).
3. One step has at most one task, campaign or approval: the key `system:script:<run>:<step>`, a double release gives one task (SCR-8).
4. The "Run" button does not move any action approval to approved (SCR-7).
5. The `fanout` items at execution are only a subset of the list frozen at the approval of the run (SCR-7).
6. A step's `untrusted_seen` is not less than the run's flag and the flags of the predecessors whose data it receives (SCR-9).
7. The rights of any step are not wider than the initiator's rights, checked both at acceptance and at release (SCR-5, SCR-8).
8. A draft with `untrusted_seen` does not get into `running` without the initiator's button (SCR-6).
9. Every transition is written in one transaction with an event (`script.drafted`, `script.started`, `script.state`, `script.step`) and deliveries (DLV-2).
10. Attempts do not wait for a person, a timer or an event; only the run record waits (CNT-4).

## Versions

| Version | What appears |
|---|---|
| v0.1 | all states of the run and the steps; the kinds `task`, `fanout`, `action`, `confirm`; a routine action without approval (D-78); pause only by the initiator's button; a step whose release was rejected by a hold becomes `failed(held)` |
| v0.2 | the kinds `wait` and `await` (a waiting record of the run without a task, an `await` timeout gives `failed(await_timeout)`, the run deadline `wait_run_deadline`, SCR-13); a script from git started by a command through `proposed` (SCR-14); a hold puts the run into `paused` (SCR-10); a script from git started by a schedule without steps with an action goes directly into `running` |
