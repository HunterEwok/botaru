# Skeleton checks

This folder holds the only executable code of the skeleton: the checks that run on every PR and on every push to `main` (workflow `.github/workflows/skeleton.yml`). They need Python 3.11 or newer (CI runs 3.11 and 3.13) and the packages from `requirements.txt`, which pins every package, transitive ones included, together with the hashes of their files.

```sh
python3 -I -m pip install --require-hashes -r ci/requirements.txt
git fetch origin main              # base for the additive-only check
python3 -I ci/run_all.py           # all checks; the base defaults to origin/main
python3 -I ci/run_all.py -v        # the same with notes and the time of each step
```

Run `git fetch origin main` before a local run: without the base branch the additive-only check prints a note and compares nothing; in CI, or when a base is named explicitly, a missing base is an error. All checks run in one process and take 10-20 seconds (faster with PyYAML built against libyaml). The `-I` flag keeps the current folder and `PYTHON*` variables out of the checks.

| File | What it checks or does |
|---|---|
| `check_registry.py` | form of the rule registry: each number once; an active rule has a text, a check method, `waivable`, a version, a profile and a document; a retired rule has a date, a reason and a replacement; a deferred rule keeps its row and adds `deferred`, `reason`, `return_when` and `mechanism_removed`; a rule changed in a later version keeps its row with `until` (the last version it applies to) and `superseded_by` (active rules of a later version that replace it), or an empty `superseded_by` and `end_reason` when it ends without a replacement; unknown row fields are an error |
| `gen_rules.py` | builds `conformance/rules.md`, `conformance/retired.md`, `conformance/deferred.md` and the "Rules of this section" sections, and removes the section from a document without rules; with `--check` it only compares |
| `gen_lifecycles.py` | builds `spec/lifecycles/<object>.yaml` from the tables of `<object>.md`; with `--check` it compares and checks that transitions name declared states and that transition tables belong to machines by caption |
| `gen_suites.py` | checks the hand-edited suite files `conformance/suites/<suite>/<area>.yaml` (ids, file placement, active rules, existing fixtures), fills their derived fields `version` and `rules` and rebuilds the suite index in `docs/quality.md`; with `--check` it only compares |
| `gen_codes.py` | builds the lint code table below from `lint_codes.py`; with `--check` it compares |
| `gen_openapi.py` | builds the P1 OpenAPI 3.1 description `spec/bindings/http/p1.openapi.json` from the P1 rows of `spec/ports/operations.yaml` (MAN-12); with `--check` it compares and checks that every body reference exists |
| `check_text.py` | rule and decision numbers in text, Markdown links and their anchors, repository paths in backticks, port operations, no em dash in any text file, YAML and JSON readable without duplicate keys, every rule explained in the documents, no name of a specific product in a rule text (`[vendor_in_rule]`: Telegram, Zabbix, PostgreSQL, systemd, polkit, Anthropic; D-66) |
| `check_schemas.py` | JSON Schema 2020-12 schemas and their `$id`, every `$ref`, `x-botaru-*` fields, closed input schemas, closed registries, the `schemas/valid` and `schemas/invalid` fixtures |
| `check_fixtures.py` | every fixture in `conformance/fixtures/`: the YAML is readable, `description` and `rules` are present, rule numbers are active, check `id` values are unique across `conformance/` and `evals/` |
| `check_crossrefs.py` | cross-file checks: the operation map `spec/ports/operations.yaml` (form, schemas exist, an empty schema only with `note`, `contract: false` exactly on the rows of P2, C1-C4, E2, E4 and T (C10, D-114), `operator_only` only on P2 rows as `true` or a string (PRM-14), rules active, every operation of a port sheet is in the map, every operation schema is named in the map, the `op` field of the common schema, the binding path); the `expect.task.record` expectation against `task-record.schema.json`, in `examples/hello-bot/sequence.yaml` too; an `enum` with `x-botaru-registry` against the registry entries (and `x-botaru-registry-kind`); the alarms of `incident-classes.yaml` and `holds.yaml` in the event catalog; a key without a value in a YAML flow mapping (`[yaml_null_key]`); the behavior fixture envelope against `conformance/fixtures/fixture.schema.json`, and the operations, loops and reasons it names; `call` and `stand` step bodies against the request schema of their map row (`[fixture_body_invalid]`, lead decision 11; short ids, the `$last` and `$from:` references and a missing `nonce` in a `decide` body are allowed per `conformance/runner.md`, and a step that expects a schema refusal is not compared); the `profiles` field of a map row (for a row with rules it is the union of the profiles of its rules); the E1 delivery kinds are in `delivery-kinds.yaml` or `decision-points.yaml`; the code text catalog `spec/bindings/channels/messages/en.yaml` has every key that DLV-9 and the key columns of `docs/messaging.md` name (`[catalog_key_missing]`); every metric of `spec/bindings/telemetry/metrics.yaml` has an instrument, a unit and labels from `label_sets` (OBS-13); the note `[schema_orphan]` about a `spec/model/` schema nothing refers to |
| `check_manifests.py` | the `examples/` manifests as `farmctl apply` sees them, flows (P1 bodies and `activity` against the operation map schemas) and the lint fixtures in `conformance/fixtures/manifests/`; the tree budget of a type is not below the reserve of one model call (`tree_budget_below_call`); with `--tools-list` it compares a `tools/list` dump of your own MCP server with `tool.yaml`; with `--manifests` your own folder goes on top of the examples, with `--driver-schemas` your own driver schemas (section "Your own files outside the skeleton") |
| `check_evals.py` | the eval cases in `evals/cases/`, thresholds and stand data, and the catalog `evals/catalog.yaml` against the case files (same rules, version and blocking); with `--cases`, `--seed` and `--manifests` your own cases on top of the skeleton cases (section "Your own files outside the skeleton") |
| `check_additive.py`, `check-additive` | additive-only changes: schemas and closed registries against the merge base (MAN-3); narrowing is caught also through a new `allOf` item, fewer `anyOf` and `oneOf` branches, a new `dependentSchemas` name, a new `not`, `if`, `contains`, `uniqueItems: true` or `multipleOf`, a changed `if` or `not`, a new or changed `oneOf` branch (matched by content, not position) that is not provably disjoint from the others, `unevaluatedItems: false`, and any keyword other than an annotation on a schema that was absent, `true` or `{}`; before the first release a removal or an incompatible change recorded by the same change in `spec/removed.yaml` is a note `[change_recorded]` |
| `public_safety.py` | keys and tokens, IPv4 and IPv6 addresses, domains, mail, time zones and coordinates, bytecode and binary files, file names, forbidden words (the list comes from a secret, see below), tutorial tenants in every folder, and no Cyrillic character in any public file or path (`[cyrillic]`: the project is English only) |
| `readiness.py` | version readiness: every rule of the version has a fixture or a described check, as three numbers (section below); `--strict` fails the run, `--list` lists the gaps |
| `run_all.py` | runs everything; a missing check script is an error |
| `common.py`, `lint_codes.py` | shared functions and the lint code dictionary |

