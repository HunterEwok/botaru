# Lifecycle: connection

## In short

A connection (`Connection`) is a recorded access of one person to their own account at an external service, for example their mail or calendar (SEC-7). The person creates it themselves through the consent adapter: the adapter shows the provider's consent screen, receives the token, puts it into the deployment secret store and registers the connection with P2 `connection.register` on behalf of the person. The model never creates, names or chooses a connection, and the token value never passes through P1-P4. A plugin tool with `credential: per_principal(<provider>, <scopes>)` gets only the active connection of the task's initiator that carries all the scopes; tasks of other principals and of the service principal `system` get none and the call ends with `error(connection_missing)`. A write on behalf of the person stays a tier 3 action with an approval. A connection lives in the tenant of the person; the record is of class `content` and is deleted by an erasure request (RET-7).

Record fields: `id` (prefix `cn_`), `tenant`, `principal` (only `human`), `provider` (from the tenant catalog `connection_providers`), `scopes` (from `spec/model/connection-scopes.yaml`), `state`, `token_ref` (a reference into the deployment secret store, never the value), `created_at`, `expires_at` (at most `max_days` of the provider, 90 days by default), `revoked_at`, `revoke_reason`. Schema: `spec/model/connection.schema.json`.

## States

| State | Meaning | Terminal |
|---|---|---|
| `active` | the connection is in force; tools of the provider use it for tasks of this initiator | no |
| `expired` | `expires_at` arrived by the store clock; tools do not use it, the person connects again | yes |
| `revoked` | the person, an admin, offboarding, erasure or the provider ended it; the token reference is deleted and the deployment deletes the token | yes |

## Transitions

| From | To | Trigger | Condition | Rules |
|---|---|---|---|---|
| (none) | `active` | P2 `connection.register` from the consent adapter on behalf of the person | the principal is an active `human` of the tenant and comes from the on-behalf headers, not from the body; the provider is in the tenant catalog `connection_providers`; the scopes are a subset of the provider's scopes; `expires_at` is not later than `max_days`; a second active connection of the same person and provider is revoked in the same transaction | SEC-7 |
| `active` | `expired` | the background loop `retention` | `expires_at` arrived | SEC-7, TIM-1 |
| `active` | `revoked` | P2 `connection.revoke` | the person themselves, or a person with `admin:people` in the tenant; reason `by_person` | SEC-7 |
| `active` | `revoked` | offboarding of the principal | in the offboarding transaction of the principal; reason `offboarding` | SEC-7, LCY-3 |
| `active` | `revoked` | an erasure request of the person (`/mydata erase`, `forget`) | in the erasure work of the principal; reason `erasure`; the record is then deleted like other `content` of the person | SEC-7, RET-7 |
| `active` | `revoked` | the consent adapter reports that the provider revoked the token | reason `provider` | SEC-7 |

## Invariants

- Only the person creates their connection, through the consent adapter; the model, a worker, a route and a schedule never create, change or choose a connection (SEC-7).
- The core selects the connection by the task's initiator; a task of another principal, of a bot, of a service principal or of `system` never uses it. The cache key of a `per_principal` tool includes the connection id (PRM-12).
- The token value is kept only by the deployment secret store; records, audit, events and the model context carry only `token_ref`.
- A write with a connection is a tier 3 action with an approval; a read scope never enables a write.
- The person's data report lists their connections (SEC-10); offboarding and erasure revoke and delete them (LCY-3, RET-7).

## Versions

| Version | What appears |
|---|---|
| v0.2 | the whole cycle, the consent adapter, `credential: per_principal` |
