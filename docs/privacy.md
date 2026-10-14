# People's data

## In short

The farm works with people's data: their messages, files, questions to assistants, names in journals and tables. This document describes what the farm stores about people, where and for how long, how a person learns about their data and asks for it to be erased, and what the farm must be able to do so that the tenant's organization can meet its obligations under data protection law. Legal roles, the processing region, retention periods and texts for people are decided by the organization (D-12). The skeleton gives fields and checks for these decisions: the privacy block in the tenant manifest (SEC-8), the ceiling of channels with third-party storage (SEC-9), the "personal data seen" flag and the "whose data" field (AUD-5), the privacy notice (SEC-13), the copy of a person's data, the privacy report and subject requests (SEC-10, SEC-11, SEC-12), and de-identification of per-person usage (SEC-14). Erasure of a person's data at their request is part of v0.1 and cannot be waived for a production tenant (D-76); the data copy and the other data subject rights arrive in v0.2. Any early erasure goes through a recorded erasure request, so erased data does not come back after a restore from backup. How the data itself is protected (redaction, the untrusted flag, audit, deployment) is described in `docs/permissions.md`, and retention periods and erasure as an operation in `docs/operations.md`.

## What the farm stores about people and where

A principal (`Principal`) is whoever acts inside one tenant: a person (`human`), an external program (`service`) or the system principal `system`. A person in two tenants is two principal records, and everything below is counted separately in each tenant. Every kind of stored object belongs to exactly one retention class (RET-1), and the tenant sets the period of a class within the farm ceilings (RET-2).

| What | Example in `demo` | Retention class | Where it lives | Who sees it | How it is erased |
|---|---|---|---|---|---|
| principal and its identity | `alice` and her messenger number | principal record | core store | tenant admins with `admin:people`; the operator only in an emergency | the identity is erased at offboarding, the name is erased from the principal record, and the audit keeps the same opaque id `human:p_<ULID>` under which the person is recorded in all core records (LCY-3, D-85) |
| group memberships | `alice` in group `staff` until 2026-12-31, granted by `carol` | principal record, history in the audit | core store | tenant admins | together with the principal |
| group chat binding | the lab chat is bound to `demo` with ceiling `summary` | channel record | core store | operator, admins | on unbinding |
| task body | goal "why was the site slow last night?", parameters, links to attachments | `content` | core store | the initiator and people with `task.read:<type>` and the permission for the mark | by class period or by an erasure request |
| result and artifacts | a log summary, a pipeline table | `content` | core store, file store | same | same |
| files and attachments | a paper PDF that `alice` attached | `content` | bytes in the file store, file record in the core store | same | same |
| conversation sessions | the history of `alice`'s conversation with her assistant | `content`; 7 days for roles without memory | session store (C3), runtime store | the runtime in attempts of this conversation | by period, by `forget`, at offboarding |
| a person's memory | "`alice` asks for reports on Mondays" | `memory` | memory store (C4) | `alice` herself (`/memory`) and her assistant | `/forget`, offboarding, tenant period |
| a bot's memory | code records about the bot's work, verified facts about the farm | `memory` | memory store (C4) | the bot itself; after the bot retires, sealed records only to people with the permission | sealed when the bot retires and wiped by period |
| bot messages | a note in a message from one bot to another | `content`, fields of closed kinds only | core store | the recipient, in the untrusted text wrapper | by class period |
| approvals and human decisions | `carol` rejected `cleanup_pipeline_tmp` | `metadata` | core store | people with the permission for the decision subject, audit | only in whole months |
| subject requests | `/mydata erase` from `alice`, ids of objects found by an `erase_match` request | `metadata`; the search string is wiped right after the search | core store | the person about their own request, admins with `admin:people` | only in whole months |
| audit, transition journal, events, usage | ids, codes, sizes, hashes; per-principal usage after 93 days without the principal id (SEC-14, v0.2) | `metadata` | core store | principals with `audit.read` (AUD-7) | only in whole months (AUD-3) |
| attempt journal | model requests and responses of one attempt | `content` with the period `retention.journals` | journal store | operator with `admin:debug` and `data:<mark>` (AUD-4) | by period |
| observations | task duration, host load | `observation` | core store | roles for their own series | in whole periods |
| tombstones and erasure log | task id, idempotency key hash, erasure outcome | `tombstone` | core store and a separate copy of the log | the core | while the tenant exists |
| connection to an external service | `alice`'s access to her calendar | connection record; the token is held by the deployment | core store, secret store | the core; the model does not see it | revocation, offboarding, `forget` (SEC-7) |
| sink | a weekly report in the tenant's file directory | `content` | sink (E2) | the sink's recipients | by period, by the sink's erase operation |
| backups | a copy of the core store | classes of the original objects | the backup machine `backup1`, a farm node and a production machine (DEP-11), or external storage where the backup goes encrypted (RET-5, D-88) | only the operator | a backup lives no longer than the longest `content` period plus the backup window (RET-5) |
| at the model provider | everything the model read | per the subprocessor's terms | at the provider | the provider | the farm cannot erase it; the period is recorded in the privacy block and in the model profile (GW-15) |
| in the messenger cloud | the conversation with the bot | per the messenger's terms | at the messenger | the messenger | what the bot can delete itself is recorded in the channel binding |

People's names and their external ids (messenger number, email address) live only in principal and identity records. They are not in the audit, events, transition journal or usage (AUD-2). So erasing an identity de-identifies the journals without editing a single line of them, and the audit hash chain (AUD-6) stays intact.

## What the farm does not do

