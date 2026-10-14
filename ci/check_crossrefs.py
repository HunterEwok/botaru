#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Cross-file checks: the operation map, registries, the event catalog, behaviour fixtures.

- the operation map spec/ports/operations.yaml: form, the request and response schemas exist,
  an empty schema only with a reason, rules are active (or deferred with the mechanism kept), every operation of a port sheet is in
  the map, every operation schema is named in the map, the op field of a schema knows the map
  operations, the HTTP binding path is built without substitutions;
- a schema leaf with x-botaru-registry and enum: every enum value is among the entry ids of
  the registry (with x-botaru-registry-kind only among the entries of that kind);
- the alarm field in spec/model/incident-classes.yaml and spec/model/holds.yaml names an
  event type from spec/bindings/events/catalog.yaml;
- YAML of fixtures and manifests: a flow-style mapping has no key without a value
  (a comma inside {...} without quotes gives a key with null);
- behaviour fixtures (except schemas/, manifests/, state-machine/) pass the schema
  conformance/fixtures/fixture.schema.json; a call step names a map operation,
  stand.loop names a loop of spec/ports/background-loops.md, expect.reason names
  a code of spec/model/reasons.yaml;
- the call.request body of a behaviour fixture passes the request schema of its map row
  (lead decision 11, [fixture_body_invalid]); the allowances of conformance/runner.md
  are listed in common.call_body_errors;
- the body of a stand step, as the runner builds it by the stand table of conformance/runner.md
  (common.stand_body), passes the request schema of the map row T.<op>;
- the profiles field of a map row is a non-empty list of profiles; for a row with rules
  it equals the union of the profiles of its rules;
- every delivery kind name in the "Delivery kinds" table of the E1 sheet is in
  spec/model/delivery-kinds.yaml or spec/model/decision-points.yaml (schema field
  names, enum values, decision subjects and tenant names do not count);
- the code text catalog spec/bindings/channels/messages/en.yaml has the form {locale, texts},
  its locale is in the closed list of common.schema.json, and it holds every key that rule
  DLV-9 and the key columns of the tables in docs/messaging.md name (DLV-9, DLV-18);
- the metrics catalog spec/bindings/telemetry/metrics.yaml: every metric has an instrument
  kind, a unit and labels only from its label_sets, and no label names a person (OBS-13);
- note [schema_orphan]: a spec/model/ schema without a single reference
  by $id, by file name or from the operation map.
