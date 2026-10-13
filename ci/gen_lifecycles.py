#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Builds spec/lifecycles/<object>.yaml from the tables of <object>.md.

The table "State | Meaning | Terminal" gives the states, the table
"From | To | Trigger | Condition | Rules" gives the transitions. If the file
holds several machines (a campaign and its item), a machine is named by the
caption above its state table ("Campaign:", "### Run"), and a transition table
belongs to the machine with the same caption. A transition table whose caption
matches no machine is an error when there is more than one machine or when
one machine has several transition tables with different captions.

Cell rules (vocabulary fixed in the skeleton tools, see ci/README.md):
- "any", "any open", "any except ..." in a From cell means any state of the
  machine except the named ones and except terminal states: a terminal state
  has no outgoing transitions;
- "(none)" means the object does not exist yet; "(none), `x`" means "none or x";
- the leading list of names in backticks names only declared states: a typo
  in a name is an error, not a silent skip.

  python3 ci/gen_lifecycles.py          rebuild the YAML
  python3 ci/gen_lifecycles.py --check  check that the YAML matches the tables
                                        and that transition states are declared
"""
import copy
import os
import re
import sys

import yaml

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import ROOT, Report, id_regex, md_cells, read, registry, rel

rep = Report('lifecycles')
ID_RE = id_regex(registry())
LC = os.path.join(ROOT, 'spec', 'lifecycles')
STATE_HDR = '| State | Meaning | Terminal |'
TRANS_HDR = '| From | To | Trigger | Condition | Rules |'
NAME = r'`[a-z_][a-z0-9_]*(?:\([^`]*\))?`'
LEAD = re.compile(r'^(?:\(none\),?\s*)?(?:any(?:\s+open)?,?\s*except\s+)?((?:' + NAME + r'(?:,\s*|\s+or\s+|\s+and\s+)?)+)')


def ticks(text):
    return re.findall(r'`([a-z_][a-z0-9_]*)(?:\([^`]*\))?`', text)


def caption(lines, i):
    j = i - 1
    while j >= 0 and not lines[j].strip():
        j -= 1
    t = lines[j].strip() if j >= 0 else ''
    if t.startswith('## '):
        return None
    return t.lstrip('#').strip().rstrip(':') or None


def tables(lines, header):
    out = []
    for i, line in enumerate(lines):
        if line.strip() == header:
            rows, k = [], i + 2
            while k < len(lines) and lines[k].startswith('|'):
                rows.append((k + 1, md_cells(lines[k])))
                k += 1
            out.append((caption(lines, i), rows, i + 1))
    return out


TO_NONE = {'(no transition)': 'no_transition', '(none)': 'deleted', '(no record)': 'deleted', 'none': 'deleted',
           '(deleted)': 'deleted'}


def side(text, known, end='from'):
    """States in a From or To cell; the rest of the cell text is kept in text.

    A To cell has two kinds of empty end: `no_transition` ("(no transition)": the object
    stays where it was) and `deleted` ("(none)", "(no record)", "(deleted)": the record is removed).
    `none` only means "the object does not exist yet" and appears only in a From cell.
    """
    t = ' '.join(text.split())
    if end == 'to' and t in TO_NONE:
        return {TO_NONE[t]: True}
    if end == 'to' and t.startswith('(') and t.endswith(')') and not ticks(t):
        return {'no_transition': True, 'text': t}
    if t in ('(none)', '(no transition)', '(no record)', 'none'):
        return {'none': True}
    if t.startswith('(') and t.endswith(')') and not ticks(t):
        return {'none': True, 'text': t}
    out = {}
    if t.startswith('(none)'):
        out['none'] = True  # "(none), recipient `waiting`": no object, or it is in waiting
    if re.match(r'^any\b', t):
        out['any'] = True
        out['except'] = [x for x in ticks(t) if x in known]
    else:
        out['states'] = [x for x in ticks(t) if x in known]
    if re.match(r'^same\b', t):
        out['same'] = True
    if not (out.get('states') or out.get('any') or out.get('same')):
        out['unparsed'] = True
    lead = LEAD.match(t)
    unknown = [x for x in ticks(lead.group(1)) if x not in known] if lead else []
    if unknown:
        out['unknown'] = unknown
    out['text'] = t
    return out


def for_machine(t, machine):
    """Copy of a transition for one machine: for "any" in From the terminal states are excluded."""
    t = copy.deepcopy(t)
    frm = t['from']
    if frm.get('any'):
        terminal = [st['id'] for st in machine['states'] if st['terminal']]
        frm['except'] = frm['except'] + [x for x in terminal if x not in frm['except']]
    for end in ('from', 'to'):
        t[end].pop('unknown', None)
    return t


def build(path):
    lines = read(path).split('\n')
    name = os.path.basename(path)[:-3]
    rp = rel(path)
    title = lines[0].split(':', 1)[-1].strip()
    machines = []
    for cap, rows, at in tables(lines, STATE_HDR):
        states = []
        for ln, c in rows:
            if len(c) != 3:
                rep.error(f'{rp}:{ln}', f'[lifecycle_row_form] a state row does not have three cells: {c}')
                continue
            ids = ticks(c[0])
            if not ids:
                rep.error(f'{rp}:{ln}', f'[lifecycle_state_name] the state has no name in backticks: {c[0]}')
                continue
            states.append({'id': ids[0], 'terminal': bool(re.match(r'^yes\b', c[2].strip())), 'meaning': ' '.join(c[1].split())})
        machines.append({'name': cap, 'states': states, 'transitions': []})
    if not machines:
        rep.error(rp, '[lifecycle_no_states] no state table')
        return None
    trans = tables(lines, TRANS_HDR)
    names = {m['name'] for m in machines}
    trans_caps = {cap for cap, _, _ in trans if cap is not None}
    for cap, rows, at in trans:
        target = [m for m in machines if m['name'] == cap]
        fallback = not target
        if fallback:
            if len(machines) > 1:
                rep.error(f'{rp}:{at}', f'[lifecycle_machine_unknown] transition table "{cap or "without caption"}" belongs '
                          f'to no machine {sorted(n or "" for n in names)}: give it the same caption as its state table')
            elif len(trans_caps) > 1:
                rep.error(f'{rp}:{at}', f'[lifecycle_machine_unknown] transition tables with different captions {sorted(trans_caps)} '
                          f'for one state table: split the state table by the same captions')
            target = machines
        for ln, c in rows:
            if len(c) != 5:
                rep.error(f'{rp}:{ln}', f'[lifecycle_row_form] a transition row does not have five cells: {c[:2]}')
                continue
            for m in target:
                known = {st['id'] for mm in (machines if fallback else [m]) for st in mm['states']}
                t = {'from': side(c[0], known), 'to': side(c[1], known, 'to'), 'by': ' '.join(c[2].split()),
                     'when': ' '.join(c[3].split()), 'rules': sorted(set(x.group(0) for x in ID_RE.finditer(c[4])))}
                for end, cell in (('from', 'From'), ('to', 'To')):
                    if t[end].get('unparsed'):
                        rep.error(f'{rp}:{ln}', f'[lifecycle_state_unknown] the {cell} cell names no declared state'
                                  f'{" of machine " + m["name"] if len(target) > 1 or not fallback and len(machines) > 1 else ""}: {t[end]["text"]}')
                    elif t[end].get('unknown'):
                        rep.error(f'{rp}:{ln}', f'[lifecycle_state_unknown] the {cell} cell names undeclared states '
                                  f'{t[end]["unknown"]}: {t[end]["text"]}')
                m['transitions'].append(for_machine(t, m))
    if len(machines) == 1:
        machines[0]['name'] = name
    return {'object': name, 'title': title, 'source': f'spec/lifecycles/{name}.md',
            'generated': 'ci/gen_lifecycles.py', 'machines': machines}


class NoAliases(yaml.SafeDumper):
    def ignore_aliases(self, data):
        return True


check = '--check' in sys.argv
HEAD = '# Generated by ci/gen_lifecycles.py from the tables of {src}. Do not edit by hand.\n'
for f in sorted(os.listdir(LC)):
    path = os.path.join(LC, f)
    if f.endswith('.yaml') and not os.path.exists(path[:-5] + '.md') and read(path).startswith('# Generated by ci/gen_lifecycles.py'):
        rep.error(rel(path), f'[generated_orphan] generated lifecycle without the table {f[:-5]}.md: delete the file')
        continue
    if not f.endswith('.md'):
        continue
    doc = build(path)
    if doc is None:
        continue
    out = HEAD.format(src=doc['source']) + yaml.dump(doc, Dumper=NoAliases, allow_unicode=True, sort_keys=False, width=120)
    target = path[:-3] + '.yaml'
    old = read(target) if os.path.exists(target) else None
    if old == out:
        continue
    if check:
        rep.error(rel(target), '[generated_stale] does not match the tables: run python3 ci/gen_lifecycles.py')
    else:
        with open(target, 'w', encoding='utf-8') as fh:
            fh.write(out)
        rep.note(rel(target), '[generated_written] rebuilt')
rep.finish()
