#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Text and links: rule and decision numbers, file paths, port operations.

- a mentioned rule number (any token like ABC-12, except SHA-256 and
  similar) is in the registry and is not retired;
- a mentioned decision number (D-NN) is in docs/decisions.md;
- a Markdown link ([x](path), [x](path "title"), [x]: path) leads to an
  existing file inside the public repository, and the anchor #frag exists in
  the target file (a heading or <a id>); a link /path is taken from the root;
- a path in backticks that starts with docs/, spec/, conformance/,
  examples/, evals/, ci/ or deploy/ exists; a path that does not exist yet is
  marked right after the backtick: `path` (v0.2) or `path` (planned ...);
  patterns with <...>, {...} and * are not checked;
- a port operation such as "P2 `hold.set`" is described in the port sheet;
- every active rule is explained in at least one document;
- the text of an active rule names no concrete system (VENDORS: Telegram,
  Zabbix, PostgreSQL, systemd, polkit, Anthropic): a rule talks about a port,
  the system name lives in the binding sheet (D-66, [vendor_in_rule]);
- YAML and JSON files are readable, a duplicate key is an error;
- no text file contains an em dash.
"""
import collections
import functools
import json
import os
import re
import sys

import yaml

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import (DEC_RE, GENERATED, REGISTRY, ROOT, SKIP_DIRS, Report, id_key, id_regex, known_decisions,
                    load_yaml_all, no_dup_json, read, registry, rel, walk)

rep = Report('text and links')
rows = [r for r in registry() if isinstance(r, dict) and isinstance(r.get('id'), str)]
ID_RE = id_regex(rows)
status = {r['id']: r.get('status') for r in rows}
decisions = known_decisions()
MARK = '<!-- generated: rules -->'
EM_DASH = '\u2014'
# Tokens like ABC-12 that are not rule numbers.
NOT_RULES = {'SHA', 'UTF', 'ITS', 'ISO', 'RFC', 'CVE', 'EPSG', 'UTM', 'AES', 'TLS', 'HMAC', 'AGPL', 'GPL', 'LGPL', 'NOTE'}
ANY_ID = re.compile(r'\b([A-Z]{2,4})-(\d+)([a-z]?)\b')
LINK = re.compile(r'\]\(\s*<?([^)\s>#]*)(#[^)\s>]*)?>?(?:\s+"[^"]*")?\s*\)')
REFDEF = re.compile(r'^\s{0,3}\[[^\]]+\]:\s*<?([^\s>#]*)(#[^\s>]*)?')
REPO_PREFIX = ('docs/', 'spec/', 'conformance/', 'examples/', 'evals/', 'ci/', 'deploy/')
TICK_PATH = re.compile(r'`((?:' + '|'.join(re.escape(p) for p in REPO_PREFIX) + r')[^`\s]*)`(\s*\((?:v0\.\d|planned))?')

ports_ops = collections.defaultdict(set)
for f in walk(('.md',), os.path.join(ROOT, 'spec', 'ports')):
    code = os.path.basename(f)[:2].upper()
    code = code if re.match(r'^[PCE][1-5]$', code) else 'ANY'
    for m in re.finditer(r'`([a-z_][a-z_0-9]*(?:\.[a-z_0-9]+)*)', read(f)):
        ports_ops[code].add(m.group(1))


def slug(text):
    """Heading anchor as GitHub builds it."""
    text = re.sub(r'\[([^\]]*)\]\([^)]*\)', r'\1', text.strip()).lower()
    text = re.sub(r'[^\w\- ]', '', text)
    return text.replace(' ', '-')


@functools.lru_cache(maxsize=None)
def anchors(path):
    out, counts, in_code = set(), collections.Counter(), False
    for line in read(path).split('\n'):
        if line.lstrip().startswith(('```', '~~~')):
            in_code = not in_code
            continue
        if in_code:
            continue
        out.update(re.findall(r'<a\s+(?:id|name)="([^"]+)"', line))
        m = re.match(r'^#{1,6}\s+(.*?)\s*#*\s*$', line)
        if m:
            s = slug(m.group(1))
            out.add(s if not counts[s] else f'{s}-{counts[s]}')
            counts[s] += 1
    return out


def check_link(where, f, target, frag):
    if re.match(r'^[a-z][a-z0-9+.-]*:', target):
        return
    if not target:
        tp = f
    else:
        base = ROOT if target.startswith('/') else os.path.dirname(f)
        tp = os.path.normpath(os.path.join(base, target.lstrip('/')))
    inside = os.path.relpath(tp, ROOT)
    if inside.startswith('..') or inside.split(os.sep)[0] in SKIP_DIRS:
        rep.error(where, f'[link_outside] link {target} leads outside the public repository')
    elif not os.path.exists(tp):
        rep.error(where, f'[link_broken] broken link {target}')
    elif frag and frag != '#' and tp.endswith('.md') and os.path.isfile(tp) and frag[1:] not in anchors(tp):
        rep.error(where, f'[link_anchor] {rel(tp)} has no anchor {frag}')


def path_exists(p, f):
    """A path from the root or from the folder of the file; a fixture name without extension also works."""
    p = re.sub(r'(#.*|:\d+(?:-\d+)?)$', '', p).rstrip('.,;:)')
    for full in (os.path.join(ROOT, p), os.path.join(os.path.dirname(f), p)):
        if os.path.exists(full) or any(os.path.exists(full.rstrip('/') + e) for e in ('.yaml', '.yml', '.json', '.md')):
            return True
    return False


cited = collections.Counter()
for f in walk():
    rp = rel(f)
    try:
        text = read(f)
    except UnicodeDecodeError:
        continue  # public_safety.py checks non-text files
    if rp != 'LICENSE':
        for n, line in enumerate(text.split('\n'), 1):
            if EM_DASH in line:
                rep.error(f'{rp}:{n}', '[em_dash] em dash: skeleton texts use a colon, a comma or a new sentence')
    if not rp.endswith(('.md', '.yaml', '.yml', '.json')):
        continue
    if rp.endswith('.json'):
        try:
            json.loads(text, object_pairs_hook=no_dup_json)
        except ValueError as e:
            rep.error(rp, f'[json_invalid] the JSON cannot be read: {e}')
        continue
    if rp.endswith(('.yaml', '.yml')):
        try:
            load_yaml_all(f)
        except yaml.YAMLError as e:
            rep.error(rp, f'[yaml_invalid] the YAML cannot be read: {" ".join(str(e).split())[:240]}')
    gen = rp in GENERATED or rp == REGISTRY
    in_retired_list = rp in ('conformance/retired.md', REGISTRY)
    prose = text.split(MARK)[0]
    if rp.endswith('.md') and not rp.startswith('conformance/'):
        for m in ID_RE.finditer(prose):
            cited[m.group(0)] += 1
    in_code = False
    for n, line in enumerate(text.split('\n'), 1):
        where = f'{rp}:{n}'
        fence = line.lstrip().startswith(('```', '~~~'))
        if fence:
            in_code = not in_code
        if re.search(r'\bW-[A-Z]{3,5}\b', line):
            rep.error(where, '[work_area_name] name of a draft work area (W-...) in public text')
        if not gen:
            for m in ANY_ID.finditer(line):
                rid = m.group(0)
                if m.group(1) in NOT_RULES:
                    continue
                if rid not in status:
                    rep.error(where, f'[rule_unknown] rule number {rid} is not found in the registry')
                elif status[rid] == 'retired' and not in_retired_list:
                    rep.error(where, f'[rule_retired] reference to the retired number {rid}')
        for m in DEC_RE.finditer(line):
            if int(m.group(1)) not in decisions:
                rep.error(where, f'[decision_unknown] decision {m.group(0)} is not found in docs/decisions.md')
        if not rp.endswith('.md') or in_code or fence:
            continue
        code_free = re.sub(r'`[^`]*`', '``', line)
        for m in LINK.finditer(code_free):
            check_link(where, f, m.group(1), m.group(2))
        m = REFDEF.match(code_free)
        if m:
            check_link(where, f, m.group(1), m.group(2))
        if not gen:
            for m in TICK_PATH.finditer(line):
                p = m.group(1)
                if m.group(2) or re.search(r'[<>{}*]|\.\.\.|…', p):
                    continue
                if not path_exists(p, f):
                    rep.error(where, f'[path_missing] path {p} does not exist: create the file, drop the path '
                              'or mark it `path` (v0.N) or `path` (planned ...)')
        if not rp.startswith('spec/ports/'):
            for m in re.finditer(r'\b([PCE][1-5])\s+`([a-z_]+(?:\.[a-z_]+)*)`', line):
                known = ports_ops.get(m.group(1), set()) | ports_ops.get('ANY', set())
                op = m.group(2)
                if not any(o == op or o.startswith(op + '.') for o in known):
                    rep.error(where, f'[port_op_unknown] operation {m.group(1)} {op} is not described in spec/ports')

for rid in sorted((i for i, s in status.items() if s == 'active' and not cited[i]), key=id_key):
    rep.error(REGISTRY, f'[rule_unexplained] rule {rid} is not explained in any document')
# ---------------------------------------------------------------- system names in rules (D-66)
VENDORS = ('Telegram', 'Zabbix', 'PostgreSQL', 'systemd', 'polkit', 'Anthropic')
VENDOR_RE = re.compile(r'(?<!\w)(' + '|'.join(VENDORS) + r')(?!\w)', re.I)
for r in rows:
    if r.get('status') == 'active' and isinstance(r.get('text'), str):
        for name in sorted({m.group(1) for m in VENDOR_RE.finditer(r['text'])}):
            rep.error(f'{REGISTRY} {r["id"]}', f'[vendor_in_rule] the rule text names the system {name}: a rule talks about a port '
                      'or a kind of system, the name lives in the binding sheet (D-66)')

rep.finish(limit=400)