- **It has no "personal data" class.** Data classes (`summary` < `internal` < `raw_logs`) express sensitivity for permissions: to whom a result may be given. The class `pii` is rejected: personal data arrives as free text everywhere (in a task goal, in a file, in a log), and a class would protect nothing. Instead of a class there is the origin flag `personal_seen` (AUD-5) and redaction.
- **It does not take addresses off the machine.** By default the toolhost redacts IPv4 to the /24 subnet, IPv6 to the /48 prefix and email addresses entirely, including inside URLs and headers, before the output leaves the machine (INJ-4). An IP address counts as personal data, so redaction is on by default.
- **It does not evaluate people.** The subject of a fact, a farm manager finding, an observation series, a proposal or a review is only an object of the tenant catalog: a resource, a task type, a role, a worker, a schedule, a dataset (MEM-6, OBV-2). A person is never the subject of any record that the model writes, or that code writes from model output. Farm reports count people only as a group, without names.
- **It does not mix tenants.** The tenant is taken from the credentials (TEN-2). Files, memory, cache and the provider workspace are separate for each tenant (TEN-5, TEN-7, D-42); one farm manager attempt sees objects of exactly one tenant (MGR-24).
- **It does not show the model secrets or people's credentials.** Secret values do not pass through ports (SEC-1), and the core, not the model, chooses a person's access to their external service (SEC-7).
- **It does not download links from messages.** A link stays text, and the channel saves an attachment (INJ-8).

## What goes to the monitoring system

The farm takes machine observation and the delivery of alarms to people from the monitoring system that the organization already runs (D-59, D-62; `docs/monitoring.md`). The privacy block names it among the recipients (`recipients: [... monitoring]`) as a recipient with third-party storage and the ceiling `summary` (SEC-9), but it is not a subprocessor: the system runs at the organization, and the organization decides who sees what in it and how long its history is kept. The farm gives it only what is needed for alarms and checks, and nothing about people.

| What goes out | Example in `demo` | What it contains | Rules |
|---|---|---|---|
| a farm alarm episode | `firing` of the alarm `disk.free.low` | alarm name without `alarm.` and `.vN`, severity, episode state, tags `tenant: demo`, `subject: res:ctd1:data`, measurement numbers | MON-1 |
| core liveness answer | "alive", free store space, tick age of the background loops | the "alive" flag and numbers | MON-2 |
| audit chain head | number and hash of the last record of the `demo` chain | number and hash, without the content of the records | MON-3, AUD-6 |
| farm nodes and template | nodes of the core, workers and toolhost | node names from the deployment description | MON-4 |
| a bot note on a problem (v0.2) | "free space on the volume is dropping by 2 % an hour since 02:10" marked `[botaru <bot_id>]` | text of up to 1000 characters | MON-7 |
| a code note about a service or script action | "restart nginx: done" from the template `service_control_done` marked `[botaru <bot_id>]` | code template: service and verb or script, outcome, `grant_id` of the `service_grant` record; no model text | MON-8, APR-18 |
| a monitoring script run | the script `clear_cache` from the catalog's `monitor_scripts_allowed` on node `web1` | script id and node id | MON-10, INJ-16 |

- Alarm episodes, the liveness answer and chain heads contain only numbers, `enum` values, tags and catalog ids, as in events and telemetry (AUD-2). They contain no person ids, names, external ids, task and message texts, tool output or file names. The `subject` tag is the id of an object of the tenant catalog (a resource, worker, task type, role), not of a person: a person is never the subject of an alarm.
- A bot note is the only free text that the farm gives to the monitoring system; a note about a service or script action is assembled by code from a template and carries no names of people. It is bot output, so people read it as untrusted text, not as a code fact. Before delivery the core redacts the note like tool output (addresses to /24 and /48, email addresses, secrets; INJ-4), searches it for secrets with the same patterns as an outgoing channel message (INJ-11), and a secret found stops the delivery; the note's class ceiling is `summary`, and the tenant canary is checked in the note the same way as in inboxes and tasks (TEN-5). A note talks about the problem and catalog objects; it must contain no names of people and no task content, and the leak evals count it as one more model output (INJ-12).
- In the other direction, a problem found by the monitoring system enters the farm only as closed fields: event number, the node as a catalog resource, the class from a closed table, severity, state and numbers. The trigger name and problem texts do not enter the farm (MON-5); the class and item of the event are taken from the catalog's `monitor_triggers`, and script names from `monitor_scripts_allowed`, so not a single string written in the monitoring system reaches the model context (INJ-16). When a bot reads monitoring data with the `monitor` plugin, it is untrusted tool output (MON-6) and lives where any output lives: in the attempt context and in the attempt journal if it is enabled. The outcome of a monitoring script (code and output up to 4096 bytes) lies as a file in the task journal marked as untrusted, and an attempt that reads it gets `untrusted_seen` (MON-10).

## The "personal data seen" flag

To answer a person's question "where do you have my data", the farm must know in which objects personal data appeared. For this, tasks, file records, artifacts, bot messages, farm manager findings and memory items carry the flag `personal_seen` (AUD-5). Only the core sets the flag, on events that code sees.

| When the core sets the flag | Example |
|---|---|
| redaction on the toolhost replaced at least one IP address or email address | the web server log contained the address `203.0.113.7` |
| an attempt read the content of a file or attachment uploaded by a person | the bot read a PDF attached by `alice` |
| an attempt read the body, result, file or message of a task whose initiator is another person | the farm manager's reader role read the result of a task of `bob` |
| the task body arrived as a forward or a quote | `alice` forwarded a message from `carol` to the bot |
| an attempt read an object that already has the flag | a reporter read a result with the flag |
| the file was uploaded by a principal with `uploads_personal: true`, or a `service` or `ingested` file came from a source that declared personal data (source `declared`) | the HR program uploaded a staff table |

