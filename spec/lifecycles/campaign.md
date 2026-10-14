# Lifecycle: campaign and campaign item

## In short

A campaign (`Campaign`) is a frozen plan of mass work of one tenant: a task or action template, a list of items, a rollout policy (`canary`, `after_canary`, `max_in_flight`, `max_failures`, `on_unknown`, `deadline`) and an optional continuation. The core creates the whole campaign record in one C1 `campaign.create` transaction; after that only the campaign state, the pause reason, the counters and the item states change, while the template, the items, the policy and `plan_hash` never change (ORC-1). A campaign item (`CampaignItem`) is a line of the plan; on release the core creates an ordinary task for it with the fields `campaign` and `campaign_item`, and this task follows its own cycle (`spec/lifecycles/task.md`). Only deterministic core code releases items, each at most once (ORC-3, ORC-4). A campaign with an action is started by one human approval for the whole plan (ORC-2); the approval follows its own cycle (`spec/lifecycles/approval.md`): it is `executing` while the campaign runs, and `executed` when the campaign is terminal.

## States

Campaign:

| State | Meaning | Terminal |
|---|---|---|
| `proposed` | the campaign is recorded, the reserve is taken, it waits for a human decision on the approval card; for a campaign without a card this state lasts only inside the creation transaction | no |
| `running` | items are released by the rollout policy | no |
| `paused` | new items are not released, released ones are finished; the pause reason is from a closed list: `gate`, `unknown`, `human`, `held`, `no_executor`, `estimate_exceeded` | no |
| `stopping` | the campaign is stopped: unreleased items became `skipped`, released ones are finished; the stop reason is recorded | no |
| `completed` | all items are terminal, the failed ones do not exceed the `max_failures` threshold | yes |
| `failed` | stopped by the threshold, by a canary failure or by the `deadline` | yes |
| `canceled` | canceled by a person, by canceling the tree root, by revoked rights of the initiator or by the initiator being disabled | yes |
| `rejected` | the approval was rejected, expired or canceled; when the proposal checks refuse, there is no record, the idempotency key is not taken, and the refusal is written to the audit | yes |

Item:

| State | Meaning | Terminal |
|---|---|---|
| `pending` | waits for release | no |
| `released` | released: the item's task is created, for an action also exactly one write call from the snapshot | no |
| `ok` | the item's task is `completed`; for an action the write call is `executed` | yes |
| `failed` | the item's task is `failed`; for an action the write call is `exec_failed` | yes |
| `unknown` | for an action the lease of the write call expired without an outcome; the item is not repeated | yes |
| `skipped` | not released: the campaign was stopped or canceled, or a person excluded the item | yes |
| `canceled` | a released read item was canceled together with the campaign, or its task was canceled | yes |

## Transitions

