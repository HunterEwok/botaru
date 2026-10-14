# Retired rule numbers

This file is generated from the registry `conformance/requirements.yaml`.

A rule number is never reused. These numbers are no longer active: the rule was revoked by a decision or merged into another rule. Active rules are in [rules.md](rules.md).

| Number | Title | Since | Why | Replaced by |
|---|---|---|---|---|
| AGG-1 | Aggregator role profile | 2026-10-05 | The limits of the `aggregator` profile are written as a row of the single table of role profiles. | [PRF-1](rules.md#PRF-1) |
| AGG-6 | Proposal coverage by an aggregator | 2026-10-06 | There is no coverage: an aggregator's proposal is assessed on its own and does not take credit from the lower-level author. | none |
| COM-1 | Commission declaration and reviewer profile | 2026-10-05 | The limits of the `reviewer` profile are written as a row of the single table of role profiles, and the commission's composition is set by the charter with seats and by the commission manifest with thresholds and weights. | [PRF-1](rules.md#PRF-1) |
| COM-11 | Calibration within a lens | 2026-10-14 | Merged into COM-17 in the simplification of v0.4 (D-114): calibration and accuracy measured the same thing, how often a lens executor is right, and one relative measure per lens stays. | [COM-17](rules.md#COM-17) |
| COM-12 | The flag "all votes from one model for one seat" | 2026-10-14 | The flag of a ballot bloc from one model existed only for the vote "who leaves", which the simplification of v0.4 removed (D-114, D-115). | none |
| DLV-8 | Outgoing webhook as a separate rule | 2026-10-05 | The outgoing webhook became a kind of E2 sink; its address is covered by DLV-6, the work id in every send by FEN-7, the signature by the sink sheet. | [DLV-6](rules.md#DLV-6), [FEN-7](rules.md#FEN-7) |
| GOV-1 | Identity in records (research number) | 2026-10-05 | The bot identity rule moved to the identity family as IDN-8. | [IDN-8](rules.md#IDN-8) |
| GOV-2 | Identity grants no permissions (research number) | 2026-10-05 | The rule moved to the identity family as IDN-9. | [IDN-9](rules.md#IDN-9) |
| GOV-5 | Seats and epoch (research number) | 2026-10-05 | The seat holder rule moved to the identity family as IDN-10. | [IDN-10](rules.md#IDN-10) |
| GOV-7 | Ballot | 2026-10-14 | The secret mandatory vote "who leaves" was removed in the simplification of v0.4 (D-114, D-115): code proposes the leaver by accuracy (GOV-43), otherwise a person chooses it from the list of seats on the promotion card (GOV-12). Ballots, their package, labels and forced schema went with it. | [GOV-12](rules.md#GOV-12), [GOV-43](rules.md#GOV-43) |
| GOV-8 | Code counts "who leaves" | 2026-10-14 | The secret mandatory vote "who leaves" was removed in the simplification of v0.4 (D-114, D-115): code proposes the leaver by accuracy (GOV-43), otherwise a person chooses it from the list of seats on the promotion card (GOV-12). The rounds, the two-thirds threshold and the tie keys went with it. | [GOV-12](rules.md#GOV-12), [GOV-43](rules.md#GOV-43) |
| GOV-10 | Free governance text | 2026-10-06 | There is no free model text in a case: there are no speeches and no ballot notes, because there are no elections (D-33). | none |
| GOV-11 | Handover of seat notes | 2026-10-05 | The handover of seat notes at a change of holder was replaced by offering verified facts to the successor and the newcomer choosing among them (D-28). | [MEM-7](rules.md#MEM-7) |
| GOV-15 | Money and priority of a case | 2026-10-14 | The money reserve and the priority class of a case existed only for the ballots of the vote "who leaves", which the simplification of v0.4 removed (D-114, D-115). | none |
| GOV-18 | One promotion trigger | 2026-10-06 | The rule about the single promotion trigger was merged with the queue and eligibility rule. | [GOV-14](rules.md#GOV-14) |
| GOV-20 | Audit sample of rejected proposals | 2026-10-07 | The audit sample is written in full in the improvement cycle rules: three strata and a monthly minimum (DIG-5), the blind card and "Lift veto" (DIG-6), and people's answers enter the calibration under COM-11. | [DIG-5](rules.md#DIG-5), [DIG-6](rules.md#DIG-6) |
| GOV-23 | No vote is lost | 2026-10-14 | The mandatory vote, ballot reissue, ballots by the code rule and the flag "stuck" went with the vote "who leaves" in the simplification of v0.4 (D-114, D-115). The case pause (a hold on the scope `governance`) is now written in GOV-14. | [GOV-14](rules.md#GOV-14) |
| GOV-24 | Trigger for manager elections | 2026-10-06 | There are no manager elections, so there is no trigger to open them (D-33). | none |
| GOV-26 | Candidate speech | 2026-10-06 | There are no candidate speeches, because there are no manager elections (D-33). | none |
| GOV-27 | Election ballot package | 2026-10-06 | There is no election ballot package and no election execution in `farm`, because there are no elections (D-33). | none |
| GOV-37 | No retroactive effect | 2026-10-14 | Merged into GOV-36 in the simplification of v0.4 (D-114): the same behaviour, written once. | [GOV-36](rules.md#GOV-36) |
| GOV-44 | Trigger, queue and eligibility for promotion from v0.3 | 2026-10-14 | Merged into GOV-42 in the simplification of v0.4 (D-114): the same behaviour, written once. | [GOV-42](rules.md#GOV-42) |
| GW-4 | P4 checks the request model | 2026-10-05 | There is one model selection rule: GW-7 took in the check of the request model and the ban on aliases without an exact version. | [GW-7](rules.md#GW-7) |
| GW-8 | A role only with profiles allowed to the tenant | 2026-10-14 | Merged into GW-7 in the simplification of v0.4 (D-114): the same behaviour, written once. | [GW-7](rules.md#GW-7) |
| HLD-4 | Maintenance window | 2026-10-08 | People set the maintenance window in the monitoring system, and the farm sets a code hold from it; there is no silencing effect (D-59, D-62). | [MON-9](rules.md#MON-9) |
| HLD-5 | Holds are visible and recorded | 2026-10-14 | Merged into HLD-1 in the simplification of v0.4 (D-114): the same behaviour, written once. | [HLD-1](rules.md#HLD-1) |
| IDM-5 | One channel update gives one task | 2026-10-14 | Merged into DLV-5 in the simplification of v0.4 (D-114): the same behaviour, written once. | [DLV-5](rules.md#DLV-5) |
| MEM-8 | The newcomer picks the facts itself | 2026-10-14 | Merged into MEM-7 in the simplification of v0.4 (D-114); the choice of facts by a model task was then dropped: code copies the rechecked facts to the successor (MEM-7). | [MEM-7](rules.md#MEM-7) |
| MGR-3a | One attempt, one tenant (name from the review) | 2026-10-05 | A name from the review with a letter in the number; the rule got number MGR-24. | [MGR-24](rules.md#MGR-24) |
| MGR-19 | Grant lens | 2026-10-06 | There is no grant lens: the core checks a grant, and the manager seat holder or a person decides it (D-33). | none |
| MGR-29 | Seat hold during elections (draft number) | 2026-10-06 | The draft number for a seat hold during elections was never introduced; its content went into GOV-22, and there are no holder elections (D-33). | [GOV-22](rules.md#GOV-22) |
| MGR-40 | Type history not from its own attempts | 2026-10-14 | Merged into MGR-17 in the simplification of v0.4 (D-114): the same behaviour, written once. | [MGR-17](rules.md#MGR-17) |
| MGR-43 | The batch of grant requests | 2026-10-14 | Merged into MGR-18 in the simplification of v0.4 (D-114): the same behaviour, written once. | [MGR-18](rules.md#MGR-18) |
| MGR-44 | A disk incident is preventable only with quotas | 2026-10-14 | Merged into MGR-45 in the simplification of v0.4 (D-114): the same behaviour, written once. | [MGR-45](rules.md#MGR-45) |
| MGR-46 | The notice of a preventable incident | 2026-10-14 | Merged into GOV-47 in the simplification of v0.4 (D-114): the same behaviour, written once. | [GOV-47](rules.md#GOV-47) |
| MGR-56 | The manager line covers the analysis | 2026-10-14 | Merged into MGR-49 in the simplification of v0.4 (D-114): the coverage lint of the manager line sits next to its monthly limit, with the same numbers and code. | [MGR-49](rules.md#MGR-49) |
| MSG-10 | One sender's share of an inbox | 2026-10-14 | Merged into MSG-15 in the simplification of v0.4 (D-114): the same behaviour, written once. | [MSG-15](rules.md#MSG-15) |
| OBS-5 | External "core is alive" watchdog | 2026-10-08 | The farm has no watchdog: core liveness is checked by the organization's monitoring system by polling P2 `health` (D-60). | [MON-2](rules.md#MON-2) |
| OBS-7 | Alarm acknowledgement and escalation | 2026-10-08 | Acknowledgement, escalation and notification of people about alarms are done in the monitoring system; the farm has no alarm delivery of its own (D-62). | [MON-1](rules.md#MON-1) |
| OBS-12 | Alarm about an overdue usage reconciliation | 2026-10-14 | Merged into BUD-10 in the simplification of v0.4 (D-114): the same behaviour, written once. | [BUD-10](rules.md#BUD-10) |
| PRP-15 | Share of one's own evidence | 2026-10-14 | Merged into PRP-22 in the simplification of v0.4 (D-114): the same behaviour, written once. | [PRP-22](rules.md#PRP-22) |
| REP-1 | Reporter profile | 2026-10-05 | The limits of the `reporter` profile are written as a row of the common role profile table PRF-1. | [PRF-1](rules.md#PRF-1) |
| TIM-4 | Schedule time zone | 2026-10-14 | Merged into TIM-3 in the simplification of v0.4 (D-114): the same behaviour, written once. | [TIM-3](rules.md#TIM-3) |
| TRN-1 | Limits of the translator role | 2026-10-05 | The limits of the `translator` profile are written as a row of the common profile table PRF-1. | [PRF-1](rules.md#PRF-1) |
| TRN-9 | The translator pauses after three confirmed links | 2026-10-14 | Replaced in the simplification of v0.4 (D-114) by the auto-hold of any role after three confirmed incident links in 90 days, set through holds and the same for every role (HLD-9); the owner agreed to it with the proposals of 2026-10-14. | [HLD-9](rules.md#HLD-9) |
