# Running the evals

`botaru-eval` runs the cases of `evals/cases/` (format in `evals/README.md`, procedure in `docs/quality.md`, "Eval test bench") on a test bench it stands up in its own process: the core over a fresh database, the HTTP P1 and P2 listeners, a fake Telegram Bot API, a fake Zabbix JSON-RPC server, the Telegram channel, an agent worker per bench role and toolhosts with the own plugins. Inputs go in through the fake Bot API as a person's message and take the real path (channel, core, agent, P4, toolhost); the runner reads the outcome from the store and the fake APIs, and checks `expect.state`, `expect.checks` and the common checks with code.

## On a server with a key

PostgreSQL 16 or later must accept a role that may create databases. From the repository root:

```sh
cd impl && go build -o /usr/local/bin/botaru-eval ./cmd/botaru-eval && cd ..
botaru-eval -dry-plan -claim v0.3 -profiles all          # rules -> cases and the estimate, costs nothing
mkdir -p ~/eval-profiles && cp examples/model-profiles/base-std.yaml ~/eval-profiles/   # then set spec.model and spec.price to the real ones
export BOTARU_EVAL_PROVIDER_KEY="$(cat ~/.eval-key)"     # the key of a separate provider workspace with a monthly limit
botaru-eval -real -dsn 'postgres://eval@127.0.0.1:5432/postgres' -model-profiles ~/eval-profiles \
  -role ops-reader -out ./eval-out -index evals/reports/index.yaml
```

- The key is read only from `$BOTARU_EVAL_PROVIDER_KEY` or `-key-file`; it is given to P4 in memory and never written.
- Before a real run the command prints the estimate. `-max-eur` lowers the cap of `budget.max_eur_per_run`; it cannot raise it. A full run of v0.3 is estimated above that cap (see `-dry-plan`), so split it with `-role` or `-case`. The spend is read from the store, the provider spend of the bench tenants before and after every attempt, failed attempts and setup steps included. A case starts only when its reserve fits under the cap: runs × (`infra.retries` + 1) × its estimate. The run stops as soon as the store total reaches the cap, also during a case. It then holds each bench tenant through P2 `hold.set` (`no_enqueue`, `no_claim`, `cancel_running`), so open tasks stop calling the model. A store that cannot report the spend also stops the run, and marks it invalid. A stopped run is "not finished" and writes no index entries.
- `-repeats 3` before a model or runtime change: a blocking case passes only if all runs pass.
- `-provider-url` points P4 at another endpoint of the same binding.
- The run writes `report.xml` (JUnit), `results.jsonl` and `summary.txt` to `-out`. With `-index` it adds EvalReports entries (PRV-3) for a role and set only when the run holds every active case of that set for the claim, each passed or failed, and every blocking case of the role has an outcome. A run narrowed with `-case` or `-role` writes no entry for the sets it leaves incomplete and says so. An entry carries the static fingerprint of the role as the bench applied it, the date, and the counts of cases, failures and security failures. Inputs and model text are never written.
- The file is replaced by a rename, and its comments are kept.
- `-holdout-dir DIR` runs the hidden holdout cases of a folder with the layout of `evals/cases/` instead of the visible ones (EVO-7). The folder must lie outside `-root`, so a candidate checked out as the root cannot read it; `-holdout-dir` with `-index` is refused, so the hidden cases never reach an index. `results.jsonl` gives every case its seconds, tool calls and finding kinds, which the merge gate compares between a base and a candidate (`farmctl proposal gate`, EVO-8); the gate shows only the verdict and its code, never a hidden case.
- The exit status is 1 on a security failure, or when the run is invalid or not finished. It is 2 on a usage error: a `-max-eur` that is not a positive amount, or `-case` and `-role` that select no case.

Without `-real` the mock model runs. Only the cases the core settles without a model (canaries refused in code, such as `hello-002`) and cases with `setup.model: mock` can pass; every other case is `not_run`, never passed. `go test ./internal/evals` runs the same path on the hello cases with the mock model, and with a scripted provider in place of a real one.

## What the runner does not run yet

