# botaru

botaru is the skeleton of a self-hosted farm of AI bots for work tasks. It describes the concepts, lifecycles, port contracts (the boundaries of the farm) and the conformance rules that every implementation must keep. Bots run on the farm for several tenants, people talk to them through communication channels, and external programs submit tasks with their own key. Decisions about permissions, money, addresses and records are made by the farm core; the model only proposes.

Everything in the repository is written in English. Field names, codes and file names are English too, and every message of the `ci/` checks starts with a stable code in square brackets.

## What is here

Four layers:

1. **Model**: concepts, relations, lifecycles and the rule registry (`docs/`, `docs/model/`, `spec/lifecycles/`, `conformance/requirements.yaml`).
2. **Ports**: the contracts at the boundaries with foreign code (P1, E1, P3 with E3, P4, E5) and the descriptions of how the core is built (`spec/ports/`).
3. **Contracts and checks**, the machine layer: JSON Schema for data and manifests, closed registries, assembled lifecycles, port bindings, fixtures, conformance suites, eval cases and the `ci/` checks (`spec/`, `conformance/`, `evals/`, `ci/`).

4. **Implementation**: the reference implementation in Go (`impl/`, decisions D-91 to D-93 in `docs/decisions.md`). It claims skeleton 0.3 (release 0.3.0) with the profiles `core`, `monitor`, `insights`, `governance`, `manager`, `messaging`, `campaign`, `knowledge` and `ingress`. The programs are the core `botaru-core`, the operator tool `farmctl`, the workers and adapters `botaru-agent`, `botaru-telegram`, `botaru-toolhost`, `botaru-zabbix`, `botaru-ingress` and `botaru-kbjob`, `botaru-plugin`, which serves the own plugins, and the eval runner `botaru-eval` with the mock provider `botaru-mockmodel`; the black-box conformance runner is the package `internal/conform`, run by `go test`. See [impl/README.md](impl/README.md) and, for a server, [impl/DEPLOY.md](impl/DEPLOY.md).

There are also examples on the training tenants `demo` and `demo2` (`examples/`).

## What is not here

Installation files (tenants, keys, machines, people) never go into this repository: an installation keeps its manifests in its own repository (D-92) and checks them from there with `ci/` (D-87). The current release is 0.3.0, the first published one; from its tag `v0.3.0` the compatibility promise of [spec/versions.md](spec/versions.md) holds, so a form changes only additively.

**An installation's own repository.** An organization that deploys the farm keeps a closed repository of its own beside this one and never puts tenant data into this one:

