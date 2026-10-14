#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Version readiness: every rule of the version has something that checks it.

A rule is covered when every name in its fixtures field is covered; a rule without
names is covered if its number is in the rules of at least one check with an id.
A name from the fixtures field of a rule is covered if:
- the fixture path (lease/stale-complete, authorize/holds/, fixture:files/x)
  exists in conformance/fixtures/ as a .yaml or .json file or a folder;
- the suite check (blackbox:core/x, adapter:channel/z, lint:roles/w)
  or the fixture or case number (fx-msg-01, ctd-001) is written as an id
  in YAML inside conformance/ or evals/.

Covered rules are counted as three numbers (lead decision 11):
- by data the skeleton CI runs itself: every name is covered by a fixture in
  conformance/fixtures/manifests/ (lint check_manifests.py) or
  conformance/fixtures/schemas/ (check_schemas.py);
- by data for an implementation runner or eval: every name is covered by data,
  but at least one only by a behaviour fixture or an evals/ case, which the
  implementation runner or an eval with a model executes;
- by a description only: at least one name is covered only by a suite entry in
  conformance/suites/ (given, action, expectation; the implementation writes
  the check code), a state-machine/ fixture (transitions in prose), a behaviour
  fixture whose call body fails its request schema ([fixture_body_invalid] in
  check_crossrefs.py), or an eval case of a later version.
A separate line: core rules with waivable: false covered only by a description.

Note [fixture_not_citing]: a fixture or case from the fixtures field of a rule
does not name that rule in its rules (neither its own nor those of the file).

  python3 ci/readiness.py [v0.1]                report
  python3 ci/readiness.py v0.1 --strict         error if the version is not covered
  python3 ci/readiness.py v0.1 --list           list the uncovered names
  python3 ci/readiness.py v0.1 --extra <folder> one more folder with implementation fixtures (can repeat)

