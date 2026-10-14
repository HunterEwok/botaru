# Lifecycle: governance case

## In short

A case (`GovernanceCase`) is one promotion of a functionary into the commission of its tenant. There is one case kind, `promotion`, with the mode `removal` (one holder from the list L leaves) or `vacancy` (the candidate takes a free seat). Only the core's `governance` loop opens a case, for the head of the promotion queue (GOV-6, GOV-14). In the `removal` mode the core writes the list L with the figures of every seat (GOV-12); code proposes the leaver by accuracy when it can (GOV-43), and otherwise a person chooses it on the card. The outcome takes effect only after a person's button (GOV-9). There are no ballots. A case lives only in the commission's tenant, it has no free text, and no bot sees it (GOV-29). Only the core writes the case record; its schema is `spec/model/governance-case.schema.json`.

## States

| State | Meaning | Terminal |
|---|---|---|
| `opened` | The list L and the figures of its seats are written (GOV-12) | no |
| `confirming` | The `promotion_card` card waits for a human decision for up to 7 days: the proposed leaver, or the choice from L | no |
| `effective` | A person confirmed; the change is done in one transaction | yes |
| `rejected` | A person rejected with a reason code | yes |
| `expired` | 7 days without a decision; nobody leaves | yes |
| `canceled` | The case is canceled with a reason: `human`, `candidate_ineligible` or `seat_changed` | yes |

The flag `hold` (a pause by people, a hold on the `governance` scope) is not a state: it is set on top of any open state.

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `opened` | the `governance` loop | the queue head under the conditions of GOV-14; the tenant has no open case and no hold on the `governance` scope; in the `removal` mode L is not empty and the `swap_cooldown` pause has passed | GOV-6, GOV-12, GOV-14 |
| `opened` | `confirming` | the `governance` loop | the card is sent: in the `vacancy` mode with the free seat, in the `removal` mode with L, the figures of its seats and the leaver code proposed, if any | GOV-12, GOV-14, GOV-43 |
| any open | same, flag `hold` | P2 `hold.set` on the `governance` scope | a hold by people; lifting the hold continues the case from the same place | GOV-14 |
| `confirming` | `effective` | the "Confirm" button (`governance.confirm:commission`) | "Details" was opened; the candidate's change has no unanswered attribution candidates; the deadline has not passed; in the `removal` mode the button names the proposed seat or the seat the person chose from L | GOV-9, GOV-12, GOV-30 |
| `confirming` | `rejected` | the "Not now" or "Wrong candidate" button (a "Reject" reason code) | `governance.confirm:commission` | GOV-9, GOV-31 |
| `confirming` | `expired` | the `approval-expiry` loop (the outcome on silence from the decision registry row, APR-12) | 7 days without a decision | GOV-9 |
| any open | `canceled(human)` | P2 `governance.cancel` | a person with `admin:governance` in the case's tenant | GOV-14 |
| any open | `canceled(candidate_ineligible)` | the `governance` loop | the candidate lost eligibility: left, is under a hold by people on the `bot` scope, or got a confirmed attribution | GOV-14 |
| any open | `canceled(seat_changed)` | the core on a holder change | a seat from L changed its holder or became free | GOV-12, IDN-10 |

## Invariants

- A tenant has at most one open case; a case does not cross tenants (GOV-16).
- L does not change after opening, and the farm manager seat is never in it (GOV-12).
- The card offers only seats of L; code proposes a seat only under GOV-43, and the proposal decides nothing before the person's button.
- No bot sees the case and no bot takes part in the choice (GOV-29).
- Every transition, the list with its figures, the proposal, the flags and the decision write audit metadata (GOV-21).
- After `effective` the change is done in one transaction: the holder, the epochs, the leaving, the new worker identity of the candidate, the fact offers, the seal, the `swap_cooldown` clocks and the newcomer protection.
- After `rejected` the outcome is set by the reason code (GOV-31): `not_now` keeps the entry as the queue head and gives a `swap_cooldown` pause; `wrong_candidate` returns the entry to the queue head the first time with its own `swap_cooldown` pause, during which the next entries may go ahead (D-101), and removes it the second time. A case is never reopened with another list.
- After `expired` nobody leaves, the entry stays the queue head, and the next case comes no earlier than after `swap_cooldown`; `expired` does not count toward the "Reject" count.
- After `canceled` the candidate's entry returns to its previous place in the queue; if the candidate left, the entry is removed.

## Versions

| Version | What appears |
|---|---|
| v0.1 | Only a prohibition: bots have no operations that open, move or close a case (GOV-6); a person assigns seats |
| v0.2 | The whole case cycle, the list L with the figures of its seats, the person's choice, flags and the `promotion_card` card |
| v0.3 | The leaver code proposes by accuracy (GOV-43) |
