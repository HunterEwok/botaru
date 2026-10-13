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
  became false where it was not; a schema that was absent, true or {} got any
  keyword other than an annotation (title, description, ..., x-botaru-*
  metadata except x-botaru-format);
- a limit became stricter: maxLength, maxItems, maxProperties, maximum,
  exclusiveMaximum decreased, minLength, minItems, minProperties,
  minimum, exclusiveMinimum increased or appeared;
- pattern, format, x-botaru-format, propertyNames, $ref, multipleOf changed
  or appeared;
- not, if, contains, minContains, maxContains, uniqueItems: true, anyOf or
  oneOf appeared; minContains increased, maxContains decreased; if or not
  changed (a changed negated schema is not compared, it is an error), or a
  definition that not or if reaches through $ref (in any file, by $id) changed,
  or one that a oneOf branch reaches changed and the branches (with $ref
  followed) were provably disjoint before but are not now;
- allOf got a new item (a new constraint), prefixItems got an item narrower
  than the old items (or unevaluatedItems) at its position (absent accepts
  anything, false accepted nothing) or lost an item wider than what now
  governs its position, anyOf or oneOf lost branches,
  oneOf got a new or changed branch (branches are matched by content without
  annotations, not by position) that is not provably disjoint from every
  other branch (different const or enum values, disjoint types, or a property
  required in both with disjoint const or enum values), dependentSchemas got
  a new name, patternProperties got a new pattern with an asserting schema
  (unless the object allowed no names before);
- values are compared as JSON compares them: true is not 1, 1 equals 1.0;
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
CHANGED = ('pattern', 'const', 'format', 'x-botaru-format', 'propertyNames', '$ref', 'multipleOf', 'if', 'not')
# Keywords in CHANGED whose value is a schema: an annotation edit inside them changes nothing
SCHEMA_VALUED = ('propertyNames', 'if', 'not')
# Subschemas whose change to false is reported as [closed_now]
CLOSABLE = ('additionalProperties', 'items', 'unevaluatedProperties', 'unevaluatedItems')
# Keywords whose appearance narrows the schema
NEW_NARROWING = ('contains',)  # not and if are caught by CHANGED  # minContains, maxContains, multipleOf are caught by TIGHTER and CHANGED


def types(s):
    t = s.get('type')
    ts = set(t) if isinstance(t, list) else ({t} if t else None)
    return ts | {'integer'} if ts and 'number' in ts else ts


def canon(v):
    """A JSON value as JSON Schema compares it: true is not 1, 1 equals 1.0, key order has no meaning."""
    if isinstance(v, bool) or v is None:
        return ('lit', v)
    if isinstance(v, (int, float)):
        return ('num', v)
    if isinstance(v, str):
        return ('str', v)
    if isinstance(v, list):
        return ('list', tuple(canon(x) for x in v))
    if isinstance(v, dict):
        return ('obj', tuple(sorted((k, canon(x)) for k, x in v.items())))
    return ('other', repr(v))


def same(a, b):
    return canon(a) == canon(b)


def among(v, vs):
    return any(same(v, x) for x in vs)


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
    if va is not None and vb is not None and not any(among(v, vb) for v in va):
        return True
    ta, tb = types(a), types(b)
    if ta is not None and tb is not None and not ta & tb:
        return True
    # required and properties constrain only objects: the discriminator proof needs both branches to be objects
    if ta != {'object'} or tb != {'object'}:
        return False
    pa, pb = a.get('properties') or {}, b.get('properties') or {}
    both = set(a.get('required') or []) & set(b.get('required') or [])
    return any(isinstance(pa, dict) and isinstance(pb, dict) and k in pa and k in pb
               and values(pa[k]) is not None and values(pb[k]) is not None
               and not any(among(v, values(pb[k])) for v in values(pa[k])) for k in both)


# Keywords that do not change which values pass
ANNOTATIONS = {'title', 'description', '$comment', 'examples', 'default', 'deprecated', 'readOnly', 'writeOnly',
               '$id', '$schema', '$anchor', 'contentEncoding', 'contentMediaType', 'contentSchema'}
