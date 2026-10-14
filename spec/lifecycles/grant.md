# Lifecycle: model grant

## In short

A model grant (`ModelGrant`) is a record by which the model gateway P4 gives one task of a worker a stronger model profile for one or two attempts (D-18, D-21). The worker itself makes the request in a closed form; code checks the right to ask and all ceilings; the seat holder with the current epoch or a person decides. A grant lives in the task's tenant, is paid from its `grant` line and ends together with the task. The whole cycle comes in v0.2; before that the `granted` slot is reserved and P4 does not allow it.

Record fields: `id`, `tenant`, `task`, `role`, `profile`, `attempts_left` (1 or 2), `cap_eur` (the amount in euros for the whole grant, stored in euro cents, BUD-9), `expires_at` (no later than 6 h after the decision), `kind` (`grant`, or `takeover` for a takeover, from v0.2), `line` (`grant`, or `takeover` for kind `takeover`), `decided_by` (`{kind: seat, epoch}` or `{kind: human, principal}`), `untrusted`, the refusal `reason`, `state`.

## States

| State | Meaning | Terminal |
|---|---|---|
| `requested` | the worker called `request_model_grant` (or `grant.request` for `job`), the attempt finished without spending, with `awaiting_model_grant` | no |
| `screened` | code checked the schema, the right to ask, the evidence, the price, the budget and the rate | no |
| `pending` | waits for a decision of the seat holder or a person; the task waits with `model_grant_pending` | no |
| `approved` | the grant is approved, attempts remain, the period has not expired; the task is in the queue again | no |
| `active` | an attempt of the task runs on the `granted` slot | no |
| `consumed` | the attempts or the amount of the grant are used up, or the task finished | yes |
| `denied` | a refusal by code, the seat holder or a person, with a reason | yes |
| `expired` | no decision within `grant_wait_max`, or the grant period expired | yes |
| `revoked` | a person revoked the grant | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `requested` | P3 `request_model_grant`; P1 `grant.request` for `job` | the role has the ladder `model_grants.ladder`, the tenant's `grant` line is greater than 0; one request per task; the profile is from the ladder ∩ the profiles allowed to the tenant, not the seat profile | MGR-17 |
| `requested` | `denied` | the core at intake | no right to ask (`grant_not_eligible`: neither the initial input above the class threshold nor the type history by MGR-17); the evidence did not match `reason`; the price does not fit (`grant_over_budget`); the rate (`grant_rate_limited`: more than 3 requests per role in the tenant per day, MGR-17) | MGR-17 |
| `requested` | `screened` | the core at intake | all code checks passed | MGR-17 |
| `screened` | `pending` | the core | always, right after the code checks; the request waits for the next batch of the seat holder or for a person's decision | MGR-18 |
| `pending` | `approved` | P3 `grant_decide` of the seat holder (the `manager.grants.v1` task, in a batch once every 10 minutes, key `system:grants:<tenant>:<window>`, IDM-2); P2 `model_grant.issue` of a person with `model_grant.manage` | the epoch is current; at most 2 attempts, a period of at most 6 h, the `cap_eur` amount within the `grant` line and within the seat's daily grant ceiling `seat_grants_eur_day`; in distrust mode only the first step of the ladder | MGR-18, MGR-22 |
| `pending` | `denied` | P3 `grant_decide` with a refusal; P2 of a person with `model_grant.manage` | the reason is from a closed list | MGR-18 |
| `pending` | `expired` | the `grant` loop | no decision within `grant_wait_max` (15 minutes), the reason `grant_expired_unanswered`; the task continues on the base model | MGR-18, MGR-2 |
| `approved` | `active` | the core at the start of a task attempt | P4 allows the `granted` slot only to this task | MGR-18 |
| `active` | `approved` | the core at the end of an attempt | `attempts_left` is greater than 0 and the task has not finished | MGR-18 |
| `active`, `approved` | `consumed` | the core | the grant attempts ran out, the task finished, or the rest of `cap_eur` does not hold the reserve of the next call: such a call does not start (`budget_exhausted`), the attempt is not spent, the task continues on the base model | MGR-18, GW-2 |
| `approved`, `active` | `expired` | the `grant` loop | `expires_at` arrived; P4 no longer allows the `granted` slot, a running attempt finishes without spending, the task continues on the base model | MGR-18 |
| `approved`, `active` | `revoked` | P2 `model_grant.revoke` of a person with `model_grant.manage` | a running attempt is canceled with the reason `grant_revoked` without spending, the task continues on the base model | MGR-18 |

## Invariants

- A grant belongs to one task of one tenant; other tasks, the role and the worker's identity do not get it, and it does not get into the bot's track record.
- The right to ask never depends on signals that the worker's own attempts produce: the type history is computed only by a run of `grant_eval_set` on the base model, or is set by a person's mark `grant_eligible: true` in the type manifest (MGR-17). The failure share of working attempts is only shown to the holder.
- A record of kind `grant` never gives the seat profile to a worker. The one and only path of a tenant task to the seat profile is a takeover by a person's card: a record of kind `takeover` for 1 attempt on the new task `system:takeover:<task>`, paid from the `takeover` line (MGR-21).
- A grant does not change the tools, the tier, the rights and other budget lines. The code ceilings apply to any decision of the model or a person.
- Before every call under a grant P4 reserves the worst case in the rest of `cap_eur` and in the `grant` line (GW-2). The output limit is set by `max_output_tokens` of the model profile (BUD-9).
- The request's `note` is untrusted; the seat holder does not see it.
- P4 allows the `granted` slot only with an active grant of this task with remaining attempts and before `expires_at`; a model name from the request body is not accepted (GW-7).
- A holder's decision with an old epoch is rejected (`fenced`).

## Versions

| Version | What appears |
|---|---|
| v0.1 | only the reserved `granted` slot; no requests and no grants |
| v0.2 | the whole cycle, the type history by MGR-17 |