## Messages

Every message starts with a stable code in square brackets, then the place and the text: `error  [link_broken] docs/quality.md:12: broken link x.md`. Codes are never renamed; you can search by them and fixtures refer to them. In GitHub Actions an error becomes an annotation on the file line. Errors are always printed, notes with `-v` and in GitHub Actions. If `manifests/*` fixtures fail with "expects ok", fix the first error in `examples/` first: such cases use the examples as background and are skipped while the examples fail.

## Version readiness

`python3 ci/readiness.py v0.1` prints three numbers (lead decision 11), in total and per profile:

- rules covered by data the skeleton CI runs itself: the fixtures in `conformance/fixtures/manifests/` (linted by `check_manifests.py`) and `conformance/fixtures/schemas/` (`check_schemas.py`);
- rules covered by data for an implementation runner or an eval: the behavior fixtures in `conformance/fixtures/` and the `evals/` cases; the skeleton CI checks their form and call bodies, and the implementation runner or an eval with a model runs the behavior;
- rules covered only by a check description: a `conformance/suites/` entry (given, action, expectation; the implementation writes the check code), a `state-machine/` fixture (transitions in prose), a behavior fixture whose call body fails its request schema, or an eval case of a later version.

A separate line gives the number of `core` rules with `waivable: false` that are covered only by a description. The note `[fixture_not_citing]` means that a fixture from the `fixtures` field of a rule does not name that rule in its `rules`. A name `impl:<folder>/<TestName>` is a Go test of the reference implementation in `impl/<folder>/`; it counts as data for a runner, and the comment right above the test function names the rules it checks. Deferred rules are in no version and are not counted. While a deferred rule has `mechanism_removed: false`, fixtures, eval cases, suite checks and texts may still name it (`check_text.py` notes `[rule_deferred_kept]`); with `mechanism_removed: true` any mention outside the registry, `conformance/deferred.md`, `docs/decisions.md` and `CHANGELOG.md` is the error `[rule_deferred]`. A version is ready when `python3 ci/readiness.py v0.1 --strict` passes; `run_all.py` and CI run the readiness of every version from `VERSIONS` in `ci/common.py` with `--strict`, so a version rule without a check fails the PR. The black-box runner, which runs the suites against a live implementation, is not part of the skeleton: it is written together with the first implementation (`docs/quality.md`).

