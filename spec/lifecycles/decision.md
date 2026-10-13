# Lifecycle: human decision on a card

## In short

A human decision is the common cycle for any subject that waits for a person's button: an action approval, a proposal, a campaign, a script draft, a `confirm` step of a script, a farm manager `control` card, a personal memory record, a task takeover, a promotion case, an attribution of an incident to a change, a person's request about their own data. Each subject lives in its own cycle (for example `approval.md`, the proposal cycle, the case cycle), and this file describes only the common part: the pending card and its outcome. The core creates the subject and the card deliveries by the decision registry row (APR-12); the channel draws the card from the record (APR-3); a person decides through P1 `decide(subject_kind, id, nonce, decision, reason_code)` from the channel where the card was delivered (APR-4). A decision is made once, with one exception: for an attribution of an incident to a change, the answers "No" and "Don't know" do not close the subject while the attribution window is open (GOV-13), while the answer "Yes" and the closing of the window are final; only its author can withdraw a "Yes", on the dispute card, after the incident was judged false (D-79). A notification without a decision (`notice`, for example `manager_stopped` with ready `farmctl` commands) does not go through this cycle: it has a registry row, a deadline and repeats, but no `decide`. Farm alarms do not go through this cycle either: the monitoring system delivers them to people, and "Acknowledged" is pressed there (MON-1). Operation buttons (pause and cancel of a campaign or a script, a hold, pause and cancel of a stuck case) do not decide a subject and do not go through this cycle: they call their own operation with the same checks of the channel, the `nonce` and the `human` principal (APR-4).

## States

