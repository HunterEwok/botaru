# Models and money

## In short

The farm pays the model provider by API key (D-04) and counts each tenant's money itself. Every model call goes through the model gateway P4 inside the core (D-45). P4 is not a pass-through proxy: it checks the request against the tool catalog, resolves the model from declarations, reserves the worst case and charges by the provider's response, so overspending any farm limit is zero. Each tenant has its own workspace and key at the provider with its own spending and rate ceilings (D-42), and the workspace ceiling remains a safeguard, not the main limit: the farm limit is always below it. The model is chosen not by the runtime and not by the model itself but by declarations: the model profile, the role's slot, the task type's purpose and the tenant's model list. Money is split into lines by the purpose of the work, so analysis, reviews and farm manager work do not eat into the main work. A provider outage does not kill tasks: attempts and runs are spent only where the model answered. Model names and prices are a snapshot as of a date (D-05), so the document names models by rank and gives prices with the date they were checked.

## Provider, key and workspace

Each tenant has its own workspace at the provider and its own key to it (GW-3, D-42). Only P4 holds the key: it is a secret of the P4 process, and the runtime, the workers and the model do not see it. A call for a tenant's task goes only with that tenant's key and into its workspace, including a call on behalf of the farm manager seat in this tenant (MGR-24). This way tenants' bills and their prompt caches do not mix: the provider's cache lives inside the workspace (TEN-5). The model handle that the runtime uses to reach P4 is valid only while the lease it is bound to is alive: the attempt's lease or, with `model_attribution: per_profile`, the worker's lease (RT-3).

The organization's limits at the provider are shared by all its workspaces, so one workspace per tenant is not enough. Each workspace (each tenant, the test ones and the quality evaluation bench) has its own spending ceiling and its own rate limits at the provider, and the deployment describes them for each workspace (GW-9). The core keeps these ceilings consistent with its own limits:

- a tenant's monthly limit `month_eur` is no more than its workspace ceiling multiplied by a share from the deployment (0.9 by default), and the sum of `month_eur` of all tenants is no more than the same share of the organization's ceiling: the farm limit runs out before the provider ceiling, and the tenant gets `budget_exhausted` with a reset time, not `provider_spend_cap` until the end of the month;
- the limits of the test workspaces and the quality evaluation bench together are no more than the share of the organization's limit that the deployment sets (for example, 100 USD a month for the bench); one eval run on the bench is also limited by the thresholds `thresholds.budget` (`max_eur_per_run`, `max_eur_per_case`), and when they are exceeded the run stops (`evals/README.md`);
- the check runs at `apply`: the core compares the description with the tenants' limits;
- there is no daily probe: `farmctl doctor` reads the ceilings at the provider if the provider binding allows it and shows a mismatch with the description, and a configuration failure on a real call sets the reason `provider_config` for the pair (GW-13).

Because the rate limits belong to the tenant's own workspace, a provider rate limit answer is a wait for that tenant, not a refusal, and it does not touch other tenants (GW-10).

There is no automatic switch to another key, another workspace or another provider: when a ceiling is exhausted, the spending would otherwise silently move to another bill. The runtime reaches the model only through P4, with no fallback or auxiliary providers bypassing it (DEP-5); a fallback model is possible only when declared and only inside P4 (GW-5).

Who pays whom for a tenant's work, that is, the payment model, is not decided (D-12). In any case the farm counts usage per tenant itself.

### Region and retention at the provider

The processing region and the allowed retention period of requests at the provider are set by the tenant's privacy block (`docs/operations.md`, SEC-8): each provider in `privacy.subprocessors` has the fields `region` and `retention`.

```yaml
# tenants/demo/tenant.yaml (spec fragment)
privacy:
  subprocessors:
    - provider: provider-a
      agreement_ref: "contracts/provider-a-dpa"
      region: region-a                            # from the region list of the provider binding
      retention: {mode: standard, max_days: 30}   # or {mode: zero}
```

Only P4 sets the region of a call (GW-14): from the `region` of the called provider's entry, in the way that the provider binding describes, that is, with a request field or a workspace setting. The runtime does not set the region, and P4 rejects a region field in its request (GW-1). A profile whose provider does not support this region does not pass `apply` for this tenant. P4 does not call a provider that is not in `privacy.subprocessors`, neither on the main nor on the fallback model (SEC-8).

