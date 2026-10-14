# Channel bindings

## In short

Sheet E1 (`spec/ports/e1-channel.md`) says "external system", "update", "button" and names no messenger. A channel binding is a file in this folder that says, for one messenger, how its fields map onto `Activity` (`spec/model/activity.schema.json`) and `Delivery` (`spec/model/delivery.schema.json`), how the driver checks the authenticity of an update and the real sender, how it knows the chat membership and how it renders deliveries. There is one reference channel in v0.1: Telegram (`telegram.md`). Another channel is added as a new file with the same content and a new driver under the meta-schema `spec/manifests/drivers/driver-config.schema.json`; sheet E1 and the core do not change.

## What a channel binding must describe

| Section | What it holds | Rules |
|---|---|---|
| Version | the messenger API version the binding targets, and the date it was checked | |
| Receiving updates | how updates are received (webhook or polling), how the authenticity of an update is checked, when the offset moves or receipt is acknowledged | IDN-3, DLV-5 |
| Input -> `Activity` | which update fields give `update_id`, `sender`, `chat`, `text`, `attachments`, `reply_to`, `forwarded`, `quoted`, `received_at`; which updates give no `Activity` | IDN-3, IDN-7, INJ-8, TEN-4 |
| Sender | where the identity comes from (`<kind>:<id>`), when `verified: false`, which `identity_assurance` the driver declares | IDN-1, IDN-3 |
| Chat membership | how the driver learns that members joined or left, under what conditions it may declare `membership_assurance: enforced` | DLV-7, IDN-13 |
| Buttons | how a card button carries only `(subject, id, nonce)` and the button code, how a press becomes P1 `decide` or an operation, how the channel acknowledges the press to the messenger | APR-3, APR-4 |
| Deliveries | which messenger call renders a delivery, how link previews and markup are turned off, how `card_update` edits an already sent message by `external_id` | INJ-11, DLV-6, FEN-7 |
| Capabilities | the values of `max_text`, `buttons`, `files`, `edit`, `ai_disclosure`, `provenance` for this messenger | DLV-12, DLV-13 |
| Rate and errors | the messenger's rate limits, how the channel answers them (`fail` with `retryable` and `retry_after_s`), which answers are final | DLV-3, DLV-4 |
| Secrets | which secrets are needed (bot token, webhook secret), only by name from the deployment | SEC-1, IDN-7 |
| Checks | what the fake messenger server must be able to do for the `conformance/suites/channel` suite | |

A binding introduces no new operations, reasons or record fields (`spec/bindings/README.md`): an operation exists only as a row of sheet E1, a reason only as a row of `spec/model/reasons.yaml`, a shape only as a schema in `spec/model`. Messenger fields that sheet E1 does not need are not described by the binding and not used by the driver.
