# Lifecycle: incident attribution candidate

## In short

An attribution candidate (`Attribution`) is a possible link between an incident and an executed change that a functionary's proposal brought. Only code creates a candidate, by the rules of `attribution-rules.yaml`, or a person with a P2 command; only a person with `governance.attribute` decides (GOV-13). A confirmed attribution takes away the change's promotion trigger and goes into lens calibration. A person's silence does not expire: the promotion trigger waits until every candidate has been answered, and the attribution window stays open all that time (GOV-30). The answers "No" and "Don't know" can be changed while the window is open; a person cannot change "Yes" or the closing of the window. If people judged the incident a false alarm (with a reason code and "Details", MGR-8), candidates without a "Yes" answer are canceled, and an attribution with a "Yes" answer becomes disputed: its fate is decided by the author of the answer on a card, or by 7 days of silence (D-57, D-79).

## States

| State | Meaning | Terminal |
|---|---|---|
| `candidate` | The candidate is created and waits for an answer on the `attribution_card` card | no |
| `unknown` | A person answered "Don't know" or did not answer before the card's deadline: "undecided", the promotion trigger is closed, a reminder once a week | yes, when the attribution window has closed |
| `confirmed` | A person answered "Yes": the incident is linked to the change | yes |
| `rejected` | A person answered "No": there is no link. The answer can be changed while the change's attribution window is open | yes, when the attribution window has closed |
| `disputed` | An incident with a "Yes" answer was judged a false alarm (`dismissed`): the attribution is disputed, the author of the "Yes" answer got an `attribution_dispute` card; the change has no promotion trigger, the attribution does not go into calibration | no |
| `canceled` | The incident was judged a false alarm (`dismissed`) and there was no "Yes" answer, or the author of the "Yes" answer withdrew it or stayed silent for 7 days: the attribution is canceled with the reason `incident_dismissed` and means nothing anymore | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `candidate` | the `governance` loop by `attribution-rules.yaml` | an incident of a class with `change_sensitive` touches the changed object directly (`direct`) or through a first-level dependency in the catalog (`dependency_1`) and is opened within the attribution window: 14 days from `realized`, longer until the promotion trigger appears, and for a change that gave the trigger of a queue entry, until the case of that entry is `effective` | GOV-13 |
| (none) | `candidate` | P2 `attribution.create` | a person with `governance.attribute` in the change's tenant; the attribution window is open | GOV-13 |
| `candidate`, `unknown`, `rejected` | `confirmed` | the "Yes" button on the card or the reminder | `governance.attribute`; "Details" was opened (APR-14); the attribution window is open | GOV-13, GOV-9 |
| `candidate`, `unknown` | `rejected` | the "No" button | `governance.attribute`; "Details" was opened (APR-14) | GOV-13 |
| `candidate` | `unknown` | the "Don't know" button or the `approval-expiry` loop (the outcome on silence from the decision registry row, APR-12) | the answer "Don't know", or the card's deadline (7 days) passed without an answer | GOV-30 |
| `rejected` | `unknown` | the "Don't know" button | `governance.attribute`; the attribution window is open | GOV-13 |
| `candidate`, `unknown`, `rejected` | `canceled` | the core in the transaction of the incident's transition to `dismissed` (P2 `incident.dismiss {reason_code, details}` or a channel command of a person with `incident.manage`) | the incident of this attribution is judged false; the pending card is closed; the attribution window does not matter here | GOV-13, MGR-8 |
| `confirmed` | `disputed` | the core in the same `dismissed` transaction | the attribution has a "Yes" answer; the author of the answer gets an `attribution_dispute` card in a direct chat with the reason code and the "Details" of the person who dismissed the incident | GOV-13, D-79 |
| `disputed` | `confirmed` | the "Yes" button on the `attribution_dispute` card | only the author of the "Yes" answer; the card's deadline is 7 days | GOV-13 |
| `disputed` | `canceled` | the "Withdraw" button on the `attribution_dispute` card; the `approval-expiry` loop at the card's deadline; `disabled` of the answer's author | the author withdrew the answer, stayed silent for 7 days or is disabled; reason `incident_dismissed` | GOV-13 |

## Invariants

- No bot, including seat holders and the farm manager, creates, changes, confirms or hides an attribution (GOV-13).
- A person cannot change "Yes" or the closing of the attribution window, while "No" and "Don't know" can be changed while the window is open: this is the only exception to the one-time decision on a subject (APR-4).
- Judging an incident false cancels candidates without a "Yes" answer and moves `confirmed` to `disputed`; no new candidates are created for such an incident. Nobody erases someone else's "Yes" answer except its author or 7 days of the author's silence (D-79). `canceled` does not count as a confirmed attribution: it does not take away the change's trigger, does not close eligibility for 90 days, and leaves the lens calibration. The trigger appears under the general conditions, but not before the cancellation; a case already canceled because of this attribution is not resumed (GOV-13, GOV-14, D-57).
- While the change has candidates in `candidate`, `unknown` or `disputed`, there is no promotion trigger; the trigger appears only when every candidate, except the canceled ones, was answered "No". The change trial (PRP-21) and its outcome (PRP-22) are not shifted or extended by waiting for an answer (GOV-30).
- An attribution confirmed later for a change that already gave the trigger of a queue entry removes that trigger (GOV-14).
- A candidate in `unknown` does not expire; once a week people with `governance.attribute` get a reminder, at most 3 times per candidate, after that a line in the weekly summary (GOV-30).
- "Yes" and "No" are accepted only after "Details" was opened; for every answer the core writes `response_ms` and `details_opened` to the audit and shows them on the promotion card (GOV-13, GOV-9).
- `confirmed` on the change of a candidate of an open case gives `canceled(candidate_ineligible)`; a confirmed attribution within 90 days closes eligibility for promotion (GOV-14).
- Confirmed attributions go into the calibration of the safety and operations lenses (COM-11).
- The card has no model text: only the incident, the class, the changed objects and the rule by which code found the link.

## Versions

| Version | What appears |
|---|---|
| v0.1 | Only a prohibition: bots have no operations on attributions (GOV-13) |
| v0.2 | The whole cycle: candidates, the `attribution_card` card, `unknown` and reminders, the `disputed` dispute and the `attribution_dispute` card |
| v0.3 | The same cycle for the kinds `rejected_proposal` (card `rejection_link_card`, GOV-38, GOV-40) and `script_run` (card `script_link_card`, TRN-7) by the schema `attribution.v2`; a dismissal of the incident cancels such candidates and returns departed members (GOV-42) |
