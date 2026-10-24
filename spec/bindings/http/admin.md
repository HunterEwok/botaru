# HTTP binding v1: P2 AdminAPI

## In short

P2 listens separately from P1, and worker tokens are not accepted on it (IDN-7). The envelope is the same as for P1 (`README.md` of this folder): `POST /v1/p2/<operation>` with an `application/json` body by the request schema from the map `spec/ports/operations.yaml`, a `200` answer by the response schema, a refusal as problem details. This file says only what is specific to P2: which credentials it accepts and how a channel passes on commands of people.

## Credentials

| Who | `Authorization: Bearer` | What else | What it opens |
|---|---|---|---|
| a person through `farmctl` | the person's credentials, issued by the deployment | | operations within the person's permissions; operator operations from the closed list PRM-14 only this way |
| a channel on behalf of a person | the P2 secret of the channel driver: one per driver in a tenant, issued by the core in the `worker.enroll` answer of the channel process (`p2_token`), stored by the deployment (D-87 (3)) | `Botaru-On-Behalf` and `Botaru-Chat` by the `on-behalf` schema; the identity kind matches the driver of the secret, and for a text command the chat is `direct:` and the private chat of this person with this channel; the private chat is checked by the person's identity, no `ChatBinding` is needed for it (PRM-15, order of checks in `README.md`); otherwise `forbidden` | operations within this person's permissions; an operator operation is always refused with `operator_only` (PRM-14) |
| the monitoring system | the `health` key (one of the `health_keys` values) | | only `health`; any other operation with this key is refused (MON-2, TEN-2) |
| the external backup process | its own report key | | only `backup.report` (RET-5) |
| the usage reconciliation process | its own report key | | only `spend.reconcile` (BUD-10) |

- The `health` key is not a subject: it has no tenant, and the `health` answer carries no tenant data. The `health` handler lives outside the pool of request handlers and answers both when the store is unavailable and in `not_ready`.
- Bots and the subjects `service` and `system` decide nothing through P2: decisions on cards go through P1 `decide` from a `human` subject.
- An operation button on a card (campaign pause, hold on `rollback_card`) calls the same P2 operation with the headers `Botaru-On-Behalf`, `Botaru-Chat` and `Botaru-Message` and passes checks 1-3 from `README.md`: the chat and message are the chat and message of the card delivery, so the button also works in a group chat. Check 4 (private chat only) applies to a text command (APR-4, PRM-15).

## Bodies

- An operation with an `op` field in the request schema (for example `principal.add` and `principal.off` by the `p2-principal-request` schema, `worker.drain` by the `p2-worker-request` schema) puts the operation name of the path into `op`.
- Read operations by tenant (`status`, `doctor`, `hold.list`, `deadletter.list` and others by the map) use the `p2-tenant-request` body. The `tenant` field is accepted only from an operator of the service tenant `farm`; for everyone else the tenant is taken from the credentials.
- A changing operation without its own response shape answers with the `done-response` schema: `ok`, the id of the created object and the number of the operation's audit record (AUD-1).
- `apply` receives from `farmctl` the commit number and the manifests of that commit (`p2-apply-request`); the core applies the set as a whole or not at all (MAN-2). Plan mode is `dry_run: true`. An operator of a tenant applies only objects of that tenant (`metadata.tenant`) and its knowledge; objects of every tenant (global kinds, a model profile without a tenant), `Tenant` objects, skills, deletions and objects of another tenant take an operator of `farm`, and anything else gets `403 forbidden`. A plan (`dry_run: true`) by an operator of a tenant may also hold objects of every tenant and its own `Tenant` object, as the gate of a proposal plans the shared set, but no delete and no object of another tenant. Such a plan lists only the changes and findings of that tenant and of every tenant: findings on objects of every tenant come without their detail, and findings on objects of other tenants or against the sum of all tenants (GW-9) are left out without a sign, so a clean plan of a tenant does not promise that the farm's apply of the set passes.

## Checks

- A worker token on P2 gets `401 unauthenticated` (IDN-7).
- The `health` key on any operation other than `health` gets `403 forbidden`.
- An operator operation with a channel secret gets `403 operator_only` even on behalf of a person with the required permissions (PRM-14).
- A command with `Botaru-On-Behalf` of a foreign kind (`telegram:1001` from a channel with the `hook` driver) or with `Botaru-Chat` that is not this person's private chat gets `403 forbidden`.
