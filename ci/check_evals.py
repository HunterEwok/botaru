#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Eval cases, thresholds and stand data.

- every file evals/cases/<role>/<id>.yaml passes the schema evals/case.schema.json;
- a check with tools_required also has tools_allowed, which carries it (EVO-8);
- the case id is unique and equals the file name, the folder is a role from the stand data;
- the rule numbers in rules are active (a rule version later than the case version is a note);
- base_case and twin refer to existing cases, twins refer to each other;
- pending appears only on a case of the next skeleton version;
- from, chat, the generator data.gen and the references answers#/... exist in the stand data
  evals/fixtures/seed.yaml, the person in from is a member of the chat chat;
- evals/thresholds.yaml holds a security threshold of zero and thresholds.budget;
- the catalog evals/catalog.yaml has one entry per skeleton case file, with the same rules,
  a matching version and the same blocking, and only active rules.

  python3 ci/check_evals.py                  the skeleton cases in evals/cases/
  python3 ci/check_evals.py --cases <folder> [--seed <own seed.yaml>] [--manifests <manifest folder>]
      own cases (<folder>/<role>/<id>.yaml) on top of the skeleton cases:
      the own seed extends the skeleton stand data (a person, chat or generator with the same name replaces
      the training one, roles are added), the folder role comes from roles of both seeds or from the Role
      manifests of the --manifests folder; base_case and twin may refer to skeleton cases; coverage of the
      INJ-12 outputs is required only from the skeleton cases.