Campaign:

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `proposed` | P3 `propose_campaign`; P1 `campaign.propose` with an attempt token | a live attempt; a role with `campaigns.propose` of the needed kind; the rights for every item on behalf of every initiator; the policy and size are not wider than the ceilings; the ceiling under distrust or with the initiator `system`; the tree size and the whole price estimate fit into the reserve; for a campaign with an action there is no hold in the scope (ORC-11); a card is needed (a campaign with an action, a read campaign above `confirm_read_over`, or a read campaign that lacks only the daily limit of the human initiator); the core creates an approval with the subject `campaign` | ORC-1, ORC-2, ORC-5, ORC-6, ORC-8, ORC-14, APR-2 |
| (none) | `rejected` | P3 `propose_campaign`; P1 `campaign.propose` | the first failed check: `campaign_policy_exceeded`, `campaign_untrusted_limit`, `no_group_cap`, `tree_too_large`, `budget_exhausted`, `held` (a campaign with an action in the scope of a hold), `fenced`; the same key with a different plan gives `idempotency_conflict`; the model gets a short "no" | ORC-1, ORC-5, ORC-6, ORC-8, ORC-14, PRM-8 |
| (none) | `proposed` | the "Retry failed" button (P1 `campaign.control` with `retry_failed`, P2 `campaign.retry_failed`) | the human initiator pressed it; the original campaign is terminal; a new campaign from its `failed`, `unknown` and `skipped` items with the same template, a policy that is not wider, the `untrusted_seen` of the original and a `refs` link, without `then`; a campaign with an action always gets a new approval | ORC-1, ORC-2 |
| (none) | `proposed` | release of a `fanout` step of a script run (the core loop `script`) | a person approved the run; the list is a subset of the list frozen at approval; a campaign with an action gets its own approval; the reserve is taken from the run's reserve; there is no `then` | ORC-1, ORC-8, SCR-7, SCR-8 |
| `proposed` | `running` | the core in the same creation transaction | no card is needed: a read campaign below `confirm_read_over` whose estimate also fits into the initiator's daily limit (under distrust or with the initiator `system`, at most 10 items as an explicit list), or a read campaign of a `fanout` step; no hold covers the scope; the first items are released | ORC-2, ORC-4, ORC-14 |
| `proposed` | `running` | the "Approve" button (P1 `decide` with the subject `campaign`) | a `human` principal from the card's chat with its `nonce`, the right `action.approve:<a>` (for a read campaign `task:<type>`), the `self_approve` rule, and on a production tenant the person who pressed is not the initiator; the initiator is active and has the rights; in the same transaction the approval becomes `executing` and the canary is released | ORC-2, ORC-3, APR-4, APR-9 |
| `proposed` | `paused` (`held`) | the core in the same creation transaction; the "Approve" button | no card is needed or a person approved, but the campaign scope is covered by a hold with an effect that forbids release or execution; items are not released; for a campaign with an action the approval is `executing` | ORC-11 |
| `proposed` | `rejected` | the "Reject" button (P1 `decide`); the background loop `approval-expiry`; P2 `hold.set` with `no_action`; cancellation of the approval together with its reason | the approval moved to `rejected`, `expired` or `canceled`; the reserve is released, no items were released | ORC-2, ORC-11, APR-6, HLD-3 |
| `running` | `paused` (`gate`) | the core in the transaction that closes the last canary item | all canary items are `ok`, `after_canary: manual`; with `after_canary: auto` there is no pause, the `campaign` loop waits out `bake` | ORC-4 |
| `running` | `paused`, the item is unknown | the core in the transaction where an item became `unknown` | `on_unknown: pause` | ORC-4 |
| `running` | `paused` (`human`) | the "Pause" button (P1 `campaign.control`); P2 `campaign.control` | the human initiator, a person with the right to approve, or a tenant admin (`admin:people`) | ORC-4 |
| `running` | `paused` (`held`) | P2 `hold.set` and the core's auto-stops | the hold scope covers the campaign or its items, the effect is `no_enqueue`, `no_claim`, `no_action` or `cancel_running`; the same transaction as the hold record | ORC-11, HLD-3 |
| `running` | `paused` (`no_executor`) | the background loop `campaign` | no live worker of its type takes a released item for longer than `start_by` | ORC-4 |
| `running` | `paused` (`estimate_exceeded`) | the core at release | the next item does not fit into the remaining campaign reserve | ORC-8 |
| `paused` | `running` | the "Resume" button (P1 `campaign.control` with `resume`) | a `human` principal from the card's chat with its `nonce`, the right to approve, not the initiator when `self_approve: false` and on a production tenant; the initiator is active and has the rights; the reason is `gate`, `unknown`, `human` or `no_executor`; the reason `held` only when no hold covers the campaign anymore; the reason `estimate_exceeded` is never lifted | ORC-2, ORC-8, ORC-11, APR-4, APR-9 |
| `running`, `paused` | `stopping` | the core in the transaction that closes an item | the number of failed items exceeded the threshold (`threshold_exceeded`) or a canary item failed | ORC-4 |
| `running`, `paused` | `stopping` | the background loop `campaign` | the `deadline` passed (`deadline_exceeded`) | ORC-4 |
| `running`, `paused` | `stopping` | the "Cancel" button (P1 `campaign.control` with `cancel`); P2 `campaign.control` | the human initiator, a person with the right to approve, or a tenant admin (`admin:people`); the reason is `initiator` if the initiator canceled, and `admin` if a person with the right to approve or a tenant admin did | ORC-4 |
| `running`, `paused` | `stopping` | cancellation of the tree root; rights recheck at release | the root is canceled; the initiator's right was revoked (`rights_revoked`) or the initiator is disabled (`person_inactive`) | ORC-6, PRM-3 |
| `stopping` | `failed` | the core in the transaction where the last released item became terminal, or at once if there are no released items | the stop reason is `threshold_exceeded` or `deadline_exceeded`; on the threshold the core raises `alarm.campaign.threshold` | ORC-4 |
| `stopping` | `canceled` | same | the stop reason is `initiator` or `admin`, a root cancellation, `rights_revoked` or `person_inactive` | ORC-4, ORC-6 |
| `running`, `paused` | `completed` | the core in the transaction where the last item became terminal | all items are terminal, the failed ones do not exceed the threshold | ORC-4 |

