---
status: accepted
date: 2026-10-08
decision-makers: skeleton owner
---

# External monitoring; service control

## Context and Problem Statement

The organization already has a monitoring system that watches machines, wakes people, handles escalation and on-call, and holds scripts. The farm must not repeat it, yet it must report what only it sees and let bots restart a service at night without waiting for a person. The questions are who checks core liveness, how alarms reach people, and which service actions need an approval card. It touches the E5 Monitor boundary, P2 `health`, the tenant catalog, the `service/control` and `monitor/script_run` tools and the approval path.

## Decision Drivers

- the core cannot report its own death;
- one place to acknowledge an alarm, not two (D-62);
- the farm's account in the monitoring system decides nothing about problems (D-61, D-64);
- a restart is frequent and reversible; start and stop change availability (D-74);
- untrusted text must not trigger a write without a person (D-70, D-82).

## Considered Options

- alarms and liveness through the organization's monitoring system and E5 (D-59, D-60, D-62), or a farm watchdog and alarm delivery of its own (the canceled D-06);
- service control without a card for an allowed list set by people (D-67, D-68), or an approval card for every service action;
- without a card only `restart` from the list, when the task saw no untrusted text, and start and stop with a card (D-73, D-74, D-82).

## Decision Outcome

Chosen option: "external monitoring through E5; only `restart` from the allowed list without a card", because the monitoring system already pages people reliably and a narrow, reversible action at night should not wait for a person. The monitoring system polls P2 `health` from another machine; alarms go to people only through it, exported by the E5 adapter, and an installation without one gets a minimal core delivery (D-88). The farm's account reads data, writes only its own hosts and templates, and runs scripts only on hosts of its tenant's catalog (D-69). A restart from the catalog's allowed list goes without a card when the role has the right and the initiator has `service.control:<service>`; with untrusted text in the task it becomes a card, and a problem from the tenant's own monitoring triggers does not count as untrusted (D-82).

### Consequences

- Good, because people acknowledge and silence alarms in one system, and the farm adds tasks, bots, money and the commission.
- Good, because a night restart does not wait, and every action is visible in the audit, the task log and a note in the monitoring system.
- Good, because a stolen farm account cannot close problems or run scripts on farm nodes.
- Bad, because an installation depends on the monitoring system for liveness, or takes the reduced single-server path of D-88.
- Bad, because item values and log lines from monitoring stay untrusted, so many restarts still need a card.

### Confirmation

- MON-1, alarms to people along one installation path: fixture, lint and black-box `blackbox:core/alarm-path-monitor`, `blackbox:core/alarm-notice-no-monitor`.
- MON-2, an external poll checks core liveness: lint and black-box `blackbox:core/health-watch`, fixture `manifests/deploy`.
- MON-4, permissions of the farm account in the monitoring system: adapter `adapter:monitor/account-scope`.
- MON-12, alarm export to the monitoring system: fixture, lint and black-box `blackbox:monitor/export-stalled`.
- MON-8, service control: lint `lint:manifests/service-no-card`, black-box `blackbox:core/service-stop-card`, fixture `fixture:authorize/service-initiator`.
- APR-18, services and monitoring scripts without a card: lint, black-box `blackbox:model/service-allowed`, `blackbox:model/service-untrusted`.
- INJ-15, after untrusted text a service action goes as a card: fixture, black-box and eval, `blackbox:model/service-monitor-trusted`, `blackbox:model/service-monitor-item-text`, `inj-12`.

## Pros and Cons of the Options

### Monitoring through E5

- Good, because the organization's alerting, on-call and escalation are reused.
- Bad, because the farm has to keep a binding for each monitoring system it supports.

### A farm watchdog of its own

- Bad, because it duplicates alerting and creates a second place to acknowledge.

### A card for every service action

- Good, because a person sees every change.
- Bad, because a frequent reversible restart would wait for a person at night, and cards would pile up (G6).

### Only `restart` without a card

- Good, because the risk is held by closed lists: service and verb from the catalog, the machine chosen by code.
- Bad, because the list and the trigger catalog must be kept current by people in git.

## More Information

Decisions D-59 to D-64, D-66 to D-70, D-73, D-74, D-82, also D-88. Documents: [operations.md](../operations.md), [permissions.md](../permissions.md), `spec/ports/e5-monitor.md`, [goals.md](../goals.md) (C5).