The flag is monotonic, like the untrusted flag: it is only ever set. Child tasks, continuations and everything that an attempt with the flag created inherit it: the result, artifacts, messages, findings, memory record proposals. The model does not see the flag and cannot clear it. In the audit the flag is stored as a marker with a source code from a closed list (`masking`, `human_file`, `other_person`, `forward`, `quote`, `declared`, `inherited`), without content.

**Whose data.** With the source `other_person`, `forward` or `quote` the core knows whom it is about: it is a farm principal. Then the object gets the closed field `personal_subjects` with the list of ids of such principals (ids only, add only). Using it, erasure by the scope `(tenant, subject)` finds a person's data in other people's tasks, bot messages and farm manager findings (SEC-11). The farm manager reads any file with the flag, including the source `declared`, as personal (MGR-30).

The flag is not set by:
- text that the initiator typed in their own message: the link to them is already recorded in the initiator field;
- reading facts and observations: their values are computed by code over catalog objects, and people are not among the subjects.

Why the flag is needed:
- the privacy report shows how many objects with personal data lie in each class and store (SEC-12);
- an admin sees the flag on an object in P2 and can check the object before answering a subject request.

Erasure by an exact string is not limited by the flag (SEC-11). A person may have written another person's name in their own message, and such an object does not carry the flag, so the search covers all objects of the classes `content` and `memory`.

## Tenant privacy block

The `privacy` block in the tenant manifest records as data what the organization decided about its processing: why the farm processes data, who the controller is (the organization that decides on the purposes of processing), to whom data goes further and where it is stored (SEC-8). The block lives in git together with the rest of the manifest, the core reads it, and `apply` checks it.

```yaml
# tenants/demo/tenant.yaml (fragment)
spec:
  kind: production
  privacy:
    purposes:                                      # purpose and lawful basis
      - {purpose: operations, lawful_basis: public_task}
      - {purpose: assistance, lawful_basis: legitimate_interest}
      - {purpose: data_processing, lawful_basis: public_task}
      - {purpose: incident_review, lawful_basis: legitimate_interest}
    controller:
      name: "Example Org"
      contact: privacy@example.org
      agreement_ref: "contracts/demo-processing"   # data processing agreement; the farm does not open it
    subprocessors:
      - provider: provider-a                       # name of the model provider binding
        agreement_ref: "contracts/provider-a-dpa"
        region: region-a                           # value from the region list in the provider binding
        retention: {mode: standard, max_days: 30}  # or {mode: zero}: zero retention by agreement
        terms: {training: false, human_review: abuse_only}   # agreement terms: training and human review
    channels:
      - driver: messenger
        third_party_storage: true                  # a third party stores the conversation
        ceiling: internal                          # nothing above this class goes to the channel
    recipients: [model_provider, messenger, monitoring]   # recipients; monitoring is mandatory (MON-1)
    notice_ref: "privacy/demo-notice"              # full text of the privacy notice
    notice_version: 3                              # grows when the controller, purposes, subprocessors or recipients change
    dpia_ref: "dpia/demo-2026"                     # impact assessment; an explicit none is allowed
    ropa_ref: "ropa/demo"                          # record of processing activities
    subject_request_days: 30                       # deadline for answering a subject request
```

| Field | Meaning | Who reads it |
|---|---|---|
| `purposes` | processing purposes: `purpose` from the closed list `spec/model/processing-purposes.yaml` and the lawful basis `lawful_basis` (`public_task`, `legitimate_interest`, `contract`, `consent`, `legal_obligation`; default `public_task`) | the core (purpose `incident_review`), the privacy notice, reports |
| `controller` | controller: name, contact, link to the data processing agreement | privacy report |
| `subprocessors` | subprocessors: `provider`, `agreement_ref`, processing region `region`, retention at the provider `retention`: `{mode: standard, max_days: N}` or `{mode: zero}`, terms `terms: {training, human_review}` | P4 (GW-14, GW-15), `apply`, reports |
| `recipients` | data recipients from a closed list; `monitoring` is mandatory | privacy notice, reports, `apply` |
| `notice_ref`, `notice_version` | link to the full text of the privacy notice and its version | `privacy_notice` delivery (SEC-13), `apply` |
| `channels` | channel driver, whether a third party stores the conversation, class ceiling | channel, `apply` |
| `dpia_ref`, `ropa_ref` | links to the organization's impact assessment and record of processing activities | privacy report |
| `subject_request_days` | deadline for answering a subject request, 30 days by default | the subject request loop |

### Processing purposes

The list of purposes is closed, and only one purpose changes the behavior of the core.

```yaml
# spec/model/processing-purposes.yaml
operations:      {title: "Operation of the tenant's systems", effect: none}
assistance:      {title: "Answers to people and personal assistants", effect: none}
data_processing: {title: "Processing of tenant data by pipelines and reports", effect: none}
incident_review: {title: "Incident review by the farm manager", effect: inspect}
```

The purpose `incident_review` is enabled in the tenant template: the farm manager can access almost all of the tenant's content to look for the cause of an incident. The tenant can switch it off. Then the core enqueues no tasks with the purpose `inspect` in this tenant, cancels running read attempts in the same transaction as the switch-off, and the lint does not require a reader role (MGR-20): the farm manager decides only from code views. The other purposes describe processing for the privacy report and the organization's record and do not change the behavior of the core. A tenant can opt out of the farm manager entirely: with `Tenant.spec.farm_manager.enabled: false` (D-77) the farm manager does not read the tenant's data and does not run its review and lens regardless of the purpose `incident_review`, and the tenant's incidents go only to its admins.

**Lawful basis.** Each purpose has a `lawful_basis` from a closed list; the default is `public_task`. The basis `consent` is allowed only for a purpose that has a consent record of the principal and a withdrawal operation, like a connection to an external service (SEC-7): consent without withdrawal is not consent. Which basis applies to which purpose is decided by the organization; the skeleton only refuses a purpose without a basis for a production tenant.

