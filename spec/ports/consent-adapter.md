# Consent adapter

## In short

The consent adapter is an edge process through which a person connects their own account at an external service, for example their mail or calendar, to the farm (SEC-7). It is not a fourteenth port: the core calls nothing in it, and it uses only P2 operations on behalf of the verified person. It shows the provider's consent screen, receives the token, puts it into the deployment secret store and registers a connection record (`spec/lifecycles/connection.md`). The model never takes part: it neither starts a connection nor sees the token or the account. The deployment runs the adapter next to the channel; the reference adapter is the OAuth 2.0 authorization code flow with PKCE.

## Who calls and who implements

| Side | Who | What |
|---|---|---|
| the person | in a private chat with the bot, the command `/mydata connect <provider>` | the core issues a link nonce (single use, 10 minutes, bound to the tenant and the verified principal), and the channel answers with a link to the adapter that carries it; after the registration the person confirms the `connection_confirm` card in the same private chat |
| the adapter | an edge process of the deployment | the consent screen of the provider, the token exchange, the secret store, P2 `connection.register` and `connection.revoke` on behalf of the person |
| the core | P2 | checks the link nonce, the provider and the scopes against the tenant catalog `connection_providers`, refuses an external account that another principal has connected, generates `token_ref` and writes the record as `pending` until the person confirms the card |

## Operations

The adapter calls these P2 operations; their rows are in `p2-admin-api.md` and `operations.yaml`.

| Call | When | What the adapter passes | Refusals (reason codes) | Version | Rules |
|---|---|---|---|---|---|
| P2 `connection.register` | after the provider returned a token for the scopes the person chose | the link nonce, the provider, the external account `account_id` (the subject id the provider returned), the granted scopes and `expires_at`; the principal comes from the nonce, not from the adapter or the body; the answer gives `token_ref`, under which the adapter stores the token | `forbidden`, `connection_link_invalid`, `connection_account_taken` | v0.2 | SEC-7 |
| P2 `connection.revoke` | the person revoked access on the adapter page, or the provider reported that the token was revoked | the connection id | `forbidden` | v0.2 | SEC-7 |

## Guarantees

- The token value stays in the deployment secret store. It never passes through P1-P4, records, audit, events, logs or the model context; the core keeps only `token_ref`, which it generates itself (`conn/<tenant>/<principal>/<id>`), so the adapter cannot point a connection at another person's token.
- The person chooses the scopes on the consent screen; the adapter asks the provider only for scopes that the tenant catalog allows for this provider (`spec/model/connection-scopes.yaml`).
- A link works once, for one principal, for 10 minutes, and only with the nonce that the core issued. A link that someone forwards to another person does not give the sender that person's account: the connection stays `pending` until the principal of the link confirms the `connection_confirm` card in their own private chat, and the card shows the external account. An external account that has an active or pending connection of another principal is refused (`connection_account_taken`). An unconfirmed connection is revoked after 10 minutes (`not_confirmed`).
- When the core revokes a connection (offboarding of the person or the tenant, erasure, compromise, `/mydata connect revoke`), the adapter deletes the token from the secret store and revokes it at the provider where the provider allows that.

## What the adapter does not do

- It does not call tools and does not read the external account; tool calls with a connection go through P3 with `credential: per_principal`.
- It does not create connections for bots, for service principals or for `system`.
- It does not choose a connection for a task: the core selects it by the task's initiator.
