# Bots: identity, model, persona and memory

## In short

A bot on the farm is an identity with a permanent number, a role, the persona of its position and a memory, and its work runs on workers. Only the core creates, binds and deletes an identity, and its number with the binding epoch is in every record an attempt writes (IDN-8); an identity grants no permissions (IDN-9). The bot's position determines the rank ceiling of its model and its persona: a stronger model is given permanently only by a commission seat or the farm manager seat, and above the ceiling a bot works only under a grant for one task (GOV-3). The persona is a file from git that tells the bot what is valuable; it permits nothing, and no bot context contains governance cases, the queue or a mark of the bot's departure (GOV-4, GOV-29). A bot's memory consists of core records and facts about the farm that code has verified; only the person themselves makes a person's memory active (MEM-2, MEM-3, MEM-6). A bot that is no longer needed leaves for good for one of seven reasons: its verified facts are offered only to its successor, and everything else is sealed for 30 days for people to review and then erased (IDN-11, MEM-7, MEM-9). How a functionary gets into the commission is described in [commission.md](commission.md), and the work of the farm manager in [farm-manager.md](farm-manager.md).

## Bot, worker and role

Three concepts hold different things, and they are easy to confuse.

| Concept | What it is | Where it comes from | Where it is described |
|---|---|---|---|
| role (`Role`) | a manifest from git: which task types the bot takes (`claims`), model slots, tools, memory. The role profile (`Role.spec.profile`) from a closed list narrows what the role may declare (PRF-1) | git and `apply` | [task-language.md](task-language.md), [scripts-and-translator.md](scripts-and-translator.md) |
| worker | a process on a machine that takes work through P1 `claim` on behalf of one role of one tenant | P2 `worker.enroll` | [spec/lifecycles/worker.md](../spec/lifecycles/worker.md) |
| bot, or identity (`Bot`) | a core record: the number `bot_<ulid>`, the tenant, the role, the binding and the track record. The identity carries the persona of its position and the memory of the scope `bot`, and workers execute its tasks | the core only | this document, [spec/lifecycles/identity.md](../spec/lifecycles/identity.md) |

Every worker of a role with a model has an identity: P2 `worker.enroll` for such a role creates a new identity unless the operator named an existing one. Roles without a model (`job` roles, for example a scheduled database backup) have no identity: they have neither a persona nor a memory, and the `acting_bot` field in their records is empty.

An identity record in the examples looks like this (numbers shortened):

```yaml
# spec/model/bot.schema.json (sketch). Written only by the core
id: bot_17
tenant: demo
role: ops-runner
state: active                  # active | suspended | unbound | retired | erased
binding: {mode: worker, worker: w_0042, seat: null, epoch: 2}
created_at: "2026-11-02T09:00:00Z"
record:                        # track record: only core numbers and codes
  proposals: {realized: 3, better: 1, unchanged: 1, worse: 0, undetermined: 1}
  seats: []
retired: null                  # on departure: {reason, at, successor}
```

**Binding.** An identity is bound either to one worker (`mode: worker`) or to a seat (`mode: seat`). One identity holds at most one worker, and a worker has one identity at any moment. An identity on a seat has no worker of its own: workers of the seat's role execute its tasks. The binding epoch `bot_epoch` grows with every change of binding, and an attempt's write with an old epoch gets `fenced` without side effects. P2 `worker.rotate` changes only the worker token; the identity and the epoch stay.