A region outside the EU requires a transfer basis, and each controller tenant has its own, not the shared model profile (D-87 (2)). A production tenant (`kind: production`) whose `privacy.subprocessors` contains the provider of a profile from `models_allowed` with a region outside the EU (the list of such regions is in the provider binding's note) passes `apply` only if `spec.model_transfer` has an entry for this provider or region:

```yaml
model_transfer:
  - {provider: provider-a, basis: scc, checked: '2026-10-01'}   # basis: scc | adequacy | consent
```

Without the entry, `apply` rejects the profile with the code `transfer_basis_missing` (GW-14).

A model profile has fields for retention at the provider (GW-15): `provider_retention_days` (how many days the provider keeps requests and responses of this model, with a check date) and `requires_retention` (whether the model is unavailable with zero retention). A profile with `provider_retention_days` greater than the `max_days` of its provider's entry, or with `requires_retention: true` at a provider with `mode: zero`, does not pass `apply` in this tenant's `models_allowed`, and P4 rejects a call on such a profile with `provider_config`.

Retention is not all of the provider's terms. The profile has more fields with a check date (GW-15): `provider_training` (whether the provider trains models on API requests), `provider_human_review` (whether the provider's people read requests: `none`, `abuse_only`, `yes`) and `subprocessors` (the provider's own subprocessors for this model, for example its cloud hosting). What is acceptable is set by the provider's entry in the tenant's privacy block: `privacy.subprocessors[].terms` with the fields `training: false` and `human_review: none | abuse_only`, and each subprocessor of the profile must itself be listed in the tenant's `privacy.subprocessors`. A profile with `provider_training: true`, with human review above what is allowed, or with a subprocessor outside the list does not pass `apply` in `models_allowed`, and P4 rejects a call on it with `provider_config`.

Every P4 call goes through these checks, not only bot attempts: an embedding model call from a `kb.ingest.v1` task and eval runs on the bench go through the same GW-14 and GW-15. The embedding model has its own `ModelProfile`, and its provider is subject to region, retention and terms just like the provider of the attempt's model.

An example with a date. As of 2026-10-05, the provider whose prices are checked below deleted API inputs and outputs within 30 days, kept flagged violations for up to 2 years and feedback for 5 years, and did not train models on API data; rank 4 models required 30-day retention and were unavailable with zero retention. As of 2026-10-07 its call region was `global` by default or `us` (1.1 times more expensive), and it did not offer an EU region.

## Model profile

The model is set not in the runtime and not directly in the role but in a named `ModelProfile` manifest. A profile is either shared by the farm or a tenant's own.

```yaml
apiVersion: botaru/v1alpha1
kind: ModelProfile
metadata: {name: base-std}
spec:
  provider: provider-a                   # binding in spec/bindings/model-providers/; v0.1 reference is anthropic.md (D-85)
  model: <exact model name with version> # an alias without a version does not pass apply
  family: family-a                       # from model-families.yaml
  rank: 2                                # 1-4; GOV-3 enforces the role's rank ceiling
  pool: family-a                         # shared rate limit pool at the provider
  fallback: []                           # names of other profiles; empty by default
  fallback_on: [overloaded, unavailable, retired]
  max_output_tokens: 8000                # output limit of one call, reasoning included
  tokens_per_s: 60                       # output speed; P4 computes the response deadline from it (GW-12)
  price:                                 # euros per million tokens; example numbers
    input: 1.72
    output: 8.60
    cache_write: 2.15
    cache_read: 0.17
    checked: 2026-10-05
  provider_retention_days: {value: 30, checked: 2026-10-05}
  requires_retention: false
  provider_training: {value: false, checked: 2026-10-05}        # training on API requests
  provider_human_review: {value: abuse_only, checked: 2026-10-05} # none | abuse_only | yes
  subprocessors: []                      # the provider's subprocessors for this model
  retires_at: null                       # model retirement date, if the provider announced it
```

| Field | Meaning |
|---|---|
| `provider` | provider binding; the provider must be in the tenant's `providers_allowed` and in its `privacy.subprocessors` (SEC-8) |
| `model` | exact model name with version; a profile with an alias without a version does not pass `apply` (GW-7) |
| `family` | model family from `model-families.yaml`; lenses with a veto in a production tenant have profiles of different families, and a match is the `apply` error `veto_lenses_same_family` (GOV-34, `docs/improvements.md`) |
| `rank` | rank 1-4, a rough measure of strength and price; GOV-3 checks the role's rank ceiling |
| `pool` | rate limit pool at the provider; models of one family usually share a pool (GW-10, GW-11) |
| `fallback`, `fallback_on` | fallback profiles and the reasons for which they may be used (GW-5) |
| `max_output_tokens` | output limit of one call; reasoning is part of the output |
| `tokens_per_s` | the model's output speed with a check date; the call response deadline is `max_output_tokens` divided by it, with a 2x margin and no less than 60 s (GW-12) |
| `price` | prices in euros per million tokens of input, output, cache write and cache read, with a check date; reserves and charges are computed from them (BUD-9) |
| `provider_retention_days`, `requires_retention` | retention at the provider (GW-15) |
| `provider_training`, `provider_human_review`, `subprocessors` | provider terms with a check date: training on requests, human review, subprocessors; checked against the tenant's `privacy.subprocessors[].terms` (GW-15) |
| `retires_at` | the announced model retirement date; `farmctl doctor` warns about it in advance |

The provider quotes prices in its own currency. The person who edits the profile converts them to euros and sets the `checked` date; the core does not fetch prices and exchange rates itself, but it watches the age of the date (GW-17): a date older than 60 days gives a warning in `farmctl doctor`, and with one older than 120 days the profile does not pass `apply`. A stale price understates reserves, and the farm limit stops being a ceiling. After a model change the price is recomputed: the new model may have a different tokenizer, and the same text gives a different number of tokens.

A tenant lists the allowed profiles in `models_allowed`, and a role refers to profiles through slots:

```yaml
# tenants/demo/tenant.yaml (spec fragment)
models_allowed: [base-std, analysis-std, review-a, review-b, seat-std, manager-std, translate-std]
providers_allowed: [provider-a]
---
# roles/ops-reader/role.yaml (spec fragment)
models:
  base: base-std              # main work
  analysis: analysis-std      # only the purposes insight and aggregate
```

A role whose `base` or `analysis` slot profile is not in the tenant's `models_allowed` does not pass `apply` for this tenant and does not get `enroll` in it (GW-7).

## Slots and purposes

Which model an attempt gets is decided by the core from the task type's purpose (`purpose`, the closed list `spec/model/task-purposes.yaml`). In v0.1 there are first the purposes `work` and `report` and the slot `base`; after the core and the first bot, together with the improvement cycle, the farm manager and the translator, the purposes `translate`, `insight`, `aggregate`, `review`, `manage`, `inspect` and the slot `seat` appear; `governance` and the slot `granted` appear from v0.2 (D-71, D-80). The purpose determines in code the model slot and the budget line; the priority class is set by the type (`dispatch.priority_class`, DSP-5), and the purposes `insight`, `aggregate` and `review` always run in class 5 from v0.2 (BUD-8).

| Purpose | What it is | Slot | Budget line |
|---|---|---|---|
| `work` | main work | `base` | `work` |
| `report` | a reporter's report | `base` | `work` |
| `translate` | translating an assignment into a script | `base` | `work` |
| `insight` | a functionary's analysis of its own work | `analysis` | `insight` |
| `aggregate` | generalization by an aggregator | `analysis` | `aggregate` |
| `review` | a commission lens review | `base` of the reviewer role; `seat` if a seat writes the review | `review` |
| `governance` | none since D-115: a case runs no bot tasks | `seat` | `governance` |
| `manage` | a farm manager decision in a tenant | `seat` | `manager` |
| `inspect` | reading tenant data for the farm manager | `seat` | `manager` |

Two more paths do not depend on the purpose:

- the slot `granted`: the profile from an active grant of a stronger model to this task (MGR-18), from v0.2; the line `grant` pays for the grant, and the grant ceiling is the amount `cap_eur` in euros, within whose remainder P4 reserves each call under it (GW-2), not a number of tokens;
- takeover (MGR-21), from v0.2: a tenant's task is executed with the seat's profile on a person's card and paid from the line `takeover`.

There is one rule for choosing the model (GW-7). P4 resolves a handle's model only as the profile of one of the role's slots and only if the profile is in the tenant's `models_allowed`. The core, not the request, determines the slot: `analysis` for the purposes `insight` and `aggregate`; `seat` (the `model_profile` from the seat's charter: `commission.model_profile` for commission seats, `farm_manager.model_profile` for the farm manager seat) only for an attempt on behalf of a seat with the current seat epoch; `base` for the other purposes; `granted` only under an active grant. On top of this comes the rank check (GOV-3): the profile's rank is no higher than the position ceiling of the acting identity (`acting_bot`) at the attempt's epoch or, while there are no identities, the ceiling of the role profile; higher only under an active grant, otherwise `rejected(rank_exceeded)`. The request body, the task parameters and the tools do not choose the model: a model name in the request body makes no choice, and P4 rejects a request with a model other than the resolved one. A task type whose purpose requires a slot that the role does not have does not pass `apply`.

