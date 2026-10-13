# Lifecycle: proposal

## In short

A proposal (`Proposal`) is born at a successful `complete` of an analysis or aggregator attempt, passes code checks and the commission of its tenant, is shown to people, is executed by the farm's ordinary path and is closed by execution, by a refusal of people or by a deadline. Only code changes the state, by this table, and only a person presses the buttons (PRP-6). A refusal at creation creates no record: a `proposal.rejected.v1` event with a reason remains. The audit sample branch shows people some of the proposals rejected and parked by the commission as a blind card. The change trial after "Accept" runs its own cycle, see [trial.md](trial.md); how the whole improvement cycle works is described in [improvements.md](../../docs/improvements.md).

## States

| State | Meaning | Terminal |
|---|---|---|
| `screened` | created by the core; waits for code checks and for money for the reviews | no |
| `in_review` | the code checks passed, the `tree_budget_eur` sum of the review types is reserved, the lens tasks are handed out (for the kind `none` only `value` and `security`, COM-14) | no |
| `recommended` | the code verdict is "recommend"; waits for the digest | no |
| `parked` | waits; the reason is from a closed list: `score`, `veto_lens_missing`, `review_budget`, `conflict`, `evidence_drift` | no |
| `dismissed` | did not pass a code check or the verdict; the reason: a check code (for example `no_op`), `veto`, `below_min`, `low_score` | yes; can be selected into the audit sample |
| `presented` | went into a delivered digest; the card waits for a decision | no |
| `declined` | a person pressed "Reject" with a reason from a closed list | yes |
| `expired` | the `park_ttl`, `recommend_ttl` or `decide_ttl` deadline expired | yes |
| `noted` | a proposal of the kind `none` is accepted "for information"; nothing is created, there is no trial and no credit | yes |
| `realizing` | accepted: the ordinary path object is created or an executor is assigned, the trial record is created | no |
| `realized` | executed by a verified link | yes, except for the transition to `reverted` |
| `realization_failed` | the ordinary path object was not executed (`object_failed`) or `realize_by` expired (`timeout`) | yes |
| `reverted` | the executed change was reverted by a commit with a `Botaru-Revert` line with the reason `harm_observed`, `superseded` or `other`; it gives no trigger for promotion | yes |
| `sampled` | selected into the audit sample by the recorded seed | no |
| `sample_presented` | the blind card "Would you accept?" is delivered | no |
| `sample_judged` | a person answered "Would accept"; the scores and the veto are revealed | no |
| `sample_closed` | the sample item is closed without acceptance | yes |

An aggregator proposal does not change the states of other proposals: an overlap with them gives a conflict (PRP-9) or a `related_to` mark (PRP-17).

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `screened` | `complete` of an analysis or aggregator attempt (port P1) | the body passed the schema; the evidence matched what was reread; the change is by the allowlist, in its own folder, the target is `proposable`, the direction is allowed; the metric is from the registry; the limits are not exceeded; otherwise there is no record, there is a `proposal.rejected.v1` event | INS-3, PRP-1, PRP-2, PRP-3, PRP-5, PRP-7, PRP-8, PRP-10, PRP-11, PRP-16, PRP-21 |
| `screened` | `in_review` | the core loop `proposal` | the code checks passed; the `tree_budget_eur` sum of the review types of the lenses being enqueued is reserved in the `review` line | COM-5, COM-6, COM-14 |
| `screened` | `dismissed` | the `proposal` loop | a code check failed (for example `no_op`) | COM-5 |
| `screened` | `parked` | the `proposal` loop | the target is locked by a conflict (`conflict`), or there is no money for reviews for longer than `review_wait_max` (`review_budget`) | PRP-9, COM-6 |
| `in_review` | `recommended` | the `proposal` loop | the code verdict is `recommended`; for the kind `none` by two reviews | COM-4, COM-14 |
| `in_review` | `parked` | the `proposal` loop | the verdict `parked(score)` or `parked(veto_lens_missing)` | COM-4 |
| `in_review` | `dismissed` | the `proposal` loop | the verdict `dismissed(veto)`, `dismissed(below_min)` or `dismissed(low_score)` | COM-4 |
| `parked` | `screened` | the `proposal` loop | the evidence changed by more than the declared share; the trial of the conflicting proposal ended; the `review` line was topped up; the lens with a veto has an executor again | PRP-5, PRP-9, COM-6, COM-7 |
| `parked` | `expired` | the `proposal` loop | `park_ttl` expired (30 days by default) | PRP-6 |
| `parked` (`score`), `dismissed` (`veto`, `below_min`, `low_score`) | `sampled` | the `proposal` loop, the monthly audit sample | selected by the recorded seed | DIG-5 |
| `recommended` | `presented` | delivery of the `proposal_digest` digest through a channel (E1) | the evidence reread before showing is within tolerance | DIG-1, DIG-2, PRP-2 |
| `recommended` | `parked` | the `proposal` loop when assembling the digest | an evidence drift (`evidence_drift`) or a conflict | PRP-2, PRP-9 |
| `recommended` | `expired` | the `proposal` loop | 28 days without being shown (`recommend_ttl`) | PRP-6 |
| `presented` | `realizing` | the "Accept" button: the `decide` operation (P1) from the card's channel (APR-4) | the kind is not `none`; the person has the right to decide and to start the realization; the target is not locked by a conflict or a running trial | DIG-3, PRP-4, PRP-9, PRP-18, PRP-21 |
| `presented` | `noted` | the "Accept" button | the kind is `none` | DIG-3, PRP-4 |
| `presented` | `declined` | the "Reject" button and the reason card | the reason is from a closed list | DIG-3, DIG-4, PRP-5 |
| `presented` | `recommended` | the "Postpone" button | at most 2 times | DIG-3 |
| `presented` | `expired` | the `proposal` loop | `decide_ttl` expired (14 days by default) | PRP-6 |
| `presented` | `parked` | the `proposal` loop | a conflicting proposal moved to `realizing`; the card goes out | PRP-9 |
| `realizing` | `realized` | the core: the object of "Accept" or "Lift veto" is executed; a commit with the line `Botaru-Proposal: <id>` is applied; a commit is linked through P2 `proposal.link_commit` | the link is verified: the commit is applied, only the tenant's folder, the fields are at the accepted values or moved in the named direction | PRP-19, PRP-8, PRP-20 |
| `realizing` | `realization_failed` | the core | the object was not executed (`object_failed`) or `realize_by` expired (`timeout`) | PRP-18 |
| `realized` | `reverted` | the core: a commit with the line `Botaru-Revert: <id> <reason>` is applied | the commit returns the values of the reverse edit; the reason is from a closed list, `other` without it; `harm_observed` closes the trial with the outcome `worse` | PRP-20, PRP-22, PRP-23 |
| `sampled` | `sample_presented` | delivery of `proposal_sample_card` through a channel (E1) | a recipient with `proposal.decide:<category>`; the recipient's address and ceiling | DIG-5, DIG-6, DIG-2 |
| `sample_presented` | `sample_judged` | the "Would accept" button | the answer is written to the sample and the calibration before the scores are shown | DIG-6, COM-11 |
| `sample_presented` | `sample_closed` | the "Would not accept" button or the `decide_ttl` deadline | | DIG-6 |
| `sample_judged` | `realizing` | the "Accept" button (no veto) or "Lift veto" (there is a veto, the capability `proposal.override_veto:<category>`, after "Details") | the kind is not `none`; the rights; the target is not locked | DIG-6, PRP-4, PRP-9, PRP-18, PRP-21 |
| `sample_judged` | `noted` | the "Accept" or "Lift veto" button | the kind is `none` | DIG-6, PRP-4 |
| `sample_judged` | `sample_closed` | the "Reject" button or the `decide_ttl` deadline | | DIG-6 |