A test of the reference implementation counts as data for a runner: it runs
against an implementation (the workflow go runs it on every change).
"""
import collections
import os
import re
import sys

import yaml

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import ROOT, Report, call_body_errors, id_key, load_yaml_all, registry, rel, walk

argv = sys.argv[1:]
extra_dirs = []
while '--extra' in argv:
    i = argv.index('--extra')
    extra_dirs.append(os.path.abspath(argv[i + 1]) if i + 1 < len(argv) else '')
    del argv[i:i + 2]
args = [a for a in argv if not a.startswith('-')]
version = args[0] if args else 'v0.1'
strict, listing = '--strict' in argv, '--list' in argv
rep = Report(f'readiness {version}')
for d in extra_dirs:
    if not os.path.isdir(d):
        rep.error(d or '--extra', '[extra_dir_missing] --extra needs an existing folder')
        rep.finish()

FX = os.path.join(ROOT, 'conformance', 'fixtures')
SUITES = os.path.join(ROOT, 'conformance', 'suites') + os.sep
CI_FX = tuple(os.path.join(FX, d) + os.sep for d in ('manifests', 'schemas'))
PROSE_FX = os.path.join(FX, 'state-machine') + os.sep
CI, RUNNER, DESCR = 'ci', 'runner', 'description'
RANK = {CI: 3, RUNNER: 2, DESCR: 1}
declared = {}  # id -> best kind
referenced = collections.defaultdict(set)  # rule -> ids of checks that have it in rules
cites = collections.defaultdict(set)  # check id or file path -> rule numbers it names
file_kind = {}  # file path -> kind


def best(a, b):
    return a if b is None or (a is not None and RANK[a] >= RANK[b]) else b


def all_rules(node, out):
    if isinstance(node, dict):
        for rid in node.get('rules') or [] if isinstance(node.get('rules'), list) else []:
            if isinstance(rid, str):
                out.add(rid)
        for v in node.values():
            all_rules(v, out)
    elif isinstance(node, list):
        for v in node:
            all_rules(v, out)
    return out


def collect(node, kind, top):
    if isinstance(node, dict):
        if isinstance(node.get('id'), str):
            declared[node['id']] = best(kind, declared.get(node['id']))
            own = all_rules(node, set())
            cites[node['id']] |= own | top
            for rid in own:
                referenced[rid].add(node['id'])
        for v in node.values():
            collect(v, kind, top)
    elif isinstance(node, list):
        for v in node:
            collect(v, kind, top)


def kind_of_file(f, docs):
    if f.startswith(SUITES):
        return DESCR
    if f.startswith(CI_FX):
        return CI
    if f.startswith(PROSE_FX):
        return DESCR
    if f.startswith(FX) or any(f.startswith(d + os.sep) for d in extra_dirs):
        if any(call_body_errors(d) for d in docs):
            return DESCR
    return RUNNER


roots = [os.path.join(ROOT, 'conformance'), os.path.join(ROOT, 'evals')] + extra_dirs
for d in roots:
    for f in walk(('.yaml', '.yml'), d):
        if f.endswith('requirements.yaml') and d.startswith(ROOT):
            continue
        try:
            docs = load_yaml_all(f)
        except yaml.YAMLError as e:
            rep.error(rel(f), f'[yaml_invalid] cannot be read, its ids are not counted: {" ".join(str(e).split())[:200]}')
            continue
        kind = kind_of_file(f, docs)
        file_kind[f] = kind
        for doc in docs:
            top = {x for x in (doc.get('rules') or []) if isinstance(x, str)} if isinstance(doc, dict) and isinstance(doc.get('rules'), list) else set()
            cites[f] |= all_rules(doc, set())
            collect(doc, kind, top)

# Eval cases of later versions: names from the rows of the version table
# in evals/README.md for versions after the checked one. A rule of the current version may
# name such a case in advance; it can be covered only in that version.
later = set()
ev = os.path.join(ROOT, 'evals', 'README.md')
if os.path.exists(ev):
    vkey = tuple(int(x) for x in version[1:].split('.'))
    for line in open(ev, encoding='utf-8'):
        m = re.match(r'^\| v(\d+)\.(\d+) \|', line)
        if not m or (int(m.group(1)), int(m.group(2))) <= vkey:
            continue
        for pre, lo, hi in re.findall(r'`([a-z][a-z0-9-]*-)(\d+)`\.\.`[a-z][a-z0-9-]*-(\d+)`', line):
            for n in range(int(lo), int(hi) + 1):
                later.add(f'{pre}{n:0{len(lo)}d}')
        later.update(re.findall(r'`([a-z][a-z0-9-]*-\d+)`', line))


GO_FUNC = re.compile(r'^func (Test[A-Za-z0-9_]*)\(')
RULE_TOKEN = re.compile(r'\b[A-Z]{2,3}-\d+[a-z]?\b')
go_tests = {}  # (folder under impl/, TestName) -> rule numbers named in the comment above it
OTHER_PLATFORMS = {'aix', 'android', 'darwin', 'dragonfly', 'freebsd', 'illumos', 'ios', 'js', 'netbsd', 'openbsd',
                   'plan9', 'solaris', 'wasip1', 'windows', '386', 'arm', 'arm64', 'loong64', 'mips', 'mipsle',
                   'mips64', 'mips64le', 'ppc64', 'ppc64le', 'riscv64', 's390x', 'wasm'}


def scan_go_tests():
    base = os.path.join(ROOT, 'impl')
    for f in walk(('_test.go',), base) if os.path.isdir(base) else []:
        folder = os.path.relpath(os.path.dirname(f), base).replace(os.sep, '/')
        # go test ./... ignores files and folders that start with _ or . and folders named testdata
        if any(x.startswith(('_', '.')) or x == 'testdata' for x in os.path.relpath(f, base).split(os.sep)):
            continue
        text = open(f, encoding='utf-8').read()
        # only tests that go test runs in CI (linux/amd64, no tags): a file under a build constraint or with a
        # GOOS or GOARCH suffix other than linux or amd64 may never run, so its tests cover nothing
        parts = os.path.basename(f)[:-len('_test.go')].split('_')
        if re.search(r'^//go:build ', text, re.M) or any(p in OTHER_PLATFORMS for p in parts[1:]):
            continue
        comment = []
        for line in text.split('\n'):
            if line.startswith('//'):
                comment.append(line)
                continue
            m = GO_FUNC.match(line)
            if m:
                go_tests[(folder, m.group(1))] = set(RULE_TOKEN.findall(''.join(comment)))
            comment = []


scan_go_tests()


def covered(name):
    """What covers a name: (kind or None, rule numbers named by the covering check, or None)."""
    if name.startswith('impl:'):
        folder, _, test = name[len('impl:'):].rpartition('/')
        hit = go_tests.get((folder, test))
        return (RUNNER, hit) if hit is not None else (None, None)
    if name in declared and name not in later:
        return declared[name], cites[name]
    m = re.match(r'^([a-z]+):(.+)$', name)
    path = m.group(2) if m and m.group(1) == 'fixture' else (None if m else name)
    if path and '/' in path:
        for base in [FX] + extra_dirs:
            p = os.path.join(base, path.rstrip('/'))
            hits = [p + e for e in ('', '.yaml', '.yml', '.json') if os.path.isfile(p + e)]
            if os.path.isdir(p):
                hits = [f for f in file_kind if f.startswith(p + os.sep)]
                if not hits:
                    return DESCR, None
            if hits:
                kinds = {file_kind.get(f, CI if f.startswith(CI_FX) else RUNNER) for f in hits}
                kind = CI if kinds == {CI} else (RUNNER if kinds & {CI, RUNNER} else DESCR)
                return kind, set().union(*(cites.get(f, set()) for f in hits))
    if name in later:
        return DESCR, None
    return None, None


rows = [r for r in registry() if isinstance(r, dict) and r.get('status') == 'active' and r.get('version') == version]
by_profile = collections.defaultdict(collections.Counter)
missing = collections.defaultdict(list)
kinds = collections.Counter()
core_hard_descr = []
for r in sorted(rows, key=lambda r: id_key(r['id'])):
    prof = r.get('profile')
    by_profile[prof]['all'] += 1
    names = r.get('fixtures') or []
    if names:
        got = {}
        for n in names:
            got[n], cited = covered(n)
            if got[n] in (CI, RUNNER) and cited is not None and r['id'] not in cited:
                rep.note(f'{version} {r["id"]}', f'[fixture_not_citing] check {n} from the fixtures field does not name the rule in its rules')
        gaps = [n for n, k in got.items() if k is None]
        ks = set(got.values())
        kind = CI if ks == {CI} else (RUNNER if ks <= {CI, RUNNER} else DESCR)
    else:
        # a rule without check names is covered if a check in conformance/ or evals/ refers to it
        refs = referenced.get(r['id']) or set()
        gaps = [] if refs else ['(no check names)']
        kind = None
        for x in refs:
            kind = best(declared.get(x), kind)
        kind = kind or DESCR
    if gaps:
        by_profile[prof]['open'] += 1
        missing[r['id']] = gaps
    else:
        by_profile[prof][kind] += 1
        kinds[kind] += 1
        if prof == 'core' and r.get('waivable') is False and kind == DESCR:
            core_hard_descr.append(r['id'])


def numbers(c):
    return f'by data the skeleton CI runs {c[CI]}, by data for a runner or eval {c[RUNNER]}, by a description only {c[DESCR]}'


for prof in sorted(by_profile, key=str):
    c = by_profile[prof]
    rep.info(f'{prof}: {c["all"]} rules; {numbers(c)}, not covered {c["open"]}')
rep.info(f'core with waivable: false covered only by a description: {len(core_hard_descr)} of '
         f'{sum(1 for r in rows if r.get("profile") == "core" and r.get("waivable") is False)}')
if listing:
    for rid in sorted(missing, key=id_key):
        print(f'  {rid}: ' + ', '.join(missing[rid]))
total = len(rows)
print(f'{version}: rules {total}; covered {numbers(kinds)}; not covered {len(missing)}')
if strict and missing:
    for rid in sorted(missing, key=id_key):
        rep.error(f'{version} {rid}', '[rule_not_covered] version rule without a check: ' + ', '.join(missing[rid]))
rep.finish(limit=50)
