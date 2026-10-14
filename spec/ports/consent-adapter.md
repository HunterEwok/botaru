# Consent adapter

How the core is built, not a contract: the adapter is a process of the deployment that uses only P2 on behalf of the verified person (C10, D-114).

## In short

The consent adapter lets a person connect their own account at an external service, for example their mail or calendar, to the farm (SEC-7). It shows the provider's consent screen, receives the token, puts it into the deployment secret store and registers a connection record (`spec/lifecycles/connection.md`). The core calls nothing in it, so it adds no port operation. The model never takes part: it neither starts a connection nor sees the token or the account. The reference is the OAuth 2.0 authorization code flow with PKCE, running next to the channel.

## How it works

- The person sends `/mydata connect <provider>` in a private chat; the core issues a link nonce (single use, 10 minutes, bound to the tenant and the verified principal), and the channel answers with a link to the adapter that carries it.
- After the provider returned a token for the scopes the person chose, the adapter calls P2 `connection.register` with the nonce, the provider, the external account id, the granted scopes and `expires_at`. The principal comes from the nonce, not from the adapter. The core checks the provider and the scopes against the tenant catalog (`spec/model/connection-scopes.yaml`), refuses an external account that another principal has connected (`connection_account_taken`), generates `token_ref` (`conn/<tenant>/<principal>/<id>`) and keeps the record `pending` until the person confirms the `connection_confirm` card in their own private chat; an unconfirmed connection is revoked after 10 minutes.
- The person revokes on the adapter page, or the provider reports a revoked token: the adapter calls P2 `connection.revoke`. When the core revokes a connection (offboarding, erasure, compromise, `/mydata connect revoke`), the adapter deletes the token from the secret store and revokes it at the provider where the provider allows that.
- The token value stays in the deployment secret store and never passes through P1-P4, records, audit, events, logs or the model context.

## What the adapter does not do

It calls no tools and does not read the external account: tool calls with a connection go through P3 with `credential: per_principal`. It creates no connection for a bot, a service principal or `system`, and it does not choose a connection for a task: the core selects it by the task's initiator.
