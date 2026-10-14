# Models and money

## In short

The farm pays the model provider by API key (D-04) and counts each tenant's money itself. Every model call goes through the model gateway P4 inside the core (D-45). P4 is not a pass-through proxy: it checks the request, resolves the model from declarations, reserves the worst case before the call and charges by the provider's response, so no farm limit is ever overspent. Each tenant has its own workspace and key at the provider (D-42); the workspace ceiling is a safeguard, and the farm limit is always below it. The model is chosen by declarations (model profile, role slot, task purpose, tenant list), never by the runtime or the model. Money is split into budget lines by the purpose of the work, so analysis, reviews and the farm manager do not eat into the main work. A provider outage does not kill tasks: attempts and runs are spent only where the model answered. Model names and prices are a snapshot at a date (D-05), so this page names models by rank.

## How it works

### Provider, key and workspace

Only P4 holds a tenant's provider key, and a call for a tenant's task goes only with that key into that tenant's workspace, including calls on behalf of the farm manager seat ([GW-3](../conformance/rules.md#GW-3), [MGR-24](../conformance/rules.md#MGR-24)). Bills and prompt caches of tenants therefore never mix (TEN-5). The runtime reaches P4 with a model handle that lives only as long as its lease (RT-3), and it has no other path to a model (DEP-5).

