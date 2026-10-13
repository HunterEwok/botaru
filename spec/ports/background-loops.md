# Background loops of the core

Sheet version: 0.1.0.

## In short

Background loops are duties of the core that run by time, not by call: clear expired leases, enqueue a task by schedule, send out deliveries, delete content when its period ends. They are not ports and not services, but parts of the core with one shared contract. The hot path (`claim`, a tool call) goes through the C1 store and long polling, not through the loops. No loop calls the model; everything they decide, code decides.

## Shared contract

Each loop:

- is deterministic and never calls the model;
- works towards the desired state from the store records and does not rely on missed events;
- is idempotent and safe with two running core copies (a lock in the store);
- publishes an event for each of its transitions;
- fails on its own: the failure of one loop does not stop the others;
- marks its tick in the store. The tick age is visible in P2 `status` and `doctor` (OBS-4), and P2 `health` answers "alive" to the monitoring system only if the store answers and all loops marked their tick in time (MON-2);
- changes nothing while the store is unavailable and waits until the core leaves `not_ready`.

## Loops

| Loop | What it does | Version | Rules |
|---|---|---|---|
| `reaper` | Expired leases: an attempt gets `lease_expired`, a read call `error(lease_expired)`, a write call `exec_unknown`; a tool call that nobody took within `start_by` (30 seconds) gets `error(no_executor)`. A lease that expired while the core was not running ends with `core_unavailable` and does not spend the attempt | v0.1 | FEN-1, FEN-3, FEN-5, APR-7, CNT-4 |
| `deadline` | `start_by` gives an event, `deadline` moves the task to `failed`; an attempt without progress for longer than `timeouts.stall_after` ends with `stalled` and the alarm `alarm.task.stalled` | v0.1 | DLV-1, CNT-4 |
| `schedule` | Enqueues a task for each schedule slot with the slot key; a slot is a UTC instant, the time zone is set in the schedule; overlap defaults to "skip", the catch-up window is one period; checks `deliver_to` on each run | v0.1 | IDM-2, PRM-10 |
| `delivery` | Creates deliveries with `target`, checks the ceiling and the chat membership, retries, ends with `dead`; `monitor_alarm` and `monitor_audit_head` deliveries do not die by number of attempts, but are retried with a pause of up to 10 minutes while the episode is open or the head is not uploaded; an export that has failed for longer than 15 minutes gives the code message `monitor_export_stalled` to the tenant admins and the operator no more than once in 6 hours | v0.1 | DLV-3, DLV-6, DLV-7, MON-11 |
| `approval-expiry` | The deadlines of all subjects of human decisions: moves overdue pending ones to the on-silence outcome from the decision registry row, sends reminders and repeats by that row (an action approval has one `card_update` reminder at half the period, APR-19) and updates the cards; from v0.2 removes a disputed attribution of an incident when the author of "Yes" stays silent for 7 days (D-79) | v0.1 | APR-3, APR-4, APR-12, APR-19, GOV-13 |
| `budget` | Limit windows, threshold events at 80% and 100% (one per period), release of the reserves of terminal tasks | v0.1 | BUD-1, BUD-4 |
| `provider_check` | Once a day compares the ceilings of each workspace with the tenant limits and makes a probe for each pair (workspace, model profile); a mismatch gives an alarm and a line in `doctor` | v0.1 | GW-9 |
| `membership` | Membership periods; when a person is disabled, cancels their pending approvals | v0.1 | APR-6 |
| `worker-liveness` | Raises the `worker.stale` event and an alarm by `last_seen`; the worker's self-report is for reference | v0.1 | OBS-4, STA-2 |
| `erasure` | Sends erasure requests to the stores through the erase operations of their ports (C1 `file.purge` and `task.purge`, C3 `delete` and `purge`, C4 `erase`, E1 `erase`, E2 `erase`, E4 `purge`, E5 `erase`) and collects the result of each (`done`, `failed`, `not_applicable`); a store without an erase operation answers `failed`, the request stays open and gives an alarm by its deadline. The core pushes work to nobody, so for edge stores (E1, E2, E4, E5) the loop creates `erasure` work units (`erase` or `match`, schema `erasure-work`) that they take with P1 `claim` and hand in with `complete` with the `erasure` field | v0.1 | RET-7, RET-4 |
| `hold` | Hold deadlines: a hold ends by `until` (`hold.expired`) or by removal by a person, the loop itself does not remove a hold early; the effects of holds on pending approvals and deliveries; auto-stops; from v0.2 holds by maintenance windows of the monitoring system (author `system:monitor`, deadline at the end of the window, effect `pause_action`: pending approvals are paused, their deadline is extended by the window length; `no_action` only for hosts with `maintenance_hold: true`) | v0.1 | HLD-2, HLD-3, HLD-5, MON-9 |
| `alarm` | Alarm episodes: deduplication by the `alarmkey` key, episode transitions `firing` and `resolved` with a `seq` number; a repeated `firing` of an open episode increases `repeat_count` and creates no deliveries; each change of an episode gives one `monitor_alarm` delivery for the E5 monitoring adapter of its tenant (alarms about the farm itself for the `farm` adapter); after the connection to the monitoring system is restored, all open episodes are exported again (MON-11). The loop delivers nothing to people: repeats, acknowledgement, escalation and silencing live in the monitoring system | v0.1 | OBS-6, MON-1 |
| `audit-head` | At least once a day puts the head of the audit chain of each tenant and of `farm` (`epoch`, `seq`, `hash`, the signature `sig` by the core key over the object with `epoch` and `key_id`, `spec/hashing.md`) as a `monitor_audit_head` delivery for E5 and to the addresses the tenant named | v0.1 | AUD-6, MON-3 |
| `inbox` | Inbox entry deadlines, the outcome `dead(delivery_exhausted)`, the `inbox.flood` event, waking roles by an `on: inbox` route no more than once per window | v0.1 | MSG-9, MSG-10, MSG-11, MSG-12, MSG-16 |
| `observe` | Rollups of observation series: window, function from a closed list | v0.1 | OBV-4 |
| `proposal` | Selection of proposals by code, lens reviews, verdict by code, digest to people, recording a change trial on "Accept" | v0.1 | PRP-2, COM-4, DIG-1 |
| `health` | Three core health indicators and alarms on them; the values are part of the P2 `health` answer | v0.1 | OBS-9, MON-2 |
| `campaign` | Release of campaign items by the rollout policy, failure thresholds, the `bake` and `deadline` timers, pause on `no_executor` and on a hold, continuation; from v0.2 pace and waves | v0.1 | ORC-1, ORC-3, ORC-4, ORC-8, ORC-11 |
| `script` | Release of script steps without predecessors by order and conditions, the run deadline, the deadlines of decisions on the run and on `confirm` steps | v0.1 | SCR-8, SCR-11 |
| `incident` | Opens incidents by alarm and class, sets the first measure by code, marks `resolved` by the alarm state; a new incident of a stopping class stops the farm manager seat | v0.1 | MGR-7, MGR-8, GOV-22 |
| `control` | Deadlines of control operations (manager holds live no longer than 2 h), limits and the safeguard (its triggering gives the alarm `alarm.manager.breaker` with severity `page`); the money pause of the seat by tenant (`paused_in`) until the start of the next daily window of the `manager` line; tenants with `farm_manager.enabled: false` are not part of the review and reading (D-77) | v0.1 | MGR-9, MGR-14 |
| `identity` | Deadlines of identities and seats: after `decide_within` (7 days) a stopped farm manager seat becomes `vacant`, and the bot leaves with the trigger `replaced_after_incident`; an identity in `unbound` for longer than `unbound_ttl` (30 days) leaves with the trigger `worker_gone`; fact offers close by `open_until`; at the end of `seal_ttl` what is sealed gets an erasure request with the reason `bot_retired`; the alarm `alarm.manager.absent` if a seat task waits for a worker longer than 5 minutes | v0.1 | IDN-11, GOV-22, MEM-7, MEM-9, MGR-2 |
| `retention` | Deletes content by the tenant's retention class periods without an erasure request: in v0.1 only the classes `content` and `observation` older than the default period (RET-2), from v0.2 all classes, including files after `expires_at` with the fragments and embeddings of corpora; tombstones remain | v0.1 | RET-2, RET-3, RET-5, KNW-5 |
| `grant` | Model grant deadlines | v0.2 | MGR-18 |
| `governance` | Promotion trigger and queue, attribution candidates linking an incident to a change by `attribution-rules.yaml` and weekly reminders about them, opening cases, rounds, the `stuck` flag, the reopened case after `wrong_leaver` and the entry pause after `wrong_candidate`, the monthly `commission_review` card | v0.2 | GOV-12, GOV-13, GOV-14, GOV-23, GOV-30, GOV-31, GOV-35 |
| `expectation` | Timeouts of waiting for external events declared in the task type | v0.2 | CNT-6 |

## Why this way

- A description with one contract is simpler than a "controller framework": two dozen loops keep the same promises.
- A loop that called the model would make core decisions depend on untrusted text; so all loop logic is code.
- The tick of each loop is part of the P2 `health` answer, which the monitoring system polls from outside the core machine, so a loop that failed silently is visible from outside even if the core answers (MON-2).
