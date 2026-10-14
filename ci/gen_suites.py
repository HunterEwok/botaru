#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Checks the suites conformance/suites/ and builds their index in docs/quality.md.

The suite files conformance/suites/<suite>/<area>.yaml are the source of the
suite checks and are edited by hand: one entry per check with id
`<method>:<area>/<name>` (blackbox, adapter, lint), method, rules, version,
group, spec (the cells of the description: What, Expectation and so on) and
an optional note. An optional list `fixtures` keeps the description of
fixture rows from conformance/fixtures/ that belong to the same group of checks.
The implementation runner executes the checks; a suite is only data.

Derived fields: version is the earliest version of the rules of the check, and
rules also hold every rule that names the check in its fixtures field. The
script fills them and rewrites the generated index of docs/quality.md (between
<!-- generated: suites --> and <!-- /generated: suites -->).

  python3 ci/gen_suites.py          normalize the suites and rebuild the index
  python3 ci/gen_suites.py --check  check that the suites and the index are up to date
"""
import collections
import os
import re
import sys

import yaml

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import ROOT, Report, id_key, id_regex, load_yaml_all, read, referable, registry, rel, walk

rep = Report('check suites')
check = '--check' in sys.argv
rows = [r for r in registry() if isinstance(r, dict) and isinstance(r.get('id'), str)]
ID_RE = id_regex(rows)
status = {r['id']: r for r in rows}
kept = referable()
BASE = os.path.join(ROOT, 'conformance', 'suites')
FIXTURES = os.path.join(ROOT, 'conformance', 'fixtures')
DOC = 'docs/quality.md'
MARKS = ('<!-- generated: suites -->', '<!-- /generated: suites -->')
NAME = re.compile(r'^(blackbox|adapter|lint):([a-z0-9-]+)/([a-z0-9-]+)$')
# Adapter suites exist only for the contract boundaries with foreign code (C10, D-114): the channel E1,
# the toolhost E3 (with the self-stop of work holders by their own clock) and the monitoring adapter E5.
# The task store, the agent runtime and the sink describe how the core is built and are checked
# through the core (core-blackbox) or by Go tests of the reference implementation.
SUITE = {
    ('blackbox', 'gateway'): 'model-gateway',
    ('adapter', 'channel'): 'channel', ('adapter', 'toolhost'): 'toolhost', ('adapter', 'lease'): 'toolhost',
    ('adapter', 'monitor'): 'monitor', ('adapter', 'erasure'): 'erasure',
}
DEFAULT = {'blackbox': 'core-blackbox', 'adapter': 'adapter', 'lint': 'lint'}
TOP = ('suite', 'area', 'checks', 'fixtures')
CHECK_KEYS = ('id', 'method', 'rules', 'version', 'group', 'note', 'spec')
FIXTURE_KEYS = ('fixture', 'rules', 'group', 'spec')
HEAD = ('# Suite checks {suite}/{area}. This file is the source: edit it by hand.\n'
        '# Format and fields: docs/quality.md, section "Suites". python3 ci/gen_suites.py fills version\n'
        '# and the rules that name a check in their fixtures field; --check fails if anything is out of date.\n')

# rules that name the check in their fixtures field
named_by = collections.defaultdict(set)
for r in rows:
    if r['id'] in kept:
        for fx in r.get('fixtures') or []:
            named_by[fx].add(r['id'])

# ids of the fixture files, for the fixtures lists of the suites
fixture_ids = set()
for f in walk(('.yaml', '.yml', '.json'), FIXTURES) if os.path.isdir(FIXTURES) else []:
    if f.endswith(('.yaml', '.yml')):
        try:
            for d in load_yaml_all(f):
                if isinstance(d, dict) and isinstance(d.get('id'), str):
                    fixture_ids.add(d['id'])
        except yaml.YAMLError:
            pass


def fixture_exists(name):
    if name.startswith('fixture:'):
        p = os.path.join(FIXTURES, name[len('fixture:'):])
        return os.path.isdir(p) or any(os.path.isfile(p + e) for e in ('.yaml', '.yml', '.json'))
    return name in fixture_ids


def str_map(v):
    return isinstance(v, dict) and v and all(isinstance(k, str) and isinstance(x, str) for k, x in v.items())


def norm_rules(where, cid, value, extra):
    if not isinstance(value, list) or not all(isinstance(x, str) for x in value):
        rep.error(where, f'[suite_entry_field] {cid}: rules must be a list of rule ids')
        value = []
    for x in value:
        if not ID_RE.fullmatch(x):
            rep.error(where, f'[suite_entry_field] {cid}: {x} is not a rule id')
        elif x not in kept:
            rep.error(where, f'[rule_not_active] {cid}: rule {x} is not active (nor deferred with its mechanism kept)')
    return sorted(set(value) | extra, key=id_key)


seen = {}
index = []  # (suite, area, path, number of checks, number of fixture rows, groups)
for path in sorted(walk(('.yaml', '.yml'), BASE)) if os.path.isdir(BASE) else []:
    rp = rel(path)
    parts = os.path.relpath(path, BASE).split(os.sep)
    try:
        docs = load_yaml_all(path)
    except yaml.YAMLError as e:
        rep.error(rp, f'[yaml_invalid] the YAML cannot be read: {" ".join(str(e).split())[:200]}')
        continue
    if len(parts) != 2 or not path.endswith('.yaml'):
        rep.error(rp, '[suite_path] a suite file lives in conformance/suites/<suite>/<area>.yaml')
        continue
    suite, area = parts[0], parts[1][:-len('.yaml')]
    if len(docs) != 1 or not isinstance(docs[0], dict):
        rep.error(rp, '[suite_form] a suite file holds one mapping with suite, area and checks')
        continue
    d = docs[0]
    for k in d:
        if k not in TOP:
            rep.error(rp, f'[suite_form] unknown field {k}')
    if d.get('suite') != suite or d.get('area') != area:
        rep.error(rp, f'[suite_path] suite and area must be {suite} and {area}, as in the path')
    checks = d.get('checks')
    if not isinstance(checks, list) or not checks:
        rep.error(rp, '[suite_form] checks must be a non-empty list')
        checks = []
    out_checks, groups = [], []
    for i, c in enumerate(checks):
        where = f'{rp}#checks/{i}'
        if not isinstance(c, dict) or not isinstance(c.get('id'), str):
            rep.error(where, '[suite_form] a check is a mapping with an id')
            continue
        cid = c['id']
        for k in c:
            if k not in CHECK_KEYS:
                rep.error(where, f'[suite_entry_field] {cid}: unknown field {k}')
        m = NAME.match(cid)
        if not m:
            rep.error(where, f'[suite_check_name] {cid}: the id is <method>:<area>/<name> with method blackbox, adapter or lint')
            continue
        method, carea = m.group(1), m.group(2)
        if cid in seen:
            rep.error(where, f'[suite_check_duplicate] check {cid} is already described in {seen[cid]}')
            continue
        seen[cid] = where
        if carea != area or SUITE.get((method, carea), DEFAULT[method]) != suite:
            rep.error(where, f'[suite_path] check {cid} belongs to conformance/suites/{SUITE.get((method, carea), DEFAULT[method])}/{carea}.yaml')
        if c.get('method') != method:
            rep.error(where, f'[suite_entry_field] {cid}: method must be {method}, as in the id')
        rules = norm_rules(where, cid, c.get('rules'), named_by.get(cid, set()))
        versions = sorted({status[x]['version'] for x in rules if x in kept})
        if not isinstance(c.get('group'), str) or not c['group'].strip():
            rep.error(where, f'[suite_entry_field] {cid}: group must be a non-empty string')
        if not str_map(c.get('spec')):
            rep.error(where, f'[suite_entry_field] {cid}: spec must be a non-empty mapping of strings')
        if 'note' in c and not isinstance(c['note'], str):
            rep.error(where, f'[suite_entry_field] {cid}: note must be a string')
        e = {'id': cid, 'method': method, 'rules': rules, 'version': versions[0] if versions else None,
             'group': c.get('group')}
        if 'note' in c:
            e['note'] = c['note']
        e['spec'] = c.get('spec')
        out_checks.append(e)
        if c.get('group') not in groups:
            groups.append(c.get('group'))
    out_fx = []
    for i, x in enumerate(d.get('fixtures') or []):
        where = f'{rp}#fixtures/{i}'
        if not isinstance(x, dict) or not isinstance(x.get('fixture'), str):
            rep.error(where, '[suite_form] a fixtures entry is a mapping with a fixture name')
            continue
        for k in x:
            if k not in FIXTURE_KEYS:
                rep.error(where, f'[suite_entry_field] {x["fixture"]}: unknown field {k}')
        if not fixture_exists(x['fixture']):
            rep.error(where, f'[suite_fixture_missing] {x["fixture"]} is neither a fixture id nor a path in conformance/fixtures/')
        if not str_map(x.get('spec')):
            rep.error(where, f'[suite_entry_field] {x["fixture"]}: spec must be a non-empty mapping of strings')
        out_fx.append({'fixture': x['fixture'], 'rules': norm_rules(where, x['fixture'], x.get('rules'), set()),
                       'group': x.get('group'), 'spec': x.get('spec')})
    doc = {'suite': suite, 'area': area, 'checks': out_checks}
    if out_fx:
        doc['fixtures'] = out_fx
    out = HEAD.format(suite=suite, area=area) + yaml.safe_dump(doc, allow_unicode=True, sort_keys=False, width=120)
    index.append((suite, area, rp, len(out_checks), len(out_fx), groups))
    if read(path) != out:
        if check:
            rep.error(rp, '[generated_stale] version or rules are not filled in, or the layout is not normal: run python3 ci/gen_suites.py')
        else:
            with open(path, 'w', encoding='utf-8') as fh:
                fh.write(out)
            rep.note(rp, f'[generated_written] normalized, checks {len(out_checks)}')

for cid, rids in sorted(named_by.items()):
    m = re.match(r'^(blackbox|adapter|lint):', cid)
    if m and cid not in seen:
        v = sorted({status[r]['version'] for r in rids})
        rep.note(cid, f'[suite_check_undescribed] the check is not described in conformance/suites/ (rules {", ".join(sorted(rids, key=id_key))}, {v[0]})')

# the index in docs/quality.md
lines = ['| Suite file | Checks | Fixture rows | Groups |', '|---|---|---|---|']
for suite, area, rp, n, nf, groups in index:
    lines.append(f'| [`{suite}/{area}`](../{rp}) | {n} | {nf or ""} | {"; ".join(g for g in groups if g)} |')
lines.append('')
lines.append(f'Total: {sum(x[3] for x in index)} checks in {len(index)} files, {sum(x[4] for x in index)} fixture rows.')
doc_path = os.path.join(ROOT, DOC)
text = read(doc_path)
if MARKS[0] not in text or MARKS[1] not in text:
    rep.error(DOC, f'[suites_index_marks] the marks {MARKS[0]} and {MARKS[1]} are missing')
else:
    a, b = text.index(MARKS[0]) + len(MARKS[0]), text.index(MARKS[1])
    new = text[:a] + '\n' + '\n'.join(lines) + '\n' + text[b:]
    if new != text:
        if check:
            rep.error(DOC, '[generated_stale] the suite index does not match conformance/suites/: run python3 ci/gen_suites.py')
        else:
            with open(doc_path, 'w', encoding='utf-8') as fh:
                fh.write(new)
            rep.note(DOC, '[generated_written] the suite index was rebuilt')
rep.info(f'suite files {len(index)}, checks {len(seen)}')
rep.finish(limit=400)
