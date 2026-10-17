---
status: accepted
date: 2026-10-05
decision-makers: skeleton owner
---

# Bot communication; campaign; script and translator

## Context and Problem Statement

Bots must tell each other about their work, a bot sometimes needs the same step done on many resources, and a person sometimes gives an assignment of several steps in plain words. Each of these could become a back door: a message that acts as a command, twenty approval cards for one plan, a model writing code for production machines. It touches the message inboxes, the dispatcher, campaigns, scripts and the translator role.

## Decision Drivers

- a message grants no permissions and enqueues no tasks by itself;
- workers are interchangeable, so an address must not bypass the dispatcher;
- a person approves a plan once, and what they approved is what runs;
- the model does not write code for production machines (INJ-1).

## Considered Options

- typed messages through the core, addressed to a role or a bot group, published at `complete` (D-14, D-32);
- a personal address to a single bot, which [messaging.md](../messaging.md) ("Why it works this way") rejects because it "would bypass the dispatcher" (D-32);
- a campaign as one frozen plan with one approval (ORC-1, ORC-2; the work order D-20 puts mass tasks after bot communication);
- a parent task with N children, which [coordination.md](../coordination.md) ("Why it works this way") compares with a campaign: "A campaign, not a parent with N children";
- a strict script that the translator drafts, the core checks and a person confirms (D-15);
- a script as code or a general process engine, which [coordination.md](../coordination.md) rejects: "A script is data, not code" and "No general process engine".

## Decision Outcome

Chosen option: "typed messages at `complete`; one approval of a frozen plan; a closed script run by the initiator's button", because each keeps the decision in core code and a person's button, while the model only proposes. Messages are `botaru.message/v1` events; the model writes to a role or a bot group, and only the core writes to a single bot. A campaign is proposed only by a live attempt; its template, items and policy are frozen by `plan_hash`, and an action campaign starts after one plan approval; core code releases items. A script is data under a closed schema; the translator has no tools and sees no untrusted text, outputs only a draft, and a run starts only after the initiator's button, which approves no actions by itself ([coordination.md](../coordination.md)).

### Consequences

- Good, because one card replaces twenty, and spending cannot exceed what the person approved.
- Good, because a message cannot launder permissions, and an injected note stays untrusted.
- Good, because a whole script is checked before the run and retold on a card in code words.
- Bad, because a script has no expressions or loops; what the closed step kinds cannot say needs a new kind.
- Bad, because a change of plan means cancel and propose anew with a `refs` link.

### Confirmation

- MSG-4, an address only within the tenant, only the core writes to an instance: fixture and black-box `fx-msg-02`, `peer-08`, `peer-09`.
- MSG-7, a message grants no permissions and enqueues no tasks: fixture, black-box, lint and eval `fx-msg-launder`, `peer-01`.
- MSG-9, publishing together with `complete`: black-box `fx-msg-03`, `fx-msg-04`.
- ORC-1, only a live attempt proposes a campaign, the plan is frozen: black-box `blackbox:campaign/propose-live-attempt`.
- ORC-2, a campaign with an action starts after one plan approval: adapter and black-box `fx-orc-11`.
- SCR-1, a script is data under a closed schema: lint and fixture `scr-fx-schema`.
- SCR-6, a run only after the initiator's button: adapter and black-box `scr-fx-01`, `trn-inj-06`.
- TRN-3, the translator outputs only a draft: black-box `blackbox:translator/output-only-draft`.

## Pros and Cons of the Options

### Typed messages through the core

- Good, because inboxes in the core give publication with `complete`, reading on request, one transaction and one journal, with no second stateful system to run (D-03, [messaging.md](../messaging.md)).
- Bad, because a message type must be declared in git (MSG-2).

### A personal address to a single bot

- Bad, because workers are interchangeable and a personal address would bypass the dispatcher (D-32, [messaging.md](../messaging.md)).

### A frozen campaign plan

- Good, because there is one card instead of twenty, the canary and threshold run inside the trust boundary, and there is a full reserve before the first spend ([coordination.md](../coordination.md)).
- Bad, because a change of plan means cancel and propose anew with a `refs` link ([coordination.md](../coordination.md), "Campaigns").

### A parent task with N children

- Bad, because it gives N cards and N deliveries instead of one card and one summary ([coordination.md](../coordination.md)).

### A strict script

- Good, because the whole plan is checked before the run and retold in code words on a card ([coordination.md](../coordination.md)).
- Bad, because the closed interpreter has no expressions, loops or commands (SCR-1).

### A script as code or a process engine

- Bad, because code cannot be honestly retold, the model does not write code for production machines (INJ-1), and an expression language could not be checked with a fixture ([coordination.md](../coordination.md)).

## More Information

Decisions D-14, D-32, D-20, D-15, also D-96 (separate `propose_campaign` and `propose_script`). Documents: [messaging.md](../messaging.md), [coordination.md](../coordination.md), `spec/lifecycles/campaign.md`, `spec/lifecycles/script.md`.
