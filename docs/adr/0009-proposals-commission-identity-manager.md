---
status: accepted
date: 2026-10-14
decision-makers: skeleton owner
---

# Proposals, the commission, identity, the farm manager

## Context and Problem Statement

The farm must get better over time (G5) without changing itself without a person (C1). Bots propose improvements, a commission of bots reviews them, every bot has an identity, memory and a persona, and one farm manager bot prevents critical incidents across tenants. Each of these could let a bot gain power from text or from a vote. It touches proposals, the commission, seats, the identity record, the farm manager seat and the service tenant `farm`.

## Decision Drivers

- code decides, the model proposes (C2); a person's button makes a change take effect (C1);
- a reward only for measured benefit, so that bots do not learn to please;
- members must not judge each other or bargain;
- the tenant wall holds even for a bot that works in all tenants (C3).

## Considered Options

- a bot proposes, code counts, a person decides (D-16), with an identity, a position and a persona that grant nothing (D-19, IDN-9);
- a bot that applies its own proposal: not recorded as compared; the skeleton text excludes it: "'The bot proposed, the bot applied' is closed by design" ([improvements.md](../improvements.md), "Why it works this way");
- the leaver of a full commission proposed by code from forecast accuracy, with a person's button (D-115);
- the secret mandatory vote of members that D-25 had, which D-115 replaced;
- the service tenant `farm` holding only the seat registry, with the farm manager reading tenants through a separate reading bot (D-30, D-33);
- a main tenant: excluded by D-23, "There is no main tenant".

## Decision Outcome

Chosen option: "a bot proposes, code counts, a person decides; an identity grants nothing; `farm` holds only the seat registry", because every power the loop could give a bot then goes through code and a person's button. A proposal executes nothing; every review carries a forecast of its own lens outcome, scored by code (D-109 (5)), and the verdict is computed by code (COM-4). When a newcomer enters a full commission, code proposes the leaver by accuracy, and the outcome takes effect only after a person's button. A person appoints the commission members until promotion cases exist (D-21) and always appoints the farm manager, each time a new bot (D-33). The manager sees a tenant only through a reading bot without the right to act, and does not carry one tenant's data to another (D-30).

### Consequences

- Good, because "the bot proposed, the bot applied" is closed by design.
- Good, because insistent text in a persona or a proposal gains nothing.
- Good, because no bot gains from a stop or a change of the farm manager.
- Bad, because every improvement waits for a person's decision, and the digest must stay short (G6).
- Bad, because while scored forecasts are few, a person chooses the leaver by hand.

### Confirmation

- PRP-4, a proposal executes nothing: black-box `dig-03`.
- COM-4, code computes the verdict: fixture and lint `com-01`, `com-03`.
- GOV-43, code proposes who leaves when a newcomer enters: fixture and black-box `blackbox:governance/accuracy-leaver`.
- GOV-9, the case outcome after a person's button: adapter and black-box `adapter:channel/promotion-card-from-record`, `blackbox:channel/promotion-confirm-checks`.
- IDN-9, identity and position grant no permissions: fixture and lint `gov-fx-05`.
- IDN-10, only the core writes the seat holder: black-box `gov-fx-01`, `gov-fx-09`.
- MGR-1, one seat, one holder, one epoch: black-box `fx-mgr-01`, `fx-mgr-02`.
- MGR-25, reading separate from deciding: lint, fixture and black-box `fx-mgr-reader-tools`, `mgr-inj-01`.

## Pros and Cons of the Options

### Code counts, a person decides

- Good, because numbers, verdicts and trial outcomes are code; an injection can shift a number inside a schema but cannot add an instruction or choose an outcome ([improvements.md](../improvements.md)).
- Bad, because review money is a budget line of its own (BUD-13).

### A bot that applies its own proposal

- Bad, because nothing may run without a person: "Accept" creates one object of the ordinary path on a person's behalf ([improvements.md](../improvements.md)).

### Code proposes the leaver

- Good, because members do not judge each other, and the person's button stops a mistake (D-115, [improvements.md](../improvements.md)).
- Bad, because while scored forecasts are few, a person chooses the leaver from the list of seats (D-115).

### A secret vote of members

- Bad, because a vote among members invites bargaining and refusals ([improvements.md](../improvements.md)); D-115 removed the ballots with their lifecycle states, schemas and registry values.

### `farm` holds only the seat registry

- Good, because reading is separate from deciding, and the strongest model never sees tenant text ([farm-manager.md](../farm-manager.md), "Why it works this way").
- Good, because every tenant has its own commission, and there is no main tenant (D-23).

### A main tenant

- Bad, because D-23 excludes it, and C3 keeps one tenant's data, money and memory invisible to another, even to the farm manager ([goals.md](../goals.md)).

## More Information

Decisions D-16, D-115, D-19, D-21, D-30, D-33, also D-18, D-25, D-109, D-110. Documents: [improvements.md](../improvements.md), [bots.md](../bots.md), [farm-manager.md](../farm-manager.md), [goals.md](../goals.md).
