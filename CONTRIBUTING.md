# How to contribute

The skeleton rests on the rule registry, the port sheets, the lifecycles and the machine files in `spec/` (JSON Schema files and closed registries). The documents in `docs/` explain them. Every change starts with what changes in the rule registry, a port, a lifecycle, a schema or a value registry, and only then in the explaining text.

There is no farm code here: the core, `farmctl` and the black-box runner are written by a separate implementation (D-13). In the skeleton you therefore describe an extension as files, and the `ci/` checks verify their shape and consistency. The behaviour of a bot, MCP calls and delivery are checked by the runner and the evals of an implementation.

## Contribution terms

The skeleton is released under AGPL-3.0-only (D-83). External pull requests are accepted after the author signs the Contributor License Agreement [CLA.md](CLA.md) (D-89): the author keeps the rights to the contribution and grants the project owner a perpetual, irrevocable license with the right to sublicense, plus a patent license. A bot (for example CLA Assistant) checks the signature: it comments on the pull request with a link, the author signs once with their GitHub account, and the pull request is merged only after the signature. The agreement text is still a draft to be reviewed by a lawyer.

## What I add, where I put it, what I run

| What I add | Where I put it | What to do next | What CI catches |
|---|---|---|---|
| plugin | in your own installation repository: `<folder>/tools/<plugin>/tool.yaml` (kind `ToolPlugin`), recipe [7.2](docs/extending.md); sample `examples/tools/data/tool.yaml` | add the tool to a role's `tools` and the resources to the tenant catalog; compare the `tools/list` output of your server with the manifest | `python3 -I ci/check_manifests.py --manifests <folder> --tools-list tools.json` |
| channel | in your own installation repository: a `Channel` manifest in `<folder>/tenants/<tenant>/channels.yaml`, a driver config schema against the meta-schema `spec/manifests/drivers/driver-config.schema.json` and a binding note from the template `spec/bindings/channels/README.md`, recipe [7.4](docs/extending.md) | attach the binding note to an issue "binding: <channel>"; a pull request with it to the skeleton is possible after signing [CLA.md](CLA.md) | `python3 -I ci/check_manifests.py --manifests <folder>`: a driver schema `*.schema.json` with `$id` `urn:botaru:schema:driver.<driver>:1` in the folder (or `--driver-schemas <schema folder>`) passes the meta-schema, and the channel's `spec.config` passes the driver schema; without a schema the note `driver_schema_missing` |
| task type | in your own installation repository: `<folder>/tasktypes/<type>.yaml` (kind `TaskType`, name ends with `.vN`), recipe [7.7](docs/extending.md); sample `examples/tasktypes/data.describe.v1.yaml` | add the type to a role's `claims` and `task:<type>` to a group | `python3 -I ci/check_manifests.py --manifests <folder>` |
| bot | in your own installation repository: manifests in `<folder>/manifests/` (the role `roles/<role>/role.yaml` of kind `Role`, `tasktypes/`, a group in `tenants/<tenant>/`), cases in `<folder>/evals/cases/<role>/<id>.yaml`, your own people, chats and test bench role in `<folder>/evals/seed.yaml`; recipe [7.1](docs/extending.md); sample `examples/hello-bot/` | a group with `task:<type>`; a command, route or schedule for the type; eval cases for the role (row "eval case") | `python3 -I ci/check_manifests.py --manifests <folder>/manifests`, `python3 -I ci/check_evals.py --cases <folder>/evals/cases --seed <folder>/evals/seed.yaml --manifests <folder>/manifests` |
| rule | a row in `conformance/requirements.yaml` (fields below, section [Rules](#rules)); an ADR only if the rule changes a port or a contract | `python3 ci/gen_rules.py`; mention the number in the prose of the document named in the `doc` field | `check_registry.py`, `gen_rules.py --check`, `check_text.py`, `readiness.py` |
| schema | `spec/model/<name>.schema.json` or `spec/manifests/<kind>.schema.json` following the rules of [spec/README.md](spec/README.md) | fixtures in `conformance/fixtures/schemas/valid/` and `conformance/fixtures/schemas/invalid/` | `check_schemas.py`, `check_additive.py` |
| closed registry value | an entry in `entries` of the file `spec/model/<registry>.yaml`; raise `version` | nothing; a value is never removed or renamed | `check_schemas.py` |
| lifecycle | tables in `spec/lifecycles/<object>.md` | `python3 ci/gen_lifecycles.py` | `gen_lifecycles.py --check` |
| suite check | a row in a check table of `docs/quality.md` | `python3 ci/gen_suites.py` | `gen_suites.py --check`, `readiness.py` |
| manifest lint fixture | `conformance/fixtures/manifests/<name>.yaml`: `id`, `rules`, `description`, `cases[]` with `expect` (`ok` or a stable lint error code of `ci/check_manifests.py`; the codes are listed in [ci/README.md](ci/README.md)) | nothing | `check_manifests.py` runs the lint over it |
| behaviour fixture | `conformance/fixtures/<area>/<name>.yaml`: `id`, `rules`, `description`, data and `expect` | its name in the `fixtures` field of the rule | `readiness.py`; it is data for the implementation's runner |
| eval case | a skeleton case: `evals/cases/<role>/<id>.yaml` against the schema `evals/case.schema.json` (the field `expect.checks` is required); a new skeleton role goes into `roles` of `evals/fixtures/seed.yaml`; `from` and `chat` come from the same file; a row in the catalog of [evals/README.md](evals/README.md). Your own case lives in your own repository (row "bot"): your own `seed.yaml` extends the skeleton's test bench data, and the role comes from it or from the `Role` manifests | nothing | `check_evals.py`; your own: `check_evals.py --cases <folder> --seed <your seed.yaml>` |
| example manifest | `examples/`, training data only (section [Text](#text)) | nothing | `check_manifests.py`, `public_safety.py` |

`examples/` holds the training samples of the skeleton. Keep your own additions (plugin, channel, task type, bot) in your own installation repository and check them from there (`ci/README.md`, section "Your own files outside the skeleton").

Before a pull request:

```sh
python3 -I -m pip install --require-hashes -r ci/requirements.txt
git fetch origin main
python3 -I ci/run_all.py origin/main
```

Without `git fetch origin main` the "additive only" check does not find the base branch and compares nothing. Generated files (`conformance/rules.md`, `conformance/retired.md`, the "Rules of this section" sections, `spec/lifecycles/*.yaml`, `conformance/suites/`) are never edited by hand: you edit the source and then run the generator. What each check does is described in [ci/README.md](ci/README.md).

If after a change in `examples/` the fixtures `conformance/fixtures/manifests/*` fail with the message "expects ok", fix the first error in `examples/` first: fixtures with `with_examples: true` load the training manifests as background.

## Rules

- A rule is written once, in `conformance/requirements.yaml`. The files `conformance/rules.md`, `conformance/retired.md` and the "Rules of this section" sections in the documents are generated from the registry and are never edited by hand.
- Fields of a row: `id`, `title`, `status`, `text`, `check`, `waivable`, `version`, `profile`, `decisions`, `fixtures`, `doc`; the optional `level: SHOULD` marks a recommendation rule. A sample row is in [docs/quality.md](docs/quality.md), section "Rule registry".
- After changing the registry, run `python3 ci/gen_rules.py`: it regenerates `conformance/rules.md`, `conformance/retired.md` and the "Rules of this section" sections.
- The number of a new rule is mentioned in the prose of the document named in the `doc` field, not only in the generated table.
- A rule number is never reused. A retired rule stays as a row with `status: retired`, a date, a reason and the field `replaced_by` if another rule replaced it.
- A new rule gets the next free number of its family: the highest number the family ever had, plus one.
- A new rule family is added to the profile table of [docs/quality.md](docs/quality.md), section "Rule registry".
- A rule is self-contained: no references such as "as above" or "the rest unchanged". References to the numbers of other rules are allowed.
- A rule has a check method (`check`) and the skeleton version from which it is checked (`version`). An implementation cannot put a rule with `waivable: false` into its list of expected failures.
- The decision a rule rests on is given by its number from `docs/decisions.md`.

## Ports and protocols

- A port operation is first described in the port sheet in `spec/ports/`; only then can documents mention it as `P2 hold.set`.
- A decision that changes a port or a contract (an operation, a wire schema, a guarantee of a port sheet) is recorded as an architecture decision record (ADR) (D-45, D-86 (13)); an ordinary new rule is recorded as a registry row that refers to a decision D-NN. An ADR goes into `docs/adr/<NNNN>-<title>.md` in MADR 4.0.0 format; the template and the procedure are in [docs/adr/README.md](docs/adr/README.md), and when an ADR is needed is stated in [docs/architecture.md](docs/architecture.md), section "Decisions (ADR)".
- A thin port (C3, C4, E2) that has no second implementation by the release after v0.2 becomes a description rather than a mandatory contract (D-46, D-80).

## Text

- Everything in the repository is written in English, in full sentences: documents, README, `ci/README.md`, issues and pull requests (D-85). A term is explained where it is first used.
- Every sentence describes how the skeleton works now. The history of changes lives in `CHANGELOG.md` and in the retired rows of the registry.
- Examples use only training data: the tenant `demo` (the second one is `demo2`), the service tenant `farm`, the people `alice`, `bob`, `carol`, `dave`, `erin`, the bot `@demo_bot`, domains under `example.org`, addresses from 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, the hosts `web1`, `db1`, `ctd1`. The default time zone is `UTC`; where another zone is needed, the training zone `Europe/Berlin` is used (D-85).
- In core records a person is written as an opaque principal id `human:p_<ULID>`; the names `alice`, `bob` and the others appear only in manifests, examples and fixtures (D-85).
- The repository contains no secrets, no names of real people, organizations, domains or installations, and no hints at the country of an installation (D-13).
- The words "core", "external program", "worker", "bot", "functionary" and "tenant" are used in the meanings given in the glossary `docs/model/glossary.md`.
