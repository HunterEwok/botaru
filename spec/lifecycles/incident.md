# Lifecycle: incident

## In short

An incident (`Incident`) is a record that only the core opens, on a code alarm and a catalog class `IncidentClass`. Alarms about machines and resource disks are raised by the `job` role without a model, from monitoring system problems after a recheck through its API (MON-5, D-63). An incident belongs to exactly one tenant or to the farm (the subject "farm"). Only code moves the state up to `resolved`, a person or the seat holder closes the incident after `resolved`, and only a person declares it false. Only a person sets the link to a cause (`attribution`); it lives separately from the state and is described in the incident attribution cycle. Every transition writes an event in the same transaction and edits the one incident message to people (MGR-39). Calling people for an unhandled incident is the alarm `alarm.incident.unhandled`, which the monitoring system delivers to people (MON-1, D-62).

Record fields: `id`, `class`, `severity`, `tenant`, `subject`, `alarm` (the alarm block: the key `alarmkey`, the numbers `values`, the origin `origin` and `monitor_event`, the number of the monitoring system problem if the alarm was raised from one, otherwise empty; OBS-6), `state`, `implicated` (up to 20 targets, code recomputes them once a minute), `ops` (operations on the incident), `taken_by` (which person took it up, `incident.ack`), `stops_manager_effect` (whether the incident gave a seat hold and why not, codes of MGR-33 and MGR-34), `opened_at`, `resolved_at`, `closed_at`, `closed_by`, `dismiss` (for a false incident: `reason_code`, `details`, who dismissed it).

## States

| State | Meaning | Terminal |
|---|---|---|
| `open` | a code alarm holds by the class conditions, the core opened an incident and built the list of involved parties | no |
| `contained` | code applied the first measure of the class (`contain_auto`) | no |
| `mitigating` | the first operation of the farm manager or a person was applied for the incident | no |
| `resolved` | the alarm has been in the `resolved` state longer than the class's `resolve.for`; an alarm from a monitoring problem is in `resolved` when the problem is closed and a recheck by code confirmed it | no |
| `reopened` | an alarm with the same key came back within 1 h after `resolved`; the number stays the same, it behaves like `open` | no |
| `closed` | a person or the seat holder closed the incident after `resolved` | yes |
| `dismissed` | a person declared the alarm false with a reason code and "Details" | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `open` | the core's `incident` loop | a code alarm (`alarm.*`, OBS-6), including one raised from a monitoring system problem (MON-5), is in `firing`, the class's `detect.when` conditions hold for `for`; there is no open incident of the same class and subject in the window of the key `system:incident:<class>:<subject>:<window>` (IDM-2) | MGR-7, MGR-8, INJ-7, MON-5 |
| `open`, `reopened` | `contained` | the `incident` loop | the first measure `contain_auto` is applied by code | MGR-8 |
| `open`, `contained`, `reopened` | `mitigating` | C1 `control.apply` | the first operation of the farm manager or a person with the basis "this incident" | MGR-8, MGR-9, MGR-13 |
| `open`, `contained`, `mitigating`, `reopened` | `resolved` | the `incident` loop | the alarm has been in `resolved` longer than `resolve.for` | MGR-8 |
| `resolved` | `reopened` | the `incident` loop | an alarm with the same key is in `firing` again no later than 1 h after `resolved` | MGR-8 |
| `resolved` | `closed` | P2 `incident.close` (a person of T with `incident.manage`); the built-in tool `incident_close` in the closing pass (the seat holder) | only after `resolved`; the holder with the current epoch | MGR-8 |
| `open`, `contained`, `mitigating`, `resolved`, `reopened` | `dismissed` | P2 `incident.dismiss {reason_code, details}` or a channel command of a person of T with `incident.manage` | a false alarm; only a principal of kind `human`; a reason code from the closed list (`false_alarm`, `threshold_misconfigured`, `maintenance`, `duplicate`, `test`, `other`) and a "Details" text of up to 500 characters are mandatory, otherwise `dismiss_reason_required`; from v0.2, in the same transaction, attribution candidates without a "Yes" answer are canceled, and an attribution with a "Yes" answer becomes disputed (`disputed`) with a card to the author of the answer (GOV-13, D-79) | MGR-8, GOV-13 |

Separate from the state:

