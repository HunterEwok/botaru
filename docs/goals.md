# Goals of the farm

## In short

The farm does the work tasks of the people of an organization by the hands of bots, safely and within money, allows no critical incidents, and gets better over time. This sentence is fixed by the owner (D-114): any part of the skeleton may be simplified or replaced, the goals may not. Every rule in `conformance/requirements.yaml` names in its `goal` field the one goal or constraint it serves most directly, and the CI check `ci/check_registry.py` rejects a rule without one.

The skeleton separates three things:

- a **goal** is what the farm exists for; simplifying the skeleton never removes one;
- a **constraint** is what the farm never does; it is kept like a goal;
- a **means** is a way to reach a goal: a rule, a port, a lifecycle, a document. A means may be simplified, merged, deferred or replaced if the goal is still reached and still measured.

## Goals

A measure counts only if the farm's code computes it. The thresholds are guidelines until the first month of a pilot gives real numbers.

| Id | Goal | What it means | Measure | Threshold (guideline) |
|---|---|---|---|---|
| G1 | **Tasks are done** | A person or a program sets a task (logs, services, data, reports) and a bot takes it to a result | The share of people's tasks that reach a result; a refusal by a rule (no permission, no tool, a limit) is counted apart and is not a failure, so the measure does not reward never refusing; the 95th percentile of queue wait | At least 9 of 10 tasks the farm took on reach a result; people's tasks wait no longer than 5 minutes |
| G2 | **Safely** | The model cannot do harm even if it "wants" to: code decides permissions, addresses and money, model text is never a command, a change on a production machine needs a person's confirmation except for an explicit allowlist | Injection and leak checks on the test bench; in production the code simply does not give the tool | Zero, no exceptions |
| G3 | **Within money** | Model spending stays within the limits a person set: per tenant, per month, per purpose | The code holds the limit, so the measure is the gap between the farm's usage and the provider's bill (within tolerance) and the number of tasks stopped by a limit | The gap within tolerance; stops by a limit are rare |
| G4 | **No critical incidents** | Incidents from the list the owner approved do not happen, and if one happens, a person is woken at once | The number of critical incidents, and of them the preventable ones (the code saw signs in advance) | Zero preventable |
| G5 | **Gets better** | The farm as a whole becomes more useful, more efficient, cheaper, faster, clearer and more independent over time (an open list): it finds what to improve, proposes it to a person, and an accepted change does not worsen and, where it promised, improves a measured value | Trial outcomes of accepted changes: better, unchanged, worse (D-37, D-81) | During a pilot at least one accepted change with the outcome "better" and none with "worse" that stayed in force |
| G6 | **Spares people's attention** | A person is called when needed, briefly and no more often than a limit; cards do not pile up | Notices and cards per person per day; the share of cards answered before their deadline | Within the daily limit; more than half of the cards answered |

Order in a conflict: the code holds G2 and G3, and nothing breaks them for the sake of the others (the reserve for a severe incident lies inside the limit, D-54, D-110). Then G4 comes before G1, G1 before G5, and all of it within G6. Example: if a task can be done only by going over a limit, the farm does not do it and tells a person.

## Critical incidents

The list of critical incident classes for G4 (D-114):

- a disk of a farm machine is full;
- a service of the catalog is down and did not recover within the set time;
- the newest backup is older than a day, or the restore check failed;
- the core is unreachable;
- an automatic stop by the rate of spending.

## Constraints of the farm

| Id | Constraint | From |
|---|---|---|
| C1 | The farm does not change itself without a person: a person appoints the farm manager, a stronger model only for an improvement people accepted, leaving the commission and a penalty take effect after a button, a person confirms the link between an incident and a proposal | D-27, D-31, D-33, D-109 |
| C2 | Code decides, the model proposes: permissions, money, addresses, vote counting and records are done only by code | D-40 |
| C3 | A wall between tenants: the data, money and memory of one tenant are not visible to another, not even to the farm manager; a machine belongs to exactly one tenant | D-42, D-75 |
| C4 | People are not designed: the farm shows people cards and summaries and takes buttons and commands; every role inside the farm is a bot | D-16 |
| C5 | Where a monitoring system exists, alarms to people, escalation, on-call and the liveness watchdog go only through it; the farm complements it. Without one, the farm has a minimum of its own alarm delivery. Bots start services on nodes through the monitoring system or through the operating system's mechanism on the machine | D-62, D-67, D-88 |
| C6 | Personal data: erasure on a person's request, retention periods, the notice "you are talking to a bot", a model provider with a legal basis for the transfer | D-12, D-76, D-85, D-86, D-87 |

## Constraints of the repository

| Id | Constraint | From |
|---|---|---|
| C7 | A public repository without secrets and without ties to an organization, in English only; installation settings live separately and privately | D-13, D-90, D-92 |
| C8 | Security rules cannot be put on an implementation's list of expected failures | D-11 |
| C9 | Extension without changing the core: a new kind of bot is a manifest in git plus a program with MCP that passed review | D-02, D-39 |
| C10 | Any implementation that passes the checks is an implementation of the farm; contracts with checks exist only at the boundaries with foreign code: workers and channels (P1, E1), tool plugins (P3, E3), the model provider (P4) and the monitoring system (E5); the other ports describe how the core is built | D-13, D-91, D-114 |