## Your own files outside the skeleton

An installation keeps its manifests, cases and fixtures in its own repository and checks them with the same scripts:

```sh
python3 -I ci/check_manifests.py --manifests ../my-install/manifests   # the examples/ manifests are the background
python3 -I ci/check_manifests.py --manifests ../my-plugin --tools-list tools.json   # server tools/list dump against tool.yaml
python3 -I ci/check_manifests.py --manifests ../my-channel --driver-schemas ../my-channel/drivers   # your own driver schema
python3 -I ci/check_evals.py --cases ../my-install/evals/cases --seed ../my-install/evals/seed.yaml --manifests ../my-install/manifests
python3 -I ci/readiness.py v0.1 --extra ../my-install/conformance       # implementation fixtures cover rules too
```

An object from your own folder with the same kind, tenant and name replaces the tutorial one. Errors of the tutorial examples are then not shown, except errors of the examples themselves.

- `--manifests` does not enter hidden folders (`.github/` and others) and skips documents without an `apiVersion` of the form `botaru/...` and without `steps` (eval cases, `docker-compose.yml`, `mkdocs.yml`); in a kind folder (`tools/`, `roles/`, `tasktypes/`, `tenants/`, `messagetypes/`, `model-profiles/`, `manifests/`) such a document gives the note `[not_a_manifest]`, and a document with a manifest `kind` but without `apiVersion` stays an error. So `--manifests` can also be given the root of the installation repository.
- Your own driver schema is a `*.schema.json` with the `$id` `urn:botaru:schema:driver.<driver>:<N>` in the `--manifests` folder, or any `*.schema.json` from `--driver-schemas <dir>`: it passes the meta-schema `spec/manifests/drivers/driver-config.schema.json`, and the `spec.config` of your own `Channel` or `Sink` with that driver passes it (`driver_config_invalid`). Your own channel with a driver that has neither a binding note in `spec/bindings/channels/` nor a schema gives the note `driver_schema_missing`.
- `check_evals.py --cases` adds your cases to the skeleton cases: `base_case` and `twin` may refer to skeleton cases, and ids do not repeat. `--seed <your seed.yaml>` extends `evals/fixtures/seed.yaml` (a person, chat or generator with the same name replaces the tutorial one, roles are added); the folder role also comes from the `Role` manifests of the `--manifests` folder. Coverage of all INJ-12 outputs is required of the skeleton cases only (lead decision 5, review4).

## What the checks expect from text and schemas

- A repository path in backticks (`docs/...`, `spec/...`, `conformance/...`, `examples/...`, `evals/...`, `ci/...`, `deploy/...`) exists. A path that does not exist yet is marked right after the backtick: `` `spec/x.md` (v0.2) `` or `` `spec/x.md` (planned with the implementation) ``. Patterns with `<...>`, `{...}` and `*` are not checked.
- In an input schema (`x-botaru-direction: input`) every object has `additionalProperties: false`, a map with a value type has `maxProperties`, a string has `maxLength`, `enum` or `const`, an array has `maxItems`, and there is no empty schema `{}`. A place that, per its description, is closed by a manifest schema is marked `"x-botaru-closed-by": "<what closes it>"`.
- A schema fixture in `schemas/invalid/` may name the expected error path for each body: `expect_at: [severity, items/0/kind]`. A lint fixture case may refine its expectation: `expect: {error: <code>, at: <path>, also: [<code>]}`; any other error, except the codes in `also`, fails the case.
- Public files use the `UTC` time zone, examples and fixtures use the tutorial zone `Europe/Berlin`; addresses come from 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24 and 2001:db8::/32; domains are under example.org; tenants are demo, demo2 and farm.
- No Cyrillic character appears in a public file or path (`[cyrillic]`). Locale codes such as `ru` stay as data.

## Parsed vocabulary

The checks and generators read some strings from content files and write some strings into generated files. These strings are fixed; anything else is free text for the checks.

