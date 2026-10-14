# Machine files of the skeleton

## In short

`spec/` holds the public API of the skeleton: port sheets and lifecycles in words, and next to them their machine forms. A machine form is a JSON Schema, a closed registry in YAML or a generated lifecycle YAML. Every fact is written in one place: the shape of data only in a schema, a closed list of values only in a registry, a rule only as a row of `conformance/requirements.yaml`. The `ci/` checks make sure the files agree with each other.

## What is where

| Folder or file | What is in it |
|---|---|
| `VERSION` | the skeleton version, for example `0.1.0` |
| `versions.md` | the version map: port sheets, the `/v1` path, the `.vN` suffixes, `apiVersion`; what compatibility promises before 1.0.0 |
| `hashing.md` | the canonical JSON form per RFC 8785 (JCS), SHA-256 hashes written as `sha256:<hex>`, the Ed25519 signature of the audit head |
| `model/*.schema.json` | data schemas: task bodies, core records, port requests, events |
| `model/*.yaml` | closed registries: reasons, formats, capabilities, retention classes and other lists |
| `manifests/` | the manifest wrapper `manifest.schema.json` and the `spec` schema of every kind |
| `lifecycles/<object>.md` | a lifecycle in words; `ci/gen_lifecycles.py` generates `lifecycles/<object>.yaml` from its tables |
| `ports/` | port sheets, including the test bench control port `test-control.md` |
| `bindings/` | bindings of ports to protocols: `http/` (P1 and P2), `mcp/`, `model-providers/` (the v0.1 reference `anthropic.md`), `events/`, `telemetry/`, `monitor/`, `os/` |

## Schemas

- Every schema follows JSON Schema 2020-12: `"$schema": "https://json-schema.org/draft/2020-12/schema"`.
- The `$id` of a schema has the form `urn:botaru:schema:<name>:<major>`, for example `urn:botaru:schema:task-body:1`; the file is named `<name>.schema.json`, and major version 2 and later lives in the file `<name>.v2.schema.json` next to the previous one. `$ref` and `$schema` are resolved only from the local schema registry, and network references are rejected; a reference is written by `$id`: `urn:botaru:schema:common:1#/$defs/tenant_name`.
- Shared building blocks (tenant name, task type name, UTC instant, euro cents and micro-euros, rule number, the principal reference `principal_ref`, the hash `hash`) live in `model/common.schema.json` and are used by reference, not copied.
- A reference to a principal in a core record is the opaque id `human:p_<ULID>` per the building block `principal_ref`, not a person's name; names such as `alice` appear only in manifests, examples and fixtures, and the runner maps them to ids through `evals/fixtures/seed.yaml` or the `given` of a fixture (D-85).
- Every `*_hash` field refers to the building block `common#/$defs/hash`: SHA-256 over JSON in the canonical RFC 8785 form, written as `sha256:<hex>`; how the hash of each kind is computed is stated in [hashing.md](hashing.md).
- An input schema, that is, the schema of anything the core accepts from outside (a task body, a port request, a manifest), is closed: `"additionalProperties": false` on every level, `maxItems` on arrays, and `maxLength`, `enum`, `const` or a format on strings. Regular expressions in the skeleton's schemas are set by the skeleton itself. Schemas that come from manifests (`params_schema` and `result_schema` of a task type, an action schema, tool arguments) have no `pattern` of their own: a string leaf there is an `enum`, `enum_from`, `const` or an `x-botaru-format` from the registry `model/formats.yaml` (INJ-1). An output schema (a core response, a record, an event) does not forbid extra fields: workers tolerate unknown fields (MAN-3).
- Skeleton extension fields:

| Field | Where | Meaning |
|---|---|---|
| `x-botaru-direction` | schema root | `input` (the core accepts it from outside), `output` (the core returns it) or `record` (a core record) |
| `x-botaru-format` | string leaf | a format name from `model/formats.yaml`; only the registry sets the regular expression of a format |
| `x-botaru-registry` | string leaf | a value from the closed registry `model/<name>.yaml`, for example `reasons` |
| `x-botaru-registry-kind` | string leaf next to `x-botaru-registry` | the value of the `kind` field of the registry entries the leaf takes values from, for example `scope` for `holds` |
| `x-botaru-closed-by` | open node of an input schema | what closes the node instead of the schema itself, for example `params_schema of the task type` or `args of the action`; without this mark an open input node fails `ci/check_schemas.py` |
| `x-botaru-retention` | root of the schema of a stored object | a retention class from `model/retention-classes.yaml` (RET-1) |
| `x-botaru-read-by` | result field | `code` if the field is read by core code or a route; such a field is closed only |
| `x-botaru-rules` | any level | numbers of the rules this part of the schema enforces |
| `x-botaru-version` | schema root or field | the skeleton version in which the schema or field appears, `v0.1` by default |

- Within a major version a schema changes only by adding optional fields. A breaking change is a new file `<name>.v2.schema.json` with `$id` `urn:botaru:schema:<name>:2` (MAN-3); the check `ci/check-additive` compares schemas with the main branch. What else compatibility promises (registry values, rule numbers, `apiVersion`) is collected in [versions.md](versions.md).

## Closed registries

A registry is YAML of this form:

```yaml
registry: reasons            # registry name, equal to the file name
version: 1                   # grows with every change of the registry
description: Reasons of refusals and outcomes in port responses
source: docs/tasks-and-delivery.md
entries:
  - id: fenced               # a value unique within the registry
    since: v0.1
    rules: [FEN-2]
```

The fields of an entry other than `id` are specific to each registry and are described in its `description` and in the `source` document. A value that is not in the registry fails the core's check and `farmctl apply`. A registry value is never removed or renamed within a major version of the skeleton; a retired value stays as an entry marked `retired`, and a changed registry gets a new `version`. `ci/check-additive` checks this.

## Lifecycles

A lifecycle is described by tables in `lifecycles/<object>.md`: "State | Meaning | Terminal" and "From | To | Trigger | Condition | Rules". The file `lifecycles/<object>.yaml` is generated from these tables by `python3 ci/gen_lifecycles.py` and is never edited by hand. Transition fixtures live in `conformance/fixtures/state-machine/<object>.yaml`.

## Fixtures and suites

Fixtures (`conformance/fixtures/`) are data with an expected outcome only, without code. A schema fixture has the fields `schema` (the schema path), `rules`, `description` and `data` or `cases`; the schema accepts `schemas/valid/` and rejects `schemas/invalid/`. Other fixtures are YAML with the fields `id` (if the fixture has a number, for example `fx-msg-01`), `description`, `rules`, data and `expect`.

The skeleton CI runs the `schemas/` and `manifests/` fixtures. The other fixtures are data for an implementation's runner: CI checks their shape, rule numbers and the uniqueness of `id` (`ci/check_fixtures.py`), and `ci/readiness.py` counts their names in the `fixtures` field of rules. How the runner reads fixtures and how it controls the test bench is written in [conformance/runner.md](../conformance/runner.md).

The shape of a behaviour fixture is set by the schema `conformance/fixtures/fixture.schema.json`. Ids of core objects in fixtures can be shortened to the form `<prefix>_<number>` (`i_311`, `t_501`); the runner expands a short form to an `object_id` in the same way throughout a case. The hyphen form (`i-311`) is not used.

Suites (`conformance/suites/<suite>/<area>.yaml`) are descriptions of checks against a live implementation: what is given, what is done and what is expected. they are edited by hand, and `python3 ci/gen_suites.py` checks them and fills their derived fields; the implementation writes the check code, and its runner runs it (D-85). Version readiness `ci/readiness.py` shows three numbers: rules covered by data that the skeleton CI runs (`conformance/fixtures/manifests/`, `conformance/fixtures/schemas/`); rules covered by data for an implementation's runner or eval; rules covered only by a check description (D-86 (11)).