### Fallback model

A fallback profile is used only for a reason from the closed list `fallback_on` (`overloaded`, `unavailable`, `retired`) and only from the `fallback` list of the slot's profile (GW-5). It itself passes the same checks as the main one:

- it is in the tenant's `models_allowed`, and its provider is in `providers_allowed` and in `privacy.subprocessors` (SEC-8);
- its rank is not above the ceiling: at `apply` the ceiling of the role profile, at each call the position ceiling of the acting identity (GOV-3);
- its region and retention are acceptable (GW-14, GW-15);
- the role has an eval report on it without security failures (an entry of the `EvalReports` index, `evals/reports/index.yaml` (planned in the installation repository)).

Every call on a fallback model writes the actual model and a fallback flag into the usage line and into the event `model.fallback.v1`. By default the `fallback` list is empty: provider overload means waiting (section "Provider outages"), not a silent model change. A silent model change breaks three things at once: the eval (the answer came from a different model than the one that passed the check), the budget (a reserve at a different price) and data processing (a different provider is a different data processor).

## What P4 does on every call

1. Checks the handle: the lease is alive, the store epoch is current (GW-3, RT-3).
2. Checks the request (GW-1): `tools` matches the P3 catalog of this handle by names and schema hashes; there are no provider server tools, no `mcp_servers`, no `container` and no region field; there are no fields outside the allowlist of the provider binding `spec/bindings/model-providers/<provider>.md` (in v0.1 there is one reference binding, to the Anthropic Messages API, `anthropic.md`, D-85). A runtime that added a built-in tool fails at the gateway. The translator's catalog consists of a single `propose_script` (PRF-1), and a translator request with another tool is rejected.
3. Resolves the model profile by slot (GW-7) and checks the rank (GOV-3), the provider against `privacy.subprocessors` (SEC-8), the region and the retention (GW-14, GW-15).
4. Looks at the pair's circuit breaker (GW-11) and whether the pair waits for the provider rate limit (GW-10).
5. Reserves the worst case in all applicable limits, in the task's budget line and, for a call under a grant, in the remainder of the grant amount `cap_eur` (GW-2), at the most expensive model it may use in this call (GW-6), and sets an output limit in the request no higher than the profile's `max_output_tokens`.
6. Sets the region (GW-14) and the prompt cache point after the unchanging part of the request (GW-16) and sends the request with the tenant's key and into the tenant's workspace (GW-3).
7. Having received a response, charges usage per the provider's response summed over all iterations, including server-side compaction, cache write and read and reasoning, writes a usage line (BUD-3) and releases the rest of the call's reserve (GW-2).
8. Having received no response, charges the call's reserve in full (BUD-11).

Black-box check: the number of calls to the mock provider behind P4 equals the number of usage lines.

### Prompt cache

The prompt cache is part of the provider binding (GW-16). The binding lists the cache control fields that are in the GW-1 allowlist and the rule for where P4 itself sets the cache point: after the unchanging part of the request, that is, the instructions and the tool catalog. P3 returns tools in a stable order, so that this part does not change from call to call. The cache lives only inside the tenant's workspace. Cache writes and reads are paid at the profile prices and are part of the reserve and the charge (BUD-9). Fixture: the second call of the same run reads the cache, and its usage line shows cache reads above 0. As of 2026-10-04 the provider's cache lived about 5 minutes, so it saves within one question but usually does not survive between a person's messages.

## Provider outages

A provider can be unavailable for minutes and hours. During this time the farm must not burn task attempts and runs, and after recovery it must not hit the provider with all the waiting tasks at once.

| Reason | When | Spends an attempt | Retry | Outcome |
|---|---|---|---|---|
| `provider_unavailable` | network, 5xx responses, provider overload, response deadline, open circuit | no | yes, with a growing pause until `deadline` | `failed(deadline_exceeded)` |
| `provider_rate_limited` | the provider asks to wait | no | yes, after `retry_after_s` | `failed(deadline_exceeded)` if the wait would go past the deadline |
| `provider_quota` | the pair waits for the rate limit of the tenant's workspace (GW-10) | no | the task waits for dispatch | `failed(deadline_exceeded)` |
| `provider_spend_cap` | the workspace spending ceiling at the provider is exhausted | no | no | `failed` and a code alarm (CAP-2) |
| `provider_config` | the key is wrong or revoked, no access, the model is not found or withdrawn, the workspace configuration failed, the model is unavailable with the workspace retention | no | not on a timer, the task waits for a fix | code alarm; `failed(deadline_exceeded)` if not fixed before the deadline |
| `budget_exhausted` | the farm's own limit is exhausted | no | no | `rejected` at enqueue or `failed` during work (BUD-4) |

