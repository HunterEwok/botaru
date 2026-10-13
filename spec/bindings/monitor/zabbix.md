# Monitoring system binding: Zabbix

## In short

This is the reference binding of port E5 to Zabbix 7.0 and later. The full table of duties ("how it maps onto Zabbix") is in the port sheet, in the section "Reference binding: Zabbix 7.0 and later" of the file `spec/ports/e5-monitor.md`, and is not repeated here: a fact lives in one place. This file collects the names that the binding pins and says where the data lives and which checks hold the binding.

## What it implements

| What | Operations of sheet E5 | Since |
|---|---|---|
| export of alarm episodes to trapper items and triggers of the farm template | alarm export, retries until uploaded (MON-1, MON-11) | v0.1 |
| signed audit head in the item history | audit head (MON-3) | v0.1 |
| adapter tick and core liveness by polling P2 `health` | adapter tick, `health` polling (MON-2, LCY-6) | v0.1 |
| problem input by webhook and re-check | `ext.monitor.problem.v1` (MON-5) | v0.1 |
| script list and script run | `ext.monitor.scripts.v1`, `monitor_script` (MON-10, MON-8) | v0.1 |
| problem notes | `monitor_note`: code note from v0.1 (MON-8), bot note from v0.2 (MON-7) | v0.1, v0.2 |
| `match` and `erase` of notes | `erasure` loop (RET-4, RET-7) | v0.1 |
| maintenance windows | `ext.monitor.maintenance.v1` (MON-9) | v0.2 |
| a Zabbix user for each adapter process | permissions and their check in `doctor` (MON-4) | v0.1 |

## Names the binding pins

| What | Name in Zabbix |
|---|---|
| farm host for export and liveness | `botaru-core` (machine `core1`) |
| alarm trapper items by severity | `botaru.alarm[info]`, `botaru.alarm[warn]`, `botaru.alarm[page]` |
| severity mapping on export | `info` -> Information, `warn` -> Warning, `page` -> High |
| severity of a problem of class `unknown` for `alarm.monitor.problem` | always `warn`: the class table sets the severity, not the problem severity in Zabbix |
| problem tags | `tenant`, `alarm`, `subject`, `severity`, `scope`, `origin`, `key` |
| audit head | text trapper `botaru.audit.head[<tenant>]` |
| adapter tick | numeric trapper `botaru.adapter.alive[<tenant>]`, window by the macro `{$BOTARU.ADAPTER.WINDOW}` |
| core liveness | HTTP agent `botaru.health`, key in the macros `{$BOTARU.HEALTH.KEY}` and `{$BOTARU.HEALTH.KEY.NEXT}`, window `{$BOTARU.HEALTH.WINDOW}` |
| user groups of adapters | `botaru-mon-farm`, `botaru-mon-demo`, `botaru-mon-demo2` |
| note | `event.acknowledge` only with `action` 4 ("add message"), mark `[botaru <bot_id>]` |

The export fields and data shapes are set by the schemas `spec/model/monitor-alarm.schema.json`, `spec/model/monitor-audit-head.schema.json`, `spec/model/monitor-note.schema.json`, `spec/model/monitor-script.schema.json`, `spec/model/monitor-script-result.schema.json` and the data schemas of the `ext.monitor.*` events in `spec/model/event-data/`. A problem class maps to a farm alarm by the table `spec/model/monitor-map.yaml`.

## Data in git

Farm hosts, the farm template, the group and the user for each adapter process live as data in `deploy/monitor/`: `deploy/monitor/farm/` for the `farm` process and `deploy/monitor/<tenant>/` for the process of each tenant.

## Checks

- The `conformance/suites/monitor` suite with a mock monitoring system (the list of checks is in sheet E5).
- Fixtures `conformance/fixtures/monitor/`: the class table, storage headroom (CAP-5) and volume quotas (CAP-6).
- `farmctl doctor`: the permissions of each adapter user through the API, the history period of the audit witness (a warning if shorter than 90 days), the export age (MON-4, MON-3, MON-11).
