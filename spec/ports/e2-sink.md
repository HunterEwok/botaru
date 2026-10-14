# E2 Sink

Sheet version: 0.1.0.

## In short

E2 is the adapter that writes artifacts to external storage: a worker of kind `sink` that takes deliveries through P1 and writes them to an address built from the manifest. An outgoing webhook is also a kind of sink. The port is thin: one page, one reference adapter (disk, JSON and Markdown), a minimal set of checks (D-46). The reference disk sink works from v0.1 together with the erasure operations `match` and `erase`, because erasure on request depends on them; the sheet as the contract of a replaceable adapter and the outgoing webhook appear in v0.2. A sink of one tenant works only with its own deliveries, and only the sink whose name is in the delivery's `target` (TEN-3, TEN-4). An export of a person's or a tenant's data for the operator (P2 `tenant.export`, `principal.export`) goes only to a sink with a single recipient and encryption (`Sink.spec.export_only: {recipient_ref, key_ref}`).

## Who calls and who implements

- **Implemented by** a sink adapter with a worker token of its tenant; the tenant's `Sink` manifest sets the address base, the class ceiling, the retention period and, for an export sink, `export_only: {recipient_ref, key_ref}` (SEC-10): the single recipient and the file encryption key.
- **Calls** the core through P1: `claim(delivery)`, `file.get`, `complete`, `fail`.

## Operations

| Operation | Who calls | What it does | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| P1 `claim(delivery)` -> `write` -> `complete(location)` | adapter | Writes the artifact; the write is idempotent by `delivery.id`; writes `ai_generated: true` into the metadata of a file with model text; the address is built from the manifest base, the tenant and the id, never from the model answer | `fenced` | v0.1 | DLV-3, DLV-6, DLV-13, FEN-7 |
| outgoing webhook (`driver: webhook`) | adapter of the sink kind | An HTTP POST of the delivery as JSON to the manifest `base` exactly (an `https://` address outside the farm), with the Standard Webhooks headers: `webhook-id` = `delivery.id`, `webhook-timestamp`, `webhook-signature` = `v1,<base64 HMAC-SHA256>` over `<id>.<timestamp>.<body>` with the secret named by `signing_key_ref`; 2xx completes; a timeout, 429 and 5xx are retried up to `dead`; any other answer is `dead` at once; no redirects; file references without bytes; it keeps no copy, so the core closes its erasure steps with `not_applicable` and `declared_none` | `fenced` | v0.2 | DLV-17, DLV-3, FEN-7 |
| `match(tenant, subject, needles)` | `erasure` loop as an `erasure` work unit (the adapter takes it with P1 `claim` and hands in the result with `complete`) for a match search | Searches the tenant's written artifacts for the subject's exact strings and answers only with a list of `delivery.id`, without content; a sink without `match` in the `erasure` suite counts as `failed`, not as "nothing found" | `not_ready` | v0.1 | RET-4, RET-7, SEC-11 |
| `erase` | `erasure` loop as an `erasure` work unit (the adapter takes it with P1 `claim` and hands in the result with `complete`) on an erasure request | Deletes what was written by the scope `(tenant)`, `(tenant, subject)` (artifacts where the subject is the initiator or is in `personal_subjects`) or by a list of ids; the result `done`, `failed` or `not_applicable` goes into the erasure request | `not_ready` | v0.1 | RET-4, RET-7 |

## Guarantees

- MUST: a delivery failure does not restart the task; deliveries have their own leases, retries and the `dead` outcome (DLV-3).
- MUST: the sink measures the lease period by its own monotonic clock and stops writing if it did not renew the lease (FEN-6); an external write carries the work id `delivery.id` (FEN-7).
- MUST: the class of what is written is not above the ceiling of the sink manifest.
- MUST: every sink that keeps what it wrote has `match` and `erase` from v0.1 (a webhook sink keeps nothing, DLV-17); `erase` by scope and by a list of ids deletes in all copies the sink holds and answers with a result into the erasure request (RET-4, RET-7).
- MUST: a data export for the operator is accepted only by a sink with `export_only`: the file is encrypted with the `key_ref` key, there is one recipient, the subject gets the link to the file as code text in a private message; a sink without `export_only` is refused for `tenant.export` and `principal.export` (LCY-2, SEC-10).
- MUST: a sink takes only deliveries whose `target` names it (TEN-3).

## Bindings

The `Sink` manifest schema (`export_only`); the suite `conformance/suites/sink` (v0.2) and the `erasure` suite (`match` finds the subject's canary, `erase` by these ids removes it, a repeated `match` is empty).

## What the port does not do

It does not take addresses from the task body or the model text and does not keep artifacts longer than the manifest period.