A task that waited for the provider and ended with `failed(deadline_exceeded)` keeps the provider reason in its outcome, and the schedule pause (CAP-4) counts such an outcome as `attempts_exhausted`: a schedule does not breed dead tasks while the provider is down. Which provider responses mean which reason is listed by the provider binding. Response codes differ between providers and change over time, so the skeleton rules do not contain them. The workspace ceiling trips only if the farm itself made a mistake: a tenant's monthly limit is no more than its workspace ceiling (GW-9).

### Circuit breaker

P4 keeps a circuit breaker for each (workspace, model pool) pair (GW-11). A series of provider failures (network, 5xx, overload, response deadline) opens the circuit. While the circuit is open:

- P4 answers `provider_unavailable` immediately, without contacting the provider;
- the dispatcher does not hand out tasks that need this pair (dispatch condition D18 in `docs/coordination.md`, `waiting_reason: provider_unavailable`), and the runtime is not started for them;
- the tasks' `deadline` keeps running.

After a pause the circuit is half-open: one task makes a probe call; success closes the circuit, failure opens it again. A provider configuration error opens the circuit of its pair until it is fixed (GW-13). Fixture: the mock provider answers 529 ("overloaded") for 30 minutes; no task ends with `attempts_exhausted` or `runs_exhausted` before its `deadline`, and there are no more provider calls during the failure than there are probes.

### One retry layer

Only one layer retries a provider error (GW-12). The model SDK in the runtime makes no retries, and P4 retries a call once, only if the request was not sent. The runtime does not retry a P4 error: it ends the run with the reason that P4 returned. After that the core retries the task per the type's retry policy. The pause before a provider-caused retry grows with a multiplier up to a ceiling (60 s by default) and is taken with full random jitter, from zero to the current pause, so that the waiting tasks do not all arrive at once after the provider recovers. Without this rule retries multiply: three layers of three retries give 27 requests for one error.

P4 computes the call response deadline from the profile rather than using a fixed one: `max_output_tokens` divided by the output speed `tokens_per_s`, with a 2x margin and no less than 60 seconds. A rank 4 call with long reasoning takes minutes, and a short deadline would turn it into a call without a response with a full charge. A client retry inside P4 is allowed only if the request was not sent, that is, the call has no provider `request_id`; after the body is sent there is no retry, and a call without a response is charged per BUD-11, because otherwise the provider would execute and bill for both. Fixture: the mock provider takes longer than the deadline to answer; there is exactly one provider call.

### Which runs count

Besides the three attempts, a task has a non-waivable ceiling on runtime runs, `max_runs` (6 by default, FEN-3). P4 marks in the run record whether the run got at least one model response. Only a run with a model response counts toward the ceiling (GW-13). A run that ended with `provider_unavailable`, `provider_rate_limited`, `provider_quota`, `provider_config` or `core_unavailable` (core downtime) before the first response does not count. This way a minute of provider unavailability does not kill tasks in progress, and the model still cannot work on one task without end.

A provider configuration error gives the reason `provider_config`. It does not spend an attempt, is not retried on a timer, gives a code alarm and opens the circuit of its pair, and the task waits for a fix until its `deadline`. Retrying a wrong key on a timer is pointless: only a person can fix it.

### Rate limits

P4 keeps no rate limiter of its own: each tenant's workspace has its own rate limits at the provider (GW-9), so one tenant or an eval run does not take them from the others. A provider answer that asks to wait ends the call with `provider_rate_limited` and the wait time; it does not spend an attempt, and the pair (workspace, model pool) waits until that time (GW-10). The dispatcher does not hand out a task of a waiting pair (dispatch condition D19, `waiting_reason: provider_quota`), and the runtime is not started for it. A run that ended before its first response with `provider_rate_limited` or `provider_quota` does not count toward `max_runs`, and the retry pause has a ceiling and jitter (GW-12).

## Money

### Unit and limits

The usage and reserve of one call are computed from the prices in the model profiles as an integer number of micro-euros (`eur_micros`, 1 EUR = 1,000,000) without rounding at the call (BUD-9, D-87 (1)). Euro cents appear only in limits, reports and `farmctl`: the sum over a window (day, hour, month, tree) is rounded up to a whole euro cent. A `hello-bot` call on `base-std` (180 input tokens, 20 output) costs 260 micro-euros, not a cent. The price of a call includes input, output, cache write and read, reasoning and server-side compaction iterations. Reasoning matters: as of 2026-10-05, rank 3 and 4 models could not switch it off, and it was billed as output. Three different quantities have different names:

| Name | Where | Meaning |
|---|---|---|
| `tree_budget_eur` | task type | money limit of the whole task tree in euros: the root and all child tasks spend from it |
| `max_output_tokens` | model profile | output limit of one call; reasoning is part of the output |
| `run_max_input_tokens` | `limits` of the attempt work item (P1) | how many input tokens the runtime may send to the model in one run, summed over all calls of the run |

There is no `max_tokens` field in the farm's manifests and contracts; the lint rejects it (BUD-9). `tree_budget_eur` is mandatory for every type with a model, including the review types `review.*`, analysis, generalization, ballots and the farm manager: a type with a model without it does not pass `apply`.

Limits on the example of `demo`:

| Limit | Where it is set | Example in `demo` |
|---|---|---|
| tenant per month | `month_eur` in the tenant manifest | 300 EUR |
| tenant per day | `day_eur`, by default one twentieth of `month_eur` | 15 EUR |
| tenant per hour | `hour_eur`, by default one hundredth of `month_eur`; it is the HLD-7 hold threshold, not a reserve limit | 3 EUR |
| principal per day | the largest `day_eur` among the principal's groups; only for tasks of line `work` | `user` 1 EUR, `staff` 5 EUR, `system-ops` (principal `system`) 3 EUR |
| task tree | `tree_budget_eur` of the root type | `ops.investigate.v1`: 0.40 EUR |
| budget line | `lines` in the tenant manifest; in v0.1 `work`, `insight`, `aggregate`, `review` and `manager`, the others from v0.2 | `insight` 15 EUR a month and no more than 5 per role, `aggregate` 5, `review` 50, `governance` 5, `manager` 2 EUR a day and 30 a month |
| units other than money | `spec/model/meters.yaml`; `tokens` and `message` are counted in usage lines without a limit of their own (BUD-5) | from v0.2, 3 tasks in progress per person |
| wake-ups | `wake_day_eur` and `wakes_per_day` in `Tenant.spec.messaging`, per (tenant, recipient role) pair | 1.5 EUR (one tenth of `day_eur`) and 200 a day |

