# Consent adapter

## In short

The consent adapter is an edge process through which a person connects their own account at an external service, for example their mail or calendar, to the farm (SEC-7). It is not a fourteenth port: the core calls nothing in it, and it uses only P2 operations on behalf of the verified person. It shows the provider's consent screen, receives the token, puts it into the deployment secret store and registers a connection record (`spec/lifecycles/connection.md`). The model never takes part: it neither starts a connection nor sees the token or the account. The deployment runs the adapter next to the channel; the reference adapter is the OAuth 2.0 authorization code flow with PKCE.

## Who calls and who implements

| Side | Who | What |
|---|---|---|
| the person | in a private chat with the bot, the command `/mydata connect <provider>` | the channel answers with a one-time link to the adapter, bound to the verified principal and valid for 10 minutes |
| the adapter | an edge process of the deployment | the consent screen of the provider, the token exchange, the secret store, P2 `connection.register` and `connection.revoke` on behalf of the person |
| the core | P2 | checks the principal, the provider and the scopes against the tenant catalog `connection_providers` and writes the record |

## Operations

The adapter calls these P2 operations; their rows are in `p2-admin-api.md` and `operations.yaml`.

| Call | When | What the adapter passes | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| P2 `connection.register` | after the provider returned a token for the scopes the person chose | the provider, the granted scopes, `token_ref` (the name under which the deployment secret store keeps the token) and `expires_at`; the principal comes from the on-behalf headers of the one-time link, not from the body | `forbidden` | v0.2 | SEC-7 |
| P2 `connection.revoke` | the person revoked access on the adapter page, or the provider reported that the token was revoked | the connection id | `forbidden` | v0.2 | SEC-7 |

## Guarantees

- The token value stays in the deployment secret store. It never passes through P1-P4, records, audit, events, logs or the model context; the core keeps only `token_ref`.
- The person chooses the scopes on the consent screen; the adapter asks the provider only for scopes that the tenant catalog allows for this provider (`spec/model/connection-scopes.yaml`).
- A one-time link works once, for one principal, for 10 minutes; a link opened by another account at the provider still registers the connection only for the principal of the link.
- When the core revokes a connection (offboarding, erasure, `/mydata connect revoke`), the adapter deletes the token from the secret store and revokes it at the provider where the provider allows that.

## What the adapter does not do

- It does not call tools and does not read the external account; tool calls with a connection go through P3 with `credential: per_principal`.
- It does not create connections for bots, for service principals or for `system`.
- It does not choose a connection for a task: the core selects it by the task's initiator.