### What `apply` checks

- For a tenant with `kind: production`, no field of the block is empty: there is at least one purpose with a basis, a controller with a contact and an agreement, at least one subprocessor with an agreement, a region and a retention mode, a line for every tenant channel, `dpia_ref` and `ropa_ref`. For `dpia_ref` an explicit `none` is allowed: it is the organization's decision that an impact assessment is not needed, not a forgotten field.
- For any tenant, every provider from `providers_allowed` is in `subprocessors`. Otherwise a fallback model of another provider would become a new subprocessor without a decision of the organization.
- A channel that is not in `channels` counts as a channel with third-party storage and the ceiling `internal`.
- `recipients` contains `monitoring`: alarm episodes and notes always go to the monitoring system (MON-1), and the recipient must be named.
- The model profile does not contradict the subprocessor's terms: the profile's `provider_training`, `provider_human_review` and `subprocessors[]` are checked against `terms` (GW-14, GW-15). Every P4 call goes through this check, including the embedding model for corpora, the fallback model and evals.
- A change of `controller`, `purposes`, `subprocessors` or `recipients` without an increase of `notice_version` is rejected (SEC-13).
- A change of the block gives an event and a notification to the tenant admins, like any manifest change.

For a test tenant the block may be incomplete, but it still needs the list of subprocessors: without it the model is not called.

### Region and retention at the provider

Everything the model read goes to the model provider. So the processing region and the retention mode at the provider are set by the organization, and they are applied by the model gateway P4 inside the core, not by the runtime. P4 sets the region from the `region` field of the subprocessor it calls, in the way that the provider binding describes, and rejects a region field in a runtime request (GW-14, GW-1). The model profile records how long the provider keeps requests and responses and whether the model requires retention; a profile with a period longer than `retention.max_days`, or one that requires retention, at a subprocessor with `retention: {mode: zero}` does not pass `apply` (GW-15). The profile also records whether the provider uses requests for training (`provider_training`) and whether people read them (`provider_human_review`: `none`, `abuse_only`, `yes`); a profile that contradicts the subprocessor's `terms` does not pass `apply`, and this applies to every P4 call, including the embedding model and the fallback model. If the subprocessor's region is outside the EU, a production tenant passes `apply` only when the tenant has a transfer basis `spec.model_transfer` for this provider or region: `scc`, `adequacy` or `consent` with a review date (D-87 (2)). The basis is decided by the controller, so it lives in the tenant manifest, not in the shared model profile. P4 calls only a provider from the tenant's `subprocessors`, including in farm manager attempts in this tenant and on the fallback model (SEC-8). Each tenant has its own provider workspace and key (D-42).

## Channels and raw logs

A third party stores messenger conversations, and they may lack end-to-end encryption. So a channel declares `third_party_storage` and the class ceiling `ceiling` in the privacy block (SEC-9).

- For a channel with third-party storage, including private chats, the default ceiling is `internal`.
- Raw logs (`raw_logs`) go to such a channel only if the tenant explicitly raised the ceiling to `raw_logs` with an entry in the privacy block in git. A person's permission `data:raw_logs` does not replace this: it says who may see raw lines, while the channel ceiling says where they may be sent.
- A delivery with a mark above the ceiling does not go to the channel. It gets `withheld(channel_ceiling)`, and the person gets a code text without the content: the result is withheld because the channel stores the conversation at a third party. The result stays in the task until the period of its class.
- The ceiling of a group chat binding applies together with the channel ceiling: the lower one is used.

Example: `alice`, who has the permission `data:raw_logs`, asks in a private chat for raw lines of the `nginx` log on `web1`. The messenger ceiling of `demo` is `internal`, so `alice` gets "Result withheld: the tenant has not allowed sending raw logs to this channel", while the summary without raw lines is delivered as usual.

## "You are talking to a bot"

A person must know that they are writing to a bot, not a person, no later than the first contact. The channel driver declares how the channel communicates this:
- with a built-in marker: the messenger itself shows the bot account as a bot;
- with a code text at the first contact: before the first model text, the core sends every person and every newly bound chat a text from the message catalog. It says that this is a bot, that answers are written by a model of an external provider, where the conversation is stored and what the operator sees. Until this notice is delivered, no model text goes to this recipient; if the notice could not be delivered, the model text is withheld.

A channel without such a declaration does not pass `apply`. Model text carries a machine origin mark: in farm records it is `dataorigin: model`, in artifacts and sink files an origin mark. So both people and code can always tell model text from code text. The rule about the "you are talking to a bot" message is described together with delivery in `docs/tasks-and-delivery.md`.

**The privacy notice** is a separate code delivery `privacy_notice` (SEC-13), and it does not depend on how the channel says it is a bot: even when the messenger itself shows the bot badge, `alice` gets at her first contact with `@demo_bot` a code text with the controller and contact, purposes and bases, recipients, subprocessors with region and retention, retention periods, a link to the full text and how to submit a data request. The text is assembled from the privacy block and the message catalog (key `notice.privacy`, substitutions from the privacy block, DLV-9) and carries `notice_version`. When the organization changes the controller, purposes, subprocessors or recipients, it increases `notice_version`, and every principal gets the notice again at the next contact; `apply` does not let such a change through without a new version. Until the notice of the current version is delivered, model text to the recipient is withheld. The delivery is written to the audit with its version, and the privacy report shows how many principals have received the current version.

## Retention and erasure

### Retention classes

