# botaru

botaru is the skeleton of a self-hosted farm of AI bots for work tasks. It describes the concepts, lifecycles, port contracts (the boundaries of the farm) and the conformance rules that every implementation must keep. Bots run on the farm for several tenants, people talk to them through communication channels, and external programs submit tasks with their own key. Decisions about permissions, money, addresses and records are made by the farm core; the model only proposes.

Everything in the repository is written in English. Field names, codes and file names are English too, and every message of the `ci/` checks starts with a stable code in square brackets.

## What is here

Four layers; the fourth, the reference implementation in Go, is only started:

1. **Model**: concepts, relations, lifecycles and the rule registry (`docs/`, `docs/model/`, `spec/lifecycles/`, `conformance/requirements.yaml`).
2. **Ports**: what each operation on each of the thirteen boundaries means (`spec/ports/`).
3. **Contracts and checks**, the machine layer of v0.1: JSON Schema for data and manifests, closed registries, assembled lifecycles, port bindings, fixtures, conformance suites, eval cases and the `ci/` checks (`spec/`, `conformance/`, `evals/`, `ci/`).

4. **Implementation**: the reference implementation in Go (`impl/`, decisions D-91 to D-93 in `docs/decisions.md`). Today it is a module skeleton with its CI checks; the work on the core has not started.

There are also examples on the training tenants `demo` and `demo2` (`examples/`).

## What is not here

There is no working farm code yet. The core, the `farmctl` command line tool and the black-box conformance runner will be written in `impl/` (D-92). The implementation does not edit the skeleton files and passes the skeleton's conformance suite. You therefore cannot start a bot from this repository yet. What you can do today is describe an extension as files and check their shape with `ci/`. The behaviour of a bot, MCP calls and delivery are checked by the runner and the evals of an implementation.

The executable code today is the `ci/` checks that run on every pull request and the Go module skeleton in `impl/` with its own checks (`.github/workflows/go.yml`).

## Where to start

If you want to understand the farm:

1. [Goals](docs/goals.md): what the farm is for, the critical incidents and the constraints; every rule serves one of them.
2. [Overview](docs/overview.md): the farm on one screen, one message from start to end, the four layers, versions.
3. [Glossary](docs/model/glossary.md): terms with a "do not confuse with" column.
4. [Architecture](docs/architecture.md): the core, the thirteen ports, tenants, the trust boundary.
5. [Task language](docs/task-language.md): roles, task types, actions and schedules.
6. [Decisions](docs/decisions.md) and [open questions](docs/open-questions.md).

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

What is checked today: manifest and wrapper schemas, closed parameters, group permissions, role profiles, eval cases against their schema, the rule registry and the files assembled from it, links and rule numbers in the text, "additive only" changes of schemas, and public safety (no secrets, real domains or addresses). What is not checked: bot behaviour, MCP calls, delivery. The table of all checks is in [ci/README.md](ci/README.md). Version readiness (`python3 -I ci/readiness.py v0.1`) prints three numbers: rules covered by data that the skeleton CI runs itself, rules covered by data for an implementation's runner or eval, and rules covered only by a check description. For v0.1 on 2026-10-13 these are 12, 179 and 196 of 387; of the 171 `core` rules that an implementation may not waive, 110 are still covered only by a description, and that line is printed too.

## Layout

| Folder or file | What is in it |
|---|---|
| `docs/` | documents explaining the parts of the farm: tasks and delivery, permissions, security, privacy, messaging, coordination, operations, monitoring, models and money, improvements, scripts, bots, commission, farm manager, quality |
| `docs/model/` | glossary and object relations |
| `docs/adr/` | architecture decision records in MADR 4.0.0 format and their template |
| `spec/README.md` | how the machine files are laid out: schemas, registries, lifecycles, fixtures |
| `spec/model/`, `spec/manifests/` | data and manifest schemas, closed registries |
| `spec/ports/` | the thirteen port sheets, core background loops, the sheet template |
| `spec/lifecycles/` | object lifecycles, one file per object, with the assembled YAML |
| `spec/bindings/` | bindings of ports to protocols (HTTP, MCP, model providers, events, telemetry, monitoring, OS) |
| `conformance/requirements.yaml` | the rule registry: every rule is written here once |
| `conformance/rules.md`, `conformance/retired.md` | lists of active and retired rules, assembled from the registry |
| `conformance/fixtures/`, `conformance/suites/` | fixtures (data only) and descriptions of checks against a live implementation |
| `evals/` | eval case format, cases and thresholds for checking bots with a real model |
| `examples/` | worked examples on the training tenants `demo` and `demo2` |
| `ci/` | the skeleton checks for every pull request |
| `impl/` | the reference implementation in Go (D-92): today a module skeleton, see `impl/README.md` |
| `deploy/` | a sample deployment topology; it is not runnable |
| `CONTRIBUTING.md` | how to make changes, what to run, contribution terms |
| `CLA.md` | the Contributor License Agreement signed before an external pull request (draft) |
| `CHANGELOG.md` | change log |

## Skeleton versions

| Version | In short |
|---|---|
| v0.1 | model, ports and their sheets, the core and the first bot, bot messaging, stop, the improvement cycle with the trial outcome of a change, retention and deletion, money in euro cents, plugin placement, notices for people and the privacy block; then contracts without procedures: campaigns, scripts and the translator, the farm manager seat, bot identity and memory |
| v0.2 | commission and promotion procedures, model grants, metrics and escalation, tenant lifecycles, knowledge and skills, people's rights to their data |

A version says from which release the conformance suite checks a rule. What compatibility promises before 1.0.0 is written in [spec/versions.md](spec/versions.md).

## Contributing

External pull requests are accepted after the author signs the Contributor License Agreement [CLA.md](CLA.md); a bot (for example CLA Assistant) checks the signature with a comment on the pull request. See [CONTRIBUTING.md](CONTRIBUTING.md#contribution-terms).

## License

Copyright (C) 2026 HunterEwok

GNU Affero General Public License version 3, this version only (AGPL-3.0-only, decision D-83). The full text is in [LICENSE](LICENSE).