| What | Who | Rules |
|---|---|---|
| `incident.ack`: a person took up the incident; the alarm `alarm.incident.unhandled` is not raised, and a raised one moves to `resolved`. People set "Acknowledged" on the alarm itself in the monitoring system | a person of T | MGR-8, MON-1 |
| changing the severity | only a person | MGR-8 |
| the link to a cause (code builds the candidate, a person decides) | a person with `governance.attribute` | MGR-8, GOV-13 |
| the alarm `alarm.incident.unhandled` (`page`) if within `page_if_unhandled_after` there is neither `incident.ack` nor an operation; the monitoring system delivers it to people, repeats and escalation are there too | the `incident` loop | MGR-2, MON-1 |
| a note of the decision role on the monitoring system problem for which the incident is open (from v0.2, at most 10 per hour per tenant); the note cannot acknowledge, close or silence the problem or change its severity | the seat holder | MON-7 |
| an action on a service or a monitoring script on the subject's node (`service/control`, `monitor/script_run`): a `service_grant` record, a line in the incident message, the code note `service_control_done` on the problem, a state check after 2 minutes | the seat holder | MON-8, MON-10, MGR-9 |
| the farm manager's response task on opening (key `system:incident:<class>:<subject>:<window>`) and the closing pass on `resolved` (key `system:incident:<incident>:close`), only if the subject is in a tenant | the `incident` loop | MGR-4, IDM-2 |
| a seat hold for an incident of a class with `stops_manager: true` | the seat loop (GOV-22) in the opening transaction, under the conditions of MGR-33 and MGR-34 | MGR-33, MGR-34 |

## Invariants

- Only the core creates an incident, on a code alarm and a class from the catalog in git. Neither the model, nor a bot, nor tool output opens, changes or hides an incident or an alarm (INJ-7, GOV-13). The trigger name and the texts of a monitoring problem do not get into the incident: only closed event fields and the numbers of the recheck (MON-5).
- The seat holder starts, stops and restarts services and runs monitoring scripts only on the node of the incident's subject, only from the list that code assembled from the tenant catalog, the class (`responses.services`, `responses.monitor_scripts`) and the tenant's consent (MON-8, MON-10, D-67). A call from the list goes without a card (D-68); after untrusted text, including after read findings, it goes as an approval card to a person (INJ-15, D-70). Such actions do not change the incident itself: only code sets `resolved`, by the alarm state.
- Only code sets `resolved`, by the alarm state. The seat holder does not close the incident before `resolved`, does not declare it false, does not change the severity and does not set the attribution.
- Closing an incident does not release holds. The first measure of a class (`contain_auto`) is a hold with a deadline: it ends by `until` if a person did not release it earlier. In the closing pass the seat holder releases only its own holds, and the core does not release holds itself (HLD-2).
- `dismissed` is possible from any unfinished state, including `resolved`: declaring false an alarm that already went away by itself is needed to return a stopped seat (GOV-22). A dismissal always carries a reason code and "Details", which only people see. From v0.2 it also cancels attribution candidates without a "Yes" answer, and the change can again give a promotion trigger; it does not erase someone else's "Yes" answer but moves it into a dispute, which the author of the answer or 7 days of silence decide (GOV-13, D-57, D-79).
- In a tenant with `farm_manager.enabled: false` (MGR-42, D-77) the incident goes only to the people of the tenant: there are no seat tasks for it, it gives no seat hold, and `farm` operators see it only as numbers in the daily summary.
- An incident belongs to one tenant (TEN-1) or to the farm. An incident with the subject "farm" has no seat tasks: code and people handle it.
- The list of involved parties is built by code with functions from a closed list, at most 20 targets, and adds to the involved workers their bot identities, roles and bot groups, and to the involved tasks their types; the seat holder chooses targets only from it (MGR-9).
- Every transition writes an event (`incident.opened`, `incident.resolved`, `incident.closed` and so on) and an edit of the incident message in one transaction (MGR-39).
- An incident of a class with `stops_manager: true` opened before the holder was assigned or returned gives no seat hold. Exception: a seat in `stopped` only for the reason `no_control`, which appeared inside `prevent_window` while the holder was responsible for the seat (MGR-34), gets the reason `incident` anyway (GOV-22).

## Versions

| Version | What appears |
|---|---|
| v0.1 | all states and transitions, the class catalog, incidents from monitoring system problems, the farm manager's response tasks, the farm manager's actions on services and monitoring scripts on the subject's node, one message per incident, the alarm `alarm.incident.unhandled` |
| v0.2 | a human decision on the attribution of an incident to a change (the attribution cycle) and a dispute over a "Yes" answer on `dismissed`; farm manager notes on monitoring system problems |