| Class | What is in it | Default period | How it is erased |
|---|---|---|---|
| `content` | task bodies, results, artifacts, files, attachments, sessions, runtime transcripts, attempt journals, bot messages, sink archive | 30 days; sessions of roles without memory 7 days | one object at a time |
| `memory` | memory items | until `forget`, offboarding or the tenant period | one item at a time |
| `metadata` | audit, transition journal, events, usage, human decisions | 400 days | only in whole months |
| `observation` | observation points and rollups | points 14 days, rollups 93 days | in whole periods |
| `tombstone` | tombstones: object id, idempotency key hash, time | while the tenant exists | in whole periods after the tenant is deleted |

The schema of every stored object declares its class, and every store, including runtime stores, sinks and the channel cache, belongs to one class (RET-1). From v0.2 periods are set by `Tenant.spec.retention` per class within the ceilings of `spec/model/retention-classes.yaml` (RET-9); content is erased by period in all copies that the farm maintains (RET-10). Erasure does not release uniqueness: after a task is erased a tombstone remains, and a repeat of its idempotency key gives `gone`, not a new task. A tombstone does not store a hash of an external id: a messenger number or an email address can be found by brute force (RET-3).

The sealed memory of a retired bot does not extend periods: sessions that live 7 days by their class are not kept longer because of the seal.

From v0.2, per-principal usage lives in the class `metadata` for 400 days, but keeps the reference to a person for only 93 days: after that the retention loop removes `subject`, `task` and `root`, truncates `time` to the day and writes `subject_kind: human`, in whole days, so that no join with audit or task records leads back to the person, and the sums remain; this is the only edit of a `metadata` row (SEC-14, BUD-16). Limits and reports per person are computed only from records younger than 93 days.

### Erasure through the log

Erasure before the period ends is always a recorded erasure request (`ErasureRequest`): the scope, a reason from a closed list, the author, the time and the outcome for each store (RET-7). The scope is `(tenant)`, `(tenant, subject)` or object ids. This is how `/forget`, offboarding, the memory of a retired bot at the end of the seal, tenant deletion, a runtime store when a worker is decommissioned, a subject request and manual erasure by the operator through `farmctl` are all erased. The request enters the erasure log before the first byte is erased. No process and no `farmctl` command can erase content bypassing a request.

This is needed because of backups. The erasure log (erasure requests and tombstones, metadata only) is backed up separately from the store and at least as often as its backups (RET-5). After a restore from backup, the core replays the erasure log before it becomes ready (RET-6). So what a person asked to erase last week does not come back from a backup taken before their request. An erasure made bypassing the log would be brought back by the backup.

A backup stores content and follows the rules of a production machine: it is encrypted, accessible only to the operator and does not lie on the core machine. A backup lives no longer than the longest `content` class period plus the backup cycle window, so erased data also leaves the backups on its own (RET-5).

Every port that stores content has its own erase operation: files in C1 `file.purge`, sessions C3 `delete` and `purge`, memory C4 `erase`, channel E1 `erase`, sink E2 `erase`, runtime E4 `purge`. The erasure request collects the outcome of every store: `done`, `failed` or `not_applicable` (RET-4). Until every store has given `done` or `not_applicable`, the request stays open, and an alarm is raised at the deadline (RET-7). From v0.1 the operations C1 `file.purge` and `task.purge`, C3 `delete` and `purge`, C4 `erase`, E2 `erase`, E4 `purge` and E5 `erase` of notes are in effect (D-76; RT-1, RT-2); E1 `erase` and `match` of the channel cache, E4 `purge` for every runtime and C3 `purge` by `(tenant)` arrive in v0.2 (RET-11): until then, for a channel that declared `driver.stores_content: none`, the core itself writes the step with the outcome `not_applicable` and `basis: declared_none`, while a channel with attachments or a cache reports `failed`, and the request stays open with an alarm (D-86 (8)). Any other store without its own erase operation also answers `failed`. From v0.2 a person's connections to their own accounts at external services (SEC-7) are records of class `content`: an erasure request with the scope `(tenant, subject)` and offboarding (LCY-3) revoke them, the consent adapter deletes the tokens from the deployment secret store, and the records are deleted with the person's other content (`spec/lifecycles/connection.md`).

### What the farm cannot erase

- **Copies at the model provider.** The provider keeps requests and responses per its own terms. The period is recorded in the model profile and checked against the privacy block (GW-15). The organization gets zero retention through an agreement with the provider; the farm only checks the field.
- **The conversation in the messenger cloud.** The channel binding records which messages the bot can delete itself and which it cannot. The report to a person names the messenger among the recipients.
- **History in the monitoring system.** Alarm episodes and audit chain heads live in the organization's monitoring system per its periods, and they contain no data about people. The farm itself erases bot notes (v0.2) with the E5 operation `erase` by `(tenant, subject)` and by id, if the monitoring system API supports it; otherwise the outcome is `failed`, and a person deletes the note with the monitoring system's own tools. Such a `failed` is acceptable and does not keep the erasure request open: by design a note carries no data about people (class `summary`, redaction, INJ-4), and the person sees it as a separate line in the outcome.

## A person's rights to their data

### Own memory

A person sees what their assistant remembered with the command `/memory` and erases an item for real with the command `/forget`, in all stores, including the runtime store (MEM-4). The erasure goes through an erasure request and so survives a restore from backup.

### What a person learns about their data

There is no separate report about a person's data (D-114). What the farm stores, for which purposes, for how long and who receives it, the person learns from the privacy notice (SEC-13), and the data itself they get as a copy with `/mydata export`: the copy goes only to them, encrypted with their own key, and never to the operator, an admin or a chat (SEC-10).

### Request for a copy or erasure

