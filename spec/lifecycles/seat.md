# Lifecycle: seat

## In short

A seat (`Seat`) is a chair in a tenant's commission or the farm manager's chair; the seat holder is the bot identity on it. Commission seats are declared by the tenant charter `tenants/<t>/governance.yaml`, the farm manager seat by the farm charter `farm/governance.yaml` (GOV-33). Only the core writes the holder, in one transaction that increments the seat epoch by 1 (IDN-10). A commission seat has the states `vacant`, `held` and the terminal `removed`; the farm manager seat has `vacant`, `held` and `stopped` with a set of stop reasons (GOV-22). A task on behalf of a seat carries the seat epoch, and with an outdated epoch its write gets `fenced`.

## States

Commission seats:

| State | Meaning | Terminal |
|---|---|---|
| `vacant` | There is no holder, seat tasks are not handed out. The lens is executed by the fallback role `fallback_role` if the charter named one (COM-7); otherwise a lens without a veto stays silent, and a lens with a veto keeps proposals in `parked(veto_lens_missing)` (COM-4) | no |
| `held` | There is a holder. Seat tasks are handed out on the current epoch | no |
| `removed` | The commission seat was removed from the charter | yes |

Farm manager seat:

| State | Meaning | Terminal |
|---|---|---|
| `vacant` | There is no holder, seat tasks are not handed out; the farm works without a farm manager (MGR-2) | no |
| `held` | There is a holder, the set of stop reasons is empty. Seat tasks are handed out on the current epoch | no |
| `stopped` | The set of reasons contains `human` (a hold of the `seat` scope), `no_control` (the safeguard) or `incident` (a stopping incident). Seat tasks are not handed out in any tenant | no |

A commission seat whose holder is under a hold by people of the `bot` scope stays `held`, but its tasks are not handed out: for that time the fallback role executes the lens, as for a vacant seat.

## Transitions

