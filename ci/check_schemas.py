#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Schemas, closed registries and schema fixtures.

- every *.schema.json file in spec/ and evals/ is a valid JSON Schema 2020-12
  with a unique $id; every $ref of every schema resolves inside the repository
  (also inside properties and unused $defs);
- an input schema (x-botaru-direction: input) is closed: every object has
  additionalProperties: false (or unevaluatedProperties: false), every string
  has maxLength, enum or const, every array has maxItems, and there is no empty
  schema {}; an open place closed by a manifest schema is marked
  x-botaru-closed-by: <what closes it>;
- a closed registry spec/model/*.yaml has the form {registry, version, entries}
  with unique entry ids;
- the schema accepts the fixture conformance/fixtures/schemas/valid/** and
  rejects conformance/fixtures/schemas/invalid/**; a fixture has at least one
  body and active rule numbers; the optional expect_at (error path per body)
  is compared with the error paths of the schema.

A schema fixture is YAML: schema (path to the schema or its $id), rules (rule
numbers), description and data (one body) or cases (a list of bodies).
"""
import os
import re
import sys

import yaml

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import ROOT, Report, iter_nodes, load_json, load_yaml, referable, rel, schema_registry, walk

from jsonschema import Draft202012Validator, FormatChecker
from jsonschema.exceptions import SchemaError

rep = Report('schemas')
schemas, by_path = {}, {}
for f in walk(('.schema.json',)):
    rp = rel(f)
    try:
        doc = load_json(f)
    except ValueError as e:
        rep.error(rp, f'[json_invalid] the JSON cannot be read: {e}')
        continue
    if not isinstance(doc, dict):
        rep.error(rp, '[schema_not_object] the schema is not an object')
        continue
    if doc.get('$schema') != 'https://json-schema.org/draft/2020-12/schema':
        rep.error(rp, '[schema_dialect] $schema must be https://json-schema.org/draft/2020-12/schema')
    sid = doc.get('$id')
    if not sid:
        rep.error(rp, '[schema_id_missing] no $id')
        continue
    if sid in schemas:
        rep.error(rp, f'[schema_id_duplicate] $id {sid} is already used by {by_path[sid]}')
    try:
        Draft202012Validator.check_schema(doc)
    except SchemaError as e:
        rep.error(rp, f'[schema_invalid] invalid schema: {e.message}')
        continue
    schemas[sid], by_path[sid] = doc, rp

MODEL = os.path.join(ROOT, 'spec', 'model')
active_rules = set(referable())
COMPOSE = ('$ref', 'allOf', 'anyOf', 'oneOf', 'not', 'if', 'then', 'else', 'const', 'enum')
# Keys under which schemas live (not data such as enum, examples, default).
SUBSCHEMA_MAPS = ('properties', 'patternProperties', '$defs', 'definitions', 'dependentSchemas')
SUBSCHEMA_ONE = ('items', 'additionalProperties', 'unevaluatedProperties', 'unevaluatedItems', 'contains',
                 'propertyNames', 'not', 'if', 'then', 'else')
SUBSCHEMA_LIST = ('allOf', 'anyOf', 'oneOf', 'prefixItems')


def reg_ids(name):
    p = os.path.join(MODEL, f'{name}.yaml')
    if not os.path.exists(p):
        return None
    doc = load_yaml(p) or {}
    return {e.get('id') for e in doc.get('entries', []) if isinstance(e, dict)}


def subschemas(node, path='', skip_conditions=False):
    """Schema nodes (not data) with their JSON Pointer path."""
    if isinstance(node, bool):
        yield path, node
        return
    if not isinstance(node, dict):
        return
    yield path, node
    for k in SUBSCHEMA_MAPS:
        if isinstance(node.get(k), dict):
            for name, sub in node[k].items():
                yield from subschemas(sub, f'{path}/{k}/{name}', skip_conditions)
    for k in SUBSCHEMA_ONE:
        if k in ('not', 'if') and skip_conditions:
            continue  # a condition or a negation does not define the shape of the data
        if k in node and isinstance(node[k], (dict, bool)):
            yield from subschemas(node[k], f'{path}/{k}', skip_conditions)
    for k in SUBSCHEMA_LIST:
        if isinstance(node.get(k), list):
            for i, sub in enumerate(node[k]):
                yield from subschemas(sub, f'{path}/{k}/{i}', skip_conditions)


def types_of(n):
    t = n.get('type')
    return set(t) if isinstance(t, list) else ({t} if isinstance(t, str) else set())


def input_problems(n, parent_key):
    """What is open in a node of an input schema."""
    if n.get('x-botaru-closed-by'):
        return []
    out = []
    ts = types_of(n)
    composed = any(k in n for k in COMPOSE)
    if not n and parent_key not in ('not',):
        return ['an empty schema {} accepts anything']
    is_map = isinstance(n.get('additionalProperties'), dict) and n['additionalProperties'] \
        or isinstance(n.get('patternProperties'), dict) and n['patternProperties']
    if ('object' in ts or isinstance(n.get('properties'), dict)) and not composed \
            and n.get('additionalProperties') is not False and n.get('unevaluatedProperties') is not False \
            and not (is_map and 'maxProperties' in n):
        out.append('an object needs additionalProperties: false, a map with typed values needs maxProperties'
                   if is_map else 'an object needs additionalProperties: false')
    if 'string' in ts and not any(k in n for k in ('maxLength', 'enum', 'const', '$ref')):
        out.append('a string needs maxLength, enum or const')
    if 'array' in ts and 'maxItems' not in n and not any(k in n for k in ('const', 'enum', '$ref')):
        out.append('an array needs maxItems')
    return out


formats, retention = reg_ids('formats'), reg_ids('retention-classes')
for sid, doc in schemas.items():
    rp = by_path[sid]
    m = re.fullmatch(r'urn:botaru:schema:([a-z0-9][a-z0-9.-]*):([1-9][0-9]*)', sid)
    if not m:
        rep.error(rp, f'[schema_id_form] $id {sid} is not of the form urn:botaru:schema:<name>:<major>')
    elif rp.startswith('spec/'):
        want = f'{m.group(1)}.schema.json' if m.group(2) == '1' else f'{m.group(1)}.v{m.group(2)}.schema.json'
        if os.path.basename(rp) != want:
            rep.error(rp, f'[schema_file_name] the schema file with $id {sid} must be named {want}')
    direction = doc.get('x-botaru-direction')
    if direction not in (None, 'input', 'output', 'record'):
        rep.error(rp, f'[schema_direction] x-botaru-direction {direction!r} is not one of input, output, record')
    if direction == 'record' and 'x-botaru-retention' not in doc:
        rep.error(rp, '[schema_retention_missing] a stored object schema (x-botaru-direction: record) names no x-botaru-retention class (RET-1)')
    if 'x-botaru-retention' in doc and retention is not None and doc['x-botaru-retention'] not in retention:
        rep.error(rp, f'[schema_retention] retention class {doc["x-botaru-retention"]!r} is not in retention-classes.yaml')
    for path, node in iter_nodes(doc):
        where = f'{rp}#{path or "/"}'
        fmt = node.get('x-botaru-format')
        if fmt is not None:
            if formats is None:
                rep.error(where, '[format_registry_missing] x-botaru-format without the registry spec/model/formats.yaml')
            elif fmt not in formats:
                rep.error(where, f'[format_unknown] format {fmt!r} is not in formats.yaml')
        reg = node.get('x-botaru-registry')
        if reg is not None and not os.path.exists(os.path.join(MODEL, f'{reg}.yaml')):
            rep.error(where, f'[registry_unknown] registry {reg} is not found in spec/model')
        for rid in node.get('x-botaru-rules', []) or []:
            if rid not in active_rules:
                rep.error(where, f'[rule_not_active] rule {rid} in x-botaru-rules is not active')
    if direction == 'input':
        for path, node in subschemas(doc, skip_conditions=True):
            if path == '/$defs' or isinstance(node, bool):
                continue
            parent_key = path.rsplit('/', 2)[-2] if path.count('/') >= 2 else path.rsplit('/', 1)[-1]
            for why in input_problems(node, parent_key):
                rep.error(f'{rp}#{path or "/"}', f'[input_schema_open] input schema: {why} (or x-botaru-closed-by)')

registry = schema_registry()
path_to_id = {p: s for s, p in by_path.items()}


def validator(ref):
    sid = path_to_id.get(ref, ref)
    if sid not in schemas:
        return None
    return Draft202012Validator(schemas[sid], registry=registry, format_checker=FormatChecker())


# all $refs resolve: every $ref of every schema is looked up in the registry from the base of its schema
for sid, doc in schemas.items():
    resolver = registry.resolver(base_uri=sid)
    for path, node in iter_nodes(doc):
        ref = node.get('$ref')
        if isinstance(ref, str):
            try:
                resolver.lookup(ref)
            except Exception as e:  # noqa: BLE001 - referencing.exceptions.Unresolvable and PointerToNowhere
                rep.error(f'{by_path[sid]}#{path or "/"}', f'[ref_unresolved] reference {ref} does not resolve: {type(e).__name__}')

for f in walk(('.yaml', '.yml'), os.path.join(ROOT, 'spec', 'model')):
    rp = rel(f)
    try:
        doc = load_yaml(f)
    except yaml.YAMLError as e:
        rep.error(rp, f'[yaml_invalid] the YAML cannot be read: {" ".join(str(e).split())[:200]}')
        continue
    if not isinstance(doc, dict) or not {'registry', 'version', 'entries'} <= set(doc):
        rep.error(rp, '[registry_form] a closed registry must have the fields registry, version and entries')
        continue
    ids = [e.get('id') for e in doc['entries'] if isinstance(e, dict)]
    if len(ids) != len(doc['entries']) or None in ids:
        rep.error(rp, '[registry_entry_id] every registry entry must have an id')
    dup = {i for i in ids if ids.count(i) > 1}
    if dup:
        rep.error(rp, f'[registry_id_duplicate] duplicate ids: {sorted(dup)}')

base = os.path.join(ROOT, 'conformance', 'fixtures', 'schemas')
for kind in ('valid', 'invalid'):
    for f in walk(('.yaml', '.yml', '.json'), os.path.join(base, kind)):
        rp = rel(f)
        try:
            fx = load_json(f) if f.endswith('.json') else load_yaml(f)
        except (ValueError, yaml.YAMLError) as e:
            rep.error(rp, f'[fixture_unreadable] the fixture cannot be read: {" ".join(str(e).split())[:200]}')
            continue
        if not isinstance(fx, dict) or 'schema' not in fx or not ({'data', 'cases'} & set(fx)):
            rep.error(rp, '[fixture_form] a schema fixture must have schema and data or cases')
            continue
        for rid in fx.get('rules') or []:
            if rid not in active_rules:
                rep.error(rp, f'[rule_not_active] rule {rid} is not active')
        v = validator(fx['schema'])
        if v is None:
            rep.error(rp, f'[fixture_schema_unknown] schema {fx["schema"]} is not found')
            continue
        bodies = fx['cases'] if 'cases' in fx else [fx['data']]
        if not isinstance(bodies, list) or not bodies:
            rep.error(rp, '[fixture_empty] the fixture has no body')
            continue
        expect_at = fx.get('expect_at')
        if expect_at is not None and (not isinstance(expect_at, list) or len(expect_at) != len(bodies)):
            rep.error(rp, '[fixture_expect_at] expect_at is a list of paths, one per body')
            expect_at = None
        for i, body in enumerate(bodies):
            errs = list(v.iter_errors(body))
            if kind == 'valid' and errs:
                rep.error(f'{rp}[{i}]', f'[fixture_rejected] the schema rejected the body: {errs[0].message[:160]}')
            if kind == 'invalid' and not errs:
                rep.error(f'{rp}[{i}]', '[fixture_accepted] the schema accepted a body it must reject')
            if kind == 'invalid' and errs and expect_at is not None:
                want = str(expect_at[i]).strip('/')
                paths = {'/'.join(map(str, e.absolute_path)) for e in errs}
                paths |= {'/'.join(map(str, c.absolute_path)) for e in errs for c in (e.context or [])}
                if want not in paths:
                    rep.error(f'{rp}[{i}]', f'[fixture_wrong_place] the schema rejected the body elsewhere: expected {want or "/"}, '
                              f'errors at {sorted(p or "/" for p in paths)}')
rep.info(f'schemas {len(schemas)}')
rep.finish(limit=400)
