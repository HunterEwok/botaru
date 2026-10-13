# Lifecycle: promotion queue entry

## In short

A promotion queue entry (`PromotionEntry`) appears for a functionary whose proposal people accepted, the core executed, and whose promotion probation (`probation`, 14 days from `realized`) passed without a confirmed incident attribution and without a measured deterioration (GOV-14, GOV-32). A trigger is given only by a change whose promotion probation ended when the v0.2 rules were already in force in the tenant. Every tenant has its own queue, a bot has one entry in it, and there is no deadline and no length ceiling. Only the `governance` loop maintains the entry; the bot does not see it (GOV-29). The head of the queue gets a case when the conditions for it are met.

## States

| State | Meaning | Terminal |
|---|---|---|
| `queued` | The entry waits for its turn; ordered by the time the trigger appeared | no |
| `in_case` | A promotion case is open for the entry | no |
| `removed` | The entry is removed | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `queued` | the `governance` loop | a promotion trigger by GOV-14; the bot has no entry; the bot is a functionary (a role of the profile `worker`, `manager`, `reporter` or `aggregator`, the worker is enrolled without an owner) | GOV-14 |
| `queued`, `in_case` | `queued`, placed by the time of the new trigger | the `governance` loop | a person confirmed an incident attribution to the change that gave the trigger, and the track record has a later trigger; an open case gets `canceled(candidate_ineligible)` | GOV-14, GOV-13 |
| `queued`, `in_case` | `removed` | the `governance` loop | the same, but there is no later trigger | GOV-14, GOV-13 |
| `queued` | `queued` | the `governance` loop when trying to open a case | eligibility failed (`not_eligible`): the entry waits and lets the next one go ahead | GOV-14 |
| `queued` | `in_case` | the `governance` loop | the entry is the first of the eligible ones, the conditions for opening a case are met | GOV-6, GOV-14 |
| `in_case` | `removed` | the case becomes `effective` | the candidate took the seat | GOV-9 |
| `in_case` | `in_case` | the case is `rejected` with the reason `wrong_leaver` | only once per entry: the `governance` loop immediately opens a new case without the rejected seat with the flag `reopened_after_wrong_leaver`; if L is empty, the entry becomes `queued` first | GOV-31 |
| `in_case` | `queued`, first | the case is `rejected` with the reason `not_now`, or for the first time with the reason `wrong_candidate`; the case is `expired` | the next case no earlier than `swap_cooldown` after `not_now` and `expired` | GOV-31, GOV-14 |
| `in_case` | `removed` | the second case of this entry is `rejected` with the reason `wrong_candidate` | the entry comes back only with the bot's next success | GOV-31 |
| `in_case` | `queued`, previous place | the case is `canceled` | the candidate did not leave | GOV-14 |
| `queued`, `in_case` | `removed` | the core | the bot retired (`retired`) or a person seated it with P2 `seat.assign` | GOV-14, IDN-11 |

## Invariants

- A bot has at most one entry; the next success of a bot that is already in the queue is written to its track record (GOV-14).
- The queue order is set by the time the entry's trigger appeared; "Reject" with the reason `not_now` or the first `wrong_candidate`, and `expired`, keep the entry first.
- An entry does not expire over time and is not evicted by the queue length.
- Only a functionary has an entry: personal assistants, translators, commission members and roles without a model have no entries.
- Eligibility (30 days of service, no confirmed attributions within 90 days, the identity is `active`) is checked when a case is opened, not when entering the queue.
- The "Reject" counters with the reasons `wrong_candidate` and `wrong_leaver` live in the entry and are reset together with it.
- A trigger is given only by a proposal that stays `realized` at the moment the trigger appears; `reverted` and a `canceled` trial give no trigger (GOV-14).
- No bot sees the queue or its place in it (GOV-29).

## Versions

| Version | What appears |
|---|---|
| v0.2 | The whole entry cycle; before v0.2 the successes of functionaries are written only to the track record |
