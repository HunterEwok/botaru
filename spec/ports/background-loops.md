# Background loops of the core

How the core is built, not a contract (C10, D-114).

## In short

Background loops are duties of the core that run by time, not by call: end expired leases, enqueue a task by schedule, send out deliveries, delete content when its period ends. They are parts of the core, not ports or services. The hot path (`claim`, a tool call) goes through the store and long polling, not through the loops. No loop calls the model: everything they decide, code decides, so no core decision depends on untrusted text.

## What every loop does

- It is deterministic and works towards the desired state from the store records, without relying on missed events.
- It is idempotent and safe with two running core copies (a lock in the store), and publishes an event for each of its transitions.
- It fails on its own: the failure of one loop does not stop the others.
- It marks its tick in the store. The tick age is visible in P2 `doctor` (OBS-4), and P2 `health` answers "alive" only if the store answers and every loop marked its tick in time, so a loop that failed silently is visible from outside (MON-2).
- It changes nothing while the store is unavailable and waits until the core leaves `not_ready`.

## Loops

| Loop | What it does | Rules |
|---|---|---|
| `reaper` | Ends expired leases: an attempt gets `lease_expired`, a read call `error(lease_expired)`, a write call `exec_unknown`, a call nobody took within `start_by` `error(no_executor)`; a lease that expired while the core was down ends with `core_unavailable` without spending the attempt; from v0.2 a lease of a draining worker ends with `worker_drain` after `drain_deadline` | FEN-1, FEN-3, FEN-5, APR-7, CNT-4, LCY-4 |
| `deadline` | `start_by` gives an event, `deadline` fails the task; an attempt without progress past `timeouts.stall_after` ends with `stalled` and the alarm `alarm.task.stalled` | DLV-1, CNT-4 |
| `schedule` | Enqueues a task for each schedule slot with the slot key (a UTC instant; overlap skips; catch-up of one period) and checks `deliver_to` on each run | IDM-2, PRM-10 |
| `delivery` | Creates deliveries with `target`, checks the ceiling and chat membership, retries until `dead`; `monitor_alarm` and `monitor_audit_head` deliveries retry while the episode is open, and a stalled export gives `monitor_export_stalled`; from v0.2 also the per-person day limit, quiet hours and the notice digest | DLV-3, DLV-6, DLV-7, MON-11, APR-15, DLV-10 |
| `approval-expiry` | Moves overdue human decisions to their on-silence outcome from the decision registry, sends reminders (APR-19) and updates cards; from v0.2 removes a disputed attribution after 7 days of silence and computes the monthly "approved without reading" summary | APR-3, APR-4, APR-12, APR-19, GOV-13, APR-13 |
| `budget` | Limit windows, threshold events at 80% and 100% once per period, release of the reserves of terminal tasks and of unreconciled estimates, the alarm about an overdue reconciliation | BUD-1, BUD-4, BUD-10 |
| `membership` | Membership periods; disabling a person cancels their pending approvals | APR-6 |
| `worker-liveness` | The `worker.stale` event and an alarm by `last_seen`; the self-report is for reference | OBS-4, STA-2 |
| `erasure` | Sends erasure requests to every store (C1 `file.purge` and `task.purge`, C3 `delete` and `purge`, C4 `erase`, and to edge stores as `erasure` work units through P1 `claim`) and collects `done`, `failed` or `not_applicable`; a store without an erase operation answers `failed`, and the request stays open with an alarm | RET-7, RET-4 |
| `hold` | Hold deadlines and effects on pending approvals and deliveries; a hold ends by `until` or by a person, never early by the loop; auto-stops; from v0.2 holds from monitoring maintenance windows and the schedule auto-stop after `K` failed slots | HLD-2, HLD-3, HLD-1, MON-9, CAP-4 |
| `alarm` | Alarm episodes deduplicated by `alarmkey`, transitions `firing` and `resolved` with a `seq`; each change gives one `monitor_alarm` delivery for E5, and open episodes are exported again after the connection returns; repeats, acknowledgement and escalation live in the monitoring system | OBS-6, MON-1 |
| `audit-head` | At least daily puts the signed head of the audit chain of each tenant and of `farm` (`spec/hashing.md`) as a `monitor_audit_head` delivery | AUD-6, MON-3 |
| `inbox` | Inbox deadlines, `dead(delivery_exhausted)`, the `inbox.flood` event, waking roles by an `on: inbox` route at most once per window | MSG-9, MSG-15, MSG-11, MSG-12, MSG-16 |
| `observe` | Rollups of observation series by window and a function from a closed list | OBV-4 |
| `proposal` | Selection of proposals by code, lens reviews, verdict by code, the digest to people, the change trial on "Accept"; from v0.2 the monthly seed, the shadow reviewer, the audit sample and the trial figures | PRP-2, COM-4, DIG-1, COM-10, DIG-5, DIG-8 |
| `health` | Three core health indicators and their alarms, part of the P2 `health` answer | OBS-9, MON-2 |
| `campaign` | Release of campaign items by the rollout policy, failure thresholds, `bake` and `deadline`, pause on `no_executor` and on a hold; from v0.2 pace and waves | ORC-1, ORC-3, ORC-4, ORC-8, ORC-11 |
| `script` | Release of script steps by order and conditions, the run deadline and the decision deadlines | SCR-8, SCR-11 |
| `incident` | Opens incidents by alarm and class, sets the first measure by code, marks `resolved` by the alarm state; an incident of a stopping class stops the farm manager seat | MGR-7, MGR-8, GOV-22 |
| `control` | Deadlines of control operations (manager holds live at most 2 h), limits and the safeguard with `alarm.manager.breaker`; tenants with `farm_manager.enabled: false` are left out (D-77) | MGR-9, MGR-14 |
| `identity` | Seat and identity deadlines: a stopped manager seat becomes `vacant` after `decide_within`, an identity `unbound` past `unbound_ttl` leaves with `worker_gone`, fact offers close, at the end of `seal_ttl` the sealed data gets a `bot_retired` erasure request, and `alarm.manager.absent` | IDN-11, GOV-22, MEM-7, MEM-9, MGR-2 |
| `retention` | Deletes content by retention class without an erasure request: in v0.1 `content` and `observation` past the default period (RET-2), from v0.2 every class by the tenant periods in every copy (RET-9, RET-10), files after `expires_at` with corpus fragments, sealed data whose period ends first (MEM-10), and the principal in usage lines after 93 days; tombstones remain | RET-2, RET-3, RET-5, RET-9, RET-10, MEM-10, KNW-5, BUD-16, SEC-14 |
| `grant` | Model grant deadlines; every 10 minutes one `manager.grants.v1` batch task per tenant with a pending request | MGR-18 |
| `governance` | Promotion trigger and queue, attribution candidates by `attribution-rules.yaml` and their weekly reminders, cases with the list of seats; from v0.3 the proposed leaver, the entry pause after `wrong_candidate` and the monthly `commission_review` card | GOV-12, GOV-13, GOV-14, GOV-30, GOV-31, GOV-35, GOV-43 |
| `expectation` | Timeouts of waiting for external events declared in the task type (v0.2) | CNT-6 |

The export window of an offboarding tenant (LCY-1) needs no loop: P2 `tenant.delete` compares the store clock with the end of the window at the call.