| Where | Fixed strings |
|---|---|
| `spec/lifecycles/*.md` (read by `gen_lifecycles.py`) | state table header `\| State \| Meaning \| Terminal \|`; transition table header `\| From \| To \| Trigger \| Condition \| Rules \|`; a Terminal cell starts with `yes` or `no`; whole From or To cells `none` and `(none)`, the prefix `(none), ...`; `any`, `any open`, `except`; a cell starting with `same`; `(no transition)`, `(no record)`, `(deleted)`, and any other text in parentheses in a To cell also means no transition; `or` and `and` between state names; the state table and its transition tables carry the same caption |
| `conformance/suites/*/*.yaml` (read by `gen_suites.py`) | top fields `suite`, `area`, `checks`, `fixtures`; check fields `id`, `method`, `rules`, `version`, `group`, `note`, `spec`; fixture row fields `fixture`, `rules`, `group`, `spec`; the header comment `# Suite checks <suite>/<area>. This file is the source: edit it by hand.` |
| `docs/quality.md` (written by `gen_suites.py`) | the marks `<!-- generated: suites -->` and `<!-- /generated: suites -->` around the suite index |
| generated headers | `# Generated by ci/gen_lifecycles.py from the tables of {src}. Do not edit by hand.` |
| generated rule files (written by `gen_rules.py`) | `## Rules of this section`, `\| Rule \| About \| Since \| Non-waivable \|`, `# Skeleton rules`, `## Summary`, `Total`, `Explained in: `, `Checked by: `, the meta line `since ... · non-waivable \| waivable · profile ... · recommendation · check: ... · decisions: ...`, `# Retired rule numbers`, `\| Number \| Title \| Since \| Why \| Replaced by \|`, `none` |
| decisions | `D-NN` in any text and in the `decisions` field of `conformance/requirements.yaml`; a decision table row in `docs/decisions.md` starts with `\| D-NN \|` |
| `spec/ports/*.md` (read by `check_crossrefs.py`) | the heading `## Operations`; `Operation` as the first header cell of an operations table; the paragraph start `**Delivery kinds.**` in `spec/ports/e1-channel.md` and `Version` as the first header cell of its table |
| `spec/bindings/model-providers/<provider>.md` (read by `check_manifests.py`) | the section `## Region`; a region row whose name does not start with `eu` is outside the EU |
| path markers (`check_text.py`) | `` `path` (planned ...) `` and `` `path` (v0.2) `` |
| pattern lines (`public_safety.py`) | the line marker `# ps: pattern` |
| eval cases (`check_evals.py`) | INJ-12 output names `links in the answer`, `script draft`, `memory record proposals`; catalog fields `areas`, `area`, `cases`, `case`, `group`, `rules`, `decisions`, `version`, `block`, `method`, `spec`, and the `block` values `yes`, `no`, `threshold`, `without a model` |
| `evals/README.md` (read by `readiness.py`) | rows of the version table start with `\| vN.M \|`; case ids in backticks, ranges as `` `x-01`..`x-09` `` |

## Forbidden words

The list of forbidden words (the organization name, places, names) is not stored in the repository at all, not even as hashes: short stems can be brute-forced in seconds. `public_safety.py` reads it from the file named by the `BOTARU_FORBIDDEN_FILE` variable or from the text in the `BOTARU_FORBIDDEN` variable. Plain text, one entry per line: `p <stem>` matches any word that starts with the stem, `w <word>` matches only the whole word, case does not matter, `#` starts a comment. A word that is found is not printed in the output, only its place. There are two narrow exceptions, both stored as a hash, not as text: the start of the owner copyright line in `README.md` and `LICENSE`, and the owner name in `.github/CODEOWNERS` (only as `@<name>`), in `CLA.md` (the parties to the agreement, D-89) and in `impl/go.mod` and the Go files of `impl/` (only in the module path `github.com/<name>/botaru`, D-92).

The owner sets the list as follows:

- in GitHub: Settings > Secrets and variables > Actions > New repository secret, name `BOTARU_FORBIDDEN`, value the text of the list; the workflow passes it to the check step;
- locally: `BOTARU_FORBIDDEN_FILE=/path/outside/the/repository/forbidden-words.txt python3 -I ci/run_all.py`.

Without a list, a local run gives a note. In CI on push to `main` (and on `merge_group`, if a merge queue is ever enabled) it is the error `[forbidden_missing]`. A pull request never gets the secret, from a fork or from a branch of this repository, because its run executes PR code nobody has reviewed yet and that code could print the list; there the missing list is a note. A merge queue exists only for repositories of an organization, and this repository belongs to a personal account, so the owner checks the words of every PR locally before merging it. The check runs the checker from `main`, never the PR's own copy, because PR code given the list could print or send it; the PR tree is only read, through `BOTARU_ROOT`:

```sh
git fetch origin main pull/<n>/head:pr-<n>
git worktree add ../pr-<n> pr-<n>
# in a checkout of an up-to-date main
BOTARU_ROOT=../pr-<n> BOTARU_FORBIDDEN_FILE=/path/outside/the/repository/forbidden-words.txt python3 -I ci/public_safety.py
git worktree remove ../pr-<n>
```

Without that run, the words of a PR are checked only after the merge, that is, already in the public `main`.

`python3 -I ci/public_safety.py --history` (with the list) checks the forbidden words in every file of every commit reachable from any branch or tag, not only the current files. Every commit of a public repository is public: a word found in an old commit is not removed by a new commit, and the owner decides what to do about it.

