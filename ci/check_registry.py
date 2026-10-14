#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Rule registry conformance/requirements.yaml: the form of every row.

An active rule has a text, a goal or constraint from docs/goals.md that it
serves (D-114), a check method, the waivable flag, a version,
a profile and a document; a retired one has a date, a reason and a replacement.
A deferred rule keeps its full row (text, version, profile, document) so it
can come back, and adds the date it was deferred, the reason, the condition
under which it returns and whether its mechanism was removed from the code
and the skeleton; no version checks it and no text may refer to it.
A number appears once. Rules carry no pending mark. Unknown row fields
(a typo such as decison:) are an error. A rule that a later version changes
keeps its row with until (the last version it applies to) and
superseded_by (the active rules of the next versions that replace it), so
an implementation that claims the earlier version is still checked by it.
A rule that ends without a replacement has until, an empty superseded_by and
end_reason.
"""
import datetime
import os
import re
import sys

import yaml

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import CHECKS, PROFILES, REGISTRY, ROOT, VERSIONS, Loader, Report, known_decisions, known_goals, read, registry

ACTIVE_KEYS = {'id', 'title', 'status', 'text', 'goal', 'check', 'waivable', 'version', 'profile', 'level',
               'decisions', 'fixtures', 'doc', 'until', 'superseded_by', 'end_reason'}
RETIRED_KEYS = {'id', 'title', 'status', 'retired', 'reason', 'replaced_by'}
DEFERRED_KEYS = ACTIVE_KEYS - {'until', 'superseded_by', 'end_reason'} | {'deferred', 'reason', 'return_when', 'mechanism_removed'}

rep = Report('registry')
try:
    rows = registry()
except yaml.YAMLError as e:
    rep.error(REGISTRY, f'[yaml_invalid] the registry cannot be read: {str(e)[:300]}')
    rep.finish()
if not isinstance(rows, list):
    rep.error(REGISTRY, '[registry_not_list] the registry must be a list of rows')
    rep.finish()

# file line of every registry row: messages point to requirements.yaml:<line>
lines = []
try:
    node = yaml.compose(read(os.path.join(ROOT, REGISTRY)), Loader=Loader)
    lines = [item.start_mark.line + 1 for item in node.value] if isinstance(node, yaml.SequenceNode) else []
except yaml.YAMLError:
    pass

decisions = known_decisions()
goals = known_goals()
seen = {}
for n, r in enumerate(rows):
    rid = r.get('id') if isinstance(r, dict) else None
    where = f'{REGISTRY}:{lines[n]}' if n < len(lines) else f'{REGISTRY}[{n}]'
    where += f' {rid}'
    if not rid or not re.match(r'^[A-Z]{2,3}-\d+[a-z]?$', str(rid)):
        rep.error(where, '[rule_id_form] a rule number has the form FAMILY-number')
        continue
    if rid in seen:
        rep.error(where, f'[rule_id_duplicate] the number already exists on line {seen[rid]}')
    seen[rid] = where.split(' ')[0]
    if not r.get('title'):
        rep.error(where, '[rule_title_missing] no title')
    status = r.get('status')
    allowed = {'active': ACTIVE_KEYS, 'retired': RETIRED_KEYS, 'deferred': DEFERRED_KEYS}.get(status)
    if allowed is not None:
        extra = sorted(set(r) - allowed)
        if extra:
            rep.error(where, f'[rule_unknown_field] unknown fields {extra}; allowed are {sorted(allowed)}')
    if status in ('active', 'deferred'):
        if not str(r.get('text') or '').strip():
            rep.error(where, '[rule_text_missing] no rule text')
        if r.get('goal') not in goals:
            rep.error(where, f'[rule_goal] goal {r.get("goal")!r} is not a goal or constraint of docs/goals.md '
                      f'{sorted(goals)}: every rule names what it serves (D-114)')
        checks = r.get('check') or []
        if not checks:
            rep.error(where, '[rule_check_missing] no check method: no check, no rule')
        for c in checks:
            if c not in CHECKS:
                rep.error(where, f'[rule_check_unknown] unknown check method {c!r}')
        if not isinstance(r.get('waivable'), bool):
            rep.error(where, '[rule_waivable] waivable must be true or false')
        if r.get('version') not in VERSIONS:
            rep.error(where, f'[rule_version] version {r.get("version")!r} is not one of {VERSIONS}')
        if r.get('profile') not in PROFILES:
            rep.error(where, f'[rule_profile] profile {r.get("profile")!r} is not one of {PROFILES}')
        if r.get('level') not in (None, 'SHOULD'):
            rep.error(where, '[rule_level] the level is written only as SHOULD')
        for d in r.get('decisions') or []:
            m = re.fullmatch(r'D-(\d+)', str(d))
            if not m or int(m.group(1)) not in decisions:
                rep.error(where, f'[decision_unknown] decision {d} is not found in docs/decisions.md')
        fixtures = r.get('fixtures')
        if not isinstance(fixtures, list):
            rep.error(where, '[rule_fixtures_form] fixtures must be a list')
        for fx in fixtures or []:
            if re.search(r'(^|[:/])pending($|/)', str(fx)):
                rep.error(where, '[rule_pending] a pending mark on a version rule is not allowed')
            m = re.match(r'^([a-z]+):', str(fx))
            if m and m.group(1) in CHECKS and m.group(1) not in checks:
                rep.note(where, f'[rule_fixture_method] {fx}: method {m.group(1)} is not in check, the name only links')
        until = r.get('until')
        if until is not None or r.get('superseded_by') is not None:
            if until not in VERSIONS or r.get('version') not in VERSIONS \
                    or VERSIONS.index(until) < VERSIONS.index(r.get('version')) or until == VERSIONS[-1]:
                rep.error(where, f'[rule_until] until {until!r} must be a version from {VERSIONS}, not before the rule version and not the last one')
            if not isinstance(r.get('superseded_by'), list):
                rep.error(where, '[rule_superseded_by] a rule with until names the rules that replace it in superseded_by, '
                          'an empty list if it ends without a replacement')
            elif not r.get('superseded_by') and not str(r.get('end_reason') or '').strip():
                rep.error(where, '[rule_end_reason] a rule that ends without a replacement says why in end_reason')
        if r.get('end_reason') is not None and (until is None or r.get('superseded_by')):
            rep.error(where, '[rule_end_reason] end_reason is only for a rule with until and an empty superseded_by')
        doc = r.get('doc')
        if not doc or not os.path.exists(os.path.join(ROOT, doc)):
            rep.error(where, f'[rule_doc_missing] document {doc!r} not found')
        if status == 'deferred':
            if not isinstance(r.get('deferred'), (datetime.date, str)) or not r.get('deferred'):
                rep.error(where, '[deferred_date] the deferred rule has no date in deferred')
            for k in ('reason', 'return_when'):
                if not str(r.get(k) or '').strip():
                    rep.error(where, f'[deferred_{k}] the deferred rule has no {k}')
            if not isinstance(r.get('mechanism_removed'), bool):
                rep.error(where, '[deferred_mechanism] mechanism_removed must be true or false: '
                          'whether the mechanism of the rule was removed from the code and the skeleton')
    elif status == 'retired':
        if not isinstance(r.get('retired'), (datetime.date, str)) or not r.get('retired'):
            rep.error(where, '[retired_date] the retired number has no date')
        if not str(r.get('reason') or '').strip():
            rep.error(where, '[retired_reason] the retired number has no reason')
        if not isinstance(r.get('replaced_by'), list):
            rep.error(where, '[retired_replaced_by] replaced_by must be a list, empty if there is no replacement')
    else:
        rep.error(where, f'[rule_status] status {status!r} is not active, deferred or retired')

active = {r['id'] for r in rows if isinstance(r, dict) and r.get('status') == 'active'}
by_id = {r['id']: r for r in rows if isinstance(r, dict) and r.get('status') == 'active' and isinstance(r.get('id'), str)}
for r in by_id.values():
    until = r.get('until')
    for x in r.get('superseded_by') or []:
        t = by_id.get(x)
        if t is None:
            rep.error(f'{REGISTRY} {r["id"]}', f'[rule_superseded_by] replacement {x} is not an active rule')
        elif until in VERSIONS and t.get('version') in VERSIONS and VERSIONS.index(t['version']) <= VERSIONS.index(until):
            rep.error(f'{REGISTRY} {r["id"]}', f'[rule_superseded_by] replacement {x} is of version {t["version"]}, not later than until {until}')
        elif t.get('until') is not None and t.get('until') == until:
            rep.error(f'{REGISTRY} {r["id"]}', f'[rule_superseded_by] replacement {x} ends in the same version')
for r in rows:
    if isinstance(r, dict) and r.get('status') == 'retired':
        for x in r.get('replaced_by') or []:
            if x not in active:
                rep.error(f'{REGISTRY} {r.get("id")}', f'[retired_replacement] replacement {x} is not an active rule')
rep.finish()
