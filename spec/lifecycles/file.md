# Lifecycle: file

## In short

A file is bytes plus a record that only the core writes (ART-1). The file cycle is short: a file is kept until its period ends or until an erasure request deletes it. A file does not change in place: a new version is a new file with a new `file_ref`. After deletion the record remains as a tombstone without content, so that `file_ref` is not used again, and a link in an artifact gives a clear `file_gone` mark instead of a task error. The description of files and their fields is in `docs/capabilities.md`, section "Files and artifacts".

## States

| State | Meaning | Terminal |
|---|---|---|
| `stored` | the bytes and the record exist; the file is handed out by P1 `file.get` to those who are allowed (ART-3) | no |
| `expired` | the `expires_at` period has ended; the file is no longer handed out, the bytes wait for deletion by the `retention` loop | no |
| `purged` | the bytes are deleted in all farm stores, together with the fragments of knowledge corpora in the corpus index (KNW-5); the record remains as a tombstone without content | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `stored` | P1 `file.put`: a channel (a person's attachment), a `job` worker or a sandbox toolhost (a result file) | the caller works for this tenant; the mark is not below the lower bound and not above what the uploader is allowed; the size is within the limits; for a file from a `service` principal with `uploads_personal: true` and for an `ingested` file from a resource with `personal: true` the core sets `personal_seen: declared` | ART-1, ART-3, ART-4, ART-6, CAP-1 |
| `stored` | `expired` | `expires_at` arrived; the core loop `retention` records the transition, and a hand-out after `expires_at` already answers `gone` | the period is the smaller of `Tenant.spec.retention.files` and the task type's `result_ttl`, if the file became a result | ART-1, RET-2 |
| `expired` | `purged` | the core loop `retention` or the `erasure` loop on an erasure request; C1 `file.purge` (from v0.2) | for `retention`: no later than the period plus the loop period | RET-2, RET-4, RET-7, KNW-5 |
| `stored` | `purged` | the core loop `erasure` on an erasure request; C1 `file.purge` (from v0.1, D-76) | an erasure request with the reason `forget`, `offboarding`, `tenant_delete`, `subject_request` or `manual` is written to the erasure log before the first byte is deleted | RET-4, RET-7, KNW-5 |

## Invariants

- Only the core writes the file record. A worker, a runtime and the model only pass bytes and get a `file_ref` (ART-1).
- A `file_ref` carries 128 random bits and is not reused, even after `purged` (TEN-7, RET-3).
- Files are not deduplicated across tenants: identical bytes of two tenants are two files (TEN-5).
- File bytes do not go through task bodies, events, the audit and usage lines: only the `file_ref` is there (ART-1).
- File content in any state is untrusted for the model (ART-2).
- Only the core sets `personal_seen`: at the write with the source `declared`, if the source declared personal data (ART-6), or later, if redaction found it (AUD-5); the flag is not removed and survives `expired`, so that the privacy report and an erasure request see the object.
- A file in `expired` and `purged` is not handed out: P1 `file.get` answers `gone`, and the artifact shows `file_gone` in its place; the task does not fail from this.
- No process and no `farmctl` command has a direct deletion of bytes around the `retention` loop and an erasure request (RET-7).
- After a store is restored from backup the erasure log is replayed, and files deleted after the backup was taken are deleted again (RET-6).

## Versions

| Version | What appears |
|---|---|
| v0.1 | The states `stored` and `purged`: the file record, P1 `file.put` and `file.get` (ART-1, ART-2, ART-3); `personal_seen: declared` at the write (ART-6); C1 `file.purge` on an erasure request (RET-4, RET-7): deleting a person's data at their request exists from the first version and cannot be waived for a tenant with `kind: production` (D-76). |
| v0.2 | The `expired` state and deletion by period by the `retention` loop (RET-2); deletion of corpus fragments together with the file (KNW-5); size limits (ART-4). |