# Keywords that assert nothing by themselves: annotations and definitions used only through $ref
NON_ASSERTING = ANNOTATIONS | {'$defs', 'definitions'}
# Annotations that set the base URI or dialect: a change of them can change what a $ref resolves to,
# so they stay when two schemas are compared for sameness
IDENTIFIERS = {'$id', '$schema', '$anchor'}


def annotation(k):
    # the x-botaru-* keys are metadata for the ci/ linters; x-botaru-format is a format and stays asserting
    return k in ANNOTATIONS or (k.startswith('x-botaru-') and k != 'x-botaru-format')


def asserting(s):
    return sorted(k for k in s if k not in NON_ASSERTING and not annotation(k))
# Keywords whose value maps names to subschemas, and keywords whose value is data, not a schema
NAME_MAPS = {'properties', 'patternProperties', '$defs', 'definitions', 'dependentSchemas'}
LITERALS = {'const', 'enum', 'required', 'dependentRequired'}


def bare(s, names=False):
    """The schema without descriptive annotations, for telling whether two schemas accept the same values."""
    if isinstance(s, list):
        return [bare(x) for x in s]
    if not isinstance(s, dict):
        return s
    if names:
        return {k: bare(v) for k, v in s.items()}
    return {k: (v if k in LITERALS else bare(v, k in NAME_MAPS)) for k, v in s.items() if not annotation(k) or k in IDENTIFIERS}


def unmatched(old_list, new_list):
    """Branches matched by content without annotations, with multiplicity:
    (old ones left over, (index, new) ones left over)."""
    left = list(old_list)
    fresh = []
    for i, b in enumerate(new_list):
        k = next((j for j, a in enumerate(left) if same(bare(a), bare(b))), None)
        if k is None:
            fresh.append((i, b))
        else:
            del left[k]
    return left, fresh


