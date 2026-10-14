# Lifecycle: alarm

Kind: `record` (the kinds are listed in [the lifecycle index](README.md)).

## In short

An alarm is not an object from git but a core record of one trouble episode with the key `alarmkey` (the alarm name and the subject id from the catalog, for example `disk.free.low:db1`). The episode is opened by an `alarm.<name>.vN` event with `state: firing` produced by code: the core or a worker of type `job` (INJ-7). The episode is closed again by code, with an event with `state: resolved` (OBS-6). The episode has a change sequence number `seq` (1 on opening, 2 on closing) and a repeat counter `repeat_count`. The background loop `alarm` maintains the episodes and puts every change of an episode into a delivery for the monitoring adapter E5, and the adapter passes it to the organization's monitoring system (MON-1). This delivery does not die after a number of attempts: it is retried while the episode is open or its closing has not been exported, it goes in `seq` order, and after a connection failure the core re-exports all open episodes (MON-11). Only the monitoring system delivers an alarm to people: notification, "Acknowledged", escalation, on-call rotations and silencing live there (D-62). A farm alarm has no recipients, no acknowledgement and no silencing.

## States

| State | Meaning | Terminal |
|---|---|---|
| `firing` | the trouble exists; the episode is open, the opening delivery for E5 is created and retried until exported; repeats with the same key do not open a new episode but increase `repeat_count` | no |
| `resolved` | code reported that the trouble is gone; the episode is closed, the closing delivery is created and retried until exported; a new `firing` with the same key opens a new episode | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `firing` | an `alarm.<name>.vN` event with `state: firing` from the core or a `job` worker; for monitoring problems, from a `job` task that rechecked the problem in code | there is no open episode with this `alarmkey`; `seq` 1, `repeat_count` 0; in the same transaction the `alarm` loop puts a delivery for E5 with `seq` and the tags `tenant`, `alarm`, `subject`, `severity` (alarms about the farm also carry `scope: farm`) | OBS-6, INJ-7, MON-1, MON-5, MON-11 |
| `firing` | `firing` | a repeated `firing` event with the same key | no event and no export; the episode increases `repeat_count` and updates `last_seen_at` | OBS-6, MON-1 |
| `firing` | `firing` | the `alarm` loop when the connection to the monitoring system is restored (the first successful `complete` of an E5 delivery after a failure) | re-export: a new opening delivery with the same `seq` for every open episode; the state does not change | MON-11 |
| `firing` | `resolved` | an event with `state: resolved` from code; for monitoring problems, after the problem is closed and rechecked by a `job` task | the same `alarmkey`; `seq` 2; the `alarm` loop puts a closing delivery for E5, which does not reach the monitoring system before the opening delivery | OBS-6, MON-1, MON-5, MON-11 |

## Invariants

- Only code opens and closes an alarm. The model and texts from logs, messages and monitoring problems can neither raise nor suppress an alarm (INJ-7). A person in the farm does not change the state of an alarm.
- At most one episode is open per `alarmkey`.
- A repeated `firing` of an open episode writes no event: the journal does not grow from trouble that lasts a month (OBS-6).
- Alarm data and the export are only numbers and `enum` values by the common schema, without people ids and texts (OBS-6, MON-1).
- Every change of an episode gives exactly one delivery for E5 with its `seq` and no delivery to an E1 channel (MON-1); a re-export after a connection failure repeats the opening delivery with the same `seq`, and the adapter does not open it a second time.
- An export delivery does not end as `dead` after a number of attempts: the core retries it with a growing pause of up to 10 minutes while the episode is open or its closing has not been exported; a newer delivery of the same `alarmkey` replaces the older one (MON-11). A failed export does not change the state of the alarm.
- The adapter passes the changes of one `alarmkey` in `seq` order: `resolved` does not go out before the `firing` of the same episode (MON-11).
- The age of the last successful export per tenant, `monitor_export_age_s`, is visible in `health`, `status` and `doctor`; an age over 15 minutes raises the alarm `alarm.monitor.export_stalled` and the code message `monitor_export_stalled` to the tenant admins and the operator, at most once every 6 hours (MON-1, MON-11, OBS-11).
- Notifying people, repeated notifications, "Acknowledged", escalation, on-call rotations and silencing are done in the monitoring system by the export tags; farm accounts have no rights to acknowledge, close or silence problems (MON-1, MON-4).
- A hold neither closes an alarm episode nor delays its export.

## Versions

| Version | What appears |
|---|---|
| v0.1 | the states `firing` and `resolved`; the common schema; the episode's `seq` and `repeat_count`; export of every episode change through E5; alarms for monitoring problems (`alarm.disk.free.low`, `alarm.disk.fill.rate`, `alarm.monitor.problem`) with the tag `origin: monitor`; an export that does not die, ordering by `seq`, re-export after a connection failure, `monitor_export_age_s`, the alarm `alarm.monitor.export_stalled` and the message `monitor_export_stalled` (MON-11) |