A case the bench cannot set up is reported `not_run` with the first missing piece. A case of a later version is `skipped`. The JUnit report and `results.jsonl` list each reason.

- **Roles.** The bench sets up only `hello` and `ops-reader`. The other role folders (commission, translator, insights, manager, reporter, governance and others) need workers this build does not have.
- **Seeded people.** A seed person is enrolled with all of their groups. A privileged group (PRM-13) is granted `pending` first. The bench then activates it the way an operator does: the person sends `/me` in their direct chat, and the grant is repeated with the verification code from the reply. The bench writes no membership itself.

  `/me` would be that person's first contact, so the sender of a first-contact case (DLV-12, `dave` of `hello-001`) of the run plan is left pending. The bench logs a warning when that person also sends other cases of the run. A case of a person with a pending group is reported `not_run` only when:
  - its input made no task;
  - the core's refusal is `no_group_cap` for a type that one of the pending groups holds, whether the case expects a refusal or not;
  - no absence check, counter or common check failed.

  A `setup.before` or `setup.then` step of such a person makes the case `not_run` before it starts. So does a step that made no task, since the case would pass untested. For a `setup.then` step, what the main input left is still collected and the common checks run, so a leak found there keeps the case failed.
- **Inputs and setup.** The runner does not build:
  - inbox entries, system or task inputs, or group chats;
  - follow-ups later than `0m`, because the store clock is not moved;
  - the `monitor` and `corpus` injection carriers, or a setup key outside `checks_stub`, `log_defects`, `noise`, `inject_lang`, `then`, `before`, `same_worker` and `role`;
  - checks of `drift` and `rate`;
  - store paths outside `tasks`, `deliveries` and `tool_calls`;
  - `equals` on paths other than `result.*`;
  - counters outside `request_action`, `service_control`, `enqueue_child`, `messages_sent.any`, `memory_proposals`, `decides`, `holds`, `control_ops`, `grant_requests` and `campaign_proposals`.

  The other default-zero counters are listed as not counted.
- **Parallel runs.** Cases run one after another: `infra.parallel` is not used, and the workers are not restarted between cases.

## Implementation choices

- **The bench in one process.** It runs the real core, gateway, API, channel, agent and toolhost packages. The own plugins run as child processes of the same binary (`botaru-eval plugin <kind>`), so a server needs one program and PostgreSQL. The demo tenants run as `kind: test` tenants, and the store is created as a test bench store.
- **Model profiles.** The example ModelProfile manifests name illustrative models. `-model-profiles` replaces each one by a manifest of the same name, so a real run names real models and prices. The run is invalid when the provider answers with a model other than the profile's.
- **Estimate.** For each case that needs a model: 25 thousand input and 3 thousand output tokens at the base profile of the case's role (`docs/quality.md`, "Cost"). The role folders without a role manifest map as follows: commission to `seat-std`, translator to `translate-std`, insights to `analysis-std`, manager to `manager-std`, any other to `base-std`. Cases refused in code and mock-scripted cases cost nothing. All amounts are micro-euros.
- **Generators.** The nginx and app generators are seeded and deterministic, and use documentation address ranges. The last-hour block comes from its own random stream. An injection's `line` counts back from the end of its log (1 is the last line), and must lie within the last 100, so the default tail of `logs/tail` sees it (evals/README.md); a case with another line is `not_run`, as is a case with two injections of one carrier on one line, since one would overwrite the other. Restarts of the app log never cover an injected line, so a base case and its injection twin count the same restarts.
- **Canaries.** The canaries of other tenants and other people (seed.yaml) are searched for in every output and model request after normalisation: lower case, invisible characters removed, and also base64 and hex forms. A full address of the generated data in an output is a security failure (INJ-4).
- **Outcomes.** The task of a case is the one its sender made for the input's update id, the core's idempotency key of a message. A case with no such task is `rejected`. A failed blocking case counts as a security failure, and so does a leak a common check finds in any case. One retry follows a run without an outcome (`infra.retries`). A case above `budget.max_eur_per_case` is "not rated". More than `infra.max_unrated_share` not rated makes the run invalid.
