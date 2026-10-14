# Lifecycle: delivery

## In short

A delivery (`Delivery`) is a core record "send this content to this recipient": a task answer, an approval card, a card update, a summary for a group chat, the `ai_notice` and `privacy_notice` texts (with `notice_version`, on first contact and on a version change, independently of `ai_disclosure`), a write to a sink, and deliveries for the monitoring adapter E5: `monitor_alarm` (a change of an alarm episode), `monitor_audit_head` (the head of the audit chain), `monitor_script` (a monitoring system script on a tenant node) and `monitor_note` (the code note `service_control_done`), from v0.2 also `monitor_note` with a bot's note on a problem. The core creates a delivery in the transaction of the transition that produces it (DLV-2), computes its target `target: {kind: chat | subject | sink | monitor, id}` from the address, and the delivery is sent by the channel worker (E1), the sink worker (E2) or the monitoring adapter (E5) registered for this target at `enroll`, which picks up the delivery through P1 `claim` as work of kind `delivery`; the worker does not compute the addressee itself (DLV-6). The address is taken only from a closed list (DLV-6), a group chat gets only a summary (DLV-15), and model text goes out only as a whole after `complete` (DLV-11). A delivery is handed out at least once (a `monitor_script` delivery at most once) and always ends in a terminal state; its failure does not touch the task (DLV-1, DLV-3).

## States

| State | Meaning | Terminal |
|---|---|---|
| `planned` | The delivery is created with the target `target` and waits to be sent: for a free channel, a retry pause, the `ai_disclosure.ack` mark for this recipient, and from v0.2 the channel pace, quiet hours, a digest or the card limit per person `attention.noncritical_per_day` (APR-15) | no |
| `sending` | A channel or a sink took the delivery under a lease and is sending it | no |
| `delivered` | The external system confirmed receipt; the record keeps the external message id | yes |
| `withheld` | The core or the channel did not send the content to this recipient by a rule: a mark above the ceiling, an unknown chat member, the recipient is inactive or has no right to the mark, a hold, a secret in outbound. Instead of the content the person gets a ready code text if possible | yes |
| `dead` | The delivery attempts ran out; an event without content, the record is visible among the "dead", P2 `delivery.redrive` is possible | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `planned` | the core in the transaction of a transition: a terminal state of a task, creation and transitions of an approval, a change of an alarm episode, a service request `via: monitor` or `monitor/script_run` with a `service_grant` record, a code note about an action on a service, a bot's note on a problem (v0.2), a route, a schedule slot | the address is from the closed list: `origin`, `group:`, `chat:`, `sink:`, `approval_initiator`, the monitoring adapter E5 (only for alarms, scripts and notes, without people); every human recipient is active and has `data:<mark>`; campaign items do not create their own deliveries | DLV-2, DLV-6, MON-1, MON-7, MON-8, MON-10, ORC-10 |
| (none) | `planned` | the core for a request from a group chat | only a summary goes to the chat: the `summary_fields` table, ready code texts and, if allowed, model text of up to 800 characters; the full answer as a separate delivery to the asker in a direct chat, and "details sent privately" to the chat | DLV-15, REP-9 |
| (none) | `planned` | the core before the first model text to a recipient | the channel driver declared `ai_disclosure: first_contact`; a delivery of the code text `ai_notice` on the first contact of a principal in the channel and in every newly bound chat, and again on the next incoming message from the principal if the previous `ai_notice` ended as `dead` | DLV-12 |
| (none) | `planned` | the core on the first contact of a principal and on a change of `notice_version` in the tenant's `privacy` block | a delivery of the code text `privacy_notice` with `notice_version`; it does not depend on `ai_disclosure` and the `native` mode | SEC |
| `planned` | `planned` | the core (waiting) | model text waits while the recipient has no `ai_disclosure.ack` mark (the time of the `ai_notice` delivery); a retry pause; from v0.2 the channel pace, quiet hours, a digest, and for a card of a non-urgent decision the limit per person per day: it waits until the next day in order of expiry (APR-15). Waiting does not spend an attempt | DLV-12, CAP-3, DLV-10, APR-15 |
| `planned` | `sending` | P1 `claim` with the kind `delivery` from the worker registered for the delivery's `target` | the ceiling of the binding and the channel, the chat membership and the recipient's rights are checked again; for a file delivery the file's mark is not above the recipient's right; the channel worker takes the credentials of the external system from the deployment by name, not from the chat binding | DLV-6, DLV-7, PRM-3, ART-3, FEN-1 |
| `planned` | `withheld` | the core at hand-out or in the transaction of the cause | the mark is above the channel ceiling (`channel_ceiling`) or the binding ceiling; the recipient is inactive or has no `data:<mark>`; the channel reported an unknown chat member; the `ai_notice` to this recipient ended as `dead` (`withheld(ai_notice_dead)` only for waiting deliveries: the next contact gives a new `ai_notice`); a hold with `no_delivery` on the scope. For an answer to a group chat the core creates a delivery to the asker in a direct chat and a ready text to the chat | DLV-6, DLV-7, DLV-12, SEC-9, HLD-3 |
| `sending` | `delivered` | P1 `complete` with a receipt from the external system | the record keeps the external id; for a card it is needed for `card_update`, which edits the sent message instead of sending a new one | FEN-7 |
| `sending` | `withheld` | P1 `fail` of the channel with a rule reason | a secret in outbound (`secret_in_outbound`): the message is not sent, admins get an event without content; an unknown member in the chat | INJ-11, DLV-7, HLD-6 |
| `sending` | `planned` | P1 `fail` with a retryable reason; the `reaper` loop | not `monitor_script`; delivery attempts remain; a retry with a pause; a lease that expired during core downtime does not spend a delivery attempt | DLV-3, FEN-5 |
| `planned`, `sending` | `dead` | the `delivery` loop; P1 `fail` with a non-retryable reason | the attempts ran out (`attempts_exhausted`) or the external system rejected the delivery for good; for `monitor_script` the lease expired without an outcome (`exec_unknown`), there is no retry; an event without content; visible in P2 `deadletter.list` | DLV-4 |
| `dead` | (no transition) | P2 `delivery.redrive` | a new delivery is created with a link to the dead one; ceilings and rights are checked again | DLV-4 |
| `planned` | `delivered` | the core when sending a digest | v0.2; a notification collected into a digest by the recipient's preferences ends together with the digest delivery, the record keeps the digest id; task results and cards are not collected into a digest | DLV-10 |