```yaml
# tenants/demo/tenant.yaml (spec fragment)
budgets:
  month_eur: 300
  day_eur: 15                              # default month_eur/20
  hour_eur: 3                              # default month_eur/100; HLD-7 hold threshold
  lines:                                   # sublimits inside the total; default 0
    insight:    {month_eur: 15, per_role_month_eur: 5}
    aggregate:  {month_eur: 5}
    review:     {month_eur: 50}
    governance: {month_eur: 5}
    manager:    {day_eur: 2, month_eur: 30}
```

The tenant's month and day limits, the tree limit and the line are the four reserve limits; the daily limit of a person and the wake-up ceiling are two simple counters next to them (BUD-1, MSG-11). Limits live only in the tenant manifest in git. P2 `budget.set` only temporarily lowers a principal's limit and only with a deadline (MAN-5); a limit can be raised only by editing the manifest and running `apply`. Schedules pay from the tenant's monthly and daily limits and from the daily limit of the principal `system`. A principal's daily limit applies only to tasks of line `work` (BUD-1): reviews, analysis, the farm manager's review and seat tasks are limited only by their line and the tenant limits, so a hundred reviews in a day do not get in the way of a nightly schedule from `system`.

### Reserve and charge

Overspending any limit is zero (BUD-2). Money is reserved twice.

1. When a task is enqueued (`enqueue`), the core reserves an estimate of its price in all applicable limits (BUD-1): the tenant's monthly and daily limits, the daily limit of the initiating principal (only for tasks of line `work`), the task tree limit and the budget line of the type's purpose (BUD-7). If a task has several initiators (for example, a wake-up on several messages that have `origininitiators`), the reserve is taken from the daily limit of each of them. A wake-up task also reserves from the daily wake-up ceiling per (tenant, recipient role) pair, `wake_day_eur` and `wakes_per_day` from `Tenant.spec.messaging`: this is the MSG-11 counter, not a budget line. Campaign items with a card and script steps are paid from the reserve of the campaign or the run (BUD-12). If any limit is exhausted: `rejected(budget_exhausted)`, while a wake-up is skipped with an event, and the inbox entries wait for their deadline (MSG-11).
2. Before each model call, P4 reserves the worst case: an upper estimate of the input plus `max_output_tokens` at the most expensive model that P4 may use in this call, that is, the slot's profile and its declared fallbacks (GW-2, GW-6). A tool call with `billing: external` goes ahead only after a reserve at its `price_eur` from the plugin manifest (BUD-5).

The enqueue estimate is computed as the 90th percentile of the actual price of the finished tasks of this type in the tenant over the last 30 days, using the price of the whole task tree from the usage lines (BUD-14). For a type with no finished tasks in 30 days, the estimate is the worst case: at the most expensive model that P4 may resolve for tasks of this type, and at the tree limit `tree_budget_eur` (GW-6). The way the enqueue estimate is computed does not affect zero overspending: before each call P4 reserves the worst case anyway.

After the response, P4 charges the actual usage and releases the rest of the call's reserve. The task's reserve is released when the task becomes terminal, and a fixture checks that a stuck reserve does not eat the limit (BUD-1).

**A call without a response.** A call that P4 sent to the provider and for which it got no response (connection drop, deadline, restart of the core or P4) is charged at its full reserve with a usage line marked `spend_estimated`, and this reserve is not released until reconciliation with the provider's report or its release by time (BUD-11, BUD-10). The provider may have executed the request and billed for it without the farm learning about it; charging the worst case keeps the limit a ceiling, and reconciliation returns to the limit what the provider did not take. Fixture: the mock provider accepts the request and drops the connection; the `spend_estimated` line equals the call's reserve, and the reserve stays charged after the task ends until reconciliation.

### Usage line

For every model and tool call and for every publication of a bot message, the core writes a usage line (BUD-3).

| Field | Meaning |
|---|---|
| time, tenant, task, tree root | when, whose work and in which task tree |
| principal, chat | initiator and conversation |
| role, worker | who executed |
| `bot` | the bot identity on whose behalf the attempt ran, if there is one |
| `purpose` | the task type's purpose; using it and the task type, P2 `manager.cost` splits the farm manager's usage into decisions, reads, review and reviews |
| `line` | budget line (BUD-7) |
| model | the actual profile and the fallback model flag (GW-5) |
| unit and quantity | for a model, tokens by kind: input, output, cache write, cache read, reasoning, compaction iterations; for a bot message the unit `message` with the size and kind of the message, and the line goes to the sender's tree (MSG-11) |
| price | in micro-euros `eur_micros`, without rounding at the call (BUD-9) |
| fingerprint | the attempt's run fingerprint (PRV-1) |
| flags | `spend_estimated` (BUD-11), `spend_reconciled` (BUD-10) |

The principal in the line is needed for the daily limit and is kept for 93 days: from v0.2, for a line whose `subject` is a person, the `retention` loop removes `subject`, `task` and `root`, truncates `time` to the day and writes `subject_kind: human`, in whole days, so that no join with audit or task records leads back to the person. The amounts and the other fields of the line remain for monthly reports, the line stays in class `metadata`, and this removal is the only edit of a `metadata` row (BUD-16, SEC-14). A series "how much `alice` spent per day over a year" does not exist (DLV-14).

Model numbers are taken only from the provider's response through P4. Usage reported by a worker or the runtime is for reference only: it can be incomplete. As of 2026-10-05, with server-side context compaction, the top-level input and output counters in the provider's response did not include compaction iterations, and the full usage was the sum over iterations.

### Budget lines

Every usage line carries a budget line `line`; the core sets it from the type's purpose, not the worker and not the model (BUD-7). The lines are: `work`, `insight`, `aggregate`, `review`, `governance`, `manager`, `grant` and `takeover`.