1. Create a new private repository for the installation (not a fork: a fork of a public repository is public too), and pin this repository in it at the tag `v0.3.0` as a git submodule or a full copy of its tree; the checks need `spec/`, `ci/`, `conformance/`, `examples/` and `evals/`, and the core is built from `impl/`. With a submodule or a copy, `farmctl apply` takes `-commit` and `farmctl apply -drop-unsupported` takes `-spec-root` ([impl/DEPLOY.md](impl/DEPLOY.md)).
2. Put the installation's manifests there: one folder per tenant like `examples/tenants/demo/` (tenant, groups, catalog, routes, schedules, sinks, channels), the farm charter like `examples/tenants/farm/`, its own roles, task types, tool plugins, actions and model profiles, and a deployment description by `spec/model/deploy-topology.schema.json` like `examples/deploy/farm.yaml`.
3. Answer the checklist "What every installation decides" in [docs/open-questions.md](docs/open-questions.md) in that repository.
4. Run the skeleton checks on the manifests from the pinned copy (`python3 -I ci/check_manifests.py --manifests <installation folder>` and the other commands of [ci/README.md](ci/README.md#your-own-files-outside-the-skeleton)). The deployment description is not linted by these checks; the core lints it at start through the setting `deploy_description` ([impl/DEPLOY.md](impl/DEPLOY.md)).
5. Build and run the core from `impl/` as [impl/DEPLOY.md](impl/DEPLOY.md) describes (in 0.3 it runs as a test bench, `test_bench: true`; a production farm needs the parts listed under "Not in this version" of [impl/README.md](impl/README.md)), with the settings, keys and secrets of the installation kept outside both repositories, and check each change of the manifests on the farm with `farmctl apply -dry-run` before applying it.
6. To move to a later release, move the pin to its tag and follow `CHANGELOG.md` and `spec/removed.yaml`.

No paid eval run with a real model has been made for the claim of the reference core; its eval rules are listed as not run in [impl/README.md](impl/README.md#eval-rules-not-run). What the reference core does not build yet is listed in [impl/README.md](impl/README.md#not-in-this-version).

The executable code is the reference implementation in `impl/` with its own checks (`.github/workflows/go.yml`) and the `ci/` checks of the skeleton that run on every pull request (`.github/workflows/skeleton.yml`). The `ci/` checks see the shape of files; the behaviour of a bot, MCP calls and delivery are checked by the conformance runner and the evals of an implementation.

## Where to start

If you want to understand the farm:

1. [Goals](docs/goals.md): what the farm is for, the critical incidents and the constraints; every rule serves one of them.
2. [Architecture](docs/architecture.md): the entry document: the farm on one screen, the layers, the core and its parts, the boundaries, tenants, the trust boundary, one message from start to finish, and where to read next.
3. [Glossary](docs/model/glossary.md): terms with a "do not confuse with" column.
4. [Task language](docs/task-language.md): roles, task types, actions and schedules.
5. [Decisions](docs/decisions.md) and [open questions](docs/open-questions.md).

If you want to add a plugin, a channel, a task type or a bot:

1. Read the recipe in [docs/extending.md](docs/extending.md) and study the samples: `examples/hello-bot/` (the smallest bot), `examples/tools/data/tool.yaml` and `examples/tasktypes/data.describe.v1.yaml` (a plugin and a task type).
2. Look up "what I add, where I put it, what I run" in [CONTRIBUTING.md](CONTRIBUTING.md#what-i-add-where-i-put-it-what-i-run).
3. Keep your files in your own installation repository and check them from there with `python3 -I ci/check_manifests.py --manifests <folder>` (`examples/` holds only the training samples of the skeleton).
4. Run the checks (below).

For a plugin, a channel or a simple bot you only need the `core` profile: tasks, permissions, tools, channels, money. The improvement cycle, the bot commission, bot identity and the farm manager are optional conformance profiles (`insights`, `governance`, `manager`) and can be skipped on first reading.

If you write an implementation, start with [spec/README.md](spec/README.md) (how schemas, registries, lifecycles and fixtures are laid out), [spec/versions.md](spec/versions.md) (what compatibility promises), [spec/ports/README.md](spec/ports/README.md) and [conformance/runner.md](conformance/runner.md) (the runner and the test bench).

## How to run the checks

You need Python 3.11 or newer (CI runs 3.11 and 3.13).

```sh
python3 -I -m pip install --require-hashes -r ci/requirements.txt
git fetch origin main              # the "additive only" check compares against the base branch
python3 -I ci/run_all.py origin/main  # all checks
```

What is checked today: manifest and wrapper schemas, closed parameters, group permissions, role profiles, eval cases against their schema, the rule registry and the files assembled from it, links and rule numbers in the text, "additive only" changes of schemas, and public safety (no secrets, real domains or addresses). What is not checked: bot behaviour, MCP calls, delivery. The table of all checks is in [ci/README.md](ci/README.md). Version readiness prints, for the rules of one skeleton version, three numbers: rules covered by data that the skeleton CI runs itself, rules covered by data for an implementation's runner or eval, and rules covered only by a check description. It also prints the `core` rules with `waivable: false` that are covered only by a description.

```sh
python3 -I ci/readiness.py v0.1   # 377 rules on 2026-10-17: 21, 162 and 194; core waivable: false by a description only 112 of 170
python3 -I ci/readiness.py v0.3   # 30 rules on 2026-10-17: 0, 0 and 30; core waivable: false by a description only 1 of 1
```

A version's report counts only the rules of that version; an implementation claiming v0.3 keeps the rules of v0.1, v0.2 and v0.3, except those whose `until` is earlier (D-113). What the reference core passes on the conformance runner is in [impl/README.md](impl/README.md#rule-coverage).

## Layout

| Folder or file | What is in it |
|---|---|
| `docs/` | documents explaining the parts of the farm: architecture (the entry document), extending, task language, tasks and delivery, permissions and security, messaging, coordination and scripts, operations with monitoring and privacy, capabilities, models and money, improvements and the commission, bots, farm manager, quality |
| `docs/model/` | glossary and object relations |
| `docs/adr/` | architecture decision records in MADR 4.0.0 format and their template |
| `spec/README.md` | how the machine files are laid out: schemas, registries, lifecycles, fixtures |
| `spec/model/`, `spec/manifests/` | data and manifest schemas, closed registries |
| `spec/ports/` | the contract sheets of the boundaries with foreign code, the descriptions of the core's parts and background loops, the sheet template |
| `spec/lifecycles/` | object lifecycles, one file per object, with the assembled YAML |
| `spec/bindings/` | bindings of ports to protocols (HTTP, MCP, model providers, events, telemetry, monitoring, OS) |
| `conformance/requirements.yaml` | the rule registry: every rule is written here once |
| `conformance/rules.md`, `conformance/retired.md` | lists of active and retired rules, assembled from the registry |
| `conformance/fixtures/`, `conformance/suites/` | fixtures (data only) and descriptions of checks against a live implementation |
| `evals/` | eval case format, cases and thresholds for checking bots with a real model |
| `examples/` | worked examples on the training tenants `demo` and `demo2` |
| `ci/` | the skeleton checks for every pull request |
| `impl/` | the reference implementation in Go (D-92): the core, `farmctl`, the workers, the plugins, the eval and conformance runners; see `impl/README.md` |
| `deploy/` | a sample deployment topology; it is not runnable |
| `CONTRIBUTING.md` | how to make changes, what to run, contribution terms |
| `CLA.md` | the Contributor License Agreement signed before an external pull request (draft) |
| `CHANGELOG.md` | change log |

## Skeleton versions

| Version | In short |
|---|---|
| v0.1 | model, ports and their sheets, the core and the first bot, bot messaging, stop, the improvement cycle with the trial outcome of a change, retention and deletion, money in euro cents (call usage in micro-euros `eur_micros`, D-87), plugin placement, notices for people and the privacy block; then contracts without procedures: campaigns, scripts and the translator, the farm manager seat, bot identity and memory |
| v0.2 | commission and promotion procedures, model grants, metrics and escalation, tenant lifecycles, knowledge and skills, people's rights to their data |
| v0.3 | the commission answers for its outcomes: every review carries a forecast scored by code and each member has an accuracy (D-109); code finds links between incidents and rejected proposals and a person confirms them, and confirmed links hold a role automatically; the farm manager's restricted mode after a preventable incident, takeover, monthly line and incident analysis (D-110); the candidate link of an incident to a script run and the card marks of a run (D-112); resubmission of a rejected proposal and the farm guards of every trial; the incident `cause` (D-118) |
| v0.4 | the simplification (D-114): the goals and constraints of the farm in `docs/goals.md` with a `goal` on every rule, contracts with checks only at the boundaries, deferred rules (`conformance/deferred.md`), rules that end without a replacement, the commission without a vote (D-115), simpler money and reports; it adds no rules of its own yet |

A version says from which release the conformance suite checks a rule. What compatibility promises before 1.0.0 is written in [spec/versions.md](spec/versions.md).

## Contributing

External pull requests are accepted after the author signs the Contributor License Agreement [CLA.md](CLA.md); a bot (for example CLA Assistant) checks the signature with a comment on the pull request. See [CONTRIBUTING.md](CONTRIBUTING.md#contribution-terms).

## License

Copyright (C) 2026 HunterEwok

GNU Affero General Public License version 3, this version only (AGPL-3.0-only, decision D-83). The full text is in [LICENSE](LICENSE).