The organization's limits at the provider are shared by all its workspaces, so the deployment describes a spending ceiling and rate limits for each workspace, including the test and eval workspaces, and `apply` checks that every tenant's `month_eur` and their sum stay below a share (0.9 by default) of those ceilings ([GW-9](../conformance/rules.md#GW-9)). The farm limit therefore runs out first, and a tenant gets `budget_exhausted` with a reset time instead of `provider_spend_cap` until the end of the month. `farmctl doctor` reads the ceilings at the provider where the binding allows it and shows a mismatch. Because rate limits belong to the tenant's own workspace, a rate limit answer is a wait for that tenant only ([GW-10](../conformance/rules.md#GW-10)).

There is no automatic switch to another key, workspace or provider: spending would silently move to another bill. A fallback model is possible only when declared, and only inside P4 ([GW-5](../conformance/rules.md#GW-5)). Who pays whom for a tenant's work is not decided (D-12); the farm counts usage per tenant in any case.

### Region, retention and provider terms

The tenant's privacy block sets, for each provider in `privacy.subprocessors`, the processing `region`, the allowed `retention` (`{mode: standard, max_days: 30}` or `{mode: zero}`) and the acceptable `terms` ([Operations](operations.md), SEC-8). Only P4 sets the call region; a region field from the runtime is rejected ([GW-14](../conformance/rules.md#GW-14)). A production tenant whose allowed profiles call a provider outside the EU needs a transfer basis of its own in `spec.model_transfer` (D-87 (2)), otherwise `apply` answers `transfer_basis_missing`. The model profile carries the provider's retention, training, human review and subprocessors with a check date, and a profile worse than the tenant's entry does not pass `apply`, while P4 refuses a call on it with `provider_config` ([GW-15](../conformance/rules.md#GW-15)). Every P4 call goes through these checks, including embedding calls from `kb.ingest.v1` and eval runs. As of 2026-10-07 the provider used below offered no EU region and required 30-day retention for rank 4 models.

### Model profile

A model is named in a `ModelProfile` manifest, shared by the farm or owned by a tenant. This is the `demo` example profile `examples/model-profiles/base-std.yaml`:

```yaml
apiVersion: botaru/v1alpha1
kind: ModelProfile
metadata: {name: base-std}
spec:
  provider: provider-a            # binding in spec/bindings/model-providers/ (v0.1 reference: anthropic.md, D-85)
  model: model-a-small-2026-09    # exact name with version; an alias does not pass apply (GW-7)
  family: family-a                # from model-families.yaml; profiles of one family share a pool of rate limits (GW-10)
  rank: 1                         # 1-4; the role's rank ceiling is checked against it (GOV-3)
  pool: family-a                  # shared rate limit pool
  fallback: []                    # declared fallbacks only (GW-5); empty by default
  fallback_on: [overloaded, unavailable, retired]
  max_output_tokens: 8000         # output limit of one call, reasoning included
  tokens_per_s: 60                # gives the response deadline (GW-12)
  price: {input: 1.00, output: 4.00, checked: 2026-10-01}   # EUR per million tokens
  provider_retention_days: {value: 30, checked: 2026-10-01}
  requires_retention: false
  provider_training: {value: false, checked: 2026-10-01}
  provider_human_review: {value: abuse_only, checked: 2026-10-01}
  subprocessors: []
  retires_at: null                # doctor warns ahead of a retirement date
```

The person who edits a profile converts the provider's prices to euros and sets `checked`; the core fetches no prices or rates, but a date older than 60 days gives a `doctor` warning and one older than 120 days fails `apply` ([GW-17](../conformance/rules.md#GW-17)), because a stale price understates reserves. A new model may have a new tokenizer, so its price is recomputed.

A tenant lists the profiles it allows in `models_allowed` and the providers in `providers_allowed`; a role refers to profiles through slots (`models: {base: base-std}` for `data-reader`). A role whose slot profile is not allowed in a tenant does not pass `apply` there ([GW-7](../conformance/rules.md#GW-7)).

### Slots and purposes

The core picks the slot from the task type's purpose (`spec/model/task-purposes.yaml`), not from the request. The purpose also sets the budget line.

| Purpose | What it is | Slot | Budget line |
|---|---|---|---|
| `work`, `report`, `translate` | main work, a reporter's report, a translator draft | `base` | `work` |
| `insight` | a functionary's analysis of its own work | `analysis` | `insight` |
| `aggregate` | an aggregator's generalization | `analysis` | `aggregate` |
| `review` | a commission lens review | `base` of the reviewer role, `seat` if a seat holder writes it | `review` |
| `governance` | no bot tasks since D-115; the line stays | `seat` | `governance` |
| `manage`, `inspect` | a farm manager decision or read in a tenant | `seat` | `manager` |

Two paths do not depend on the purpose: the slot `granted` under an active model grant, paid from the line `grant` within the grant amount `cap_eur` (MGR-18), and a takeover on the seat profile, paid from the line `takeover` (MGR-21). The `seat` slot applies only to an attempt on behalf of a seat with the current seat epoch. On top of the slot, P4 checks that the profile's rank is within the acting identity's position ceiling, otherwise `rejected(rank_exceeded)` (GOV-3). A model name in the request body chooses nothing ([GW-7](../conformance/rules.md#GW-7)).

A fallback profile is used only for a reason from `fallback_on`, passes the same checks as the main one, and every call on it is recorded with the actual model ([GW-5](../conformance/rules.md#GW-5)). The list is empty by default, because a silent model change breaks the eval, the budget and data processing at once.

### What P4 does on every call

1. Checks the handle: live lease, current store epoch ([GW-3](../conformance/rules.md#GW-3)).
2. Checks the request against the P3 catalog and the binding's field allowlist: no provider server tools, `mcp_servers`, `container` or region field ([GW-1](../conformance/rules.md#GW-1)). The translator's catalog holds only `propose_script` (PRF-1).
3. Resolves the profile by slot and checks rank, provider, region and retention (GW-7, GOV-3, GW-14, GW-15).
4. Looks at the circuit breaker and the rate limit wait of the (workspace, model pool) pair (GW-10, GW-11).
5. Reserves the worst case in every applicable limit and caps the request's output at `max_output_tokens` ([GW-2](../conformance/rules.md#GW-2), [GW-6](../conformance/rules.md#GW-6)).
6. Sets the region and the prompt cache point after the unchanging part of the request (instructions and tool catalog) and sends the call with the tenant's key ([GW-16](../conformance/rules.md#GW-16)).
7. On a response, charges the usage summed over all iterations (input, output, cache write and read, reasoning, compaction), writes a usage line and releases the rest of the reserve ([BUD-3](../conformance/rules.md#BUD-3)).
8. Without a response, charges the full reserve ([BUD-11](../conformance/rules.md#BUD-11)).

A black-box check counts the calls to the mock provider and the usage lines: the numbers are equal.

### Provider outages

None of these reasons spends an attempt. `provider_unavailable` (network, 5xx, overload, deadline, open circuit) and `provider_rate_limited` retry with a pause until the task `deadline`; `provider_quota` waits for dispatch; `provider_config` (wrong key, withdrawn model) raises a code alarm and waits for a person; `provider_spend_cap` and the farm's own `budget_exhausted` are not retried ([BUD-4](../conformance/rules.md#BUD-4), CAP-2). The provider binding maps response codes to these reasons, so rules never name codes. A schedule counts a provider-caused `deadline_exceeded` as `attempts_exhausted` (CAP-4), so it does not breed dead tasks during an outage.

- **Circuit breaker.** A series of failures opens the circuit of a (workspace, model pool) pair: P4 answers `provider_unavailable` at once, the dispatcher holds tasks of that pair with `waiting_reason: provider_unavailable`, and deadlines keep running. After a pause one task makes a trial call, which closes or reopens the circuit ([GW-11](../conformance/rules.md#GW-11)).
- **One retry layer.** The runtime SDK does not retry, the runtime does not retry a P4 error, and P4 retries only a request that was never sent (no provider `request_id`). The core then retries the task with a capped, fully jittered pause ([GW-12](../conformance/rules.md#GW-12)). Three layers of three retries would give 27 requests for one error.
- **Response deadline.** P4 computes it from the profile: `max_output_tokens / tokens_per_s`, times 2, at least 60 seconds, because a rank 4 call with long reasoning takes minutes.
- **Which runs count.** Only a runtime run that got a model response counts toward `max_runs` (6, FEN-3); runs that ended on a provider or core reason before the first response do not ([GW-13](../conformance/rules.md#GW-13)).

### Money: unit and limits

A call's usage and reserve are integer micro-euros (`eur_micros`) computed from profile prices, without rounding at the call; limits, reports and `farmctl` show window sums rounded up to a euro cent ([BUD-9](../conformance/rules.md#BUD-9), D-87 (1)). Three names are kept apart: `tree_budget_eur` (money for a whole task tree, set in the type and mandatory with a model), `max_output_tokens` (one call's output, in the profile) and `run_max_input_tokens` (one run's input, in the P1 work item); a `max_tokens` field fails the lint.

The limits of `demo` (`examples/tenants/demo/tenant.yaml` and `groups.yaml`):

```yaml
budgets:
  month_eur: 300
  day_eur: 15          # default month_eur / 20
  hour_eur: 3          # default month_eur / 100; the HLD-7 spend-rate stop threshold, not a reserve limit
  lines:               # sublimits inside the total; default 0
    insight:    {month_eur: 15, per_role_month_eur: 5}
    aggregate:  {month_eur: 5}
    review:     {month_eur: 50}
    governance: {month_eur: 5}
    manager:    {day_eur: 2, month_eur: 30}
messaging: {wake_day_eur: 1.5, wakes_per_day: 200}   # wake-up counter per (tenant, recipient role), MSG-11
# groups.yaml: user 1 EUR a day, staff 5 EUR, system-ops (principal system) 3 EUR
```

Limits live only in the tenant manifest; P2 `budget.set` can only lower a principal's limit for a while (MAN-5). A principal's daily limit (the largest `day_eur` of their groups) applies only to tasks of the line `work` ([BUD-1](../conformance/rules.md#BUD-1)), so a hundred reviews do not block a nightly schedule of `system`. Money is the one reserved unit: tokens and bot messages are counted in usage lines without a limit of their own, and a tool with `billing: external` must declare `price_eur` and is reserved like a model call ([BUD-5](../conformance/rules.md#BUD-5)). The non-consumable cap `budgets.tasks_in_flight` (3 per person by default) makes extra direct requests wait with `in_flight_cap` instead of being refused (BUD-6).

### Reserve and charge

Overspending any limit is zero ([BUD-2](../conformance/rules.md#BUD-2)). Money is reserved twice:

1. **At enqueue** the core reserves an estimate in the tenant month and day limits, the daily limit of every initiator (line `work` only), the tree limit and the budget line ([BUD-1](../conformance/rules.md#BUD-1), [BUD-7](../conformance/rules.md#BUD-7)). The estimate is the 90th percentile of the actual tree price of this type in the tenant over 30 days, and without history the worst case at the tree limit ([BUD-14](../conformance/rules.md#BUD-14)). A wake-up also counts against the wake-up counter and is skipped with an event rather than refused (MSG-11).
2. **Before each call** P4 reserves the worst case: the input bound plus `max_output_tokens` at the most expensive profile it may use in this call, fallbacks included ([GW-6](../conformance/rules.md#GW-6)).

After the response P4 charges the actual usage and releases the rest; the task's reserve is released when the task becomes terminal. A call without a response is charged at its full reserve with a usage line marked `spend_estimated`, because the provider may have executed and billed it ([BUD-11](../conformance/rules.md#BUD-11)); reconciliation returns what the provider did not take.

The usage line records time, tenant, task, root, principal, chat, role, worker, `bot`, `purpose`, `line`, the actual model and fallback flag, units by kind, the price in `eur_micros`, the run fingerprint and the flags `spend_estimated` and `spend_reconciled` ([BUD-3](../conformance/rules.md#BUD-3)). Model numbers come only from the provider's response through P4; usage reported by a runtime can miss compaction iterations. From v0.2 the `retention` loop strips the person from usage lines after 93 days, keeping the amounts for monthly reports ([BUD-16](../conformance/rules.md#BUD-16)).

### Budget lines

The core sets the line of every usage line from the purpose ([BUD-7](../conformance/rules.md#BUD-7)): `work`, `insight`, `aggregate`, `review`, `governance`, `manager`, `grant`, `takeover`. A line is a sublimit inside the monthly limit, every line except `work` defaults to 0, and exhausting a line never touches `work` tasks. The lines `review`, `governance` and `manager` are reserved in full before the call and never cross their limits.

- The farm manager's work in a tenant (decisions, reads, the daily review) is paid from that tenant's `manager` line with its key; 30% of the day is kept for sev1, and `apply` requires `manager.day_eur` to cover three worst-case incident reads, otherwise `manager_line_too_small` (MGR-20). A tenant with `farm_manager.enabled: false` has no `manager` line (D-77, MGR-42).
- Lens reviews are paid from `review`, at most 30% of it per author role a month; when it runs short, proposals wait in `screened` and then park as `parked(review_budget)` (COM-6). From v0.2 the line also pays the `farm_manager` lens (GOV-25), shadow reviews (COM-10) and the necessity lens on escalations (MGR-6).
- From v0.2 the purposes `insight`, `aggregate` and `review` run in priority class 5 and take at most `dispatch.background_share` (20%) of a shared bot group, so they never take workers from the main work (BUD-8).

At 80% of the tenant's month or day limit, or of a person's daily limit, code sends one message per period; at 100% the task is refused with the reset time. The threshold counts actual usage and the reserves of running attempts, not of tasks waiting for the provider, so six hours of provider downtime do not exhaust a person's day at zero spending. Days and months follow the tenant's time zone (TIM-2). Separately, spending above `hour_eur` in an hour makes code set a `no_claim` stop on every agent role for an hour (HLD-7), so a broken schedule cannot burn a month overnight.

### Campaigns and scripts

A campaign with an approval card is paid from the monthly limit and its type's line up to the amount on the card, and the core reserves that amount in full when it is proposed (ORC-8, [BUD-12](../conformance/rules.md#BUD-12)). A person's daily limit does not apply to its items, otherwise a 40-item campaign would never fit into 5 EUR. A read campaign without a card that is short only of the person's daily limit gets a card instead of a refusal (D-53). A script run is paid like a campaign with a card, at the worst case over all branches, from the line `work` (SCR-8). An item that does not fit the remaining reserve is not released, and the campaign pauses with `estimate_exceeded`, which "Continue" does not lift.

### Reconciliation with the provider

The farm counts usage itself, and code reconciles it with the provider's bill (BUD-10):

1. The operator imports the provider's usage report with `farmctl spend reconcile --file` (or a process outside the core with the reporting key does), one P2 `spend.reconcile` request per UTC day.
2. The core compares the provider's daily sum per workspace with its own usage lines for that day; there is no line-by-line match, since the report has no request ids.
3. The difference becomes one signed correction line. The day's `spend_estimated` lines get `spend_reconciled` without a price change and their reserve returns to the limits. A difference above the tolerance raises `alarm.spend.estimate_drift`.
4. A `spend_estimated` line that no reconciliation reached within `spend.estimate_release_after` (7 days) releases its reserve by time; the later correction line brings in what was actually billed. No reconciliation for `spend.reconcile_overdue_days` (35 days) raises `alarm.spend.reconcile_overdue`.
5. A reconciliation that closes the gap clears the reconciliation alarms and restores the daily limits; neither a person nor time clears them.

**After a restore from backup** the core does not become ready until the usage gap since the backup is filled with `spend_reconciled` lines from the provider's report (RET-6); otherwise calls made between the backup and the failure would vanish from the limits. The only exception is `restore.complete --spend-pending`: the core becomes ready with `alarm.spend.unreconciled`, and the daily limits of all tenants are halved until a reconciliation covers the gap.

### Fixed costs in doctor

Reviews, the farm manager's lens, its daily review and its incident reads run even when nobody asks anything. There is no estimated price table: `farmctl doctor` shows the actual usage of each line over 30 days next to its limits, the daily review as its own row ([BUD-15](../conformance/rules.md#BUD-15)), and P2 `manager.cost` splits the farm manager's usage by kind of work. The money checks at `apply` are three formulas: the workspace ceiling (GW-9), the `manager` line (MGR-20) and the `review` line ([BUD-13](../conformance/rules.md#BUD-13)). The last one requires, when `insight` or `aggregate` is above 0, that `review.month_eur` covers the weekly proposal ceiling of all roles times 4.4 times the price of reviewing one proposal, that is, the sum of `tree_budget_eur` of the review types of all lenses. COM-6 reserves the same sum when a proposal goes to review, so lint and reserve compute the same number. A lens profile with `max_output_tokens` above 2000 fails `apply`, because its worst-case reserve would dwarf the review price.

## Why it works this way

- P4 is not a pass-through proxy: a passed-through request would give the model the provider's server tools around P3, runtime-reported usage misses compaction, and a shared key mixes tenants' caches and bills.
- Reserving the worst case before every call makes overspending zero for any estimate, and keeping the farm limit below the workspace ceiling means refusals come from the farm with a reset time.
- Charging a call without a response in full keeps the limit a ceiling, and reconciliation gives back what the provider did not take; a network failure does not "spend" a month.
- Declarations choose the model because a silent change breaks eval, budget and data processing together.
- Budget lines default to 0, so analysis and reviews are switched on explicitly with money, and the `line` field separates the usage history from the first day.
- One retry layer, a capped jittered pause and counting only runs with a response keep an outage from killing tasks or turning into a storm of retries after recovery.

## Example on `demo`

**One request.** A person in the group `staff` (5 EUR a day) sends a CTD file with `/describe`. The core enqueues `data.describe.v1` (purpose `work`, `tree_budget_eur: 0.20`). With no finished tasks of this type in 30 days, the estimate is the tree limit, so the core reserves 0.20 EUR in the month (300), the day (15), the person's day (5), the tree (0.20) and the line `work`. The `data-reader` worker takes the attempt; its `base` slot resolves to `base-std` (rank 1, 1.00 and 4.00 EUR per million input and output tokens). Before the first call P4 reserves the worst case inside the tree: the input bound plus 8000 output tokens, which is 32,000 micro-euros for the output alone. The provider answers with 3,000 input and 400 output tokens: P4 charges 3,000 + 1,600 = 4,600 micro-euros, writes the usage line and releases the rest of the call reserve. When the task completes, the rest of the 0.20 EUR returns to every limit, and the person's day shows 0.01 EUR (the window sum rounded up to a cent). For scale, the `hello-bot` call on `base-std` (180 input tokens, 20 output) costs 260 micro-euros.

**A dropped connection.** If the provider accepted a call and the connection dropped, the line is `spend_estimated` at the full call reserve, the reserve stays charged after the task ends, and the next reconciliation (or 7 days) turns it into the provider's real figure.

**Fixed costs.** A tenant with 10 workers and about 43 proposals a month, at prices of 2026-10-05 and 0.86 EUR per USD, with token counts that are assumptions to be measured:

| Part | Assumption | Per month |
|---|---|---|
| commission reviews, 4 lenses of rank 3 | 8k input, 3k output per lens, about 0.37 USD per proposal | about 13.7 EUR |
| farm manager lens, rank 4 | 8k input, 3k output, about 0.23 USD per proposal | about 8.6 EUR |
| farm manager daily review, rank 4 | 15k input, 3k output, about 0.30 USD a day | about 7.7 EUR |
| farm manager reads, rank 4 | up to 60k input per read, 3 reads per incident | about 2.5 USD per incident |
| total | | 30-40 EUR |

That is 10-13% of the 300 EUR month. Nothing overspends, but an empty `review` line would silently stall improvements, which is why `doctor` shows every line and the BUD-13 lint catches the shortage. Provider prices on those dates were 1/5, 2/10, 4/20 and 10/50 USD per million input/output tokens for ranks 1 to 4; batch mode was half price and is not part of P4.

<!-- generated: rules -->
## Rules of this section

Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md](../conformance/rules.md).

| Rule | About | Since | Non-waivable |
|---|---|---|---|
| [BUD-1](../conformance/rules.md#BUD-1) | Reserve at enqueue in the tenant, tree and line limits | v0.1 | yes |
| [BUD-2](../conformance/rules.md#BUD-2) | Overspend of any limit is zero | v0.1 | yes |
| [BUD-3](../conformance/rules.md#BUD-3) | A usage line for every call and message | v0.1 | yes |
| [BUD-4](../conformance/rules.md#BUD-4) | Code decides on exhausted budgets and thresholds | v0.1 | no |
| [BUD-5](../conformance/rules.md#BUD-5) | Money is the one reserved unit | v0.1 | yes |
| [BUD-7](../conformance/rules.md#BUD-7) | Budget lines by task purpose | v0.1 | yes |
| [BUD-9](../conformance/rules.md#BUD-9) | Budget in micro-euros per call and in euro cents in limits | v0.1 | yes |
| [BUD-10](../conformance/rules.md#BUD-10) | Reconciliation of usage with the provider report by code | v0.1 | no |
| [BUD-11](../conformance/rules.md#BUD-11) | A call without a response charges its full reserve | v0.1 | yes |
| [BUD-12](../conformance/rules.md#BUD-12) | A campaign with a card and a script run are paid from the monthly limit by the amount on the card | v0.1 | yes |
| [BUD-13](../conformance/rules.md#BUD-13) | Review money covers the weekly proposal ceiling | v0.1 | no |
| [BUD-14](../conformance/rules.md#BUD-14) | The enqueue estimate uses the type's actual price | v0.1 | no |
| [BUD-15](../conformance/rules.md#BUD-15) | Actual usage per budget line in doctor | v0.1 | no |
| [BUD-16](../conformance/rules.md#BUD-16) | The principal in a usage line is anonymized after 93 days | v0.2 | yes |
| [GW-1](../conformance/rules.md#GW-1) | The model request matches the catalog and the allowlist | v0.1 | yes |
| [GW-2](../conformance/rules.md#GW-2) | Worst-case reserve and charging by the provider response | v0.1 | yes |
| [GW-3](../conformance/rules.md#GW-3) | Only P4 has the provider key, each tenant has its own workspace | v0.1 | yes |
| [GW-5](../conformance/rules.md#GW-5) | Only a declared fallback model, with a record | v0.1 | yes |
| [GW-6](../conformance/rules.md#GW-6) | Reserve at the most expensive allowed model | v0.1 | yes |
| [GW-7](../conformance/rules.md#GW-7) | A model only by the role slot and the tenant list | v0.1 | yes |
| [GW-9](../conformance/rules.md#GW-9) | Each workspace has its own ceilings, checked at apply | v0.1 | yes |
| [GW-10](../conformance/rules.md#GW-10) | A provider rate limit is a wait | v0.1 | yes |
| [GW-11](../conformance/rules.md#GW-11) | A circuit breaker in P4 | v0.1 | yes |
| [GW-12](../conformance/rules.md#GW-12) | One retry layer with a pause ceiling and jitter | v0.1 | yes |
| [GW-13](../conformance/rules.md#GW-13) | Only runs with a response count, a configuration error does not spend an attempt | v0.1 | yes |
| [GW-14](../conformance/rules.md#GW-14) | Only P4 sets the call region | v0.1 | yes |
| [GW-15](../conformance/rules.md#GW-15) | Provider retention and terms in the model profile | v0.1 | yes |
| [GW-16](../conformance/rules.md#GW-16) | The prompt cache is the provider's | v0.1 | yes |
| [GW-17](../conformance/rules.md#GW-17) | Profile prices and speed with a date and a shelf life | v0.1 | no |
