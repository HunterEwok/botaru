# Lifecycle: governance case

## In short

A case (`GovernanceCase`) is one promotion of a functionary into the commission of its tenant, together with the "who leaves" vote if there is no free seat. There is one case kind, `promotion`, with the mode `removal` (someone in the commission leaves) or `vacancy` (the candidate takes a free seat without a vote). Only the core's `governance` loop opens a case, for the head of the promotion queue (GOV-6, GOV-14); code runs the rounds and the count (GOV-8, GOV-23); the outcome takes effect only after a person's button (GOV-9). A case lives only in the commission's tenant, it has no free text, and no bot sees it (GOV-29). Only the core writes the case record; its schema is `spec/model/governance-case.schema.json`.

## States

| State | Meaning | Terminal |
|---|---|---|
| `opened` | The snapshot is taken (GOV-12): the voters V, the list L, the ordering seed | no |
| `parked` | There is no money in the `governance` line to evaluate the case (`parked(governance_budget)`) | no |
| `balloting(n)` | Round n is running: the core collects exactly one ballot from each of `expected` | no |
| `counted(n)` | Round n is counted by code | no |
| `confirming` | There is an outcome; the `promotion_card` card waits for a human decision for up to 7 days | no |
| `effective` | A person confirmed; the change is done in one transaction | yes |
| `rejected` | A person rejected with a reason code | yes |
| `expired` | 7 days without a decision; nobody leaves | yes |
| `canceled` | The case is canceled with a reason: `human`, `candidate_ineligible` or `seat_changed` | yes |

The flags `hold` (a pause by people, a hold on the `governance` scope) and `stuck` (a round waits for votes longer than `stuck_after`) are not states: they are set on top of any open state.

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `opened` | the `governance` loop | the queue head under the conditions of GOV-14; the tenant has no open case and no hold on the `governance` scope; in the `removal` mode L is not empty and the `swap_cooldown` pause has passed | GOV-6, GOV-12, GOV-14 |
| `opened` | `parked` | the `governance` loop | the `governance` line lacks the money to evaluate the case, starting with the farm manager's ballot | GOV-15 |
| `parked` | `opened` | the `governance` loop | the money appeared | GOV-15 |
| `opened` | `confirming` | the `governance` loop | the `vacancy` mode, or L has one seat: its holder leaves without ballots | GOV-8, GOV-14 |
| `opened` | `balloting(1)` | the `governance` loop | the money is reserved, the ballot tasks are enqueued | GOV-7, GOV-15 |
| `balloting(n)` | `counted(n)` | the `governance` loop | exactly one ballot of the round is recorded for each of `expected` | GOV-23 |
| `counted(n)` | `balloting(n+1)` | the `governance` loop | there is no outcome: the seat with the fewest votes is struck out | GOV-8 |
| `counted(n)` | `confirming` | the `governance` loop | there is an outcome: a seat with at least `need` votes, or the only one left | GOV-8 |
| any open | same, flag `hold` | P2 `hold.set` on the `governance` scope or the "Pause" button on the `governance_stuck` card | a hold by people; lifting the hold continues the case from the same place | GOV-23 |
| any open | same, flag `stuck` | the `governance` loop | a round waits for votes longer than `stuck_after` (24 h) for an infrastructure reason; the flag is removed when the round is closed | GOV-23 |
| `confirming` | `effective` | the "Confirm" button (`governance.confirm:commission`) | "Details" was opened; the candidate's change has no unanswered attribution candidates; the deadline has not passed | GOV-9, GOV-30 |
| `confirming` | `rejected` | the "Reject" button with a reason code | `governance.confirm:commission` | GOV-9, GOV-31 |
| `confirming` | `expired` | the `approval-expiry` loop (the outcome on silence from the decision registry row, APR-12) | 7 days without a decision | GOV-9 |
| any open | `canceled(human)` | P2 `governance.cancel` or the "Cancel" button on the `governance_stuck` card | a person with `admin:governance` in the case's tenant | GOV-23 |
| any open | `canceled(candidate_ineligible)` | the `governance` loop | the candidate lost eligibility: left, is under a hold by people on the `bot` scope, or got a confirmed attribution | GOV-14 |
| any open | `canceled(seat_changed)` | the core on a holder change | a seat from L changed its holder or became free | GOV-12, IDN-10 |

## Invariants

- A tenant has at most one open case; a case does not cross tenants (GOV-16).
- The snapshot of V, L and the seed does not change after opening. `expected` decreases only by a person's action or a core loop, with the flag `voters_removed` (GOV-23).
- A round does not close without a ballot from each of `expected`. A `defaulted` ballot is written only for a reason in the model; an infrastructure failure holds the round; more than half `defaulted` in a round give the flag `defaulted_majority` and the line "outcome by the code rule" on the card (GOV-23).
- The outcome is deterministic and always exists: exactly one leaver in at most |L| - 1 rounds (GOV-8).
- A ballot is a seat id and up to 3 reason codes; there is no free text in a case (GOV-7).
- A ballot task carries the core key `system:governance:<case>:<round>:<bot>` (IDM-2): a voter has one task per round, and handing it out again on another worker is a new attempt of the same task (GOV-7, GOV-23).
- No bot sees the case; a ballot sees only its own package (GOV-29).
- Every transition, ballot, count, flag and decision writes audit metadata (GOV-21).
- After `effective` the change is done in one transaction: the holder, the epochs, the leaving, the new worker identity of the candidate, the fact offers, the seal, the `swap_cooldown` clocks and the newcomer protection.
- After `rejected` the outcome is set by the reason code (GOV-31): `wrong_leaver` immediately opens a new case without the rejected seat, with the flag `reopened_after_wrong_leaver`, and is accepted at most once per queue entry; `not_now` keeps the entry as the queue head and gives a `swap_cooldown` pause; `wrong_candidate` returns the entry to the queue head the first time and removes it the second time.
- After `expired` nobody leaves, the entry stays the queue head, and the next case comes no earlier than after `swap_cooldown`; `expired` does not count toward the "Reject" count.
- After `canceled` the candidate's entry returns to its previous place in the queue; if the candidate left, the entry is removed.

## Versions

| Version | What appears |
|---|---|
| v0.1 | Only a prohibition: bots have no operations that open, move or close a case (GOV-6); a person assigns seats |
| v0.2 | The whole case cycle, ballots, the count, flags, the `promotion_card` and `governance_stuck` cards |
