# Deferred rules

This file is generated from the registry `conformance/requirements.yaml`.

A deferred rule is not part of any skeleton version: no version checks it, and no document, schema or registry may refer to it. Its row keeps the full text so that the rule can come back unchanged when the condition in "Returns when" holds; it then becomes active in the version current at that time. "Mechanism removed" says whether the code and the skeleton were cleaned of what the rule needed. An implementation does not offer the mechanism of a deferred rule even where its fields are still in the schemas: without its rule the mechanism has none of its conditions (D-114).

Deferred rules: 41.

| Number | Title | Deferred | Why | Returns when | Mechanism removed |
|---|---|---|---|---|---|
| [APR-8](#APR-8) | Debug replay without approvals or writes | 2026-10-14 | A debug replay is a developer convenience, not needed before the pilot; it returns together with all its prohibitions. | Debugging a task needs a replay of its recorded journal. | no |
| [APR-13](#APR-13) | Summary "approved without reading" | 2026-10-14 | A monthly summary of approvals given without reading is a report, not a protection; not needed before the pilot. | Approval cards are many enough that people may approve without reading. | no |
| [AUD-4](#AUD-4) | The attempt journal is only in a deletable store | 2026-10-14 | The attempt journal for debugging is not needed before the pilot; it returns together with APR-8 and its default prohibition for production. | Debugging a task needs its recorded journal. | no |
| [BUD-6](#BUD-6) | In-flight task limit without refusal | 2026-10-14 | An in-flight limit per person matters under load; the pilot has few people. | People of a tenant run enough tasks at once to crowd each other out. | no |
| [BUD-8](#BUD-8) | The lowest priority class for analysis and reviews | 2026-10-14 | A background share protects work tasks under load; in the pilot separate budget lines (BUD-7) are enough. | Analysis and review tasks delay work tasks. | no |
| [CAP-3](#CAP-3) | Delivery rate to a channel | 2026-10-14 | The channel driver keeps the rate limit of the external system itself (retry_after); the pilot does not reach channel limits. | Deliveries hit the rate limits of a channel. | no |
| [CNT-6](#CNT-6) | Waiting for an event only from the type | 2026-10-14 | The pilot starts with operations tasks; routes on events already react to events without a waiting task. | A task type needs to wait for an external event inside one task. | no |
| [COM-9](#COM-9) | The shadow field in a review | 2026-10-14 | The shadow field exists only for the shadow reviewer (COM-10). | COM-10 returns. | no |
| [COM-10](#COM-10) | Shadow reviewer at the tenant's choice | 2026-10-14 | The shadow reviewer is off by default (D-103) and not needed for the pilot. | The tenant wants to compare a candidate reviewer before seating it. | no |
| [COM-18](#COM-18) | A stronger model for the most accurate member | 2026-10-14 | A stronger model for the most accurate member has a ceiling of 0 euro by default and needs scored forecasts; it does not switch on in the pilot. The outcome of D-109 (5) returns with it. | The commission has scored forecasts and a budget for the reward. | no |
| [DIG-9](#DIG-9) | A clear benefit of a rejected change reaches people as one line | 2026-10-14 | Depends on PRP-25 and returns with it. | PRP-25 returns. | no |
| [DLV-17](#DLV-17) | Outgoing webhook sink | 2026-10-14 | The pilot delivers to people and to the monitoring system; no outgoing webhook is needed. When it returns, its SSRF protection and signature stay mandatory. | A tenant needs to push results to an external program. | no |
| [DSP-9](#DSP-9) | Dispatch can be explained | 2026-10-14 | An operator of the pilot sees why a task waits from the queue counters and doctor; a separate explanation operation is not needed yet. | An operator asks why a task waits and the counters do not answer. | no |
| [DSP-11](#DSP-11) | Bot group quota and a rule's share | 2026-10-14 | A pilot tenant has few bot groups; the per-type max_running (DSP-7) is enough. | Several bot groups of one tenant compete for capacity. | no |
| [GOV-35](#GOV-35) | Locked commission | 2026-10-14 | The card "the commission is stuck" works on three months of flags and does not fire before the pilot ends. | The commission has worked for three months. | no |
| [GOV-39](#GOV-39) | The farm manager may only hint at a link | 2026-10-14 | Before the pilot links are found by code and confirmed by people; a hint of the farm manager is not needed. | Code misses links that the farm manager could point at. | no |
| [INS-5](#INS-5) | Analysis to the role's bots in turn | 2026-10-14 | Analysis in turn among the bots of a role matters for a fair path with several bots per role; the pilot has one or two. | A role has several bots that compete for analysis. | no |
| [KNW-6](#KNW-6) | Corpus search is a word search | 2026-10-14 | How the corpus search works inside belongs to the knowledge profile, not needed for operations tasks. | The knowledge profile enters the pilot. | no |
| [LCY-5](#LCY-5) | Plugin update and deprecation | 2026-10-14 | Before the pilot a plugin is updated with a short blocked state; a smooth update with two fingerprints is not needed. | A plugin update during work blocks tasks that people wait for. | no |
| [MAN-7](#MAN-7) | Deprecation, version removal and apiVersion change | 2026-10-14 | Deprecation and an apiVersion converter are needed when the manifest version changes, not for the pilot. | The manifest apiVersion changes. | no |
| [MAN-12](#MAN-12) | The P1 OpenAPI description | 2026-10-14 | The OpenAPI description of P1 helps foreign workers; the pilot runs its own workers. | A foreign worker is written against P1. | no |
| [MEM-5](#MEM-5) | Task order within one conversation | 2026-10-14 | Ordering tasks within one conversation matters for chats with people; operations tasks of the pilot do not need it. | Bots hold conversations with people where order matters. | no |
| [MGR-12](#MGR-12) | Task pinning | 2026-10-14 | Task pinning is a rare measure; in the pilot hold, quarantine, request_action and people through farmctl are enough. | The farm manager needs to send a task to one named worker. | no |
| [MON-7](#MON-7) | Bot notes on problems | 2026-10-14 | Bot notes on monitoring problems are useful but not needed before the pilot and need editing of notes and a data recipient. | Operators ask for bot notes on problems in the monitoring system. | no |
| [OBS-13](#OBS-13) | The metrics catalog | 2026-10-14 | Machine and service metrics are in the monitoring system (D-59) and the core decides nothing by them; the catalog is not needed. | The core needs metrics of its own beyond the monitoring system. | no |
| [OBV-8](#OBV-8) | P1 obs.append for job attempts | 2026-10-14 | In the pilot the series of the core and the data of the monitoring system (MON-6) are enough; job attempts need not write series. | A job needs to write observation points itself. | no |
| [ORC-15](#ORC-15) | Waves and pace of a campaign | 2026-10-14 | A campaign of the pilot has a canary, max_in_flight and a failure threshold (ORC-4); waves and pace are not needed. | A campaign touches enough items that a staged rollout after the canary is needed. | no |
| [PRM-11](#PRM-11) | A dry run without side effects | 2026-10-14 | A dry run is a convenience for programs; it returns together with its prohibition for workers. | A program needs to check an enqueue without creating a task. | no |
| [PRP-17](#PRP-17) | A late related proposal earns no credit | 2026-10-14 | Credit protection against late similar proposals matters with many bots of one role; the pilot has few. | Several bots of one role propose the same changes. | no |
| [PRP-25](#PRP-25) | Replay of a rejected change over the history | 2026-10-14 | Replaying a rejected change over the history needs a replay engine of alarms and limits; too costly before the pilot. The outcome of D-109 (8) returns with it. | The farm has enough history of rejected changes to replay them. | no |
| [RET-9](#RET-9) | Tenant retention periods | 2026-10-14 | The default retention periods (RET-2) are enough for the pilot. | A tenant needs retention periods other than the defaults. | no |
| [SBX-1](#SBX-1) | Tier 2 lives only in the sandbox | 2026-10-14 | Tier 2 and the sandbox are not needed for operations tasks; the rule returns whole with its prohibitions, and the reserve unit of sandbox seconds returns with it. | A tenant needs data analysis by model-written code. | no |
| [SBX-2](#SBX-2) | One disposable container per task | 2026-10-14 | Goes with SBX-1. | SBX-1 returns. | no |
| [SCR-12](#SCR-12) | A script from git on a schedule | 2026-10-14 | Scripts from git on a schedule are a second way to run things next to task schedules (TIM-3), which the operations pilot uses. | A tenant needs a multi-step script on a schedule. | no |
| [SCR-13](#SCR-13) | The steps wait and await | 2026-10-14 | Steps that wait days for events are not needed for the first operations tasks. | A script needs to wait for time or an external event. | no |
| [SCR-14](#SCR-14) | A script from git with parameters and a free goal | 2026-10-14 | Scripts from git with parameters are a second launch path next to the translator; not needed before the pilot. | A tenant needs a reviewed script with parameters. | no |
| [SEC-7](#SEC-7) | A person's connection to an external service | 2026-10-14 | People's connections to their own external services serve the personal assistant, not the operations pilot; the rule returns whole with its protections. | The personal assistant enters the pilot. | no |
| [SEC-12](#SEC-12) | Privacy report | 2026-10-14 | A privacy report is a convenience for a data protection assessment; before the pilot the links of SEC-8 are enough. | The organization asks for a privacy report. | no |
| [SKL-4](#SKL-4) | Reading a skill through the core | 2026-10-14 | Reading a skill through the core belongs to the skills feature; SKL-2 and SKL-3 keep the protection. | Roles of the pilot use skills. | no |
| [STA-3](#STA-3) | Two columns and a mismatch code | 2026-10-14 | last_seen and the leases in status are enough for the operator of the pilot. | The operator cannot tell a worker's state from last_seen and leases. | no |
| [TRN-8](#TRN-8) | Confirmed cases enter the translator check of the tenant | 2026-10-14 | Confirmed cases in the tenant check set need incidents, which there are none of before the pilot. | The translator has confirmed incident cases. | no |

<a id="APR-8"></a>
## APR-8. Debug replay without approvals or writes

serves [G5](../docs/goals.md) · was v0.2 · profile debug · check: blackbox

A debug replay of a task (P2 `task.replay`) is an operator operation under the same `operator_access.debug_journal` decision as P2 `task.journal` (AUD-4), with `admin:debug` and `data:<mark>`: with `deny` it answers `forbidden`, with `notify` it writes an audit record and sends the tenant admins a code message, with `allow` it writes an audit record. A replay answers every tool call only from the recorded attempt journal of the original and never reads live: it is refused with `replay_journal_missing` when the journal is off or expired, and with `replay_personal` when the original carries `personal_seen` or made a call with `credential: per_principal` (SEC-7). It is refused with `replay_role_forbidden` for a task of purpose `manage` and for a task of a manager role, and a task of type `action.*` is not replayed. The replay is a new task in the task's tenant with the type, body, mark and tier of the original; an inactive initiator gives `forbidden`, and it is charged to the tenant limits like the original, at most 10 replays per tenant per day (`replay_quota`). Only a closed list of tools is replayed from the journal: built-in tools without `changes_state` in `spec/model/builtin-tools.yaml` and plugin tools with `access: read` and without `credential`. Every other call, including `request_action`, `enqueue_child`, `send_message`, `escalate`, `request_model_grant`, `service/control`, `monitor/script_run` and any write tool, is only recorded: a replay never creates approvals, tasks, messages, grants, escalations, `service_grant` records or write calls. A replay does not deliver the result to `origin`, returns it only to the operator who requested it, and does not leave the task's tenant.

<a id="APR-13"></a>
## APR-13. Summary "approved without reading"

serves [G2](../docs/goals.md) · was v0.2 · profile core · check: blackbox

Once a month the core computes, per tenant and subject kind rather than per person, two shares: decisions faster than the threshold `attention.fast_decision_s` (default 10 seconds after delivery) without "Details" opened, and "Approve" on cards with `untrusted_seen`. The summary goes to the tenant admins. Series, lists and comparisons of individual people are not built and are not shown to anyone. If fewer than three people decided on a subject kind during the month, the share for that kind is not shown separately.

<a id="AUD-4"></a>
## AUD-4. The attempt journal is only in a deletable store

serves [G5](../docs/goals.md) · was v0.2 · profile debug · check: store, blackbox

The core writes the attempt journal (model requests and responses as P4 saw them, and tool calls with wrapped output as P3 saw them). It is stored only in a deletable store of class `content`, with the tenant's expiry `retention.journals` and the task's mark; it does not reach audit, events or the model context, and only an operator reads it, through P2 `task.journal` with `admin:debug` and `data:<mark>`. Operator reading of the journal is governed by the tenant field `operator_access.debug_journal: deny | notify | allow` (default `notify`): with `deny`, P2 `task.journal` answers `forbidden`; with `notify`, every read writes an audit record and sends the tenant admins a code message without content; with `allow`, only an audit record. Debug replay (APR-8) is decided by the same field. For a tenant with `kind: production` the journal is off until the tenant sets an expiry; for a test tenant the default expiry is 7 days.

<a id="BUD-6"></a>
## BUD-6. In-flight task limit without refusal

serves [G1](../docs/goals.md) · was v0.2 · profile core · check: blackbox

A principal of kind `human` or `service` has at most `Tenant.spec.budgets.tasks_in_flight` (default 3) of their direct requests with a running attempt. A further direct request is not refused: it waits in `submitted` with `waiting_reason: in_flight_cap` (dispatch condition D21), and the principal's waiting direct requests of one type are handed out in enqueue order; a request held by another dispatch condition (no executor, a hold, a ceiling) does not hold back the requests behind it. Tasks of `system`, campaign items, children and continuations do not count.

<a id="BUD-8"></a>
## BUD-8. The lowest priority class for analysis and reviews

serves [G1](../docs/goals.md) · was v0.2 · profile core · check: fixture, store

Tasks of the purposes `insight`, `aggregate` and `review` go in priority class 5 (`background`, DSP-5) whatever the type's `dispatch.priority_class` and the request say. One exception: tasks of `review.escalation.v1` (the review type of the `necessity` lens, `spec/model/lenses.yaml`) go in class 3 and are outside the share, because an escalation waits for them. In a bot group whose roles also claim types of other purposes, the class 5 tasks of these purposes together hold at most `Tenant.spec.dispatch.background_share` (default 20%) of the group's capacity: its `max_running`, or the number of its active workers when it has none, rounded down but at least one. A task above the share waits with `waiting_reason: group_quota`.

<a id="CAP-3"></a>
## CAP-3. Delivery rate to a channel

serves [G1](../docs/goals.md) · was v0.2 · profile core · check: adapter, blackbox

The channel driver sheet declares its maxima, and `Channel.spec.rate` (`per_minute` for the bot, `per_chat_per_minute`, `per_group_per_minute`) can only lower them; a driver without them gets the farm ceiling from the deployment settings. The delivery loop keeps to the rate: a delivery that must wait stays `planned` without spending an attempt, and deliveries to one chat go out in order. A channel answer that the external system limited the rate (`retry_after_s`) puts that chat on pause for that time and does not spend a delivery attempt.

<a id="CNT-6"></a>
## CNT-6. Waiting for an event only from the type

serves [G1](../docs/goals.md) · was v0.2 · profile core · check: lint, blackbox

Waiting for an external event is declared only in the task type: `await: {event, match, timeout, then, on_timeout}`. `event` is an event type of the events catalog; `match` is 1-5 pairs (`event_field`, `result_field`) that must all be equal, where the result field has `x-botaru-read-by: code` and the event field is a closed field of the event data schema (OUT-1); `timeout` is required and no longer than the farm ceiling (7 days by default). The core writes a waiting record in the transaction of `complete`. The first event of the same tenant that matches enqueues the `then` continuation (trigger `await`); the timeout enqueues `on_timeout` (trigger `timer`), or without it ends the record with the event `await.expired`. Each waiting record gives at most one continuation (CNT-2).

<a id="COM-9"></a>
## COM-9. The shadow field in a review

serves [G5](../docs/goals.md) · was v0.1 · profile insights · check: lint, fixture

The `review-result` schema carries the field `shadow` (default `false`). The verdict code does not read a review with `shadow: true`; it is not shown to people or reviewers and goes only to the comparison of seat and shadow scores (COM-10).

<a id="COM-10"></a>
## COM-10. Shadow reviewer at the tenant's choice

serves [G5](../docs/goals.md) · was v0.2 · profile insights · check: blackbox, store

The shadow is off by default: a commission manifest without a `shadow` block, or with `shadow: off`, gets no shadow tasks, and `doctor` shows the check `commission.shadow_off` (D-103). A tenant turns it on with a `shadow` block; then for a share of proposals the fallback role of a lens (COM-7) gets a shadow review task of that lens on the `seat` slot with the lens seat profile (GW-7), without memory facts and without a persona; the shadow runs only for lenses whose review a seat holder executed. A proposal is selected when `sha256(seed ‖ proposal id ‖ lens)` falls below the share by the rule and test vector of `spec/hashing.md` ("shadow draw"); the seed is drawn monthly per tenant and written to audit (`commission.shadow_seed`, D-101). The `shadow` block sets `veto_lenses` (0.25 when the block omits it) and `other_lenses` (0.2 when omitted), each above 0 and at most 1; a share of 0 inside the block fails the lint `shadow_zero_without_off` at `apply`, because the shadow is turned off only by leaving the block out or by `shadow: off`. A shadow task gets its own core key `system:review:<proposal>:<lens>:shadow` (IDM-2) and does not merge with the seat's task. Its reviews have `shadow: true` (COM-9) and are paid from the `review` line. For each lens, code takes the pairs "seat score minus shadow score" of the last 30 days, drops pairs with a difference of 0, and sets the flag `shadow_divergence` when at least 15 pairs remain and a one-sided exact sign test gives p below 0.05 for "seat below shadow"; people see it in P2 `commission.status` and in `doctor` (`commission.shadow_divergence`).

<a id="COM-18"></a>
## COM-18. A stronger model for the most accurate member

serves [G5](../docs/goals.md) · was v0.3 · profile insights · check: lint, blackbox

With `commission.accountability: on` (GOV-36) the holder with the best accuracy (COM-17) over 90 days in the tenant, with at least 20 scored forecasts, gets two things for 90 days. (1) Its reviews of hard proposals run on the profile `reward_profile` of its seat in the charter: a proposal is hard when code marks it high risk, it touches a protected field, or it has `untrusted_seen` (the thresholds `high_risk` and `untrusted` of COM-8). The core issues for such a review attempt a grant of `kind: reward` on this profile for one attempt (MGR-18 ceilings apply; the slot is `granted`, GW-7), paid from the `review` line within `commission.reward_cap_eur_month` (0 by default, so there is no reward until people set an amount). The rank of `reward_profile` is no higher than the ceiling of the seat role's profile (GOV-3), and the reward is not applied if it would put the two veto lenses on one model family (GOV-34). (2) Protection from leaving by accuracy (GOV-43). The protection does not apply to departures for a confirmed link (GOV-42) and does not apply if without it nobody could leave. No bot context mentions the reward (GOV-46).

<a id="DIG-9"></a>
## DIG-9. A clear benefit of a rejected change reaches people as one line

serves [G5](../docs/goals.md) · was v0.3 · profile insights · check: blackbox

When a replay (PRP-25) gives `benefit` and no harm, the core adds to the tenant's digest the item `replay_item`: "the commission rejected it, the records say it would have helped", with the proposal in code words and the replay numbers, and the button "Send to decide" for people with `proposal.decide:<category>`. At most one such item per tenant per week, within the per-person card limit (APR-15); silence changes nothing. The button moves the proposal to `presented` on the ordinary path to people (PRP-4), without a new commission review; the author gets the usual credit if people accept it.

<a id="DLV-17"></a>
## DLV-17. Outgoing webhook sink

serves [G1](../docs/goals.md) · was v0.2 · profile core · check: adapter, lint, fixture

An outgoing webhook is an E2 sink with `driver: webhook`. Its address is the manifest `base` exactly: an `https://` address whose host is not a farm host and not a loopback, private, link-local, shared or unspecified address, never taken from a task body or model text; lint rejects a webhook sink without such a base or without `signing_key_ref` (code `webhook_sink_invalid`). At every send the sink resolves the host itself, refuses the send as `dead` when any resolved address is a farm address or in those ranges, and connects only to an address it checked, so a changed DNS answer cannot turn the sink to the farm or its cloud metadata service. Each send is an HTTP POST of the delivery as JSON with the Standard Webhooks headers: `webhook-id` equals `delivery.id` (FEN-7), `webhook-timestamp` is the send time in seconds, and `webhook-signature` is `v1,<base64 HMAC-SHA256>` over `<id>.<timestamp>.<body>` with the deployment secret named by `signing_key_ref`. A 2xx answer completes the delivery; a timeout, 429 and 5xx are retried as delivery failures up to `dead` (DLV-3); any other answer is `dead` at once; redirects are not followed. A retry keeps the id and gets a new timestamp and signature. The body carries file references and metadata, never file bytes. The sink keeps no copy, so the core closes its erasure steps itself with `not_applicable` and basis `declared_none`.

<a id="DSP-9"></a>
## DSP-9. Dispatch can be explained

serves [G5](../docs/goals.md) · was v0.2 · profile core · check: blackbox

P2 `dispatch.explain` shows, for each condition of the closed list, how many workers pass it and what place the task has in the dispatch order, and P2 `dispatch.queue` gives counters by class and fairness key. Both operations only read and return only metadata. Direct requests of people are counted as one row of kind `interactive`; neither operation returns a task body, goal or principal id, keeps a history or raises an alarm.

<a id="DSP-11"></a>
## DSP-11. Bot group quota and a rule's share

serves [G1](../docs/goals.md) · was v0.2 · profile core · check: fixture, lint

A bot group's `max_running` caps the running attempts on the group's member workers: `claim` does not hand a task to a member of a group whose members already run that many attempts, and a worker in several groups is held by each group that has a cap. A dispatcher rule's `max_share` caps the running attempts of the tasks the rule matches on the members of the rule's `group` at the share of the group's capacity, rounded down but at least one; the capacity is the group's `max_running`, or the number of its active members when it has none (as for BUD-8). `apply` rejects a rule with `max_share` and without `group` (`max_share_without_group`). A task held by either cap waits in `submitted` with `waiting_reason: group_quota` (D10); no attempt is spent and no work is lost (DSP-7). The core counts running attempts from the lease records in the `claim` transaction, so two concurrent `claim` calls never pass a cap together. P2 `dispatch.explain` shows the cap and how many places are taken (DSP-9).

<a id="GOV-35"></a>
## GOV-35. Locked commission

serves [G5](../docs/goals.md) · was v0.2 · profile governance · check: fixture, blackbox

A tenant's commission counts as locked when a flag holds steadily: the audit sample flag `commission_underrates` (DIG-5) is in three monthly digests in a row, or three cases in a row in the tenant closed with the same one of the case flags `identical_reasons`, `evicts_top_calibrated`, `evicts_upheld_veto`, `single_model_bloc` and `defaulted_majority` (GOV-19). For `commission_underrates` of a veto lens the basis names the seats of that lens, for a case flag all seats. Then the `governance` loop sends the tenant admins with `admin:governance` the card `commission_review` without buttons (`spec/model/decision-points.yaml`): the seats, the lens, the holders, the flag and code figures, and the ready commands `farmctl seat vacate <seat>` and `farmctl seat assign <seat> <bot>` (replacing the holder); the card is repeated once a month while the flag holds. `doctor` and P2 `commission.status` show "commission locked" (`commission_locked`) with the same basis. The card decides nothing and frees no seat: only a person changes the holder, through `farmctl` (PRM-14, IDN-10), and neither code nor a bot opens a case on this basis.

<a id="GOV-39"></a>
## GOV-39. The farm manager may only hint at a link

serves [G4](../docs/goals.md) · was v0.3 · profile governance · check: blackbox

A decision attempt of the farm manager seat may hint at a link with the built-in `link_hint` (closed fields: the incident and the proposal ids of its own tenant), at most 3 hints per tenant per week; a hint about a proposal whose author is the seat holder is refused with `own_proposal`. A hint creates nothing: people with `governance.attribute` see it in P2 marked "manager's hint, not checked", and only a person makes it a candidate through P2 `attribution.create` with `kind: rejected_proposal`, subject to the exclusions of GOV-38. The manager's input never contains the scores, the vetoes, who was against or for, or the state of links (GOV-46).

<a id="INS-5"></a>
## INS-5. Analysis to the role's bots in turn

serves [G5](../docs/goals.md) · was v0.1 · profile insights · check: fixture, store

The dispatcher hands out tasks of the purpose `insight` to the role's bots in turn (dispatcher condition D16): the run goes to the bot of the role with a ready worker that has gone longest without an analysis of this role, and on a tie to the bot with the smaller id. Besides the role's series, the analysis input contains the series of the bot's own attempts (by `acting_bot`) and the rows of its "My outcomes" (DIG-7). The bot whose attempt submitted the result is recorded as the proposal's author (PRP-14).

<a id="KNW-6"></a>
## KNW-6. Corpus search is a word search

serves [G1](../docs/goals.md) · was v0.2 · profile knowledge · check: blackbox

From v0.2 `kb_search` takes a corpus of the role's resource scope and a `query` in the format `search_query` (`kb-search-request`). The core splits the query into words by Unicode letters and digits, lowercases them, ignores words shorter than two characters and at most keeps the first 16; there are no operators, wildcards, regular expressions or phrase marks, and the core writes the query text into no answer, log or event. A fragment matches when it holds every word; matches are ordered by the number of word occurrences, then by file id and fragment number, at most `limit` (20) of them (`kb-search-response`). `kb_read` returns up to 20 fragments of one document from a fragment id in document order, only when the fragment's file belongs to the named corpus of the role's resource scope in the task's tenant; a fragment of another corpus or tenant, of a deleted file and a nonexistent one give the same `not_found`. Both answers carry the corpus trust (KNW-2).

<a id="LCY-5"></a>
## LCY-5. Plugin update and deprecation

serves [C9](../docs/goals.md) · was v0.2 · profile core · check: lint, blackbox

A plugin is updated without `blocked`: during the transition `source.accepted` lists up to two pairs (image digest, `tools/list` fingerprint), both matching the manifest's `tools`, and `hello` passes with either; an `apply` that removes the old pair makes a toolhost with the old image `blocked`. The model always sees the tool descriptions of the manifest. Deprecation and sunset of a plugin follow MAN-7.

<a id="MAN-7"></a>
## MAN-7. Deprecation, version removal and apiVersion change

serves [C9](../docs/goals.md) · was v0.2 · profile core · check: lint, blackbox

A manifest can carry `deprecated: true` and `sunset: <date>`; `apply` warns about references to a deprecated object, and after `sunset` rejects new and existing references to it. Task type `.vN` is removed only when it has no non-terminal tasks and no schedule or route points to it; otherwise the `apply` status shows an error and the type stays (MAN-2). When the manifest `apiVersion` changes (D-44), `farmctl` ships a converter whose output gives the same `apply --dry-run` plan; this part is checked from the release that changes `apiVersion`.

<a id="MAN-12"></a>
## MAN-12. The P1 OpenAPI description

serves [C9](../docs/goals.md) · was v0.2 · profile core · check: lint, blackbox

From v0.2 the P1 HTTP binding has an OpenAPI 3.1 description `spec/bindings/http/p1.openapi.json`, generated by `ci/gen_openapi.py` from the P1 rows of `spec/ports/operations.yaml`: one `POST /v1/p1/<operation>` per row, every body a reference to its `spec/model` schema, the headers and problem details of `spec/bindings/http/README.md`. It has no types of its own, so it changes only additively together with the schemas (MAN-3), and the skeleton CI fails when it does not match the map. A core serves exactly these paths under `/v1/p1/`.

<a id="MEM-5"></a>
## MEM-5. Task order within one conversation

serves [G1](../docs/goals.md) · was v0.2 · profile core · check: blackbox

A task type of `worker_kind: agent` declares `conversation_concurrency: serial | parallel`, default `serial`. With `serial`, one role has no more than one attempt in progress in one conversation: the core does not hand out a task of this conversation on `claim` while the same role has an attempt running in it, and hands out the tasks of that role in the conversation in enqueue order (dispatcher condition D17); a task held by another dispatch condition does not hold back the tasks behind it, and tasks of other roles in the conversation are not ordered against it; a task held back waits with `waiting_reason: conversation_busy`. A clarification from a person is a new task in the same conversation, and it waits its turn (D-41).

<a id="MGR-12"></a>
## MGR-12. Task pinning

serves [G4](../docs/goals.md) · was v0.1 · profile manager · check: store

`pin_task` enqueues a class 1 task through the `control` lane (DSP-5) in tenant T for the named worker. The type is taken from the class's `responses.pin_types` ∩ the permissions of group `farm-manager-ops` ∩ the `claims` of the worker's role; tier no higher than 1; no more than `pins_per_worker_hour` (3) per worker; chain depth is counted (PRM-6). Onto a quarantined worker only types with `pinnable: true` and mark `summary` are pinned, without `file_ref` and without tools with tenant data. This is the only task path with a worker field, and it belongs to the core: requests have no worker field (DSP-4). A pinning task gets key `system:control:<epoch>:<number>` (IDM-2).

<a id="MON-7"></a>
## MON-7. Bot notes on problems

serves [G5](../docs/goals.md) · was v0.2 · profile monitor · check: adapter, blackbox

A role whose manifest has `outputs: [monitor_note]` can return with a task result a note on a monitoring problem: the `eventid` of a problem on a node of its own tenant's catalog and a text of up to 1000 characters. The note goes in the optional `monitor_note` field of P1 `complete`, next to `result`; a refused note is dropped with its reason in the task journal and does not change the task outcome. The monitoring system is a third-party store read by the organization's people outside the tenant: with profile `monitor` it is named as a data recipient in the processing description (`recipients: monitoring`), the note's data class is no higher than `summary`, and the text passes the same redaction as outgoing data to third-party stores (INJ-4: ip, email, secret), the secret check as for an outgoing message (INJ-11; the INJ-12 leak check includes notes) and the tenant canary (TEN-5: the note text is part of the canary set). The core checks that the problem is on a node of this tenant and creates a delivery for E5; the adapter adds the note to the problem as a message marked `[botaru <bot_id>]`. A tenant gets no more than 10 notes per hour, extra ones are rejected with `service_rate_limited`. A note cannot acknowledge, close, change the severity of or silence a problem, and the farm account has no permissions for that (MON-4). Fixtures: the eleventh note in an hour is rejected and the task still succeeds; a note on a problem of a node of another tenant is rejected; a note delivery carries no acknowledgement attribute; a note with an email address and an IP goes out redacted; a note with data of a class above `summary` is rejected; the tenant canary in a note does not reach the mock monitoring system.

<a id="OBS-13"></a>
## OBS-13. The metrics catalog

serves [G5](../docs/goals.md) · was v0.2 · profile core · check: lint, blackbox

From v0.2 the core exports the metrics of `spec/bindings/telemetry/metrics.yaml` through the telemetry binding: each with the instrument, unit and labels of its entry, labels only from `label_sets`, and no `botaru.*` metric outside the catalog. The catalog holds only what the farm alone knows from its records; machine, disk, network and service metrics are left to the monitoring system (D-59), and no core decision reads a metric. A metric carries no id of a person, chat, worker or external system. The skeleton CI checks the catalog form; the black box checks that every catalog metric appears in the export of a mock collector after one task, and that no other `botaru.*` name does.

<a id="OBV-8"></a>
## OBV-8. P1 obs.append for job attempts

serves [G5](../docs/goals.md) · was v0.2 · profile insights · check: blackbox

From v0.2 P1 `obs.append` is accepted only under the lease of a `job` attempt (another holder or a stale token gets `fenced`) and only into a series from the `observes` of the attempt's task type (`type_not_allowed`). The value must be of the series kind (`input_schema_invalid`), and the subject must be a catalog object of the series subject kind in the task's tenant (`ref_invalid`). The core writes the point in the transaction of the call with the store time, the tenant and the producer `job` with the attempt; the body carries none of them. One attempt writes at most 1000 points, more get `service_rate_limited`. Points are only added; a point is never changed.

<a id="ORC-15"></a>
## ORC-15. Waves and pace of a campaign

serves [G4](../docs/goals.md) · was v0.2 · profile campaign · check: fixture, store

`rollout.waves` is a list of 1-10 cumulative shares of the items after the canary, strictly increasing, the last one `100%`; a proposal that breaks this is rejected with `campaign_waves_invalid` and creates no record. Wave k may release, in item order `n`, the items after the canary up to the share of wave k of their number, rounded up. Wave 1 opens when the canary gate opens (`after_canary`), or at once without a canary; wave k+1 opens only when every item of wave k is final, the failure threshold is not crossed and the `bake` pause has passed since the last of them ended. Without `waves` all items after the canary form one wave. `rollout.rate_per_min` caps the releases of the campaign in any 60 seconds. An item that a closed wave or the pace holds back stays `pending`: that is not a failure and spends nothing. The `campaign` loop and the end of an item release it later by the same gates. Waves and pace only narrow release; they never pass an item that another gate of ORC-4 holds. The campaign record keeps the number of the open wave (`wave`, 0 for the canary), written by the core in the transaction that opens the wave, and the progress card is updated then (ORC-10).

<a id="PRM-11"></a>
## PRM-11. A dry run without side effects

serves [G1](../docs/goals.md) · was v0.2 · profile core · check: blackbox

P1 `enqueue` with `mode: validate` passes all enqueue checks, but creates no record, does not take the idempotency key and reserves no budget; the core writes an audit line with a dry-run flag. For something foreign or nonexistent the dry run answers the same as a real enqueue. The answer (type, `max_tier`, `data_mark`, the decision and reason, the budget estimate) goes to the calling principal in the `validate` block of the response, not to the model. The built-in tool `enqueue_child` has no `mode` parameter, and a P1 `enqueue` with `mode: validate` from a caller that holds an attempt or lease token (a worker in an attempt) is refused with `forbidden`, so code next to a model cannot probe with it. A dry run counts against the enqueue limits of the tenant and the caller (`max_enqueue_per_hour`, CAP-1) like a real enqueue, so it cannot flood the audit.

<a id="PRP-17"></a>
## PRP-17. A late related proposal earns no credit

serves [G5](../docs/goals.md) · was v0.2 · profile insights · check: store, fixture

The core links a proposal with proposals of another author on which people decided in the last 90 days, if they have the same change kind and their targets or fields overlap (`related_to`). People and reviewers see the mark. If the related proposal was accepted or declined, the late proposal earns its author no credit and creates no promotion trigger, although the change itself can pass the commission, the people and the trial as usual. Proposals whose author is a role of profile `farm-manager` (the farm manager in its daily review, MGR-41, `origin.level` 2) are not part of the `related_to` of other authors' proposals and do not take credit away from them: the core computes relatedness only among proposals of functionaries and aggregators (`origin.level` 0 and 1).

<a id="PRP-25"></a>
## PRP-25. Replay of a rejected change over the history

serves [G5](../docs/goals.md) · was v0.3 · profile insights · check: fixture, blackbox

When the commission dismisses or parks by score a proposal of kind `manifest_change` that sets only fields marked `replayable: true` in `spec/model/proposable-fields.yaml` (alarm thresholds, schedule windows, limits), the core replays the change by code over the recorded monitoring and farm series of the last 30 days: which alarms and incidents of the classes with the proposal's problem code would have fired earlier, later or not at all, and which limits would have been hit. Nothing changes on the farm, no model takes part, and people see nothing. The result is one of `benefit`, `harm`, `none`, `mixed`, written into the proposal record, and code scores the forecasts of the commission by it (COM-16) as a trial outcome (`benefit` as `better`, `harm` as `worse`). A replay is not a trial: it gives no promotion trigger. Changes of code, permissions or new actions are never replayed.

<a id="RET-9"></a>
## RET-9. Tenant retention periods

serves [C6](../docs/goals.md) · was v0.2 · profile core · check: lint, fixture, blackbox

From v0.2 a tenant sets its retention periods in `Tenant.spec.retention`: `classes` for the classes that have `ceiling_days` in `spec/model/retention-classes.yaml` (`content`, `memory` and `metadata`), `files` (the file period) and `journals` (the attempt journal period, AUD-4). A class period lies between `min_days` and `ceiling_days` of its class (`content` 1 to 400 days, `memory` 30 to 3650 days, `metadata` 93 to 3650 days); `files` and `journals` are not longer than the tenant's `content` period. Lint rejects anything else with `retention_period`. A field the tenant leaves out takes the class default; `memory` without a tenant period lives until `forget` or an erasure request, `tombstone` has no period, and `observation` keeps its own periods (OBV-4). When a file is written the core sets `expires_at` to the write time plus the smaller of `retention.files` and the type's `result_ttl` if the file became a result (ART-1). A shorter period of `content`, `memory`, `files` or `journals` applied later reaches objects already stored at the next loop run; a shorter `metadata` period applies only to records written after the `apply` that set it, so a manifest change never deletes audit, decisions or spending already written (AUD-3, D-108); a longer period never extends an object that has already expired.

<a id="SBX-1"></a>
## SBX-1. Tier 2 lives only in the sandbox

serves [G2](../docs/goals.md) · was v0.2 · profile sandbox · check: lint, blackbox

A tool of tier 2 is only in a plugin with `placement: sandbox`, and such a plugin holds only tools of tier 2 with `access: read` and `target: any`: the sandbox has no write tools and no actions. A role that claims a type of tier 2 has no tool with `placement: host` in `tools`, because live closed data next to code and untrusted text would assemble the "lethal trifecta" in one task even without network. `apply` rejects a break of either with `sandbox_placement` or `sandbox_live_tools`. A tenant has at most one sandbox toolhost, on a sandbox VM (DEP-1, DEP-2). An installation that does not claim the `sandbox` profile has no tier 2: its core rejects at `apply` a type or a tool of tier 2 and a plugin with `placement: sandbox` (MAN-11), and a single server without a separate VM never claims the profile.

<a id="SBX-2"></a>
## SBX-2. One disposable container per task

serves [G2](../docs/goals.md) · was v0.2 · profile sandbox · check: blackbox, adapter

The sandbox toolhost starts one container for one task and never reuses it for another task or tenant. The container has no network and no secrets, a read-only root, no privileges, an unprivileged user and limits on memory, CPU, processes and file size. `/in` is mounted read-only and only the toolhost fills it: with a slice of a tenant catalog dataset whose class is not above the task's `data_mark`, after redaction and within the copy limit, and with the task's attachments read through P1 `file.get` under the call lease; the source never changes. `/out` is a separate volume with a limit. The output of a command goes to the model only after redaction and up to 32 KB, and the attempt gets `untrusted_seen` from the first read of file content (INJ-13); in the audit and events `argv` is only a hash and a length (AUD-2). Command time counts on the `sandbox_seconds` meter reserved before use (BUD-5). At the end of the attempt the toolhost stops the container first and then reads `/out`: it opens files without following links, takes only regular files, checks the type by the first bytes, hands out only CSV, JSON, GeoJSON, PNG and the model's script as plain text within the type's `result_media_types`, and writes each through P1 `file.put` with a mark not lower than the task's (ART-3); the script gets the mark "written by the model". Then the container and both volumes are destroyed.

<a id="SCR-12"></a>
## SCR-12. A script from git on a schedule

serves [G1](../docs/goals.md) · was v0.2 · profile campaign · check: lint, blackbox

A `kind: Script` manifest reviewed in the tenant's git can be started by a schedule (`Schedule.spec.script`, an optional field from v0.2) without a person's button only if it needs no `script.run` (SCR-6): no `action` step, no `fanout` with an action, no `confirm` step and no step whose role has `service_control` or `monitor_scripts`; it has no `await` step, so an external event cannot steer a run without a person; it keeps within `Tenant.spec.scripts` (SCR-5); and the run's permissions are those of `system`: every `task` and `fanout` type of the script is listed explicitly in `system-ops` (PRM-10). Otherwise `apply` rejects the schedule. The `deliver_to` of such a schedule is checked at `apply` and at every run, as for a task schedule (PRM-10). The run has initiator `system`, fairness key `script:<run>` and the slot rules of TIM-3; at most one run of a schedule is live, and a slot that comes while the previous run is live is skipped with `run_live`. Its estimate is reserved at each slot as at a run approval (SCR-8), and a slot that does not fit is skipped with `budget_exhausted` (TIM-3). A hold on the schedule stops it (CAP-4), and a hold on the run pauses it (SCR-10).

<a id="SCR-13"></a>
## SCR-13. The steps wait and await

serves [G1](../docs/goals.md) · was v0.2 · profile campaign · check: lint, blackbox

A `wait` step has only `for`, a duration. An `await` step has `event` (an event type of the events catalog, or an `ext.*` event of a source channel of the tenant whose data is closed by `ingress.mapping`, ING-2), `match` (1-5 pairs of an `event_field` and a value: a literal, `{ref: <step>.<field>}` to a closed result field with `x-botaru-read-by: code` (SCR-3), or `{param: <name>}` in a script from git, SCR-14) and a required `timeout`. `for` and `timeout` are each at most the run deadline. At release the core writes a waiting record and no task: there is no attempt and no lease, only the run record waits (CNT-4). For `wait` the timer ends the step `ok`. For `await` the first event of the same tenant whose fields all match ends the step `ok`, and the step result holds only the closed fields of the event data schema (OUT-1), which later steps read by reference; the timeout ends the step `failed(await_timeout)`, and the run goes on by `on_failure`. The end of such a step releases the next steps as any step end does (trigger `script`); a waiting record ends at most one step. A run whose script has a `wait` or `await` step has the deadline `Tenant.spec.scripts.wait_run_deadline` (7 days by default, the farm ceiling); other runs keep `run_deadline` (6 hours). A paused run (SCR-10) keeps its waiting records, and a step that ends during the pause releases nothing until the run resumes; a cancel or the run deadline ends the record and the step is `canceled`. `apply` and draft acceptance reject a `wait` or `await` step with missing or foreign fields or a duration above the deadline (`script_wait_fields`).

<a id="SCR-14"></a>
## SCR-14. A script from git with parameters and a free goal

serves [G1](../docs/goals.md) · was v0.2 · profile campaign · check: lint, blackbox

A `kind: Script` manifest reviewed in the tenant's git may declare `params`: at most 16 named parameters, each of type integer, number or boolean, or a string with `enum` or a format from `formats.yaml` (there is no free string, INJ-1), with an optional `default`. Steps use a parameter only as a whole value `{param: <name>}` in `params`, `args` and `await.match`. Only such a script may declare `goals`: free goal texts of at most 2000 characters, reviewed in git and used without substitution, which an agent step names with `goal: {template: <id>}`; a name may not repeat a goal template of the step's type. A draft from the model or an improvement proposal with `goals`, `params` or `{param}` is rejected at acceptance with `script_free_goal` (SCR-4, SCR-5). A person starts a script from git with a command from `Tenant.spec.script_commands` (`/<command> key=value ...`): the core checks each value against the declared parameter, a missing one takes its `default` and without a default the command is refused with `script_param_invalid`; then it accepts the script like a draft (SCR-5) with the person as the initiator and creates the run in `proposed` with the `script_confirm` card, and the button and its checks are those of SCR-6. The run record keeps the manifest name and its commit, and `plan_hash` covers the parameter values. A schedule starts a script from git only with the default values (SCR-12). `apply` rejects an undeclared or free parameter, a `{param}` that names no declared parameter, a goal name that is in neither list or in both, and a script command that takes a built-in or a tenant command (`script_param_invalid`, `builtin_command`).

<a id="SEC-7"></a>
## SEC-7. A person's connection to an external service

serves [G1](../docs/goals.md) · was v0.2 · profile connections · check: blackbox

A person's access to their external service is a recorded connection (`Connection`): the tenant, the principal, a provider from the tenant catalog, access scopes from a closed list, the state and an expiry. The person creates the connection themselves through the consent adapter, not the model; the deployment stores the token value, and it does not pass through P1-P4. The link to the adapter carries a nonce that the core issues at `/mydata connect`: single use, valid for 10 minutes, bound to the tenant and the principal. P2 `connection.register` must present it, and the core takes the principal from the nonce, not from the adapter. The request names the external account (`account_id`, the subject id at the provider); the core refuses an account that has an active or pending connection of another principal (`connection_account_taken`), generates `token_ref` itself and writes the connection as `pending`. It becomes `active` only after the person confirms the code card `connection_confirm` in their private chat; the card shows the provider, the external account and the scopes. A plugin declares `credential: per_principal(<provider>, <scopes>)`, and the core selects the connection by the task's initiator; the model neither names nor chooses the account. Tasks of other principals and of the service principal `system` do not use the connection. A write on behalf of a person remains a tier 3 action with an approval. Offboarding and `forget` revoke and delete the connection.

<a id="SEC-12"></a>
## SEC-12. Privacy report

serves [C6](../docs/goals.md) · was v0.2 · profile core · check: blackbox

P2 `tenant.privacy_report` collects per tenant: the processing purposes; the retention classes with periods and object counts, of them with `personal_seen`; the subprocessors with region, retention mode and agreement reference; channels with third-party storage and their ceilings; references to the impact assessment and the record of processing activities; the number of open and overdue requests by people; the number of content reads for incident review over the period. The report holds only metadata. Tenant people with `admin:people` or `audit.read` and the operator get it.

<a id="SKL-4"></a>
## SKL-4. Reading a skill through the core

serves [G1](../docs/goals.md) · was v0.2 · profile knowledge · check: blackbox

From v0.2 a run whose runtime declares `skills: farm` gets the built-in tool `skill_read` in its P3 catalog, only for profiles `worker` and `manager` and never in a `handover` task (PRF-1, MEM-7). Its arguments are closed: `name` by an enum of the role's pinned skills and `file` by an enum of that skill's files (`skill-read-request`); the answer is the file text from the core copy of the pinned tree, at most 32 KiB per call with `next_offset` for the rest (`skill-read-response`). A skill outside the role, or a tree whose hash does not match, is not readable (SKL-2). Skill text is reviewed git text and does not set `untrusted_seen`; it changes no tool, tier or data class (SKL-3).

<a id="STA-3"></a>
## STA-3. Two columns and a mismatch code

serves [G5](../docs/goals.md) · was v0.2 · profile messaging · check: fixture, blackbox

The `status.workers` view and P2 `status` show side by side the core observations (presence, worker lifecycle, holds, leases, `last_seen` age) and the last self-report with its source and time, plus a mismatch code from the closed list `state-mismatch.yaml`: `reported_ready_unreachable`, `reported_idle_not_claiming`, `reported_busy_no_lease`, `reported_draining_still_claiming`, `reported_plugins_differ`, `state_stale`. Core code computes the codes by a table of conditions; on a mismatch the core observation prevails. The self-report column carries only the phase from the closed list of `bot-state.schema.json`, its source and time; no other field of a worker's message reaches another bot's model context.

<a id="TRN-8"></a>
## TRN-8. Confirmed cases enter the translator check of the tenant

serves [G5](../docs/goals.md) · was v0.3 · profile campaign · check: eval, blackbox

A confirmed script link (TRN-7) becomes a check case of the tenant once a person wrote or approved the correct script for the assignment, or marked it `ambiguous` or `unsupported`. The case is a tenant record, not a git file: it holds the assignment text, lives by the task's retention period and is reached by a person's erasure request. Only the translator check of that tenant uses it. A new translator version (model profile or persona) is not enabled in the tenant until it passes all its cases; a deleted case leaves the check, and the check report says so. On the correctness set the share of `ambiguous` and `unsupported` answers is no more than `evals.translator_clarify_share_max` (0.2 by default), so that the translator does not ask about everything.
