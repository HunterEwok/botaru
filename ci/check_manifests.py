#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Manifest lint: the same checks farmctl apply runs, over the files in git.

- every YAML in examples/ with an apiVersion field is a manifest in the envelope
  {apiVersion, kind, metadata, spec}: the kind comes from spec/manifests/kinds.yaml,
  the envelope and spec pass the schemas in spec/manifests/ (MAN-1), there is no
  status field, and only tenant kinds have metadata.tenant (TEN-6);
- schema leaves inside manifests (params_schema, result_schema, args,
  params, output_schema, fields_schema) are closed per INJ-1, and every x-botaru-format
  is in spec/model/formats.yaml; catalog values pass their format
  (MAN-10);
- group capabilities have no wildcards on actions, data and decisions (PRM-7), no
  grants on action.* types (APR-5), plus the non-waivable PRM-2 bans and the checks
  from the lint:manifests/* and lint:roles/profile-limits rows of docs/quality.md
  that can be checked from files;
- flows and the hello-bot example (YAML with a steps field): P1 request bodies (and the activity
  of a submit_activity step) pass the request schema of the row in spec/ports/operations.yaml,
  call: {port, op, request} step bodies pass the same check as behavior
  fixtures (common.call_body_errors), P3 call arguments and action
  requests pass the closed manifest schemas with catalog lists
  substituted, and calls listed under denied fail the schema;
- fixtures conformance/fixtures/manifests/*.yaml: every case in cases has
  an expected result expect (ok or {error: code, at: path, also: [codes]});
  the lint runs itself against them: if the expected code is missing at path at, or other
  errors not in also appear, that is a fixture error;
- a YAML in examples/ without apiVersion and without steps is an error (except data in
  NOT_MANIFESTS); the folder examples/deploy/ is the deployment description.

Error codes are stable: fixtures refer to them. The code list is in
ci/lint_codes.py, the table in ci/README.md (python3 ci/gen_codes.py).

  python3 ci/check_manifests.py                        skeleton examples and fixtures
  python3 ci/check_manifests.py --manifests <dir>      own manifests on top of the examples
  python3 ci/check_manifests.py --manifests <dir> --driver-schemas <driver schema dir>
      driver schemas of own channels and sinks: driver-config meta-schema and spec.config (MAN-4)
  python3 ci/check_manifests.py --manifests <dir> --tools-list [<plugin>=]tools.json
      compare the tools/list dump of your own MCP server with tool.yaml (INJ-2, INJ-3);
      without --manifests the plugin from examples/ is compared; the option can repeat
"""
import datetime
import json
import os
import re
import sys

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import (PROFILES, ROOT, Report, call_body_errors, expects_schema_reject, iter_nodes, load_json, load_yaml,
                    load_yaml_all, ops_by_name, read, rel, rules, schema_registry, walk)
from common import schemas as all_schemas
from lint_codes import CODES

import yaml
from jsonschema import Draft202012Validator
from jsonschema.exceptions import best_match

rep = Report('manifests')
MODEL = os.path.join(ROOT, 'spec', 'model')
EXAMPLES = os.path.join(ROOT, 'examples')
DEPLOY = os.path.join(EXAMPLES, 'deploy') + os.sep
# YAML in examples/ that are neither manifests nor flows: data of the tutorial examples.
NOT_MANIFESTS = {'examples/hello-bot/mock-model.yaml'}
FIXTURES = os.path.join(ROOT, 'conformance', 'fixtures', 'manifests')
API_VERSION = 'botaru/v1alpha1'


def norm(x):
    """Turns YAML dates and times into strings, the way apply sees them."""
    if isinstance(x, dict):
        return {str(k): norm(v) for k, v in x.items()}
    if isinstance(x, list):
        return [norm(v) for v in x]
    if isinstance(x, datetime.datetime):
        return x.strftime('%Y-%m-%dT%H:%M:%SZ')
    if isinstance(x, datetime.date):
        return x.isoformat()
    return x


def model_registry(name):
    p = os.path.join(MODEL, f'{name}.yaml')
    if not os.path.exists(p):
        return {}
    doc = load_yaml(p) or {}
    return {e['id']: e for e in doc.get('entries', []) if isinstance(e, dict) and 'id' in e}


# ---------------------------------------------------------------- schemas and registries
schemas = {sid: d for sid, (p, d) in all_schemas().items() if p.startswith('spec/')}
REGISTRY = schema_registry()
_validators = {}
_err_cache = {}


def validator(schema):
    key = json.dumps(schema, sort_keys=True)
    if key not in _validators:
        _validators[key] = Draft202012Validator(schema, registry=REGISTRY)
    return _validators[key]


def errors_of(schema, data, limit=3):
    """Schema errors; the result is cached by content (fixtures with examples run the same manifests many times)."""
    key = (json.dumps(schema, sort_keys=True), json.dumps(data, sort_keys=True, default=str), limit)
    if key not in _err_cache:
        _err_cache[key] = _errors_of(schema, data, limit)
    return list(_err_cache[key])


def _errors_of(schema, data, limit):
    errs = sorted(validator(schema).iter_errors(data), key=lambda e: (len(list(e.absolute_path)), e.message))
    out = []
    for e in errs[:limit]:
        while e.context:
            e = best_match(e.context)
        path = '/'.join(str(p) for p in e.absolute_path) or '/'
        out.append(f'{path}: {e.message[:200]}')
    return out


FORMATS = {k: re.compile(v['pattern']) for k, v in model_registry('formats').items() if v.get('pattern')}
KINDS = {}
_kinds_path = os.path.join(ROOT, 'spec', 'manifests', 'kinds.yaml')
if os.path.exists(_kinds_path):
    KINDS = {e['id']: e for e in (load_yaml(_kinds_path) or {}).get('entries', [])}
CAPS = model_registry('capabilities')
CAP_FORMS = {e['id']: e for e in CAPS.values() if e.get('kind') == 'capability'}
DATA_CLASSES = {k: e for k, e in CAPS.items() if e.get('kind') == 'data_class'}
ROLE_PROFILES = model_registry('role-profiles')
PURPOSES = model_registry('task-purposes')
FAMILIES = model_registry('model-families')
FORBIDDEN_PROGRAMS = set(model_registry('forbidden-programs'))
REQUIREMENTS = rules()
# Built-in channel commands: commands cannot take them (PRM-17, docs/task-language.md).
BUILTIN_COMMANDS = {'/help', '/about', '/lang', '/mydata', '/tasks', '/status', '/cancel', '/me', '/team',
                    '/run', '/service'}
# Farm ceilings (docs/tasks-and-delivery.md, "Deadlines and ceilings"; DLV-16).
CEILINGS = {'lease': 120, 'heartbeat': 30, 'tool_start_by': 30}
# Decision and management capabilities: system does not have them (PRM-10), nor do delegated_groups (PRM-16).
DECISION_CAPS = {'action.approve', 'proposal.decide', 'control.approve', 'control.apply', 'hold.set', 'hold.release',
                 'script.run', 'incident.manage', 'manager.direct', 'audit.read', 'governance.confirm',
                 'governance.attribute', 'proposal.override_veto', 'model_grant.manage', 'task.takeover'}
REVERT_REASONS = {'harm_observed', 'superseded', 'other'}  # PRP-22 via MAN-5
DUR = re.compile(r'^([1-9][0-9]{0,5})(s|m|h|d)$')
DUR_S = {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}


def seconds(d):
    m = DUR.match(str(d or ''))
    return int(m.group(1)) * DUR_S[m.group(2)] if m else None


def skeleton_version():
    """(major, minor) of the skeleton version from spec/VERSION."""
    try:
        parts = read(os.path.join(ROOT, 'spec', 'VERSION')).strip().split('.')
        return int(parts[0]), int(parts[1])
    except (OSError, ValueError, IndexError):
        return (0, 1)


def transfer_covered(entries, provider, region):
    """Whether the tenant has a transfer basis for the provider and region (Tenant.spec.model_transfer, D-87 (2))."""
    for e in entries or []:
        if not isinstance(e, dict):
            continue
        p, r = e.get('provider'), e.get('region')
        if (p == provider and r in (None, region)) or (p is None and r == region):
            return True
    return False


def non_eu_regions():
    """{provider: regions outside the EU} from the "Region" section of the notes spec/bindings/model-providers/<provider>.md.

    A region outside the EU is a row of the section table whose name does not start with eu (D-86 (6), GW-14).
    """
    out = {}
    d = os.path.join(ROOT, 'spec', 'bindings', 'model-providers')
    for fn in sorted(os.listdir(d)) if os.path.isdir(d) else []:
        if not fn.endswith('.md') or fn in ('README.md', 'mock.md'):
            continue
        inside, regs = False, set()
        for line in read(os.path.join(d, fn)).split('\n'):
            if line.startswith('## '):
                inside = line.strip() == '## Region'
                continue
            if inside and line.startswith('| `'):
                name = line.split('`')[1]
                if not name.startswith('eu'):
                    regs.add(name)
        out[fn[:-3]] = regs
    return out


NON_EU = non_eu_regions()


def farm_hosts_default():
    p = os.path.join(EXAMPLES, 'deploy', 'farm.yaml')
    if os.path.exists(p):
        for doc in load_yaml_all(p):
            if isinstance(doc, dict):
                spec = doc.get('spec', doc)
                if isinstance(spec, dict) and spec.get('farm_hosts'):
                    return list(spec['farm_hosts']), rel(p)
    return ['core1', 'backup1'], None


# ---------------------------------------------------------------- manifest set
class Findings:
    def __init__(self):
        self.items = []  # (level, code, where, msg)

    def error(self, code, where, msg):
        assert code in CODES, f'code {code} is not described in ci/lint_codes.py'
        self.items.append(('error', code, where, msg))

    def note(self, code, where, msg):
        assert code in CODES, f'code {code} is not described in ci/lint_codes.py'
        self.items.append(('note', code, where, msg))

    def codes(self, level='error'):
        return {c for lv, c, _, _ in self.items if lv == level}


walk_nodes = iter_nodes


def has_key(node, key):
    return any(key in n for _, n in walk_nodes(node))


def strings(node):
    if isinstance(node, str):
        yield node
    elif isinstance(node, dict):
        for k, v in node.items():
            yield str(k)
            yield from strings(v)
    elif isinstance(node, list):
        for v in node:
            yield from strings(v)


def enum_from_lists(node):
    return {n['enum_from'] for _, n in walk_nodes(node) if isinstance(n.get('enum_from'), str)}


def free_text(node):
    """Whether the output schema has a string without enum, const and format (INJ-3)."""
    for _, n in walk_nodes(node):
        if n.get('type') == 'string' and 'maxLength' in n and not any(
                k in n for k in ('enum', 'enum_from', 'const', 'x-botaru-format')):
            return True
    return False


class Set:
    """Declared objects of one apply set."""

    def __init__(self, docs):
        self.docs = docs  # [(where, doc)]
        self.objs = {}  # (kind, tenant, name) -> (where, spec)
        self.by_kind = {}

    def add(self, kind, tenant, name, where, spec, f):
        key = (kind, tenant, name)
        if key in self.objs:
            f.error('duplicate_object', where, f'{kind} {name} is already declared in {self.objs[key][0]}')
            return
        self.objs[key] = (where, spec)
        self.by_kind.setdefault(kind, []).append((tenant, name, where, spec))

    def get(self, kind, name, tenant=None):
        for t, n, w, s in self.by_kind.get(kind, []):
            if n == name and (tenant is None or t in (tenant, None)):
                return s
        return None

    def names(self, kind, tenant=None):
        return {n for t, n, _, _ in self.by_kind.get(kind, []) if tenant is None or t in (tenant, None)}

    def each(self, kind):
        return self.by_kind.get(kind, [])


def parse_cap(cap):
    """Parses a capability string: (form name, argument) or (None, None)."""
    if cap in CAP_FORMS and '<' not in (CAP_FORMS[cap].get('form') or '<'):
        return cap, None
    if ':' in cap:
        base, arg = cap.split(':', 1)
        if base in CAP_FORMS and '<' in (CAP_FORMS[base].get('form') or ''):
            return base, arg
    if cap in CAP_FORMS:
        return cap, None
    return None, None


def lint(docs, today=None, farm_hosts=None, examples_mode=False, claimed=None, skills=None):
    """Apply checks over a set; returns Findings. claimed: the claimed conformance profiles of the
    installation (conforms_to.profiles), None when unknown. skills: {name: {path: text}} of the skills/ folder."""
    f = Findings()
    today = today or datetime.date.today()
    farm_hosts = farm_hosts if farm_hosts is not None else farm_hosts_default()[0]
    S = Set(docs)
    # 1. Envelope, kind, schema (MAN-1, TEN-6).
    for where, doc in docs:
        if not isinstance(doc, dict):
            f.error('schema_invalid', where, 'manifest is not an object')
            continue
        if 'status' in doc:
            f.error('status_in_git', where, 'there is no status field in git: the core keeps status (MAN-1)')
        kind = doc.get('kind')
        if kind not in KINDS:
            f.error('unknown_kind', where, f'kind {kind!r} has no registered schema (MAN-1)')
            continue
        if doc.get('apiVersion') != API_VERSION:
            f.error('api_version', where, f'apiVersion {doc.get("apiVersion")!r}, expected {API_VERSION}')
            continue
        meta = doc.get('metadata') if isinstance(doc.get('metadata'), dict) else {}
        scope = KINDS[kind].get('scope')
        tenant = meta.get('tenant')
        if scope == 'tenant' and not tenant:
            f.error('tenant_missing', where, f'kind {kind} requires metadata.tenant')
            continue
        if scope in ('global', 'self') and tenant:
            f.error('tenant_on_global', where, f'kind {kind} is global: it never has metadata.tenant (TEN-6)')
            continue
        sname = KINDS[kind]['schema'].split(':')[3].replace('-', '_')
        errs = errors_of({'$ref': f'urn:botaru:schema:manifest:1#/$defs/kind_{sname}'}, doc)
        if errs:
            f.error('schema_invalid', where, f'{kind} {meta.get("name")}: ' + '; '.join(errs))
            continue
        if kind == 'Tenant':
            tenant = meta['name']
        S.add(kind, tenant, meta['name'], where, doc['spec'], f)
        if has_key(doc['spec'], 'max_tokens'):
            f.error('max_tokens_field', where, 'manifests have no max_tokens field (BUD-9)')
    tenants = S.names('Tenant')
    # 2. Leaf formats inside manifests (INJ-1).
    for where, doc in docs:
        if not isinstance(doc, dict):
            continue
        for path, n in walk_nodes(doc.get('spec')):
            fmt = n.get('x-botaru-format')
            if fmt is not None and fmt not in FORMATS:
                f.error('format_unknown', f'{where}#{path}', f'format {fmt!r} is not in formats.yaml (INJ-1)')
    types = {n: s for _, n, _, s in S.each('TaskType')}
    tools = {}
    for _, pname, pwhere, ps in S.each('ToolPlugin'):
        for t in ps.get('tools', []):
            tools[f'{pname}/{t["name"]}'] = (pname, ps, t, pwhere)
    actions = {n: s for _, n, _, s in S.each('Action')}
    profiles = {n: s for _, n, _, s in S.each('ModelProfile')}
    roles = {n: (w, s) for _, n, w, s in S.each('Role')}

    # 3. Catalogs: values, services, farm hosts, machines of one tenant.
    catalogs = {}
    host_owner = {}
    for tenant, name, where, cs in S.each('Catalog'):
        lists = {'hosts': [h['id'] for h in cs.get('hosts', [])],
                 'services': [s['name'] for h in cs.get('hosts', []) for s in h.get('services', [])],
                 'monitor_triggers': [t['triggerid'] for t in cs.get('monitor_triggers', [])]}
        for r in cs.get('resources', []):
            fmt = r.get('format', 'id')
            if fmt not in FORMATS:
                f.error('format_unknown', where, f'resource {r["id"]}: format {fmt!r} is not in formats.yaml (MAN-10)')
            else:
                for v in r.get('values', []):
                    if not FORMATS[fmt].fullmatch(str(v)):
                        f.error('catalog_value_format', where,
                                f'resource {r["id"]}: value {v!r} does not pass format {fmt} (MAN-10)')
            if r.get('list'):
                lists.setdefault(r['list'], []).extend(str(v) for v in r.get('values', []))
            if r.get('host') in farm_hosts:
                f.error('farm_host_in_catalog', where, f'resource {r["id"]} is on farm host {r["host"]} (DEP-11)')
            if r.get('host') and r['host'] not in lists['hosts']:
                f.error('ref_missing', where, f'resource {r["id"]}: machine {r["host"]} is not in the catalog hosts')
        catalogs[tenant] = lists
        for h in cs.get('hosts', []):
            mon = h.get('monitor_host')
            ids = {h['id']} | ({mon, mon.split('.')[0]} if mon else set())
            if ids & set(farm_hosts):
                f.error('farm_host_in_catalog', where, f'machine {h["id"]} or host {mon} is a farm host (DEP-11)')
            for key in ids:
                other = host_owner.get(key)
                if other and other != tenant:
                    f.error('host_two_tenants', where, f'{key} is already in the catalog of tenant {other} (TEN-4, D-75)')
                host_owner[key] = tenant
            scripts = {s['name'] for s in h.get('monitor_scripts_allowed', [])}
            for svc in h.get('services', []):
                nc = svc.get('no_card', ['restart'])
                if any(v != 'restart' for v in nc):
                    f.error('service_no_card', where, f'service {svc["name"]}: no_card allows only restart (MON-8)')
                if svc['via'] == 'host' and not svc.get('unit'):
                    f.error('service_unit_missing', where, f'service {svc["name"]}: via: host without unit (MON-8)')
                if svc['via'] == 'monitor':
                    if svc.get('unit'):
                        f.error('service_unit_missing', where, f'service {svc["name"]}: unit only with via: host')
                    for verb in svc['control']:
                        sc = (svc.get('scripts') or {}).get(verb)
                        if not sc or sc not in scripts:
                            f.error('service_script_unknown', where,
                                    f'service {svc["name"]}: {verb} has no script from monitor_scripts_allowed (MON-8, MON-10)')
    # a (machine, service) pair belongs to one tenant only
    svc_owner = {}
    for tenant, _, where, cs in S.each('Catalog'):
        for h in cs.get('hosts', []):
            for svc in h.get('services', []):
                k = (h['id'], svc['name'])
                if k in svc_owner and svc_owner[k] != tenant:
                    f.error('host_two_tenants', where, f'service {svc["name"]} on {h["id"]} already belongs to {svc_owner[k]} (D-75)')
                svc_owner[k] = tenant
    all_lists = {}
    for lists in catalogs.values():
        for k, v in lists.items():
            all_lists.setdefault(k, set()).update(v)

    def check_lists(where, node):
        for ef in enum_from_lists(node):
            src, _, lname = ef.partition('.')
            if src == 'catalog' and catalogs and lname not in all_lists:
                f.note('enum_from_unknown_list', where, f'list {ef} is in no catalog of the set')

    # 4. Groups: capabilities (PRM-7, APR-5, PRM-2).
    groups = {}
    for tenant, name, where, gs in S.each('Group'):
        groups[(tenant, name)] = gs
        for cap in gs.get('caps', []):
            base, arg = parse_cap(cap)
            if base is None:
                f.error('cap_unknown', where, f'group {name}: capability {cap!r} is not in capabilities.yaml')
                continue
            if '*' in cap:
                ok = base == 'task' and re.fullmatch(r'[a-z][a-z0-9_]*(\.[a-z0-9_]+)*\.\*', arg or '') \
                    and not (arg or '').startswith('action.') and name != 'system-ops'
                if not ok:
                    f.error('cap_star', where, f'group {name}: wildcard in {cap!r} (PRM-7)')
                    continue
            if base == 'task' and (arg or '').startswith('action.'):
                f.error('action_type_grant', where, f'group {name}: grant {cap!r} on an action.* type (APR-5)')
            if base == 'task' and arg and '*' not in arg and types and arg not in types:
                f.note('ref_missing', where, f'group {name}: type {arg} is not in the set')
            if base == 'data':
                dc = DATA_CLASSES.get(arg)
                if not dc or dc.get('reserved'):
                    f.error('cap_data_class', where, f'group {name}: data class {arg!r} is not accepted')
            if base in ('action.request', 'action.approve') and arg not in actions \
                    and arg not in ('service.control', 'monitor.script_run'):
                f.error('cap_unknown_action', where, f'group {name}: action {arg!r} is not in the set')
            if name == 'system-ops' and base in DECISION_CAPS:
                f.error('system_decision_cap', where, f'group system-ops: system has no decision capabilities (PRM-10)')
        for d in gs.get('data', []):
            if d not in DATA_CLASSES or DATA_CLASSES[d].get('reserved'):
                f.error('cap_data_class', where, f'group {name}: data class {d!r} is not accepted')
        for inh in gs.get('inherits', []):
            if not S.get('Group', inh, tenant):
                f.error('ref_missing', where, f'group {name}: inherited group {inh} does not exist')

    def eff_caps(tenant, gname, seen=()):
        g = groups.get((tenant, gname)) or {}
        caps, data = set(g.get('caps', [])), set(g.get('data', []))
        for inh in g.get('inherits', []):
            if inh not in seen:
                c2, d2 = eff_caps(tenant, inh, seen + (gname,))
                caps |= c2
                data |= d2
        return caps, data

    # 5. Tenants.
    for _, tname, where, ts in S.each('Tenant'):
        if ts.get('kind', 'production') == 'production' and ts.get('self_approve') is True:
            f.error('self_approve_production', where, 'production tenant with self_approve: true (APR-9)')
        for cmd, tt in (ts.get('commands') or {}).items():
            if cmd in BUILTIN_COMMANDS:
                f.error('builtin_command', where, f'command {cmd} is built in and cannot be taken (PRM-17)')
            if tt.startswith('action.'):
                f.error('action_type_grant', where, f'command {cmd} sets an action.* type (APR-5)')
            elif types and tt not in types:
                f.error('ref_missing', where, f'command {cmd}: type {tt} is not in the set')
        assistant_types = {t for w, r in roles.values() if r.get('memory') == 'person' for t in r.get('claims', [])}
        for dt in ts.get('default_types', []):
            t = dt if isinstance(dt, str) else dt.get('type')
            if t in assistant_types:
                f.error('assistant_in_default_types', where, f'assistant type {t} in default_types (DSP-10)')
            elif types and t not in types:
                f.error('ref_missing', where, f'default_types: type {t} is not in the set')
        for rname, r in (ts.get('run') or {}).items():
            if 'type' in r:
                tt = types.get(r['type'])
                if tt is None:
                    f.error('ref_missing', where, f'run {rname}: type {r["type"]} is not in the set')
                    continue
                if (tt.get('run') or {}).get('approval') != 'none':
                    f.error('run_needs_approval_none', where,
                            f'run {rname}: type {r["type"]} without run.approval: none; a pipeline that writes is an action (APR-17)')
                props = (tt.get('params_schema') or {}).get('properties', {})
            else:
                act = actions.get(r['action'])
                if act is None:
                    f.error('ref_missing', where, f'run {rname}: action {r["action"]} is not in the set')
                    continue
                props = (act.get('args') or {}).get('properties', {})
            for a in r.get('args', []):
                if a not in props:
                    f.error('run_arg_unknown', where, f'run {rname}: argument {a} is not in the closed schema (APR-17)')
        for gname in ts.get('delegated_groups', []):
            if (tname, gname) not in groups:
                f.error('ref_missing', where, f'delegated_groups: group {gname} does not exist')
                continue
            caps, data = eff_caps(tname, gname)
            bad = []
            for cap in caps:
                base, arg = parse_cap(cap)
                if base in ('action.request', 'service.control', 'monitor.script_run') or base in DECISION_CAPS \
                        or (base or '').startswith('admin') or cap.startswith('admin'):
                    bad.append(cap)
                elif base == 'task' and arg in types and (types[arg].get('tier', 0) >= 3 or
                                                          DATA_CLASSES.get(types[arg].get('data_class'), {}).get('order', 0) > 1):
                    bad.append(cap)
            bad += [f'data:{d}' for d in data if (DATA_CLASSES.get(d) or {}).get('order', 9) > 1]
            if bad:
                f.error('delegated_group_privileged', where,
                        f'delegated_groups: group {gname} carries {sorted(bad)} (PRM-16)')
        fm = ts.get('farm_manager') or {}
        if fm.get('enabled', True):
            mp = fm.get('model_profile')
            if mp and mp not in ts.get('models_allowed', []):
                f.error('manager_profile_not_allowed', where,
                        f'manager seat profile {mp} is not in models_allowed (MGR-20)')
            line = ((ts.get('budgets') or {}).get('lines') or {}).get('manager') or {}
            if not any((line.get(k) or 0) > 0 for k in ('month_eur', 'day_eur')):
                f.error('manager_line_missing', where, 'manager budget line is not above 0 while the manager is enabled (MGR-20)')
        priv = ts.get('privacy') or {}
        # monitoring is a mandatory recipient only when the installation claims the monitor profile (D-88)
        if 'monitor' in (claimed or []) and 'monitoring' not in priv.get('recipients', ['monitoring']):
            f.error('recipients_monitoring', where, 'privacy.recipients has no monitoring while the monitor profile is claimed (MON-1)')
        subs = {s['provider']: s for s in priv.get('subprocessors', [])}
        for mp in ts.get('models_allowed', []):
            prof = profiles.get(mp)
            if prof is None:
                if profiles:
                    f.error('ref_missing', where, f'models_allowed: profile {mp} is not in the set')
                continue
            if (prof.get('provider_training') or {}).get('value') is True:
                f.error('provider_training', where, f'profile {mp}: provider_training true (GW-15)')
            sub = subs.get(prof['provider'])
            if subs and sub is None:
                f.error('subprocessor_undeclared', where, f'profile {mp}: provider {prof["provider"]} is not in privacy.subprocessors (GW-15)')
            for ps in prof.get('subprocessors', []):
                if subs and ps['provider'] not in subs:
                    f.error('subprocessor_undeclared', where,
                            f'profile {mp}: subprocessor {ps["provider"]} is not in privacy.subprocessors (GW-15)')
            if (sub and ts.get('kind', 'production') == 'production'
                    and sub.get('region') in NON_EU.get(prof['provider'], set())
                    and not transfer_covered(ts.get('model_transfer', []), prof['provider'], sub.get('region'))):
                f.error('transfer_basis_missing', where,
                        f'profile {mp}: provider {prof["provider"]} with region {sub.get("region")} outside the EU for a production tenant '
                        'without a spec.model_transfer record (GW-14, D-87 (2))')
            if sub:
                hr = (prof.get('provider_human_review') or {}).get('value')
                if hr == 'yes' and (sub.get('terms') or {}).get('human_review') in ('none', 'abuse_only'):
                    f.error('provider_human_review', where, f'profile {mp}: provider_human_review yes contrary to terms (GW-15)')
                ret = sub.get('retention') or {}
                days = (prof.get('provider_retention_days') or {}).get('value')
                if ret.get('mode') == 'zero' and prof.get('requires_retention'):
                    f.error('provider_retention', where, f'profile {mp}: requires_retention with mode: zero (GW-15)')
                if ret.get('mode') == 'standard' and days is not None and days > ret.get('max_days', 0):
                    f.error('provider_retention', where, f'profile {mp}: retention of {days} days exceeds max_days (GW-15)')

    # 6. Task types.
    for _, tname, where, ts in S.each('TaskType'):
        if tname.startswith('action.'):
            f.error('action_type_grant', where, f'type {tname}: only the core sets action.* names (APR-5)')
        if ts['purpose'] not in PURPOSES:
            f.error('purpose_unknown', where, f'type {tname}: purpose {ts["purpose"]!r} is not in task-purposes.yaml')
        for k, cap in CEILINGS.items():
            s = seconds((ts.get('timeouts') or {}).get(k))
            if s is not None and s > cap:
                f.error('timeout_ceiling', where, f'type {tname}: timeouts.{k} is above the farm ceiling {cap} s')
        alarms = [e for e in ts.get('events', []) if e.startswith('alarm.')]
        if alarms and ts['worker_kind'] != 'job':
            f.error('alarm_not_job', where, f'type {tname}: alarm.* in events only for job (INJ-7)')
        if alarms and (ts.get('inbox') or {}).get('read'):
            f.error('alarm_inbox', where, f'type {tname}: a type with alarm.* does not read the inbox (INJ-7)')
        if ts.get('observes') and ts['worker_kind'] != 'job':
            f.error('observes_not_job', where, f'type {tname}: observes only for job (OBV-3)')
        must_false = ts['purpose'] != 'work' or alarms or ts.get('makes_backup')
        if must_false and ts.get('proposable', True) is not False:
            f.error('proposable_required_false', where, f'type {tname}: proposable: false is required (PRP-10)')
        if (ts.get('run') or {}).get('approval') == 'none':
            for rn, (rw, rs) in roles.items():
                if tname in rs.get('claims', []):
                    writes = [t for t in rs.get('tools', []) if t in tools and tools[t][2].get('access') == 'write']
                    if rs.get('actions_may_request') or writes:
                        f.error('run_not_readonly', where,
                                f'type {tname} with run.approval: none takes role {rn} with actions or write tools (APR-17)')
        check_lists(where, ts.get('params_schema'))

    # 7. Plugins and tools.
    for _, pname, where, ps in S.each('ToolPlugin'):
        for t in ps.get('tools', []):
            if t.get('enabled') is False:
                continue
            tid = f'{pname}/{t["name"]}'
            params = dict(t.get('params') or {})
            if t.get('args_from'):
                act = actions.get(t['args_from'].split(':', 1)[1])
                if t.get('params'):
                    f.error('args_from_second_schema', where, f'{tid}: with args_from there are no own params (MAN-9)')
                if act is None:
                    if actions:
                        f.error('ref_missing', where, f'{tid}: action {t["args_from"]} is not in the set')
                else:
                    params.update((act.get('args') or {}).get('properties', {}))
                    ex = act.get('executor') or {}
                    if (ex.get('plugin'), ex.get('tool')) != (pname, t['name']):
                        f.error('args_from_mismatch', where, f'{tid}: executor of {t["args_from"]} is different (MAN-9)')
            for c in (t.get('commands') or {}).values():
                params.update(c.get('params') or {})
            if bool(t.get('target_from')) == bool(t.get('target')):
                f.error('tool_target_missing', where, f'{tid}: exactly one of target_from and target: any is required (TEN-3a)')
            elif t.get('target_from') and t['target_from'] not in params:
                f.error('tool_target_unknown', where, f'{tid}: target_from {t["target_from"]} is not a tool parameter (TEN-3a)')
            if ps['placement'] == 'host' and t.get('untrusted_output') is True \
                    and not {'ip', 'email', 'secret'} <= set(t.get('redact', [])):
                f.error('redact_required', where, f'{tid}: placement: host and untrusted_output: true without redact [ip, email, secret] (INJ-4)')
            if t.get('untrusted_output') is False and t.get('output_schema') and free_text(t['output_schema']):
                f.error('untrusted_output_false_text', where, f'{tid}: untrusted_output: false with free text in the output (INJ-3)')
            for cname, c in (t.get('commands') or {}).items():
                argv = c['argv']
                if not argv[0].startswith('/'):
                    f.error('cmd_argv_path', where, f'{tid} {cname}: program path is not absolute (INJ-14)')
                if os.path.basename(argv[0]) in FORBIDDEN_PROGRAMS:
                    f.error('cmd_forbidden_program', where, f'{tid} {cname}: {argv[0]} is in forbidden-programs.yaml (INJ-14)')
                for i, a in enumerate(argv):
                    if '{' in a or '}' in a:
                        m = re.fullmatch(r'\{([a-z][a-z0-9_]*)(\.[a-z_]+)?\}', a)
                        if not m or '--' not in argv[:i] or m.group(1) not in params:
                            f.error('cmd_param_position', where,
                                    f'{tid} {cname}: parameter {a!r} is not a whole element after -- or not from params (INJ-14)')
            check_lists(where, t.get('params'))

    # 8. Actions.
    for _, aname, where, a in S.each('Action'):
        routine_fields = ('writes_only', 'runs_per_hour', 'runs_per_day')
        if a['risk'] == 'routine':
            missing = [k for k in routine_fields if k not in a] + ([] if a.get('idempotent') is True else ['idempotent: true'])
            if missing:
                f.error('routine_fields', where, f'action {aname}: risk: routine without {missing} (MAN-9)')
            if a.get('approvals') == 2:
                f.error('routine_fields', where, f'action {aname}: risk: routine with approvals: 2 (MAN-9, APR-10)')
        elif any(k in a for k in routine_fields):
            f.error('routine_fields', where, f'action {aname}: writes_only and counters only with risk: routine (MAN-9)')
        if a.get('approvals') == 2 and a['risk'] != 'routine' and skeleton_version() < (0, 2):
            f.error('not_in_version', where, f'action {aname}: approvals: 2 arrives in v0.2 (APR-10, APR-16)')
        ex = a['executor']
        tool = tools.get(f'{ex["plugin"]}/{ex["tool"]}')
        if tools and tool is None:
            f.error('executor_unknown', where, f'action {aname}: tool {ex["plugin"]}/{ex["tool"]} does not exist')
        elif tool and tool[2].get('access') != 'write':
            f.error('executor_unknown', where, f'action {aname}: executor is not a write tool')
        check_lists(where, a.get('args'))

    # 9. Roles (PRF-1, GOV-3, MEM-2, APR-1, APR-5).
    for rname, (where, rs) in roles.items():
        pname = rs.get('profile', 'worker')
        prof = ROLE_PROFILES.get(pname)
        if prof is None:
            f.error('role_profile_unknown', where, f'role {rname}: profile {pname!r} is not in role-profiles.yaml (PRF-1)')
            continue
        for tt in rs.get('claims', []):
            ts = types.get(tt)
            if ts is None:
                f.note('ref_missing', where, f'role {rname}: type {tt} is not in the set')
                continue
            if ts['purpose'] not in prof['purposes']:
                f.error('role_profile_purpose', where, f'role {rname}: type {tt} with purpose {ts["purpose"]} is outside profile {pname} (PRF-1)')
            if prof.get('max_task_tier') is not None and ts['tier'] > prof['max_task_tier']:
                f.error('role_profile_tier', where, f'role {rname}: type {tt} of tier {ts["tier"]} is above profile {pname} (PRF-1)')
            if ts['worker_kind'] != rs['worker_kind']:
                f.error('worker_kind_mismatch', where, f'role {rname}: type {tt} is for {ts["worker_kind"]}')
        for field in prof.get('must_be_empty', []):
            head, _, sub = field.partition('.')
            v = rs.get(head)
            if sub and isinstance(v, dict):
                v = v.get(sub)
            if v:
                f.error('role_profile_forbidden', where, f'role {rname}: profile {pname} forbids {field} (PRF-1)')
        for field in ('service_control', 'monitor_scripts', 'insight'):
            if rs.get(field) and field not in prof.get('may_declare', []):
                f.error('role_profile_forbidden', where, f'role {rname}: profile {pname} does not declare {field} (PRF-1)')
        mem = rs.get('memory', 'none')
        mem = 'read_bot' if isinstance(mem, dict) else mem
        if mem not in prof.get('memory', []):
            f.error('role_profile_memory', where, f'role {rname}: memory {mem} is outside profile {pname} (PRF-1)')
        for slot in (rs.get('models') or {}):
            if slot not in prof.get('slots', []):
                f.error('role_profile_slot', where, f'role {rname}: slot {slot} is outside profile {pname} (PRF-1)')
        for slot, ceil in (('base', prof.get('max_rank')), ('analysis', prof.get('analysis_max_rank'))):
            mp = (rs.get('models') or {}).get(slot)
            for p in [mp] + list((profiles.get(mp) or {}).get('fallback', [])):
                if p in profiles and ceil is not None and profiles[p]['rank'] > ceil:
                    f.error('rank_exceeded', where, f'role {rname}: profile {p} of rank {profiles[p]["rank"]} is above the ceiling {ceil} of slot {slot} (GOV-3)')
        if rs.get('campaigns') and prof.get('campaigns_propose') and rs['campaigns'].get('propose') not in prof['campaigns_propose']:
            f.error('role_profile_forbidden', where, f'role {rname}: campaigns.propose is outside profile {pname} (PRF-1)')
        if rs.get('scripts') and prof.get('scripts_propose') and rs['scripts'].get('propose') not in prof['scripts_propose']:
            f.error('role_profile_forbidden', where, f'role {rname}: scripts.propose is outside profile {pname} (PRF-1)')
        for tref in rs.get('tools', []):
            if '/' in tref:
                t = tools.get(tref)
                if t is None:
                    if tools:
                        f.error('ref_missing', where, f'role {rname}: tool {tref} is not in the set')
                elif t[2].get('access') != 'read':
                    f.error('role_write_tool', where, f'role {rname}: {tref} is not read; the model has no write tools (APR-1, PRM-2)')
        for tt in rs.get('may_enqueue', []):
            if tt.startswith('action.'):
                f.error('action_type_grant', where, f'role {rname}: action.* in may_enqueue (APR-5)')
        for an in rs.get('actions_may_request', []):
            if actions and an not in actions:
                f.error('ref_missing', where, f'role {rname}: action {an} is not in the set')
        if mem == 'person':
            bad = [tt for tt in rs.get('claims', []) if tt in types and
                   (types[tt]['data_class'] != 'summary' or (types[tt].get('inbox') or {}).get('read'))]
            bad += [t for t in rs.get('tools', []) if t in tools and tools[t][2].get('untrusted_output')]
            if bad:
                f.error('memory_person', where, f'role {rname}: memory: person with {bad} (MEM-2)')

    # 9a. One level of aggregators (AGG-3): no aggregator among the sources, no more than max_sources.
    for rname, (where, rs) in roles.items():
        if rs.get('profile') != 'aggregator':
            continue
        agg = rs.get('aggregate') or {}
        src = agg.get('sources') or {}
        count = sum(len(src.get(k) or []) for k in ('roles', 'bot_groups', 'series', 'message_kinds'))
        if agg.get('max_sources') is not None and count > agg['max_sources']:
            f.error('aggregator_sources_over_max', where,
                    f'role {rname}: {count} source entries, more than max_sources {agg["max_sources"]} (AGG-3)')
        named = [(r, f'sources.roles {r}') for r in src.get('roles') or []]
        for bg in src.get('bot_groups') or []:
            g = S.get('BotGroup', bg) or {}
            named += [(r, f'bot group {bg}') for r in (g.get('select') or {}).get('roles') or []]
        for r, via in named:
            if r in roles and roles[r][1].get('profile') == 'aggregator':
                f.error('aggregator_source_aggregator', where,
                        f'role {rname}: {via} is aggregator role {r}; one level of aggregators (AGG-3)')

    # 9b. Exposure of datasets to the model (PRV-4). A dataset without model_exposure counts as aggregates.
    agent_tools = {t for _, rs in roles.values() if rs.get('worker_kind') == 'agent' for t in rs.get('tools', [])}
    for tenant, cname, where, cs in S.each('Catalog'):
        for r in cs.get('resources', []):
            exp = r.get('model_exposure') or ('aggregates' if r.get('kind') == 'dataset' else None)
            for tref in r.get('read_by', []):
                if exp is None or tref not in agent_tools:
                    continue
                t = (tools.get(tref) or (None, None, {}))[2]
                if exp == 'none':
                    f.error('model_exposure_agent_read', where,
                            f'resource {r["id"]}: model_exposure none, but agent tool {tref} reads it (PRV-4)')
                elif exp == 'aggregates' and (t.get('untrusted_output') is not False
                                              or free_text(t.get('output_schema') or {})):
                    f.error('model_exposure_not_aggregate', where,
                            f'resource {r["id"]}: model_exposure aggregates, but agent tool {tref} has output '
                            'that is not only numbers and enum values (PRV-4)')

    # 10. Routes and schedules.
    def check_address(where, tenant, addr):
        if isinstance(addr, dict):
            an = addr['approval_initiator']['action']
            if actions and an not in actions:
                f.error('ref_missing', where, f'address approval_initiator: action {an} does not exist')
            return
        kind, _, name = addr.partition(':')
        if kind == 'group' and not S.get('Group', name, tenant) and not S.get('BotGroup', name, tenant):
            f.error('ref_missing', where, f'address {addr}: the group is not in tenant {tenant}')
        if kind == 'sink' and not S.get('Sink', name, tenant):
            f.error('ref_missing', where, f'address {addr}: the sink is not in tenant {tenant}')

    def system_has(tenant, tt):
        caps, _ = eff_caps(tenant, 'system-ops')
        return f'task:{tt}' in caps

    for tenant, rname, where, rs in S.each('Route'):
        mode = rs.get('mode', 'task')
        if mode == 'task' and 'task' not in rs:
            f.error('route_mode', where, f'route {rname}: mode: task without task')
        if mode == 'report' and 'report' not in rs:
            f.error('route_mode', where, f'route {rname}: mode: report without report')
        tt = (rs.get('task') or {}).get('type')
        if tt:
            if tt.startswith('action.'):
                f.error('action_type_grant', where, f'route {rname}: action.* type (APR-5)')
            elif types and tt not in types:
                f.error('ref_missing', where, f'route {rname}: type {tt} does not exist')
            elif groups and not system_has(tenant, tt):
                f.error('system_ops_missing', where, f'route {rname}: task:{tt} is not in system-ops (PRM-10)')
            ttype = types.get(tt) or {}
            if 'params' in rs['task'] and ttype:
                for e in errors_of(resolve(ttype['params_schema'], catalogs.get(tenant)), rs['task']['params']):
                    f.error('params_invalid', where, f'route {rname}: params: {e}')
        for addr in rs.get('deliver_to', []):
            check_address(where, tenant, addr)
    for tenant, sname, where, ss in S.each('Schedule'):
        cron = ss['cron']
        if len(cron.split()) != 5 or re.search(r'(CRON_)?TZ\s*=', cron):
            f.error('cron_invalid', where, f'schedule {sname}: cron of five fields without CRON_TZ and TZ (TIM-2)')
        if ('task' in ss) == ('script' in ss):
            f.error('schedule_target', where, f'schedule {sname}: exactly one of task and script (SCR-12)')
            continue
        if 'script' in ss:
            continue  # v0.2: the SCR-12 checks of a scheduled script come with the Script kind
        tt = ss['task']['type']
        if tt.startswith('action.'):
            f.error('action_type_grant', where, f'schedule {sname}: action.* type (APR-5)')
            continue
        ttype = types.get(tt)
        if ttype is None:
            if types:
                f.error('ref_missing', where, f'schedule {sname}: type {tt} does not exist')
            continue
        if not (ttype.get('timeouts') or {}).get('deadline'):
            f.error('schedule_deadline', where, f'schedule {sname}: type {tt} has no timeouts.deadline')
        if groups and not system_has(tenant, tt):
            f.error('system_ops_missing', where, f'schedule {sname}: task:{tt} is not in system-ops (PRM-10)')
        if ttype.get('proposable', True) is False and ss.get('proposable', True) is not False:
            f.error('proposable_required_false', where, f'schedule {sname}: type {tt} is not proposable, proposable: false is required (PRP-10)')
        for e in errors_of(resolve(ttype['params_schema'], catalogs.get(tenant)), ss['task'].get('params', {})):
            f.error('params_invalid', where, f'schedule {sname}: params: {e}')
        for addr in ss.get('deliver_to', []):
            check_address(where, tenant, addr)

    # 11. Message kinds, model profiles, charter, drivers.
    for _, mname, where, ms in S.each('MessageType'):
        if ms['kind'] == 'state' and ms['producers'] != ['code']:
            f.error('state_agent_producer', where, f'kind {mname}: the state family has only code in producers (STA-1, MSG-2)')
        if not mname.startswith(ms['kind'] + '.'):
            f.error('message_name', where, f'kind {mname}: the name starts with family {ms["kind"]} (MSG-2)')
        need = ('delivery', 'ttl') if ms['kind'] == 'report' else ('stale_after',)
        for k in need:
            if k not in ms:
                f.error('message_fields', where, f'kind {mname}: family {ms["kind"]} needs field {k} (MSG-2)')
    for _, pname, where, ps in S.each('ModelProfile'):
        if FAMILIES and ps['family'] not in FAMILIES:
            f.note('family_unregistered', where, f'profile {pname}: family {ps["family"]} is not in model-families.yaml; the deployment adds it')
        checked = (ps.get('price') or {}).get('checked')
        try:
            age = (today - datetime.date.fromisoformat(checked)).days
        except (TypeError, ValueError):
            age = None
        if age is not None and age > 120:
            (f.note if examples_mode else f.error)('price_stale', where, f'profile {pname}: price is {age} days old, more than 120 (GW-17)')
        elif age is not None and age > 60:
            f.note('price_old', where, f'profile {pname}: price is {age} days old, more than 60 (GW-17)')
    # The tree limit of a type with a model covers the reserve of one P4 call (GW-2, BUD-2): output
    # max_output_tokens at the output price of the most expensive profile of the slot for the type purpose
    # (analysis for insight and aggregate, base otherwise; GW-7) and its fallbacks.
    # Input is not in the lower bound, so an error means "not enough even for the output".
    for _, tname, where, ts in S.each('TaskType'):
        budget = ts.get('tree_budget_eur')
        if ts.get('worker_kind') != 'agent' or not isinstance(budget, (int, float)) or isinstance(budget, bool):
            continue
        worst = None
        for rname, (_, rs) in roles.items():
            if tname not in (rs.get('claims') or []):
                continue
            slot = 'analysis' if ts.get('purpose') in ('insight', 'aggregate') else 'base'  # GW-7
            mp = (rs.get('models') or {}).get(slot)
            if mp:
                for p in [mp] + list((profiles.get(mp) or {}).get('fallback') or []):
                    ps = profiles.get(p) or {}
                    price = (ps.get('price') or {}).get('output')
                    if isinstance(ps.get('max_output_tokens'), int) and isinstance(price, (int, float)):
                        cost = ps['max_output_tokens'] * price / 1_000_000
                        if worst is None or cost > worst[0]:
                            worst = (cost, p, rname, slot)
        if worst and budget < worst[0]:
            f.error('tree_budget_below_call', where,
                    f'type {tname}: tree_budget_eur {budget} is below the reserve of one call {worst[0]:.4f} EUR '
                    f'(profile {worst[1]} of slot {worst[3]} of role {worst[2]}, output only; GW-2, BUD-2)')
    for tenant, gname, where, gs in S.each('Governance'):
        com = gs.get('commission') or {}
        ceil = ROLE_PROFILES.get('reviewer', {}).get('max_rank')
        for p in [com.get('model_profile')] + [s.get('model_profile') for s in com.get('seats', [])]:
            if p in profiles and ceil is not None and profiles[p]['rank'] > ceil:
                f.error('rank_exceeded', where, f'charter {gname}: seat profile {p} is above the reviewer ceiling {ceil} (GOV-3)')
    forbidden = set(schemas.get('urn:botaru:schema:driver-config:1', {}).get('$defs', {}).get('forbidden', {}).get('enum', []))
    for kind in ('Channel', 'Sink'):
        for _, name, where, cs in S.each(kind):
            bad = sorted({k for _, n in walk_nodes(cs.get('config')) for k in n if k in forbidden})
            if bad:
                f.error('driver_config_forbidden', where,
                        f'{kind} {name}: spec.config has fields {bad}; they exist only in the common envelope (MAN-4)')

    # 11a. v0.2 checks: two approvers (APR-16), curated corpora (KNW-2), source channels (ING-1),
    # export sinks (SEC-10), skill folders (SKL-1).
    for tenant, tname, where, ts in S.each('Tenant'):
        if ts.get('kind', 'production') != 'production':
            continue
        for aname, a in actions.items():
            if a.get('risk') != 'high':
                continue
            req_caps = [g for (t, g), gs in groups.items() if t == tname and f'action.request:{aname}' in gs.get('caps', [])]
            if a.get('approvals', 1) == 1:
                via_roles = [r for r, (_, rs) in roles.items() if aname in rs.get('actions_may_request', [])]
                if req_caps or via_roles:
                    f.error('high_risk_one_approver', where,
                            f'production tenant {tname}: high risk action {aname} with approvals: 1 can be requested '
                            f'(groups {req_caps}, roles {via_roles}) (APR-16)')
            elif req_caps:
                approvers = [g for (t, g), gs in groups.items() if t == tname and f'action.approve:{aname}' in gs.get('caps', [])]
                if not approvers:
                    f.note('approvers_fewer_than_two', where,
                           f'production tenant {tname}: no group grants action.approve:{aname}; the core counts people at apply '
                           'and doctor warns while fewer than two people other than the initiator can approve (APR-16)')
    for tenant, cname, where, cs in S.each('Catalog'):
        for r in cs.get('resources', []):
            if r.get('kind') == 'corpus' and r.get('trust') == 'curated' and r.get('source', 'git') != 'git':
                f.error('corpus_curated_source', where,
                        f'corpus {r.get("id")}: trust: curated with source: {r.get("source")}; only a git corpus is curated (KNW-2)')
    for tenant, name, where, cs in S.each('Channel'):
        if cs.get('direction') == 'in' and not cs.get('ingress'):
            f.error('ingress_missing', where, f'channel {name}: a source channel (direction: in) without ingress (ING-1)')
    for tenant, name, where, ss in S.each('Sink'):
        eo = ss.get('export_only')
        if isinstance(eo, dict) and ('recipient' in eo) == ('recipient_ref' in eo):
            f.error('export_only_recipient', where,
                    f'sink {name}: export_only needs exactly one of recipient and recipient_ref (SEC-10)')
    for sname, files in (skills or {}).items():
        for _, msg in skill_problems(sname, files):
            f.error('skill_format', f'skills/{sname}/', msg)

    # 12. Global manifests without tenant data (TEN-6).
    for kind, entry in KINDS.items():
        if entry.get('scope') != 'global':
            continue
        for _, name, where, gs in S.each(kind):
            for s in strings(gs):
                if s in tenants or re.search(r'(?<![\w.])(user|staff|service|chat|group):[a-z]', s) \
                        or re.search(r'[\w.+-]+@[\w-]+\.[\w.]+', s):
                    f.error('global_tenant_data', where, f'{kind} {name}: value {s!r} looks like tenant data (TEN-6)')
                    break
    return f


SKILL_TYPES = ('.md', '.txt', '.yaml', '.json', '.csv')
SKILL_TOP = {'SKILL.md', 'references', 'assets'}


def skill_problems(name, files):
    """SKL-1: one skill folder as {relative path: text or bytes}; returns [(code, message)].
    A value None marks a symbolic link, a value with the key 'mode' an executable file."""
    out = []
    if 'SKILL.md' not in files:
        return [('skill_format', f'skill {name}: no SKILL.md (SKL-1)')]
    if len(files) > 50:
        out.append(('skill_format', f'skill {name}: {len(files)} files, at most 50 (SKL-1)'))
    total = 0
    for path, body in sorted(files.items()):
        top = path.split('/')[0]
        if top not in SKILL_TOP:
            out.append(('skill_format', f'skill {name}: {path} is outside SKILL.md, references/ and assets/ (SKL-1)'))
        if body is None or (isinstance(body, dict) and body.get('link')):
            out.append(('skill_format', f'skill {name}: {path} is a symbolic link (SKL-1)'))
            continue
        if isinstance(body, dict) and body.get('executable'):
            out.append(('skill_format', f'skill {name}: {path} is executable (SKL-1)'))
            body = body.get('text', '')
        if not path.endswith(SKILL_TYPES):
            out.append(('skill_format', f'skill {name}: {path} has a type outside {list(SKILL_TYPES)} (SKL-1)'))
        data = body.encode('utf-8') if isinstance(body, str) else body
        if not isinstance(data, bytes):
            continue
        try:
            data.decode('utf-8')
        except UnicodeDecodeError:
            out.append(('skill_format', f'skill {name}: {path} is not UTF-8 text (SKL-1)'))
        total += len(data)
        if path == 'SKILL.md' and len(data) > 32 * 1024:
            out.append(('skill_format', f'skill {name}: SKILL.md is over 32 KiB (SKL-1)'))
    if total > 256 * 1024:
        out.append(('skill_format', f'skill {name}: the tree is over 256 KiB (SKL-1)'))
    text = files['SKILL.md'] if isinstance(files['SKILL.md'], str) else ''
    m = re.match(r'---\n(.*?)\n---\n', text, re.S)
    fm = None
    if m:
        try:
            fm = yaml.safe_load(m.group(1))
        except yaml.YAMLError:
            fm = None
    if not isinstance(fm, dict):
        out.append(('skill_format', f'skill {name}: SKILL.md has no YAML front matter (SKL-1)'))
        return out
    for e in errors_of({'$ref': 'urn:botaru:schema:skill:1'}, fm):
        out.append(('skill_format', f'skill {name}: front matter: {e} (SKL-1)'))
    if fm.get('name') != name:
        out.append(('skill_format', f'skill {name}: front matter name {fm.get("name")!r} differs from the folder name (SKL-1)'))
    return out


def read_skills(root):
    """The skills/ folder under root as {name: {path: text | {'link': True} | {'executable': True, 'text': ...}}}."""
    base = os.path.join(root, 'skills')
    out = {}
    if not os.path.isdir(base):
        return out
    for name in sorted(os.listdir(base)):
        d = os.path.join(base, name)
        if not os.path.isdir(d) or name.startswith('.'):
            continue
        files = {}
        for dp, dns, fns in os.walk(d, followlinks=False):
            for fn in dns + fns:
                full = os.path.join(dp, fn)
                relp = os.path.relpath(full, d).replace(os.sep, '/')
                if os.path.islink(full):
                    files[relp] = {'link': True}
                elif os.path.isfile(full):
                    with open(full, 'rb') as fh:
                        data = fh.read()
                    try:
                        text = data.decode('utf-8')
                    except UnicodeDecodeError:
                        text = data
                    files[relp] = {'executable': True, 'text': text} if os.access(full, os.X_OK) else text
        out[name] = files
    return out


def resolve(schema, lists):
    """Turns a closed manifest schema into a checkable one: enum_from from the catalog, format from the registry."""
    if isinstance(schema, list):
        return [resolve(s, lists) for s in schema]
    if not isinstance(schema, dict):
        return schema
    out = {}
    for k, v in schema.items():
        if k == 'enum_from':
            src, _, name = v.partition('.')
            if src == 'catalog' and lists is not None and name in lists:
                out['enum'] = list(lists[name])
            else:
                out['type'] = 'string'
        elif k == 'x-botaru-format':
            if v in FORMATS:
                out['pattern'] = FORMATS[v].pattern
        elif k in ('properties', '$defs'):
            out[k] = {pk: resolve(pv, lists) for pk, pv in v.items()}
        elif k in ('items', 'additionalProperties', 'anyOf', 'oneOf', 'allOf'):
            out[k] = resolve(v, lists)
        elif k.startswith('x-botaru-') or k == 'default':
            continue
        else:
            out[k] = v
    return out


def closed_params(params, lists):
    return resolve({'type': 'object', 'additionalProperties': False, 'properties': params or {}}, lists)


# ---------------------------------------------------------------- MAN-3, MAN-5, MAN-8
def breaking(before, after, path=''):
    """Breaking differences of a closed schema: removed field, new required field, changed leaf."""
    out = []
    if not isinstance(before, dict) or not isinstance(after, dict):
        return [] if before == after else [path or '/']
    bp, ap = before.get('properties'), after.get('properties')
    if isinstance(bp, dict) or isinstance(ap, dict):
        bp, ap = bp or {}, ap or {}
        out += [f'{path}/{k} removed' for k in bp if k not in ap]
        out += [f'{path}/{k} became required' for k in set(after.get('required', [])) - set(before.get('required', []))]
        for k in bp:
            if k in ap:
                out += breaking(bp[k], ap[k], f'{path}/{k}')
        rest_b = {k: v for k, v in before.items() if k not in ('properties', 'required', 'description', 'title')}
        rest_a = {k: v for k, v in after.items() if k not in ('properties', 'required', 'description', 'title')}
        if rest_b != rest_a:
            out.append(f'{path or "/"} changed')
        return out
    strip = lambda d: {k: v for k, v in d.items() if k not in ('description', 'title')}  # noqa: E731
    return [] if strip(before) == strip(after) else [f'{path or "/"} changed']


def lint_additive(before_docs, after_docs, f):
    """MAN-3: within .vN of a task type and a message kind, only optional fields may be added."""
    old = {(d.get('kind'), d.get('metadata', {}).get('name')): (w, d) for w, d in before_docs if isinstance(d, dict)}
    for where, d in after_docs:
        key = (d.get('kind'), (d.get('metadata') or {}).get('name'))
        if key not in old or key[0] not in ('TaskType', 'MessageType', 'Action'):
            continue
        for field in ('params_schema', 'result_schema', 'fields_schema', 'args'):
            b, a = (old[key][1].get('spec') or {}).get(field), (d.get('spec') or {}).get(field)
            if b is None:
                continue
            diff = breaking(b, a or {})
            if diff:
                f.error('breaking_change', where, f'{key[0]} {key[1]}: {field}: {"; ".join(diff)}; version .v(N+1) is required (MAN-3)')


def lint_commit(commit, f, where):
    """MAN-5: Botaru-Proposal and Botaru-Revert lines in the commit message and the tenant folder boundaries."""
    msg = commit.get('message', '')
    tenant = commit.get('proposal_tenant')
    linked = False
    for line in msg.splitlines():
        m = re.match(r'^Botaru-Revert:\s*(\S+)(?:\s+(\S+))?\s*$', line)
        if line.startswith('Botaru-Revert:'):
            if not m or m.group(2) not in REVERT_REASONS:
                f.error('revert_reason_invalid', where, f'line {line!r}: reason is not in {sorted(REVERT_REASONS)} (MAN-5, PRP-22)')
            linked = True
        if re.match(r'^Botaru-Proposal:\s*\S+\s*$', line):
            linked = True
    if commit.get('link_commit'):
        linked = True
    if linked and tenant:
        for p in commit.get('files', []):
            if not p.startswith(f'tenants/{tenant}/'):
                f.error('proposal_scope', where, f'proposal commit touches {p} outside tenants/{tenant}/ (MAN-5, PRP-8)')


def lint_conforms(case, f, where):
    """MAN-8: conforms_to and the expected failures file."""
    ct = case.get('conforms_to') or {}
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', str(ct.get('skeleton', ''))):
        f.error('conforms_to_invalid', where, 'conforms_to.skeleton is not a skeleton version x.y.z (MAN-8)')
    profs = ct.get('profiles') or []
    if 'core' not in profs or any(p not in PROFILES for p in profs):
        f.error('conforms_to_invalid', where, f'conforms_to.profiles {profs}: core is required, profiles from {PROFILES} (MAN-8)')
    for rid in case.get('expected_failures', []):
        r = REQUIREMENTS.get(rid)
        if r and r.get('waivable') is False:
            f.error('expected_failure_not_waivable', where, f'{rid} with waivable: false in expected failures fails the run (MAN-8)')


# ---------------------------------------------------------------- flows and hello-bot
def check_flow(where, doc, sets, f):
    """Flow or sequence: a step is written with the verb call: {port, op, request}
    of the fixture envelope or in the old form {port, op, request | activity | tool, args}.
    The P1 body is checked by the request schema from the operations map (activity of submit_activity
    too), P3 arguments by the closed schema of the tool or action."""
    tenant = (doc.get('given') or {}).get('tenant', 'demo')
    lists = sets['catalogs'].get(tenant)
    ops = ops_by_name()

    def tool_of(name):
        if name in sets['tools']:
            return sets['tools'][name]
        # model-facing name <plugin>_<tool> (P3 tools/list)
        return next((t for k, t in sets['tools'].items() if k.replace('/', '_', 1) == name), None)

    def norm_step(st):
        c = st.get('call') if isinstance(st.get('call'), dict) else st
        req = c.get('request') if isinstance(c.get('request'), dict) else {}
        out = {'port': str(c.get('port') or '').upper(), 'op': c.get('op') or c.get('method') or '', 'request': c.get('request'),
               'response': c.get('response'), 'legacy': c is st}
        if c is st and 'activity' in st and out['op'] == 'submit_activity' and out['request'] is None:
            out['request'] = st['activity']
        if out['op'] == 'tools/call':
            out['tool'] = c.get('tool') or req.get('name')
            out['args'] = c['args'] if 'args' in c else req.get('arguments', {})
        if out['op'] == 'request_action':
            out['action'] = c.get('action') or req.get('action')
            out['args'] = c['args'] if 'args' in c else req.get('args', {})
        return out

    def args_errors(c):
        if c['op'] == 'tools/call':
            t = tool_of(c.get('tool'))
            if t is None:
                return [f'tool {c.get("tool")} does not exist']
            params = dict(t[2].get('params') or {})
            for cm in (t[2].get('commands') or {}).values():
                params.update(cm.get('params') or {})  # as in lint(): command params are part of the closed schema
            return errors_of(closed_params(params, lists), c.get('args') or {})
        if c['op'] == 'request_action':
            a = sets['actions'].get(c.get('action'))
            if a is None:
                return [f'action {c.get("action")} does not exist']
            return errors_of(resolve(a['args'], lists), c.get('args') or {})
        return []

    for i, st in enumerate(doc.get('steps', [])):
        if not isinstance(st, dict):
            continue
        w = f'{where}#steps/{i}'
        c = norm_step(st)
        row = ops.get((c['port'], c['op']))
        if c['port'] == 'P1' and row and not expects_schema_reject(st.get('expect')):
            for part in ('request', 'response'):
                sid = f'urn:botaru:schema:{row.get(part)}:1' if row.get(part) else None
                if c[part] is not None and sid in schemas:
                    body = c[part]
                    if part == 'request' and not c['legacy']:
                        continue  # the call body is checked by call_body_errors below, with the runner.md permissions
                    for e in errors_of({'$ref': sid}, body):
                        f.error('flow_body_invalid', w, f'P1 {c["op"]} {part} ({row[part]}): {e}')
            req = c['request'] if isinstance(c['request'], dict) else {}
            if c['op'] == 'enqueue' and isinstance(req.get('body'), dict):
                tt = sets['types'].get(req['body'].get('type'))
                if tt is None:
                    f.error('flow_ref_missing', w, f'type {req["body"].get("type")} does not exist')
                else:
                    for e in errors_of(resolve(tt['params_schema'], lists), req['body'].get('params', {})):
                        f.error('flow_body_invalid', w, f'params: {e}')
            elif c['op'] == 'enqueue' and 'body' in st and isinstance(st['body'], dict):
                tt = sets['types'].get(st['body'].get('type'))
                if tt is None:
                    f.error('flow_ref_missing', w, f'type {st["body"].get("type")} does not exist')
                else:
                    for e in errors_of(resolve(tt['params_schema'], lists), st['body'].get('params', {})):
                        f.error('flow_body_invalid', w, f'params: {e}')
        if c['port'] == 'P3':
            for e in args_errors(c):
                f.error('flow_args_invalid', w, e)
    for at, oid, msg in call_body_errors({'steps': doc.get('steps', [])}):
        f.error('flow_body_invalid', f'{where}#{at}', f'{oid}: {msg}')
    for i, st in enumerate((doc.get('expect') or {}).get('denied', [])):
        c = norm_step(st)
        if st.get('reason') == 'input_schema_invalid' and c['port'] == 'P3' and not args_errors(c):
            f.error('flow_denied_passes', f'{where}#expect/denied/{i}', 'a call from denied passes the closed schema')


# ---------------------------------------------------------------- tools/list dump (lead decision 12)
def check_tools_list(pname, ps, listing, f, where):
    """Compares the tools/list dump of a plugin server with tool.yaml (INJ-2, INJ-3).

    The set of names in the live list equals the set of tool names in the manifest (the server also
    returns a tool with enabled: false); for an enabled tool
    every manifest parameter is in inputSchema.properties, and every name in
    inputSchema.required is in the manifest parameters; the readOnlyHint and
    destructiveHint annotations do not contradict access."""
    res = listing.get('result', listing) if isinstance(listing, dict) else None
    live = res.get('tools') if isinstance(res, dict) else None
    if not isinstance(live, list) or not all(isinstance(t, dict) and isinstance(t.get('name'), str) for t in live):
        f.error('tools_list_mismatch', where, 'the dump does not look like a tools/list answer: expected {"tools": [{"name": ...}]} '
                'or a JSON-RPC answer with result.tools')
        return
    mine = {t['name']: t for t in ps.get('tools', []) if isinstance(t, dict) and 'name' in t}
    names = [t['name'] for t in live]
    for n in sorted({x for x in names if names.count(x) > 1}):
        f.error('tools_list_mismatch', where, f'{pname}: tool {n} appears twice in tools/list')
    for n in sorted(set(names) - set(mine)):
        f.error('tools_list_mismatch', where, f'{pname}: tool {n} from tools/list is not in tool.yaml; toolhost becomes blocked (INJ-2)')
    for n in sorted(set(mine) - set(names)):
        f.error('tools_list_mismatch', where, f'{pname}: tool {n} from tool.yaml is not in tools/list (INJ-2)')
    for t in live:
        m = mine.get(t['name'])
        if not m or m.get('enabled') is False:
            continue
        schema = t.get('inputSchema') if isinstance(t.get('inputSchema'), dict) else {}
        props = set((schema.get('properties') or {}) if isinstance(schema.get('properties'), dict) else {})
        params = set(m.get('params') or {})
        for c in (m.get('commands') or {}).values():
            params |= set((c or {}).get('params') or {})
        for prm in sorted(params - props):
            f.error('tools_list_mismatch', where, f'{pname}/{t["name"]}: parameter {prm} from tool.yaml is not in the server inputSchema (INJ-2)')
        for prm in sorted(set(schema.get('required') or []) - params):
            f.error('tools_list_mismatch', where, f'{pname}/{t["name"]}: the server requires {prm}, but tool.yaml has no such parameter (INJ-2)')
        ann = t.get('annotations') if isinstance(t.get('annotations'), dict) else {}
        access = m.get('access')
        if access == 'read' and (ann.get('readOnlyHint') is False or ann.get('destructiveHint') is True):
            f.error('mcp_annotation_conflict', where, f'{pname}/{t["name"]}: access: read, but the server declares '
                    + ('readOnlyHint: false' if ann.get('readOnlyHint') is False else 'destructiveHint: true') + ' (INJ-3)')
        if access == 'write' and ann.get('readOnlyHint') is True:
            f.error('mcp_annotation_conflict', where, f'{pname}/{t["name"]}: access: write, but the server declares readOnlyHint: true (INJ-3)')


def tools_list_args():
    """Pairs (plugin or None, file) from the repeatable option --tools-list [<plugin>=]<file.json>."""
    out, argv = [], sys.argv[1:]
    for i, a in enumerate(argv):
        if a == '--tools-list':
            v = argv[i + 1] if i + 1 < len(argv) else ''
            name, _, path = v.rpartition('=') if '=' in v else (None, '', v)
            out.append((name or None, path))
    return out


def run_tools_list(*sets):
    """Plugins come from the first set that has them (own manifests before tutorial ones)."""
    pairs = tools_list_args()
    if not pairs:
        return
    plugins = {}
    for docs in sets:
        plugins = {(d.get('metadata') or {}).get('name'): (w, d.get('spec') or {}) for w, d in docs
                   if isinstance(d, dict) and d.get('kind') == 'ToolPlugin'}
        if plugins:
            break
    tl = Findings()
    for name, path in pairs:
        if not path or not os.path.isfile(path):
            rep.error(path or '--tools-list', '[tools_list_file_missing] --tools-list needs a JSON file with a tools/list answer')
            continue
        try:
            listing = load_json(os.path.abspath(path))
        except ValueError as e:
            rep.error(path, f'[tools_list_unreadable] JSON does not parse (a repeated key is an error too, I-JSON): {str(e)[:200]}')
            continue
        if name is None:
            if len(plugins) != 1:
                rep.error(path, f'[tools_list_plugin] the set has {len(plugins)} plugins: use --tools-list <plugin>={path}')
                continue
            name = next(iter(plugins))
        if name not in plugins:
            rep.error(path, f'[tools_list_plugin] plugin {name} is not in the manifest set')
            continue
        check_tools_list(name, plugins[name][1], listing, tl, path)
    emit(tl)
    rep.info(f'tools/list dumps {len(pairs)}')


# ---------------------------------------------------------------- run
# YAML is read by the shared loader common.Loader: YAML 1.2 (the key on is a string,
# Route.spec.on), a repeated key is an error.
def load_docs(path):
    out = []
    try:
        for i, d in enumerate(load_yaml_all(path)):
            if d is not None:
                out.append((f'{rel(path)}#{i}' if i else rel(path), norm(d)))
    except yaml.YAMLError as e:
        rep.error(rel(path), f'[yaml_invalid] YAML does not parse: {" ".join(str(e).split())[:240]}')
    return out


def context(f_docs):
    S = {'types': {}, 'tools': {}, 'actions': {}, 'catalogs': {}}
    for _, d in f_docs:
        if not isinstance(d, dict):
            continue
        k, spec, name = d.get('kind'), d.get('spec') or {}, (d.get('metadata') or {}).get('name')
        if k == 'TaskType':
            S['types'][name] = spec
        elif k == 'Action':
            S['actions'][name] = spec
        elif k == 'ToolPlugin':
            for t in spec.get('tools', []):
                S['tools'][f'{name}/{t["name"]}'] = (name, spec, t, None)
        elif k == 'Catalog':
            lists = {'hosts': [h['id'] for h in spec.get('hosts', [])],
                     'services': [s['name'] for h in spec.get('hosts', []) for s in h.get('services', [])],
                     'monitor_triggers': [t['triggerid'] for t in spec.get('monitor_triggers', [])]}
            for r in spec.get('resources', []):
                if r.get('list'):
                    lists.setdefault(r['list'], []).extend(str(v) for v in r.get('values', []))
            S['catalogs'][(d.get('metadata') or {}).get('tenant')] = lists
    return S


def emit(findings, prefix=''):
    for level, code, where, msg in findings.items:
        (rep.error if level == 'error' else rep.note)(where, f'[{code}] {prefix}{msg}')


def lint_deploy(topo, f, where):
    """Deployment lint (DEP-10): deploy-topology schema and listeners of the kind: core process (A4, D-86 (4))."""
    for e in errors_of({'$ref': 'urn:botaru:schema:deploy-topology:1'}, topo):
        f.error('schema_invalid', where, f'deployment description: {e}')
    hosts = (topo or {}).get('hosts') or {}
    worker_nets = {n for h in hosts.values() for pr in (h or {}).get('processes') or []
                   if pr.get('kind') not in ('core', 'store', 'backup', 'watch') for n in pr.get('networks') or []}
    for hname, h in hosts.items():
        for pr in (h or {}).get('processes') or []:
            lis = pr.get('listeners') or {}
            if pr.get('kind') != 'core' or not lis:
                continue
            if 'test' in lis and h.get('kind', 'production') == 'production':
                f.error('listener_test_in_production', where,
                        f'machine {hname}: process {pr.get("name")} has a stand port listener on a kind: production machine (D-86 (4))')
            shared = sorted(set((lis.get('p2') or {}).get('networks') or []) & worker_nets)
            if shared:
                f.note('listener_admin_worker_network', where,
                       f'machine {hname}: P2 network {shared} is among the worker networks (IDN-7)')
    # D-88: farm on one server and without a monitoring system (MON-2, RET-5).
    core_hosts = {hn for hn, h in hosts.items() for pr in (h or {}).get('processes') or [] if pr.get('kind') == 'core'}
    production = any((hosts.get(hn) or {}).get('kind', 'production') == 'production' for hn in core_hosts)
    watch_hosts = {hn for hn, h in hosts.items() for pr in (h or {}).get('processes') or [] if pr.get('kind') == 'watch'}
    if not (topo or {}).get('monitoring'):
        if not watch_hosts or not ((topo or {}).get('watch') or {}).get('notify_ref'):
            f.error('watch_missing', where, 'no monitoring system (the monitor profile is not claimed), and no kind: watch watcher '
                    'with watch.notify_ref: nobody checks that the core is alive (MON-2, D-88)')
        elif watch_hosts <= core_hosts:
            f.note('watch_same_host', where, f'the watcher is only on the core machine {sorted(watch_hosts)}: it will not see that machine die (MON-2)')
    elif (topo or {}).get('watch'):
        f.note('watch_with_monitoring', where, 'there is both a monitoring system and a watcher: the monitoring system checks that the core is alive (MON-2)')
    backup_hosts = {hn for hn, h in hosts.items() for pr in (h or {}).get('processes') or [] if pr.get('kind') == 'backup'}
    bk = (topo or {}).get('backup') or {}
    off_core = backup_hosts - core_hosts
    if production and not off_core:
        if not (bk.get('target') == 'external' and bk.get('encrypted') is True and bk.get('key_ref')):
            f.error('backup_external_required', where,
                    'no copies on a separate machine: backup.target: external, encrypted: true and key_ref are required (RET-5, D-88)')
    if bk.get('target') == 'host' and bk.get('host') and bk['host'] in core_hosts:
        f.error('backup_external_required', where, f'backup.host {bk["host"]} is the core machine (RET-5)')


def obj_key(d):
    m = d.get('metadata') or {} if isinstance(d, dict) else {}
    return (d.get('kind'), m.get('tenant'), m.get('name')) if isinstance(d, dict) else None


def collect(root, skip_deploy=True):
    """Manifests and flows of a folder: YAML with apiVersion and YAML with steps."""
    found, steps = [], []
    for p in walk(('.yaml', '.yml'), root):
        if skip_deploy and p.startswith(DEPLOY):
            continue  # the deployment lint checks the deployment description, it is not a manifest
        for where, d in load_docs(p):
            if isinstance(d, dict) and 'apiVersion' in d:
                found.append((where, d))
            elif isinstance(d, dict) and 'steps' in d:
                steps.append((where, d))
            elif isinstance(d, dict) and 'kind' in d:
                rep.error(where, '[manifest_no_api_version] document with kind but without apiVersion: manifest not checked')
            elif rel(p) not in NOT_MANIFESTS:
                rep.error(where, '[not_a_manifest] document is neither a manifest nor a flow: add apiVersion '
                          'or list the file in NOT_MANIFESTS in ci/check_manifests.py')
    return found, steps


# Manifest kind folders in your own installation folder (CONTRIBUTING.md): YAML in them must be a manifest.
KIND_DIRS = {'tools', 'roles', 'tasktypes', 'tenants', 'messagetypes', 'model-profiles', 'manifests'}
# Channel drivers that have a skeleton binding note spec/bindings/channels/<driver>.md.
KNOWN_CHANNEL_DRIVERS = {os.path.splitext(x)[0] for x in os.listdir(os.path.join(ROOT, 'spec', 'bindings', 'channels'))
                         if x.endswith('.md') and x != 'README.md'} if os.path.isdir(os.path.join(ROOT, 'spec', 'bindings', 'channels')) else set()


def driver_schema_dirs():
    """Folders from the repeatable option --driver-schemas <dir>."""
    out = []
    for j, a in enumerate(sys.argv):
        if a == '--driver-schemas':
            d = os.path.abspath(sys.argv[j + 1]) if j + 1 < len(sys.argv) else ''
            if not os.path.isdir(d):
                rep.error(d or '--driver-schemas', '[manifests_dir_missing] --driver-schemas needs an existing folder')
                rep.finish()
            out.append(d)
    return out


def lint_driver_schemas(own, own_dir, extra_dirs, f):
    """Driver schemas of your own folder (MAN-4, lead decision 5 of review4).

    A driver schema is a *.schema.json with $id urn:botaru:schema:driver.<driver>:<N> in your own folder or any
    *.schema.json from --driver-schemas. Each passes the driver-config:1 meta-schema; spec.config of your own Channel
    or Sink with that driver passes the driver schema. Your own channel with a driver that has neither a skeleton binding note nor a
    schema gives the note driver_schema_missing.
    """
    found = {}
    paths = [(p, False) for p in walk(('.schema.json',), own_dir)
             if not any(x.startswith('.') for x in os.path.relpath(p, own_dir).split(os.sep)[:-1])]
    for d in extra_dirs:
        paths += [(p, True) for p in walk(('.schema.json',), d)]
    for p, explicit in paths:
        where = os.path.relpath(p)
        try:
            sch = json.load(open(p, encoding='utf-8'))
        except (OSError, ValueError) as e:
            f.error('schema_invalid', where, f'driver schema does not parse as JSON: {str(e)[:120]}')
            continue
        m = re.fullmatch(r'urn:botaru:schema:driver\.([a-z][a-z0-9_-]*):[1-9][0-9]*', str(sch.get('$id', '')))
        if not m and not explicit:
            continue
        if not m:
            f.error('schema_invalid', where, 'driver schema $id is not of the form urn:botaru:schema:driver.<driver>:<N> (MAN-4)')
            continue
        for e in errors_of({'$ref': 'urn:botaru:schema:driver-config:1'}, sch):
            f.error('driver_config_forbidden', where, f'driver schema {m.group(1)}: {e} (MAN-4)')
        found[m.group(1)] = (where, sch)
    for where, d in own:
        if d.get('kind') not in ('Channel', 'Sink'):
            continue
        spec = d.get('spec') or {}
        drv = spec.get('driver')
        if drv in found:
            for e in errors_of(found[drv][1], spec.get('config', {})):
                f.error('driver_config_invalid', where, f'spec.config does not match the schema of driver {drv} ({found[drv][0]}): {e} (MAN-4)')
        elif d.get('kind') == 'Channel' and drv not in KNOWN_CHANNEL_DRIVERS:
            f.note('driver_schema_missing', where,
                   f'channel with driver {drv}: no skeleton binding note and no driver schema in the folder or in --driver-schemas (MAN-4)')


own_dir = None
if '--manifests' in sys.argv:
    i = sys.argv.index('--manifests')
    own_dir = os.path.abspath(sys.argv[i + 1]) if i + 1 < len(sys.argv) else None
    if not own_dir or not os.path.isdir(own_dir):
        rep.error(str(own_dir), '[manifests_dir_missing] --manifests needs an existing folder')
        rep.finish()

farm_hosts, farm_src = farm_hosts_default()
if not farm_src:
    rep.note('examples/deploy/farm.yaml', '[farm_hosts_default] no such file: farm hosts are taken from the text of docs/quality.md (core1, backup1)')
manifests, flows = collect(EXAMPLES) if os.path.isdir(EXAMPLES) else ([], [])

if own_dir:
    # Own manifests on top of the examples: an object with the same kind, tenant and name replaces the tutorial one.
    # Lead decision 5 (review4): files outside manifest kinds are skipped. Hidden folders (.github/ and others)
    # are not walked; a document without apiVersion botaru/... and without steps is not a manifest (eval case, docker-compose.yml,
    # mkdocs.yml); a note about it only in a kind folder (KIND_DIRS), a document with a manifest kind and no apiVersion
    # stays an error.
    own, own_flows, skipped_files = [], [], set()
    for p in walk(('.yaml', '.yml'), own_dir):
        parts = os.path.relpath(p, own_dir).split(os.sep)
        if any(x.startswith('.') for x in parts[:-1]):
            continue
        for i, d in enumerate(load_yaml_all(p)):
            if d is None:
                continue
            where = os.path.relpath(p) + (f'#{i}' if i else '')
            d = norm(d)
            if isinstance(d, dict) and str(d.get('apiVersion', '')).startswith('botaru/'):
                own.append((where, d))
            elif isinstance(d, dict) and 'steps' in d and 'apiVersion' not in d:
                own_flows.append((where, d))
            elif isinstance(d, dict) and 'apiVersion' not in d and d.get('kind') in KINDS:
                rep.error(where, '[manifest_no_api_version] document with a manifest kind but without apiVersion: manifest not checked')
            else:
                skipped_files.add(p)
                if set(parts[:-1]) & KIND_DIRS:
                    rep.note(where, '[not_a_manifest] document in a kind folder is neither a manifest nor a flow: skipped')
    keys = {obj_key(d) for _, d in own}
    docs = [(w, d) for w, d in manifests if obj_key(d) not in keys] + own
    found = lint(docs, farm_hosts=farm_hosts, examples_mode=True, skills=read_skills(own_dir))
    lint_driver_schemas(own, own_dir, driver_schema_dirs(), found)
    found.items = [x for x in found.items if not x[2].startswith('examples/')] + \
        [x for x in found.items if x[2].startswith('examples/') and x[0] == 'error']
    emit(found)
    flow_f = Findings()
    for where, d in own_flows:
        check_flow(where, d, context(docs), flow_f)
    emit(flow_f)
    run_tools_list(own, docs)
    rep.info(f'own manifests {len(own)}, flows {len(own_flows)}, tutorial manifests in the background {len(docs) - len(own)}, '
             f'non-manifest files skipped {len(skipped_files)}')
    rep.finish()

if tools_list_args():
    run_tools_list(manifests)
    rep.finish()

EXAMPLE_PROFILES = ['core', 'monitor'] if 'monitoring' in (load_yaml(os.path.join(EXAMPLES, 'deploy', 'farm.yaml')) or {}) else ['core']
examples_found = lint(manifests, farm_hosts=farm_hosts, examples_mode=True, claimed=EXAMPLE_PROFILES,
                      skills=read_skills(EXAMPLES))
emit(examples_found)
deploy_f = Findings()
for dp in ('examples/deploy/farm.yaml', 'examples/deploy/single.yaml', 'deploy/compose.example.yaml'):
    if os.path.exists(os.path.join(ROOT, dp)):
        lint_deploy(load_yaml(os.path.join(ROOT, dp)), deploy_f, dp)
emit(deploy_f)
examples_broken = bool(examples_found.codes('error'))
ctx = context(manifests)
flow_f = Findings()
for where, d in flows:
    check_flow(where, d, ctx, flow_f)
emit(flow_f)

# Lint fixtures: cases with an expected result.
n_cases, skipped = 0, 0
if os.path.isdir(FIXTURES):
    for p in walk(('.yaml', '.yml'), FIXTURES):
        rp = rel(p)
        try:
            fx = norm(load_yaml(p) or {})
        except yaml.YAMLError as e:
            rep.error(rp, f'[yaml_invalid] YAML does not parse: {" ".join(str(e).split())[:240]}')
            continue
        for k in ('id', 'rules', 'description', 'cases'):
            if k not in fx:
                rep.error(rp, f'[fixture_field_missing] the fixture has no field {k}')
        for rid in fx.get('rules', []):
            if rid not in REQUIREMENTS:
                rep.error(rp, f'[rule_unknown] rule {rid} is not in the registry')
        today = datetime.date.fromisoformat(fx['today']) if fx.get('today') else datetime.date(2026, 10, 12)
        for i, case in enumerate(fx.get('cases', [])):
            n_cases += 1
            where = f'{rp}#cases/{i}'
            docs = [(f'{where}/manifests/{j}', d) for j, d in enumerate(case.get('manifests', []))]
            if case.get('with_examples', fx.get('with_examples')):
                if examples_broken:
                    skipped += 1  # one error in examples/ is not multiplied across all fixtures with examples
                    continue
                keys = {obj_key(d) for _, d in docs if isinstance(d, dict)}
                docs = [(w, d) for w, d in manifests if obj_key(d) not in keys] + docs
            claimed = (case.get('conforms_to') or fx.get('conforms_to') or {}).get('profiles')
            got = lint(docs, today=today, farm_hosts=case.get('farm_hosts', farm_hosts), claimed=claimed,
                       skills=case.get('skills')) if (docs or case.get('skills')) else Findings()
            if 'before' in case:
                lint_additive([(f'{where}/before/{j}', d) for j, d in enumerate(case['before'])], docs, got)
            if 'commit' in case:
                lint_commit(case['commit'], got, where)
            if 'conforms_to' in case:
                lint_conforms(case, got, where)
            if 'driver_schema' in case:
                for e in errors_of({'$ref': 'urn:botaru:schema:driver-config:1'}, case['driver_schema']):
                    got.error('driver_config_forbidden', where, f'driver schema: {e} (MAN-4)')
            if 'flow' in case:
                check_flow(where, case['flow'], context(docs), got)
            if 'deploy' in case:
                lint_deploy(case['deploy'], got, where)
            exp = case.get('expect', 'ok')
            errs = got.codes('error')
            name = case.get('name')
            if exp == 'ok':
                if errs:
                    rep.error(where, f'[fixture_expect_ok] case {name!r} expects ok, but the lint gave {sorted(errs)}: '
                              + '; '.join(m for lv, _, _, m in got.items if lv == 'error')[:400])
                continue
            want = exp.get('error') if isinstance(exp, dict) else exp
            at = exp.get('at') if isinstance(exp, dict) else None
            also = exp.get('also', []) if isinstance(exp, dict) else []
            if want not in CODES:
                rep.error(where, f'[fixture_code_unknown] case {name!r} expects code {want!r}, which is not in ci/lint_codes.py')
                continue
            hits = [(w, m) for lv, c, w, m in got.items if lv == 'error' and c == want]
            extra = errs - {want} - (set(CODES) if also is True else set(also or []))
            if not hits:
                rep.error(where, f'[fixture_expect_error] case {name!r} expects {want}, but the lint gave {sorted(errs) or "ok"}')
            elif at and not any(at in w or at in m for w, m in hits):
                rep.error(where, f'[fixture_expect_at] case {name!r}: {want} not at {at}: ' + '; '.join(m for _, m in hits)[:300])
            elif extra:
                rep.error(where, f'[fixture_expect_extra] case {name!r}: besides {want} also {sorted(extra)}: '
                          + '; '.join(m for lv, c, _, m in got.items if lv == 'error' and c in extra)[:300]
                          + ' (fix the case manifest or list the codes in expect.also)')
if skipped:
    rep.info(f'cases with with_examples skipped: {skipped}: fix the errors in examples/ first, '
             'these cases use the examples in the background')
rep.info(f'manifests {len(manifests)}, flows {len(flows)}, fixture cases {n_cases}')
rep.finish(limit=400)
