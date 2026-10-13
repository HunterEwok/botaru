#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Additive only: schemas and closed registries change inside a version .vN without breaking (MAN-3).

Compares every *.schema.json schema and every closed registry
spec/model/*.yaml with their state at the branch point from the base branch
(git merge-base). A breaking change is an error:
- a schema or registry file was removed or renamed;
- a field was removed from properties, $defs, patternProperties;
- a field was added to required (and to dependentRequired);
- a type was narrowed (a type left the type list; integer is part of number) or
  type appeared where there was none;
- a value left enum, enum or const appeared, const changed;
- additionalProperties, items, unevaluatedProperties or unevaluatedItems
  became false where it was not; a subschema that was absent or true became
  a non-empty schema (true is compared as an empty schema);
- a limit became stricter: maxLength, maxItems, maxProperties, maximum,
  exclusiveMaximum decreased, minLength, minItems, minProperties,
  minimum, exclusiveMinimum increased or appeared;
- pattern, format, x-botaru-format, propertyNames, $ref, multipleOf changed
  or appeared;
- not, if, contains, minContains, maxContains, uniqueItems: true, anyOf or
  oneOf appeared; minContains increased, maxContains decreased; if changed;
  a not schema widened (it is compared with reversed polarity);
- allOf got a new item (a new constraint), anyOf or oneOf lost branches,
  oneOf got a branch that is not provably disjoint from every other branch
  (different const or enum values, disjoint types, or a property required in
  both with disjoint const or enum values), dependentSchemas got a new name;
- an entry (id) left a registry, or entries changed while the registry version did not.
A breaking schema change is made as a new file .v(N+1) next to .vN;
a retired registry value stays an entry marked retired.

  python3 ci/check_additive.py [base]   origin/main by default (or BASE_REF)

Without a base: in GitHub Actions and with an explicit argument this is an error;
in a local run without git it is a note. An all-zero base (the first push of a
branch) skips the comparison.
"""
import json
import os
import re
import subprocess
import sys

import yaml

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import ROOT, Loader, Report, no_dup_json, walk

rep = Report('additive only')
explicit = next((a for a in sys.argv[1:] if not a.startswith('-')), None)
base = explicit or os.environ.get('BASE_REF') or 'origin/main'
in_ci = bool(os.environ.get('GITHUB_ACTIONS'))


def git(*args):
    return subprocess.run(['git', '-C', ROOT, *args], capture_output=True, text=True)


if re.fullmatch(r'0{7,40}', base):
    rep.info(f'base {base}: first push of the branch, nothing to compare')
    rep.finish()
top = git('rev-parse', '--show-toplevel')
if top.returncode != 0 or git('rev-parse', '--verify', '--quiet', base + '^{commit}').returncode != 0:
    why = (top.stderr or '').strip().split('\n')[0] if top.returncode != 0 else f'there is no branch {base}'
    msg = (f'[base_missing] base {base!r} not found ({why}): a checkout with fetch-depth: 0 '
           f'or git fetch origin main is needed; comparison not done')
    if in_ci or explicit:
        rep.error(base, msg)
    else:
        rep.note(base, msg + ' (a note in a local run without git)')
        rep.info('comparison not done: no base (details: -v)')
    rep.finish()
mb = git('merge-base', base, 'HEAD')
base_commit = mb.stdout.strip() if mb.returncode == 0 and mb.stdout.strip() else base
prefix = os.path.relpath(ROOT, top.stdout.strip()).replace(os.sep, '/')
prefix = '' if prefix == '.' else prefix + '/'
old_paths = [p for p in git('ls-tree', '-r', '--name-only', '--full-tree', base_commit).stdout.split('\n')
             if p.startswith(prefix)]
old_files = [p for p in old_paths if p.endswith('.schema.json')]
old_regs = [p for p in old_paths if re.fullmatch(re.escape(prefix) + r'spec/model/[^/]+\.yaml', p)]
now_files = walk(('.schema.json',))
if not any(p.startswith(prefix + 'spec/') for p in old_paths):
    rep.info(f'base {base} ({base_commit[:12]}) has no spec/ yet: the skeleton is new, nothing to compare')
    rep.finish()
if not old_files and now_files:
    rep.error(base, f'[base_empty] {base} ({base_commit[:12]}) holds no schema under {prefix or "the root"}: '
              'comparison not done')

LT, GT = (lambda a, b: a < b), (lambda a, b: a > b)
TIGHTER = {'maxLength': LT, 'maxItems': LT, 'maxProperties': LT, 'maximum': LT, 'exclusiveMaximum': LT,
           'minLength': GT, 'minItems': GT, 'minProperties': GT, 'minimum': GT, 'exclusiveMinimum': GT,
           'minContains': GT, 'maxContains': LT}
CHANGED = ('pattern', 'const', 'format', 'x-botaru-format', 'propertyNames', '$ref', 'multipleOf', 'if')
# Keywords whose appearance narrows the schema
NEW_NARROWING = ('not', 'if', 'contains')  # minContains, maxContains, multipleOf are caught by TIGHTER and CHANGED


def types(s):
    t = s.get('type')
    ts = set(t) if isinstance(t, list) else ({t} if t else None)
    return ts | {'integer'} if ts and 'number' in ts else ts


def values(s):
    if not isinstance(s, dict):
        return None
    if 'const' in s:
        return [s['const']]
    return list(s['enum']) if isinstance(s.get('enum'), list) else None


def disjoint(a, b):
    """True only when no value can match both schemas; unknown is not disjoint."""
    if not isinstance(a, dict) or not isinstance(b, dict):
        return a is False or b is False
    va, vb = values(a), values(b)
    if va is not None and vb is not None and not any(v in vb for v in va):
        return True
    ta, tb = types(a), types(b)
    if ta is not None and tb is not None and not ta & tb:
        return True
    pa, pb = a.get('properties') or {}, b.get('properties') or {}
    both = set(a.get('required') or []) & set(b.get('required') or [])
    return any(isinstance(pa, dict) and isinstance(pb, dict) and k in pa and k in pb
               and values(pa[k]) is not None and values(pb[k]) is not None
               and not any(v in values(pb[k]) for v in values(pa[k])) for k in both)


def compare(old, new, where):
    # true accepts everything, like an empty schema
    old = {} if old is True else old
    new = {} if new is True else new
    if not isinstance(old, dict) or not isinstance(new, dict):
        if old is not False and new is False:
            rep.error(where, '[schema_narrowed] the schema became false: no values are left')
        return
    ot, nt = types(old), types(new)
    if ot is not None and nt is not None and not ot <= nt:
        rep.error(where, f'[type_narrowed] type narrowed: {sorted(ot)} -> {sorted(nt)}')
    if ot is None and nt is not None:
        rep.error(where, f'[type_added] type {sorted(nt)} appeared: the set of values is narrowed')
    if 'enum' in old and 'enum' in new:
        gone = [v for v in old['enum'] if v not in new['enum']]
        if gone:
            rep.error(where, f'[enum_value_removed] values {gone} left enum')
    if 'enum' not in old and 'enum' in new:
        rep.error(where, '[enum_added] enum appeared: the set of values is narrowed')
    for k, tighter in TIGHTER.items():
        if k in new and (k not in old or tighter(new[k], old[k])):
            rep.error(where, f'[limit_tightened] {k} is stricter: {old.get(k)} -> {new[k]}')
    for k in CHANGED:
        if k in new and old.get(k) != new[k]:
            rep.error(where, f'[keyword_changed] {k} changed: {old.get(k)!r} -> {new[k]!r}')
    added_req = set(new.get('required', [])) - set(old.get('required', []))
    if added_req:
        rep.error(where, f'[required_added] new required fields {sorted(added_req)}')
    for prop, deps in (new.get('dependentRequired') or {}).items():
        more = set(deps) - set((old.get('dependentRequired') or {}).get(prop, []))
        if more:
            rep.error(where, f'[required_added] dependentRequired.{prop}: new {sorted(more)}')
    for k in ('additionalProperties', 'items', 'unevaluatedProperties', 'unevaluatedItems'):
        if old.get(k, True) is not False and new.get(k, True) is False:
            rep.error(where, f'[closed_now] {k} became false')
    if len(new.get('prefixItems', [])) > len(old.get('prefixItems', [])) and 'prefixItems' in old:
        rep.error(where, '[prefix_items_added] new items in prefixItems')
    for key in ('properties', '$defs', 'patternProperties', 'dependentSchemas'):
        op, np = old.get(key, {}), new.get(key, {})
        if not isinstance(op, dict) or not isinstance(np, dict):
            continue
        for name, sub in op.items():
            if name not in np:
                rep.error(where, f'[field_removed] {key}.{name} removed')
            else:
                compare(sub, np[name], f'{where}/{key}/{name}')
    # not is compared with reversed polarity: the new negated schema must not accept more than the old one
    if 'not' in old and 'not' in new:
        compare(new['not'], old['not'], f'{where}/not(reversed)')
    for key in ('items', 'additionalProperties', 'then', 'else', 'contains', 'propertyNames',
                'unevaluatedProperties', 'unevaluatedItems'):
        if isinstance(old.get(key), dict) and isinstance(new.get(key), (dict, bool)):
            compare(old[key], new[key], f'{where}/{key}')
        elif old.get(key, True) is True and isinstance(new.get(key), dict) and new[key]:
            # an absent subschema (or true) accepts everything: any non-empty schema in its place narrows
            rep.error(where, f'[keyword_added] {key} became a schema: the set of values is narrowed')
    for k in NEW_NARROWING:
        if k in new and k not in old:
            rep.error(where, f'[keyword_added] {k} appeared: the set of values is narrowed')
    if new.get('uniqueItems') is True and old.get('uniqueItems') is not True:
        rep.error(where, '[keyword_added] uniqueItems: true appeared: the set of values is narrowed')
    if isinstance(new.get('allOf'), list) and len(new['allOf']) > len(old.get('allOf') or []):
        rep.error(where, f'[keyword_added] new items in allOf ({len(old.get("allOf") or [])} -> {len(new["allOf"])}): '
                  'every new item is a new constraint')
    for key in ('anyOf', 'oneOf'):
        if isinstance(old.get(key), list) and len(new.get(key) or []) < len(old[key]):
            rep.error(where, f'[keyword_added] fewer branches in {key} ({len(old[key])} -> {len(new.get(key) or [])})')
        if key not in old and key in new:
            rep.error(where, f'[keyword_added] {key} appeared: the set of values is narrowed')
    ob, nb = old.get('oneOf'), new.get('oneOf')
    if isinstance(ob, list) and isinstance(nb, list):
        for i in range(len(ob), len(nb)):
            if not all(disjoint(nb[i], b) for j, b in enumerate(nb) if j != i):
                rep.error(where, f'[oneof_overlap] new oneOf branch {i} is not provably disjoint from the others: '
                          'values matching two branches would stop passing')
    od, nd = old.get('dependentSchemas') or {}, new.get('dependentSchemas') or {}
    if isinstance(od, dict) and isinstance(nd, dict):
        for name in sorted(set(nd) - set(od)):
            rep.error(where, f'[keyword_added] new name dependentSchemas.{name}: a new conditional constraint')
    for key in ('allOf', 'anyOf', 'oneOf', 'prefixItems'):
        for i, (a, b) in enumerate(zip(old.get(key, []), new.get(key, []))):
            compare(a, b, f'{where}/{key}/{i}')


def show(p):
    out = git('show', f'{base_commit}:{p}')
    return out.stdout if out.returncode == 0 else None


for p in old_files:
    rp = p[len(prefix):]
    full = os.path.join(ROOT, rp)
    if not os.path.exists(full):
        rep.error(rp, '[schema_removed] the schema was removed or renamed: a breaking change is made as a new file .v(N+1)')
        continue
    try:
        old = json.loads(show(p) or 'null')
        new = json.load(open(full, encoding='utf-8'), object_pairs_hook=no_dup_json)
    except ValueError as e:
        rep.error(rp, f'[json_invalid] the schema cannot be read: {e}')
        continue
    compare(old, new, f'{rp}#')

for p in old_regs:
    rp = p[len(prefix):]
    full = os.path.join(ROOT, rp)
    if not os.path.exists(full):
        rep.error(rp, '[registry_removed] the closed registry was removed or renamed')
        continue
    try:
        old = yaml.load(show(p) or '', Loader=Loader) or {}
        new = yaml.load(open(full, encoding='utf-8'), Loader=Loader) or {}
    except yaml.YAMLError as e:
        rep.error(rp, f'[yaml_invalid] the registry cannot be read: {" ".join(str(e).split())[:200]}')
        continue
    if not isinstance(old, dict) or not isinstance(new, dict):
        continue
    oe = {e.get('id'): e for e in old.get('entries') or [] if isinstance(e, dict)}
    ne = {e.get('id'): e for e in new.get('entries') or [] if isinstance(e, dict)}
    gone = [i for i in oe if i not in ne]
    if gone:
        rep.error(rp, f'[registry_value_removed] values {gone} left the registry: a retired value stays '
                  'an entry marked retired')
    if oe != ne and old.get('version') == new.get('version'):
        rep.error(rp, f'[registry_version_same] registry entries changed, but version stayed {new.get("version")!r}')

rep.info(f'base {base} ({base_commit[:12]}): compared schemas {len(old_files)}, registries {len(old_regs)}, '
         f'schemas now {len(now_files)}')
rep.finish()