| State | Meaning | Terminal |
|---|---|---|
| `pending` | the subject waits for a decision; the cards are delivered or wait for delivery (beyond the card limit per person, APR-15); "Details" openings and votes can accumulate if the decision needs two people; for an attribution the answer "No" or "Don't know" is also kept here while the attribution window is open | no |
| `decided` | a person with the right made one of the decisions of the registry row; after that the subject lives in its own cycle (executing an action, realizing a proposal, the case outcome taking effect); for an attribution this is the answer "Yes" or the closing of the attribution window | yes |
| `expired` | the deadline passed without a decision, and the registry row says that silence closes the subject; the outcome of silence is written in the row (for example "the action is not executed", "nobody leaves") | yes |
| `canceled` | the core took the subject off decision, with a reason: a hold, the initiator being disabled or losing the right, restore from backup, a change of the seat holder, cancellation by an admin or the operator, closing of the subject by its own cycle | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `pending` | the core creates a subject that needs a person (`request_action`, a proposal in `presented`, a campaign, a script draft, a `confirm` step, a farm manager operation with a card, a memory record, a takeover, a case outcome, an attribution candidate, a person's request about their own data) | the card kind has a registry row; the recipients are found by the right from the row; the card deliveries are created in the same transaction | APR-12, APR-3 |
| `pending` | `pending` | the "Remind approvers" button on the initiator's copy of the card: P1 `card.remind(subject_kind, id, nonce)` (subject `approval`) | the initiator pressed it; at most once every 5 minutes; approvers without a decision get a `card_update` | APR-19 |
| `pending` | `pending` | the "Details" button: P1 `card.details(subject_kind, id, nonce)` | the card's channel; the person has the right to read the record (`data:<class>` not below the mark); above the group chat ceiling the details go to a direct chat | APR-11, APR-14, PRM-3 |
| `pending` | `pending` | a person's button: P1 `decide` of the first of two approvers | the row needs two different people (`approvals: 2`) | APR-16 |
| `pending` | `pending` | the background loop `approval-expiry` at the deadline | in the registry row silence does not close the subject but gives a reminder (for example an incident attribution: the candidate becomes `unknown` ("undecided"), the promotion trigger does not appear (GOV-30), a reminder once a week; a person's request about their own data: a reminder at half of the deadline, a notification to the operator at the deadline) | APR-12 |
| `pending` | `pending` | a person's button on an `attribution` subject: "No" or "Don't know" | the attribution window is open; the answer is written to the subject and as a decision event, and a person with the right can change it | APR-4, GOV-13 |
| `pending` | `decided` | closing of the attribution window | an `attribution` subject: the last answer "No" or "Don't know" (or `unknown` after silence) becomes final | APR-4, GOV-13 |
| `decided` | `pending` | P2 `incident.dismiss` with a reason code for an incident that has a "Yes" answer (v0.2) | the attribution becomes disputed; the author of the "Yes" gets a dispute card and confirms (`decided`) or withdraws the answer; 7 days of silence withdraw it; while the dispute is open there is no promotion trigger | APR-14, GOV-13, D-79 |
| `pending` | `decided` | a person's button: P1 `decide(subject_kind, id, nonce, decision, reason_code)` | a `human` principal with strong identity from the card's channel; the `nonce` is valid; the right to this decision at the moment of pressing; in a group chat only with `membership_assurance: enforced`; self-approval is forbidden where the row requires it; for "Lift veto", "Confirm" of a case, "Yes" and "No" of an attribution, "Details" was opened before; for an attribution only "Yes" leads to `decided` | APR-4, APR-9, APR-14, IDN-3 |
| `pending` | `expired` | the background loop `approval-expiry` | the deadline from the registry row or from the subject record has passed; silence closes the subject | APR-12 |
| `pending` | `canceled` | the core in the transaction of the cause: P2 `principal.off`, P2 `ungrant`, P2 `hold.set` with the effect `no_action`, restore from backup, P2 `seat.assign` or `seat.vacate`, cancellation by an admin or the operator, closing of the subject by its cycle (the campaign was canceled, the case was canceled, the person withdrew their request, the attribution candidate was canceled together with a false incident) | the reason is written into the subject | APR-6, HLD-3, PRM-3 |

Every transition and every "Details" opening create a `card_update` delivery with the outcome or with an "already approved" mark, so that all recipients see the same thing. A repeated `decide` with the same principal, the same decision and the same `nonce` returns the same success and changes nothing (IDM-3); a different decision or a different principal on a decided subject gets `already_decided`; pressing after the deadline gets `expired`; pressing without the right gets `forbidden`, and the full reason is written to the audit (PRM-8).

## Invariants

- The transition to `decided` happens exactly once; the decision is atomic and made once (APR-4). The only exception: for an attribution, the answers "No" and "Don't know" do not close the subject before the attribution window closes and can be changed, while only its author changes a "Yes", on the dispute card. Judging an incident false does not erase someone else's "Yes": it makes the attribution disputed, and the author of the answer decides (GOV-13, D-79).
- Only a `human` principal decides; `decide` from `service`, `system` and bots is rejected (APR-4).
- `decide` is accepted only from the channel where the card of this subject was delivered, with its `nonce`; something forwarded or quoted is not a decision (APR-4, IDN-3).
- The channel draws the card from the record by a code template; the button carries only `(subject_kind, id, nonce)`; model text is visible only behind "Details", in the untrusted text wrapper (APR-3).
- Every card kind has a registry row, and at the bottom of the card there is the line "why you, until when, what happens if you stay silent" (APR-12).
- Every decision writes a decision event with `response_ms` and `details_opened`; only per-tenant summaries are computed from it (APR-11, APR-13).
- In a channel without buttons the card shows a text command with the id and the `nonce`, which is checked the same way as a button press; such commands are accepted only in a direct chat (PRM-15).
- A case that is decided by an operator operation gets no buttons: the notification shows a ready `farmctl` command (PRM-14).
- The decider being disabled or losing the right does not cancel the subject: other people with the right can decide (APR-6).

## Versions

| Version | What appears |
|---|---|
| v0.1 | the cycle and the subjects `approval`, `memory_record` and `subject_request` for data erasure (D-76); the decision registry and the bottom line of the card; the decision event with `response_ms` and `details_opened`; P1 `card.details`; the copy of the approval card to the initiator and P1 `card.remind` (APR-19); the subjects `proposal` (D-71), `campaign`, `script`, `script_gate`, `control`; campaign and script operation buttons; the `manager_stopped` notification with commands instead of buttons |
| v0.2 | the subjects `takeover`, `governance_case`, `attribution`, `subject_request` for a copy of data; changing the answers "No" and "Don't know" for an attribution while the attribution window is open; the attribution dispute after an incident is judged false (D-79); "Details" is mandatory before the decisions from the list; the card limit per person per day; two approvers for high-risk actions; the "approve without reading" summary |
