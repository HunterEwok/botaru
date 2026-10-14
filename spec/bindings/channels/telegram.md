# Channel binding: Telegram

## In short

The reference E1 channel of v0.1. The `telegram` driver talks to the Telegram Bot API server on behalf of the tenant's bot (in the examples this is `@demo_bot` of the `demo` tenant) and turns its updates into `Activity`, button presses into P1 `decide` and operations, and deliveries into messages. This file holds only the Bot API fields and calls that the sheet `spec/ports/e1-channel.md` and the docs need; the driver does not use the other Bot API capabilities. Manifest example: `examples/tenants/demo/channels.yaml`.

## Version

| What | Value |
|---|---|
| API | Telegram Bot API 7.0 or later: this version has the fields `link_preview_options`, `forward_origin` and `quote`, which the binding relies on |
| Call address | `<Bot API server>/bot<token>/<method>`; the server address is a deployment setting, on the test bench it points to a fake server inside the test bench network (`docs/quality.md`, section "Bot evaluation (eval)") |
| Check | the owner checks against the Bot API description before the v0.1 release; the binding is checked by the `conformance/suites/channel` suite on a fake server |

## Secrets

- The deployment stores the bot token and gives it to the channel process by name from the deployment settings; the value does not get into the core store (SEC-1, IDN-7). The P2 secret of the channel is not the bot token: the core issues it to the `telegram` driver at `worker.enroll` (field `p2_token`, D-87 (3)).
- The webhook secret (`secret_token` of the `setWebhook` method) is also a deployment secret by name. The bot token is not part of the webhook path or the logs.

## Receiving updates

The driver receives updates in one of two ways; the Bot API does not allow using both at once.

| Way | How it works | Authenticity | When receipt is acknowledged |
|---|---|---|---|
| Polling `getUpdates` | long polling with the `timeout` parameter and the `allowed_updates` list | the answer came over TLS from the Bot API server on a request with the bot token | the driver passes `offset` = last `update_id` + 1 only after the core answered `submit_activity` for this update (DLV-5) |
| Webhook `setWebhook` | the Bot API server sends a `POST` with one update to the driver's address; at `setWebhook` the driver sets `secret_token` and `allowed_updates` | each request carries the header `X-Telegram-Bot-Api-Secret-Token`, equal to the webhook secret; the value is compared with the secret in constant time; a request without the header or with another value is refused with code `401` and creates nothing (IDN-3) | the driver answers `2xx` only after the core answers; on the core's `not_ready` it answers with an error, and the Bot API server submits the update again (DLV-5) |

`allowed_updates` are `message`, `callback_query`, `chat_member` and `my_chat_member`. The driver does not request other kinds of updates and does not submit them to the core.

One update submitted twice (a webhook retry, two channel processes, a restart before `offset` moved) gives one task: the core builds the idempotency key from the channel name and `update_id` (`system:activity:<channel>:<update_id>`, DLV-5).

## Input: `message` -> `Activity`

| `Activity` field | Source in Telegram |
|---|---|
| `kind` | `message` |
| `channel` | the farm channel name from the manifest (`telegram`) |
| `update_id` | `Update.update_id` |
| `sender.identity` | `telegram:<Message.from.id>`; the numeric user id, not `username` |
| `sender.verified` | `true` if the message has `from`, has no `sender_chat` and `from.is_bot` is false; `false` for a message on behalf of a chat or channel (`sender_chat`) and for an anonymous group administrator |
| `chat.kind` | `direct` for `Message.chat.type: private`; `group` for `group` and `supergroup`. The driver does not submit messages of chats of type `channel` |
| `chat.external_id` | `Message.chat.id` as a string |
| `text` | `Message.text`, for a file `Message.caption`; markup (`entities`) is not passed on. A command is the first word in a private chat; in a group the form is `/<command>@demo_bot` |
| `attachments` | the message file (`document`, the largest `photo`, `audio`, `video`, `voice`): the driver gets it with the `getFile` method, downloads it from the Bot API server and puts it through P1 `file.put`; the `Activity` holds only `file_ref` (INJ-8). The driver never downloads a URL from the text |
| `reply_to` | `Message.reply_to_message.message_id` if the message is a reply |
| `forwarded` | `true` if the message has `forward_origin`, `via_bot` (a third-party inline bot composed the text), `is_automatic_forward` (automatic forwarding from a channel to a discussion group) or `sender_business_bot`: such a message is never a command (IDN-7) |
| `quoted` | `true` if the message has `quote` or `external_reply`: a quote is not a command |
| `received_at` | the time the driver received it, in UTC; the core does not use `Message.date` for decisions |

