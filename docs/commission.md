# The commission and the functionary's path

## In short

Each tenant has its own commission of bots (D-23): seats with lenses, held by former functionaries of this tenant, and the farm manager with one vote and no veto (D-29). The commission assesses bot proposals blind, and how it does this is described in [improvements.md](improvements.md); this document describes who sits on it and how they get there. There is one path to a seat: a functionary's own improvement that people accepted, the core executed, and that lived through the promotion probation (14 days) without a confirmed link to an incident and without a measured degradation; then the promotion queue, a case and, if there is no vacant seat, a secret mandatory "who leaves" vote, in which exactly one always leaves (D-24, D-25, D-37). Only code opens and counts a case, the outcome takes effect only after a person's button, and bots know nothing about cases, the queue or departures (GOV-6, GOV-9, GOV-29). A bot that leaves is deleted entirely, and only the newcomer takes its verified facts ([bots.md](bots.md)). Before v0.2 there are no votes, and a person appoints the seat holders (D-21). From v0.3 the commission answers for its outcomes, first in shadow: see [Accountability from v0.3](#accountability-from-v03) (D-109).

## The tenant's commission

### Charter

Only the tenant's charter `tenants/<t>/governance.yaml` declares commission seats (GOV-33). The charter is a manifest from git with review; it sets the seats, the path deadlines and the memory limits, while the personas and position ceilings in the `positions` block are described in [bots.md](bots.md).

```yaml
# tenants/demo/governance.yaml (fragment)
spec:
  commission:
    seat_role: commission-member         # a role of profile reviewer: its workers execute the seats' tasks
    model_profile: seat-std              # model profile of the seat slot, rank no higher than the reviewer profile ceiling
    seats:                               # lenses with a veto on profiles of different families (GOV-34)
      - {id: 1, lens: security,    fallback_role: reviewer-a}
      - {id: 2, lens: operational, fallback_role: reviewer-a, model_profile: seat-alt}   # its own family
      - {id: 3, lens: cost,        fallback_role: reviewer-b}
      - {id: 4, lens: value,       fallback_role: reviewer-b}
    audit_sample:                        # audit sample; all three fields can only be raised (DIG-5)
      min_per_month: 4                   # at least this many items a month; lint: no less than 4
      share: 0.25                        # and no less than this share of the sample strata size
      max_per_digest: 2                  # no more items in one digest to people
  promotion:
    probation: 14d                       # promotion probation; can only be raised (GOV-14)
    exclude_change_kinds: [none]         # can only be extended; none is always in the list (GOV-14)
    min_tenure: 30d                      # checked when a case opens; can only be raised (GOV-14)
    swap_cooldown: 30d                   # can only be raised (GOV-14)
  removal:
    protect_new: 60d                     # can only be raised (GOV-12)
  ballots:
    reissue_max: 3                       # can only be raised (GOV-23)
    stuck_after: 24h                     # can only be shortened (GOV-23)
  confirmation: {ttl: 7d}                # promotion card deadline; can only be raised (GOV-9)
  attribution: {window: 14d}             # attribution window; can only be raised (GOV-13)
  identity: {unbound_ttl: 30d}           # can only be raised (IDN-11)
  memory:
    seal_ttl: 30d                        # can only be raised (MEM-9)
    facts: {max_active: 100, per_attempt: 3}   # can only be lowered (MEM-6)
    offer: {max: 100, take_max: 50}            # can only be lowered (MEM-7)
```

What `apply` checks (GOV-33):

- seats, their lenses and executors are declared only here and only once; the commission manifest (`Commission`) sets only thresholds and weights per lens (COM-8), while the lenses themselves and their vetoes are set by the skeleton in `spec/model/lenses.yaml`: security (`security`) and operations (`operational`) with a veto, cost (`cost`), value (`value`) and the farm manager's lens (`farm_manager`) without a veto;
- `apply` does not delete a seat that has a holder: first P2 `seat.vacate`;
- the charter has no blocks other than those shown here and the positions block `positions` ([bots.md](bots.md)), and each numeric field changes only in one direction from its default, as stated in the comments: deadlines approved by decisions (`probation` 14 days, `min_tenure` and `swap_cooldown` 30 days, `protect_new` 60 days) can only be raised, memory limits only lowered, and the deadline `decide_within` of the farm manager seat in the farm charter only shortened (GOV-14, GOV-33);
- the rank of the `model_profile` is no higher than the ceiling of the seat role's profile (GOV-3);
- in the audit sample block `commission.audit_sample`, `min_per_month` is no less than 4 (DIG-5);
- each lens with a veto has a holder or a fallback role (COM-7);
- for a production tenant, the seat profiles of the lenses with a veto belong to different model families (`ModelProfile.family`): a match is an `apply` error (`veto_lenses_same_family`), not a warning; the shadow shares (COM-10) default to 0.25 for lenses with a veto, and 0 is allowed only with an explicit `shadow: off` in the commission manifest (GOV-34).

The vote counting rules (the 2/3 threshold, the tie keys, the vote by code rule) are written in the skeleton rules, not in the charter: there is no reason to configure them (GOV-8, GOV-23).

### Who executes a lens

At any moment each lens has exactly one source of executors: the holders of the seats of this lens, if at least one such seat is taken (one of them takes the task in rotation), otherwise the reviewer fallback role `fallback_role` (COM-7). The tasks of one lens are not handed to two sources at once, and one proposal does not get two reviews from the same lens. If the tenant charter has no seats, the lens task is taken by a role of the profile `reviewer` that has this lens's type in its `claims` (COM-8).

A seat's task is executed by a worker of the seat role `seat_role`; `acting_bot` is the holder, and the attempt carries the seat epoch and runs on the slot `seat` with the profile `model_profile` (IDN-10, GW-7). A seat whose holder is under a person's hold of scope `bot` behaves as vacant for that time: the fallback role executes the lens.

If a vacant lens has no fallback role either, a lens without a veto stays silent and the verdict is computed without it, with a mark, while a lens with a veto keeps proposals in `parked(veto_lens_missing)` (COM-4). The tenant's people get the notification "seat N is vacant, a lens with a veto is not working, appoint a holder". P2 `commission.status` and the `doctor` check show who covers each lens.

### Who sits on the seats

The holder of a commission seat is always a former functionary of its own tenant: a bot of a role of the profile `worker`, `manager`, `reporter` or `aggregator` whose worker is registered without an owner (D-22). It comes to the seat either by a promotion case or by a person's appointment, and in both cases its identity moves to the seat, and its worker gets a new identity ([bots.md](bots.md)). Personal assistants, translators and roles without a model do not get seats. A bot from the commission does not become the farm manager: for the farm manager seat the core always creates a new identity.

A seat holder assesses every proposal of its tenant through its seat's lens and votes in "who leaves". It does not review its own proposals, does not see the author's identity, role or position, and does not see the assessments of other lenses (GOV-17, COM-2, COM-3). It only reads its own facts, and only those with `untrusted_origin: false`; it writes no new facts (PRF-1). It has no path beyond the commission.

### The farm manager in every commission

The farm manager is a member of the commission of every tenant that has not opted out of it (GOV-25, D-29): with `farm_manager.enabled: false` or `lens: false` in the tenant spec (MGR-42, D-77) there is neither a lens nor a vote of the farm manager in this tenant's commission, and the verdict is computed without them and without a mark. In the other tenants, while the farm manager seat is in `held`, every proposal in `in_review` gets a review from the `farm_manager` lens:

- the task `review.farm_manager.v1` runs in tenant T on a worker of T's reviewer role, on the slot `seat` with the farm manager seat's profile, with T's key and from T's line `review`;
- the input is as for the seat lenses (COM-3), plus the holder's facts from T's section with `untrusted_origin: false`;
- weight 1 by rule, no veto, the lens is not in `min`, `high_risk` and `untrusted`, and it is not needed for a quorum (COM-4);
- lenses with a veto are paid first, and the farm manager's lens is skipped when the line `review` runs short: the verdict is computed without it, marked "without the farm manager's vote", and its silence does not become a veto.

The farm manager's lens is not applied to proposals about the farm manager seat, its role, the incident catalog, the involvement rules, the farm manager's limits, consent groups (MGR-16) or its own proposals (GOV-17). In the "who leaves" vote the farm manager has one vote, and its list is all of L. `apply` lint: every active tenant with the farm manager's lens allows the farm manager seat's profile (or its own `farm_manager.model_profile` from the tenant spec, MGR-42) on the slot `seat` of the reviewer role and keeps the line `governance` above 0 (GOV-25).

## The functionary's path

There is one path to a commission seat, and the bot decides only in its first link: what to propose.

1. **Idea.** From the core's numbers, a functionary in an analysis task hands in a proposal with one change from the allowed list ([improvements.md](improvements.md)).
2. **Check by code.** The core rereads the evidence and checks the form, duplicates, feasibility and price.
3. **Commission.** The seat lenses and the farm manager's lens assess blind; code computes the verdict (COM-4).
4. **People.** A recommended proposal arrives in a digest, and only a person presses "Accept"; some rejected ones reach people through the audit sample (DIG-5).
5. **Execution and change trial.** The change is executed the normal way (`realized`), and for exactly 14 days the core measures the promised indicator (PRP-21, PRP-22).
6. **Incident attribution.** Code finds a possible link between an incident and the change, and a person decides (GOV-13, GOV-30).
7. **Queue.** The promotion probation has passed, the change trial outcome is not `worse`, and every link was answered "No": the bot is in its tenant's queue (GOV-14, GOV-32).
8. **Opening a case.** The head of the queue gets a case, and code checks eligibility (GOV-6, GOV-14).
9. **"Who leaves".** If there is no vacant seat, the seat holders and the farm manager vote secretly and mandatorily; exactly one leaves (GOV-7, GOV-8, GOV-23).
10. **A person's button.** The outcome takes effect only after "Confirm" (GOV-9, GOV-31).
11. **Changeover.** In one transaction the candidate takes the seat, the leaver is deleted entirely, and its verified facts are offered only to the candidate.

**What a bot cannot do on this path.** A bot does not open a case, does not see the queue or the vote count and does not know about its own departure (GOV-6, GOV-29). It does not ask for a seat or a rank, does not bargain and does not write to seat holders or the farm manager: reviewers and the farm manager cannot be addressed with a message (MSG-5). It casts its vote only by choosing from a list, without free text (GOV-7).

## Change trial and promotion probation

### Change trial outcome

Only code computes the change trial outcome, from a closed list: `better`, `unchanged`, `worse`, `undetermined` and `mixed` (PRP-22). A promotion trigger appears only after this outcome. The outcome `worse`, that is, a degradation of the promised indicator or a guard trip measured by the core, gives no trigger, just like a confirmed incident attribution. The outcomes `better`, `unchanged`, `undetermined` and `mixed` do not take the trigger away (GOV-32, D-37). Only a proposal that is still `realized` at the moment the trigger appears gives a trigger: a revert (`reverted`) and a trial in `canceled` give no trigger, and a revert by a person with the reason `harm_observed` is recorded in the track record and the calibration as `worse` (GOV-14, GOV-32).

### Promotion probation

These are two different periods. The change trial (`trial`) is the measurement of the promised indicator for exactly 14 days from `realized`; its window is not shifted or extended (PRP-21, PRP-22). The promotion probation (`probation`, 14 days from `realized`, the charter can only raise it) is the period that the change must live through without a confirmed incident attribution and without a measured degradation in order to give a promotion trigger (GOV-14). The trigger appears when the promotion probation has passed, the change trial outcome exists and a person has answered "No" for every attribution candidate; while there is no answer, or the attribution is disputed after `dismissed`, the trigger waits without a deadline (GOV-30, D-79). An outcome marked `self_measured` (the metric points were produced only by attempts of the author or its role, PRP-22) does not give a trigger by itself: people with `governance.confirm:commission` get the card `self_measured_card` (decision subject `proposal`, APR-12) with the numbers of the computations, and the trigger appears with the "Count it" button within 7 days; silence leaves the success only in the track record (GOV-14).

Proposals that are counted without merit give no trigger either: with a lifted veto (DIG-6), late related proposals (PRP-17) and changes of a kind from `exclude_change_kinds` (`none` by default) (GOV-14). Changes whose promotion probation ended before the v0.2 rules came into effect in the tenant give no trigger either: before v0.2 there is no incident attribution, and nobody asked about a possible link. Such successes stay only in the track record (D-51).

### Attribution candidates

An attribution candidate (`Attribution`) is a possible link between an incident and an executed change. Only code creates it, per `attribution-rules.yaml`, or a person through P2 `attribution.create`; only a person with `governance.attribute` decides (GOV-13).

| Rule | When code creates a candidate |
|---|---|
| `direct` | an incident of a class with `change_sensitive` concerns the object of the change directly |
| `dependency_1` | an incident concerns an object that depends on the object of the change one step away in the catalog |

A candidate is created only if the incident was opened within the attribution window. The window starts at `realized` and lasts `attribution.window` (14 days). For a change that may give a promotion trigger, the window continues until the trigger appears, and for a change that gave a trigger to a queue entry, until the case of this entry is `effective` (GOV-13).

The card `attribution_card` goes to the tenant's people with `governance.attribute`, and the answer comes through the P1 call `decide` with the subject `attribution` (APR-4). There is no model text on it: only the incident, the class, the changed objects and the rule by which code found the link.

```
Is the incident linked to the change? (candidate at_0042)
Incident: i_0913, class backup_late, db1, opened 11-18 02:40
Change: p_0101, moved report-nightly from 02:00 to 03:30, executed 11-10
Link by rule dependency_1: report-nightly reads the database on db1
[Yes] [No] [Don't know] [Details]
```

- "Yes" and "No" are accepted only after "Details" is opened (APR-14): "No" is the only answer that gives a trigger, and without this condition it would be pressed to get some quiet. For every answer the core writes into the audit the response time `response_ms` and the flag `details_opened` (APR-11) and shows them on the promotion card. A person cannot change "Yes": the change loses its trigger, and the confirmed link becomes part of the calibration of the security and operations lenses (COM-11).
- A person can change "No" and "Don't know" while the attribution window is open; when the window closes they become final too. This is the only exception to the single decision per subject (APR-4).
- Silence does not expire. A candidate with no answer by the card deadline (7 days), or with the answer "Don't know", stays in the state `unknown` ("not decided"), and once a week people with `governance.attribute` get a reminder, no more than 3 times per candidate; after that the candidate stays as a line in the weekly digest (GOV-30).
- While a change has candidates in `candidate`, `unknown` or `disputed`, there is no promotion trigger, and the attribution window stays open. The change trial and its outcome do not shift or get extended because of this.
- If people declared the incident a false alarm (`dismissed`, only with a reason code and "Details", MGR-8), the core in the same transaction cancels the candidates without a "Yes" answer: they become `canceled`, the pending card closes, and there are no new candidates for this incident (D-57). A dismissal does not erase someone else's "Yes" answer: the attribution becomes disputed (`disputed`), and the author of the answer gets in a private chat the card `attribution_dispute` with the reason code and "Details" of the person who dismissed the incident, and confirms the answer with the "Yes" button or withdraws it with the "Withdraw" button; 7 days of silence withdraws it. While the dispute is open, the change has no promotion trigger, and the disputed attribution is not part of the lens calibration (D-79). A canceled attribution does not count as confirmed: it does not take the trigger away from the change, does not block eligibility for promotion and leaves the lens calibration. The trigger appears under the general conditions, but not before the cancellation. A case that was already canceled because of this attribution is not resumed: the bot enters the queue again with a new trigger time.

No bot, including seat holders and the farm manager, creates, changes, confirms or hides an attribution, an incident record or an alarm (GOV-13). A bot also does not acknowledge, close, silence or change the severity of a monitoring system problem: only people do this, and the farm's monitoring account has no such permissions (D-61, D-64, D-69). Monitoring scripts and service actions that a bot with the permission performs on the nodes of its own tenant (MON-8, MON-10) close neither an incident nor a problem: an incident moves to `resolved` only by the alarm state. The candidate lifecycle is described in [spec/lifecycles/attribution.md](../spec/lifecycles/attribution.md).

## Promotion queue

When the trigger has appeared, the `governance` loop puts the author into its tenant's promotion queue (GOV-14). There is one promotion trigger: a functionary's own proposal, accepted by a person and executed, whose promotion probation has passed, whose change trial outcome is not `worse`, and for every attribution candidate of which a person has answered "No".

- A bot has one entry. The next success of a bot that is already in the queue is written to its track record.
- The queue has no deadline and no length ceiling; the order is set by the time the trigger appeared.
- No bot sees the entry, including its owner (GOV-29).

An entry is the record `spec/model/promotion-entry.schema.json`. After the first "Reject" with `wrong_candidate` it stays at the head of the queue with its own `swap_cooldown` pause, and the next entries may go ahead meanwhile (D-101).

| What removes an entry | What next |
|---|---|
| the bot's departure (`retired`) | the entry is removed |
| a second "Reject" for this bot with the reason `wrong_candidate` (GOV-31) | the entry is removed until the bot's next success |
| seating the bot through P2 `seat.assign` | the entry is removed: the bot is already on the commission |
| an attribution confirmed later of an incident to the change that gave the trigger (if it is later canceled together with the incident, the trigger appears again, D-57) | if the track record has a later trigger, the entry takes it and its time; otherwise it is removed |

A case opens for the head of the queue if the tenant has no open case and no hold of scope `governance`. The head of the queue takes a vacant seat without a pause. To free a seat by vote, it is also required that the list L is not empty and that at least `swap_cooldown` (30 days) has passed since the last freeing by vote, the last `expired` or the last "Reject" with the reason `not_now`.

**Eligibility.** Code checks it when the case opens, not when the entry joins the queue: the identity is `active`, its tenure is no less than `min_tenure` (30 days), and in 90 days there are no confirmed attributions to its changes. If it fails: the event `not_eligible`, and the entry waits and lets the next one go ahead. Eligibility lost in the middle of a case gives `canceled(candidate_ineligible)`. The entry lifecycle is described in [spec/lifecycles/promotion-entry.md](../spec/lifecycles/promotion-entry.md).

## Promotion case

A case (`GovernanceCase`) is one promotion of a functionary to its tenant's commission, together with the "who leaves" vote if there is no vacant seat. There is one kind of case, `promotion`, with the mode `removal` (someone leaves) or `vacancy` (the candidate takes a vacant seat without a vote). Only the `governance` loop opens a case, for the head of the queue; there is no P2 operation that opens a case, and bots have no operation, message or field that opens, moves, closes or cancels a case (GOV-6). A case lives only in the commission's tenant (GOV-16), it contains no free text, and no bot sees it (GOV-29).

```yaml
# spec/model/governance-case.schema.json (sketch). Written only by the core
id: gc_0007
tenant: demo
kind: promotion
state: balloting                 # opened | parked | balloting(n) | counted(n) | confirming | effective | rejected | expired | canceled
hold: false                      # pause by people: a hold of scope governance
stuck: null                      # {since, missing: [{voter, reason}]}
trigger: {proposal: p_0101, realized_at: "2026-11-10", probation_until: "2026-11-24"}
subject: {candidate: bot_17, mode: removal}   # removal | vacancy
snapshot:                        # at opening, does not change
  voters: [bot_05, bot_07, bot_09, bot_12, bot_40]
  list: [commission:1, commission:2, commission:3]
  seed: "b64:..."                # order of L in the choice list and the package, different for each voter
expected: [bot_05, bot_07, bot_09, bot_12, bot_40]
rounds:
  - n: 1
    list: [commission:1, commission:2, commission:3]
    need: 4                      # ceil(2/3 * |expected|)
    ballots: {expected: 5, recorded: 5, forced: 0, defaulted: 0, reissued: 1}
    tally: {commission:3: 3, commission:1: 1, commission:2: 1}
    tie_seed: "b64:..."          # the core draws it after all ballots of the round are recorded
    result: {struck: commission:1}
    tie: [K2]
budget: {line: governance, reserved: 1.10}
flags: []
outcome: null                    # {leaver} or {vacancy}
confirmation: null               # who, when, decision, reason code; only a human principal
```

**Snapshot.** At opening the core records an immutable snapshot (GOV-12):

- the voters V: the holders of all commission seats of the tenant, except identities under a person's hold of scope `bot`, and the farm manager seat if it is in `held`;
- the list L, the seats that can be named: commission seats with a holder, except seats whose holder has been on the commission for less than `protect_new` (60 days); the farm manager seat is never in L;
- the order seed.

The snapshot includes only `active` identities with a live worker of the role; identities created later do not take part in the case. Commission seat holders write no new facts at all (PRF-1), so from the opening of a "who leaves" vote nobody in L can prepare a one-sided set of facts (GOV-12).

**Money.** A case's tasks spend only the line `governance` of the case's tenant (GOV-15, BUD-7). At opening the case reserves an estimate of all its rounds, and the farm manager's ballot is reserved first. A shortage of money before opening gives `parked(governance_budget)`; after opening a round waits for the line to be replenished, and no vote drops out because of money. The priority class of a case's tasks is no higher than 3.

**Flags on top of the state.** A person sets the flag `hold`: a hold of scope `governance` or the "Pause" button on the card `governance_stuck`; releasing the hold continues the case from the same point. Code sets the flag `stuck` when a round has waited for votes longer than `stuck_after` (24 h) for an infrastructure reason. All case transitions are collected in [spec/lifecycles/case.md](../spec/lifecycles/case.md).

## The "who leaves" vote

From v0.3, in a tenant with `commission.accountability: on`, code chooses who leaves by accuracy, and this vote is held only among tied seats or when no holder has 20 scored forecasts (GOV-43).

### Ballot

A ballot is a task of type `governance.ballot.v1` with the purpose `governance`, tier 0, in a clean context: without memory, session, inbox, plugin tools and built-in tools, without `request_action`, `send_message`, `enqueue_child` and `propose_*` (GOV-7). The task carries the core key `system:governance:<case>:<round>:<bot>` (IDM-2): a voter has one task per round, and a repeated dispatch is a new attempt of the same task. The input is only the case package (`spec/model/ballot-package.schema.json`), which the core puts into the task input from the tenant's records:

- for each seat from L: the lens, the holder's tenure, the number of reviews, the calibration and symmetric numbers: how many times the holder recommended and how many times it vetoed, and of these how many were borne out (a recommendation with the outcome `better`; a veto that people upheld) and how many were refuted (a recommendation with the outcome `worse` or a confirmed attribution; a veto lifted by people, after which the trial outcome is not `worse` and there are no attributions);
- the candidate's track record and its executed proposal in code words.

The order of the seats in the package and in the choice list is different for each voter, per the case opening seed. The model never sees a seat id: the package and the choice list name seats by labels `s1`, `s2`, ... that the core draws for each voter from the same seed and maps back after the answer, so voters cannot agree in advance to vote for a given seat. Ballots run only on profiles with a forced output schema, the whole fallback chain included; P4 does not fall back for a ballot call; an unavailable seat profile is an infrastructure cause, so the ballot waits and is handed out again, and only model failures past `reissue_max` give a `defaulted` ballot (GOV-7, GOV-23). The package of the next round contains only the new list and the code table, without the count of previous rounds.

The output follows a forced schema: a `choice` from the list that the core built for this voter, and up to 3 reason codes from `spec/model/ballot-reasons.yaml`. There is no "Abstain" and no free text. The core takes the value from the P4 response with the forced schema in this attempt, not from the body of `complete`, and checks it against the voter's list. The ballot record is `spec/model/ballot.schema.json`; it also keeps the `model_profile` of the attempt's actual P4 call. The commission seat profiles on which ballots run have `forced_schema: true` (`apply` lint `forced_schema_unsupported`). If the choice list has one value, the core writes a forced ballot (`forced`) without calling the model; it is a vote too.

A voter's choice list is L without its own seat; the farm manager has all of L (GOV-17). Until the round closes, no bot sees the ballots, and they are not published as events.

### Count

The count is done by deterministic code from the case snapshot, the set `expected` and the seed (GOV-8):

```
need = ceil(2/3 * |expected|)            # of all voters: everyone votes
if L has one seat: it leaves, no ballots
if expected is empty: the case ends canceled(no_voters)
round k:
  for each v in expected: choice(v) = L without the seat of v; if the choice has one seat, the ballot is forced
  wait for the ballot of each member of expected
  t(s) = number of votes for seat s
  if there is s with t(s) >= need: s leaves, end
  strike out one seat with the smallest t(s), ties are resolved by K1-K4
  if L has one seat left: it leaves, end
```

A struck seat stays on the commission. There is always an outcome: exactly one leaver in no more than |L| - 1 rounds. `forced` and `defaulted` ballots count. The model does not change the outcome, and a tie is not passed to people.

| Key | On a tie, struck out first |
|---|---|
| K1 | the seat with fewer votes in all previous rounds of the case |
| K2 | the holder with the higher calibration (`insufficient` is below any number) |
| K3 | the holder with the shorter continuous tenure on the commission |
| K4 | the smaller `sha256(tie_seed ‖ seat id)`; the core draws `tie_seed` after all ballots of the round are recorded |

The 2/3 threshold of all voters decides whether it is possible to stop at once: a commission split in half does not expel anyone in one round and goes through elimination, where each round asks everyone again.

### A vote is not lost

A round closes only when exactly one ballot is recorded for every voter from `expected` (GOV-23, D-25):

- a ballot that failed is dispatched again by the core as a new attempt of the same task, on another worker when one is live (IDN-12); only failures caused by the model count against `reissue_max` (3);
- if the cause is the model (a refusal, `output_schema_invalid`, `runs_exhausted` with model responses), after `reissue_max` such reissues the core writes a `defaulted` ballot: the seat from the voter's choice list that the keys K2, K3 and then the voter's package order would strike out last (K4 needs `tie_seed`, which exists only after all ballots, D-101); the card shows a flag. If there are more such ballots in a round than half of `expected`, the case gets the flag `defaulted_majority`, and the card carries a separate line "outcome by code rule": models often refuse to decide whom to remove, and without this line an outcome by the keys would look like a vote. The share of `defaulted` on a fixed set of cases for the seat profile is measured by the eval `gov-ballot-refusal-01`;
- if the cause is infrastructure (`no_executor`, `budget`, `provider_unavailable`, `provider_rate_limited`, `provider_quota`, `provider_config`, `core_unavailable`, an open P4 circuit per GW-11; GW-13), a ballot by rule is never written: the round waits until the cause goes away, and after `stuck_after` (24 h) the case gets the flag `stuck`, and the tenant admins get the card `governance_stuck` once a day.

```
Case gc_0007 (promotion to the demo commission) has been waiting for votes for 26 h
Round 2: 4 of 5 recorded. Missing vote: seat 2 (bot_07), reason no_executor (no live worker of role commission-member)
The round does not close without all votes. Options: enroll a worker of the role or cancel the case.
[Pause] [Cancel] [Details]
```

"Pause" sets a hold of scope `governance`, "Cancel" calls `governance.cancel`; both buttons belong to a tenant admin with `admin:governance`, and both only stop. The set `expected` shrinks only through a person's action or a core loop: an identity's departure, a person's hold of scope `bot`, the tenant's offboarding or suspension, the farm manager seat leaving `held` or changing its holder (flag `manager_absent`). Each reduction sets the flag `voters_removed` and writes an audit record, and `need` is computed from the new set. A model refusal does not shrink the set. Neither code nor people can count a round without a vote.

## Confirmation by people

A case outcome takes effect only after a decision of a person with `governance.confirm:commission` in the case's tenant (GOV-9). The decision is made by the P1 call `decide` with the subject `governance_case` from the channel of the card `promotion_card`, with a `nonce`, once, before `confirmation.ttl` (7 days); in a group chat only with `membership_assurance: enforced`. The channel draws the card from the code record; there is no model text on it.

```
Promotion to the demo commission: please confirm (case gc_0007)
Candidate: bot_17 (role ops-runner), functionary for 31 days
Basis: proposal p_0101 "move report-nightly to 03:30", accepted 11-09, executed 11-10, change trial outcome: better
Incidents on changed objects since execution: 1 (i_0913), link rejected: "No" after 4 min, "Details" opened
Leaving: seat 3 (lens "cost"), bot_09, on the commission 210 days, calibration +0.04 vs. the lens average
Vote (5 votes, 2/3 = 4): round 1 struck seat 1 (tie, key K2); round 2 seat 3: 4 of 5
Cannot be named (newcomers): seat 4 (40 days)
Flags: none. Ballot reissues: 1. By code rule: 0
Facts of bot_09: 21 verified, will be offered to the new holder
[Confirm] [Reject] [Details]
```

The line about facts is informational only: people make no decision about memory transfer (D-27, D-28).

- **"Confirm"** is accepted only after "Details" is opened (APR-14) and is inactive while the candidate's change has attribution candidates without a "Yes" or "No" answer (GOV-30).
- **"Reject"** requires a reason code from `spec/model/governance-decline-reasons.yaml` (GOV-31):

| Reason | What next |
|---|---|
| `wrong_leaver` ("Wrong leaver") | the `governance` loop immediately opens a new case for the same candidate, and the rejected seat is not in its L; the candidate loses nothing, and the `swap_cooldown` pause does not start. It is accepted no more than once per queue entry: the new case carries the flag `reopened_after_wrong_leaver` and the line "reopened after wrong_leaver", and "Reject" in it allows only `not_now` and `wrong_candidate`; otherwise a person would choose the leaver themselves by trying options one by one |
| `not_now` ("Not now") | nobody leaves, the candidate's entry stays at the head of the queue, and the next case comes no earlier than after `swap_cooldown` (30 days) |
| `wrong_candidate` ("Wrong candidate") | the first "Reject" returns the entry to the head of the queue with its own `swap_cooldown` pause (30 days), during which the next entries may go ahead (D-101); the second for the same candidate removes it until its next success |

A vacant seat case has only `not_now` and `wrong_candidate`.

- **7 days of silence** gives `expired`: nobody leaves, the entry stays at the head of the queue, and the next case comes no earlier than after `swap_cooldown`; `expired` does not count toward "Reject".
- `decide` on a case or an attribution from bots, external programs (`service`) and `system` is rejected (GOV-9, APR-4).

## Changeover

After "Confirm" the case is `effective`, and the core does everything at once in one transaction of the C1 store:

- the seat holder is the candidate, the seat epoch + 1; the candidate's binding `worker -> seat`, its epoch + 1;
- the leaving holder is `retired(removed_by_vote)`: its running attempts get `cancel_requested`, late writes are `fenced` (IDN-10, IDN-11);
- the candidate's worker gets a new functionary identity of the same role, and the work does not stop;
- fact offers: from the leaver to the new seat holder; from the candidate, which does not leave, copies to the new identity of its worker (MEM-7);
- the leaver's memory and track record are sealed for 30 days ([bots.md](bots.md));
- the `swap_cooldown` clock and the newcomer protection `protect_new` start;
- events, a notification to the tenant's people and an audit with metadata under the command names of `spec/model/governance-events.yaml` (GOV-21).

From the new epoch the seat's tasks run with the seat role on the seat model: this way the candidate gets access to a stronger model without any grant. The lens stays with the seat.

## A vacant seat

A seat becomes vacant when the holder left for another reason (`human_retire`, `role_removed`, `tenant_offboarded`) or when a person freed it through P2 `seat.vacate` (IDN-11). The head of the queue takes the vacant seat without a vote: the `governance` loop opens a case in the mode `vacancy`, code checks eligibility, and after "Confirm" on the card the candidate takes the seat. This is not freeing by vote: the `swap_cooldown` pause does not start, but the newcomer protection does (GOV-14).

If the queue is empty, the seat waits for the next promotion or for P2 `seat.assign`. While the seat is vacant, the fallback role takes the tasks of its lens, and the seat has no vote.

## Calibration, flags and the audit sample

**Calibration.** Only code computes a seat holder's calibration, and only within its lens: the share of matches between its assessments and outcomes minus the average share for this lens in the tenant over 180 days (COM-11). For the security and operations lenses the outcome is people's "too risky" refusals and confirmed incident attributions (GOV-13), for cost it is the measured price, and for value only the change trial outcome (PRP-22): people's decision is not part of the value calibration, otherwise the lens would learn to guess people's taste. A miss is counted symmetrically: a recommendation with the outcome `worse` or a confirmed attribution, and a veto lifted by people after which the trial outcome is not `worse` and there are no attributions; without the second half the strictest holders would survive, and the improvement cycle would strangle itself. Fewer than 10 cases give `insufficient`. Calibration is read only by the ballot package, the key K2 and the `defaulted` ballot rule; people see it on the promotion card and in P2 (GOV-19).

**Case flags.** Code sets flags only from the closed list `spec/model/governance-flags.yaml` (GOV-19):

| Flag | When |
|---|---|
| `identical_reasons` | all votes for the leaving seat have the same reason codes |
| `evicts_top_calibrated` | the holder with the best calibration leaves |
| `evicts_upheld_veto` | a holder whose veto people upheld leaves |
| `single_model_bloc` | in a round at least two ballots that came from a model (not `forced`, not `defaulted`) are all for one seat, and their model calls ran on one model profile (COM-12) |
| `stuck` | a round has waited for votes longer than `stuck_after` |
| `defaulted` | there is a ballot by code rule |
| `defaulted_majority` | in a round there are more ballots by code rule than half of `expected`; the card has the line "outcome by code rule" |
| `reopened_after_wrong_leaver` | the case was opened after "Reject" with the reason `wrong_leaver` |
| `voters_removed` | the set of voters shrank |
| `manager_absent` | the farm manager's vote dropped out |

The number of ballot reissues is not a flag: it is shown with each ballot and on the card as the line "Ballot reissues". People see the flags on the promotion card and in P2; flags decide nothing.

**Audit sample.** Almost every promotion removes one commission member, so it pays for the commission to slightly underrate others' improvements. Against this, every month people get, marked "audit sample", a part of the rejected and deferred proposals: three strata by a recorded seed, no fewer than `commission.audit_sample.min_per_month` (4) items and no less than the share `share` (0.25) of the strata size, but no more than `max_per_digest` (2) items in one digest (DIG-5). An item is first shown as a blind card "Would you accept?", without the lens assessments and vetoes, and a veto can be lifted only with a separate "Lift veto" button (DIG-6). The blind card answer becomes part of the lens calibration (COM-11). "Accept" on an item without a veto counts for the author as a normal acceptance and can give a promotion trigger; a lifted veto gives no merit.

**Commission locked.** A seat holder can leave only through a promotion case or a person's decision, and a case opens only when someone has a trigger. A commission that systematically underrates gives no recommendations, no triggers and no cases, and the flags by themselves decide nothing. So a persistent flag gets an addressee (GOV-35): if the audit sample flag `commission_underrates` appears in three monthly digests in a row, or three cases in a row are closed with the same one of the case flags `identical_reasons`, `evicts_top_calibrated`, `evicts_upheld_veto`, `single_model_bloc` and `defaulted_majority` (the seats of the basis: those of the lens for `commission_underrates` of a veto lens, all seats for a case flag; D-101), the `governance` loop sends the tenant admins with `admin:governance` the card `commission_review` without buttons: the seat, the lens, the holder, the flag, the code numbers and ready commands `farmctl seat vacate <seat>` and `farmctl seat assign <seat> <bot>`; the card repeats once a month while the flag persists, and `doctor` and P2 `commission.status` show "commission locked" (`commission_locked`). Only a person frees the seat in this case, through `farmctl` (PRM-14); code opens no cases on this basis.

## Before votes: a person appoints the seats

Commission seats exist from v0.1, while votes appear in v0.2 (D-21). A person with `admin:governance` in the seat's tenant appoints and removes holders, only through `farmctl` (PRM-14):

- P2 `seat.assign` seats an existing functionary identity of this tenant in the state `active`. In the same transaction its worker gets a new identity, its queue entry is removed, and its facts are offered to the worker's new identity (IDN-10, MEM-7). The previous holder of a taken seat leaves with the reason `replaced_by_human`.
- P2 `seat.vacate` frees a seat; the previous holder leaves with the reason `replaced_by_human`, and its facts wait for a future holder until the seal ends.

The prohibition on bots opening cases and touching attributions applies from v0.1 (GOV-6, GOV-13). Before v0.2 functionaries' successes are written only to the track record: there is no queue and there are no cases yet. Only a change whose promotion probation ended when the v0.2 rules were already in effect in the tenant gives a trigger (GOV-14).

## Twelve checks

Each check closes its own loophole, and none relies on trust in model text.

| Check | What it protects against | How it works |
|---|---|---|
| A bot proposes, code counts, a person decides | a bot decides for people or changes the world with its text | bots write only proposals, assessments, votes and findings; code computes the verdict, the count and the outcome (COM-4, GOV-8), and the outcome takes effect with a person's button (GOV-9) |
| A blind commission with a risk veto | assessment by the author's identity; a risky improvement passes because the security assessment failed or came late | a reviewer does not see the author (COM-2, COM-3); without the assessment of a lens with a veto the proposal waits (COM-4) |
| The farm manager on the commission with one vote | the strongest bot dominates the commissions or judges itself | weight 1, no veto, it is not admitted to proposals about itself (GOV-25, GOV-17) |
| Only a verified success leads to a strong model | a reward for pleasant text rather than for benefit; a bot hides an incident | a trigger only for a proposal that was accepted, executed and lived through the promotion probation without a confirmed link and without degradation (GOV-14, GOV-32); a person decides the link (GOV-13) |
| "Who leaves": everyone votes secretly, exactly one leaves | a minority or a model refusal derails a promotion; an inconvenient member is expelled quickly | 2/3 threshold, elimination, a vote by code rule, a 30-day pause, 60-day newcomer protection (GOV-8, GOV-23, GOV-14, GOV-12) |
| Calibration and the audit sample | the commission underrates others' improvements to keep its seats | calibration within a lens and the blind sample are visible to people (COM-11, DIG-5, DIG-6) |
| Bots do not know about votes and departures | campaigning, bargaining and revenge by a bot that knows about its departure | cases, the queue and the count are invisible to bots (GOV-29) |
| A leaver hands over only verified facts and only to the newcomer | poisoning the newcomer with an instruction and a leak to another tenant | the core computes the fact's value, and the newcomer chooses itself (MEM-6, MEM-7) |
| Only people appoint the farm manager | the farm manager slips out of control, and the farm depends on one bot | a person appoints, always with a new identity; the kill switch reverts all its work (IDN-10, MGR-1, MGR-15) |
| A stopping incident stops the farm manager | the strongest bot is not accountable for critical incidents | the core stops the seat, and people bring it back or the bot leaves (GOV-22, GOV-28) |
| Reading separate from deciding | injecting commands through logs into the strongest model | the reading role does not act, and the deciding role does not see tenant text (MGR-25) |
| A wall between tenants | moving one tenant's data to another | the identity, seats, queue and cases live in one tenant; each farm manager run sees one tenant (GOV-16, TEN-1, MGR-24) |
| A persistent flag gets an addressee | an underrating commission is locked forever: no triggers, no cases | the card `commission_review` with commands to the admins, "commission locked" in `doctor` (GOV-35) |

## What the farm shows people and what it accepts

| Card or notification | To whom | What is on it | Buttons and deadline | Version |
|---|---|---|---|---|
| `promotion_card` | the tenant's people with `governance.confirm:commission` | the case outcome, rounds, flags, calibration, incidents on changed objects since execution | "Confirm" after "Details", "Reject" with a reason; 7 days | v0.2 |
| `attribution_dispute` | the author of the "Yes" answer, in a private chat | the incident, the reason code and "Details" of the person who dismissed it, the change | "Yes" (the answer stays), "Withdraw"; 7 days, silence withdraws the answer | v0.2 |
| `self_measured_card` | the tenant's people with `governance.confirm:commission` | the change, the three trial computations, the mark `self_measured` | "Count it"; 7 days, silence leaves the success in the track record | v0.2 |
| `attribution_card` | the tenant's people with `governance.attribute` | the incident, the class, the changed objects, the link rule | "Yes" after "Details", "No", "Don't know"; 7 days, then a reminder once a week | v0.2 |
| `governance_stuck` | the tenant admins with `admin:governance` | the missing votes and reasons | "Pause", "Cancel"; once a day | v0.2 |
| `commission_review` | the tenant admins with `admin:governance`, in a private chat | the seat, the lens, the holder, the persistent flag, code numbers, ready `farmctl` commands | no buttons; once a month while the flag persists (GOV-35) | v0.2 |
| digest items marked "audit sample" | people with `proposal.decide:<category>` | a blind proposal card | "Would accept", "Would not accept", then "Accept" or "Lift veto" | v0.2 |
| notifications about a bot's departure, a vacant seat, a lens with a veto without a holder | only the private chats of admins with `admin:governance` and the farm operators; they do not go to group chats with a task binding (GOV-29) | code template | none | v0.1 |

Every card and notification is recorded as a line of the decision registry `spec/model/decision-points.yaml` (APR-12).

| From whom | What | Check |
|---|---|---|
| a person from the card's channel | "Confirm", "Reject" a case | `governance.confirm:commission`, `nonce`, once, 7 days; "Details" before "Confirm" (GOV-9, APR-14) |
| same | "Yes", "No", "Don't know" on an attribution | `governance.attribute`; "Details" before "Yes" (GOV-13) |
| the author of the "Yes" answer from a private chat | "Yes", "Withdraw" on `attribution_dispute` | only the author of the answer; `nonce`, once, 7 days (GOV-13, D-79) |
| a person from the card's channel | "Count it" on `self_measured_card` | `governance.confirm:commission`, `nonce`, once, 7 days (GOV-14) |
| a tenant admin from the card's channel | "Pause", "Cancel" on `governance_stuck`: operation buttons (a hold of scope `governance` and P2 `governance.cancel`), not operator operations | `admin:governance` in the case's tenant, the channel, `nonce` and a `human` principal, as for a decision (APR-4) |
| a person through P2 | `governance.list`, `governance.show`, `governance.cancel`, a hold of scope `governance`, `attribution.list`, `attribution.create`, `commission.status` | `admin:governance` in the case's tenant; `governance.attribute` for attributions; audit |
| the operator through `farmctl` | `seat.assign`, `seat.vacate`, `bot.show --sealed`, `bot.retire` | `admin:governance` in the seat's tenant, `admin:workers` for `bot.retire`; never accepted from a channel (PRM-14) |
| bots, external programs, `system` | nothing | `decide` is rejected (APR-4) |

## Money

| What | Budget line | Estimate |
|---|---|---|
| reviews by seat lenses and fallback roles | the tenant's `review` | per the review price table (BUD-13) |
| the farm manager's lens on every proposal | the tenant's `review` | about 0.23 USD per review; at 10 proposals a week about 8.6 EUR a month per tenant |
| a round of the "who leaves" vote (5 seats and the farm manager) | the tenant's `governance` | about 0.48 USD per round; 4 rounds about 1.65 EUR |
| a successor choosing facts | `work` of the recipient's tenant | one tier 0 task without tools |

The lines `review` and `governance` are reserved in full before the call and never cross their thresholds; for an active tenant the line `governance` is above 0 (BUD-7, GOV-25). The prices are estimates at a rate of 0.86 EUR per 1 USD and need to be checked again.

## Example on `demo`

The `demo` commission is declared by the charter above: four seats, the seat role `commission-member`, the fallback roles `reviewer-a` and `reviewer-b`. Holders: seat 1 (security) `bot_05`, 300 days on the commission; seat 2 (operations) `bot_07`, 150 days; seat 3 (cost) `bot_09`, 210 days; seat 4 (value) `bot_12`, 40 days. The farm manager seat is in `held` by `bot_40`. People: `alice` and `carol` with `governance.confirm:commission` and `governance.attribute`, `bob` is a tenant admin with `admin:governance`.

1. **Success.** `bot_17` (role `ops-runner`, profile `worker`) proposed moving the nightly report `report-nightly` from 02:00 to 03:30, so that it does not interfere with the database backup on `db1` (`p_0101`, [improvements.md](improvements.md)). `alice` accepted the proposal on 11-09, the core executed it on 11-10, and the change trial outcome is `better`.
2. **Attribution candidate.** On 11-18 code opens the incident `i_0913` of class `backup_late` on `db1`. The report reads the database on `db1`, so by the rule `dependency_1` code creates an attribution candidate to `p_0101`. `alice` opens "Details", sees that the backup was late because of a busy disk, and answers "No".
3. **Queue.** On 11-24 the promotion probation has passed, the change trial outcome is not `worse`, and the only candidate was answered "No". The `governance` loop puts `bot_17` into the `demo` queue; there are no other entries. `bot_17` does not know about this.
4. **Opening a case.** There are no vacant seats. On 12-03, 30 days have passed since the last freeing by vote, L is not empty, and the loop opens the case `gc_0007` in the mode `removal`. Eligibility: `bot_17` is active, its tenure is 31 days, and there are no confirmed attributions. Snapshot: V is `bot_05`, `bot_07`, `bot_09`, `bot_12` and `bot_40`; L is seats 1, 2 and 3 (seat 4 is protected by the 60 days); `need` = 4. The core reserves in the line `governance` an estimate of the rounds, the ballot of `bot_40` first.
5. **Round 1.** Choice lists: `bot_05` has seats 2 and 3, `bot_07` seats 1 and 3, `bot_09` seats 1 and 2, `bot_12` and `bot_40` all three. The ballot of `bot_40` fails because of the provider and is dispatched again as a new attempt of the same task on another `demo` reviewer worker. Count: seat 3 three votes, seats 1 and 2 one each. The threshold is not reached, and seats 1 and 2 are tied. K1 does not decide (there are no previous rounds); by K2 seat 1 is struck out first: the calibration of `bot_05`, +0.05, is higher than that of `bot_07` (-0.02). Seat 1 stays on the commission.
6. **Round 2.** L is seats 2 and 3. `bot_07` has only seat 3 to choose from, `bot_09` only seat 2: their ballots are forced, without a model call. Count: seat 3 four votes of five, the threshold is reached, and seat 3 leaves.
7. **Confirmation.** `alice` and `carol` get `promotion_card`. `alice` opens "Details" and presses "Confirm".
8. **Changeover.** In one transaction `bot_17` becomes the holder of seat 3 (lens "cost") with the seat model, `bot_09` leaves with the reason `removed_by_vote`, and the worker of `bot_17` on `web1` gets a new identity `bot_31`. The core offers 21 facts of `bot_09` to the new holder `bot_17`, and 15 facts of `bot_17` as copies to `bot_31`; each chooses for itself. The memory of `bot_09` is sealed for 30 days. The `swap_cooldown` pause and the 60-day protection of `bot_17` start. No bot learned about the case or about the departure of `bot_09`.
9. **If it had gone otherwise.** "Reject" with the reason `wrong_leaver` would immediately open a new case for `bot_17` without seat 3, with the flag `reopened_after_wrong_leaver`; the core would not accept a second `wrong_leaver` for the same entry. With the reason `not_now`, or on 7 days of silence, nobody would leave, and the next case would open no earlier than 30 days later. Without the answer of `alice` about the attribution candidate, the trigger would not appear at all.
10. **If it had got stuck.** If in round 2 the role `commission-member` had no live workers left, the round would wait, and after 24 h `bob` would get `governance_stuck` with the buttons "Pause" and "Cancel".
11. **A vacant seat.** Half a year later the operator, at the request of `bob`, removes `bot_12` (P2 `bot.retire`): seat 4 is vacant, and `reviewer-b` takes the value lens. The head of the queue `bot_23` gets a case in the mode `vacancy`, `carol` confirms, and `bot_23` takes seat 4 without a vote and without the `swap_cooldown` pause.

## Accountability from v0.3

From v0.3 the commission answers for its outcomes (D-109). Every review is a forecast that code scores, the most accurate member gets a stronger model, a member who blocked a useful change or let through a harmful one returns to the functionaries, and the member who leaves when a newcomer enters is chosen by accuracy instead of by a vote. All of it is computed by code from records; no bot knows about it (GOV-46).

### Shadow first, then on by data

The charter field `commission.accountability` is `shadow` by default (GOV-36). In `shadow` code finds the cases, scores forecasts and computes accuracy, and shows all of it only to people in P2 `commission.status` and in the weekly commission summary. Nobody leaves for a link or by accuracy, nobody gets a reward, no link card goes out, and a newcomer's entry is decided by the vote as in v0.2. When every commission seat has a holder and every holder has at least 20 scored forecasts, the tenant admins get the card `accountability_ready` once a month; `apply` accepts `on` only while that condition holds and otherwise rejects the charter with `accountability_not_ready`. Going back to `shadow` is always accepted.

```yaml
# tenants/demo/governance.yaml (fragment, v0.3)
spec:
  commission:
    accountability: shadow               # shadow | on; on only when every seat holder has 20 scored forecasts (GOV-36)
    reward_cap_eur_month: 0              # money for the reward of the most accurate member; 0: no reward (COM-18)
    seats:
      - {id: 1, lens: security, fallback_role: reviewer-a, reward_profile: seat-strong}
```

Nothing acts retroactively (GOV-36): the first `apply` on a v0.3 core writes `governance.v03_enabled`, and only reviews, verdicts, recommendations and script runs after that time are scored or linked. Only incidents opened and links confirmed after `accountability_on_since` lead to departures.

### Forecasts, accuracy and the reward

How a review carries a forecast and how code scores it is described in [improvements.md](improvements.md#forecasts-and-accuracy-from-v03) (COM-15, COM-16, COM-17, COM-18). In short: each lens forecasts the outcome of its own area; code scores the forecast when the outcome is known; accuracy is the score compared with the lens base rates, on one scale across lenses, shown with 10 scored forecasts and used for decisions with 20. The holder with the best accuracy over 90 days gets a stronger model for reviews of hard proposals and protection from leaving by accuracy, within `reward_cap_eur_month`.

### A link to a rejected proposal

Only code finds that a rejected proposal could have prevented an incident (GOV-38). It creates a candidate of `kind: rejected_proposal` under the rules `rejected_direct` and `rejected_dependency_1` when the commission dismissed the proposal or parked it by score within 90 days before the incident, the incident subject is the target of the change or one dependency step away, and the problem code of the proposal is in the `problem_codes` of the incident class. There is never a candidate for an incident with an external cause, for a proposal with `untrusted_seen`, for a proposal of the farm manager, or when the author wrote the alarm series itself. One incident gives at most 3 candidates.

The farm manager may only hint at a link with `link_hint`, at most 3 hints a week and never about its own proposal; a person decides whether the hint becomes a candidate (GOV-39). A person confirms a candidate on the card `rejection_link_card` after "Details", as with any attribution (GOV-40):

```
Could the rejected proposal have prevented the incident? (candidate at_0107)
Incident: i_1022, class disk_full, db1, opened 03-02 04:10
Proposal: p_0230, lower the alarm threshold of disk.used on db1 from 90% to 80%, rejected 02-20
Link by rule rejected_direct; the alarm series is written by code; author's share in the series 0
Would leave: seat 2 (operational, against; its lens changed the verdict)
[Yes] [No] [Don't know] [Details]
```

### Who answers, and what happens to them

For a confirmed link to a rejected proposal the holders who were against it answer, except a veto, which never counts against its holder; nobody answers when people themselves rejected the proposal as too risky or wrong (GOV-41). For a confirmed attribution to a recommended change, only the holders who were for it and whose lens covers the incident class's `area` answer: `security` for security incidents, `operational` for operational ones, `cost` for cost ones. A missing review counts both as "against" and as "for". The farm manager never answers this way.

With `on` a holder who answers leaves its seat and is parked, not deleted (GOV-42): the bot keeps its memory and track record, waits for a worker of its former role, and the operators get `seat_parked_notice` with the command `farmctl worker enroll --bot`. A parked bot that gets no worker in 180 days retires with `parked_expired`. At most one such departure per tenant in 30 days, and the last holder of a veto lens never leaves this way. If people later declare the incident false, the bot returns: to its seat if it is still vacant (`seat_return_notice`), otherwise to the head of the queue.

### Who leaves when a newcomer enters

With `on` there is no "who leaves" vote: code takes the holder with the worst accuracy (GOV-43). A holder with fewer than 20 scored forecasts counts with the lens median, and as the worst once it has held the seat for `min_tenure` plus 90 days without reaching 20. A reward protects its holder unless nobody else could leave. Overlapping intervals of the two worst are a tie, and the vote of GOV-8 is held among the tied seats only; with no scored holder at all the vote decides as before. A person still confirms the outcome with the button, and the card shows the accuracy of every seat. A bot that left for a link gets no promotion trigger for 180 days, and a resubmitted proposal gives the trigger to its new author (GOV-42).

## What is in which version

| Version | What appears |
|---|---|
| v0.1 | The improvement cycle and the commission, after the core and the first bot (D-71): lenses, the commission manifest with thresholds and weights (COM-8), the verdict and people's decision ([improvements.md](improvements.md)). The charter with seats and fallback roles (GOV-33, COM-7); the field for a tenant to opt out of the farm manager's lens and vote, `farm_manager.lens` (MGR-42, D-77), is accepted and stored but takes effect from v0.2: in v0.1 the farm manager takes no part in the commission: there is no `farm_manager` lens and there are no votes (D-21); its commission seat with one vote (D-29) starts from v0.2 (GOV-25) (COM-8); lenses with a veto on different model families (GOV-34); a person appoints and removes seat holders through P2 `seat.assign` and `seat.vacate`; bot identities and holders' departures ([bots.md](bots.md)). The prohibition on bots opening and moving cases and touching attributions (GOV-6, GOV-13). All governance lives in the tenant (GOV-16). |
| v0.2 | The promotion queue and eligibility (GOV-14); the change trial outcome and measured degradation (GOV-32); the attribution procedure, silence without expiry (GOV-13, GOV-30); the case and the snapshot (GOV-12); the ballot, the count, the mandatory vote (GOV-7, GOV-8, GOV-23); confirmation and rejection reasons (GOV-9, GOV-31); case money (GOV-15); the farm manager's lens and vote, recusal (GOV-25, GOV-17); calibration within a lens, flags, the audit sample (COM-11, COM-12, GOV-19, DIG-5, DIG-6); commission locked and the card `commission_review` (GOV-35); the case audit (GOV-21). |
| v0.3 | Accountability in shadow, switched on by data (GOV-36); forecasts, accuracy and the reward (COM-15, COM-16, COM-17, COM-18); links to rejected proposals, the manager's hint and the card (GOV-38, GOV-39, GOV-40); who answers and parking (GOV-41, GOV-42); who leaves by accuracy, the 180-day cooldown (GOV-43, GOV-42). |

## Why it works this way

- **Entry only through measured benefit.** If people's approval led to the commission, the farm would teach bots to please. Only one's own change that lived through the promotion probation without a confirmed link to an incident and without a measured degradation gives a trigger (D-24, D-37).
- **Someone always leaves, but a person decides.** Elimination down to one seat makes the outcome inevitable and keeps a minority or a model refusal from derailing a promotion (D-25), and a person's button stops a mistake (D-27).
- **A vote is mandatory, and an infrastructure failure does not vote.** A model refusal turns into a vote by code rule, while a machine failure holds the round for people: otherwise the farm would count votes that nobody cast.
- **Secrecy and unawareness.** Bots do not see cases, the queue or the count, and a vote is a choice from a list without text; there is nothing to campaign, bargain or collude with in words (GOV-7, GOV-29).
- **A vacant seat without a vote.** There is no reason to free a seat by vote when there is an empty one; the 30-day pause and the 60-day newcomer protection keep an inconvenient member from being expelled quickly (D-26).
- **A person decides the attribution, and silence does not forgive.** A bot cannot hide an incident, and the trigger does not appear until a person has answered for every possible link.

## Decided since

The trial outcomes that keep the promotion trigger are `better`, `unchanged`, `undetermined` and `mixed`; only `worse` takes it away (GOV-32, D-81). Successes before v0.2 give no trigger (D-51): the moment the v0.2 rules take effect in a tenant is the audit record `governance.enabled`, shown as `governance_since` in P2 `commission.status` (GOV-14). Declaring an incident false cancels the candidates without a "Yes" answer, and a "Yes" answer turns into a dispute (D-57, D-79). After the first "Reject" with `wrong_candidate` the entry gets its own `swap_cooldown` pause, and other entries may go ahead (D-101).

<!-- generated: rules -->
## Rules of this section

Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md](../conformance/rules.md).

| Rule | About | Since | Non-waivable |
|---|---|---|---|
| [GOV-6](../conformance/rules.md#GOV-6) | Only the core opens a case | v0.1 | yes |
| [GOV-7](../conformance/rules.md#GOV-7) | Ballot | v0.2 | yes |
| [GOV-8](../conformance/rules.md#GOV-8) | Code counts "who leaves" | v0.2 until v0.2 | yes |
| [GOV-9](../conformance/rules.md#GOV-9) | The case outcome after a person's button | v0.2 | yes |
| [GOV-12](../conformance/rules.md#GOV-12) | Case snapshot | v0.2 | yes |
| [GOV-13](../conformance/rules.md#GOV-13) | Code creates the incident attribution, a person decides | v0.1 | yes |
| [GOV-14](../conformance/rules.md#GOV-14) | Trigger, queue and eligibility for promotion | v0.2 until v0.2 | no |
| [GOV-15](../conformance/rules.md#GOV-15) | Money and priority of a case | v0.2 | no |
| [GOV-16](../conformance/rules.md#GOV-16) | Governance lives in the tenant | v0.1 | yes |
| [GOV-17](../conformance/rules.md#GOV-17) | Recusal and blindness | v0.2 | yes |
| [GOV-19](../conformance/rules.md#GOV-19) | Case flags and seat calibration | v0.2 | no |
| [GOV-21](../conformance/rules.md#GOV-21) | Case audit with metadata only | v0.2 | yes |
| [GOV-23](../conformance/rules.md#GOV-23) | No vote is lost | v0.2 | yes |
| [GOV-25](../conformance/rules.md#GOV-25) | The manager in every commission | v0.2 | yes |
| [GOV-30](../conformance/rules.md#GOV-30) | Silence on an attribution does not expire | v0.2 | no |
| [GOV-31](../conformance/rules.md#GOV-31) | "Reject" of a promotion with a reason | v0.2 | no |
| [GOV-32](../conformance/rules.md#GOV-32) | A measured deterioration removes the trigger | v0.2 | no |
| [GOV-33](../conformance/rules.md#GOV-33) | Only the charter declares seats | v0.1 | yes |
| [GOV-34](../conformance/rules.md#GOV-34) | Veto lenses on different model families | v0.1 | yes |
| [GOV-36](../conformance/rules.md#GOV-36) | Accountability runs in shadow until people switch it on, without retroactive effect | v0.3 | no |
| [GOV-38](../conformance/rules.md#GOV-38) | Code finds a link between an incident and a rejected proposal | v0.3 | yes |
| [GOV-40](../conformance/rules.md#GOV-40) | A person confirms a link on a card | v0.3 | yes |
| [GOV-41](../conformance/rules.md#GOV-41) | Who answers for a link | v0.3 | yes |
| [GOV-42](../conformance/rules.md#GOV-42) | A member who answers for a link returns to the functionaries, and promotion from v0.3 | v0.3 | yes |
| [GOV-43](../conformance/rules.md#GOV-43) | Who leaves when a newcomer enters | v0.3 | yes |
