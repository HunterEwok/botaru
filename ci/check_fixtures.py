#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Behaviour fixtures: form, rule numbers, unique ids.

The schemas/ and manifests/ fixtures are executed by the skeleton CI (check_schemas.py,
check_manifests.py). The other fixtures of conformance/fixtures/ are data for
the implementation runner; this check covers only what the file shows:
- the YAML is readable, the file holds one document that is a mapping;
- the fixture has description and rules (on the file or on every case);
- the numbers in rules of the fixture and of its cases (cases, checks, fixtures) are active;
- the ids of the fixture and of its cases are unique across conformance/ and evals/
  (schema fixture bodies are data, their ids are not checked).
"""
import collections
import os
import sys

import yaml

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import ROOT, Report, load_yaml_all, referable, rel, walk

rep = Report('fixtures')
FX = os.path.join(ROOT, 'conformance', 'fixtures')
SCHEMA_FX = os.path.join(FX, 'schemas') + os.sep
active = set(referable())
seen = collections.defaultdict(list)  # id -> [where]
ITEM_KEYS = ('cases', 'checks', 'fixtures')


def check_rules(where, value):
    if value is None:
        return
    if not isinstance(value, list) or not all(isinstance(x, str) for x in value):
        rep.error(where, '[fixture_rules_form] rules is a list of rule numbers')
        return
    for rid in value:
        if rid not in active:
            rep.error(where, f'[rule_not_active] rule {rid} is not active or does not exist')


def docs_of(f):
    try:
        return load_yaml_all(f)
    except yaml.YAMLError as e:
        rep.error(rel(f), f'[yaml_invalid] the YAML cannot be read: {" ".join(str(e).split())[:200]}')
        return None


n_files = 0
for f in walk(('.yaml', '.yml'), FX):
    rp = rel(f)
    docs = docs_of(f)
    if docs is None:
        continue
    n_files += 1
    if len(docs) != 1 or not isinstance(docs[0], dict):
        rep.error(rp, '[fixture_form] a fixture file holds one document that is a mapping')
        continue
    d = docs[0]
    if not str(d.get('description') or '').strip():
        rep.error(rp, '[fixture_description] the fixture has no description')
    items = [c for k in ITEM_KEYS for c in (d.get(k) if isinstance(d.get(k), list) else []) if isinstance(c, dict)]
    if not d.get('rules') and not (items and all(c.get('rules') for c in items)):
        rep.error(rp, '[fixture_rules_missing] the fixture has no rules (on the file or on every case): which rule does it check')
    check_rules(rp, d.get('rules'))
    if isinstance(d.get('id'), str):
        seen[d['id']].append(rp)
    if f.startswith(SCHEMA_FX):
        continue
    for key in ITEM_KEYS:
        items = d.get(key)
        if not isinstance(items, list):
            continue
        for i, c in enumerate(items):
            if not isinstance(c, dict):
                continue
            check_rules(f'{rp}#{key}/{i}', c.get('rules'))
            if isinstance(c.get('id'), str):
                seen[c['id']].append(f'{rp}#{key}/{i}')

# ids of suite checks and eval cases share one namespace with fixtures
for root in (os.path.join(ROOT, 'conformance', 'suites'), os.path.join(ROOT, 'evals', 'cases')):
    for f in walk(('.yaml', '.yml'), root):
        docs = docs_of(f)
        for d in docs or []:
            if not isinstance(d, dict):
                continue
            if isinstance(d.get('id'), str):
                seen[d['id']].append(rel(f))
            for i, c in enumerate(d.get('checks') or []):
                if isinstance(c, dict) and isinstance(c.get('id'), str):
                    seen[c['id']].append(f'{rel(f)}#checks/{i}')

for cid, where in sorted(seen.items()):
    if len(where) > 1:
        rep.error(where[1], f'[check_id_duplicate] id {cid} already exists in {where[0]}')
rep.info(f'fixture files {n_files}, check ids {len(seen)}')
rep.finish(limit=400)