The core looks up the identity only among the subjects of the channel's tenant (TEN-4). A message from a non-subject in a bound chat gives no `Activity` and no `file.put`: the driver saves nothing of it and passes nothing to the core except the event `activity.ignored(not_subject)` (MSG-17, DLV-7).

The driver declares `identity_assurance: strong`: the Bot API server sets `from.id`, and the authenticity of the update is checked by the receiving method. Messages with `sender.verified: false` do not execute privileged commands and do not approve actions (IDN-3).

## Chat membership

- The Bot API server sends `chat_member` updates only if the bot is a group administrator and `chat_member` is in `allowed_updates`. By them the driver updates the member list of the chat binding (join: a new `status` `member`, `administrator`, `creator` or `restricted` with `is_member: true`; leave: `left` or `kicked`). For members who are subjects of the tenant, the driver keeps their identity; for the others only the number of such members and the `unknown_member` flag, not their ids. A member entry is deleted on `left` or `kicked`. The member list lives only at the chat binding and is part of the `match` search by identity.
- `my_chat_member` says that the bot was added, made an administrator, demoted or removed from the chat.
- The driver may declare `membership_assurance: enforced` for a group chat only while the bot is an administrator in it and receives `chat_member`; otherwise `none` applies to that chat. A member appeared who is not among the tenant's subjects: deliveries to the chat are `withheld(unknown_member)` until checked (DLV-7).

## Card buttons: `callback_query`

- A button is an `InlineKeyboardButton` with a `callback_data` field of no more than 64 bytes. Its form: `<short card id>:<nonce>:<button code>`. For this channel the core issues the card `nonce` of exactly 22 base64url characters (128 bits, within the `p1-decide-request` schema); the button code is two characters from the table below, by which the driver restores the `code` of the card button (`card.buttons[].code`); the core knows the subject kind by the id prefix. The longest entry: 35 + 1 + 22 + 1 + 2 = 61 bytes. There is no model text and no arguments in the button (APR-3). All group members see `callback_data`, and any client can send it for any message, so the `nonce` binds to the card and is not a secret. If a button cannot be built, the card shows a text command with the id and `nonce`, and it is checked the same way as a press (APR-4).

| Button `code` | Code in `callback_data` | | Button `code` | Code in `callback_data` |
|---|---|---|---|---|
| `approve` | `ap` | | `withdraw` | `wd` |
| `reject` | `rj` | | `override_veto` | `ov` |
| `defer` | `df` | | `would_accept` | `wa` |
| `exclude` | `ex` | | `would_not_accept` | `wn` |
| `details` | `dt` | | `confirm` | `cf` |
| `remind` | `rm` | | `count` | `ct` |
| `accept` | `ac` | | `full_list` | `fl` |
| `yes` | `ye` | | `pause` | `ps` |
| `no` | `no` | | `cancel` | `cn` |
| `unknown` | `un` | |  | |

The `details` and `remind` buttons lead to P1 `card.details` and `card.remind`, `pause` and `cancel` to the button's operation, the others to P1 `decide` with `decision` equal to `code`. The driver refuses an unknown code with an `answerCallbackQuery` answer and does not pass it to the core.