"""
import collections
import os
import re
import sys

import yaml
from jsonschema import Draft202012Validator

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import PROFILES, ROOT, VERSIONS, Loader, Report, _expand_short, call_body_errors, stand_body_errors, iter_nodes, load_json, load_yaml, md_cells, read, referable, registry, rel, schema_registry, schemas, walk

rep = Report('cross-checks')
MODEL = os.path.join(ROOT, 'spec', 'model')
PORTS = os.path.join(ROOT, 'spec', 'ports')
OPS_FILE = 'spec/ports/operations.yaml'
PORT_NAMES = ('P1', 'P2', 'P3', 'P4', 'C1', 'C2', 'C3', 'C4', 'E1', 'E2', 'E3', 'E4', 'E5', 'T')
SINCE = VERSIONS
active = set(referable())


def schema_file(name):
    return os.path.join(MODEL, f'{name}.schema.json')


# ---------------------------------------------------------------- operation map
ops = load_yaml(os.path.join(ROOT, OPS_FILE)) or {}
entries = []
if not isinstance(ops, dict) or ops.get('registry') != 'operations' or not {'version', 'description', 'entries'} <= set(ops):
    rep.error(OPS_FILE, '[ops_form] the operation map has the form {registry: operations, version, description, entries}')
else:
    entries = [e for e in ops['entries'] if isinstance(e, dict)]
op_ids = set()
by_schema = collections.defaultdict(list)  # request schema -> rows
named = set()
for n, e in enumerate(entries):
    eid, port, op = e.get('id'), e.get('port'), str(e.get('op') or '')
    where = f'{OPS_FILE} {eid or n}'
    if eid in op_ids:
        rep.error(where, '[ops_id_duplicate] the map row id already exists')
    op_ids.add(eid)
    if port not in PORT_NAMES:
        rep.error(where, f'[ops_port_unknown] port {port!r} is not one of {PORT_NAMES}')
    if eid != f'{port}.{op}':
        rep.error(where, f'[ops_id_form] the id must be <port>.<op>: {port}.{op}')
    if '*' in op and e.get('wire') is not False:
        rep.error(where, '[ops_group_wire] an operation group with * is allowed only with wire: false')
    if e.get('since') not in SINCE:
        rep.error(where, f'[ops_since] since {e.get("since")!r} is not one of {SINCE}')
    for side in ('request', 'response'):
        name = e.get(side)
        if name is None:
            if e.get('since') == 'v0.1' and e.get('wire') is not False and not e.get('note'):
                rep.error(where, f'[ops_schema_null] {side}: null on a v0.1 operation without note: name the schema or the reason')
            continue
        named.add(name)
        if not os.path.exists(schema_file(name)):
            rep.error(where, f'[ops_schema_missing] there is no schema spec/model/{name}.schema.json')
    for rid in e.get('rules') or []:
        if rid not in active:
            rep.error(where, f'[rule_not_active] rule {rid} is not active or does not exist')
    if port in ('P1', 'P2', 'T'):
        if not re.fullmatch(r'[a-z][a-z0-9_.]*', op):
            rep.error(where, f'[ops_path_form] the path /v1/<port>/{op} is built only from [a-z0-9_.]')
        if e.get('request'):
            by_schema[e['request']].append(e)

# the op field of a shared request schema knows all operations if it knows at least one
for name, rows in by_schema.items():
    if not os.path.exists(schema_file(name)) or name == 'p2-control-apply-request':
        continue
    doc = load_json(schema_file(name))
    vals = set()
    for _, node in iter_nodes(doc):
        prop = (node.get('properties') or {}).get('op') if isinstance(node.get('properties'), dict) else None
        if isinstance(prop, dict):
            vals |= set(prop.get('enum') or []) | ({prop['const']} if 'const' in prop else set())
    mine = {r['op'] for r in rows}
    if vals & mine:
        for op in sorted(mine - vals):
            rep.error(f'spec/model/{name}.schema.json', f'[ops_op_enum] the op field does not know the map operation {op}')

# every operation schema is named in the map
for f in walk(('.schema.json',), MODEL):
    base = os.path.basename(f)[:-len('.schema.json')]
    if re.match(r'^(p[1-4]|e4|test)-', base) and base not in named:
        rep.error(rel(f), '[ops_schema_orphan] the operation schema is not named in any row of the operation map')

# every operation of a sheet is in the map
SHEETS = {'p1-': 'P1', 'p2-': 'P2', 'e1-': 'E1', 'e2-': 'E2', 'e4-': 'E4', 'e5-': 'E5', 'test-control': 'T'}
TICK = re.compile(r'`([^`]+)`')
n_sheet_ops = 0
for f in walk(('.md',), PORTS):
    base = os.path.basename(f)
    port = next((p for k, p in SHEETS.items() if base.startswith(k)), None)
    if not port:
        continue
    lines = read(f).split('\n')
    in_ops = op_table = False
    for i, line in enumerate(lines, 1):
        if line.startswith('## '):
            in_ops = line.strip() == '## Operations'
            continue
        if not line.startswith('|'):
            op_table = False
            continue
        if not in_ops or line.startswith('|---'):
            continue
        cell = md_cells(line)[0]
        if not op_table:
            op_table = cell == 'Operation'  # only operation tables, not field tables
            continue
        if '->' in cell:
            continue  # "input -> P1 `submit_activity`": the row names operations of another port
        head = cell.split('(')[0]  # words in parentheses are not operations
        names = []
        for part in head.split(','):
            m = TICK.match(part.strip())
            if m:
                names.append(m.group(1).split()[0])
        for name in names:
            if not re.fullmatch(r'[a-z][a-z0-9_.*]*', name):
                continue
            n_sheet_ops += 1
            if name.endswith('*') and any(x.startswith(f'{port}.{name[:-1]}') for x in op_ids):
                continue  # group `invite.*`: the map lists its operations by name
            if f'{port}.{name}' not in op_ids:
                rep.error(f'{rel(f)}:{i}', f'[ops_sheet_missing] operation {port} {name} from the sheet is not in the operation map')


# ---------------------------------------------------------------- registries and enum
def registry_entries(name):
    p = os.path.join(MODEL, f'{name}.yaml')
    doc = load_yaml(p) if os.path.exists(p) else None
    return [e for e in (doc or {}).get('entries', []) if isinstance(e, dict)] if isinstance(doc, dict) else None


n_enum = 0
for f in walk(('.schema.json',), os.path.join(ROOT, 'spec')):
    try:
        doc = load_json(f)
    except ValueError:
        continue
    for path, node in iter_nodes(doc):
        reg, enum = node.get('x-botaru-registry'), node.get('enum')
        if not reg or not isinstance(enum, list):
            continue
        ents = registry_entries(reg)
        if ents is None:
            continue  # check_schemas.py reports an unknown registry
        n_enum += 1
        kind = node.get('x-botaru-registry-kind')
        ids = {e.get('id') for e in ents if kind is None or e.get('kind') == kind}
        field = path.rsplit('/', 1)[-1]
        own = next((e for e in ents if e.get('id') == field and isinstance(e.get('values'), list)), None)
        if own is not None:  # a registry where an entry is a field and the values are in its values (runtime-deviations)
            ids = {x.get('value') for x in own['values'] if isinstance(x, dict)}
        for v in enum:
            if v not in ids:
                rep.error(f'{rel(f)}#{path}', f'[registry_enum_unknown] enum value {v!r} is not in the registry {reg}'
                          + (f' among the entries of kind: {kind}' if kind else ''))

# ---------------------------------------------------------------- alarms and the event catalog
catalog = load_yaml(os.path.join(ROOT, 'spec', 'bindings', 'events', 'catalog.yaml')) or {}
event_types = {e.get('id') for e in catalog.get('entries', []) if isinstance(e, dict)}
for reg in ('incident-classes', 'holds'):
    for e in registry_entries(reg) or []:
        alarm = e.get('alarm')
        if alarm and alarm not in event_types:
            rep.error(f'spec/model/{reg}.yaml {e.get("id")}',
                      f'[alarm_not_in_catalog] alarm {alarm} is not in spec/bindings/events/catalog.yaml')


# ---------------------------------------------------------------- empty YAML keys
def null_keys(node, out):
    if isinstance(node, yaml.MappingNode):
        for k, v in node.value:
            if (node.flow_style and isinstance(v, yaml.ScalarNode) and v.tag == 'tag:yaml.org,2002:null'
                    and v.value == '' and isinstance(k, yaml.ScalarNode)):
                out.append((k.start_mark.line + 1, k.value))
            null_keys(v, out)
    elif isinstance(node, yaml.SequenceNode):
        for v in node.value:
            null_keys(v, out)


for root in (os.path.join(ROOT, 'conformance', 'fixtures'), os.path.join(ROOT, 'examples')):
    for f in walk(('.yaml', '.yml'), root):
        try:
            nodes = list(yaml.compose_all(read(f), Loader=Loader))
        except yaml.YAMLError:
            continue  # check_fixtures.py and check_text.py report the read error
        found = []
        for nd in nodes:
            null_keys(nd, found)
        for line, key in found:
            rep.error(f'{rel(f)}:{line}', f'[yaml_null_key] key {key[:40]!r} without a value in {{...}}: '
                      'a comma inside a flow mapping without quotes; quote the value')

# ---------------------------------------------------------------- behaviour fixtures
FX = os.path.join(ROOT, 'conformance', 'fixtures')
OWN_FORM = ('schemas', 'manifests', 'state-machine')
fx_schema = load_json(os.path.join(FX, 'fixture.schema.json'))
fx_validator = Draft202012Validator(fx_schema, registry=schema_registry())
loops_text = read(os.path.join(PORTS, 'background-loops.md'))
loops = set()
for line in loops_text.split('\n'):
    if line.startswith('|') and not line.startswith('|---'):
        m = TICK.match(md_cells(line)[0])
        if m:
            loops.add(m.group(1))
reasons = {e.get('id') for e in registry_entries('reasons') or []}
op_names = collections.defaultdict(set)
for e in entries:
    op_names[e.get('port')].add(e.get('op'))
def record_errors(doc):
    """[(path, message)]: fields of expect.task.record that are not in task-record.schema.json or fail its property."""
    out = []
    sid = 'urn:botaru:schema:task-record:1'
    if sid not in schemas():
        return out
    props = schemas()[sid][1].get('properties', {})
    for path, node in iter_nodes(doc):
        exp = node.get('expect')
        task = exp.get('task') if isinstance(exp, dict) else None
        rec = task.get('record') if isinstance(task, dict) else None
        if not isinstance(rec, dict):
            continue
        for k, v in rec.items():
            if k not in props:
                out.append((f'{path}/expect/task/record/{k}', f'field {k} is not in task-record.schema.json'))
                continue
            v_ = Draft202012Validator({'$ref': f'{sid}#/properties/{k}'}, registry=schema_registry())
            err = next(iter(v_.iter_errors(_expand_short(v))), None)
            if err is not None:
                out.append((f'{path}/expect/task/record/{k}', f'task-record#/properties/{k}: {err.message[:160]}'))
    return out


def body_checks(rp, d):
    global n_body_bad
    for at, oid, msg in call_body_errors(d):
        n_body_bad += 1
        rep.error(f'{rp}#{at}', f'[fixture_body_invalid] the body of {oid} fails the request schema: {msg}')
    for at, oid, msg in stand_body_errors(d):
        n_body_bad += 1
        rep.error(f'{rp}#{at}', f'[fixture_body_invalid] the body of stand step {oid} fails the request schema: {msg}')
    for at, msg in record_errors(d):
        n_body_bad += 1
        rep.error(f'{rp}#{at}', f'[fixture_body_invalid] the expected task record does not follow the schema: {msg}')


n_fx = 0
n_body_bad = 0
for f in walk(('.yaml', '.yml'), FX):
    rp = rel(f)
    if rp.split('/')[2] in OWN_FORM:
        continue
    try:
        docs = list(yaml.load_all(read(f), Loader=Loader))
    except yaml.YAMLError:
        continue
    if len(docs) != 1 or not isinstance(docs[0], dict):
        continue  # check_fixtures.py reports the file form
    n_fx += 1
    d = docs[0]
    for err in sorted(fx_validator.iter_errors(d), key=lambda x: list(x.absolute_path))[:5]:
        at = '/'.join(str(x) for x in err.absolute_path)
        rep.error(f'{rp}#/{at}', f'[fixture_form] the fixture envelope fails fixture.schema.json: {err.message[:200]}')
    for path, node in iter_nodes(d):
        call = node.get('call')
        if isinstance(call, dict) and isinstance(call.get('port'), str):
            port = call['port'].upper()
            op = call.get('op') or call.get('method')
            if port in op_names and isinstance(op, str) and op not in op_names[port] \
                    and not any('*' in x and op.startswith(x.rstrip('*')) for x in op_names[port]):
                rep.error(f'{rp}#{path}/call', f'[fixture_op_unknown] operation {port} {op} is not in the operation map')
        stand = node.get('stand')
        if isinstance(stand, dict) and isinstance(stand.get('loop'), str) and stand['loop'] not in loops:
            rep.error(f'{rp}#{path}/stand', f'[fixture_loop_unknown] loop {stand["loop"]} is not in spec/ports/background-loops.md')
        exp = node.get('expect')
        if isinstance(exp, dict) and isinstance(exp.get('reason'), str):
            code = exp['reason'].split('(')[0]
            if re.fullmatch(r'[a-z][a-z0-9_]*', code) and code not in reasons:
                rep.error(f'{rp}#{path}/expect', f'[fixture_reason_unknown] reason {code} is not in spec/model/reasons.yaml')

    body_checks(rp, d)


# the reference hello-bot sequence in the same envelope: step bodies and the expected task record
SEQ = os.path.join(ROOT, 'examples', 'hello-bot', 'sequence.yaml')
if os.path.exists(SEQ):
    body_checks(rel(SEQ), yaml.load(read(SEQ), Loader=Loader))


# ---------------------------------------------------------------- profiles of map rows
prof_of = {i: r.get('profile') for i, r in referable().items()}
no_prof = []
for e in entries:
    want = sorted({prof_of[x] for x in e.get('rules') or [] if prof_of.get(x)})
    if 'profiles' not in e:
        no_prof.append(e.get('id'))
    elif not isinstance(e.get('profiles'), list) or not e['profiles'] or not set(e['profiles']) <= set(PROFILES):
        rep.error(f'{OPS_FILE} {e.get("id")}', f'[ops_profiles] profiles is a non-empty list of profiles from {PROFILES}')
    elif want and sorted(e['profiles']) != want:  # a row without rules: the map sets the profile
        rep.error(f'{OPS_FILE} {e.get("id")}', f'[ops_profiles] profiles {e.get("profiles")} is not equal to the profiles of the row rules {want}')
if no_prof:
    rep.error(OPS_FILE, f'[ops_profiles_missing] {len(no_prof)} map rows have no profiles field (the union of the profiles of the row rules), '
              f'for example {", ".join(map(str, no_prof[:5]))}')

# ---------------------------------------------------------------- delivery kinds of the E1 sheet
e1 = os.path.join(PORTS, 'e1-channel.md')
n_kinds = 0
if os.path.exists(e1):
    kinds = {e.get('id') for reg in ('delivery-kinds', 'decision-points') for e in registry_entries(reg) or []}
    other = {e.get('id') for e in registry_entries('decision-subjects') or []}
    other |= {os.path.basename(x) for x in (os.path.join(ROOT, 'examples', 'tenants', t) for t in
              (os.listdir(os.path.join(ROOT, 'examples', 'tenants')) if os.path.isdir(os.path.join(ROOT, 'examples', 'tenants')) else []))}
    other |= {'farm', 'farmctl'}
    for _, (_, sdoc) in schemas().items():
        for _, node in iter_nodes(sdoc):
            if isinstance(node.get('properties'), dict):
                other |= set(node['properties'])
            if isinstance(node.get('enum'), list) and node.get('x-botaru-registry') not in ('delivery-kinds', 'decision-points'):
                other |= {v for v in node['enum'] if isinstance(v, str)}
            if isinstance(node.get('const'), str):
                other.add(node['const'])
    lines = read(e1).split('\n')
    inside = False
    for i, line in enumerate(lines, 1):
        if line.startswith('**Delivery kinds.**'):
            inside = True
            continue
        if inside and line.startswith('|') and not line.startswith('|---'):
            cells = md_cells(line)
            if cells[0] == 'Version':
                continue
            for name in TICK.findall(' '.join(cells[1:])):
                n_kinds += name in kinds
                if re.fullmatch(r'[a-z][a-z0-9_]*', name) and name not in kinds and name not in other:
                    rep.error(f'{rel(e1)}:{i}', f'[delivery_kind_unknown] delivery kind {name} is neither in spec/model/delivery-kinds.yaml '
                              'nor in spec/model/decision-points.yaml')
        elif inside and line.startswith('**'):
            break

# ---------------------------------------------------------------- code text catalog (DLV-9, DLV-18)
CATALOG = os.path.join(ROOT, 'spec', 'bindings', 'channels', 'messages', 'en.yaml')
KEY_RE = re.compile(r'`((?:cmd|notice|card|number)\.[a-z_.]*[a-z_])`')
if not os.path.exists(CATALOG):
    rep.error('spec/bindings/channels/messages', '[catalog_missing] the en code text catalog is missing (DLV-9)')
else:
    cat = load_yaml(CATALOG) or {}
    texts_en = cat.get('texts') if isinstance(cat, dict) else None
    locales = load_json(os.path.join(MODEL, 'common.schema.json')).get('$defs', {}).get('locale', {}).get('enum', [])
    if not isinstance(texts_en, dict) or cat.get('locale') != 'en' or 'en' not in locales:
        rep.error(rel(CATALOG), '[catalog_form] the catalog is {locale: en, texts: {key: text}} and en is in the locale list')
        texts_en = {}
    for k, v in texts_en.items():
        if not (isinstance(v, str) or (isinstance(v, list) and all(isinstance(x, str) for x in v))):
            rep.error(rel(CATALOG), f'[catalog_value] {k}: a text is a string or a list of strings')
    wanted = {}
    dlv9 = next((r for r in registry() if r.get('id') == 'DLV-9'), {})
    for k in KEY_RE.findall(dlv9.get('text') or ''):
        wanted.setdefault(k, 'conformance/requirements.yaml DLV-9')
    msg = os.path.join(ROOT, 'docs', 'messaging.md')
    for i, line in enumerate(read(msg).split('\n'), 1):
        if not line.startswith('|') or line.startswith('|---'):
            continue
        cells = md_cells(line)
        if cells and cells[0] in ('Command', 'Key'):
            continue
        cols = cells[-1:] if len(cells) == 5 and line.count('`cmd.') else cells[:1]
        for c in cols:
            for k in KEY_RE.findall(c):
                wanted.setdefault(k, f'docs/messaging.md:{i}')
    for k, where in sorted(wanted.items()):
        if k not in texts_en and not any(t.startswith(k + '.') for t in texts_en):
            rep.error(rel(CATALOG), f'[catalog_key_missing] key {k} named in {where} is not in the en catalog (DLV-9)')

# ---------------------------------------------------------------- metrics catalog (OBS-13)
METRICS = os.path.join(ROOT, 'spec', 'bindings', 'telemetry', 'metrics.yaml')
mcat = load_yaml(METRICS) or {}
label_sets = set(mcat.get('label_sets') or [])
for e in mcat.get('entries') or []:
    where = f'{rel(METRICS)} {e.get("id")}'
    if e.get('instrument') not in ('counter', 'gauge', 'histogram'):
        rep.error(where, '[metric_instrument] instrument is counter, gauge or histogram (OBS-13)')
    if not e.get('unit'):
        rep.error(where, '[metric_unit] a metric has a unit (OBS-13)')
    labels = e.get('labels')
    if not isinstance(labels, list) or not set(labels) <= label_sets:
        rep.error(where, f'[metric_label] labels must be a list from label_sets {sorted(label_sets)} (OBS-1, OBS-13)')

# ---------------------------------------------------------------- schemas without references
texts = {}
for f in walk(('.md', '.yaml', '.yml', '.json')):
    if not rel(f).startswith(('ci/', '_review/')):
        texts[f] = read(f)
everything = '\n'.join(texts.values())
op_schema_names = {e.get(side) for e in entries for side in ('request', 'response')}
n_orphan = 0
for f in walk(('.schema.json',), MODEL):
    base = os.path.basename(f)[:-len('.schema.json')]
    keys = (f'{base}.schema.json', f'urn:botaru:schema:{base}:')
    own = texts.get(f, '')
    if base in op_schema_names or any(everything.count(k) > own.count(k) for k in keys):
        continue
    n_orphan += 1
    rep.note(rel(f), '[schema_orphan] the schema has no reference by $id, by file name or from the operation map: '
             'name it in a port sheet, in the operation map or in the event catalog')

rep.info(f'operation map rows {len(entries)}, sheet operations {n_sheet_ops}, enum leaves with a registry {n_enum}, '
         f'behaviour fixtures {n_fx} (call bodies off schema {n_body_bad}), event types {len(event_types)}, E1 delivery kinds {n_kinds}, orphan schemas {n_orphan}')
rep.finish(limit=1000)