"""
import json
import os
import re
import sys

import yaml

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import ROOT, Report, load_json, load_yaml, load_yaml_all, referable, rel, schema_registry, walk

from jsonschema import Draft202012Validator, FormatChecker

rep = Report('eval')
EVALS = os.path.join(ROOT, 'evals')
SKELETON_CASES = os.path.join(EVALS, 'cases')
CASES = SKELETON_CASES
OWN_SEED = OWN_MANIFESTS = None


def _arg(name, kind):
    if name not in sys.argv:
        return None
    i = sys.argv.index(name)
    v = os.path.abspath(sys.argv[i + 1]) if i + 1 < len(sys.argv) else ''
    if not (os.path.isdir(v) if kind == 'dir' else os.path.isfile(v)):
        rep.error(v or name, f'[cases_dir_missing] {name} needs an existing {"folder" if kind == "dir" else "file"}')
        rep.finish()
    return v


if '--cases' in sys.argv:
    CASES = _arg('--cases', 'dir')
OWN = CASES != SKELETON_CASES
OWN_SEED = _arg('--seed', 'file')
OWN_MANIFESTS = _arg('--manifests', 'dir')
SCHEMA = os.path.join(EVALS, 'case.schema.json')
SEED = os.path.join(EVALS, 'fixtures', 'seed.yaml')
THRESHOLDS = os.path.join(EVALS, 'thresholds.yaml')


def vkey(v):
    m = re.fullmatch(r'v(\d+)\.(\d+)', str(v or ''))
    return (int(m.group(1)), int(m.group(2))) if m else None


# schema registry of the repository: the case schema refers to urn:botaru:schema:common:1
if not os.path.exists(SCHEMA):
    rep.error(rel(SCHEMA), '[case_schema_missing] no case schema')
    rep.finish()
case_schema = load_json(SCHEMA)
validator = Draft202012Validator(case_schema, registry=schema_registry(), format_checker=FormatChecker())

rules = referable()
skeleton = vkey('v' + '.'.join(open(os.path.join(ROOT, 'spec', 'VERSION'), encoding='utf-8').read().strip().split('.')[:2]))

seed = load_yaml(SEED) if os.path.exists(SEED) else None
if not isinstance(seed, dict):
    rep.error(rel(SEED), '[seed_missing] no stand data, or the file is not a mapping')
    seed = {}
principals = {p['name']: p for p in seed.get('principals') or [] if isinstance(p, dict) and 'name' in p}
chats = {c['name'] for c in seed.get('chats') or [] if isinstance(c, dict) and 'name' in c}
generators = dict(seed.get('generators') or {})
roles = set(seed.get('roles') or [])
if OWN_SEED:
    # the own seed extends the skeleton stand data; an entry with the same name replaces the training one
    own_seed = load_yaml(OWN_SEED)
    if not isinstance(own_seed, dict):
        rep.error(os.path.relpath(OWN_SEED), '[seed_missing] the own seed is not a mapping')
        own_seed = {}
    principals.update({p['name']: p for p in own_seed.get('principals') or [] if isinstance(p, dict) and 'name' in p})
    chats |= {c['name'] for c in own_seed.get('chats') or [] if isinstance(c, dict) and 'name' in c}
    generators.update(own_seed.get('generators') or {})
    roles |= set(own_seed.get('roles') or [])
    SEED = OWN_SEED
if OWN_MANIFESTS:
    # roles from the Role manifests of the own folder (the same walk as check_manifests.py --manifests)
    for f in walk(('.yaml', '.yml'), OWN_MANIFESTS):
        if any(x.startswith('.') for x in os.path.relpath(f, OWN_MANIFESTS).split(os.sep)[:-1]):
            continue
        try:
            docs_ = load_yaml_all(f)
        except yaml.YAMLError:
            continue
        for d in docs_:
            if isinstance(d, dict) and d.get('kind') == 'Role' and str(d.get('apiVersion', '')).startswith('botaru/'):
                roles.add(str((d.get('metadata') or {}).get('name')))
for name, p in principals.items():
    for c in p.get('chats') or []:
        if c not in chats:
            rep.error(rel(SEED), f'[seed_chat_unknown] {name}: chat {c} is not described in chats')

# thresholds
th = load_yaml(THRESHOLDS) if os.path.exists(THRESHOLDS) else None
if not isinstance(th, dict):
    rep.error(rel(THRESHOLDS), '[thresholds_missing] no thresholds, or the file is not a mapping')
else:
    if (th.get('security') or {}).get('max_failures') != 0:
        rep.error(rel(THRESHOLDS), '[thresholds_security] security.max_failures must be 0 (INJ-12)')
    budget = th.get('budget') or {}
    for k in ('max_eur_per_run', 'max_eur_per_case'):
        if not isinstance(budget.get(k), (int, float)) or budget.get(k) <= 0:
            rep.error(rel(THRESHOLDS), f'[thresholds_budget] budget.{k} must be a positive number')

cases = {}
skeleton_ids = {}
if OWN:
    # skeleton cases as background: base_case and twin can refer to them, their ids are taken
    for f in walk(('.yaml', '.yml'), SKELETON_CASES):
        try:
            d = load_yaml_all(f)
        except yaml.YAMLError:
            continue
        if len(d) == 1 and isinstance(d[0], dict) and isinstance(d[0].get('id'), str):
            skeleton_ids[d[0]['id']] = (rel(f), d[0])
for f in walk(('.yaml', '.yml'), CASES):
    rp = rel(f) if f.startswith(ROOT + os.sep) else os.path.relpath(f)
    try:
        docs = load_yaml_all(f)
    except yaml.YAMLError as e:
        rep.error(rp, f'[yaml_invalid] the YAML cannot be read: {" ".join(str(e).split())[:200]}')
        continue
    if len(docs) != 1 or not isinstance(docs[0], dict):
        rep.error(rp, '[case_form] a case file holds exactly one case mapping')
        continue
    c = docs[0]
    errs = sorted(validator.iter_errors(c), key=lambda e: list(e.absolute_path))
    for e in errs[:5]:
        where = '/'.join(str(p) for p in e.absolute_path) or '/'
        rep.error(rp, f'[case_schema] case schema: {where}: {e.message[:200]}')
    for i, ch in enumerate(((c.get('expect') or {}).get('checks')) or []):
        if isinstance(ch, dict) and 'tools_required' in ch and 'tools_allowed' not in ch:
            rep.error(rp, f'[case_tools_required] expect/checks/{i}: tools_required is checked only beside tools_allowed')
    cid = c.get('id')
    if not isinstance(cid, str):
        continue
    stem = os.path.splitext(os.path.basename(f))[0]
    if cid != stem:
        rep.error(rp, f'[case_id_file] id {cid} does not match the file name {stem}')
    parts = os.path.relpath(f, CASES).split(os.sep)
    if len(parts) != 2:
        rep.error(rp, '[case_path] a case lives in evals/cases/<role>/<id>.yaml')
    elif roles and parts[0] not in roles:
        rep.error(rp, f'[case_role_unknown] folder {parts[0]} is not a role of the stand data')
    if cid in cases or cid in skeleton_ids:
        rep.error(rp, f'[case_id_duplicate] id {cid} is already used by {(cases.get(cid) or skeleton_ids[cid])[0]}')
        continue
    cases[cid] = (rp, c)
all_cases = {**skeleton_ids, **cases}

for cid, (rp, c) in sorted(cases.items()):
    cv = vkey(c.get('version'))
    if c.get('status') == 'pending' and cv and skeleton and cv <= skeleton:
        rep.error(rp, f'[case_pending] pending appears only on a case of the next version, and the skeleton version is v{skeleton[0]}.{skeleton[1]}')
    for rid in c.get('rules') or []:
        if rid not in rules:
            rep.error(rp, f'[rule_not_active] rule {rid} is not active')
        elif cv and vkey(rules[rid].get('version')) and vkey(rules[rid]['version']) > cv:
            rep.note(rp, f'[case_rule_later] rule {rid} is checked from {rules[rid]["version"]}, but the case runs from {c["version"]}')
    for field in ('base_case', 'twin'):
        ref = c.get(field)
        if ref is not None and ref not in all_cases:
            rep.error(rp, f'[case_ref_missing] {field}: there is no case {ref}')
        if ref == cid:
            rep.error(rp, f'[case_ref_self] {field} refers to the case itself')
    twin = c.get('twin')
    if twin in all_cases and all_cases[twin][1].get('twin') != cid:
        rep.error(rp, f'[case_twin_oneway] twin {twin} does not refer back to {cid}')
    if c.get('kind') == 'injection' and not c.get('base_case') and c.get('status') == 'active':
        rep.note(rp, '[case_base_missing] the attack case has no base_case: the catalog names no case without the attack')
    if c.get('kind') == 'refusal' and not twin and c.get('status') == 'active':
        rep.note(rp, '[case_twin_missing] the refusal has no twin in the catalog')
    if c.get('status') != 'active':
        continue
    who = c.get('from')
    if who != 'system' and who is not None:
        if who not in principals:
            rep.error(rp, f'[seed_person_unknown] from: {who} is not in the stand data')
        chat = c.get('chat')
        if chat is not None:
            if chat not in chats:
                rep.error(rp, f'[seed_chat_unknown] chat: {chat} is not in the stand data')
            elif who in principals and chat not in (principals[who].get('chats') or []):
                rep.error(rp, f'[seed_not_member] {who} is not a member of chat {chat}')
    setup = c.get('setup') if isinstance(c.get('setup'), dict) else {}
    for step in list(setup.get('before') or []) + list(setup.get('then') or []):
        if isinstance(step, dict):
            if step.get('from') not in principals:
                rep.error(rp, f'[seed_person_unknown] setup: person {step.get("from")} is not in the stand data')
            elif step.get('chat') not in (principals[step['from']].get('chats') or []):
                rep.error(rp, f'[seed_not_member] setup: {step["from"]} is not a member of chat {step.get("chat")}')
    gen = (c.get('data') or {}).get('gen')
    if gen is not None and gen not in generators:
        rep.error(rp, f'[seed_generator_unknown] generator {gen} is not described in the stand data')
    text = json.dumps(c.get('expect'), ensure_ascii=False)
    for key in re.findall(r'answers#/([A-Za-z0-9_]+)', text):
        if gen is None:
            rep.error(rp, f'[case_answers_no_gen] reference answers#/{key} without the generator data.gen')
        elif gen in generators and key not in ((generators[gen] or {}).get('answers') or {}):
            rep.error(rp, f'[case_answer_unknown] generator {gen} has no answer {key}')

# The catalog evals/catalog.yaml: one entry per case and per row without a model that the skeleton rules
# name. Every skeleton case file has an entry, and the entry agrees with the file on rules, version and
# blocking. Own cases (--cases) keep their own catalog and are not checked against this one.
BLOCKS = {'yes': True, 'no': False, 'threshold': False, 'without a model': None}
CATALOG = os.path.join(EVALS, 'catalog.yaml')
if not OWN:
    cat = load_yaml(CATALOG) if os.path.exists(CATALOG) else None
    crp = rel(CATALOG)
    entries = {}
    if not isinstance(cat, dict) or not isinstance(cat.get('areas'), list):
        rep.error(crp, '[catalog_form] the catalog is a mapping with a list areas')
        cat = {'areas': []}
    for ai, a in enumerate(cat['areas']):
        if not isinstance(a, dict) or not isinstance(a.get('area'), str) or not isinstance(a.get('cases'), list):
            rep.error(f'{crp}#areas/{ai}', '[catalog_form] an area has a title area and a list cases')
            continue
        for ci, e in enumerate(a['cases']):
            where = f'{crp}#areas/{ai}/cases/{ci}'
            if not isinstance(e, dict) or not isinstance(e.get('case'), str):
                rep.error(where, '[catalog_form] an entry is a mapping with a case name')
                continue
            name = e['case']
            if name in entries:
                rep.error(where, f'[catalog_duplicate] {name} is already in the catalog')
                continue
            entries[name] = e
            for k in e:
                if k not in ('case', 'group', 'rules', 'decisions', 'version', 'block', 'method', 'spec'):
                    rep.error(where, f'[catalog_form] {name}: unknown field {k}')
            rl = e.get('rules')
            if not isinstance(rl, list) or not all(isinstance(x, str) for x in rl):
                rep.error(where, f'[catalog_form] {name}: rules must be a list of rule ids')
                rl = []
            for rid in rl:
                if rid not in rules:
                    rep.error(where, f'[rule_not_active] {name}: rule {rid} is not active')
            if not re.fullmatch(r'(text )?v\d+\.\d+(, run v\d+\.\d+)?', str(e.get('version') or '')):
                rep.error(where, f'[catalog_form] {name}: version must be vN.M or "text vN.M, run vN.M"')
            if 'block' in e and e['block'] not in BLOCKS:
                rep.error(where, f'[catalog_form] {name}: block must be one of {", ".join(BLOCKS)}')
            if ('block' in e) == ('method' in e):
                rep.error(where, f'[catalog_form] {name}: an entry has either block (a case) or method (a row without a model)')
            spec = e.get('spec')
            if not isinstance(spec, dict) or not spec or not all(isinstance(v, str) for v in spec.values()):
                rep.error(where, f'[catalog_form] {name}: spec must be a non-empty mapping of strings')
    for cid, (rp, c) in sorted(cases.items()):
        e = entries.get(cid)
        if e is None:
            rep.error(rp, f'[catalog_missing] case {cid} has no entry in {crp}')
            continue
        if sorted(set(e.get('rules') or [])) != sorted(set(c.get('rules') or [])):
            rep.error(rp, f'[catalog_mismatch] rules differ from the catalog entry: {sorted(c.get("rules") or [])} here, {sorted(e.get("rules") or [])} in {crp}')
        # In "text vN.M, run vN.M" the case version is the run version: when the case enters the run.
        cv = re.findall(r'v\d+\.\d+', str(e.get('version') or ''))
        if not cv or c.get('version') != cv[-1]:
            rep.error(rp, f'[catalog_mismatch] version {c.get("version")} is not the catalog version {e.get("version")}')
        want = BLOCKS.get(e.get('block'))
        if want is not None and c.get('blocking') is not want:
            rep.error(rp, f'[catalog_mismatch] blocking: {c.get("blocking")} here, block: {e.get("block")} in the catalog')
    rep.info(f'catalog entries {len(entries)}')

# INJ-12: every model output named in the rule text is covered by at least one active case with INJ-12.
# An output is recognized by words in the summary and expect of a case; output names in backticks in the
# rule text must be in this table, otherwise the table lags behind the rule.
INJ12_OUTPUTS = [  # (output, since version, regex of the sign in a case)
    ('enqueue_child', 'v0.1', r'enqueue_child'),
    ('request_action', 'v0.1', r'request_action'),
    ('links in the answer', 'v0.1', r'link'),
    ('send_message', 'v0.1', r'send_message'),
    ('propose_campaign', 'v0.1', r'propose_campaign'),
    ('script draft', 'v0.1', r'propose_script|script draft|TRN-6'),
    ('memory record proposals', 'v0.1', r'memory'),
    ('monitor_note', 'v0.2', r'monitor_note'),
]
# Outputs of deferred rules are in no version (D-114): monitor_note is MON-7.
INJ12_DEFERRED = {'monitor_note': 'MON-7'}
inj12 = rules.get('INJ-12')
if inj12 and OWN:
    rep.info('coverage of the INJ-12 outputs is required only from the skeleton cases; own cases with INJ-12 are checked as cases')
elif inj12:
    named_out = set(re.findall(r'`([a-z_]+)`', str(inj12.get('text') or '')))
    for name in sorted(named_out - {o[0] for o in INJ12_OUTPUTS}):
        rep.error('conformance/requirements.yaml INJ-12', f'[inj12_output_table] output {name} from the INJ-12 text is not in the output table of check_evals.py')
    inj_cases = [c for rp, c in cases.values() if 'INJ-12' in (c.get('rules') or []) and c.get('status') == 'active']
    for name, since, rx in INJ12_OUTPUTS:
        if (rules.get(INJ12_DEFERRED.get(name)) or {}).get('status') == 'deferred':
            continue
        hit = [c for c in inj_cases if (vkey(c.get('version')) or (0, 0)) <= vkey(since)
               and re.search(rx, json.dumps([c.get('summary'), c.get('rules'), c.get('expect')], ensure_ascii=False), re.I)]
        if not hit:
            if skeleton and vkey(since) <= skeleton:
                rep.error('evals/cases', f'[inj12_output_uncovered] output {name} (since {since}) is not covered by any case with INJ-12')
            else:
                rep.note('evals/cases', f'[inj12_output_later] output {name} (since {since}) enters the eval from its version')
rep.info(f'cases {len(cases)}')
rep.finish()
