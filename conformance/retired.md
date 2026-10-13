# Retired rule numbers

This file is generated from the registry `conformance/requirements.yaml`.

A rule number is never reused. These numbers are no longer active: the rule was revoked by a decision or merged into another rule. Active rules are in [rules.md](rules.md).

| Number | Title | Since | Why | Replaced by |
|---|---|---|---|---|
| AGG-1 | Aggregator role profile | 2026-10-05 | The limits of the `aggregator` profile are written as a row of the single table of role profiles. | [PRF-1](rules.md#PRF-1) |
| AGG-6 | Proposal coverage by an aggregator | 2026-10-06 | There is no coverage: an aggregator's proposal is assessed on its own and does not take credit from the lower-level author. | none |
| COM-1 | Commission declaration and reviewer profile | 2026-10-05 | The limits of the `reviewer` profile are written as a row of the single table of role profiles, and the commission's composition is set by the charter with seats and by the commission manifest with thresholds and weights. | [PRF-1](rules.md#PRF-1) |
| DLV-8 | Outgoing webhook as a separate rule | 2026-10-05 | The outgoing webhook became a kind of E2 sink; its address is covered by DLV-6, the work id in every send by FEN-7, the signature by the sink sheet. | [DLV-6](rules.md#DLV-6), [FEN-7](rules.md#FEN-7) |
| GOV-1 | Identity in records (research number) | 2026-10-05 | The bot identity rule moved to the identity family as IDN-8. | [IDN-8](rules.md#IDN-8) |
| GOV-2 | Identity grants no permissions (research number) | 2026-10-05 | The rule moved to the identity family as IDN-9. | [IDN-9](rules.md#IDN-9) |
| GOV-5 | Seats and epoch (research number) | 2026-10-05 | The seat holder rule moved to the identity family as IDN-10. | [IDN-10](rules.md#IDN-10) |
| GOV-10 | Free governance text | 2026-10-06 | There is no free model text in a case: there are no speeches and no ballot notes, because there are no elections (D-33). | none |
| GOV-11 | Handover of seat notes | 2026-10-05 | The handover of seat notes at a change of holder was replaced by offering verified facts to the successor and the newcomer choosing among them (D-28). | [MEM-7](rules.md#MEM-7), [MEM-8](rules.md#MEM-8) |
| GOV-18 | One promotion trigger | 2026-10-06 | The rule about the single promotion trigger was merged with the queue and eligibility rule. | [GOV-14](rules.md#GOV-14) |
| GOV-20 | Audit sample of rejected proposals | 2026-10-07 | The audit sample is written in full in the improvement cycle rules: three strata and a monthly minimum (DIG-5), the blind card and "Lift veto" (DIG-6), and people's answers enter the calibration under COM-11. | [DIG-5](rules.md#DIG-5), [DIG-6](rules.md#DIG-6) |
| GOV-24 | Trigger for manager elections | 2026-10-06 | There are no manager elections, so there is no trigger to open them (D-33). | none |
| GOV-26 | Candidate speech | 2026-10-06 | There are no candidate speeches, because there are no manager elections (D-33). | none |
| GOV-27 | Election ballot package | 2026-10-06 | There is no election ballot package and no election execution in `farm`, because there are no elections (D-33). | none |
| GW-4 | P4 checks the request model | 2026-10-05 | There is one model selection rule: GW-7 took in the check of the request model and the ban on aliases without an exact version. | [GW-7](rules.md#GW-7) |
| HLD-4 | Maintenance window | 2026-10-08 | People set the maintenance window in the monitoring system, and the farm sets a code hold from it; there is no silencing effect (D-59, D-62). | [MON-9](rules.md#MON-9) |
| MGR-3a | One attempt, one tenant (name from the review) | 2026-10-05 | A name from the review with a letter in the number; the rule got number MGR-24. | [MGR-24](rules.md#MGR-24) |
| MGR-19 | Grant lens | 2026-10-06 | There is no grant lens: the core checks a grant, and the manager seat holder or a person decides it (D-33). | none |
| MGR-29 | Seat hold during elections (draft number) | 2026-10-06 | The draft number for a seat hold during elections was never introduced; its content went into GOV-22, and there are no holder elections (D-33). | [GOV-22](rules.md#GOV-22) |
| OBS-5 | External "core is alive" watchdog | 2026-10-08 | The farm has no watchdog: core liveness is checked by the organization's monitoring system by polling P2 `health` (D-60). | [MON-2](rules.md#MON-2) |
| OBS-7 | Alarm acknowledgement and escalation | 2026-10-08 | Acknowledgement, escalation and notification of people about alarms are done in the monitoring system; the farm has no alarm delivery of its own (D-62). | [MON-1](rules.md#MON-1) |
| REP-1 | Reporter profile | 2026-10-05 | The limits of the `reporter` profile are written as a row of the common role profile table PRF-1. | [PRF-1](rules.md#PRF-1) |
| TRN-1 | Limits of the translator role | 2026-10-05 | The limits of the `translator` profile are written as a row of the common profile table PRF-1. | [PRF-1](rules.md#PRF-1) |