- The reserve is taken both in the line and in the tenant's monthly limit: a line is a sublimit inside the total limit, not separate money.
- Exhausting a line rejects or defers only the tasks of that line and never touches the tasks of line `work`.
- The limit of every line except `work` is 0 by default: analysis, reviews and the rest are enabled explicitly with money, for analysis also per role (D-16).
- The lines `review`, `governance` and `manager` are reserved in full before the call and never cross their thresholds: a call that does not fit into the line's remainder does not start.
- For an active tenant the line `manager` is above 0 (lint MGR-20), and from v0.2 the line `governance` too (GOV-25). A tenant that opted out of the farm manager (`Tenant.spec.farm_manager.enabled: false`, D-77) has no `manager` line: the farm manager does not read its data and does not run its review and lens, no tasks with the purposes `manage` and `inspect` are enqueued in it, and the tenant's incidents go only to its admins.
- A line is set by the monthly limit `month_eur`; the line `insight` also has a per-role limit `per_role_month_eur`, and the line `manager` has a daily ceiling and a reserve for heavy incidents (`docs/farm-manager.md`).

Who pays from which line:

- the farm manager's work in a tenant (decisions, reads, the daily review) is paid from that tenant's one `manager` line with its key, with a daily and a monthly limit and 30% of the day kept for sev1 (MGR-20); the service tenant `farm` does not pay for it; the line's daily limit `manager.day_eur` is no less than the price of three reads of one worst-case incident at the seat profile prices, otherwise `apply` rejects the tenant with `manager_line_too_small`;
- lens reviews are paid from the line `review`, and one author role gets no more than 30% of the line per month; if the line or the role's share is not enough, proposals wait in `screened` and after `review_wait_max` move to `parked(review_budget)` (COM-6);
- from v0.2 the line `review` also pays for the review of the `farm_manager` lens on every proposal (GOV-25; it is absent with `farm_manager.enabled: false`, D-77), shadow reviews (COM-10) and the assessment of escalations to the farm manager by the necessity lens (MGR-6);
- the line `governance` stays in the budget (BUD-7); a case runs no bot tasks since D-115;
- a grant of a stronger model is paid from the line `grant`, a takeover from the line `takeover`.

### Priority class

From v0.2 tasks with the purposes `insight`, `aggregate` and `review` run in the lowest priority class (5, `background`), and it cannot be raised (BUD-8). In a bot group that also does other work they together take no more than `Tenant.spec.dispatch.background_share` (20% by default) of the group's capacity: its `max_running`, or the number of its active workers, rounded down but at least one; a task above the share waits with `group_quota`. Tasks of `review.escalation.v1` (the `necessity` lens) go in class 3 and are outside the share, because an escalation waits for them (D-100). This way analysis and reviews do not take workers away from the main work, even when their lines have money.

### Thresholds and refusal

`budget_exhausted` is not retried and does not spend an attempt (BUD-4). The 80% threshold of the tenant's monthly limit or of a principal's daily limit gives one event per period: the tenant admins get one message, and a person gets a message about their own limit. The 80% threshold of the tenant's daily limit gives the same event. At 100% the task gets a refusal with the time when the limit resets. The threshold is computed from actual usage and the reserves of running attempts: the reserve of tasks waiting for the provider (`waiting_reason: provider_unavailable` or `provider_quota`) does not count toward the threshold, even though the reserve itself is kept; overspending stays zero because the reserve is taken again before the call. Otherwise six hours of provider downtime would leave `alice` with `budget_exhausted` at zero spending. Code decides thresholds, not the model. The periods "day" and "month" are computed in the tenant's time zone, and the period boundary is one UTC instant in the reserve, the charge and the refusal text (TIM-2). Lines reserved before the call (`review`, `governance`, `manager`) never cross their thresholds.

Separately from the limits, the core watches the rate of spending: a tenant's spending per hour above `hour_eur` is the incident class `runaway_spend`, and code itself (`contain_auto`) sets a `no_claim` hold on the role for an hour (HLD-7). This way an error in a schedule does not burn a month overnight.

### Campaigns and scripts

A campaign is many tasks of one kind that a person approves with a single card (`docs/coordination.md`). A campaign with an approval card is paid from the tenant's monthly limit and the budget line of its type, up to the amount shown on the card (BUD-12). When a campaign is proposed, the core estimates the price of all its items and its continuation and reserves this amount in full (ORC-8), and the approval accepts exactly that amount. A person's daily limit does not apply to the items of such a campaign; it is checked only for their direct requests: otherwise a campaign of 40 items would not fit into an employee's daily limit of 5 EUR. A campaign without a card reserves the estimate in all BUD-1 limits, including the initiator's daily limit. If a read campaign without a card is short only of the initiating person's daily limit, then instead of a refusal it gets a card and, after approval, is paid from the monthly limit like a campaign with a card (D-53, ORC-8).

A script run (`docs/scripts-and-translator.md`) is paid the same way as a campaign with a card. When the run is approved, the core reserves its estimate in full at the worst case over all branches (SCR-8) in the tenant's monthly limit and in the line `work`, within the amount on the card; the initiator's daily limit does not apply to the run's steps. There is one line, because the step types have the purpose `work` or `report` (SCR-2). `fanout` campaigns inside a run take their reserve from the run's reserve and do not reserve it a second time.

Usage does not go beyond the shown amount. An item that does not fit into the remainder of the campaign's reserve is not released, and the campaign pauses with the reason `estimate_exceeded`. The "Continue" button does not lift such a pause: the campaign can only be canceled and the unreleased items retried in a new campaign with a new estimate (ORC-8).

### Units beyond money

Money is the one unit that is reserved before use and has limits (BUD-5). The registry `spec/model/meters.yaml` names the units a usage line carries: `eur_cents` for money, `tokens` for model tokens by kind and `message` for a bot message; they have no limits of their own. A tool whose calls the farm pays outside the model provider declares `billing: external` with the price of one call `price_eur` in its plugin manifest, and is reserved and charged like money; a tool with `billing: external` without a price does not pass `apply`. Other expenses have their own counters: store bytes in `max_storage_bytes` (CAP-1) and approval cards in `attention.cards_per_tenant_hour` (APR-2).

