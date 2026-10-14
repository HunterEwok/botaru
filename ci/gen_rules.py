#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Builds conformance/rules.md, conformance/retired.md and the "Rules of this section" sections.

There is one source: the registry conformance/requirements.yaml.
  python3 ci/gen_rules.py          rebuild the files
  python3 ci/gen_rules.py --check  check that the files are built from the current registry
"""
import collections
import os
import sys

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import ROOT, Report, id_key, read, registry, walk
from common import rel as root_rel

OUTPUT = {}
FAM_NAMES = {
    'AGG': 'Aggregators', 'APR': 'Approvals', 'ART': 'Files and artifacts', 'AUD': 'Audit',
    'BGR': 'Bot groups', 'BUD': 'Budgets', 'CAP': 'Dependency admission and refusal', 'CNT': 'Continuations',
    'COM': 'Commission', 'DEP': 'Deployment and least privilege', 'DIG': 'Showing proposals to people',
    'DLV': 'Delivery', 'DSP': 'Task pool and dispatcher', 'FEN': 'Leases', 'GOV': 'Bot governance',
    'GW': 'Model gateway', 'HLD': 'Stop', 'IDM': 'Idempotency', 'IDN': 'Identity',
    'ING': 'Incoming external events', 'INJ': 'Injection resistance', 'INS': 'Analysis of own data',
    'KNW': 'Tenant knowledge', 'LCY': 'Tenant lifecycle', 'MAN': 'Manifests', 'MEM': 'Memory',
    'MGR': 'Farm manager', 'MSG': 'Messages between bots', 'OBS': 'Observability', 'MON': 'External monitoring',
    'OBV': 'Observations', 'ORC': 'Campaigns', 'OUT': 'Structured results', 'PRF': 'Role profiles',
    'PRM': 'Permissions', 'PRP': 'Improvement proposals', 'PRV': 'Instruction versions and execution fingerprint',
    'REP': 'Reporters', 'RET': 'Retention and deletion', 'RT': 'Agent runtime', 'SCR': 'Scripts',
    'SEC': 'Secrets', 'SKL': 'Skills', 'STA': 'Bot state', 'TEN': 'Tenants', 'TIM': 'Time and clocks',
    'TRN': 'Translator',
}
MARK = '<!-- generated: rules -->'
CHECK_NAMES = {'fixture': 'fixture', 'store': 'store', 'blackbox': 'blackbox', 'adapter': 'adapter', 'eval': 'eval', 'lint': 'lint'}

rows = [r for r in registry() if isinstance(r, dict) and isinstance(r.get('id'), str)]
act = [r for r in rows if r.get('status') == 'active']
ret = [r for r in rows if r.get('status') == 'retired']
byfam = collections.defaultdict(list)
for r in act:
    byfam[r['id'].split('-')[0]].append(r)


def link(frm, to):
    return os.path.relpath(os.path.join(ROOT, to), os.path.dirname(os.path.join(ROOT, frm))).replace(os.sep, '/')


def meta(r):
    bits = [f"since {r['version']}", 'non-waivable' if r.get('waivable') is False else 'waivable', f"profile {r.get('profile')}"]
    if r.get('level') == 'SHOULD':
        bits.append('recommendation')
    bits.append('check: ' + ', '.join(r.get('check') or []))
    if r.get('decisions'):
        bits.append('decisions: ' + ', '.join(r['decisions']))
    return ' · '.join(bits)


L = ['# Skeleton rules', '',
     'This file is generated from the registry `conformance/requirements.yaml`. Edit the registry, then rebuild this file.', '',
     'Every rule is written once in the registry. The documents in `docs/` explain how a part of the farm works and refer to rules by number; the normative text of a rule is here.', '',
     'The line under a rule heading:', '',
     '- "since v0.1": the skeleton version from which the conformance suite checks the rule.',
     '- "non-waivable": an implementation cannot put the rule on its list of expected failures. "Waivable": it can, with a recorded reason.',
     '- "profile": the part of the skeleton an implementation claims conformance to separately (`core`, `messaging`, `campaign`, `insights`, `manager`, `governance`, `monitor`, `knowledge`, `ingress`, `connections`, `debug`); the `core` profile is mandatory, see [docs/quality.md](../docs/quality.md).',
     '- "recommendation": a SHOULD rule; all others are mandatory.',
     '- "check": how compliance is shown. `fixture`: a ready data sample that must pass or fail a check. `store`: a storage check. `blackbox`: a test through the ports without knowledge of the implementation internals. `adapter`: a test of a port adapter. `eval`: a check of model work quality on a set of cases. `lint`: a check of files and manifests.',
     '- "decisions": decision numbers from [docs/decisions.md](../docs/decisions.md).', '',
     '## Summary', '',
     '| Family | About | Document | Rules | Non-waivable | v0.1 | v0.2 |',
     '|---|---|---|---|---|---|---|']
tot = collections.Counter()
for fam in sorted(byfam):
    rs = byfam[fam]
    docs = collections.Counter(r.get('doc') or '' for r in rs).most_common(1)[0][0]
    c = collections.Counter(r['version'] for r in rs)
    nw = sum(1 for r in rs if r.get('waivable') is False)
    tot.update(c); tot['all'] += len(rs); tot['nw'] += nw
    L.append(f"| [{fam}](#{fam.lower()}) | {FAM_NAMES.get(fam, fam)} | [{docs}]({link('conformance/rules.md', docs)}) | {len(rs)} | {nw} | {c['v0.1']} | {c['v0.2']} |")
L.append(f"| Total | | | {tot['all']} | {tot['nw']} | {tot['v0.1']} | {tot['v0.2']} |")
L += ['', f'Retired numbers ({len(ret)}) are listed in [retired.md](retired.md).', '']
for fam in sorted(byfam):
    rs = sorted(byfam[fam], key=lambda r: id_key(r['id']))
    docs = sorted(set(r['doc'] for r in rs))
    L += [f'<a id="{fam.lower()}"></a>', f'## {fam}: {FAM_NAMES.get(fam, fam)}', '',
          'Explained in: ' + ', '.join(f'[{d}]({link("conformance/rules.md", d)})' for d in docs), '']
    for r in rs:
        L += [f'<a id="{r["id"]}"></a>', f"### {r['id']}. {r['title']}", '', meta(r), '', ' '.join(str(r['text']).split()), '']
        if r.get('fixtures'):
            L += ['Checked by: ' + ', '.join(f'`{x}`' for x in r['fixtures']), '']
OUTPUT['conformance/rules.md'] = '\n'.join(L).rstrip() + '\n'

R = ['# Retired rule numbers', '',
     'This file is generated from the registry `conformance/requirements.yaml`.', '',
     'A rule number is never reused. These numbers are no longer active: the rule was revoked by a decision or merged into another rule. Active rules are in [rules.md](rules.md).', '',
     '| Number | Title | Since | Why | Replaced by |', '|---|---|---|---|---|']
for r in sorted(ret, key=lambda r: id_key(r['id'])):
    rb = ', '.join(f'[{x}](rules.md#{x})' for x in (r.get('replaced_by') or [])) or 'none'
    R.append(f"| {r['id']} | {r['title']} | {r['retired']} | {' '.join(str(r['reason']).split())} | {rb} |")
OUTPUT['conformance/retired.md'] = '\n'.join(R) + '\n'

# per-doc sections
bydoc = collections.defaultdict(list)
for r in act:
    if r.get('doc'):
        bydoc[r['doc']].append(r)
missing_docs = []
for d, rs in sorted(bydoc.items()):
    p = os.path.join(ROOT, d)
    if not os.path.exists(p):
        missing_docs.append(d)
        continue
    t = read(p)
    if MARK in t:
        t = t[:t.index(MARK)].rstrip() + '\n'
    to_rules = link(d, 'conformance/rules.md')
    S = [MARK, '## Rules of this section', '',
         f'Generated from the registry `conformance/requirements.yaml`; the full text of every rule is in [conformance/rules.md]({to_rules}).', '',
         '| Rule | About | Since | Non-waivable |', '|---|---|---|---|']
    for r in sorted(rs, key=lambda r: id_key(r['id'])):
        S.append(f"| [{r['id']}]({to_rules}#{r['id']}) | {r['title']} | {r['version']} | {'yes' if r.get('waivable') is False else 'no'} |")
    OUTPUT[d] = t.rstrip() + '\n\n' + '\n'.join(S) + '\n'

# a document without active rules loses its "Rules of this section" section
for f in walk(('.md',)):
    d = root_rel(f)
    if d not in OUTPUT and d not in bydoc:
        t = read(f)
        if MARK in t:
            OUTPUT[d] = t[:t.index(MARK)].rstrip() + '\n'

check = '--check' in sys.argv
rep = Report('generated files')
for d in missing_docs:
    rep.error(d, '[rule_doc_missing] the document from the doc field of the registry is not found')
for path, text in sorted(OUTPUT.items()):
    full = os.path.join(ROOT, path)
    old = read(full) if os.path.exists(full) else None
    if old == text:
        continue
    if check:
        rep.error(path, '[generated_stale] does not match the registry: run python3 ci/gen_rules.py')
    else:
        with open(full, 'w', encoding='utf-8') as fh:
            fh.write(text)
        rep.note(path, '[generated_written] rebuilt')
rep.finish()
