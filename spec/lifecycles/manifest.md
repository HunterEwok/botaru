# Lifecycle: declared object (manifest)

## In short

A declared object is an object described by a manifest in git and written to the store by the P2 `apply` operation: a tenant, a group, a role, a task type, a plugin, an action, a catalog, a route, a schedule, a channel, a sink and other kinds from a closed list. A manifest lies in the wrapper `{apiVersion, kind, metadata, spec}` and passes the schema of its kind (MAN-1). `apply` brings the store to the set from the commit atomically: the whole set in one transaction or nothing (MAN-6). Only `apply` changes declared objects; P2 has no operations that change their `spec` (MAN-5). An object that disappeared from git is not deleted silently: `apply` shows it together with its dependents, and only an explicit command deletes it (MAN-2).

## States

| State | Meaning | Terminal |
|---|---|---|
| `applied` | The object is written from the applied commit and is in force | no |
| `deprecated` | v0.2. The object is in force but carries `deprecated: true` and `sunset: <date>`; `apply` warns about every reference to it | no |
| `unused` | The object is written but not used: a written object that it references disappeared, or the object itself disappeared from git and waits for explicit deletion. There is no new work for it, started work is finished; the error or the reason is visible in its status | no |
| `removed` | The object was deleted by an explicit command; its record in the store is gone, the audit remains | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `applied` | P2 `apply` | the whole set passed the kind schema and `apiVersion`, the check of references between declared objects, the rule lint (every tool has `target_from` or `target: any`, TEN-3a; `redact` for a tool with untrusted output on a machine; farm machines outside the catalog, DEP-11; service script names from `monitor_scripts_allowed`; `deadline` for a schedule type) and the ban on rights escalation; the set is written in one transaction | MAN-1, MAN-6 |
| (none), `applied`, `deprecated`, `unused` | (no transition) | P2 `apply --dry-run` | a plan of changes without a write: what will be added, changed, what disappeared from git and what depends on it | MAN-2 |
| (none), `applied`, `deprecated`, `unused` | (no transition) | P2 `apply` with an error in any object of the set, or a core failure in the middle of `apply` | no object of the set changes; the status shows the errors of the rejected set by object | MAN-6 |
| (none), `applied`, `deprecated`, `unused` | (no transition) | P2 `apply` of a commit with the line `Botaru-Proposal: <id>` or of a commit linked to a proposal through P2 `proposal.link_commit` | the commit touches files outside the folder of this proposal's tenant: the set is rejected in full with the reason `proposal_scope`, the commit is not linked to the proposal | MAN-5, PRP-8 |
| `applied` | `applied` | P2 `apply` with a changed manifest | inside a version `.vN` only additions of optional fields, a breaking change is a new object `.v(N+1)`; repeating the same set changes nothing | MAN-2, MAN-3 |
| `applied` | `unused` | the core, when a written object referenced by the declared one (a principal, a chat binding, a worker) disappeared | the reference is checked at every use; the error is written to the object's status | MAN-1 |
| `applied`, `deprecated` | `unused` | P2 `apply` of a set that does not contain the object | `apply` shows the object together with its dependents (non-terminal tasks, schedules, routes, people in a group) and does not delete it | MAN-2 |
| `unused` | `applied` | P2 `apply` in which the object is present again; the return of a written object that had disappeared | the references pass the check again | MAN-1, MAN-2 |
| `applied` | `deprecated` | P2 `apply` of a manifest with `deprecated: true` and `sunset` | v0.2; references to the object give an `apply` warning | MAN-7 |
| `deprecated` | `applied` | P2 `apply` without the `deprecated` mark | v0.2 | MAN-7 |
| `unused`, `deprecated` | `removed` | P2 `apply` with an explicit list of objects to delete | the object is not in git; there are no dependents: a task type `.vN` has no non-terminal tasks, schedules and routes pointing to it, a group has no people; otherwise a refusal with the list of dependents in the status | MAN-2, MAN-7 |

A change of the manifests' `apiVersion` (from v0.2) is not a transition: the `farmctl` converter turns the old wrapper into the new one without a change of meaning, and the `apply --dry-run` plan before and after the conversion is the same (MAN-7).

## Invariants

- Declared objects change only by `apply` from git; an urgent change without git is done with a hold, not by editing the object (MAN-5, HLD-1).
- `apply` is atomic: there are no partially applied rights, roles and types (MAN-6).
- There is no `status` field in git: the core keeps the status, with the applied commit, the set hash and the errors by object; P2 `doctor` shows the difference from the commit that the operator named (MAN-1, MAN-5).
- A kind without a registered schema is rejected; a kind and a driver from the implementation grant no capabilities and declare no tools and actions (MAN-1, MAN-4).
- Deletion is never silent: an object that disappeared from git is shown together with its dependents (MAN-2).
- The core reads the lines `Botaru-Proposal: <id>` and `Botaru-Revert: <id> <reason>` of the commit message at `apply` and links the commit to the proposal; the revert reason is taken from the closed list of PRP-22 (`harm_observed`, `superseded`, `other`). A commit linked to a proposal changes only the files of this proposal's tenant; otherwise `apply` rejects the set in full with the reason `proposal_scope` (MAN-5, PRP-8).
- Every `apply` writes an audit record with the commit number and the author (AUD-1).
- No manifest carries a `max_tokens` field; the task tree limit is set by `tree_budget_eur` (BUD-9).

## Versions

| Version | What appears |
|---|---|
| v0.1 | the states `applied`, `unused`, `removed`; the wrapper `botaru/v1alpha1`; atomic `apply`, the plan mode, the status with the commit and the set hash; explicit deletion with a check of dependents; the declaration of the skeleton version and the `conforms_to` profiles in P2 `status` (MAN-8); the lint of a `routine` class action (MAN-9, D-78) and of a catalog with service `no_card` and a single tenant per machine (D-74, D-75); after the core and the first bot (D-71, D-80): the lines `Botaru-Proposal: <id>` and `Botaru-Revert: <id> <reason>` in the commit message and the refusal `proposal_scope` (D-71) |
| v0.2 | the state `deprecated` with `sunset`; the `apiVersion` converter with a check of an identical plan |