- A press comes as a `callback_query` update: the person who pressed is `callback_query.from.id` (`telegram:<id>`), the card chat and message are `callback_query.message.chat.id` and `message_id`. The driver calls P1 `decide` (or the button's operation) with the headers `Botaru-On-Behalf: telegram:<id>`, `Botaru-Chat: <direct|group>:<chat.id>` and `Botaru-Message: <message_id>` (`spec/bindings/http/README.md`). The core takes the decision: the chat is the one where the card was delivered, the `message_id` of the press equals the `external_id` of the delivery of this card to this chat, in a group only under `membership_assurance: enforced`; otherwise `forbidden`.
- To each press the driver answers `answerCallbackQuery` with the same `callback_query_id` and a short code text from the text catalog (accepted, no permission, expired, already decided); otherwise the Telegram client shows the press as unfinished.
- A repeat of the same `callback_query` (an update repeat) gives one decision (IDM-3).

## Deliveries

| What | Bot API call | Fields |
|---|---|---|
| new message (task answer, code texts, card, summary) | `sendMessage` | `chat_id` from the delivery target, `text` of no more than 4096 characters (`max_text: 4096`), `link_preview_options: {is_disabled: true}`, without `parse_mode` (no markup), `reply_markup` with buttons for a card; the driver hands in the `message_id` of the answer in the receipt as `external_id` |
| file | `sendDocument` | the same `chat_id`, the file from `file.get`, a caption without markup |
| card update (`card_update`, script progress card) | `editMessageText` | `chat_id` and `message_id` from the `external_id` of the earlier delivery, the same prohibition of previews and markup; no new message is sent (FEN-7) |

- The driver cuts out links outside the tenant's list before the call; a found secret stops the sending (INJ-11).
- The driver shows the data origin mark `dataorigin: model` as a visible line (`provenance: visible_label`): a Bot API message has no field for such a mark. Under `ai_disclosure: first_contact`, as in the example manifest, model text for a recipient waits until the code text `ai_notice` has been delivered to them (DLV-12, DLV-13).
- There is no streaming output: model text goes as one message after the attempt's `complete` (DLV-11).

## Rate and errors

- Bot API limits: no more than one message per second to one chat, no more than 20 messages per minute to one group and about 30 messages per second per bot in total. From v0.2 the driver declares these maxima: `per_chat_per_minute: 60`, `per_group_per_minute: 20`, `per_minute: 1800`. `Channel.spec.rate` can only lower them (CAP-3; `rate: {per_minute: 20}` in the example).
- The answer `429 Too Many Requests` carries `parameters.retry_after` in seconds: the driver stops sending to that chat and hands in `fail` with `retryable: true` and `retry_after_s` equal to `retry_after`. A `5xx` answer and a dropped connection are also `retryable: true`. From v0.2 a `429` does not spend a delivery attempt: the delivery stays `planned`, and the chat is paused for `retry_after_s` (CAP-3).
- The answers `400` (bad request) and `403` (the bot was removed from the chat or the user blocked it) are final: `fail` with `retryable: false`. The farm ceiling limits the number of delivery attempts; attempts exhausted: the delivery is `dead(attempts_exhausted)`, the task does not change (DLV-3, DLV-4).
- The reason code of `fail` comes only from `spec/model/reasons.yaml`; the binding introduces no codes of its own.

## Checks

The fake Bot API server of the `conformance/suites/channel` suite and of the eval test bench can: return recorded updates through `getUpdates` and through a webhook with the header `X-Telegram-Bot-Api-Secret-Token` (and without it, for a refusal); repeat an update; answer `429` with `retry_after` and `403`; accept `sendMessage`, `editMessageText`, `answerCallbackQuery` and `sendDocument` and show the runner what was sent. Checks: `adapter:channel/offset-after-ack`, `adapter:channel/update-twice` and the `blackbox:channel/*` checks of `conformance/suites/core-blackbox/channel.yaml`.
