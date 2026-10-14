# Lifecycle: change trial

Kind: `trial` (the kinds are listed in [the lifecycle index](README.md)).

## In short

A change trial (`trial`) is a core record of whether the indicator that the bot promised in a proposal improved. The core creates it on "Accept" or "Lift veto" of any change kind except `none`, and immediately records the values of the metric and of one to five guards before the change (a guard is a series that must not get worse; the core picks them by the target kind and by the metric, the tenant can only add more). The 14-day window runs from `realized`; the core checks the guards once a day, and only code computes the outcome at the end of the window. The outcome `worse` is a deterioration confirmed by the core, which gives no trigger for promotion in the same way as an incident (D-37). Do not confuse the change trial with the promotion probation (`probation`): that is another period, and it is a condition of the promotion trigger (GOV-14). The record lives in the `trial` field of the proposal, see [proposal.md](proposal.md) and [improvements.md](../../docs/improvements.md).

## States

| State | Meaning | Terminal |
|---|---|---|
| `baseline` | the record is created, the values before are recorded, the change is not executed yet | no |
| `running` | the proposal is `realized`, the 14-day window runs, the guards are checked once a day | no |
| `guard_broken` | a guard went beyond its tolerance in the bad direction; the executor got a "Revert" card; the window continues | no |
| `better` | better: an improvement beyond the series tolerance in all computations: over all points, without the points of the author bot and without the points of its role | yes |
| `unchanged` | no change: neither an improvement in all computations nor a deterioration beyond the tolerance | yes |
| `worse` | worse: a guard fired, a deterioration beyond the tolerance in at least one computation, or a revert with the reason `harm_observed` | yes |
| `undetermined` | undetermined: fewer than `min_samples` before or after; if only the author or its role produce the points, the outcome is computed by the remaining computations with the mark `self_measured` | yes |
| `mixed` | mixed: another change was executed on the same subjects during the window | yes |
| `canceled` | the change was not executed, or was reverted without a fired guard and with a reason other than `harm_observed`; gives no trigger for promotion | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `baseline` | the "Accept" or "Lift veto" button | the change kind is not `none`; the metric and the guards are from series with a full `outcome` block; the guards by the target kind, by the metric and from `insights.extra_guards`; the values before over the 14 days before the decision | PRP-21, OBV-6 |
| `baseline` | `running` | the core sets the proposal to `realized` | the link is verified | PRP-19, PRP-21 |
| `baseline` | `canceled` | the core sets the proposal to `realization_failed` | the object was not executed or `realize_by` expired | PRP-18 |
| `running` | `guard_broken` | the core loop `proposal`, the daily check | a guard went beyond its tolerance in the bad direction | PRP-23 |
| `running` | `mixed` | the `proposal` loop, the end of the window | another proposal was executed on the same subjects, or another commit changed the fields of the same objects | PRP-22 |
| `running` | `undetermined` | the `proposal` loop, the end of the window | fewer than `min_samples` before or after in a computation that is not empty because of the points of the author or its role; an empty computation gives the mark `self_measured`, not `undetermined` | PRP-22 |
| `running` | `worse` | the `proposal` loop, the end of the window; or the core sets the proposal to `reverted` with the reason `harm_observed` | a deterioration beyond the tolerance in at least one of the three computations; or a revert with `harm_observed` | PRP-22, PRP-23, PRP-20 |
| `running` | `better` | the `proposal` loop, the end of the window | an improvement beyond the tolerance in all computations | PRP-22 |
| `running` | `unchanged` | the `proposal` loop, the end of the window | the remaining cases | PRP-22 |
| `running` | `canceled` | the core sets the proposal to `reverted` | no guard fired during the window, the revert reason is `superseded` or `other` | PRP-20 |
| `guard_broken` | `worse` | the `proposal` loop, the end of the window; or the core sets the proposal to `reverted` | | PRP-22, PRP-23 |

The order of checks at the end of the window is fixed: a fired guard, then another change of the same subjects, then the three computations with the tolerance (over all points, without the points of the author bot, without the points of its role) and `min_samples` in each.

## Invariants

- Only the core writes the trial record; the model writes neither the metric, nor the values, nor the outcome (PRP-21, PRP-22).
- A proposal has at most one trial, and it is created in the same transaction as "Accept" or "Lift veto".
- The values before and the set of guards do not change after the record is created.
- Only a series with a full `outcome` block can be a metric or a guard (OBV-6).
- The window is always 14 days from `realized` and is not extended. The promotion trigger waits for people's answers about the link of incidents to the change (GOV-30), while the trial window and its outcome are not shifted by this.
- The farm itself does not revert the change and does not write to git: the executor reverts by a commit with the reverse edit (PRP-20, PRP-23).
- The outcome `worse` gives no trigger for promotion, in the same way as a confirmed link to an incident; the outcome `unchanged` does not take the trigger away (D-37, GOV-32). The outcome `canceled` and the proposal state `reverted` give no trigger; an outcome with the mark `self_measured` gives a trigger only after a person's "Count it" button on the `self_measured_card` card (GOV-14). A disputed attribution of an incident to the change (`disputed`, GOV-13) does not change the outcome and only delays the trigger while the dispute is open (D-79).
- While a trial runs, proposals about the same fields of the same objects wait in `parked(conflict)` (PRP-9).

## Versions

| Version | What appears |
|---|---|
| v0.1 | In the order of work after the core and the first bot (D-71, D-80). The record with the values before on "Accept" (PRP-21); the states `baseline`, `running`, `canceled`, `undetermined`; the daily guard check, `guard_broken` and the "Revert" card (PRP-23); the outcomes `better`, `unchanged`, `worse`, `mixed`, `undetermined` by the three code computations, the mark `self_measured`, `worse` on a revert with `harm_observed` (PRP-22, PRP-20) |
| v0.2 | The trial numbers in the author's "My outcomes" no later than a day after the end of the window (DIG-8) |
