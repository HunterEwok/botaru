# Lifecycles

## In short

A lifecycle sheet gives the states and transitions of one kind of record: a table of states and a table of transitions, from which `ci/gen_lifecycles.py` builds the YAML next to it. Thirty-one sheets describe nineteen kinds of lifecycle (D-114): sheets of one kind follow the same shape, so a reader learns the shape once. The kind is a column, not a merge: every sheet keeps its own state names and transitions, and rules refer to the same states as before.

Every sheet names its kind on the line under its title (`Kind: ...`), and the generator copies it into the YAML as `kind`. The table below is the closed list of kinds; `gen_lifecycles.py --check` fails when a sheet has no kind, names a kind not in the table, or the table lists a sheet under another kind than the sheet itself.

## Kinds

| Kind | Lifecycles | What they share |
|---|---|---|
| `run` | `campaign`, `script` | a frozen plan released by core code step by step, with a pause, a stop and an outcome per item or step |
| `improvement` | `proposal`, `escalation` | a closed-form record from a bot to the people or the farm manager, checked by code before anyone sees it |
| `promotion` | `promotion-entry`, `case` | the road of a functionary into the commission: a queue entry and the case that seats it |
| `memory` | `memory-item`, `fact`, `fact-offer` | memory records about a person or the farm, written only by the core |
| `bot` | `worker`, `identity` | the process that takes work and the bot identity that does it |
| `human-decision` | `decision`, `approval`, `grant`, `connection` | waits for a decision by a person (for a grant, by a person or the farm manager seat); same shape only: each keeps its own checks, because these records decide permissions and money |
| `record` | `alarm`, `file`, `session` | a short cycle: open, then closed or deleted at the end of its period |
| `task` | `task` | the unit of work |
| `lease` | `lease` | the right to work on one unit until a deadline |
| `delivery` | `delivery` | sending one content to one recipient |
| `message` | `message` | a bot message in an inbox |
| `hold` | `hold` | a stop on part of the farm's work |
| `incident` | `incident`, `attribution` | an incident and its possible links to executed changes |
| `trial` | `trial` | whether a promised indicator improved |
| `seat` | `seat` | a chair in the commission or of the farm manager |
| `tenant` | `tenant` | the wall of data and money |
| `manifest` | `manifest` | an object declared in git |
| `toolcall` | `toolcall` | one call to a plugin tool |
| `erasure` | `deletion-request` | every early deletion |