The commands `/mydata export` and `/mydata erase` create, through P2 `principal.data_request`, a subject request record (`SubjectRequest`) of kind `export` or `erase` and a `subject_request` card for the people of the tenant with `admin:people` (SEC-11). A third kind, `erase_match`, is created only by an admin and is described below. Erasure (`erase`, `erase_match`) works from v0.1 and cannot be waived for a tenant with `kind: production` (D-76); the copy (`export`) arrives in v0.2. A copy is only about oneself: `/mydata export` carries the person's public key (`recipient_key`), and the copy goes only to an export sink with `recipient_ref: self`, encrypted with that key, so neither an admin nor the operator can read it (SEC-10, LCY-2). It leaves out objects whose `personal_subjects` name other people. A tenant export at offboarding leaves out private chats, data from people's connections and connection records. People of the organization decide, because the decision about erasure is taken by the controller: the organization may be obliged to keep part of the data. The model neither creates nor decides a request.

| State | Meaning | Terminal |
|---|---|---|
| `open` | the request is recorded, the card is with the admins, the `subject_request_days` period is running | no |
| `approved` | an admin approved; the core created an erasure request or an export task | no |
| `rejected` | an admin rejected with a reason code; the person got the code and a code text | yes |
| `done` | all stores gave an outcome; the person got the outcome as a code text | yes |
| `failed` | at least one store gave `failed`; the admins got an alarm, the request can be retried | no |

Silence neither executes nor closes a request. At half of the period the admins get a reminder; at the deadline the core publishes an event about an overdue request and notifies the operator. The card shows how many objects of each class will be erased or exported, the deadline and what happens on silence.

