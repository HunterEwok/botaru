---
status: accepted
date: 2026-10-07
decision-makers: skeleton owner
---

# A workspace per tenant; a program as a principal

## Context and Problem Statement

The farm serves several tenants on the provider API key of the organization (D-04), and external programs enqueue tasks next to people. Two questions follow. How are model spending, cache and invoices of tenants kept apart when the organization's limits at the provider are shared? And on whose authority does an external program work? It touches P4, the tenant manifest, the principal record and the permission model.

## Decision Drivers

- cache, invoices and limits of one tenant must not mix with another's (C3);
- the model is paid by API key, not by a subscription, because other people use the bot (D-04);
- one permission model for people, bots and programs (D-40);
- a key must not be a way around groups and capabilities.

## Considered Options

- a provider workspace, key and ceilings per tenant (D-42);
- one workspace and key shared by the tenants: not recorded as compared; D-42 excludes it, "so that cache and invoices do not mix";
- an external program as a `service` principal with its own key and groups, like a person (D-47); no alternative is recorded.

## Decision Outcome

Chosen option: "a workspace per tenant; a program as a principal", because the provider then enforces the tenant wall for cache and spending, and a program gets exactly the permissions its groups grant. Only P4 holds the keys, and a call for a tenant's task goes only with that tenant's key and in its workspace. Each workspace has its own spend and rate ceilings; a provider rate limit is a wait for that tenant, not a refusal for everyone. An external program is a principal of kind `service` with the identity `api:<key_id>`, belongs to one tenant, and calls P1 `enqueue` like any initiator.

### Consequences

- Good, because one tenant's burst does not exhaust another tenant's provider limits.
- Good, because the provider invoice per workspace can be reconciled with usage lines by code.
- Good, because a program's rights are visible as groups and capabilities and are revoked like a person's.
- Bad, because every tenant needs a workspace and a key at the provider, with its own rotation.
- Bad, because a program working in two tenants is two principals with two keys.

### Confirmation

- GW-3, only P4 has the provider key, each tenant has its own workspace: black-box and lint.
- GW-9, each workspace has its own ceilings, checked at `apply`: lint and black-box, fixture `model/profiles-lint`.
- GW-10, a provider rate limit is a wait: black-box and fixture `gateway/gateway`.
- TEN-2, the tenant and the principal come from the credentials: black-box `blackbox:model/tenant-field`.
- IDN-2, the initiator is verified and inherited: black-box `blackbox:model/child-record-fields`, `fx-msg-launder`.
- IDN-4, a principal in one tenant, secrets as hashes: black-box and impl, `blackbox:principal/one-tenant-hashed`, `impl:internal/core/TestSecretsStoredAsHashes`.
- LCY-3, the principal lifecycle: black-box `blackbox:core/principal-offboard`.

## Pros and Cons of the Options

### A workspace per tenant

- Good, because cache and invoices do not mix, and each workspace has its own spend and request rate ceilings (D-42).
- Good, because a provider rate limit is a wait for that tenant, not a refusal (GW-10).

### One shared workspace

- Bad, because cache and invoices would mix, which D-42 rules out.

### A program as a principal

- Good, because one permission model with one check at every point applies to people, bots and programs (D-40).
- Bad, because a program working in two tenants is two principals (IDN-4).

## More Information

Decisions D-42, D-47, also D-04 and D-40. Documents: [models-and-money.md](../models-and-money.md), [permissions.md](../permissions.md), [architecture.md](../architecture.md) ("Tenants and the service tenant `farm`").