## Gemini review

The workflow `.github/workflows/gemini-review.yml` is a second automatic reviewer, for when Codex has no quota left. A comment `/gemini review` on a pull request by the owner, a member or a collaborator runs the Gemini CLI (`@google/gemini-cli`, version pinned in the workflow) over the PR's diff in read-only plan mode and posts its findings as a PR comment. Only the default branch is checked out; the PR's changes are read as a diff from the API and never fetched or checked out. The comment always runs the workflow as it is on the default branch, so a change of the workflow takes effect only after its merge. The key is the repository secret `GEMINI_API_KEY` (Settings > Secrets and variables > Actions); a PR from a fork never gets it. The workflow is not a required check.

## Protecting the main branch

The repository belongs to a personal account, so GitHub offers no merge queue. The owner sets a ruleset for the `main` branch in Settings > Rules > Rulesets:

- restrict deletions and block force pushes;
- require linear history (only squash merges are allowed in the repository settings);
- require a pull request before merging, with 0 required approvals: the owner opens PRs under their own name and cannot approve them; `.github/CODEOWNERS` still marks the owner as the code owner of `LICENSE`, `SECURITY.md`, `spec/`, `conformance/requirements.yaml`, `conformance/rules.md`, `docs/decisions.md`, `ci/` and `.github/`;
- require the status checks `check (3.11)` and `check (3.13)` (workflow `.github/workflows/skeleton.yml`) and `go` (workflow `.github/workflows/go.yml`, the Go implementation in `impl/`), with branches up to date before merging;
- once the CLA bot has run on a PR, also require its check `license/cla`.

Before merging a PR the owner checks its forbidden words locally (section "Forbidden words").

## Owner settings in GitHub

The repository is public (D-13). These settings only the owner can make in GitHub:

1. Enable Private Vulnerability Reporting (Settings > Code security > Private vulnerability reporting): `SECURITY.md` refers to it.
2. Set the `BOTARU_FORBIDDEN` secret with the forbidden word list (section "Forbidden words").
3. Protect `main` with the ruleset (section "Protecting the main branch").
4. Connect a CLA bot (for example CLA Assistant, with the text of `CLA.md` in a public gist) to the repository and make its check `license/cla` required for `main`: an external PR is merged only after the contribution agreement is signed (D-89). Before the first external contribution, a lawyer reviews the text of `CLA.md`.
5. Review the whole commit history, not only the latest state, before the first release and after any suspected leak: secrets with a scanner over all commits (for example `gitleaks git --log-opts=--all`), forbidden words with `python3 -I ci/public_safety.py --history`. Every commit is public: a secret found in any commit is rotated, removing it from the history is not enough (`docs/quality.md`).

The code owner in `.github/CODEOWNERS` is already set; the owner name is allowed only by the narrow `public_safety.py` exception (section "Forbidden words").

## Generated files

Generated files are never edited by hand: edit the source (the registry, a lifecycle table, a check table, the code dictionary), then run `python3 ci/gen_rules.py`, `python3 ci/gen_lifecycles.py`, `python3 ci/gen_suites.py`, `python3 ci/gen_codes.py` or `python3 ci/gen_openapi.py`.

## Manifest lint codes

The codes of `check_manifests.py` that fixtures refer to (`expect: {error: <code>}`). A new code goes into `lint_codes.py`, then run `python3 ci/gen_codes.py`; a code the lint writes but the dictionary does not know fails `gen_codes.py --check`.