def compare(old, new, where):
    # true accepts everything, like an empty schema
    old = {} if old is True else old
    new = {} if new is True else new
    if not isinstance(old, dict) or not isinstance(new, dict):
        if old is not False and new is False:
            rep.error(where, '[schema_narrowed] the schema became false: no values are left')
        return
    if not asserting(old) and asserting(new):
        rep.error(where, f'[schema_narrowed] a schema that accepted every value got {asserting(new)}')
        return
    ot, nt = types(old), types(new)
    if ot is not None and nt is not None and not ot <= nt:
        rep.error(where, f'[type_narrowed] type narrowed: {sorted(ot)} -> {sorted(nt)}')
    if ot is None and nt is not None:
        rep.error(where, f'[type_added] type {sorted(nt)} appeared: the set of values is narrowed')
    if 'enum' in old and 'enum' in new:
        gone = [v for v in old['enum'] if not among(v, new['enum'])]
        if gone:
            rep.error(where, f'[enum_value_removed] values {gone} left enum')
    if 'enum' not in old and 'enum' in new:
        rep.error(where, '[enum_added] enum appeared: the set of values is narrowed')
    for k, tighter in TIGHTER.items():
        if k in new and (k not in old or tighter(new[k], old[k])):
            rep.error(where, f'[limit_tightened] {k} is stricter: {old.get(k)} -> {new[k]}')
    for k in CHANGED:
        prep = bare if k in SCHEMA_VALUED else (lambda v: v)
        if k in new and (k not in old or not same(prep(old[k]), prep(new[k]))):
            rep.error(where, f'[keyword_changed] {k} changed: {old.get(k)!r} -> {new[k]!r}')
    added_req = set(new.get('required', [])) - set(old.get('required', []))
    if added_req:
        rep.error(where, f'[required_added] new required fields {sorted(added_req)}')
    for prop, deps in (new.get('dependentRequired') or {}).items():
        more = set(deps) - set((old.get('dependentRequired') or {}).get(prop, []))
        if more:
            rep.error(where, f'[required_added] dependentRequired.{prop}: new {sorted(more)}')
    for k in CLOSABLE:
        if old.get(k, True) is not False and new.get(k, True) is False:
            rep.error(where, f'[closed_now] {k} became false')
    # a new prefixItems item takes a position that old items (or unevaluatedItems) governed: absent accepts
    # anything, false accepted nothing there; so it is compared with that schema like a changed subschema
    old_prefix, new_prefix = old.get('prefixItems') or [], new.get('prefixItems') or []
    # without items, a position is left to unevaluatedItems, unless an applicator may evaluate it
    applied = any(k in old for k in ('allOf', 'anyOf', 'oneOf', 'if', '$ref', '$dynamicRef', 'contains'))
    old_position = old['items'] if 'items' in old else (
        True if applied else old.get('unevaluatedItems', True))
    if isinstance(old_prefix, list) and isinstance(new_prefix, list):
        for i in range(len(old_prefix), len(new_prefix)):
            compare(old_position, new_prefix[i], f'{where}/prefixItems/{i}')
        # a removed prefixItems item leaves its position to new items, or else to new unevaluatedItems
        # (assumed to apply even where an applicator may evaluate the position, which is the stricter case)
        new_position = new['items'] if 'items' in new else new.get('unevaluatedItems', True)
        for i in range(len(new_prefix), len(old_prefix)):
            compare(old_prefix[i], new_position, f'{where}/prefixItems/{i}')
    for key in ('properties', '$defs', 'patternProperties', 'dependentSchemas'):
        op, np = old.get(key, {}), new.get(key, {})
        if not isinstance(op, dict) or not isinstance(np, dict):
            continue
        for name, sub in op.items():
            if name not in np:
                rep.error(where, f'[field_removed] {key}.{name} removed')
            else:
                compare(sub, np[name], f'{where}/{key}/{name}')
    # a new pattern constrains names that other keywords governed before; it widens only where
    # no name was allowed at all, otherwise it is compared with true, conservatively
    op, np = old.get('patternProperties') or {}, new.get('patternProperties') or {}
    if isinstance(op, dict) and isinstance(np, dict):
        closed = old.get('additionalProperties') is False and not old.get('properties') and not op
        for name in sorted(set(np) - set(op)):
            compare(False if closed else True, np[name], f'{where}/patternProperties/{name}')
    for key in ('items', 'additionalProperties', 'then', 'else', 'contains', 'propertyNames',
                'unevaluatedProperties', 'unevaluatedItems'):
        # an absent subschema accepts everything, like true: compare reports any asserting keyword in its place
        # (a subschema that became false is already [closed_now] for the keys checked above)
        closed = new.get(key) is False and key in CLOSABLE
        if old.get(key, True) is not False and isinstance(new.get(key), (dict, bool)) and not closed:
            compare(old.get(key, True), new[key], f'{where}/{key}')
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
        # a new branch, or a changed one (even widened), can make an old value match two branches;
        # branch order has no meaning, so a branch equal to an old one is unchanged wherever it stands
        left, fresh = unmatched(ob, nb)
        for i, _ in fresh:
            if not all(disjoint(nb[i], b) for j, b in enumerate(nb) if j != i):
                what = 'changed' if left else 'new'
                rep.error(where, f'[oneof_overlap] {what} oneOf branch {i} is not provably disjoint from the others: '
                          'values matching two branches would stop passing')
    od, nd = old.get('dependentSchemas') or {}, new.get('dependentSchemas') or {}
    if isinstance(od, dict) and isinstance(nd, dict):
        for name in sorted(set(nd) - set(od)):
            rep.error(where, f'[keyword_added] new name dependentSchemas.{name}: a new conditional constraint')
    for key in ('allOf', 'prefixItems'):
        for i, (a, b) in enumerate(zip(old.get(key, []), new.get(key, []))):
            compare(a, b, f'{where}/{key}/{i}')
    for key in ('anyOf', 'oneOf'):
        # match branches by content: an unchanged branch may move; the rest are paired in order
        ob, nb = old.get(key) or [], new.get(key) or []
        if not isinstance(ob, list) or not isinstance(nb, list):
            continue
        gone, fresh = unmatched(ob, nb)
        for a, (i, b) in zip(gone, fresh):
            compare(a, b, f'{where}/{key}/{i}')


def show(p):
    out = git('show', f'{base_commit}:{p}')
    return out.stdout if out.returncode == 0 else None


olds, news = {}, {}
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
    olds[rp], news[rp] = old, new


