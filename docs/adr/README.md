# Architecture decision records (ADR)

## In short

This folder holds the architecture decision records of the skeleton in MADR 4.0.0 format. An ADR is needed for a decision that changes a port or a contract, and in the cases listed in the section "Decisions (ADR)" in [docs/architecture.md](../architecture.md) (D-86 (13)); an ordinary new rule is written as a registry row with a reference to a decision D-NN. The same section has the table of the ADRs in this folder, with links to the files and the decisions they rest on. The decision that an ADR rests on is recorded as a row in [docs/decisions.md](../decisions.md).

## How to start an ADR

1. Copy [template.md](template.md) to a file `NNNN-title.md`: `NNNN` is the next free four-digit number (now 0011), and `title` is a short name in Latin letters with hyphens, for example `0011-event-broker.md`. An ADR number is never reused.
2. Fill in the sections of the template. "Context and Problem Statement", "Considered Options" and "Decision Outcome" are mandatory; "Decision Drivers", "Consequences" and "Confirmation" are optional. In "Confirmation" the ADR refers to the rule numbers of the registry `conformance/requirements.yaml` that confirm the decision.
3. Set the status: `proposed`, `accepted`, `rejected`, `deprecated` or `superseded`. A replaced ADR stays as a file with the status `superseded` and a link to the one that replaced it.
4. Add a row to the table of the section "Decisions (ADR)" in `docs/architecture.md`.
5. The ADR text is written in English, like the other documents; the section headings stay as in the MADR template.

## What is in the folder

| File | What |
|---|---|
| `README.md` | this file |
| `template.md` | the MADR 4.0.0 template |
| [0001-skeleton-from-the-model.md](0001-skeleton-from-the-model.md) | a skeleton from the model; contracts only at the boundaries |
| [0002-thin-core-queue-in-store.md](0002-thin-core-queue-in-store.md) | a thin core, the queue in the store |
| [0003-one-lease-task-states-events-schemas.md](0003-one-lease-task-states-events-schemas.md) | one lease for all work; task states; events; schemas |
| [0004-deployment-api-version-idempotency.md](0004-deployment-api-version-idempotency.md) | deployment; `apiVersion`; idempotency scope; no `input_required` |
| [0005-gateways-in-core-internal-ports.md](0005-gateways-in-core-internal-ports.md) | P3 and P4 inside the core; internal ports as descriptions |
| [0006-tenant-workspace-service-principal.md](0006-tenant-workspace-service-principal.md) | a workspace per tenant; a program as a principal |
| [0007-bot-messages-campaigns-scripts.md](0007-bot-messages-campaigns-scripts.md) | bot communication; campaign; script and translator |
| [0008-external-monitoring-service-control.md](0008-external-monitoring-service-control.md) | external monitoring; service control |
| [0009-proposals-commission-identity-manager.md](0009-proposals-commission-identity-manager.md) | proposals, the commission, identity, the farm manager |
| [0010-agent-runtime-and-language.md](0010-agent-runtime-and-language.md) | agent runtime; implementation language |

ADRs 0001 to 0010 are the ADRs that D-86 (13) said would appear with the implementation; they are now files, all with the status `accepted`; each rests on rows of `docs/decisions.md` (the "Decision" column of the table in `docs/architecture.md`) or, for 0003, on the skeleton text. New ADRs are written as files in this folder from number 0011 and get a row in that table.