The non-consumable limit `Tenant.spec.budgets.tasks_in_flight` caps the direct requests of one person or program with a running attempt. Exceeding it is not a refusal: the request waits in `submitted` with `waiting_reason: in_flight_cap` (condition D21), and the person's waiting requests of one type go out in enqueue order, while a request held by another condition (no executor, a hold, a ceiling) does not hold back the others; tasks of `system`, campaign items, children and continuations do not count (BUD-6). Default: 3 tasks in progress per person.

### Reconciliation with the provider

The farm counts usage itself, while the provider bills by its own numbers. Code reconciles the usage of each workspace with the provider's report; this is the measure of the money goal, not a manual check by the operator (BUD-10):

1. The operator exports the usage report file from the provider console and imports it with `farmctl spend reconcile --file`; a process outside the core with the provider's reporting key may hand in the same data. The core, the workers and the runtimes do not have this key.
2. The data reaches the core through P2 `spend.reconcile`, one request per UTC day of the report.
3. There is one reconciliation (D-87 (1)): the core compares the provider's daily sum per workspace with the sum of its own usage lines for the same day in micro-euros. There is no line-by-line reconciliation: the provider's report has no ids of individual requests.
4. The difference is written as one signed correction line (`correction_eur_micros`). The `spend_estimated` lines of that day get the flag `spend_reconciled` without a price change, and their reserve returns to the limits. If the difference is larger than the tolerance from the deployment settings, the core raises the alarm `alarm.spend.estimate_drift` (warn): the farm's bill diverges from the provider's bill and should be investigated.
5. A `spend_estimated` line that no reconciliation reached within `spend.estimate_release_after` (7 days by default) releases its reserve by time: it counts as zero in the limits and in the daily sum, and the correction line of its day later brings in what the provider actually billed. Reconciliation may then run at the rhythm of the provider's reports, for example monthly, without freezing the limits.
6. If a workspace has no reconciliation for longer than `spend.reconcile_overdue_days` (35 days by default, set to the rhythm of reconciliation), the core raises the alarm `alarm.spend.reconcile_overdue` (warn).
7. A reconciliation that closed the gap clears both reconciliation alarms, `alarm.spend.reconcile_overdue` and `alarm.spend.unreconciled`, and restores the daily limits; neither a person nor time clears these alarms.

After the store is restored from backup, the core does not become ready until the usage gap since the backup is filled with `spend_reconciled` lines from the provider's report (RET-6). Otherwise the calls between the backup and the failure would vanish from the limits, and the limit would stop being a ceiling. The only exception is `restore.complete --spend-pending`: the core becomes ready with the alarm `alarm.spend.unreconciled` (warn), and the daily limits of all tenants are cut in half while the gap is open; reconciliation with the report for the gap clears the alarm and restores the limits.

### Usage per line in doctor

Every tenant has fixed costs that run even when people ask nothing: commission reviews, the farm manager's lens on proposals, the farm manager's daily review, the farm manager's reads for incidents. There is no estimated price table: `farmctl doctor` shows for each tenant the actual usage of each budget line over the last 30 days next to the line's limits, with the daily review as a separate row (BUD-15), and P2 `manager.cost` shows the farm manager's usage by kind of work. For a tenant with `farm_manager.enabled: false` there are no farm manager rows (D-77). The money checks at `apply` are three formulas: the workspace ceiling (GW-9), the `manager` line (MGR-20: `manager.day_eur` is not less than three reads of one worst-case incident, 60 thousand input tokens together and three responses at the seat profile's `max_output_tokens`) and the `review` line below.

The `apply` lint checks that there is enough money for reviews with one formula (BUD-13). If a tenant's line `insight` or `aggregate` is above 0, `review.month_eur` must be no less than the sum over the roles of `insight.max_per_week` times 4.4 times the price of the reviews of one proposal. That price is the sum of `tree_budget_eur` of the review types of all lenses that review a proposal: the commission seat lenses and, from v0.2, the `farm_manager` lens (GOV-25), in the `demo` example 0.08 EUR for the types `review.*.v1` on rank 3 profiles and 0.20 EUR for `review.farm_manager.v1` on the farm manager seat profile (`docs/improvements.md`). COM-6 reserves the same sum in the line `review` when a proposal goes to review, so the lint and the reserve compute the same number, and assumptions about the number of tokens are not part of the computation. A lens model profile with `max_output_tokens` above 2000 does not pass `apply`: otherwise the worst-case reserve for a review is many times its price, and proposals sit in `screened` with a full line. Otherwise `apply` is rejected, and `farmctl doctor` shows the missing amount. When the line runs out at run time, proposals wait for review (COM-6).

## Example prices with a date

The numbers below are a snapshot as of a date, not part of the contract. Provider prices in USD per million tokens:

| Rank | Input | Output | Checked |
|---|---|---|---|
| 1 | 1 | 5 | 2026-10-04, 2026-10-05 |
| 2 | 2 | 10 | 2026-10-04, 2026-10-05 |
| 3 | 4 | 20 | 2026-10-04, 2026-10-05 |
| 4 | 10 | 50 | 2026-10-05 |

Other facts with a date:

- 2026-10-04: one question from a person is not one call but a loop, and every call with a tool resends the whole context. Four calls with 35 thousand tokens of context each at rank 2 cost about 0.28 USD without the cache and about 0.12 USD with the cache inside the loop; 1000 questions a month cost on the order of 100-300 USD. The minimum cacheable prefix of a rank 1 model was 4096 tokens.
- 2026-10-05: the provider's batch mode was half the price for non-urgent tasks without a dialog. The provider announced a model's retirement at least 60 days in advance.

Fixed parts for a tenant with 10 workers at 10 proposals a week (about 43 a month). Prices as of 2026-10-05, rate 0.86 EUR per 1 USD (the rate was not rechecked); the token counts are an assumption that needs to be measured (the lint and the reserve do not compute from them but from the `tree_budget_eur` of the review types, BUD-13):