<!-- generated: lint-codes -->
| Code | Meaning | Rules |
|---|---|---|
| `action_type_grant` | grant, command, route or type named action.* | APR-5 |
| `aggregator_source_aggregator` | an aggregator names an aggregator role among its sources, directly or through a bot group | AGG-3 |
| `aggregator_sources_over_max` | an aggregator lists more source entries than max_sources | AGG-3 |
| `alarm_inbox` | type with alarm.* reads the inbox | INJ-7 |
| `alarm_not_job` | alarm.* in events of a type that is not a job | INJ-7 |
| `api_version` | apiVersion is not botaru/v1alpha1 | MAN-1 |
| `approvers_fewer_than_two` | production tenant: no group grants action.approve for a high risk action (note) | APR-16 |
| `args_from_mismatch` | the args_from action is executed by another tool | MAN-9 |
| `args_from_second_schema` | tool with args_from has its own params | MAN-9 |
| `assistant_in_default_types` | assistant type in default_types | DSP-10 |
| `backup_external_required` | no copies on a separate machine and no encrypted external backup target | RET-5 |
| `breaking_change` | breaking schema change inside .vN | MAN-3 |
| `builtin_command` | tenant command takes a built-in channel command | PRM-17 |
| `cache_per_principal` | cache_s on a tool with credential.per_principal | PRM-12, SEC-7 |
| `cap_data_class` | data class not accepted (unknown or reserved) |  |
| `cap_star` | wildcard in a capability for actions, data or decisions | PRM-7 |
| `cap_unknown` | capability is not in spec/model/capabilities.yaml |  |
| `cap_unknown_action` | capability for an action that is not in the set |  |
| `catalog_value_format` | catalog resource value fails its format | MAN-10 |
| `cmd_argv_path` | command program path is not absolute | INJ-14 |
| `cmd_forbidden_program` | command program is in spec/model/forbidden-programs.yaml | INJ-14 |
| `cmd_param_conflict` | a parameter name has different schemas in the tool and its commands | INJ-14 |
| `cmd_param_position` | argv parameter is not a whole element after -- or not from params | INJ-14 |
| `conforms_to_invalid` | conforms_to without an x.y.z version or the core profile | MAN-8 |
| `corpus_curated_source` | corpus with trust: curated and a source other than git | KNW-2 |
| `cron_invalid` | cron is not five fields or has CRON_TZ, TZ | TIM-2 |
| `delegated_group_privileged` | a group in delegated_groups carries privileged capabilities | PRM-16 |
| `driver_config_forbidden` | driver spec.config holds fields of the common wrapper | MAN-4 |
| `driver_config_invalid` | channel or sink spec.config does not match its driver schema | MAN-4 |
| `driver_schema_missing` | own channel with a driver that has neither a skeleton binding note nor a driver schema (note) | MAN-4 |
| `duplicate_object` | an object of the same kind and name is already declared | MAN-1 |
| `enum_from_unknown_list` | the enum_from list is in no catalog of the set (note) |  |
| `escalation_threshold_lowered` | a commission escalation threshold below its default (2, untrusted 3) | COM-13 |
| `executor_unknown` | action executor is missing or not a write tool | MAN-9 |
| `expected_failure_not_waivable` | expected failures list a rule with waivable: false | MAN-8 |
| `export_only_recipient` | export_only without exactly one of recipient and recipient_ref | SEC-10 |
| `export_sink_not_self` | privacy.export_sink is not an export_only sink with recipient_ref: self | SEC-10, LCY-2 |
| `family_unregistered` | model family is not in spec/model/model-families.yaml (note) |  |
| `farm_host_in_catalog` | farm host listed in a tenant catalog | DEP-11 |
| `flow_args_invalid` | P3 call or action request arguments fail the closed schema |  |
| `flow_body_invalid` | flow step body fails the P1 schema or the type params |  |
| `flow_denied_passes` | a call from the denied section passes the closed schema |  |
| `flow_ref_missing` | flow step type not found |  |
| `format_unknown` | x-botaru-format or resource format is not in spec/model/formats.yaml | INJ-1, MAN-10 |
| `global_tenant_data` | a global manifest holds a value that looks like tenant data | TEN-6 |
| `governance_ref` | a role, task type, action, message type or route refers to governance, attribution, seat or incident operations or events | GOV-6, GOV-13, GOV-29 |
| `guard_not_metric` | insights.extra_guards names a series without a complete outcome block | OBV-6 |
| `high_risk_one_approver` | production tenant lets a high risk action with approvals: 1 be requested | APR-16 |
| `host_two_tenants` | host or service in catalogs of two tenants | TEN-4 |
| `human_subject_in_record` | a result read by code names a human principal or the kind human | DLV-14 |
| `incident_class_ref` | an incident class names an alarm outside the events catalog, a condition atom or field outside the closed list, an unknown involvement function, operation, action or pinnable type, or services and monitoring scripts for a farm subject | MGR-7 |
| `incident_contain_auto` | a first measure of an incident class is not a hold with one tenant scope, known effects and an expiry | MGR-7, HLD-2 |
| `ingress_missing` | source channel (direction: in) without ingress | ING-1 |
| `ingress_token_tier` | route from an event of a tls_token source enqueues a type above tier 0 | ING-1 |
| `ingress_tolerance` | source with auth: signature without ingress.tolerance or with more than 5m | ING-1 |
| `insight_merge_window` | insight.merge_window shorter than an hour | INS-4 |
| `insight_type_invalid` | insight type of tier above 1, not claimed, with its own result_schema, of a class above insight.max_data, claimed outside the insight section, or not in exactly one role insight section | INS-1, INS-2 |
| `insight_wider` | insight tool outside the role tools | INS-2 |
| `lens_persona_missing` | a charter seat names a lens without a persona addendum in commission.lenses | COM-19 |
| `lens_profile_output` | a role that claims a lens review has a base profile with max_output_tokens above 2000 | COM-6 |
| `listener_admin_worker_network` | the P2 listener network is a worker network (note) | IDN-7 |
| `listener_test_in_production` | test-control listener of the core on a kind: production host | DEP-10 |
| `manager_line_missing` | manager budget line is not above 0 while the manager is enabled | MGR-20 |
| `manager_profile_missing` | manager enabled without the seat profile in the farm charter | MGR-20 |
| `manager_profile_not_allowed` | manager seat profile is not in models_allowed | MGR-20 |
| `max_share_without_group` | dispatch rule with max_share and without group | DSP-11 |
| `max_tokens_field` | manifest has a max_tokens field | BUD-9 |
| `mcp_annotation_conflict` | an MCP annotation in tools/list contradicts the manifest access | INJ-3 |
| `memory_person` | memory: person with disallowed types or tools | MEM-2 |
| `message_fields` | message type lacks a field its kind requires | MSG-2 |
| `message_name` | message type name does not start with its kind | MSG-2 |
| `model_exposure_agent_read` | a resource with model_exposure: none is read by a tool of an agent role | PRV-4 |
| `model_exposure_not_aggregate` | a resource with model_exposure: aggregates is read by an agent tool whose output is not only numbers and enum values | PRV-4 |
| `not_in_version` | field or value appears in a later skeleton version | APR-10, APR-16 |
| `observes_not_job` | observes on a type that is not a job | OBV-3 |
| `params_invalid` | route or schedule params fail the closed type schema |  |
| `persona_hash` | a persona text sent with apply does not match its SHA-256 | GOV-4 |
| `persona_link` | a persona or lens addendum holds a link, an address, an id or a secret name | GOV-4 |
| `persona_missing` | a charter pins a persona by hash and apply has no text of that hash | GOV-4 |
| `persona_size` | a persona or lens addendum is longer than 4 KiB | GOV-4 |
| `persona_value_path` | a functionary persona without the value-path paragraph, or another persona with one | GOV-4 |
| `price_old` | profile price older than 60 days (note) | GW-17 |
| `price_stale` | profile price older than 120 days (a note in examples) | GW-17 |
| `proposable_required_false` | type or schedule without the required proposable: false | PRP-10 |
| `proposal_scope` | proposal commit touches files outside the tenant folder | MAN-5, PRP-8 |
| `provenance_in_result` | a result schema has a provenance field | PRV-2 |
| `provider_human_review` | provider_human_review: yes against the subprocessor terms | GW-15 |
| `provider_retention` | provider retention contradicts the subprocessor retention | GW-15 |
| `provider_training` | model profile with provider_training: true | GW-15 |
| `purpose_unknown` | type purpose is not in spec/model/task-purposes.yaml |  |
| `rank_exceeded` | model profile rank above the slot or seat ceiling | GOV-3 |
| `recipients_monitoring` | privacy.recipients lacks monitoring while the installation claims the monitor profile or its profiles are unknown | MON-1 |
| `redact_required` | placement: host and untrusted_output: true without redact [ip, email, secret] | INJ-4 |
| `ref_missing` | reference to an object that is not in the set |  |
| `retention_period` | tenant retention period outside the class bounds or longer than the content period | RET-9 |
| `revert_reason_invalid` | Botaru-Revert reason is not from the closed list | MAN-5, PRP-22 |
| `reviewer_not_independent` | a commission seat fallback role runs on the analysis profile of a role that writes proposals | COM-2 |
| `role_profile_forbidden` | role declares what the profile forbids | PRF-1 |
| `role_profile_memory` | role memory is outside the profile | PRF-1 |
| `role_profile_purpose` | role claims a type whose purpose is outside the profile | PRF-1 |
| `role_profile_slot` | role model slot is outside the profile | PRF-1 |
| `role_profile_tier` | role claims a type above the profile tier | PRF-1 |
| `role_profile_unknown` | role profile is not in spec/model/role-profiles.yaml | PRF-1 |
| `role_write_tool` | role has a tool that is not read | APR-1, PRM-2 |
| `rollup_unknown` | observation series rollup or outcome.compare is not in spec/model/rollups.yaml | OBV-4 |
| `route_mode` | route mode without its section |  |
| `routine_fields` | risk: routine fields are incomplete or set on another risk | MAN-9, APR-10 |
| `run_arg_unknown` | run argument is not in the closed schema of the type or action | APR-17 |
| `run_needs_approval_none` | run starts a type without run.approval: none | APR-17 |
| `run_not_readonly` | type with run.approval: none is claimed by a role with actions or write tools | APR-17 |
| `sandbox_live_tools` | role that claims a tier 2 type has a tool with placement: host | SBX-1 |
| `sandbox_not_claimed` | tier 2 type or tool, or a sandbox plugin, while the sandbox profile is not claimed | MAN-11, SBX-1 |
| `sandbox_placement` | tier 2 tool outside a sandbox plugin, or a sandbox plugin tool that is not a tier 2 read tool with target: any | SBX-1 |
| `schedule_deadline` | schedule type has no timeouts.deadline |  |
| `schedule_script_await` | scheduled script has an await step | SCR-12 |
| `schedule_script_needs_run` | scheduled script has a step that needs script.run | SCR-12, SCR-6 |
| `schedule_script_over_ceiling` | scheduled script is above Tenant.spec.scripts | SCR-12, SCR-5 |
| `schedule_target` | schedule has neither or both of task and script | SCR-12, TIM-3 |
| `schema_invalid` | manifest does not pass the schema of its kind | MAN-1 |
| `script_param_invalid` | script parameter is free, undeclared, without a default where a schedule needs one, or a goal name clashes | SCR-14, SCR-12 |
| `script_wait_fields` | wait or await step with missing or foreign fields, or a wait above the run deadline | SCR-13 |
| `secret_rotation_missing` | a named secret of the deployment description has no rotation method, or overlap without a window | SEC-5 |
| `self_approve_production` | production tenant with self_approve: true | APR-9 |
| `series_class_below_source` | observation series data_class below the output_class of its source tool | OBV-2 |
| `series_name_reserved` | observation series named like a core series (a name of core-series.yaml or the farm. prefix) | OBV-6 |
| `series_source_invalid` | observation series source does not fit its kind (tool without tool or field, a tool on another kind, job without field) | OBV-2, OBV-7 |
| `service_no_card` | service no_card holds something other than restart | MON-8 |
| `service_script_unknown` | no script from monitor_scripts_allowed for a service verb | MON-8, MON-10 |
| `service_unit_missing` | via: host without unit, or unit with via: monitor | MON-8 |
| `shadow_zero_without_off` | a commission shadow share of 0 instead of an explicit shadow: off | COM-10 |
| `skill_format` | skill folder breaks the format: files, types, sizes, front matter or name | SKL-1 |
| `state_agent_producer` | state message kind has producers other than code | STA-1, MSG-2 |
| `status_in_git` | manifest has a status field: status is kept by the core | MAN-1 |
| `subprocessor_undeclared` | profile provider or subprocessor is not declared in privacy.subprocessors | GW-15 |
| `summary_fields_required` | a type in default_types for a group chat has no summary_fields | REP-11 |
| `system_decision_cap` | system-ops group holds a decision or control capability | PRM-10 |
| `system_ops_missing` | route or schedule type is not granted to system-ops | PRM-10 |
| `takeover_confirm_threshold` | takeover.confirm_above_eur above 0 would lift the takeover card | MGR-21 |
| `tenant_missing` | tenant-scoped kind without metadata.tenant | TEN-6 |
| `tenant_on_global` | global kind with metadata.tenant | TEN-6 |
| `timeout_ceiling` | type timeouts above the farm ceiling | DLV-16 |
| `timezone_unknown` | tenant or schedule timezone is not in the IANA database | TIM-3 |
| `tool_target_missing` | tool lacks exactly one of target_from and target: any | TEN-3a |
| `tool_target_unknown` | target_from is not a tool parameter with a catalog list | TEN-3a |
| `tools_list_mismatch` | the plugin server tools/list output does not match the tools of tool.yaml | INJ-2 |
| `transfer_basis_missing` | production tenant with a provider region outside the EU and no spec.model_transfer entry | GW-14 |
| `tree_budget_below_call` | tree budget of a model task type is below the reserve of one model call | GW-2, BUD-2, BUD-9 |
| `unaware_text` | a persona, type instruction or tool description links scores, reviews, proposals or votes with promotion, a seat, the queue or departure, or a persona speaks of voting or stops | GOV-29 |
| `unknown_kind` | manifest kind has no registered schema in spec/manifests/kinds.yaml | MAN-1 |
| `untrusted_output_false_text` | untrusted_output: false while the output has free text | INJ-3 |
| `veto_lens_no_executor` | a veto lens has no charter seat whose fallback role claims its review type | COM-7 |
| `watch_missing` | neither a monitoring system nor a kind: watch process with notify_ref | MON-2 |
| `watch_same_host` | the watch process runs only on the core machine (note) | MON-2 |
| `watch_with_monitoring` | both a monitoring system and a watch process (note) | MON-2 |
| `webhook_sink_invalid` | webhook sink without an https base outside the farm or without signing_key_ref | DLV-17 |
| `worker_kind_mismatch` | role and type worker_kind differ |  |
<!-- /generated: lint-codes -->