On the transition to `completed`, `failed` or `canceled` the core, in the same transaction, releases the rest of the reserve, moves the approval (if there is one) to `executed`, enqueues the delivery of the final summary (except for a campaign of a `fanout` step, ORC-10) and, if the campaign has `then`, enqueues the continuation once with the key `system:cont:campaign:<campaign>` when it can be enqueued (CNT-1, CNT-2). A `rejected` campaign has neither a continuation nor a summary, and the reserve is released at the moment of the refusal.

Item:

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `pending` | C1 `campaign.create` | all items of the plan are created together with the campaign | ORC-1 |
| `pending` | `released` | the core in the transaction that closes the previous item or in the approval transaction; the background loop `campaign` (`bake`) | the campaign is `running`; there is no hold in its scope; fewer than `max_in_flight` are released and not finished; the resource is free with `exclusive: resource`; the tenant's queue of the item type is not longer than `queue_max`; every initiator has the rights; the item fits into the remaining reserve; from v0.2 the `rate_per_min` pace is not exceeded and the wave is open (ORC-15). In the same transaction the item's task with the key `system:campaign:<campaign>:<n>`, and for an action also one write call from the snapshot | ORC-3, ORC-4, ORC-6, ORC-8, ORC-11, ORC-12, ORC-15 |
| `pending` | `skipped` | the "Exclude item" button (P1 `campaign.control` with `skip`); the campaign's transition to `stopping` | the button was pressed by a `human` principal from the card's chat with the right to approve, not the initiator on a production tenant; on `stopping`, all unreleased items at once | ORC-1, ORC-2, ORC-4, APR-9 |
| `released` | `ok` | C1 `campaign.item_close` in the transaction of the terminal transition of the item's task | the task is `completed` (for an action the write call is `executed`); the `summary_fields` fields are copied into the item record | ORC-4, ORC-9 |
| `released` | `failed` | same | the task is `failed` after its attempts (for an action the write call is `exec_failed`) | ORC-4, FEN-3 |
| `released` | `unknown` | the background loop `reaper` | the lease of the write call expired without an outcome; there is no retry and no `redrive` | ORC-3, APR-7 |
| `released` | `canceled` | the campaign's transition to `stopping` on cancellation; cancellation of the item's task | a read item; released write calls are not recalled, their outcome is recorded as `ok`, `failed` or `unknown` | ORC-4, ORC-11 |

## Invariants

- The plan is frozen: the template, the items, the policy and `plan_hash` do not change after creation; the list only shrinks by a person's button (ORC-1).
- Every item is executed at most once: one item task, and for an action exactly one write call with the argument hash from the snapshot; a double release gives one task (ORC-3, IDM-2).
- Only the core changes the campaign state, the pause reason, the counters and the item states; the model and workers do not change them. The counters always equal the number of items in each state (ORC-4).
- Every transition is one C1 transaction with a `campaign.state.v1` event made of metadata only and the needed card deliveries (DLV-2, AUD-2).
- The content of item results does not change the course of the campaign: only item outcomes change it, by closed threshold and pause rules (ORC-4, ORC-9).
- While a hold covers the campaign, items are not released; lifting the hold does not resume the campaign (ORC-11).
- The reserve is taken in full at creation and released as items end; spending does not exceed the reserve, and the "Resume" button does not lift the `estimate_exceeded` pause (ORC-8).
- The continuation is enqueued at most once and only after a terminal state; its type is taken from the role's `campaigns.then` in git (CNT-1).
- The approval of a campaign with an action is `executing` while the campaign is `running`, `paused` or `stopping`, and `executed` when it is terminal.
- The tenant of a campaign and of all its items is one and does not change (TEN-1); the mark and `untrusted_seen` of the campaign are taken from the proposing task, and the continuation's `untrusted_seen` is the OR over it and all items (INJ-13).
- At most one released write call per resource with `exclusive: resource`, counting the items of all campaigns and single approvals (ORC-12).

## Versions

Campaigns are part of v0.1 and in the order of work come after the core and the first bot (D-71, D-80). The `core` profile does not contain managers and campaigns: an implementation that claims only that profile creates no states of this cycle.

| Version | What appears |
|---|---|
| v0.1 | the whole cycle: the campaign and item states, the pause reasons, the buttons "Pause", "Resume", "Exclude item", "Cancel", "Retry failed", the background loop `campaign`, campaigns of `fanout` script steps |
| v0.2 | `waves` and the `rate_per_min` pace as item release conditions (ORC-15); the record field `wave` and an update of the progress card on a wave change; no new states |
