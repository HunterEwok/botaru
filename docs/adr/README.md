# Architecture decision records (ADR)

## In short

This folder holds the architecture decision records of the skeleton in MADR 4.0.0 format. An ADR is needed for a decision that changes a port or a contract, and in the cases listed in the section "Decisions (ADR)" in [docs/architecture.md](../architecture.md) (D-86 (13)); an ordinary new rule is written as a registry row with a reference to a decision D-NN. The same section has the table of future ADRs with the statuses of the decisions. The decision that an ADR rests on is recorded as a row in [docs/decisions.md](../decisions.md).

## How to start an ADR

1. Copy [template.md](template.md) to a file `NNNN-title.md`: `NNNN` is the next free four-digit number, and `title` is a short name in Latin letters with hyphens, for example `0001-agent-runtime.md`. An ADR number is never reused.
2. Fill in the sections of the template. "Context and Problem Statement", "Considered Options" and "Decision Outcome" are mandatory; "Decision Drivers", "Consequences" and "Confirmation" are optional. In "Confirmation" the ADR refers to the rule numbers of the registry `conformance/requirements.yaml` that confirm the decision.
3. Set the status: `proposed`, `accepted`, `rejected`, `deprecated` or `superseded`. A replaced ADR stays as a file with the status `superseded` and a link to the one that replaced it.
4. Add a row to the table of the section "Decisions (ADR)" in `docs/architecture.md`.
5. The ADR text is written in English, like the other documents; the section headings stay as in the MADR template.

## What is in the folder

| File | What |
|---|---|
| `README.md` | this file |
| `template.md` | the MADR 4.0.0 template |
| `NNNN-title.md` | decision records |

There are no ADR files in the folder yet. The table in `docs/architecture.md` is the list of future ADRs that will appear with the implementation; the decisions accepted among them are already recorded as rows in `docs/decisions.md` (the "Decision" column of the table) or in the skeleton text. New ADRs are written as files in this folder.