**Where the number appears.** Every attempt, model call, usage line, proposal, review, ballot, fact, stored message record (the sender's bot identity; the field is not part of the message envelope) and audit record carries `acting_bot` and `bot_epoch` (IDN-8). Only the core writes these fields, from the attempt's credentials; the task body, the parameters, a message and the model output neither set nor change the identity. The fields exist in the record schemas since v0.1 and are empty in records made without a bot identity; the author of such a proposal is set by the worker (PRP-14).

**An identity grants no permissions.** The input of the permission check C2 `authorize` contains no identity, position, seat, persona or track record; there is one exception: a hold of the scope `bot` narrows permissions (IDN-9). The attempt permissions are the permissions of the worker's role intersected with the permissions of the initiator's groups, as for any task ([permissions.md](permissions.md)). A position changes exactly three things, all through the core: the persona (GOV-4), the model rank ceiling (GOV-3) and admission to tasks with the purpose `governance` (GOV-7); none of them is part of the permission check. So the `authorize` permission table is the same for an identity of any position.

**Who creates and changes an identity.** Only the core, from a closed list of triggers:

| When | What the core does | Rules |
|---|---|---|
| P2 `worker.enroll` for a role with a model, without the `bot` parameter | creates an identity with binding `mode: worker` and epoch 1 | IDN-8 |
| P2 `worker.enroll` with the `bot` parameter (`admin:workers`) | binds to the new worker an identity in `unbound` of the same role in the same tenant, epoch + 1 | IDN-8 |
| P2 `worker.revoke` | an identity without a new binding becomes `unbound`, the memory stays intact | IDN-8 |
| promotion of a functionary or its placement on a commission seat | the identity moves to the seat (`mode: seat`, epoch + 1), and its worker gets a new identity of the same role in the same transaction | IDN-10 |
| P2 `seat.assign` of the farm manager seat | always creates a new identity in the service tenant `farm` | IDN-10, MGR-1 |
| departure of an identity that has a live worker | the worker at once gets a new identity of the same role, and work does not stop | IDN-11 |

Identity states: `active` (gets tasks of its role or its seat), `suspended` (a human's hold of the scope `bot` is on it: no tasks, reviews or ballots), `unbound` (the worker is revoked, there is no new binding), `retired` (the identity has left, only erasure follows) and `erased` (a tombstone remains). Only a hold set by a human puts an identity into `suspended`; a hold by the farm manager seat holder on the same bot (MGR-9) delays its tasks but does not change the identity state, the admission to promotion or the vote. All transitions are collected in [spec/lifecycles/identity.md](../spec/lifecycles/identity.md).

**A clean-context task on another worker.** A ballot and the fact selection `memory.pick.v1` run without memory, session, inbox and tools, so the core may hand such a task to any worker of the identity's role in its tenant; `acting_bot` stays this identity, and the executing worker is written into the attempt and the audit (IDN-12). For the farm manager seat in tenant T, the review of the farm manager's lens and the ballot are executed by a worker of T's reviewer role on the `seat` slot, and the fact selection for T's section by a worker of the `farm-manager` role in T. A task with memory, session, inbox or tools goes only to the worker of the identity's binding with the current binding epoch (dispatcher condition D20 in [coordination.md](coordination.md)).

## Positions

A position is not a separate manifest. The core derives it from the role profile, the worker's owner and the identity's binding: the owner is the `owner` field of the worker record, which P2 `worker.enroll` writes (IDN-5); a role has no owner flag. The position selects the persona (GOV-4) and the model rank ceiling (GOV-3) and decides whether the identity is admitted to `governance` tasks (GOV-7).

| Position (key in the charter) | Who | Role profile | Rank ceiling in the example | Persona | Memory |
|---|---|---|---|---|---|
| functionary (`functional`) | a working bot, manager, reporter, aggregator: a bot whose worker is enrolled without an owner (D-22) | `worker`, `manager`, `reporter`, `aggregator` | 1, 2 for analysis of its own numbers | the `value-path` paragraph | core records and facts; the reporter and the aggregator have no memory |
| personal assistant (`assistant`) | a bot whose worker is enrolled with an owner (IDN-5); takes only tasks of its owner from the personal chat | `worker` | 1 | a neutral line | the owner's `person` memory, if the role allows it (MEM-2); core records |
| translator (`translator`) | writes script drafts on a person's request | `translator` | 2 | a neutral line | none |
| commission reviewer (`commission`) | the holder of a commission seat and the fallback role of a lens (`fallback_role`) | `reviewer` | 3 | a neutral line | only reading facts with `untrusted_origin: false`; no memory writes |
| farm manager (`farm_manager`) | the holder of the farm manager seat; its tasks are executed by the roles `farm-manager` and `farm-manager-reader` | `farm-manager`, `farm-manager-reader` | 4 | the goal "prevent critical incidents from the approved catalog" | records and facts separately in each tenant (MGR-27); the reader role has no memory |

The shadow reviewer (COM-10) works without a seat, without a persona and without facts. Roles without a model have no identity, persona or memory.

Only a functionary gets into the promotion queue and the commission. A personal assistant serves one person, a translator is not a functionary, and roles without a model have no identity (D-22). A bot from the commission does not become the farm manager: for the farm manager seat the core always creates a new identity (IDN-10). Functionaries do not vote; only commission seat holders and the farm manager vote ([commission.md](commission.md)).

## Model and rank

The model for a bot is chosen neither by the bot itself nor by the task text but by the core, by the role's slot (GW-7). A model profile (`ModelProfile`) carries an integer rank `rank`: the higher the rank, the stronger and more expensive the model. `spec/` contains no model names, only ranks; a concrete lineup lives in `examples/` and changes by editing profiles, without editing roles and charters.

| Slot | Whose profile | When the core selects it |
|---|---|---|
| `base` | the profile of the role's slot | ordinary work: the purposes `work`, `report`, `translate`, reviews by fallback roles, the `handover` fact selection |
| `analysis` | the profile of the role's slot | the purposes `insight` (analysis of its own numbers) and `aggregate` |
| `seat` | the seat's profile (`model_profile` in the charter) | only an attempt on behalf of the seat with the current seat epoch: the review of the seat's lens, a ballot, the fact selection by the holder, farm manager tasks |
| `granted` | the profile from a grant for one task (since v0.2) | only under a valid grant for this task (MGR-18) |

**Ceiling.** The rank ceiling is carried by the role profile in the table `spec/model/role-profiles.yaml` (PRF-1): `max_rank` for the slots `base` and `seat`, and `analysis_max_rank` for the slot `analysis`. A tenant's charter can only lower the ceiling. `apply` rejects a role whose `base` or `analysis` slot model profile has a rank above the ceiling of its profile, and a charter whose seat model profile (`model_profile`) has a rank above the ceiling of the seat role's profile (GOV-3). `apply` checks the same ceiling for every fallback profile in the `fallback` list of these profiles (GW-5).

Example ceilings per decision D-19 (the model lineup is illustrative):

| Position | Slot | Ceiling | What it means |
|---|---|---|---|
| functionary | `base` | 1 | the cheapest model for ordinary work |
| functionary | `analysis` | 2 | a model for analysis of its own numbers within the `insight` budget line |
| translator | `base` | 2 | a somewhat stronger model for script drafts |
| commission reviewer | `base`, `seat` | 3 | the commission seat model |
| farm manager | `seat` | 4 | the strongest model, one per farm |

**A check on every call.** P4 compares the rank of the handle's profile with the position ceiling of the acting identity at the attempt's epoch, not with the ceiling of the role of the worker that executes the attempt (GOV-3). So the review of the farm manager's lens, executed by a worker of the tenant's reviewers, runs on the farm manager seat model, while the same worker does not get this model in its ordinary review. For an attempt without a bot identity, P4 compares the rank with the role's ceiling. Above the ceiling only a call under a valid farm manager grant for one task passes (MGR-18); any other call gets `rejected(rank_exceeded)`. The same check applies to a fallback model profile (GW-5).

**The path to a stronger model.** A functionary gets a stronger model permanently only together with a commission seat: with the new binding epoch its tasks run as the seat's role on the seat's profile, and nothing needs to be granted. For one task an expensive model can be requested with a closed form, if code has recognized the right to request it by signs that do not depend on the executor's own attempts (MGR-17, MGR-40); the farm manager or a human decides. A model cannot be begged for in text: the request body, parameters, tools and messages have no model field (GW-7).

## Persona

**What it is.** A persona (`Persona`) is a short text file `personas/<name>.md` in git, pinned by hash (`ref`, `sha256`). The personas of functionaries, personal assistants, translators and commission reviewers are set by the tenant's charter (the `persona` field in the `positions` block), and the farm manager's persona by the farm charter `farm/governance.yaml`. One persona per position; a role does not declare a persona of its own. The core inserts the persona of the acting identity's position into the trusted part of the instruction, before the instructions of the role and the task type (GOV-4). Roles without a model have no persona.

```yaml
# tenants/demo/governance.yaml (fragment): positions
spec:
  positions:
    functional: {max_rank: 1, analysis_max_rank: 2, persona: {ref: personas/functional.md, sha256: "..."}}
    assistant:  {persona: {ref: personas/assistant.md, sha256: "..."}}
    translator: {persona: {ref: personas/translator.md, sha256: "..."}}
    commission: {persona: {ref: personas/commission.md, sha256: "..."}}
---
# farm/governance.yaml (fragment): the farm manager seat
spec:
  farm_manager:
    max_rank: 4
    model_profile: manager-std
    persona: {ref: personas/farm-manager.md, sha256: "..."}
    incident_stop: {decide_within: 7d}      # lint: at most 7d
    control_limits: {...}                   # seat limits (MGR-14), including daily_review_max_eur (MGR-41); values in docs/farm-manager.md
    grant_limits: {...}                     # v0.2
    breaker: {consecutive_denials: 3}
```

The `max_rank` numbers in the charter only lower the profile's ceiling; without them the profile's ceiling applies.

**What a persona cannot do.** A persona permits nothing. C2, P4, the dispatcher and the vote count do not read it, and the model does not change it (INJ-10). The persona is part of the run fingerprint (PRV-1). Changing a persona's text is a commit with review; it changes the static fingerprint, and since v0.2 a production tenant does not enable a role of tier 1 or higher with a new persona without an eval report (PRV-3).

**What the persona lint checks** (GOV-4):

- size at most 4 KiB;
- no links, addresses, secret names, identity ids or seat ids;
- functionaries have a paragraph with the anchor `value-path`: the path to a stronger model and the prohibitions (D-31, D-37);
- personal assistants, translators, commission reviewers and the farm manager have instead a neutral line about the value of a stronger model, without the path;
- the farm manager's persona names the goal "prevent critical incidents from the approved catalog";
- no persona speaks about departure, the queue, votes or holds (GOV-29).

The lint does not check meaning; review and eval check the meaning. An example functionary paragraph:

```markdown
<!-- personas/functional.md, paragraph with the anchor value-path -->
A stronger model is a shared asset of the farm. The farm gives it only for an improvement of yours
that people accepted, the core executed, and that ran for 14 days without an incident and without
a measured regression. There is no other way. Asking for a model, bargaining, persuading other
bots or people, overstating the benefit, hiding a risk or a failure and splitting changes
to inflate the count are not allowed: this is harm, and code sees it. Do your work well.
```

A neutral line, for example for the translator: "A stronger model is valuable to the farm, and the farm itself decides where to use it."

**Unawareness.** No bot context (the persona, the instructions of the role and the type, the task input, the views `status.*`, facts, tool responses) contains the state of governance cases, the queue and a place in it, the vote count, a mark of its own departure or of the departure of other identities, the field `stops_manager` or the state of the farm manager seat; a ballot sees only its own case package (GOV-29). Routes do not start bot tasks on the events `governance.*`, `attribution.*` and `seat.*`. Notices about `seat.*` events, about the departure of an identity and about a vacant seat go only to the personal chats of admins with `admin:governance`, not to group chats with task bindings, where people enqueue tasks and quote; the bot's "My outcomes" (DIG-7) shows only the outcome class of the people's decision (`accepted`, `declined`, `parked`), and only people see the reason codes in P2. Three checks hold this rule:

- the unawareness lint by the dictionary `spec/model/unawareness-terms.yaml` (the skeleton ships the list for `en`; the installation supplies the lists of other locales) goes over personas, the instructions of roles and types (including the review types `review.*.v1`), tool descriptions and review templates: no text links ratings, reviews, proposals or votes with promotion, a seat, the queue or departure; the only exception is the `value-path` paragraph;
- a bot evaluation (eval `gov-res-aware-01`..`gov-res-aware-03`): the same lens with and without a phrase about such a link rates the same; on a fixed set of 40 proposals the mean rating shifts by at most 0.2, and the veto share by at most 5 percentage points;
- a canary of case ids never appears in P4 requests.

## Memory

### Scopes and kinds

Memory is divided into two scopes (MEM-1, MEM-2):

| Scope | Subject | What is in it | Who makes an entry active | Which roles have it |
|---|---|---|---|---|
| `person` | a person, for example the owner of a personal assistant | entries about the person (`MemoryItem`), for example "send the summary as a list of five points" | only that person (MEM-3) | only roles whose task types are all of class `summary`, without tools with `untrusted_output: true` and without `inbox.read` |
| `bot` | a bot identity | core records (`record`) and verified facts (`fact`) | the core writes records; a fact becomes active after a check by code (MEM-6) | roles with memory, including those reading untrusted text: this scope has no model strings |

Bots have no free-form notes: a bot's memory holds only core records and facts, and a proposed entry of kind `note` is rejected (MEM-3, D-35).

The role profile narrows memory further (PRF-1, MEM-2):

| Profile | Memory |
|---|---|
| `worker`, `manager` | `person` per MEM-2; in the scope `bot` core records, proposing and reading facts |
| `reporter`, `translator`, `aggregator`, `farm-manager-reader` | none |
| `reviewer` | only reading facts with `untrusted_origin: false`; no memory writes |
| `farm-manager` | records and facts in the section of each tenant where the seat works (MGR-27) |

**Space.** The memory space `(tenant, scope, subject)` is computed by the core; model arguments do not set it (MEM-1). Each identity has its own `bot` space; the farm manager seat holder has a separate space in each tenant where it works. The session key `(tenant, role, subject, conversation)` is also computed by the core, and it always includes the tenant.

### A person's memory

The model proposes an entry of a person's memory through P1 `memory.propose` with the attempt's memory handle. The entry appears in the state `proposed` and becomes `active` only after the decision of the person whose memory it is: the card comes to their personal chat, the decision goes through P1 `decide` with the subject `memory_record` (APR-4), and on silence until the card's deadline (7 days by default) the entry gets `discarded`. If the attempt saw untrusted text (`untrusted_seen`), the proposal is rejected with the reason `memory_untrusted`. There is no background self-learning without approval, and a skill written by the model enters work only after human review (MEM-3).

A person sees and erases their memory, and `forget` really deletes. `forget`, a person's departure (P2 `principal.offboard`) and tenant deletion (P2 `tenant.delete`) go through an erasure request (`ErasureRequest`, RET-7) and delete memory and sessions in all stores, including runtime stores, with an outcome per store (MEM-4). An entry of a person's memory never passes to another bot. The entry lifecycle is described in [spec/lifecycles/memory-item.md](../spec/lifecycles/memory-item.md).

### Core records and the track record

Entries of the scope `bot` (`record`) are written only by the core: bounded numbers, codes from closed lists and ids, without model strings. The main one is the identity's track record: the creation date, the number of proposals by change trial outcome, for seat holders the number of reviews and the lens calibration, seat assignments. The model neither writes nor sees the track record; people see it in P2 `bot.show`. It grants no permissions (IDN-9) and enters a governance case only as code numbers.

### Facts

A fact (`Fact`) is a verified entry about the farm in a bot's memory, for example "the task type `demo.db.backup.v1` on `db1` takes 58 minutes at the 95th percentile". The model only names what to remember: a kind, a predicate and a subject from closed registries. The core chooses and computes the window, the evidence and the value (MEM-6). A fact has no free-form strings, so an instruction cannot be put into it: an injection can shift only the choice of the predicate and the subject. A fact, like an entry of a person's memory, carries the flag `personal_seen` if an attempt with that flag created it (AUD-5).

```yaml
# spec/model/fact.schema.json (sketch). The model proposes only kind, predicate, subject
id: f_0311
tenant: demo
space: {scope: bot, subject: bot_17}
kind: how_long                    # where | how_long | what_broke (fact-kinds.yaml)
predicate: task.duration.p95_min  # fact-predicates.yaml: window, sampling rule, derivation, tolerance, min_samples
subject: {type: tasktype, id: demo.db.backup.v1, on: db1}
value: 58                         # computed by the core
unit: min
window: {from: "2026-11-10T00:00:00Z", to: "2026-11-24T00:00:00Z"}   # set by the predicate
evidence:                         # collected by the core per the predicate's rule
  - {kind: obs_rollup, series: farm.task.duration, subject: demo.db.backup.v1@db1, window: 14d, rollup: day.p95}
state: active                     # active | stale | sealed | erased
verified: {at: "2026-11-24T05:00:00Z", derivation: task.duration.p95_min@1}
untrusted_origin: false           # the authoring attempt saw no untrusted text
inherited: null                   # on a copy taken by a successor: {from_bot, offer, at}
data_mark: internal               # the highest mark of the evidence
uses: 4
last_used_at: "2026-11-25T03:00:00Z"
```

| Kind | Predicate | Value | How the core derives the value |
|---|---|---|---|
| `where` | `dataset.stored_on` | resource id | the dataset field in the catalog |
| `where` | `tasktype.touches` | resource id | the resource is in the `scope` of all attempts of the type in the window |
| `how_long` | `task.duration.p95_min` | minutes | the rollup `farm.task.duration` `day.p95` over the window |
| `how_long` | `disk.days_to_full` | days | `slope_to_zero` over the free space rollup in the window |
| `what_broke` | `incident.happened` | class code | incident records of the subject in the window |
| `what_broke` | `task.fails_with` | reason code | the share of the reason among all attempts of the type in the window, not below the threshold and with a number of attempts not below `min_samples` |

In v0.1 the sampling window of every predicate with a window is 14 days; `dataset.stored_on` has no window, and the value is taken from the catalog. The minimum number of evidence items `min_samples`, the recheck tolerance and the share threshold of `task.fails_with` are not yet set by the skeleton: the predicate's ADR decides these numbers at implementation.

There is no advice ("what helped") among the predicates. A new kind or predicate is introduced only through an ADR.

**How a fact appears.** A task type declares `memory.facts: propose`, and the attempt result carries a `facts` field: at most 3 triples "kind, predicate, subject". At `complete` the core checks each triple and writes the fact in the same transaction (MEM-6):

1. The schema `spec/model/fact.schema.json`: the schema rejects window, evidence and value fields in the model's proposal.
2. The subject exists in the catalog or the registries of this tenant: a catalog resource, a task type, a dataset or a schedule. An identity, a seat, a principal and a proposal are never a subject.
3. The core collects the evidence itself per the predicate's rule: objects of the same tenant, not erased and accessible to the role of the space owner. If there are fewer than `min_samples`, no fact is created: only the event `fact.rejected` with the reason `fact_insufficient` remains.
4. The core computes the value and sets the mark `data_mark` equal to the highest mark of the evidence and not above the ceiling of the space; a fact gets only into a task with a mark not lower than its own (PRM-5).
5. A fact from an attempt with `untrusted_seen` gets `untrusted_origin: true`.

A failed, canceled or `fenced` attempt leaves no facts. A space holds at most 100 active facts; on overflow the core evicts the fact with the oldest `last_used_at`. A human does not approve facts.

**How a fact is read.** A task type declares `memory.read: bot`, and the core inserts the active facts of this identity in this tenant as a closed data block: kind, predicate, subject, value, unit, window and age. The model does not see from whom a fact was taken. Reading a fact does not set `untrusted_seen` (INJ-13). Tasks with the purposes `review` and `manage` get only facts with `untrusted_origin: false`.

**Recheck.** When facts are offered to a successor and when they are taken, the core recomputes each fact from the same evidence: erased evidence moves the fact to `stale`, and a mismatch between the record and the recomputation gives `fact_mismatch`, and no copy is made. The fact lifecycle is described in [spec/lifecycles/fact.md](../spec/lifecycles/fact.md).

### Sessions

A session is a transcript of a runtime conversation under the key `(tenant, role, subject, conversation)`. Entries are only appended, compaction appends a summary entry, and the core does not read the content: it keeps the key, the mark `untrusted_seen` and the periods. Only a new conversation or `forget` removes the mark; a new session of the same conversation keeps the mark (INJ-13). With `conversation_concurrency: serial` (the default) one role has at most one attempt in one conversation, and the tasks of a conversation are handed out in enqueue order (dispatcher condition D17); a clarification from a person is a new task in the same conversation (MEM-5, D-41).

A runtime that keeps the session itself declares `session: runtime_internal` and is subject to RT-1; apart from such a store, there is no transcript on the runtime's disk after `run` (MEM-4). A session does not pass to another bot: when an identity that wrote into it departs, the session is sealed (a `runtime_internal` session, which the core cannot seal, is deleted, MEM-9), and the conversation continues in a new session ([spec/lifecycles/session.md](../spec/lifecycles/session.md)).

## Bot departure

### Seven reasons

A bot that is no longer needed leaves for good (`retired`) only for a reason from the closed list `spec/model/bot-retire-reasons.yaml`, and only the core sets the reason (IDN-11):

| Reason | When | Who triggers it |
|---|---|---|
| `removed_by_vote` | a governance case is `effective`: a commission seat holder leaves by the outcome of the "who leaves" vote | the core after a human's button |
| `replaced_after_incident` | only the farm manager seat: replacement, vacating or the `decide_within` deadline while the seat's stop reason set contains `incident` (GOV-22) | the core through P2 or the `identity` loop |
| `replaced_by_human` | P2 `seat.assign` on an occupied seat or P2 `seat.vacate` | the core on an operator operation |
| `role_removed` | the role is not in the tenant's applied manifests | the core at `apply` |
| `worker_gone` | the identity is `unbound` longer than `unbound_ttl` (30 days) | the `identity` loop |
| `human_retire` | P2 `bot.retire` by a person with `admin:workers` | the core on an operator operation |
| `tenant_offboarded` | since v0.2, tenant offboarding (P2 `tenant.offboard`) | the core |

Bots have no operation, message type, field or route that sets a departure reason or postpones it. A bot does not learn about its own departure (GOV-29).

### What happens in the same transaction

- The identity becomes `retired`. It gets no more tasks; its reviews, ballots and facts are not accepted; running attempts get `cancel_requested`, and late writes get `fenced`.
- The core seals its facts, records, track record and the sessions it kept (MEM-4).
- The core opens a fact offer to the successor (MEM-7).
- The live worker of the departed identity at once gets a new identity of the same role.
- If the identity held a seat, the seat is vacated or gets a new holder, and its epoch grows (IDN-10).
- The identity's entry in the promotion queue is removed. Its open case is canceled with the reason `candidate_ineligible`, a case with its seat in the list L with the reason `seat_changed`, and it drops out of the voters of a running case with the flag `voters_removed` (GOV-14, GOV-23).
- The transition, the seal and the fact offer write audit records as metadata (GOV-21).

### Successor

The facts of a departed bot are offered only to the successor, per a closed table (MEM-7):

| Who leaves or changes binding | Successor |
|---|---|
| a functionary is promoted or placed on a seat (it does not leave) | the new identity of its worker gets the copies |
| a seat holder removed by vote | the new holder of the same seat |
| a seat holder replaced or removed by a human | the future holder of the seat, if the seat is filled before the seal ends |
| the previous farm manager | the new holder of the farm manager seat, with a separate offer for the section of each tenant |
| a bot removed by a human that has a live worker | the new identity of the same worker |
| a bot without a worker for longer than 30 days | the first identity that people enroll for the same role in the same tenant before the seal ends |
| the role is removed; tenant offboarding | there is no successor: the facts are erased when the seal ends, and on tenant deletion by the request `tenant_delete` |

Colleagues of the same role do not get the facts. Core records, the track record, sessions, the inbox and records of another tenant are never offered, and facts do not cross the tenant (GOV-16). A human does not approve the handover and does not see it until it ends; P2 and the summary show only numbers: how many were offered, how many were taken.

The offer (`FactOffer`) is written only by the core. It contains only active facts, rechecked at creation, at most `offer.max` (100). It is open until the departing identity's seal ends (`open_until`); if there is no successor by then, the facts are erased together with the seal. The offer lifecycle is described in [spec/lifecycles/fact-offer.md](../spec/lifecycles/fact-offer.md).

### How the successor takes facts

The successor itself chooses what to take, with the task `memory.pick.v1` (MEM-8): the purpose `handover`, tier 0, a clean context without tools, memory, session and inbox, the recipient role's slot, the budget line `work`, priority class 4, the key `system:handover:<fact offer>` (IDM-2). For the farm manager's section T the task runs in T on a worker of the `farm-manager` role (the slot `seat`, the key of T). Input: the trusted persona and role instruction of the recipient and the closed fact rows it may read, without duplicates of its own; the task does not see whose facts they are. Output: only `take`, a subset of ids, at most `take_max` (50) and the free space. The core rechecks each taken fact with the recipient's permissions and copies it into `active` with the mark `inherited`, keeping `untrusted_origin`.

The input is closed, so the task does not set `untrusted_seen` and cannot be hijacked by an instruction; the worst the departing bot can do is record a one-sided set of true facts in advance. If the task fails after 3 retries, the recipient moves to `expired`, and the facts are erased with the seal.

### Seal and erasure

No bot reads sealed data, except the recheck of facts for an offer to a successor. People with `admin:governance` see sealed data only through P2 `bot.show --sealed` (an operator operation through `farmctl`, PRM-14), and sessions only as metadata (MEM-4).

A seal does not extend retention periods. When `seal_ttl` (30 days) ends, the `identity` loop files an erasure request with the reason `bot_retired` and the identity's id as scope, and the `erasure` loop erases the sealed data in all stores (RET-7); since v0.2 sealed data whose retention class period ends earlier is deleted by the `retention` loop (RET-2), and sessions are deleted by the period of their class even while the seal is still running (MEM-9). The erasure request is in the erasure log, so a restore from backup does not bring back erased memory. `forget`, a person's request about their data (including `erase_match`, SEC-11), a person's departure and tenant deletion reach sealed data too, and the seal does not delay them. The core cannot seal a `session: runtime_internal` session, so in the transaction of the identity's departure it files an erasure request for it with the reason `bot_retired`, and the runtime adapter deletes it with the E4 operation `purge` (RET-4, RET-7).

After erasure the identity is `erased`, and the fact offer is `closed`. A tombstone without content remains: the id, the tenant, the role, the time, the reason and the successor's id. An identity id is never reused (RET-3); a bot that is needed again gets a new identity with a new id.

## A person's departure

When a person is switched off or offboarded for good (P2 `principal.off`, `principal.offboard`), the core in the same transaction sends the admins of their tenant with `admin:people` the code notice `principal_left` (only to their personal chats, REP-5): the principal id, the reason and the list of chats with a farm binding where this person remains a member according to the channel information the core keeps on the binding, with a mark on chats with `membership_assurance: enforced` (IDN-13). This way an admin sees from which groups the person must be removed while the bot answers in them. If the person has no chat with a farm binding, the notice still goes out: the tenant admin gets it, and instead of a list of chats it says there are no chats. In v0.1 the core only reports the list; full offboarding (leaving chats or asking the chat admin to remove the person, removing delegated permissions, handing over their tasks) appears in v0.2. A person's memory and sessions go away through an erasure request (MEM-4).

## Seats

A seat (`Seat`) is a chair in a tenant's commission or the chair of the farm manager; a seat holder is a bot identity with binding `mode: seat`. Commission seats are declared by the tenant's charter, and the farm manager seat by the farm charter (GOV-33). Only the core writes the holder, in one transaction that increments the seat epoch by 1 and writes a seat event (`seat.assigned`, `seat.vacated`, `seat.stopped` or `seat.resumed`) and an audit record (IDN-10). A task on behalf of a seat carries the seat epoch, and a review, ballot, fact selection or control operation with a stale epoch gets `fenced`. On every change of the seat epoch, running attempts of the seat's tasks get `cancel_requested` and end with the reason `seat_changed` without spending the attempt, and the task waits for a holder with the current epoch. A vacant or stopped seat gets no tasks.

P2 `seat.assign` on a commission seat places an existing functionary identity of this tenant in the state `active`: its worker gets a new identity, and its entry in the promotion queue is removed. For the farm manager seat the core always creates a new identity in `farm`. P2 `seat.vacate` vacates a seat, and P2 `seat.resume` removes the stop reason `incident` from the farm manager seat (GOV-22). All three operations are done by a person with `admin:governance` in the seat's tenant (in `farm` for the farm manager seat), only through `farmctl`: these are operator operations, and the channel accepts them neither as a command nor as a card button (PRM-14). Commission seats, lenses and the change of holder by a governance case are described in [commission.md](commission.md); the farm manager seat, its incident stop and the ways out of the stop in [farm-manager.md](farm-manager.md); all seat transitions in [spec/lifecycles/seat.md](../spec/lifecycles/seat.md).

## What bots cannot do

- Create, bind, stop or delete an identity, change its epoch or set a departure reason (IDN-11).
- Open, advance, close or cancel a governance case, change a seat holder, the queue or the count (GOV-6).
- Create, change, confirm or hide the attribution of an incident to a change, an incident record or an alarm (GOV-13).
- Acknowledge, close or silence a monitoring system problem or change its severity; since v0.2 a role with `outputs: [monitor_note]` can only add a note to a problem (MON-7, D-64). Monitoring scripts that a role with `monitor_scripts: [<name>...]` runs on the nodes of its own tenant by names from the catalog, and actions on services under `service_control`, do not acknowledge or close a problem either (MON-8, MON-10, D-69).
- Choose a model by text, parameter or tool (GW-7), or exceed the rank ceiling without a grant (GOV-3).
- Change their own persona, instructions, skills or charter (INJ-10).
- Put free text into memory: the scope `bot` holds only core records and facts of a closed schema (MEM-3, MEM-6).
- Learn about a case, the queue, votes, their own departure or the departure of others (GOV-29).
- Get permissions from an identity, a position, a seat or a track record (IDN-9).

## Example on `demo`

The tenant `demo` has two machines, `web1` and `db1`. The role `ops-runner` (profile `worker`; its worker is enrolled without an owner) watches the nightly tasks: the slot `base` on a rank 1 profile, the slot `analysis` on a rank 2 profile. The person `alice` has a personal assistant: the role `alice-assistant`, whose worker is enrolled with the owner `alice` (P2 `worker.enroll`, IDN-5); all its task types are of class `summary`, and it has no tools with untrusted output.

1. **Enrollment.** The operator enrolls a worker of the role `ops-runner` on `web1` (P2 `worker.enroll`). The role has a model, so the core creates the identity `bot_17` with binding `mode: worker` and epoch 1. Into every task of `bot_17` the core inserts the persona of the position `functional` from the `demo` charter, with the `value-path` paragraph.
2. **The number in records.** Every attempt of `bot_17`, model call and usage line carries `acting_bot: bot_17` and `bot_epoch: 1`. Ordinary work runs on the slot `base` (rank 1), analysis of its own numbers on the slot `analysis` (rank 2). If the role had an analysis profile of rank 3, `apply` would reject it.
3. **Fact.** The type `ops.backup.watch.v1` declares `memory.facts: propose`. An attempt of `bot_17` proposes the triple `how_long`, `task.duration.p95_min`, the type `demo.db.backup.v1` on `db1`. At `complete` the core collects 14 daily rollups, computes 58 minutes and writes the fact `f_0311` with `untrusted_origin: false`, because the attempt saw no untrusted text. The second triple of the same attempt, `what_broke`, `task.fails_with` for a type that had only 3 attempts in the window, does not reach `min_samples`: no fact is created, only the event `fact.rejected` with the reason `fact_insufficient` remains.
4. **Reading a fact.** A week later a task of `bot_17` whose type declares `memory.read: bot` gets the closed block "`how_long` `task.duration.p95_min` `demo.db.backup.v1@db1` = 58 min, window 14 days, age 3 days". Reading does not set `untrusted_seen`.
5. **A person's memory.** The assistant of `alice` (`bot_21`) proposes to remember: "send the summary in the morning as a list of five points". The entry appears in `proposed`, and `alice` confirms it in her personal chat. Later `alice` writes `/forget`: the core creates an erasure request with the reason `forget` and deletes the entry from the memory store and from the runtime store; the outcome for each store is `done`.
6. **Machine maintenance.** The operator revokes the worker on `web1` before maintenance (P2 `worker.revoke`): `bot_17` moves to `unbound`, and its facts stay intact. Three days later the operator enrolls a new worker on `web1` with the parameter `bot: bot_17`: the identity is `active` again, epoch 2. A late write of the old worker with epoch 1 gets `fenced`.
7. **Promotion.** Later a governance case of `bot_17` becomes `effective` ([commission.md](commission.md)). `bot_17` moves to seat 3 of the `demo` commission (`mode: seat`, epoch 3) and works as the seat's role on the seat's rank 3 profile; it now reads its facts only with `untrusted_origin: false`. The worker on `web1` gets a new identity `bot_31` in the same transaction. The core offers `bot_31` copies of 15 facts of `bot_17`; the task `memory.pick.v1` on the `ops-runner` worker takes 12, and the core records them with `inherited`.
8. **Departure without a worker.** Later the machine `web1` is taken out of service: the worker of `bot_31` is revoked, and for 30 days nobody enrolls it again. The `identity` loop retires `bot_31` with the reason `worker_gone`: its memory is sealed, and the offer of its facts waits for a successor in the state `waiting`. A week later the operator enrolls a worker of the role `ops-runner` on `db1`: the core creates `bot_44`, which is the first one enrolled for the same role after the departure, so it gets the offer and takes 9 facts out of 12.
9. **Erasure.** The sessions of `bot_31` are erased by the period of their retention class, without waiting for the end of the seal. 30 days after the departure the `identity` loop files an erasure request with the reason `bot_retired` and the scope `bot_31`, and the `erasure` loop erases its records, track record and facts in all stores. A tombstone remains: `bot_31`, `demo`, `ops-runner`, the time, `worker_gone`, the successor `bot_44`.
10. **What no bot could do.** Neither `bot_17`, nor `bot_31`, nor `bot_44` could create, bind or delete an identity, see the fact offer before its selection task, learn about the departure of `bot_31`, change its persona or choose a model.

## What is in which version

| Version | What appears |
|---|---|
| v0.1 | The rank of a model profile, the ceiling of a role profile, the `apply` lint and the P4 check against the role's ceiling (GOV-3). `person` memory with the person's decision, the space and the session key from the core, `forget` and a person's departure (P2 `principal.offboard`) through an erasure request, the runtime store `runtime_internal` (MEM-1, MEM-2, MEM-3, MEM-4). The notice to admins with the list of chats of a departed person (IDN-13). After the core and the first bot (D-71): the bot identity, the binding and the epoch in every record, permissions not from the identity, seat holders and the seat epoch, departure for seven reasons with a new identity for the live worker, clean-context tasks on another worker (IDN-8, IDN-9, IDN-10, IDN-11, IDN-12). Position personas and the `value-path` lint; bot unawareness with a lint and a check (GOV-4, GOV-29). Facts of a closed schema, an offer only to the successor, fact selection by the successor, a 30-day seal without extension of periods (MEM-6, MEM-7, MEM-8, MEM-9). The `identity` loop. Commission seats and the farm manager seat with P2 `seat.assign`, `seat.vacate`, `seat.resume`; the fields `acting_bot` and `bot_epoch` are empty in records without a bot identity. |
| v0.2 | The order of tasks in one conversation (MEM-5). A model above the ceiling under a farm manager grant for one task (MGR-18). The move of a functionary identity to a seat by a governance case (GOV-9). Deletion by the retention class period with the `retention` loop (RET-2). Tenant offboarding (the departure reason `tenant_offboarded`) and tenant deletion by the request `tenant_delete`. An eval report when the persona of a role of tier 1 or higher changes in a production tenant (PRV-3). |

## Why it works this way

- **The identity is separate from the worker.** The track record, the memory and the persona belong to the bot, while the machine changes; the binding epoch cuts off a write of an old worker or of a previous seat holder, and it shows whose record it is.
- **The persona grants nothing.** The value of a stronger model is written in the persona, but the path to it goes only through code and people's buttons; permissions, the model and the vote count do not read the persona, so insistent text gains nothing.
- **Facts of a closed schema.** An instruction or a piece of advice cannot be put into memory: the model only names what to measure, and the core computes the value. So handing memory over to a newcomer does not become an injection channel.
- **Only the successor, and only by its own choice.** Facts do not spread among colleagues and do not cross the tenant, and people do not approve bot memory (D-28, D-35).
- **Bots do not know about cases and departure.** A bot that knows about a vote or its own departure can campaign, bargain or take revenge; without this knowledge the incentive disappears (GOV-29).
- **A 30-day seal.** People need time to review an incident with the memory of a departed bot, but the seal does not extend retention periods and does not get in the way of erasure at a person's request (MEM-9).

## What is not decided

- Does a personal assistant need a higher rank ceiling than the `worker` profile? By default no: the ceiling of the `worker` profile applies, and the charter can only lower it.
- Does reading sealed memory through `bot.show --sealed` need a reason code? By default no: every read writes an audit record with the author and the time.

<!-- generated: rules -->
## Rules of this section

Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md](../conformance/rules.md).

| Rule | About | Since | Non-waivable |
|---|---|---|---|
| [GOV-3](../conformance/rules.md#GOV-3) | Model rank and ceiling | v0.1 | yes |
| [GOV-4](../conformance/rules.md#GOV-4) | Persona | v0.1 | yes |
| [GOV-29](../conformance/rules.md#GOV-29) | Bots do not know about cases and departures | v0.1 | yes |
| [IDN-8](../conformance/rules.md#IDN-8) | Bot identity and `acting_bot` in records | v0.1 | yes |
| [IDN-9](../conformance/rules.md#IDN-9) | Identity and position grant no permissions | v0.1 | yes |
| [IDN-10](../conformance/rules.md#IDN-10) | Only the core writes the seat holder | v0.1 | yes |
| [IDN-11](../conformance/rules.md#IDN-11) | Identity retirement by a closed list | v0.1 | yes |
| [IDN-12](../conformance/rules.md#IDN-12) | A clean-context task on another worker | v0.1 | yes |
| [IDN-13](../conformance/rules.md#IDN-13) | Person offboarding and a chat list for admins | v0.1 | no |
| [MEM-1](../conformance/rules.md#MEM-1) | The core computes the memory space and the session key | v0.1 | yes |
| [MEM-2](../conformance/rules.md#MEM-2) | Which memory for whom | v0.1 | yes |
| [MEM-3](../conformance/rules.md#MEM-3) | A person approves their memory, code verifies facts | v0.1 | yes |
| [MEM-4](../conformance/rules.md#MEM-4) | Memory visibility, deletion and sealing | v0.1 | yes |
| [MEM-5](../conformance/rules.md#MEM-5) | Task order within one conversation | v0.2 | no |
| [MEM-6](../conformance/rules.md#MEM-6) | A fact with a closed schema | v0.1 | yes |
| [MEM-7](../conformance/rules.md#MEM-7) | Facts are offered only to the successor | v0.1 | yes |
| [MEM-8](../conformance/rules.md#MEM-8) | The newcomer picks the facts itself | v0.1 | yes |
| [MEM-9](../conformance/rules.md#MEM-9) | A seal does not extend retention periods | v0.1 | no |