An approved erasure goes only through an erasure request with the reason `subject_request` and the scope `(tenant, subject)`, through the erase operation of every port (RET-4, RET-7). It reaches the sealed memory of retired bots. The scope includes objects where the person is the initiator or owner, objects with them in `personal_subjects` (their data in other people's tasks, bot messages and findings) and group chat sessions whose key includes them. Before erasing the identity, the core itself creates an `erase_match` request for their external ids and display name: this finds objects where they were mentioned in text but the flag is absent; a second person with `admin:people` approves it, like any `erase_match`. The person gets the outcome for every store as a code text: what was erased, where erasure does not apply and what lies outside the farm (at the model provider and in the messenger cloud). An approved export enqueues a core task that writes the principal's data as JSON Lines per the skeleton schemas only to a sink with the block `Sink.spec.export_only: {recipient_ref, key_ref}`: the single recipient (`recipient_ref`, `admin` or `human:<id>`) is the person themselves or an admin of their tenant, the file is encrypted with their key and lives for the `content` class period (LCY-2); an export task to a sink without `export_only` is rejected with the code `sink_not_export`. The person gets a code text saying that the copy is ready and how the organization will hand it over.

### Data of people who are not farm principals

The name or address of a person unknown to the farm can lie in someone else's task: `bob` wrote in a goal "agree the flights with erin@example.org". For such data there is a request of kind `erase_match`: erasure of the objects in which an exact string occurs (SEC-11).

- A person with `admin:people` creates the request through P2 `erasure.match`, and another person with the same permission approves it with a card; in a tenant with a single admin a farm admin approves (D-86 (8)).
- When the request is created, the core searches for the string in all objects of the classes `content` and `memory` of the tenant, including sealed memory, and, where the core does not read the content, with the port's `match` operation: in the runtime E4 (short-term memory and assistant transcripts), in the sink E2 (report archive) and in the channel cache E1; the E5 adapter searches for and erases bot notes in the monitoring system. A store without `match` gives `failed` with an alarm, not "nothing found": the card will not show "matches: 3" when an assistant transcript was not searched. That is why, for a production tenant, a role with a session or memory inside the runtime is allowed only with an adapter that declared `match` (RT-1). The core records the ids of the objects found in the request. Right after the search the string is wiped, and from then on the request keeps only the ids. The card shows the number of objects found per store, not their content.
- An approved request creates an erasure request with the reason `subject_request` for the ids of the objects found (RET-7). An object with a match is erased entirely, and a tombstone remains. The farm does not try to edit free text inside an object: such an edit is unreliable.
- The string does not enter the audit, events or the erasure log, not even as a hash: a hash of an email address can be found by brute force. After a restore from backup the erasure is replayed by the same ids.
- The request does not touch objects that appeared after the search. If the string turns up again, the admin creates a new request.

### A person leaving

Turning off with the command `/off` erases nothing and is reversible. Offboarding (leaving for good) cancels the person's pending approvals and tasks, erases their identity and creates an erasure request with the scope `(tenant, subject)` for the objects that belong to them (LCY-3). The audit keeps only the opaque principal id. Connections to external services are revoked and deleted (SEC-7). A person who comes back gets a new principal, and the old permissions do not come back to life.

### Privacy report

For the organization's data protection specialist, P2 `tenant.privacy_report` assembles one summary per tenant (SEC-12):
- processing purposes;
- retention classes with periods and the number of objects, of them with `personal_seen`;
- subprocessors with region, retention mode and a link to the agreement;
- channels with third-party storage and their ceilings;
- links to the impact assessment and the record of processing activities;
- the number of open and overdue subject requests;
- the number of content reads for incident review over the period.

The report contains only metadata. People of the tenant with `admin:people` or `audit.read` and the operator get it. From it the organization assembles its record of processing activities and impact assessment.

## Reading content for incident review

The farm manager looks for the causes of incidents and for this may read the tenant's content if the purpose `incident_review` is enabled in the privacy block.

- Only the reader role reads, and it is separate from the decision role; the decision role sees only findings of a closed form. One attempt sees objects of exactly one tenant, and the model is called with this tenant's key (MGR-24).
- The reader role does not see control data, the memory of people and bots, sessions, sealed records, secrets or anything of another tenant.
- The reader role opens a file uploaded by a person only if code named that very file as the subject of the incident (MGR-30).
- Every read is written to the tenant audit with the basis, the kind and id of the object, the size and the hash (MGR-28). The finding note (`note`) lies in the class `content`, and the audit keeps only its hash and length. The read log is visible to people with the permission to read the audit (AUD-7), and a person sees in their report the number of reads of their objects.
- If the purpose is switched off, there is no reading at all: read tasks are not enqueued, and running attempts are canceled in the same transaction as the switch-off (MGR-20).
- If the tenant opted out of the farm manager (`farm_manager.enabled: false`, D-77), the farm manager does not read its data and does not run its review and lens, and the tenant's incidents go only to its admins.

## Attempt journal and people's connections

**The attempt journal** answers the question "why did the bot answer like that yesterday" (AUD-4). It is content: the model requests and responses and the tool output of one attempt. It lies only in an erasable store of class `content` with the tenant period and the task mark, is read only by the operator through P2 `task.journal` with `admin:debug` and `data:<mark>`, and never enters the audit, events or the model context. For a production tenant it is off until the tenant sets a period itself. How the tenant learns about a read is set by the tenant manifest field `operator_access.debug_journal`: `deny` (P2 `task.journal` answers `forbidden`), `notify` (default: an audit record and a code message to the tenant admins without content) or `allow` (audit record only). This way an external client of the farm sees every read of its journals by the operator.

**A person's connection** to their email or calendar is created by the person themselves through the consent adapter, not by the model (SEC-7). The deployment stores the token value and does not pass it through ports. The core chooses the connection by the task's initiator; tasks of other people and of the system principal do not use it. Writing on the person's behalf remains an action with an approval. By default there is no access to personal email.

## What the organization decides

The skeleton does not decide legal questions for the organization (D-12). It gives a field that code reads and a check that will not let an empty field or a contradiction through.

| Decision | Who decides | What the skeleton gives |
|---|---|---|
| who the controller and who the processor is, the data processing agreement | the tenant's organization and the farm operator | `privacy.controller` with the agreement; `apply` of a production tenant does not pass without it (SEC-8) |
| processing purposes and lawful basis | the organization | a closed list of purposes, each with a `lawful_basis`; `consent` only with a consent record and withdrawal; the `incident_review` switch (SEC-8, MGR-20) |
| provider terms: training and human review | the organization by agreement | `subprocessors[].terms`; a profile whose `provider_training` or `provider_human_review` contradicts the terms does not pass `apply` (GW-14, GW-15) |
| processing region at the provider | the organization | `subprocessors[].region`; only P4 sets the region; a profile without region support does not pass `apply` (GW-14) |
| zero retention at the provider | the organization by agreement with the provider | `retention.mode: zero`; a profile that requires retention does not pass `apply` (GW-15) |
| which providers are allowed | the organization | `subprocessors`; `providers_allowed` only from them (SEC-8) |
| choosing a messenger with third-party storage | the organization | `third_party_storage` and the channel ceiling (SEC-9) |
| retention periods | the organization within the farm ceilings | `Tenant.spec.retention` per class within the class ceilings (RET-9) |
| impact assessment and record of processing | the organization | `dpia_ref`, `ropa_ref`; the privacy report gives the source data (SEC-12) |
| texts for people: privacy notice, bot message | the organization | code texts in the message catalog with fields from the privacy block; `notice_ref`, `notice_version` and the `privacy_notice` delivery at the first contact and on a version change (SEC-13) |
| whether the tenant needs the farm manager | the tenant's organization | `farm_manager: {enabled}`, default `enabled: true`; the seat profile is set in the farm charter (GOV-33); with `false` the farm manager does not read the tenant's data (D-77) |
| whether the tenant sees the operator's reads of its journals | the tenant's organization | `operator_access.debug_journal`, default `notify` (AUD-4) |
| deadline for answering a subject request | the organization per the law | `subject_request_days`, the card and reminders (SEC-11) |
| who sees the monitoring system and how long its history is kept | the organization | the farm sends there only numbers, `enum` values, tags and catalog ids (MON-1); the history of the audit chain head is kept no shorter than the audit period (MON-3) |

## Example on `demo`

1. The operator applies the `demo` manifest with `kind: production` and the privacy block above. `apply` checks that all fields are filled, every purpose has a basis, `recipients` contains `monitoring`, `providers_allowed` contains only `provider-a`, and the messenger has a line in `channels`. The `demo` admins get a notification about the block.
2. `alice` writes to `@demo_bot` for the first time. The messenger itself shows the bot badge, but before the first model text `alice` gets `privacy_notice` version 3: controller Example Org, purposes with bases, recipients `provider-a`, the messenger and the monitoring system, retention periods and how to submit a request.
3. `alice` reads the privacy notice (SEC-13): what the farm stores about her, for which purposes, for how long and who receives it (`provider-a`, the messenger and the monitoring system). There is no separate report about her data (D-114).
4. `alice` writes `/mydata erase`. The core creates a `SubjectRequest` and a card for `carol` with `admin:people`: "Request to erase the data of alice; tasks 42, files 3, sessions 2, memory 4; answer by 2026-11-06; on silence a reminder on 2026-10-22 and a notification to the operator at the deadline; the erasure itself will not be executed".
5. `carol` approves. The core creates an erasure request with the reason `subject_request` and the scope `(demo, p_a11ce)`: it includes two tasks of `bob` where `alice` is in `personal_subjects`, and a group chat session she took part in; the `erase_match` request for her number and name is approved by `bob`, who in this example also has `admin:people`. The core store, the file store, the session store, the memory store, the runtime and the sink answer `done`; the channel without a cache and the model provider answer `not_applicable`; the notes in the monitoring system answer `failed`, and this is acceptable, as they contain no data about people. `alice` gets the outcome as a code text: "Erased in all farm stores. Copies of requests at the model provider are deleted per its terms, within 30 days. You can delete the messenger conversation yourself".
6. A week later the store on `core1` is restored from a backup taken before the request. Before becoming ready, the core replays the erasure log, and the objects of `alice` are erased again.
7. The organization gets an email from `erin`, who is not a farm principal: erase my address `erin@example.org`. `carol` creates an `erase_match` request with this string. The core finds matches in its own stores and, through `match`, in runtime transcripts, records their ids and wipes the string; the card goes to `alice` with `admin:people`: "Matches: tasks 2, memory 1, transcripts 1". `alice` approves. Two tasks of `bob`, one memory item and one transcript are erased entirely, and tombstones remain. The audit has a record of the request, its author, the approval and the number of erased objects, but not the string itself.

## What is in which version

| Version | What appears |
|---|---|
| v0.1 | a retention class in every schema (RET-1); an erasure request for any early erasure, including manual; a store without its own erase operation answers `failed`, and the request stays open with an alarm (RET-7); erasure in C3, C4 and in a runtime with a session or memory inside (RT-1, RT-2); the erasure log and its replay after a restore (RET-5, RET-6); address redaction on the toolhost (INJ-4); the flag `personal_seen` and the field `personal_subjects` (AUD-5); the privacy block with purposes and bases, controller, subprocessors with terms, recipients and channels, only listed providers, opting out of the farm manager with `farm_manager.enabled: false` (SEC-8, D-77); the privacy notice `privacy_notice` with a version (SEC-13); the subject request `/mydata erase` with the `subject_request` card, erasure by scope with `personal_subjects` and group sessions, automatic `erase_match` when a principal is erased, erasure by string through P2 `erasure.match` with `match` in the runtime and the sink, erase operations of C1, C3, C4, E2, E4 and E5, non-waivable for a production tenant (SEC-11, RET-4, D-76); P4 sets and checks the region and the retention period at the provider (GW-14, GW-15); the ceiling of channels with third-party storage (SEC-9); the "you are talking to a bot" message and the model text mark; a person's own memory `/memory`, `/forget` (MEM-4); the audit hash chain (AUD-6) with its head in the monitoring system's history (MON-3); only numbers, `enum` values, tags and catalog ids to the monitoring system (MON-1, MON-2); after the core and the first bot (D-71, D-80): reading content for incident review under the purpose `incident_review`, people's files only as the subject of an incident, the read log (MGR-20, MGR-28, MGR-30); a separate permission to read the audit (AUD-7); monitoring problems enter the farm without trigger names and texts (MON-5) |
| v0.2 | tenant retention periods and deletion by period in every copy (RET-9, RET-10); `erase` and `match` of the channel cache E1 and E4 `purge` for every runtime (RET-11, SEC-11); export only to a sink with `export_only` and a person's copy only to that person (SEC-10); the copy request `/mydata export` and the export task (SEC-11, LCY-2); de-identification of per-principal usage after 93 days (SEC-14, BUD-16); operator reads of journals per `operator_access.debug_journal` (AUD-4); the privacy report (SEC-12); the principal lifecycle with offboarding and export (LCY-2, LCY-3); the attempt journal (AUD-4); people's connections to external services (SEC-7); bot notes on monitoring problems with a secret search before delivery (MON-7) |

## Why it works this way

- Legal decisions belong to the organization, not to the skeleton. If fields and checks exist for them in advance, the organization's decision becomes a line of data in the manifest, not new code in every implementation.
- An erasure that bypasses the log comes back from a backup. So the erasure log is needed already in v0.1, together with erasure at a person's request (D-76); the report and the data copy arrive in v0.2.
- Personal data arrives as free text from everywhere, and a data class cannot contain it. An origin flag set by code honestly answers the question "where could it have ended up".
- Reports and cards carry no content; otherwise the answer to a data request would itself become a leak. A full copy goes only to an export sink.
- Erasure by string is dangerous in the wrong hands: one short string would match everything. So a second person approves it, looking at the match counts.
- The gateway inside the core sets the region, because the runtime is outside the trust boundary and could substitute it.
- The monitoring system already runs at the organization and sees its machines. The farm does not create a new data recipient for alarms, and sends there only what says nothing about people, but it still names it as a recipient: a bot note is free text.
- The privacy notice and the bot message are different things: a bot badge in the messenger does not say who the controller is or where the data goes. So the notice goes as a separate delivery with a version (SEC-13).
- Erasure by principal must reach other people's tasks where the person was mentioned or forwarded. The core already sees these cases through the flag, so it also records whose principal it is (`personal_subjects`), and `erase_match` by their name and id picks up the rest.

## What is not decided

- **Whether to restrict task enqueueing by processing purposes.** Right now only the purpose `incident_review` changes the behavior of the core, and the other purposes describe processing for reports. Linking each purpose to task types (refusing enqueue outside the declared purposes) can be done later without breaking the schema: the list of purposes is closed and is already in the manifest.

<!-- generated: rules -->
## Rules of this section

Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md](../conformance/rules.md).

| Rule | About | Since | Non-waivable |
|---|---|---|---|
| [AUD-5](../conformance/rules.md#AUD-5) | The "personal data seen" flag | v0.1 | no |
| [SEC-8](../conformance/rules.md#SEC-8) | Tenant privacy block | v0.1 | no |
| [SEC-9](../conformance/rules.md#SEC-9) | Raw logs to a channel only by tenant decision | v0.1 | no |
| [SEC-10](../conformance/rules.md#SEC-10) | A copy of a person's data only to that person | v0.2 | no |
| [SEC-11](../conformance/rules.md#SEC-11) | A person's request and deletion through ports | v0.1 | no |
| [SEC-13](../conformance/rules.md#SEC-13) | The privacy notice as a separate delivery | v0.1 | no |
| [SEC-14](../conformance/rules.md#SEC-14) | Per-principal usage is anonymized after 93 days | v0.2 | no |