# not and if reverse polarity: a definition they reach through $ref may not change at all,
# since widening it narrows the outer schema; the references are followed across files by $id
def by_id(docs):
    return {d['$id']: rp for rp, d in docs.items() if isinstance(d, dict) and isinstance(d.get('$id'), str)}


def pointer(doc, ptr):
    for part in [p.replace('~1', '/').replace('~0', '~') for p in ptr.split('/')[1:]]:
        if isinstance(doc, dict) and part in doc:
            doc = doc[part]
        elif isinstance(doc, list) and part.isdigit() and int(part) < len(doc):
            doc = doc[int(part)]
        else:
            return None
    return doc


def refs(s):
    if isinstance(s, dict):
        if isinstance(s.get('$ref'), str):
            yield s['$ref']
        for k, v in s.items():
            if k not in LITERALS:
                yield from refs(v)
    elif isinstance(s, list):
        for x in s:
            yield from refs(x)


def negated(s, path=''):
    """(path, subschema) of every not and if value in a schema."""
    if isinstance(s, dict):
        for k, v in s.items():
            if k in LITERALS:
                continue
            if k in ('not', 'if'):
                yield f'{path}/{k}', v
            yield from negated(v, f'{path}/{k}')
    elif isinstance(s, list):
        for i, x in enumerate(s):
            yield from negated(x, f'{path}/{i}')


olds_by_id, news_by_id = by_id(olds), by_id(news)


def reached(rp, sub):
    """(file, fragment, old, new) of every definition a subschema reaches through $ref, transitively."""
    seen, todo = set(), [(rp, ref) for ref in refs(sub)]
    while todo:
        frm, ref = todo.pop()
        base, _, frag = ref.partition('#')
        target = olds_by_id.get(base) if base else frm
        if target is None or (target, frag) in seen:
            continue
        seen.add((target, frag))
        a = pointer(olds.get(target), frag)
        if a is None:
            continue
        yield target, frag, a, pointer(news.get(news_by_id.get(base, target) if base else target), frag)
        todo.extend((target, r) for r in refs(a))


def deref(s, frm, docs, ids):
    """The schema behind a chain of bare $ref, or s itself."""
    for _ in range(16):
        if not isinstance(s, dict) or not isinstance(s.get('$ref'), str) or set(s) - {'$ref'} - ANNOTATIONS:
            return s
        base, _, frag = s['$ref'].partition('#')
        frm = ids.get(base) if base else frm
        s = pointer(docs.get(frm), frag)
    return s


def separated(branches, rp, docs, ids):
    resolved = [deref(b, rp, docs, ids) for b in branches]
    return all(disjoint(b, c) for i, b in enumerate(resolved) for j, c in enumerate(resolved) if j != i)


def changed(a, b):
    return b is None or not same(bare(a), bare(b))


def oneofs(s, path=''):
    """(path, branches) of every oneOf in a schema."""
    if isinstance(s, dict):
        for k, v in s.items():
            if k in LITERALS:
                continue
            if k == 'oneOf' and isinstance(v, list):
                yield f'{path}/oneOf', v
            yield from oneofs(v, f'{path}/{k}')
    elif isinstance(s, list):
        for i, x in enumerate(s):
            yield from oneofs(x, f'{path}/{i}')


for rp, old in olds.items():
    # not and if reverse polarity: a definition they reach may not change at all
    for where, sub in negated(old):
        for target, frag, a, b in reached(rp, sub):
            if changed(a, b):
                rep.error(f'{target}#{frag}', f'[keyword_changed] the definition changed, and {rp}#{where} reaches it '
                          'through $ref under not or if: any change there can narrow the outer schema')
    # a widened definition behind a oneOf branch can make an old value match two branches; branches that the
    # disjointness proof (with $ref followed) separated before must stay separated
    for where, branches in oneofs(old):
        nb = pointer(news[rp], where)
        if not isinstance(nb, list) or not any(changed(a, b) for br in branches for _, _, a, b in reached(rp, br)):
            continue
        if separated(branches, rp, olds, olds_by_id) and not separated(nb, rp, news, news_by_id):
            rep.error(f'{rp}#{where}', '[oneof_overlap] a definition behind oneOf changed, and its branches are no '
                      'longer provably disjoint: values matching two branches would stop passing')

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
