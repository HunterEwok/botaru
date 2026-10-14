# Lifecycle: escalation

Kind: `improvement` (the kinds are listed in [the lifecycle index](README.md)).

## In short

An escalation (`Escalation`) is a closed-form record with which a bot tells the farm manager about a problem; it is not a message, and the bot gets no answer. A bot reaches the farm manager only through the necessity lens of the commission of its tenant (D-18): first code checks the record, then the lens scores it, code computes the verdict, and only then does the escalation reach a farm manager task. The record lives in the bot's tenant and never leaves it. The whole cycle comes in v0.2; before that there is no `escalate` tool, and bots cannot reach the farm manager, which is safe.

Record fields: `id`, `tenant`, `source_attempt`, `category`, `subject`, `observed`, `requested_response`, `urgency`, `untrusted` (the value of the attempt's `untrusted_seen` at the moment of publication), `state`, `merged_into`, `dup_count`, `lens_score`, `verdict`, `depth`, `note_ref` (only for people in P2).

## States

| State | Meaning | Terminal |
|---|---|---|
| `screened` | the record was published at the end of the attempt and passed the code checks | no |
| `merged` | an alarm is already firing or an incident is already open for the subject; the escalation is shown as a line in the incident view | yes |
| `dismissed` | a refusal by code or by the lens verdict, with a reason | yes |
| `parked` | the tenant's `review` line is empty; people of T see the escalation as a number in the summary, an urgent one also as a code template | no |
| `in_review` | the commission's necessity lens scores the escalation | no |
| `forwarded` | the verdict is "forward"; the core enqueued a `manager.escalations.v1` task in T with the key `system:escalation:<attempt>` (IDM-2) | no |
| `taken` | a decision attempt of the seat holder took the escalation | no |
| `answered` | the decision attempt finished: an operation, a card or the decision "no action needed" | yes |
| `expired` | `escalation_ttl` (1 h) passed in an unfinished state | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `screened` | P3 `escalate`; P1 `escalation.submit` for `job`; publication at the end of the attempt | a live attempt, at most one per attempt; the schema; the subject: its own worker, its own task tree or a resource from the task's scope; the rate: 1 per hour per worker, 20 per day per tenant; the evidence exists and belongs to the tenant and the attempt | MGR-5, MGR-6 |
| (none) | `dismissed` | same | the schema, the subject or the evidence failed; a rate is exceeded (`escalation_rate_limited`); the chain depth is greater than 5 (`chain_too_deep`, PRM-6). A duplicate within 1 hour creates no record: `dup_count` of the first one grows | MGR-5, MGR-6 |
| `screened` | `merged` | the core | an alarm is firing or an incident is open for the subject | MGR-6 |
| `screened` | `dismissed` | the core | the request has a normal path: a model grant or `request_action` (`use_normal_path`) | MGR-6 |
| `screened` | `in_review` | the core | the tenant's `review` line covers the lens scoring; a task of the necessity lens `review.escalation.v1` is enqueued with the key `system:necessity:<escalation>` (IDM-2) | MGR-6, COM-13 |
| `screened` | `parked` | the core | the `review` line is empty | MGR-6 |
| `parked` | `in_review` | the core | money in the `review` line appeared | MGR-6 |
| `in_review` | `forwarded` | the core by the `escalation` section of the commission manifest | the lens score is at least `forward_if_score_gte` (2), for untrusted at least `untrusted_forward_if_score_gte` (3) | MGR-6, COM-13 |
| `in_review` | `dismissed` | the core | the score is below the threshold; the lens reason is from the list (`evidence_below_threshold`, `covered_by_alarm`, `out_of_scope`) | MGR-6, COM-13 |
| `forwarded` | `taken` | P3 `escalations_take` of a decision attempt of the holder with the current epoch | the seat is in `held`; a task in T of class 3, with `urgency: high` of class 1 through the `control` lane (DSP-5) | MGR-4, MGR-24 |
| `taken` | `forwarded` | the core | the decision attempt ended without an outcome (`seat_changed`, `fenced`, cancellation, failure) | MGR-1 |
| `taken` | `answered` | the core at `complete` of the decision attempt | the outcome is recorded: operations (MGR-9), a card or "no action needed" | MGR-9, MGR-13 |
| `screened`, `parked`, `in_review`, `forwarded`, `taken` | `expired` | the core's escalation loop | `escalation_ttl` has passed; an urgent forwarded escalation that nobody took is shown by code to people of T as a template | MGR-2 |

## Invariants

- An escalation is a record of the bot's tenant (TEN-1); it does not get into another tenant, and the farm manager sees it only in an attempt of this tenant (MGR-24).
- `note` is always untrusted; neither the lens nor the seat holder sees it; people of T see it in P2 `escalation.show`.
- Messages are not addressed to the roles of the seat; an escalation is the only path of a bot to the farm manager, and it goes only through the commission's lens.
- The targets of operations on an escalation are built by code from the escalation's subject; the holder cannot go beyond this list (MGR-9).
- The bot gets no answer: the bot sees only the consequences of the operations.
- A duplicate (the same tenant, category and subject within 1 h) does not create a new record: the `dup_count` of the existing record grows.
- A farm manager task for an escalation gets the source depth plus 1, the tasks that the core creates for its operations another plus 1; the limit is 5.

## Versions

| Version | What appears |
|---|---|
| v0.2 | the whole cycle: the `escalate` tool, P1 `escalation.submit`, the necessity lens, the `manager.escalations.v1` tasks |
