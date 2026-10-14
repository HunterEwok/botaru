# E2 Sink

How the core is built, not a contract: a sink is a worker of the farm that takes work through P1, and P1 is the boundary (C10, D-114).

## In short

A sink writes artifacts to storage outside the farm: a worker of kind `sink` with a worker token of its tenant takes deliveries with P1 `claim(delivery)`, reads files with `file.get` and hands in the written location with `complete`. The reference sink writes JSON and Markdown to disk; from v0.2 an outgoing webhook is also a sink (DLV-17). A sink takes only deliveries whose `target` names it, and only of its own tenant (TEN-3, TEN-4).

## How it works

- The address is built from the `Sink` manifest base, the tenant and the delivery id, never from the task body or the model answer; the write is idempotent by `delivery.id`, which every external write carries (FEN-7). A file with model text carries `ai_generated: true` in its metadata (DLV-13).
- A delivery failure does not restart the task: deliveries have their own leases, retries and the `dead` outcome (DLV-3). The sink counts its lease by its own clock and stops writing when it did not renew it (FEN-6).
- What is written is not above the class ceiling of the manifest, and is not kept longer than the manifest period.
- Erasure (`match` and `erase`): a sink that keeps what it wrote takes `erasure` work units with `op: match` (it answers only with the `delivery.id` values where the subject's exact strings occur) and `op: erase` (it deletes by the scope `(tenant)`, `(tenant, subject)` or a list of ids), and hands in the outcome `done`, `failed` or `not_applicable` with `complete` (RET-4, RET-7, SEC-11). A sink without these operations counts as `failed`, not as "nothing found".
- The webhook sink (`driver: webhook`) posts the delivery as JSON to the manifest `base` exactly, with the Standard Webhooks headers signed by the key named in `signing_key_ref`; it checks the resolved address at every send and refuses farm, loopback, private, link-local, shared and unspecified addresses; it follows no redirects, sends file references without bytes and keeps no copy, so the core closes its erasure steps with `not_applicable` and `declared_none` (DLV-17).
- A data export for the operator (P2 `tenant.export`, `principal.export`) goes only to a sink with `export_only: {recipient_ref, key_ref}`: one recipient, the file encrypted with the key (LCY-2, SEC-10).

## Where the details are

The `Sink` manifest schema; the rows `E2.*` of `spec/ports/operations.yaml`; the delivery and erasure work units of P1 (`work-item`, `erasure-work`, `erasure-outcome`); the check `blackbox:delivery/webhook-send`.
