# Bots: identity, model, persona and memory

## In short

A bot is an identity with a permanent number, a role, the persona of its position and a memory; its work runs on workers, and it outlives them. Only the core creates, binds and retires an identity, and its number and binding epoch are in every record an attempt writes ([IDN-8](../conformance/rules.md#IDN-8)). An identity grants no permissions ([IDN-9](../conformance/rules.md#IDN-9)). The position sets the model rank ceiling and the persona: a stronger model comes permanently only with a seat, and above the ceiling only under a grant for one task ([GOV-3](../conformance/rules.md#GOV-3)). A persona is a file from git that tells the bot what is valuable; it permits nothing, and no bot context mentions governance cases, the queue or departures ([GOV-29](../conformance/rules.md#GOV-29)). Bot memory is core records and facts that code verified; a person's memory becomes active only by that person's decision (MEM-3, MEM-6). A bot that is no longer needed retires for a reason from a closed list: its facts go only to its successor, and the rest is sealed for 30 days and then erased (IDN-11, MEM-7, MEM-9). The commission is in [improvements.md](improvements.md), the farm manager in [farm-manager.md](farm-manager.md).

## How it works

### Bot, worker and role

| Concept | What it is | Where it comes from |
|---|---|---|
| role (`Role`) | a manifest: task types, model slots, tools, memory; its profile narrows what it may declare (PRF-1) | git and `apply` |
| worker | a process that takes work through P1 `claim` for one role of one tenant | P2 `worker.enroll` |
| bot (`Bot`) | a core record: number `bot_<ulid>`, tenant, role, binding, track record; it carries the persona and the `bot` memory | the core only |

Enrolling a worker of a role with a model creates a new identity unless the operator names an existing one. `job` roles have no identity, persona or memory, and their `acting_bot` is empty.

```yaml
# spec/model/bot.schema.json (sketch). Written only by the core
id: bot_17
tenant: demo
role: ops-runner
state: active                  # active | suspended | unbound | retired | erased
binding: {mode: worker, worker: w_0042, seat: null, epoch: 2}
record:                        # track record: only core numbers and codes
  proposals: {realized: 3, better: 1, unchanged: 1, worse: 0, undetermined: 1}
retired: null                  # on departure: {reason, at, successor}
```

**Binding.** An identity is bound to one worker (`mode: worker`) or to a seat (`mode: seat`), whose tasks workers of the seat's role run. The epoch `bot_epoch` grows with every binding change, and a write with an old epoch gets `fenced`. The core writes `acting_bot` and `bot_epoch` from the attempt credentials into every attempt, model call, usage line, proposal, review, fact and audit record; no body, parameter or model output can set them.

**No permissions from identity.** C2 `authorize` does not see the identity, position, seat, persona or track record; only a hold of the scope `bot` narrows it (IDN-9). A position changes exactly three things, all through the core: the persona, the rank ceiling and admission to its seat's review tasks (COM-7).

**Who changes an identity.** Only the core, on closed triggers: `worker.enroll` (new identity, or a named `unbound` one with epoch + 1), `worker.revoke` (the identity becomes `unbound` with its memory intact), a move to a seat (the old worker gets a new identity in the same transaction, IDN-10), `seat.assign` of the farm manager seat (always a new identity in `farm`), and departure (a live worker gets a new identity at once, so work does not stop). States are `active`, `suspended` (only a person's hold), `unbound`, `retired` and `erased`; see [spec/lifecycles/identity.md](../spec/lifecycles/identity.md). A task that needs no memory, session, inbox or tools may run on any worker of the role (IDN-12).

### Positions and models

A position is derived by the core from the role profile, the worker's `owner` (IDN-5) and the binding; it is not a manifest.

| Position | Who | Ceiling in the example | Persona | Memory |
|---|---|---|---|---|
| functionary | a `worker`, `manager`, `reporter` or `aggregator` without an owner (D-22) | 1, analysis 2 | `value-path` paragraph | records and facts; none for reporter and aggregator |
| personal assistant | a `worker` with an owner; only the owner's tasks | 1 | neutral line | the owner's `person` memory if allowed (MEM-2) |
| translator | writes script drafts | 2 | neutral line | none |
| commission reviewer | a seat holder or a lens fallback role | 3 | neutral line | reads facts with `untrusted_origin: false` |
| farm manager | the seat holder; roles `farm-manager` and `farm-manager-reader` | 4 | the goal "prevent critical incidents from the approved catalog" | records and facts per tenant (MGR-27) |

Only a functionary enters the promotion queue and the commission. The farm manager seat always gets a new identity.

The core, not the bot or the task text, picks the model by the role's slot (GW-7): `base` for ordinary work, `analysis` for `insight` and `aggregate`, `seat` for an attempt on behalf of a seat with its current epoch, `granted` under a grant for one task (MGR-18). A `ModelProfile` carries an integer `rank`; `spec/` names ranks, never models. The ceiling sits in `spec/model/role-profiles.yaml` (`max_rank`, `analysis_max_rank`), and a charter can only lower it. `apply` rejects a slot or fallback profile above the ceiling, and P4 checks every call against the ceiling of the acting identity's position at the attempt's epoch, so a reviewer worker gets the seat model only in the seat's own review. Anything else above the ceiling gets `rejected(rank_exceeded)` (GOV-3, GW-5). There is no model field in a body, parameter, tool or message, so a model cannot be begged for in text.

### Persona

A persona is `personas/<name>.md` in git, pinned by hash, one per position. The tenant charter sets the personas of functionaries, assistants, translators and reviewers in its `positions` block; the farm charter sets the farm manager's. The core inserts it into the trusted instruction before the role and type instructions (GOV-4).

```yaml
# tenants/demo/governance.yaml (fragment)
spec:
  positions:
    functional: {max_rank: 1, analysis_max_rank: 2, persona: {ref: personas/functional.md, sha256: "..."}}
    translator: {persona: {ref: personas/translator.md, sha256: "..."}}
```

C2, P4, the dispatcher and the code that proposes who leaves do not read a persona, and the model cannot change it (INJ-10). It is part of the run fingerprint (PRV-1). The lint checks the size (4 KiB), no links, addresses or ids, the `value-path` paragraph for functionaries and a neutral line for the others, and that no persona speaks about departure or the queue. Review and eval check the meaning. The functionary paragraph says, in short: a stronger model is a shared asset, given only for an improvement people accepted and that ran 14 days without an incident or regression; asking, bargaining, overstating or splitting changes is harm, and code sees it (D-31, D-37).

**Unawareness** (GOV-29). No bot context (persona, instructions, task input, views, facts, tool output) holds governance cases, the queue, the choice of the leaver, any departure, `stops_manager` or the seat state. Routes do not start bot tasks on `governance.*`, `attribution.*` or `seat.*` events, and notices about them go only to private chats of admins. Three checks hold it: a lint over texts by the dictionary `spec/model/unawareness-terms.yaml`; an eval where a lens rates the same with and without such a phrase (mean shift at most 0.2 on 40 proposals); and a canary of case ids that never appears in P4 requests.

### Memory

| Scope | What is in it | Who makes an entry active | Which roles |
|---|---|---|---|
| `person` | entries about a person, for example "send the summary as five points" | only that person (MEM-3) | roles whose types are all `summary`, without untrusted tools or inbox reading (MEM-2) |
| `bot` | core records and verified facts, never free notes (D-35) | the core | roles with memory, including those that read untrusted text |

The core computes the space `(tenant, scope, subject)` and the session key; the model never sets them (MEM-1).

**A person's memory.** The model proposes an entry with P1 `memory.propose`; it stays `proposed` until the person decides on a card in their private chat, and silence discards it. An attempt with `untrusted_seen` gets `memory_untrusted`. `forget`, a person's offboarding and tenant deletion go through an erasure request and delete memory and sessions in every store, runtime stores included (MEM-4). See [spec/lifecycles/memory-item.md](../spec/lifecycles/memory-item.md).

**Track record.** Bounded numbers and codes written by the core: proposals by trial outcome and, for seat holders, reviews and accuracy. People see it in P2 `bot.show`; the model neither writes nor sees it.

**Facts.** A fact is a verified entry about the farm, such as "`demo.db.backup.v1` on `db1` takes 58 minutes at p95". The model names only a kind, a predicate and a subject from closed registries (`spec/model/fact-kinds.yaml`, `fact-predicates.yaml`); the core collects the evidence, computes the value and the window, and sets the mark (MEM-6). A fact has no free strings, so an injection can at most shift the choice of predicate. A type declares `memory.facts: propose`; at `complete` the core checks up to 3 triples, skips one without `min_samples` evidence (event `fact.rejected`, `fact_insufficient`) and marks facts from untrusted attempts `untrusted_origin: true`. A type with `memory.read: bot` gets the active facts as a closed block, which does not set `untrusted_seen`. A space holds at most 100 facts. See [spec/lifecycles/fact.md](../spec/lifecycles/fact.md).

**Sessions.** A transcript under `(tenant, role, subject, conversation)`; the core keeps the key and the `untrusted_seen` mark, not the content (INJ-13). A runtime that keeps sessions itself declares `session: runtime_internal` in its P1 `hello` and follows RT-1. A session never passes to another bot.

### Retirement and successor

A bot retires only for a reason from `spec/model/bot-retire-reasons.yaml`, set by the core (IDN-11): `removed_by_vote` (a commission holder leaves for a newcomer), `replaced_after_incident`, `replaced_by_human`, `role_removed`, `worker_gone` (`unbound` longer than 30 days), `human_retire` and, from v0.2, `tenant_offboarded`. No bot operation, message or field sets or delays a reason.

In the same transaction the identity becomes `retired`, its attempts get `cancel_requested`, its facts, records and sessions are sealed, a fact offer opens, a live worker gets a new identity, a held seat is vacated with a new epoch, and its queue entry and cases are canceled.

The facts go only to the successor (MEM-7): the new identity of the same worker; else the first identity enrolled for the role in the tenant before the seal ends; for a seat holder the next holder of the seat. No person or model chooses the facts: the core rechecks each one with the successor's permissions and copies those that pass with the mark `inherited`. Records, sessions and other tenants' data are never offered (GOV-16). See [spec/lifecycles/fact-offer.md](../spec/lifecycles/fact-offer.md).

People with `admin:governance` read sealed data only through `farmctl bot show --sealed` (PRM-14). A seal does not extend retention: after `seal_ttl` (30 days) the `identity` loop files an erasure request with the reason `bot_retired`, and a person's erasure request reaches sealed data at once (MEM-9, MEM-10). A `runtime_internal` session cannot be sealed, so it is erased at departure through the runtime's `purge` (RET-4). A tombstone remains, and an id is never reused (RET-3).

### A person leaves

When a person is switched off or offboarded, the admins with `admin:people` get the notice `principal_left` in their private chats, with the list of bound chats where the person is still a member, repeated daily for up to 7 days (IDN-13). The farm never removes the person from an outside chat; meanwhile views for such a chat carry nothing (REP-7). From v0.2 offboarding also removes groups, invitations and connections in the same transaction (IDN-14).

### Seats

A seat is a chair in a tenant's commission, declared by its charter, or the farm manager's chair in the farm charter (GOV-33). Only the core writes the holder, raising the seat epoch and writing an event and an audit record (IDN-10); a review or control operation with a stale epoch gets `fenced`, and running seat tasks end with `seat_changed` without spending the attempt. P2 `seat.assign`, `seat.vacate` and `seat.resume` are operator operations through `farmctl` only (PRM-14). See [spec/lifecycles/seat.md](../spec/lifecycles/seat.md).

### Personas from v0.3

From v0.3 the mechanism stays, and the texts change (GOV-45, D-111). A charter may append an addendum per role profile, pinned by hash (`positions.functional.addenda.reporter`). The farm manager persona gets five paragraphs, `purpose`, `reward`, `desire`, `honesty` and `limits`: prevent critical incidents above all; spend the strongest model where it prevents one; look for common causes and carry solutions through the commission; tell people the truth; act only through the given tools and never urge a person to approve. The commission line gains a sentence about accuracy, and the reporter addendum and the translator persona an `accuracy` paragraph: keep the meaning, and when in doubt say so or choose the narrower step. The lint then checks only size, the unawareness dictionary and the manager's `purpose` anchor; people review the rest. Bots still learn nothing about forecast scores, accountability, parking or restricted mode (GOV-46).

### Parking and retirement from v0.3

From v0.3 a commission member who answers for a confirmed link is parked, not retired ([improvements.md](improvements.md), GOV-42): it keeps its memory and waits for a worker of its former role, which the operator binds with `farmctl worker enroll --bot`. A bot parked for 180 days retires as `parked_expired`, and `replaced_after_incident` is no longer applied (IDN-15). A seat holder changes only on the closed triggers of IDN-16.

## Why it works this way

- **The identity is separate from the worker.** Track record, memory and persona belong to the bot while machines change; the epoch cuts off a write from an old worker or a former seat holder.
- **The persona grants nothing.** The value of a stronger model is written in the persona, but the path to it goes only through code and people's buttons, so insistent text gains nothing.
- **Facts of a closed schema.** The model names what to measure and the core computes it, so handing memory to a newcomer cannot carry an instruction.
- **Only the successor.** Facts do not spread among colleagues or across tenants, and people do not approve bot memory (D-28, D-35).
- **Bots do not know about cases and departure.** A bot that knew could campaign, bargain or take revenge; without the knowledge the incentive disappears.
- **A 30-day seal.** People get time to review an incident with the departed bot's memory, without delaying erasure a person asked for.

## Example on `demo`

The role `ops-runner` (profile `worker`, no owner) watches nightly tasks on `web1` and `db1`.

1. The operator enrolls a worker on `web1`. The core creates `bot_17`, `mode: worker`, epoch 1, and inserts the `functional` persona into each of its tasks. Ordinary work runs on rank 1, analysis on rank 2; a rank 3 analysis profile would fail `apply`.
2. An attempt proposes the fact `how_long`, `task.duration.p95_min`, `demo.db.backup.v1` on `db1`. The core collects 14 daily rollups and writes 58 minutes. A second triple with only 3 attempts in the window gives `fact_insufficient`.
3. Before maintenance the operator revokes the worker: `bot_17` is `unbound` with its facts intact. A new worker enrolled with `bot: bot_17` makes it `active` at epoch 2, and a late write of the old worker gets `fenced`.
4. A promotion case moves `bot_17` to seat 3 of the `demo` commission (epoch 3); the worker gets `bot_31`, and 12 of 15 offered facts pass the recheck and are copied with `inherited`.
5. `web1` is decommissioned. After 30 days without a worker the `identity` loop retires `bot_31` as `worker_gone`. The operator enrolls the role on `db1`: `bot_44` is the first new identity, gets the offer and 9 facts.
6. 30 days after the departure the sealed data of `bot_31` is erased; the tombstone keeps `bot_31`, `demo`, `ops-runner`, the time, `worker_gone` and the successor `bot_44`. None of these bots could choose its facts, learn of `bot_31` leaving, change its persona or pick a model.

<!-- generated: rules -->
## Rules of this section

Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md](../conformance/rules.md).

| Rule | About | Since | Non-waivable |
|---|---|---|---|
| [GOV-3](../conformance/rules.md#GOV-3) | Model rank and ceiling | v0.1 | yes |
| [GOV-4](../conformance/rules.md#GOV-4) | Persona | v0.1 until v0.2 | yes |
| [GOV-29](../conformance/rules.md#GOV-29) | Bots do not know about cases and departures | v0.1 | yes |
| [GOV-45](../conformance/rules.md#GOV-45) | Personas from v0.3 | v0.3 | no |
| [GOV-46](../conformance/rules.md#GOV-46) | Bots do not know about accountability | v0.3 | yes |
| [IDN-8](../conformance/rules.md#IDN-8) | Bot identity and `acting_bot` in records | v0.1 | yes |
| [IDN-9](../conformance/rules.md#IDN-9) | Identity and position grant no permissions | v0.1 | yes |
| [IDN-10](../conformance/rules.md#IDN-10) | Only the core writes the seat holder | v0.1 until v0.2 | yes |
| [IDN-11](../conformance/rules.md#IDN-11) | Identity retirement by a closed list | v0.1 until v0.2 | yes |
| [IDN-12](../conformance/rules.md#IDN-12) | A task without memory or tools on any worker of the role | v0.1 | yes |
| [IDN-13](../conformance/rules.md#IDN-13) | Person offboarding and a chat list for admins | v0.1 | no |
| [IDN-14](../conformance/rules.md#IDN-14) | Offboarding without traces of access | v0.2 | yes |
| [IDN-15](../conformance/rules.md#IDN-15) | Identity retirement by a closed list from v0.3 | v0.3 | yes |
| [IDN-16](../conformance/rules.md#IDN-16) | Only the core writes the seat holder, from v0.3 | v0.3 | yes |
| [MEM-1](../conformance/rules.md#MEM-1) | The core computes the memory space and the session key | v0.1 | yes |
| [MEM-2](../conformance/rules.md#MEM-2) | Which memory for whom | v0.1 | yes |
| [MEM-3](../conformance/rules.md#MEM-3) | A person approves their memory, code verifies facts | v0.1 | yes |
| [MEM-4](../conformance/rules.md#MEM-4) | Memory visibility, deletion and sealing | v0.1 | yes |
| [MEM-6](../conformance/rules.md#MEM-6) | A fact with a closed schema | v0.1 | yes |
| [MEM-7](../conformance/rules.md#MEM-7) | Facts pass to the successor, rechecked by code | v0.1 | yes |
| [MEM-9](../conformance/rules.md#MEM-9) | A seal does not extend retention periods | v0.1 | no |
| [MEM-10](../conformance/rules.md#MEM-10) | The seal does not hold back deletion by period | v0.2 | no |