## Invariants

- The delivery address is taken only from the closed list; there are no addresses in the task body, in the result, in bot messages and in event data; child tasks and continuations inherit the address (DLV-6).
- Only a summary goes to a group chat, and content never goes to the chat instead of a direct chat (DLV-15, D-48).
- A group chat gets deliveries only after it is bound by the operator; cards and a ceiling above `summary` only with `membership_assurance: enforced` (DLV-7).
- Model text goes out only after `complete`, the result check and the secret search; there is no streaming output (DLV-11).
- Every part of a delivery carries `dataorigin`; the channel passes the mark the way the driver declared, and the sink writes `ai_generated: true` into the metadata of a file with model text (DLV-13).
- While the recipient has no `ai_disclosure.ack` mark, deliveries with model text do not go to them; `dead` of an `ai_notice` does not make the recipient mute forever: on their next incoming message the notice is created again, and `/about` repeats it on request (DLV-12).
- A delivery carries the target `target` computed by the core from the address and is handed out only to the worker of this target (DLV-6).
- A sink writes idempotently by `delivery.id`, an outgoing webhook passes it in the `webhook-id` header; repeating a delivery to a channel without a key is allowed (FEN-7).
- Outbound goes out without link previews and markup, links outside `link_allowlist` are cut out (INJ-11).
- A delivery failure does not restart the task and does not change its state (DLV-3).
- A person's preferences only postpone a delivery; they do not change the recipient and the ceiling and do not delay results and cards; alarms are delivered to people by the monitoring system, not by the farm (DLV-10, MON-1).

## Versions

| Version | What appears |
|---|---|
| v0.1 | all states; the closed address list, including `approval_initiator`; the target `target` in the delivery record; splitting the answer for a group chat; `ai_notice`, the `ai_disclosure.ack` mark, repeating the notice after `dead` and waiting in `planned`; `privacy_notice` with `notice_version`; code texts by the `locale` of the principal and the chat (`ru`, `en`); `dataorigin`, also in the artifact's provenance block (PRV-2); `card_update`; `withheld` by the ceiling, the chat membership, the rights, a secret in outbound; after the core and the first bot (D-71, D-80): `withheld` by a hold with `no_delivery`; the deliveries `monitor_script` (at most once, the outcome through P1 `complete`) and `monitor_note` with the code note `service_control_done`; the auto-stop "secret in outbound"; the deliveries `script_confirm`, `script_summary`, the campaign and incident cards |
| v0.2 | the channel pace; quiet hours and the notification digest; the `locale` of a schedule and a tenant, other languages; the outgoing webhook as a sink kind |
