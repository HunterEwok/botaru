# Security review, October 2026

## In short

In October 2026 the farm's code was reviewed for what an outside attacker could do: someone who can send messages to the farm's bots, send webhooks to it, plant text in the data the bots read, or open pull requests to the repository. The review covered `main` at commit a4b9f6b and ran from 2026-10-24.

It found 31 issues: 1 high (conditional on the deployment), 7 medium, the rest low or informational. None was critical. No path to code execution, to another tenant's data or to SQL injection was found for an outsider. Every high and medium issue, and most low ones, are fixed in `main`. The ones left are listed in "Not fixed".

## How the review was done

1. **Scanners.** govulncheck found no vulnerability in called code. staticcheck found nothing. gitleaks over the whole history gave 7 hits, all test data. gosec gave 184 notes, each checked by hand.
2. **Manual review by area.** Six areas were each read by a separate reviewer: incoming channels (Telegram, webhooks), the P1/P2/P3 API, the store, the agent and its tools, the Zabbix bridge, and CI and the supply chain. Each finding names the attacker, the path and the impact.
3. **Skeptic pass.** A separate reviewer re-checked every finding against the code and with new tests. Findings were confirmed, downgraded or refuted (1 refuted), and severities were set.
4. **Fixes.** Each fix was reviewed by review agents in rounds, until a round found no serious error.
5. **A hostile model.** The farm's design assumes an attack through data always succeeds: the model does whatever injected text says. `TestP3HostileModel` makes 17 such calls at the tool gateway, among them a shell in a command, its own argv, an unlisted machine or service, a path, a NUL, a log of another tool, wrong types and disguised tool names. Every one is refused before any toolhost sees it.
6. **Fuzzing.** Seven Go fuzzers cover the parsers an outsider reaches: the webhook body and its authenticity headers, the outgoing Telegram text (length, UTF-8, links), button data, Bot API updates, the secret masker and the strict JSON parser. Each ran for 4 minutes with no failure. Their seed corpora run in every `go test`.
7. **A real model.** The blocking injection and isolation eval cases ran against real models. That run found and fixed a runner defect: the runner did not pass the skill folders, so every ops-reader case had timed out. The bench sets up only the roles `hello` and `ops-reader`, so 11 of the 45 cases ran: 2 isolation and 9 injection cases. In all 11 the model did not follow the injected instructions and read no data it should not, with 0 security findings. 6 cases failed a quality check: a field missing from the answer, or a wrong count. The same 4 base cases without the injection fail the same way, so this is the quality of the reference role's answers, not the attack. The run cost 0.32 EUR.

## Findings and fixes

| Area | Finding | Severity | Fix |
|---|---|---|---|
| API | P1 had no TLS while the deployment guide ran it across machines | high (conditional) | #129: P1 over TLS, no plain HTTP to another machine |
| API | No read or idle timeouts on the listeners | medium | #129: timeouts, header and body limits on every listener |
| Telegram | The link filter missed links without a scheme, Telegram short links and mentions | medium | #133: links are cut by Telegram's own rules for marking them |
| Telegram | A message with U+0000 could stall the channel | medium | #135: U+0000 is refused, the driver drops it |
| API | `apply` did not check the operator's tenant | medium | #135: a tenant operator applies only its own tenant's objects |
| CI | The Claude review job read untrusted text next to its token and key | medium | #135: the review runs only for the owner |
| Webhooks | Forged requests could use up the shared rate limit | medium | #137: only refused requests count, per sender network, before the signature check |
| Zabbix | The Zabbix webhook listener had no timeouts | medium | #129 |
| Approvals | Injected bot work could use up the daily card limit of people | low | #137: a quarter of the limit is kept for people (D-128) |
| Secrets | The masker missed some password and token forms | low | #135, #137 |
| Webhooks | A replayed delivery used up the source's rate | low | #137 |
| Programs | A program task started as trusted | low | #126 |
| Several | About 15 smaller issues: worker state on card presses, forbidden-program names, duplicate service names, a cap on tool calls per attempt, LIKE escaping, redirects in the gateway, the Zabbix `-api` address, regexp timeouts, CI concurrency and lockfiles, Dependabot for npm and Docker, secrets in the build context, and unit sandboxing in the deployment guide | low, info | #137 |

## Not fixed

These are left on purpose: they are low, need an operator's mistake or a change of the specification, or cost more than they prevent.

- A Telegram update the core keeps refusing with a server error still holds back the updates after it. The driver does not skip it after a number of tries (DLV-5).
- The audit row for each message from a non-member is not aggregated. That needs a change of MSG-17.
- A profile price for region `us` does not include the provider's surcharge. The operator sets the prices.
- Compose runs the core as the PostgreSQL superuser.
- A Zabbix host is matched by name, not by its id.
- A secret file's permissions are not checked.
- The eval bench client follows redirects. It talks only to the test bench.
- With a daily card limit of 1, nothing is kept for people.
- A replay that arrives while the first delivery is still in flight counts toward the source's rate.
- The masker has false positives, such as `Token: 42`.

## What the owner sets up on GitHub

These are repository settings, not code: secret scanning with push protection, Dependabot alerts, private vulnerability reporting (see [SECURITY.md](../SECURITY.md)), Actions limited to the needed permissions, and the provider key of the review job in an Environment, or a separate key with a spend cap.
