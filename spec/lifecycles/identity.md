# Lifecycle: bot identity

## In short

A bot identity (`Bot`) is a core record with a permanent number `bot_<ulid>`, a tenant, a role and a binding (`binding`) to a worker or to a seat. A bot's work runs on workers, while the identity carries its number, the persona of the position, the memory of the `bot` scope and the track record. Only the core creates, binds and deletes an identity; bots have no operations that change its state (IDN-11, GOV-6). A retired identity does not come back: its memory is sealed for 30 days for analysis by people, then wiped, and a tombstone remains. A binding has the form `{mode: worker | seat, worker, seat, epoch}`; the binding epoch `bot_epoch` grows with every change of the binding and gets into every record an attempt writes (IDN-8).

## States

| State | Meaning | Terminal |
|---|---|---|
| `active` | The identity is bound to a worker (`mode: worker`) or to a seat (`mode: seat`) and gets the tasks of its role or its seat | no |
| `suspended` | There is a hold by people of the `bot` scope on the identity: no tasks, reviews or ballots; it is not included in the voters of a new case, and it drops out of a running case with the flag `voters_removed` | no |
| `unbound` | The identity's worker is revoked, there is no new binding. No tasks, the memory is intact. Happens only with a `mode: worker` binding | no |
| `retired` | The identity retired for a reason from a closed list. No tasks, reviews, ballots and facts from it are not accepted; facts, records, the track record and sessions are sealed; the fact offer to the successor is open | yes, for work: only `erased` follows |
| `erased` | The sealed data is wiped in all stores; only the tombstone remains: id, tenant, role, time, reason and the successor's id | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `active` | P2 `worker.enroll` without the `bot` parameter | a worker role with a model slot; the core creates an identity with a `mode: worker` binding and epoch 1 | IDN-8 |
| (none) | `active` | the core in the transaction of a promotion, of seating on a commission seat or of an identity's retirement | the previous identity has a live worker: it immediately gets a new identity of the same role, the work does not stop | IDN-10, IDN-11 |
| (none) | `active` | P2 `seat.assign` of the farm manager seat (`farmctl`, `admin:governance` in `farm`) | the core creates a new identity in `farm` with a `mode: seat` binding | IDN-10, MGR-1 |
| `active` | `active`, a new binding `mode: seat`, epoch + 1 | a promotion case becomes `effective`, or P2 `seat.assign` of a commission seat | the identity of a functionary of its tenant moves to the seat; its worker gets a new identity in the same transaction | IDN-10, GOV-9 |
| `active` | `suspended` | P2 `hold.set` of the `bot` scope | the hold is set by a person with the right to this scope | HLD-1, IDN-9 |
| `suspended` | `active` | P2 `hold.release` or the background loop `hold` by `until` | the last hold by a person of the `bot` scope is released or expired, there is a binding | HLD-2, HLD-1 |
| `active` | `unbound` | P2 `worker.revoke` | a `mode: worker` binding, there is no new binding | IDN-8 |
| `suspended` | `unbound` | P2 `worker.revoke` | same; the hold on the identity stays | IDN-8 |
| `unbound` | `active` | P2 `worker.enroll` with the `bot` parameter (`admin:workers`) | the same tenant and role; there is no hold of the `bot` scope; binding epoch + 1 | IDN-8 |
| `unbound` | `suspended` | P2 `worker.enroll` with the `bot` parameter | the identity has a hold of the `bot` scope; binding epoch + 1 | IDN-8 |
| `active`, `suspended`, `unbound` | `retired` | the core for a reason from `bot-retire-reasons.yaml`: a case becomes `effective` (`removed_by_vote`); P2 `seat.assign` or `seat.vacate` (`replaced_by_human`, for the farm manager seat with the reason `incident` `replaced_after_incident`); the `identity` loop by the `decide_within` deadline (`replaced_after_incident`) or `unbound_ttl` (`worker_gone`); `apply` without the role (`role_removed`); P2 `bot.retire` (`human_retire`); from v0.2 a tenant offboarding (`tenant_offboarded`) | a reason from the closed list; in the same transaction the seal (MEM-4), the fact offer (MEM-7) and a new identity for the live worker | IDN-11 |
| `retired` | `erased` | at the end of `seal_ttl` (30 days) the `identity` loop enqueues an erasure request with the reason `bot_retired` and the scope of the identity id, and the `erasure` loop executes it in all stores | the request is executed; from v0.2 records whose retention class period ended earlier are already wiped by the `retention` loop (RET-10, MEM-10); on tenant deletion everything is wiped by its `tenant_delete` request | MEM-9, IDN-11, RET-7, MEM-10 |

## Invariants

- Only the core issues an identity id, and does not reuse it, even after `erased` (IDN-8, RET-3).
- The identity's tenant does not change; a reference to an identity of another tenant is only an id and an epoch (TEN-1).
- One identity holds at most one worker, and a worker has one identity at any moment. An identity with a `mode: seat` binding has no worker of its own: the workers of the seat's role execute its tasks (IDN-12).
- Every change of the binding increments `bot_epoch`; an attempt's write with an outdated epoch gets `fenced`.
- P2 `worker.rotate` changes only the worker's token: the identity and the epoch stay.
- The identity, the position and the track record grant no rights (IDN-9).
- Only a hold by a person of the `bot` scope moves an identity to `suspended`. A hold of the farm manager seat holder on the same bot (no longer than 2 h, MGR-9) delays its tasks, but does not change the identity's state, its eligibility for promotion or its vote.
- No bot has an operation that creates, binds, stops or deletes an identity; only the core sets the retirement reason (IDN-11).
- `retired` does not return to `active`; a bot that is needed again gets a new identity with a new id.
- The tombstone has no content: only id, tenant, role, time, reason and the successor's id.
- The track record (`record`) contains only numbers and core codes: the creation date, the number of proposals by outcome, for seat holders the number of reviews and the calibration, the seat assignments. The model neither writes nor sees it.

## Versions

| Version | What appears |
|---|---|
| v0.1 | The whole cycle: `active`, `suspended`, `unbound`, `retired`, `erased`; seven retirement reasons; the seal and the tombstone; moving to a commission seat by P2 `seat.assign`; the fields `acting_bot` and `bot_epoch` in the record schemas (empty for records without a bot) |
| v0.2 | The transition `worker -> seat` by a promotion case becoming `effective`; retirement with the reason `tenant_offboarded` on a tenant offboarding; wiping by the retention class period by the `retention` loop, before the seal ends (RET-10, MEM-10) |
| v0.3 | A commission seat holder that answers for a confirmed link goes back to the functionaries as a parked bot (`parked` in the bot record) waiting for a worker of its former role; the retirement reason `parked_expired` after 180 days without a worker; `replaced_after_incident` is not applied (GOV-42, IDN-15) |