Commission seats:

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `vacant` | `apply` of the tenant charter with a new seat | an id, a lens from `lenses.yaml`, a fallback role optionally | GOV-33 |
| `vacant` | `held` | P2 `seat.assign` (`farmctl`, `admin:governance` in the seat's tenant) | the identity of a functionary of this tenant (a role of the profile `worker`, `manager`, `reporter` or `aggregator`, the worker is enrolled without an owner) in `active`; its worker gets a new identity, its queue entry is removed | IDN-10, GOV-14 |
| `vacant` | `held` | the "Confirm" button on a case with `mode: vacancy` | the case is `effective`: the queue head takes the seat without a vote | GOV-9, GOV-14 |
| `held` | `held`, a new holder, epoch + 1 | the "Confirm" button on a case with `mode: removal` | the case is `effective`: the leaver is `retired(removed_by_vote)`, the candidate is on the seat | GOV-9, IDN-11 |
| `held` | `held`, a new holder, epoch + 1 | P2 `seat.assign` on an occupied seat | the previous holder is `retired(replaced_by_human)` | IDN-10, IDN-11 |
| `held` | `vacant` | P2 `seat.vacate` | the previous holder is `retired(replaced_by_human)` | IDN-10, IDN-11 |
| `held` | `vacant` | the core when the holder retires for another reason (`human_retire`, `role_removed`, `tenant_offboarded`) | the holder's identity is `retired` | IDN-11 |
| `vacant` | `removed` | `apply` of the charter without this seat | the seat has no holder; `apply` does not remove a seat with a holder | GOV-33 |

Farm manager seat:

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `vacant` | the farm charter | the initial state | MGR-2 |
| `vacant` | `held` | P2 `seat.assign` (`farmctl`, `admin:governance` in `farm`) | the core creates a new identity in `farm` without `origin`, epoch + 1, the set of reasons is empty, a notification to the tenant admins (MGR-31); from v0.2 the MGR-23 check before the first real work | IDN-10, MGR-1, MGR-23, MGR-31 |
| `held`, `stopped` | `held`, a new holder, epoch + 1 | P2 `seat.assign` | the previous holder leaves: `replaced_by_human`, and if the set had the reason `incident`, then `replaced_after_incident`; its pending cards become `canceled(seat_changed)`, its holds live at most 15 minutes, its facts for every tenant are offered to the new holder; the set of reasons is cleared; the MGR-31 notification | IDN-10, IDN-11, MGR-1, MEM-7, MGR-31 |
| `held`, `stopped` | `vacant`, epoch + 1 | P2 `seat.vacate` | the previous holder leaves with the same reason; its running attempts are canceled, the facts wait for the next holder until the end of the seal; the set of reasons is cleared; the MGR-31 notification | IDN-10, IDN-11, MGR-31 |
| `held` | `stopped` | P2 `hold.set` of the `seat` scope (`admin:farm`) | the reason `human` is added to the set; epoch + 1, all running attempts of the holder in all tenants are canceled, `claim` of seat tasks is forbidden | MGR-15 |
| `held` | `stopped` | the core safeguard (the `control` loop) | the reason `no_control` is added to the set; epoch + 1, the holder's running attempts in all tenants are canceled | MGR-14 |
| `held` | `stopped` | the core's `incident` loop | a new incident of a class with `stops_manager: true` in any tenant, opened while the seat is in `held`; for a disk also the conditions of MGR-33 and MGR-34; the reason `incident` is added to the set; epoch + 1, the holder's running attempts in all tenants are canceled | GOV-22, GOV-28 |
| `stopped` | `stopped`, epoch + 1 | P2 `hold.set` of the `seat` scope or the core safeguard | a reason `human` or `no_control` that is new for the set is added; other reasons stay | MGR-1, MGR-14, MGR-15 |
| `stopped` | `stopped` | the core's `incident` loop | an incident of a class with `stops_manager: true` opened during the stop adds no reason and does not change the epoch: it is only appended to the `manager_stopped` card and gives no stop after the return, because it was opened before the return | GOV-22 |
| `stopped` (only `no_control`) | `stopped`, the reason `incident`, epoch + 1 | the core's `incident` loop | the safeguard fired inside the `prevent_window` of an incident for which the holder is responsible (MGR-34): at the start of the window the seat was in `held` with this holder; the reason `incident` is added to the stopped seat, the exits are the same | GOV-22, MGR-34 |
| `stopped` | `held`, epoch + 1 | P2 `hold.release` of the `seat` scope (`admin:farm`) or the background loop `hold` by `until` | the hold of the `seat` scope is released or expired: its reasons `human` or `no_control` go away; the seat is `held` only if the set is empty | MGR-15, HLD-2, HLD-5 |
| `stopped` | `held`, epoch + 1 | P2 `seat.resume` (`farmctl`, `admin:governance` in `farm`) | the incident that caused the stop and the incidents of the same minute were dismissed by people as false; removes only the reason `incident`; the seat is `held` only if the set is empty; the MGR-31 notification | GOV-22, MGR-31 |
| `stopped` | `vacant` | the core's `identity` loop | the set has `incident`, and there has been no decision for longer than `decide_within` (7 days by default and at most); the previous holder is `retired(replaced_after_incident)`, epoch + 1, the facts wait for the next holder until the end of the seal; the MGR-31 notification | GOV-22, IDN-11, MGR-31 |

## Invariants

- A seat has at most one holder. The seat epoch grows with every change of the holder, a vacating, a new stop reason and a return to `held`.
- A seat task carries the seat epoch; a review, a ballot, a fact choice or a control operation with an outdated epoch gets `fenced` without side effects (IDN-10).
- With every change of the seat epoch the running attempts of seat tasks get `cancel_requested` and end with the reason `seat_changed` without spending the attempt; the task waits for a holder with the current epoch (IDN-10).
- Holds set by the holder of the farm manager seat live at most 15 minutes after the holder is replaced (MGR-1), and on a seat stop by an incident they live out their deadline, at most 2 h (GOV-22); a stopped holder sets no new holds.
- A pause by tenant from the money safeguard (MGR-14, `paused_in`) is not a seat state: the epoch does not change, the seat stays `held`, seat tasks are not handed out only in that tenant.
- The set of stop reasons of the farm manager seat is computed from the active holds of the `seat` scope (`human` and `no_control`, HLD-1) and the reason `incident` in the seat record. The farm manager seat is in `held` only when the set is empty. P2 `seat.assign` and `seat.vacate` work from any state and clear the set.
- The `seat.*` operations and a hold of the `seat` scope go only through `farmctl`, never from a channel (PRM-14); the `manager_stopped` card shows ready commands, not buttons (MGR-35).
- The new holder of the farm manager seat is always a new identity in `farm`; a bot from a commission does not move to this seat. The holder of a commission seat is always a former functionary of its tenant.
- No bot changes the holder, the seat state or the set of reasons (GOV-6); the state of the farm manager seat and the `stops_manager` field get into no bot context (GOV-29).
- `farm` holds only the registry of the farm manager seat and the holder's identity as numbers and codes; commission seats live in their own tenant (GOV-16).
- The seat's model profile is set by the charter (`model_profile`); a model of the seat's rank is given only to an attempt on behalf of the seat with the current epoch (GOV-3).

## Versions

| Version | What appears |
|---|---|
| v0.1 | The seat registry and the epoch; commission seats in the charter, P2 `seat.assign` and `seat.vacate`; the farm manager seat with `vacant`, `held`, `stopped`, the set of reasons and P2 `seat.resume`; seats appear after the core and the first bot together with the improvement cycle (D-71) |
| v0.2 | A change of the commission seat holder by a promotion case and the queue head taking a vacant seat |
| v0.3 | The farm manager seat no longer goes to `stopped` for `incident`: a preventable incident writes `restricted_in` on the seat record (restricted mode in one tenant, lifted by P2 `seat.resume` with `--tenant`), and `decide_within` no longer retires the holder (GOV-47, MGR-47); a commission seat becomes `vacant` with epoch + 1 in the transaction of a person's "Yes" on a link the holder answers for, and the holder is parked (GOV-42, IDN-16) |