| Part | Assumption | Price | Per month |
|---|---|---|---|
| commission reviews: 4 lenses of rank 3 | 8 thousand input and 3 thousand output tokens per lens | about 0.37 USD per proposal | about 16 USD, about 13.7 EUR |
| farm manager lens, rank 4 | 8 thousand input and 3 thousand output | about 0.23 USD per proposal | about 10 USD, about 8.6 EUR |
| farm manager daily review, rank 4 | 15 thousand input and 3 thousand output | about 0.30 USD a day | about 9 USD, about 7.7 EUR |
| farm manager reads, rank 4 | up to 60 thousand input and 3-5 thousand output per read, 3 reads per incident | about 0.75-0.85 USD per read | about 2.5 USD per incident |
| total | | | 30-40 EUR |

That is 10-13% of a monthly limit of 300 EUR. There is no overspending (BUD-2, BUD-7), but with an empty `review` line improvements silently stall, so `farmctl doctor` shows the usage of every line (BUD-15), and the lint catches a shortage of money for reviews (BUD-13). A round to fill a vacant commission seat (5 seats and the farm manager) costs about 0.48 USD; 4 rounds about 1.65 EUR.

## Who does what

| Who | What it does | What it does not do |
|---|---|---|
| P4 (part of the core) | holds the tenants' keys; checks the request; resolves the model; keeps the circuit breaker; reserves, charges, writes usage lines; sets the region and the cache point | does not let a request through outside the allowlist |
| core | reserves at enqueue; sets the budget line; computes thresholds; checks workspace ceilings at `apply`; compares the imported provider report with the usage lines and turns `spend_estimated` into actual figures; lints profiles, their price age, slots, lines and money for reviews; de-identifies the principal in usage lines after 93 days | does not choose the model from the task text |
| dispatcher | does not hand out tasks with an open circuit or of a pair that waits for the provider rate limit; keeps analysis and reviews in the lowest priority class | |
| operator | creates workspaces, keys and ceilings at the provider and describes them in the deployment; exports the provider's usage report and imports it with `farmctl spend reconcile --file` | does not keep the provider key in runtimes |
| tenant people | set limits and lines in the tenant manifest, allowed profiles and providers; edit model profiles and prices with a date | |
| runtime | reaches the model only through P4 with a handle | does not retry P4 errors, does not set the region or the model |
| model | works within the reserve | does not choose the model and sees no money, except a refusal with a reason |

## What is in which version

| Version | What appears |
|---|---|
| v0.1 | P4 inside the core: request checking, reserve and charge, a key and a workspace per tenant (GW-1, GW-2, GW-3). The model profile, slots, purposes, `models_allowed`, ranks, the fallback model and a reserve at the most expensive model (GW-5, GW-6, GW-7). Euro cents and separate names instead of `max_tokens` (BUD-9), the prompt cache in the binding (GW-16), region and retention at the provider (GW-14, GW-15), workspace ceilings checked at `apply` (GW-9). The circuit breaker, one retry layer, counting only runs with a response, and the reason `provider_config` (GW-11, GW-12, GW-13). The tenant limits `month_eur`, `day_eur`, `hour_eur`, the reserve at enqueue, the usage line, thresholds (BUD-1, BUD-2, BUD-3, BUD-4); the response deadline from the profile and a retry only of an unsent request (GW-12); provider terms in the profile (GW-15); the age of profile prices (GW-17); the budget line `work` (BUD-7); charging a call without a response (BUD-11); the unit registry, so that every usage line carries its unit from the start, with money as the one reserved unit and a price for a paid tool (BUD-5, D-94). This is the money of the `core` profile and the first bot (D-71). After the core and the first bot (D-80): the lines `insight`, `aggregate` and `review` together with the improvement cycle and the commission, the lint of money for reviews (BUD-13), the slot `seat` and the translator catalog in request checking (GW-1, GW-7); the line `manager` in tenants together with the farm manager seat, absent for a tenant with `farm_manager.enabled: false` (D-77); the wake-up counter in the reserve (BUD-1, MSG-11). Campaigns with a card and script runs from the monthly limit up to the amount on the card, the pause `estimate_exceeded` (BUD-12). The enqueue estimate from the type's actual price (BUD-14). Reconciliation by code of the imported provider report, turning `spend_estimated` into actual figures, with a release by time (BUD-10). A hold on hourly spending above `hour_eur` (HLD-7). The actual usage per line in `farmctl doctor` (BUD-15). A provider rate limit as a wait (GW-10). |
| v0.2 | The limit of tasks in progress without refusal (BUD-6), the lowest priority class for analysis and reviews (BUD-8). The line `governance`. The slot `granted` with a ceiling on the grant amount in euros and the line `grant`, takeover and the line `takeover`. The `farm_manager` lens in the formula of money for reviews (BUD-13). De-identification of the principal in usage lines after 93 days (BUD-16). |

## Why it works this way

- P4 is not a pass-through proxy. A pass-through request would give the model the provider's server tools bypassing P3 (page fetching on the provider side, connecting remote MCP servers), usage reported by the runtime is incomplete because of compaction iterations, and a shared key would mix the tenants' caches and bills.
- Reserving the worst case before each call makes overspending zero for any estimate. The workspace ceiling at the provider stays a safeguard in case the farm itself makes a mistake, and the farm limit is kept below it, so that a refusal comes from the farm with a reset time, not from the provider until the end of the month.
- Phantom usage `spend_estimated` is reconciled with the provider's report: without this a network failure would "spend" a tenant's month on money the provider did not take.
- Declarations choose the model, because a silent model change breaks eval, the budget and data processing at the same time.
- Budget lines with a default limit of 0: analysis and reviews are enabled explicitly with money and do not eat into the main work, and the `line` field in the usage line separates the usage history from the first day.
- One retry layer with a pause ceiling and jitter, and counting only runs with a response: provider unavailability does not kill tasks and does not turn into an avalanche of retries after recovery.
- Separate ceilings and rate limits for each workspace: the organization's limits at the provider are shared, and without this one tenant or eval run would leave everyone without a model; a rate limit is then the wait of one tenant.

## What is not decided

- The payment model: who pays whom for a tenant's work (D-12). By default the farm counts usage per tenant and issues no invoices.
- The provider's batch mode for non-urgent tasks without a dialog is not part of P4. By default all calls go in normal mode.
- What to do if, after a restore from backup, the provider's report for the needed period is not ready yet. By default the core waits for the report and does not become ready.

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
