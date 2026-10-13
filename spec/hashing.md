# Canonical form, hashes and signature

## In short

Several farm processes compute the same hash independently of each other: the core and the action toolhost compare `args_hash` (DEP-9), the core and `audit.verify` recompute the audit chain (AUD-6), the toolhost and the core compare the `tools/list` fingerprint (INJ-2), and the core compares task bodies when a key is repeated (IDM-1). For the hashes to match, everyone uses one canonical JSON form, one hash algorithm and one way of writing the value. The signature of the audit chain head is also the same for all implementations. The decision was made at the review of 2026-10-12 (lead decision 3).

## Rules

| What | How |
|---|---|
| Canonical form | JSON Canonicalization Scheme, RFC 8785 (JCS): object keys in ascending order of UTF-16 code units, no whitespace, strings and numbers written per RFC 8785, the result in UTF-8 |
| Input | JSON on the wire is I-JSON (RFC 7493): an object with a repeated key, a string with a lone surrogate or invalid UTF-8, and a number outside `±(2^53 - 1)` are rejected by the binding before the schema check (`spec/bindings/http/README.md`), so such an object is never hashed |
| Numbers | core objects hold only integers within `±(2^53 - 1)`: money is an integer: limits in euro cents, call usage in micro-euros `eur_micros` (BUD-1, BUD-9). Objects from outside (tool schemas of plugins, an `inputSchema` with `multipleOf: 0.5`) may hold fractional numbers: they are written in the full number notation of RFC 8785 |
| Hash | SHA-256 (FIPS 180-4) over the bytes of the canonical form |
| Hash notation | `sha256:` followed by 64 lowercase hexadecimal characters; the schema uses the building block `urn:botaru:schema:common:1#/$defs/hash` |
| Signature | Ed25519 (RFC 8032, the pure variant without prehashing) over the bytes of the canonical form of the signed object; the signature is 64 bytes, written in base64 per RFC 4648 with `=` padding |
| Signing key | the core key for audit heads; the key has a `key_id` (SEC-5), and two keys are valid during a rotation window; the deployment gives the public keys with their `key_id` to those who verify the head (`audit.verify`, the monitoring system). `key_id` is part of the signed object, so a signature cannot be passed off as a signature by another key |
| Signed type | every signed object has a `type` field with the name of the object kind (`botaru.audit-head.v1`), so the signature of one object kind does not fit another |

The hashed object is taken as the schema accepted it: after parsing the JSON, without adding default values and without fields that the core adds itself. A `null` field and an absent field are different objects, and their hashes differ.

## What is hashed

| Hash | Object | Where | Rules |
|---|---|---|---|
| task body hash | the `body` field of the P1 `enqueue` request (`spec/model/enqueue-request.schema.json`) | comparison when `idempotency_key` is repeated: the same initiator and the same hash return the existing task, otherwise `idempotency_conflict` | IDM-1 |
| `args_hash` | the object of call arguments checked by the core (`payload.args` of a `toolcall` work unit, the argument snapshot of an approval, the arguments in the `service_grant` block of the audit, the arguments of a campaign item) | the action toolhost recomputes the hash from `payload.args` and compares it with the approval or the `service_grant` | DEP-9, APR-7, APR-18 |
| `plan_hash` | the plan of a campaign or a script run: the items with arguments in plan order, as the core wrote them when it put the plan up for approval | the card shows the decider the plan that will be executed | ORC-1, SCR-6 |
| `hash` of an audit record | the audit record (`spec/model/audit-record.schema.json`) with all fields except `hash` itself; `prev_hash` is part of the object | the tenant chain and the `farm` chain | AUD-6 |
| `prev_hash` of the first record | for the record `seq: 1` it is `sha256:` followed by 64 zeros | the start of the chain | AUD-6 |
| chain head | the object `{"type": "botaru.audit-head.v1", "tenant": <name>, "epoch": <store epoch>, "seq": <number>, "hash": <hash of record seq>, "key_id": <key id>}` | the signature `sig` with the core key; the delivery `monitor_audit_head` carries `tenant`, `epoch`, `seq`, `hash`, `sig`, `key_id`; the E5 adapter writes the value `epoch=<n> seq=<n> hash=<hex> sig=<base64> key=<key_id>`, where `<hex>` is the 64 characters without the `sha256:` prefix. Within one `epoch` the number `seq` only grows; a smaller `seq` is allowed only with a larger `epoch`, and such an epoch increase appears in the `farm` chain as a record of a restore from backup (RET-6) | AUD-6, MON-3, RET-6 |
| `tools_fingerprint` | the array of tools of a plugin's live `tools/list`, in ascending order of `name`, each with only `name`, `description`, `inputSchema` and `annotations` (an absent field is not written) | the toolhost sends it in P1 `hello` for each plugin, and the core compares it with the manifest and the `enroll` record | INJ-2, DEP-2 |
| P4 tool hash | an element of `tools` of a model request without `cache_control`: the object `{"name", "description", "input_schema"}` (an absent field is not written) | P4 compares the request's `tools` with the P3 catalog of the handle by names and these hashes | GW-1 |
| lease token signature | the lease token (`urn:botaru:schema:lease:1#/$defs/token`) with all fields except `sig` | at the core's choice (D-87 (5)): a core that signs the token does so when it issues it and checks it on every call under the lease; it still accepts a call by comparing it with the lease record. The holder does not parse or change the token. The core chooses the algorithm (HMAC-SHA-256 or Ed25519) and the key, because only the core checks it; `key_id` is part of the signed token | FEN-2 |
| memory proposal hash | the object `{"key": ..., "content": ...}` of the P1 `memory.propose` request | the idempotency key of `propose` | MEM-3 |
| script list hash | the array `scripts[]` of the event `ext.monitor.scripts.v1` in ascending order of `scriptid` | the E5 adapter sends the event only when the hash changes | MON-10 |

Hashes of secrets (`verification_code_hash` of a principal, hashes of program keys and tokens) use the same building block but are not compared with other processes. A secret hash MUST be computed with a keyed function or a slow function (HMAC-SHA-256 with a core key, or Argon2id); a plain SHA-256 over a secret is forbidden, because a short code is brute-forced at once.

A restore from backup starts a new epoch of the chains: the core writes into the `farm` chain an audit record of kind `restore` with the block `restore {epoch, previous_epoch, backup_at}` (`spec/model/audit-record.schema.json`). This record is hashed and is part of the chain like any other; the chain head after it is signed with the new `epoch`, and from this epoch on the number `seq` may start lower than before (D-86 (7), RET-6).

## Hashes and erasure

An unsalted SHA-256 over a person's short string (a phone number, an email address, a name) can be reversed by a dictionary search, so such a hash is personal data.

- A hash over a person's content is erased together with the object: the idempotency record of `memory.propose` is in the scope of a principal's erasure request (RET-4).
- `args_hash` in the audit is allowed only while the arguments contain no fields of class `personal`: the audit is excluded from erasure (AUD-3), and the toolhost recomputes this hash without a core key (DEP-9).

## Example

The arguments `{"source": "nginx-access", "lines": 200}` in canonical form are the bytes `{"lines":200,"source":"nginx-access"}`; `args_hash` is `sha256:` followed by the hexadecimal SHA-256 of these bytes. A check of an implementation: the same object with a different key order and whitespace gives the same hash.

## Checks

- A hash fixture is an object, its canonical form and the expected hash; the runner has the implementation compute the hash and compares. As long as there are no such fixtures in `conformance/fixtures/`, the rule is checked by a suite description.
- `audit.verify` with a mock monitoring system finds a record changed behind the core's back and rejects a head with a foreign signature (MON-3).