## Invariants

- Only the core writes the record fields, and the state changes only by this table (PRP-1, PRP-6).
- "Accept", "Reject", "Postpone", the answers of the blind card and "Lift veto" are done only by a `human` principal with strong identity and the right, from the card's channel, with the `nonce`, once; bots, `service` and `system` do not decide (APR-4, DIG-3, DIG-6). The core accepts "Lift veto" only after "Details" was opened (APR-14).
- No transition except "Accept" and "Lift veto" creates tasks above level 0, tool calls, approvals, script runs, patches and messages, except the delivery of the digest and the cards; "Accept" creates exactly one ordinary path object (PRP-4).
- Only the core sets `realized`, and only by a verified link; a value that is already in place or was set without a link does not count as execution (PRP-19).
- At most one proposal per field of one object in `realizing` or with a running trial; "Accept" is unavailable while the target is locked (PRP-9).
- Every proposal of a kind other than `none` that moved to `realizing` has exactly one trial record, created in the same transaction (PRP-21).
- The open states for the limits of a role and a bot: `screened`, `in_review`, `recommended`, `presented`, `parked` (INS-4, PRP-5, PRP-16); `parked(conflict)` because of an earlier accepted proposal of another author does not take the bot's slot (PRP-16).
- A proposal and everything it references belong to one tenant (PRP-8, TEN-7).
- A proposal with a lifted veto and a later related proposal give no credit to the author, although they go through the same states (DIG-6, PRP-17); relatedness is counted only among proposals of levels 0 and 1, proposals of the farm manager are not included in `related_to` (PRP-17).
- A trigger for promotion is given only by a proposal in `realized` with a trial outcome from the PRP-22 list; `reverted` and `realization_failed` give no trigger (GOV-14). A disputed attribution of an incident to the change (`disputed`, GOV-13) gives no trigger while the dispute is open (D-79).

## Versions

| Version | What appears |
|---|---|
| v0.1 | In the order of work after the core and the first bot (D-71, D-80). All states except the sample branch; the `parked` reasons: `score`, `veto_lens_missing`, `review_budget`, `evidence_drift`, `conflict` and the conflict transitions (PRP-9); the change kinds `none`, `action_request`, `manifest_change`, `human_change`, `script`; two lenses for the kind `none` (COM-14); lens executors on seats and the fallback role (COM-7); the revert reason in the `Botaru-Revert` line (PRP-20) |
| v0.2 | The states `sampled`, `sample_presented`, `sample_judged`, `sample_closed` and the "Lift veto" button (DIG-5, DIG-6); the `related_to` mark (PRP-17); one open proposal per bot (PRP-16) |
