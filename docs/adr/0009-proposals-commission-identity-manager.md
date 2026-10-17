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

- a bot proposes, code counts, a person decides (D-16), or a bot or the commission applies an accepted change by itself;
- the leaver of a full commission proposed by code from forecast accuracy, with a person's button (D-115), or the secret mandatory vote of members that D-25 had;
- an identity, a position and a persona that grant nothing (D-19), or a rank that widens permissions;
- the service tenant `farm` holding only the seat registry, with the farm manager reading tenants through a separate reading bot (D-30, D-33), or a main tenant with a view of all data.

## Decision Outcome

Chosen option: "a bot proposes, code counts, a person decides; an identity grants nothing; `farm` holds only the seat registry", because every power the loop could give a bot then goes through code and a person's button. A proposal executes nothing; reviews are forecasts scored by code, and the verdict is computed by code. When a newcomer enters a full commission, code proposes the leaver by accuracy, and the outcome takes effect only after a person's button. A person appoints the commission members until promotion cases exist (D-21) and always appoints the farm manager, each time a new bot (D-33). The manager sees a tenant only through a reading bot without the right to act, and does not carry one tenant's data to another (D-30).

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

- Good, because numbers, verdicts and trial outcomes are code, and an injection cannot choose an outcome.
- Bad, because the commission costs review money within a budget line.

### A secret vote of members

- Bad, because a vote among members invites bargaining and refusals; D-115 removed it with its states and schemas.

### A rank that widens permissions

- Bad, because a bot could gain capabilities by promotion, and permissions would depend on track record.

### A main tenant with all data

- Bad, because the strongest model would read every tenant's text, and the wall would have an exception.

## More Information

Decisions D-16, D-115, D-19, D-21, D-30, D-33, also D-18, D-25, D-109, D-110. Documents: [improvements.md](../improvements.md), [bots.md](../bots.md), [farm-manager.md](../farm-manager.md), [goals.md](../goals.md).
