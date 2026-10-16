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
  from the lint:manifests/* and lint:roles/profile-limits checks of
  conformance/suites/lint/manifests.yaml and roles.yaml that can be checked from files;
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
import ipaddress
import json
import os
import re
import sys
import urllib.parse
import zoneinfo

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


WEBHOOK_DOC_NETS = [ipaddress.ip_network(n) for n in ('192.0.2.0/24', '198.51.100.0/24', '203.0.113.0/24', '2001:db8::/32')]



def cron_valid(expr):
    """Five cron fields with values in range: lists, ranges and steps (TIM-3)."""
    fields = expr.split()
    if len(fields) != 5:
        return False
    for f, (lo, hi) in zip(fields, ((0, 59), (0, 23), (1, 31), (1, 12), (0, 7))):
        for part in f.split(','):
            # An empty step or range end ('*/', '5-') is refused, as the core does at apply.
            rng, slash, step = part.partition('/')
            if slash and (not re.fullmatch(r'[0-9]+', step) or int(step) < 1):
                return False
            if rng == '*':
                continue
            a, dash, b = rng.partition('-')
            if not re.fullmatch(r'[0-9]+', a) or (dash and not re.fullmatch(r'[0-9]+', b)):
                return False
            a, b = int(a), int(b) if b else int(a)
            if a < lo or b > hi or a > b:
                return False
    return True
def read_by_code(v):
    """Whether a schema has a field marked x-botaru-read-by: code."""
    if isinstance(v, dict):
        return v.get('x-botaru-read-by') == 'code' or any(read_by_code(e) for e in v.values())
    if isinstance(v, list):
        return any(read_by_code(e) for e in v)
    return False


def names_human(v):
    """Whether a schema fixes a value naming a human principal or the kind human (DLV-14)."""
    human = re.compile(r'^human(:|$)')
    if isinstance(v, dict):
        vals = [v.get('const')] + (v.get('enum') if isinstance(v.get('enum'), list) else [])
        if any(isinstance(x, str) and human.match(x) for x in vals):
            return True
        return any(names_human(e) for e in v.values())
    if isinstance(v, list):
        return any(names_human(e) for e in v)
    return False

def known_zone(name):
    """Whether an IANA time zone name exists in the zone database (TIM-3)."""
    try:
        zoneinfo.ZoneInfo(name)
    except (zoneinfo.ZoneInfoNotFoundError, ValueError, OSError):
        return False
    return True

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
# Programs whose output is text written from outside the farm: journalctl prints the service journal (INJ-14).
# The skeleton names the journal as the example and has no registry of such programs.
OUTSIDE_TEXT_PROGRAMS = {'journalctl'}
# Parameter names, or the last word of a name, that carry a network address: a URL, a host or where to send (INJ-6).
# A plural (urls, hosts, addresses) counts as the word.
ADDRESS_WORDS = {'url', 'uri', 'href', 'endpoint', 'host', 'hostname', 'address', 'addr', 'ip', 'link', 'target', 'server',
                 'domain', 'dest', 'fqdn'}


def class_refused(cls, output=False):
    """A data class a manifest may not set: unknown or reserved (pii), for an output class also secret (PRM-5)."""
    dc = DATA_CLASSES.get(cls)
    return not dc or bool(dc.get('reserved')) or (output and dc.get('output') is False)


def address_word(name):
    w = name.rsplit('_', 1)[-1]
    return w in ADDRESS_WORDS or w.removesuffix('s') in ADDRESS_WORDS or (w.endswith('es') and w[:-2] in ADDRESS_WORDS)


def closed_values(schema):
    return any(k in schema for k in ('enum', 'enum_from', 'const'))


def address_param(name, schema):
    """A URL or host argument of the model: an address name whose values are not a closed list, also as the items of
    an array (under the array's name) or a property of an object (INJ-6)."""
    schema = schema if isinstance(schema, dict) else {}
    if closed_values(schema):
        return False
    if schema.get('type') == 'array':
        return address_param(name, schema.get('items'))
    if schema.get('type') == 'object':
        return any(address_param(k, v) for k, v in sorted((schema.get('properties') or {}).items()))
    return address_word(name)


def egress_open(schema):
    """On a plugin with egress: a string, also in an array or an object, that is free or of a prose format can carry
    an address whatever its name (INJ-6)."""
    schema = schema if isinstance(schema, dict) else {}
    if closed_values(schema):
        return False
    if schema.get('type') == 'array':
        return egress_open(schema.get('items'))
    if schema.get('type') == 'object':
        return any(egress_open(v) for _, v in sorted((schema.get('properties') or {}).items()))
    if schema.get('type') == 'string':
        return 'x-botaru-format' not in schema or schema['x-botaru-format'] in PROSE_FORMATS
    return False


REQUIREMENTS = rules()
# Built-in channel commands: commands cannot take them (PRM-17, docs/task-language.md).
BUILTIN_COMMANDS = {'/help', '/about', '/lang', '/mydata', '/tasks', '/status', '/cancel', '/me', '/team',
                    '/run', '/service', '/forget', '/grant', '/ungrant', '/incident', '/manager'}
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


# Formats of spec/model/formats.yaml whose values are prose, not a closed shape: a string of
# such a format is free text (INJ-3).
PROSE_FORMATS = {'search_query'}


def free_text(node):
    """Whether the output schema has a string without enum, const and a non-prose format (INJ-3)."""
    for _, n in walk_nodes(node):
        if n.get('type') == 'string' and 'maxLength' in n and not any(
                k in n for k in ('enum', 'enum_from', 'const')) and (
                'x-botaru-format' not in n or n['x-botaru-format'] in PROSE_FORMATS):
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
                 'monitor_triggers': [t['triggerid'] for t in cs.get('monitor_triggers', [])],
                 'monitor_items': [t['item'] for t in cs.get('monitor_triggers', [])]}
        for r in cs.get('resources', []):
            if r.get('data_class') and class_refused(r['data_class']):
                f.error('cap_data_class', where, f'resource {r["id"]}: data class {r["data_class"]!r} is not accepted (PRM-5)')
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
    # the manager seat profile is set once, in the charter of the tenant farm (GOV-33, MGR-42)
    seat_mp = next(((gs.get('farm_manager') or {}).get('model_profile') for t, _, _, gs in S.each('Governance')
                    if t == 'farm' and (gs.get('farm_manager') or {}).get('model_profile')), None)
    for _, tname, where, ts in S.each('Tenant'):
        if ts.get('timezone') and not known_zone(ts['timezone']):
            f.error('timezone_unknown', where, f'tenant {tname}: time zone {ts["timezone"]} is not in the IANA database (TIM-3)')
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
            elif (t in types and not types[t].get('summary_fields')
                  and (isinstance(dt, str) or not dt.get('chats')
                       or any(ch == 'group' or str(ch).startswith('chat:') for ch in dt['chats']))):
                f.error('summary_fields_required', where, f'default_types: type {t} for a group chat has no summary_fields (REP-11)')
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
            mp = seat_mp
            if not mp:
                f.error('manager_profile_missing', where, 'the manager is enabled, but the farm charter (Governance of '
                        'tenant farm) sets no farm_manager.model_profile for the seat (GOV-33, MGR-20)')
            elif mp not in ts.get('models_allowed', []):
                f.error('manager_profile_not_allowed', where,
                        f'manager seat profile {mp} is not in models_allowed (MGR-20)')
            line = ((ts.get('budgets') or {}).get('lines') or {}).get('manager') or {}
            if not any((line.get(k) or 0) > 0 for k in ('month_eur', 'day_eur')):
                f.error('manager_line_missing', where, 'manager budget line is not above 0 while the manager is enabled (MGR-20)')
            # three readings of one worst-case incident: 60 thousand input tokens and three answers at max_output_tokens
            sp = profiles.get(mp) if mp else None
            if sp and line.get('day_eur') is not None:
                price = sp.get('price') or {}
                worst = (60000 * price.get('input', 0) + 3 * sp.get('max_output_tokens', 0) * price.get('output', 0)) / 1e6
                if line['day_eur'] < worst:
                    f.error('manager_line_too_small', where,
                            f'manager.day_eur {line["day_eur"]} is below {worst:.2f} EUR, three readings of a worst-case incident (MGR-20)')
        priv = ts.get('privacy') or {}
        # monitoring is a mandatory recipient only when the installation claims the monitor profile (D-88);
        # unknown claimed profiles count as monitor: the check fails closed
        if (claimed is None or 'monitor' in claimed) and 'monitoring' not in priv.get('recipients', ['monitoring']):
            f.error('recipients_monitoring', where, 'privacy.recipients has no monitoring while the monitor profile is claimed '
                    'or the claimed profiles are unknown (MON-1)')
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
        for path in (('continuation',), ('await', 'then'), ('await', 'on_timeout')):
            node = ts
            for k in path:
                node = (node or {}).get(k) or {}
            if (types.get(node.get('type')) or {}).get('purpose') == 'manage':
                f.error('manage_enqueued', where, f'type {tname}: {"/".join(path)} starts {node["type"]} of purpose manage (MGR-4)')
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
        if ts.get('data_class') and class_refused(ts['data_class']):
            f.error('cap_data_class', where, f'type {tname}: data class {ts["data_class"]!r} is not accepted (PRM-5)')
        if (ts.get('run') or {}).get('approval') == 'none':
            if isinstance(ts.get('tier'), int) and ts['tier'] >= 3:
                f.error('run_not_readonly', where, f'type {tname} with run.approval: none is of tier {ts["tier"]}: '
                        'without a card a pipeline changes nothing on work machines (APR-17)')
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
            if t.get('output_class') and class_refused(t['output_class'], output=True):
                f.error('cap_data_class', where, f'{tid}: output_class {t["output_class"]!r} is not accepted (PRM-5)')
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
            elif t.get('target_from') and (t['target_from'] not in params or not t.get('args_from') and not str(
                    (params[t['target_from']] or {}).get('enum_from', '')).startswith('catalog.')):
                f.error('tool_target_unknown', where,
                        f'{tid}: target_from {t["target_from"]} is not a tool parameter with a catalog list (TEN-3a)')
            if ps['placement'] == 'host' and t.get('untrusted_output') is True \
                    and not {'ip', 'email', 'secret'} <= set(t.get('redact', [])):
                f.error('redact_required', where, f'{tid}: placement: host and untrusted_output: true without redact [ip, email, secret] (INJ-4)')
            if (t.get('cache_s') or 0) > 0 and (t.get('credential') or {}).get('per_principal'):
                f.error('cache_per_principal', where, f'{tid}: cache_s on a tool with credential.per_principal; '
                        'output of a person\'s connection is never cached (PRM-12, SEC-7)')
            if t.get('untrusted_output') is False and t.get('output_schema') and free_text(t['output_schema']):
                f.error('untrusted_output_false_text', where, f'{tid}: untrusted_output: false with free text in the output (INJ-3)')
            # The model sees one schema per parameter name, so the tool and all its commands
            # must agree on it; otherwise a value allowed for one command reaches another.
            seen_params = dict(t.get('params') or {})
            for cname, c in sorted((t.get('commands') or {}).items()):
                for k, v in (c.get('params') or {}).items():
                    if k in seen_params and seen_params[k] != v:
                        f.error('cmd_param_conflict', where,
                                f'{tid} {cname}: parameter {k} has another schema elsewhere in the tool (INJ-14)')
                    seen_params.setdefault(k, v)
            all_params = dict(t.get('params') or {})
            for c in (t.get('commands') or {}).values():
                for k, v in (c.get('params') or {}).items():
                    all_params.setdefault(k, v)
            egress = bool((ps.get('network') or {}).get('egress'))
            for k, v in sorted(all_params.items()):
                if address_param(k, v) or (egress and egress_open(v)):
                    f.error('tool_address_param', where, f'{tid}: parameter {k} is a URL or host argument of the model; '
                            'the address comes from the manifest or the catalog (INJ-6)')
            for cname, c in (t.get('commands') or {}).items():
                argv = c['argv']
                if os.path.basename(argv[0]) in OUTSIDE_TEXT_PROGRAMS:
                    if t.get('untrusted_output') is False:
                        f.error('untrusted_output_false_text', where,
                                f'{tid} {cname}: {argv[0]} prints text written from outside; untrusted_output: true is required (INJ-14)')
                    if t.get('output_class') != 'raw_logs':
                        f.error('untrusted_output_class', where,
                                f'{tid} {cname}: {argv[0]} prints text written from outside; output_class raw_logs is required (INJ-14)')
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
            if ts['purpose'] == 'insight' and tt not in (rs.get('insight') or {}).get('types', []):
                f.error('insight_type_invalid', where, f'role {rname}: insight type {tt} is claimed without the role insight section naming it (INS-1)')
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
        lint_insight(f, rname, where, rs, types)
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
            if (types.get(tt) or {}).get('purpose') == 'manage':
                f.error('manage_enqueued', where, f'role {rname}: {tt} of purpose manage in may_enqueue (MGR-4)')
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
        if not cron_valid(cron) or re.search(r'(CRON_)?TZ\s*=', cron):
            f.error('cron_invalid', where, f'schedule {sname}: cron of five fields without CRON_TZ and TZ (TIM-2)')
        if ss.get('timezone') and not known_zone(ss['timezone']):
            f.error('timezone_unknown', where, f'schedule {sname}: time zone {ss["timezone"]} is not in the IANA database (TIM-3)')
        if ('task' in ss) == ('script' in ss):
            f.error('schedule_target', where, f'schedule {sname}: exactly one of task and script (SCR-12)')
            continue
        if 'script' in ss:
            # SCR-12: a script that a slot starts without a person's button runs as system
            for addr in ss.get('deliver_to', []):
                check_address(where, tenant, addr)
            sc = S.get('Script', ss['script'], tenant)
            if sc is None:
                f.error('ref_missing', where, f'schedule {sname}: script {ss["script"]} is not a kind: Script of tenant {tenant}')
                continue
            tspec = S.get('Tenant', tenant) or {}
            ceil = tspec.get('scripts') or {}
            steps = sc.get('steps') or []
            if len(steps) > ceil.get('max_steps', 10):
                f.error('schedule_script_over_ceiling', where,
                        f'schedule {sname}: script {ss["script"]} has {len(steps)} steps, above Tenant.spec.scripts.max_steps (SCR-5, SCR-12)')
            for st in steps:
                sk, stt = st.get('kind'), st.get('type')
                claimers = [rs for _, rs in roles.values() if stt and stt in rs.get('claims', [])]
                if sk in ('action', 'confirm') or (sk == 'fanout' and st.get('action')) \
                        or any(rs.get('service_control') or rs.get('monitor_scripts') for rs in claimers):
                    f.error('schedule_script_needs_run', where,
                            f'schedule {sname}: step {st.get("id")} of script {ss["script"]} needs script.run; '
                            'only a person starts it (SCR-6, SCR-12)')
                if sk == 'await':
                    f.error('schedule_script_await', where,
                            f'schedule {sname}: step {st.get("id")} waits for an external event; an unattended run has no await (SCR-12)')
                if sk in ('task', 'fanout') and stt and groups and not system_has(tenant, stt):
                    f.error('system_ops_missing', where,
                            f'schedule {sname}: step {st.get("id")}: task:{stt} is not in system-ops (PRM-10, SCR-12)')
            continue
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
    # No record that code reads from model output names a person or rates one (DLV-14).
    for _, tname, where, ts in S.each('TaskType'):
        rs = ts.get('result_schema') or {}
        if read_by_code(rs) and names_human(rs):
            f.error('human_subject_in_record', where, f'type {tname}: a result read by code names a human principal (DLV-14)')
        if 'provenance' in (rs.get('properties') or {}):
            f.error('provenance_in_result', where, f'type {tname}: the result has a provenance field; only the core writes it (PRV-2)')
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
    # Every veto lens has an executor: a seat of the lens whose fallback role claims the lens review type
    # (COM-7). A lens review answers in at most 2000 output tokens (COM-6).
    lenses = {k: e for k, e in model_registry('lenses').items() if e.get('since') == 'v0.1' and e.get('subject') == 'proposal'}
    for tenant, gname, where, gs in S.each('Governance'):
        if not roles or 'commission' not in gs:
            continue
        seats = (gs.get('commission') or {}).get('seats', [])
        # The analysis profiles of the roles that can write proposals: they claim a type of purpose insight or
        # aggregate (COM-2).
        analysis = {(rs.get('models') or {}).get('analysis') for _, rs in roles.values()
                    if any((types.get(t) or {}).get('purpose') in ('insight', 'aggregate') for t in rs.get('claims') or [])} - {None}
        for lid, le in sorted(lenses.items()):
            if le.get('veto') is not True:
                continue
            covered = any(s.get('lens') == lid and s.get('fallback_role') in roles
                          and le.get('review_type') in (roles[s['fallback_role']][1].get('claims') or []) for s in seats)
            # A lens without a seat goes to a role of profile reviewer that claims its type (COM-8).
            if not any(s.get('lens') == lid for s in seats):
                covered = covered or any(rs.get('profile') == 'reviewer' and le.get('review_type') in (rs.get('claims') or [])
                                         and (rs.get('models') or {}).get('base') not in analysis for _, rs in roles.values())
            if not covered:
                f.error('veto_lens_no_executor', where, f'charter {gname}: veto lens {lid} has no executor claiming {le.get("review_type")} (COM-7, COM-8)')
        # A seat's fallback role does not run on the analysis profile of a role that writes proposals (COM-2).
        for s in seats:
            fr = roles.get(s.get('fallback_role'))
            if fr and (fr[1].get('models') or {}).get('base') in analysis:
                f.error('reviewer_not_independent', where,
                        f'charter {gname}: seat {s.get("id")} fallback role {s.get("fallback_role")} runs on an analysis profile of a proposal author (COM-2)')
    for rname, (rwhere, rs) in sorted(roles.items()):
        if not any((types.get(t) or {}).get('purpose') == 'review' for t in rs.get('claims') or []):
            continue
        mp = (rs.get('models') or {}).get('base')
        out = (profiles.get(mp) or {}).get('max_output_tokens')
        if isinstance(out, int) and out > 2000:
            f.error('lens_profile_output', rwhere, f'role {rname}: profile {mp} has max_output_tokens {out} above 2000 for lens reviews (COM-6)')
    # Shadow shares and escalation thresholds of the commission manifest (COM-10, COM-13).
    for tenant, cname, where, cs in S.each('Commission'):
        sh = cs.get('shadow')
        if isinstance(sh, dict):
            zero = sorted(k for k in ('veto_lenses', 'other_lenses') if sh.get(k) == 0)
            if zero:
                f.error('shadow_zero_without_off', where,
                        f'commission {cname}: shadow share {zero} is 0; turn the shadow off only with shadow: off (COM-10)')
        esc = cs.get('escalation') or {}
        for k, floor in (('forward_if_score_gte', 2), ('untrusted_forward_if_score_gte', 3)):
            v = esc.get(k)
            if isinstance(v, int) and not isinstance(v, bool) and v < floor:
                f.error('escalation_threshold_lowered', where,
                        f'commission {cname}: escalation.{k} {v} is below the default {floor}; it can only be raised (COM-13)')
    forbidden = set(schemas.get('urn:botaru:schema:driver-config:1', {}).get('$defs', {}).get('forbidden', {}).get('enum', []))
    for kind in ('Channel', 'Sink'):
        for _, name, where, cs in S.each(kind):
            bad = sorted({k for _, n in walk_nodes(cs.get('config')) for k in n if k in forbidden})
            if bad:
                f.error('driver_config_forbidden', where,
                        f'{kind} {name}: spec.config has fields {bad}; they exist only in the common envelope (MAN-4)')

    # 11b. Retention periods (RET-9) and the outgoing webhook sink (DLV-17).
    rclasses = model_registry('retention-classes')
    for tenant, tname, where, ts in S.each('Tenant'):
        ret = ts.get('retention') or {}
        periods = {}
        for cls, dur in (ret.get('classes') or {}).items():
            e = rclasses.get(cls) or {}
            secs = seconds(dur)
            if 'ceiling_days' not in e:
                f.error('retention_period', where, f'tenant {tname}: retention.classes.{cls}: the class has no tenant period (RET-9)')
            elif secs is None or not e.get('min_days', 1) * 86400 <= secs <= e['ceiling_days'] * 86400:
                f.error('retention_period', where, f'tenant {tname}: retention.classes.{cls} {dur} is outside '
                        f'{e.get("min_days", 1)}..{e["ceiling_days"]} days (RET-9)')
            else:
                periods[cls] = secs
        content = periods.get('content', ((rclasses.get('content') or {}).get('default') or {}).get('days', 30) * 86400)
        for k in ('files', 'journals'):
            secs = seconds(ret.get(k)) if k in ret else None
            if secs is not None and secs > content:
                f.error('retention_period', where, f'tenant {tname}: retention.{k} {ret[k]} is longer than the content period (RET-9)')
    for tenant, name, where, ss in S.each('Sink'):
        if ss.get('driver') != 'webhook':
            continue
        base = str(ss.get('base') or '')
        try:
            u = urllib.parse.urlsplit(base)
            host = u.hostname if u.scheme == 'https' else None
        except ValueError:
            host = None
        private = False
        if host is not None:
            try:
                ip = ipaddress.ip_address(host)
                # Documentation ranges stand for public addresses in examples; the shared range of RFC 6598
                # is written as a number because the repository text holds documentation addresses only.
                if not any(ip in n for n in WEBHOOK_DOC_NETS):
                    private = (ip.is_private or ip.is_loopback or ip.is_link_local or ip.is_reserved or ip.is_unspecified
                               or ip.is_multicast or (ip.version == 4 and ip in ipaddress.ip_network((0x64400000, 10))))
            except ValueError:
                private = host == 'localhost' or host.endswith('.local') or host.endswith('.localhost')
        if host is None or private or host in farm_hosts or host.split('.')[0] in farm_hosts:
            f.error('webhook_sink_invalid', where, f'sink {name}: base {base!r} is not an https:// address outside the farm (DLV-17)')
        if not ss.get('signing_key_ref'):
            f.error('webhook_sink_invalid', where, f'sink {name}: driver webhook without signing_key_ref (DLV-17)')

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

    # 11b. v0.2 security checks: a person's copy goes only to that person (SEC-10, LCY-2), the takeover
    # card is never lifted (MGR-21), ingress authenticity (ING-1).
    for tenant, tname, where, ts in S.each('Tenant'):
        es = (ts.get('privacy') or {}).get('export_sink')
        if es:
            sk = S.get('Sink', es, tname)
            if sk is None:
                f.error('ref_missing', where, f'privacy.export_sink: sink {es} is not in tenant {tname}')
            elif ((sk.get('export_only') or {}).get('recipient_ref')) != 'self':
                f.error('export_sink_not_self', where,
                        f'privacy.export_sink {es}: a /mydata copy goes only to the person; the sink needs '
                        'export_only.recipient_ref: self (SEC-10, LCY-2)')
        cab = (ts.get('takeover') or {}).get('confirm_above_eur')
        if isinstance(cab, (int, float)) and not isinstance(cab, bool) and cab > 0:
            f.error('takeover_confirm_threshold', where,
                    f'takeover.confirm_above_eur {cab}: a takeover always needs the takeover_confirm card; the field is reserved and only 0 passes (MGR-21)')
    token_events = {}
    sources = {}
    for tenant, name, where, cs in S.each('Channel'):
        ing = cs.get('ingress') or {}
        if not ing:
            continue
        # The source of ext.<source>.<name>.vN names the service principal hook:<source>, the chat
        # and the delivery key; monitor belongs to the E5 adapter (MON-5), and one source has one
        # channel in a tenant, so the core can tie a signal to its channel (ING-3, ING-4).
        parts = str(ing.get('event') or '').split('.')
        source = parts[1] if len(parts) > 2 else ''
        if source == 'monitor':
            f.error('ingress_reserved_source', where,
                    f'channel {name}: the source monitor is reserved for the monitoring adapter E5 (MON-5, ING-4)')
        elif len(source) > 32:
            f.error('ingress_source_too_long', where,
                    f'channel {name}: source {source!r} is longer than 32 characters, so hook:<source> and the delivery key would not fit (ING-3)')
        if source:
            other = sources.setdefault((tenant, source), name)
            if other != name:
                f.error('ingress_source_duplicate', where,
                        f'channel {name}: source {source} is already the source of channel {other} in tenant {tenant} (ING-3, ING-4)')
        if ing.get('auth') == 'signature':
            tol = seconds(ing.get('tolerance'))
            if tol is None or tol > 300:
                f.error('ingress_tolerance', where,
                        f'channel {name}: auth: signature needs ingress.tolerance of at most 5m, got {ing.get("tolerance")!r} (ING-1)')
        elif ing.get('auth') == 'tls_token' and ing.get('event'):
            token_events.setdefault(tenant, set()).add(ing['event'])
    for tenant, rname, where, rs in S.each('Route'):
        ev = (rs.get('on') or {}).get('event') if isinstance(rs.get('on'), dict) else None
        tt = (rs.get('task') or {}).get('type')
        if ev and tt and ev in token_events.get(tenant, set()):
            tier = (types.get(tt) or {}).get('tier')
            if tier is None or tier > 0:
                f.error('ingress_token_tier', where,
                        f'route {rname}: event {ev} comes from a tls_token source, which routes only to tier 0 types; {tt} has tier {tier} (ING-1)')

    # 12. Global manifests without tenant data (TEN-6). The service tenant farm is also a word
    # of the language (a subject kind, a hold scope).
    for kind, entry in KINDS.items():
        if entry.get('scope') != 'global':
            continue
        for _, name, where, gs in S.each(kind):
            for s in strings(gs):
                if (s in tenants and s != 'farm') or re.search(r'(?<![\w.])(user|staff|service|chat|group):[a-z]', s) \
                        or re.search(r'[\w.+-]+@[\w-]+\.[\w.]+', s):
                    f.error('global_tenant_data', where, f'{kind} {name}: value {s!r} looks like tenant data (TEN-6)')
                    break
    lint_v02_features(S, f, types, tools, roles, claimed)
    lint_series(S, f, tools)
    lint_insight_types(S, f, roles)
    lint_lens_names(S, f)
    lint_governance(S, f)
    lint_incident_classes(S, f)
    lint_seat_profiles(S, f)
    return f


ROLLUPS = model_registry('rollups')
CORE_SERIES = model_registry('core-series')


def outcome_complete(o):
    """A complete outcome block: the rollup for comparison, the direction, a tolerance and min_samples (OBV-6)."""
    o = o if isinstance(o, dict) else {}
    return bool(o.get('compare') and o.get('better') and o.get('min_samples')
                and ('tolerance_pct' in o or 'tolerance_pp' in o))


VALUE_PATH_MARK = '<!-- value-path -->'
PERSONA_LINK = re.compile(r'(?i)(https?://|ftp://|www\.|mailto:|[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,})')
PERSONA_ID = re.compile(r'(?i)(\bbot_[0-9a-z]{6,}|\bcommission:[0-9]+|\bfarm-manager\b|\bcse_|\bcmsg_)')
PERSONA_ENV = re.compile(r'\b[A-Z][A-Z0-9]*_(KEY|TOKEN|SECRET|PASSWORD)\b')
WORD = re.compile(r'[^\W\d_][\w-]*')


def unaware_terms():
    out = {}
    for e in model_registry('unawareness-terms').values():
        for loc in ('en', 'ru', 'et'):
            out.setdefault(e.get('group'), []).extend(e.get(loc) or [])
    return out


def paragraphs(text):
    return [p.strip() for p in re.split(r'\n[ \t]*\n', text.replace('\r\n', '\n')) if p.strip()]


def has_term(terms, group, text):
    words = WORD.findall(text.lower())
    return any(w.startswith(s.lower()) for w in words for s in terms.get(group, []))


def unaware_linked(terms, text, value_path):
    """A paragraph links an act with an outcome (GOV-29); the value-path paragraph of a functionary persona is the exception."""
    for p in paragraphs(text or ''):
        if value_path and p.startswith(VALUE_PATH_MARK):
            continue
        if has_term(terms, 'act', p) and has_term(terms, 'outcome', p):
            return True
    return False


def persona_findings(text, position, terms):
    """Lint codes of a persona text for a position, or 'lens' for a lens addendum (GOV-4, COM-19, GOV-29)."""
    out = []
    if len(text.encode()) > 4096:
        out.append('persona_size')
    if PERSONA_LINK.search(text) or PERSONA_ID.search(text) or PERSONA_ENV.search(text):
        out.append('persona_link')
    if any(p.startswith(VALUE_PATH_MARK) for p in paragraphs(text)) != (position == 'functional'):
        out.append('persona_value_path')
    if unaware_linked(terms, text, position == 'functional') or any(
            not p.startswith(VALUE_PATH_MARK) and has_term(terms, 'soul', p) for p in paragraphs(text)):
        out.append('unaware_text')
    return out


def find_persona(where, ref):
    """The persona file of a charter: ref is relative to the charter's folder or one of its parents."""
    d = os.path.dirname(os.path.abspath(where.split('#')[0]))
    while True:
        p = os.path.join(d, ref)
        if os.path.isfile(p):
            return p
        parent = os.path.dirname(d)
        if parent == d:
            return None
        d = parent


def version_tuple(v):
    try:
        major, minor = str(v).lstrip('v').split('.')[:2]
        return int(major), int(minor)
    except ValueError:
        return (99, 0)


def lint_lens_names(S, f):
    """A seat or the commission manifest names a lens of lenses.yaml of the skeleton version or a tenant lens x_<name>
    declared with its meaning in the charter (COM-19); the weight of a lens with a weight by rule is not set (COM-8)."""
    skeleton = {k: e for k, e in model_registry('lenses').items() if version_tuple(e.get('since')) <= skeleton_version()}
    own = {}
    for tenant, _, _, gs in S.each('Governance'):
        own.setdefault(tenant, set()).update(k for k, v in ((gs.get('commission') or {}).get('lenses') or {}).items()
                                             if k.startswith('x_') and isinstance(v, dict) and v.get('meaning'))
    for tenant, gname, where, gs in S.each('Governance'):
        for s in (gs.get('commission') or {}).get('seats') or []:
            lens = s.get('lens')
            if lens and lens not in skeleton and lens not in own.get(tenant, set()):
                f.error('lens_unknown', where, f'charter {gname}: seat {s.get("id")} names lens {lens}, neither in lenses.yaml nor in the charter (COM-19)')
    for tenant, cname, where, cs in S.each('Commission'):
        for block in ('weights', 'min'):
            for lens in sorted(cs.get(block) or {}):
                if lens not in skeleton and lens not in own.get(tenant, set()):
                    f.error('lens_unknown', where, f'commission {cname}: {block}.{lens} names a lens neither in lenses.yaml nor in the charter (COM-19)')
                elif block == 'weights' and (skeleton.get(lens) or {}).get('weight') is not None:
                    f.error('lens_weight_fixed', where, f'commission {cname}: the weight of lens {lens} is set by rule (COM-8)')


def governance_name(n, incidents=True):
    return isinstance(n, str) and (n.startswith(('governance.', 'attribution.', 'seat.')) or (incidents and n.startswith('incident.')))


# Durations of a tenant charter that only rise from their default (GOV-33).
CHARTER_FLOORS = (('promotion', 'probation', '14d'), ('promotion', 'min_tenure', '30d'), ('promotion', 'swap_cooldown', '30d'),
                  ('removal', 'protect_new', '60d'), ('attribution', 'window', '14d'), ('confirmation', 'ttl', '7d'),
                  ('identity', 'unbound_ttl', '30d'), ('memory', 'seal_ttl', '30d'))


def lint_charter_bounds(tenant, gname, where, gs, f):
    """What the charter schema cannot check (GOV-33): a seat twice, farm_manager outside farm, durations against their direction."""
    seen = set()
    for s in (gs.get('commission') or {}).get('seats') or []:
        sid = s.get('id') if isinstance(s, dict) else None
        if sid is None:
            continue
        if sid in seen:
            f.error('charter_seat_twice', where, f'charter {gname}: seat {sid} is declared twice (GOV-33)')
        seen.add(sid)
    if not (tenant == 'farm' or (not tenant and gname == 'farm')) and 'farm_manager' in gs:
        f.error('charter_farm_manager', where, f'charter {gname}: only the farm charter declares the manager seat (GOV-33)')
    for block, field, floor in CHARTER_FLOORS:
        v = (gs.get(block) or {}).get(field)
        if v is not None and duration_s(v) < duration_s(floor):
            f.error('charter_direction', where, f'charter {gname}: {block}.{field} {v} is below its default {floor} (GOV-33)')
    kinds = (gs.get('promotion') or {}).get('exclude_change_kinds')
    if kinds is not None and 'none' not in kinds:
        f.error('charter_direction', where, f'charter {gname}: promotion.exclude_change_kinds leaves out none (GOV-33)')
    dw = ((gs.get('farm_manager') or {}).get('incident_stop') or {}).get('decide_within')
    if dw is not None and duration_s(dw) > duration_s('7d'):
        f.error('charter_direction', where, f'charter {gname}: incident_stop.decide_within {dw} is above 7d (GOV-33)')


def lint_governance(S, f):
    """Personas of the charters (GOV-4, COM-19), unawareness of bot texts (GOV-29) and no bot reference to governance (GOV-6, GOV-13)."""
    import hashlib
    terms = unaware_terms()
    for tenant, gname, where, gs in S.each('Governance'):
        com = gs.get('commission') or {}
        for s in com.get('seats') or []:
            lens = s.get('lens')
            if lens and not (((com.get('lenses') or {}).get(lens) or {}).get('persona') or {}).get('ref'):
                f.error('lens_persona_missing', where, f'charter {gname}: seat {s.get("id")} names lens {lens} without a persona addendum (COM-19)')
        lint_charter_bounds(tenant, gname, where, gs, f)
        refs = []
        for pos, pv in (gs.get('positions') or {}).items():
            if isinstance(pv, dict) and isinstance(pv.get('persona'), dict):
                refs.append((pos, pv['persona']))
        for lens, lv in (com.get('lenses') or {}).items():
            if isinstance(lv, dict) and isinstance(lv.get('persona'), dict):
                refs.append(('lens', lv['persona']))
        for position, pr in refs:
            sha = pr.get('sha256', '')
            if len(sha) != 64:
                continue  # "..." in an example: nothing is pinned
            path = find_persona(where, pr.get('ref', ''))
            if path is None:
                f.error('persona_missing', where, f'charter {gname}: persona file {pr.get("ref")} is not found (GOV-4)')
                continue
            with open(path, 'rb') as fh:
                data = fh.read()
            text = data.decode('utf-8', errors='replace')
            if hashlib.sha256(data).hexdigest() != sha:
                f.error('persona_hash', where, f'charter {gname}: {pr.get("ref")} does not match its sha256 (GOV-4)')
                continue
            codes = persona_findings(text, position, terms)
            what = f'charter {gname}: persona {pr.get("ref")} ({position})'
            if 'persona_size' in codes:
                f.error('persona_size', where, f'{what} is longer than 4 KiB (GOV-4)')
            if 'persona_link' in codes:
                f.error('persona_link', where, f'{what} holds a link, an address, an id or a secret name (GOV-4)')
            if 'persona_value_path' in codes:
                f.error('persona_value_path', where, f'{what}: the value-path paragraph is only and always in a functionary persona (GOV-4)')
            if 'unaware_text' in codes:
                f.error('unaware_text', where, f'{what} speaks of the seat, the queue, departure, voting or stops (GOV-29)')
    for _, tname, where, ts in S.each('TaskType'):
        if unaware_linked(terms, ts.get('instruction'), False):
            f.error('unaware_text', where, f'type {tname}: the instruction links scores or reviews with a seat, the queue or departure (GOV-29)')
        for e in ts.get('events') or []:
            if governance_name(e):
                f.error('governance_ref', where, f'type {tname}: event {e} is of governance (GOV-6, GOV-13)')
    for _, pname, where, ps in S.each('ToolPlugin'):
        for t in ps.get('tools') or []:
            if isinstance(t, dict) and unaware_linked(terms, t.get('description'), False):
                f.error('unaware_text', where, f'plugin {pname}: the description of {t.get("name")} links scores or reviews with a seat, the queue or departure (GOV-29)')
    for _, rname, where, rs in S.each('Role'):
        for n in (rs.get('actions_may_request') or []) + (rs.get('may_enqueue') or []):
            if governance_name(n):
                f.error('governance_ref', where, f'role {rname}: {n} is of governance (GOV-6, GOV-13)')
    for kind in ('Action', 'MessageType', 'TaskType'):
        for _, n, where, _ in S.each(kind):
            if governance_name(n):
                f.error('governance_ref', where, f'{kind} {n} is named as governance (GOV-6, GOV-13)')
    for _, n, where, rs in S.each('Route'):
        ev = (rs.get('on') or {}).get('event')
        if governance_name(ev, incidents=False):
            f.error('governance_ref', where, f'route {n} starts on {ev} (GOV-29)')


IMPLICATE_FNS = ('lease_holders_on', 'recent_targets', 'worker_of_subject', 'role_failing', 'spend_top')
DETECT_ATOMS = ('eq', 'ne', 'lt', 'le', 'gt', 'ge', 'in')
CONTAIN_SCOPES = ('worker', 'bot', 'bot_group', 'role', 'tasktype', 'resource')


def duration_s(v):
    m = re.fullmatch(r'([0-9]+)([smhd])', v) if isinstance(v, str) else None
    return int(m.group(1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[m.group(2)] if m else 0


def lint_incident_classes(S, f):
    """References of an IncidentClass (MGR-7) and its first measure, a hold with an expiry (HLD-2)."""
    cat = load_yaml(os.path.join(ROOT, 'spec', 'bindings', 'events', 'catalog.yaml')) or {}
    events = {e.get('id') for e in cat.get('entries', []) if isinstance(e, dict)}
    ops = model_registry('control-ops')
    holds = model_registry('holds')
    for _, cname, where, cs in S.each('IncidentClass'):
        if not re.fullmatch(r'[a-z][a-z0-9_]{0,62}', str(cname)):
            f.error('incident_class_ref', where, f'class {cname}: the name is not a short snake_case name of incident records (MGR-7)')
        d = cs.get('detect') or {}
        alarm = d.get('alarm')
        if not (isinstance(alarm, str) and alarm.startswith('alarm.') and alarm in events) or alarm == 'alarm.incident.unhandled.v1':
            f.error('incident_class_ref', where, f'class {cname}: detect.alarm {alarm} is not a code alarm of the events catalog (MGR-7)')
        for w in d.get('when') or []:
            field = (w or {}).get('field', '')
            if (w or {}).get('atom') not in DETECT_ATOMS or not (str(field).startswith('values.') or field in ('subject.kind', 'subject.id')):
                f.error('incident_class_ref', where, f'class {cname}: condition {w} is outside the closed atoms and fields (MGR-7)')
        for fn in cs.get('implicate') or []:
            if (fn or {}).get('fn') not in IMPLICATE_FNS:
                f.error('incident_class_ref', where, f'class {cname}: involvement function {(fn or {}).get("fn")} is not in the closed list (MGR-7)')
        r = cs.get('responses') or {}
        for op in r.get('ops') or []:
            if op not in ops:
                f.error('incident_class_ref', where, f'class {cname}: operation {op} is not in control-ops.yaml (MGR-7)')
        for a in r.get('actions') or []:
            if S.get('Action', a) is None:
                f.error('incident_class_ref', where, f'class {cname}: action {a} is not in the set (MGR-7)')
        for t in r.get('pin_types') or []:
            tt = S.get('TaskType', t)
            if tt is None or not tt.get('pinnable'):
                f.error('incident_class_ref', where, f'class {cname}: pin type {t} is not a pinnable type of the set (MGR-7)')
        if (cs.get('subject') or {}).get('kind') == 'farm' and ('services' in r or 'monitor_scripts' in r):
            f.error('incident_class_ref', where, f'class {cname}: a farm class has no services and no monitoring scripts (MGR-7)')
        for m in cs.get('contain_auto') or []:
            m = m or {}
            scope = m.get('scope') or {}
            effects = m.get('effects') or []
            ok = m.get('op') == 'hold' and len(scope) == 1 and duration_s(m.get('ttl')) > 0 and len(effects) > 0 \
                and all(k in CONTAIN_SCOPES for k in scope) \
                and all(v != '$subject' or k == ((cs.get('subject') or {}).get('kind')) for k, v in scope.items()) \
                and all((holds.get(e) or {}).get('kind') == 'effect' for e in effects)
            if not ok:
                f.error('incident_contain_auto', where, f'class {cname}: a first measure is a hold with one tenant scope, known effects and a ttl (MGR-7, HLD-2)')
    lint_store_low(S, f)


def lint_store_low(S, f):
    """The required class store_low of an incident catalog: subject farm, opened by the core's own
    alarm.store.free.low, without stops_manager (MGR-38). A set without incident classes has no catalog."""
    classes = S.each('IncidentClass')
    if not classes:
        return
    for _, cname, where, cs in classes:
        if cname != 'store_low':
            continue
        if (cs.get('subject') or {}).get('kind') != 'farm':
            f.error('incident_store_low', where, 'class store_low: the subject is the farm (MGR-38)')
        if (cs.get('detect') or {}).get('alarm') != 'alarm.store.free.low.v1':
            f.error('incident_store_low', where, 'class store_low: opens on alarm.store.free.low (MGR-38, CAP-5)')
        if 'stops_manager' in cs:
            f.error('incident_store_low', where, 'class store_low: no stops_manager field (MGR-38)')
        return
    f.error('incident_store_low', classes[0][2], 'the incident catalog has no class store_low (MGR-38)')


# The built-in tools a seat profile may have (MGR-10); impl/internal/core/seatlint.go keeps the same list.
SEAT_BUILTINS = {
    'farm-manager': {'control_ops', 'inspect_request', 'incident_close', 'incident_view', 'service/control', 'monitor/script_run',
                     'grant_decide', 'escalations_take', 'grants_pending', 'takeover_request', 'link_hint', 'attention_notice'},
    'farm-manager-reader': {'file_read'},
}
# The kinds the seat holder never changes (MGR-16): proposals only with kind none or never.
SELF_CHANGE_KINDS = ('Seat', 'IncidentClass', 'Governance', 'Commission', 'Group')
# The target kinds no control operation may name (MGR-16).
SELF_CHANGE_TARGETS = {'seat', 'seat_role', 'incident_class', 'involvement_rule', 'control_ops', 'control_limits', 'breaker',
                       'budget_line', 'group', 'consent_group', 'protected_set', 'commission', 'governance', 'holder', 'auto_stop'}


def self_change_problem():
    """An operation target kind among the MGR-16 targets, or a gap in the forbidden list of proposals (PRP-3)."""
    req = load_json(os.path.join(MODEL, 'p2-control-apply-request.schema.json'))
    kinds = (((req.get('properties') or {}).get('target') or {}).get('properties') or {}).get('kind') or {}
    if SELF_CHANGE_TARGETS & set(kinds.get('enum') or []):
        return True
    pf = load_yaml(os.path.join(MODEL, 'proposable-fields.yaml')) or {}
    covered = set(pf.get('none_only') or [])
    lines = False
    for n in pf.get('never') or []:
        covered |= set((n or {}).get('kinds') or [])
        if (n or {}).get('object') == 'Tenant' and 'spec.budgets.lines' in ((n or {}).get('fields') or []):
            lines = True
    if any((e or {}).get('object') in SELF_CHANGE_KINDS + ('Role',) for e in pf.get('entries') or []):
        return True
    return not lines or not set(SELF_CHANGE_KINDS + ('Role',)) <= covered


def lint_seat_profiles(S, f):
    """For a set with a role of a seat profile: the built-in tools of the seat profiles (MGR-10), the
    operation targets and the forbidden list of proposals (MGR-16, PRP-3)."""
    seat = [(rname, where, rs) for _, rname, where, rs in S.each('Role') if rs.get('profile') in SEAT_BUILTINS]
    if not seat:
        return
    bad = set()
    for tid, e in model_registry('builtin-tools').items():
        for p in e.get('profiles') or []:
            if p in SEAT_BUILTINS and tid not in SEAT_BUILTINS[p]:
                bad.add(p)
    self_change = self_change_problem()
    for rname, where, rs in seat:
        if rs['profile'] in bad:
            f.error('role_profile_forbidden', where, f'role {rname}: profile {rs["profile"]} has a built-in tool outside the seat list (MGR-10)')
        if self_change:
            f.error('manager_self_change', where, f'role {rname}: an operation target or a proposable field reaches a target of MGR-16')


def lint_insight(f, rname, where, rs, types):
    """The insight section of a role (INS-1, INS-2, INS-4): its types are insight types of tier 1 at most that
    the role claims and that take the core's result insight-result; its tools are among the role's own; the
    merge window is at least an hour."""
    ins = rs.get('insight') or {}
    for tt in ins.get('types', []):
        ts = types.get(tt)
        if ts is None:
            if types:
                f.error('ref_missing', where, f'role {rname}: insight type {tt} is not in the set')
            continue
        order = lambda d: (DATA_CLASSES.get(d) or {}).get('order', 0)
        wider = 'max_data' in ins and order(ts.get('data_class')) > order(ins['max_data'])
        if (ts['purpose'] != 'insight' or tt not in rs.get('claims', []) or ts.get('tier', 0) > 1
                or ts.get('result_schema') is not None or wider):
            f.error('insight_type_invalid', where, f'role {rname}: insight type {tt} is an insight type of tier 1 at most, '
                    'claimed by the role, without its own result_schema, of a class not above max_data (INS-1, INS-2)')
    own = set(rs.get('tools', []))
    for t in ins.get('tools', []):
        if t not in own and t.split('/')[0] not in own:
            f.error('insight_wider', where, f'role {rname}: insight tool {t} is not among the role tools (INS-2)')
    mw = ins.get('merge_window')
    if mw is not None and (seconds(mw) or 0) < 3600:
        f.error('insight_merge_window', where, f'role {rname}: insight.merge_window {mw} is shorter than an hour (INS-4)')


def lint_insight_types(S, f, roles):
    """Each insight type is run by exactly one role's insight section (INS-1, INS-4)."""
    for _, tname, where, ts in S.each('TaskType'):
        if ts.get('purpose') != 'insight':
            continue
        n = sum(1 for _, rs in roles.values() if tname in (rs.get('insight') or {}).get('types', []))
        if n != 1:
            f.error('insight_type_invalid', where, f'type {tname}: an insight type is in the insight section of exactly one role, not {n} (INS-1)')


def lint_series(S, f, tools):
    """Observation series (OBV-2, OBV-4, OBV-6): the source exists and fits its kind, the data class is not
    below the source tool's output_class, rollups come from spec/model/rollups.yaml, and a trial guard of the
    tenant names only a series with a complete outcome block."""
    order = lambda d: (DATA_CLASSES.get(d) or {}).get('order', 0)
    series = {}
    for tenant, name, where, ss in S.each('ObservationSeries'):
        series[(tenant, name)] = ss
        if name in CORE_SERIES or name.startswith('farm.'):
            f.error('series_name_reserved', where, f'series {name}: the name belongs to the core series (OBV-6)')
        if ss.get('data_class') not in DATA_CLASSES or DATA_CLASSES[ss['data_class']].get('reserved'):
            f.error('cap_data_class', where, f'series {name}: data class {ss.get("data_class")} is not accepted (OBV-2)')
        src = ss.get('source') or {}
        kind = src.get('kind')
        if kind == 'tool':
            if not src.get('tool') or not src.get('field'):
                f.error('series_source_invalid', where, f'series {name}: a tool source names the tool and its field (OBV-2)')
            elif src['tool'] not in tools:
                if S.names('ToolPlugin'):
                    f.error('ref_missing', where, f'series {name}: tool {src["tool"]} is not in the set')
            elif order(ss.get('data_class')) < order(tools[src['tool']][2].get('output_class')):
                f.error('series_class_below_source', where,
                        f'series {name}: data_class {ss.get("data_class")} is below the output_class of {src["tool"]} (OBV-2)')
        elif src.get('tool'):
            f.error('series_source_invalid', where, f'series {name}: a {kind} source names no tool (OBV-2)')
        elif kind == 'job' and not src.get('field'):
            f.error('series_source_invalid', where, f'series {name}: a job source names the field of run.finished.v1 (OBV-7)')
        for r in ss.get('rollups', []) + ([ss['outcome']['compare']] if (ss.get('outcome') or {}).get('compare') else []):
            if r not in ROLLUPS:
                f.error('rollup_unknown', where, f'series {name}: rollup {r} is not in spec/model/rollups.yaml (OBV-4)')
    for tenant, _, where, ts in S.each('Tenant'):
        for line in (ts.get('insights') or {}).get('extra_guards', []):
            for n in [line.get('metric')] + list(line.get('guards', [])):
                o = (CORE_SERIES.get(n) or {}).get('outcome') if n in CORE_SERIES else (series.get((tenant, n)) or {}).get('outcome')
                if not outcome_complete(o):
                    f.error('guard_not_metric', where,
                            f'tenant {tenant}: insights.extra_guards names {n}, which is not a series with a complete outcome block (OBV-6)')


WAIT_CEILING_S = 7 * 86400  # farm ceiling of a run with wait or await steps (SCR-13)
STEP_WAIT_FIELDS = {'wait': {'for'}, 'await': {'event', 'match', 'timeout'}}


def lint_v02_features(S, f, types, tools, roles, claimed):
    """v0.2 features: the sandbox (SBX-1, MAN-11), script steps wait and await (SCR-13), scripts from git with
    parameters, goals and commands (SCR-14), a scheduled script with parameters (SCR-12), the rule share (DSP-11)."""
    no_sandbox = claimed is not None and 'sandbox' not in claimed
    # SBX-1: tier 2 only in a sandbox plugin, a sandbox plugin holds only tier 2 read tools with target: any.
    for _, pname, where, ps in S.each('ToolPlugin'):
        sandbox = ps.get('placement') == 'sandbox'
        if sandbox and no_sandbox:
            f.error('sandbox_not_claimed', where, f'plugin {pname}: placement: sandbox while the sandbox profile is not claimed (MAN-11, SBX-1)')
        for t in ps.get('tools', []):
            tid = f'{pname}/{t["name"]}'
            if t.get('tier') == 2 and not sandbox:
                f.error('sandbox_placement', where, f'{tid}: a tier 2 tool outside a plugin with placement: sandbox (SBX-1)')
            elif sandbox and (t.get('tier') != 2 or t.get('access') != 'read' or t.get('target') != 'any'):
                f.error('sandbox_placement', where, f'{tid}: a sandbox plugin holds only tier 2 read tools with target: any (SBX-1)')
    for _, tname, twhere, ts in S.each('TaskType'):
        if ts.get('tier') == 2 and no_sandbox:
            f.error('sandbox_not_claimed', twhere, f'type {tname}: tier 2 while the sandbox profile is not claimed (MAN-11, SBX-1)')
    for rname, (where, rs) in roles.items():
        if not any((types.get(tt) or {}).get('tier') == 2 for tt in rs.get('claims', [])):
            continue
        for ref in rs.get('tools', []):
            plugin = ref.split('/', 1)[0]
            ps = next((x for _, n, _, x in S.each('ToolPlugin') if n == plugin), None)
            if ps is not None and ps.get('placement') == 'host':
                f.error('sandbox_live_tools', where, f'role {rname}: claims a tier 2 type and has the host tool {ref} (SBX-1)')
    # SCR-13, SCR-14: scripts from git.
    for tenant, sname, where, sc in S.each('Script'):
        tspec = S.get('Tenant', tenant) or {}
        ceil = seconds((tspec.get('scripts') or {}).get('wait_run_deadline')) or WAIT_CEILING_S
        declared = sc.get('params') or {}
        goals = sc.get('goals') or {}
        for pn, pd in declared.items():
            if pd.get('type') == 'string' and not pd.get('enum') and not pd.get('format'):
                f.error('script_param_invalid', where, f'script {sname}: parameter {pn} is a free string; give enum or format (SCR-14, INJ-1)')
            if pd.get('format') and pd['format'] not in FORMATS:
                f.error('format_unknown', where, f'script {sname}: parameter {pn}: format {pd["format"]!r} is not in formats.yaml')
            if 'default' in pd and errors_of(param_schema(pd), pd['default']):
                f.error('script_param_invalid', where, f'script {sname}: parameter {pn}: default does not pass the declaration (SCR-14)')
        for st in sc.get('steps') or []:
            sid, kind = st.get('id'), st.get('kind')
            if (types.get(st.get('type')) or {}).get('purpose') == 'manage':
                f.error('manage_enqueued', where, f'script {sname}: step {sid} starts {st["type"]} of purpose manage (MGR-4)')
            for pn in param_refs(st):
                if pn not in declared:
                    f.error('script_param_invalid', where, f'script {sname}: step {sid} names the undeclared parameter {pn} (SCR-14)')
            if kind in STEP_WAIT_FIELDS:
                need = STEP_WAIT_FIELDS[kind]
                other = set().union(*STEP_WAIT_FIELDS.values()) - need
                extra = [k for k in st if k in other or k in ('type', 'action', 'goal', 'params', 'args', 'items', 'rollout', 'who')]
                missing = sorted(k for k in need if k not in st)
                if missing or extra:
                    f.error('script_wait_fields', where, f'script {sname}: step {sid} ({kind}) misses {missing} or has {extra} (SCR-13)')
                dur = seconds(st.get('for') or st.get('timeout'))
                if dur and dur > ceil:
                    f.error('script_wait_fields', where, f'script {sname}: step {sid} waits longer than the run deadline (SCR-13)')
            elif any(k in st for k in ('for', 'event', 'match', 'timeout')):
                f.error('script_wait_fields', where, f'script {sname}: step {sid} ({kind}) has a field of wait or await (SCR-13)')
            tpl = (st.get('goal') or {}).get('template')
            if tpl:
                own = tpl in goals
                typed = tpl in ((types.get(st.get('type')) or {}).get('goal_templates') or {})
                if own and typed:
                    f.error('script_param_invalid', where, f'script {sname}: step {sid}: goal {tpl} is in both goals and the type goal_templates (SCR-14)')
                elif not own and types and st.get('type') in types and not typed:
                    f.error('ref_missing', where, f'script {sname}: step {sid}: goal template {tpl} is in neither goals nor the type (SCR-4, SCR-14)')
    for tenant, tname, where, ts in S.each('Tenant'):
        dl = (ts.get('scripts') or {}).get('wait_run_deadline')
        if dl and (seconds(dl) or 0) > WAIT_CEILING_S:
            f.error('script_wait_fields', where, f'tenant {tname}: scripts.wait_run_deadline {dl} is above the farm ceiling of 7d (SCR-13)')
        for cmd, script in (ts.get('script_commands') or {}).items():
            if cmd in BUILTIN_COMMANDS or cmd in (ts.get('commands') or {}):
                f.error('builtin_command', where, f'script command {cmd} takes a built-in or a tenant command (PRM-17, SCR-14)')
            if S.get('Script', script, tname) is None:
                f.error('ref_missing', where, f'script command {cmd}: script {script} is not a kind: Script of tenant {tname}')
        for i, rule in enumerate((ts.get('dispatch') or {}).get('rules') or []):
            if rule.get('max_share') and not rule.get('group'):
                f.error('max_share_without_group', where, f'tenant {tname}: dispatch.rules[{i}] has max_share without group (DSP-11)')
    for tenant, sname, where, ss in S.each('Schedule'):
        sc = S.get('Script', ss.get('script'), tenant) if ss.get('script') else None
        for pn, pd in ((sc or {}).get('params') or {}).items():
            if 'default' not in pd:
                f.error('script_param_invalid', where, f'schedule {sname}: parameter {pn} of script {ss["script"]} has no default (SCR-12, SCR-14)')


def param_schema(pd):
    """JSON schema of one declared script parameter (SCR-14)."""
    out = {'type': pd['type']}
    for k in ('enum', 'minimum', 'maximum'):
        if k in pd:
            out[k] = pd[k]
    if pd.get('format') in FORMATS:
        out['pattern'] = FORMATS[pd['format']].pattern
    return out


def param_refs(node):
    """Names in {param: <name>} references inside a script step."""
    if isinstance(node, dict):
        if set(node) == {'param'} and isinstance(node['param'], str):
            return [node['param']]
        return [x for v in node.values() for x in param_refs(v)]
    if isinstance(node, list):
        return [x for v in node for x in param_refs(v)]
    return []


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
                     'monitor_triggers': [t['triggerid'] for t in spec.get('monitor_triggers', [])],
                     'monitor_items': [t['item'] for t in spec.get('monitor_triggers', [])]}
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
    # SEC-6, TEN-3: a channel, a toolhost and a monitoring adapter act for one tenant and name it,
    # so a revoked secret of theirs holds that tenant's scope.
    for hname, h in hosts.items():
        for pr in (h or {}).get('processes') or []:
            if pr.get('kind') in ('channel', 'toolhost', 'monitor') and not pr.get('tenant'):
                f.error('process_tenant_missing', where,
                        f'machine {hname}: process {pr.get("name")} of kind {pr.get("kind")} names no tenant (SEC-6, TEN-3)')
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
    # SEC-5: each named secret has a rotation method; overlap names its window.
    rot = (topo or {}).get('secrets') or {}
    named = set((topo or {}).get('health_keys') or [])
    named |= {v for v in (((topo or {}).get('watch') or {}).get('notify_ref'), bk.get('key_ref'), bk.get('credentials_ref')) if v}
    named |= {s for h in hosts.values() for pr in (h or {}).get('processes') or [] for s in pr.get('secrets') or []}
    for name in sorted(named):
        m = rot.get(name) if isinstance(rot, dict) else None
        if not isinstance(m, dict) or m.get('rotation') not in ('overlap', 'cutover'):
            f.error('secret_rotation_missing', where, f'secret {name} has no rotation method in secrets (SEC-5)')
        elif m['rotation'] == 'overlap' and not m.get('window'):
            f.error('secret_rotation_missing', where, f'secret {name}: rotation overlap without a window (SEC-5)')


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
    rep.note('examples/deploy/farm.yaml', '[farm_hosts_default] no such file: farm hosts default to core1 and backup1, as in the suite checks (docs/quality.md)')
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

_FARM = load_yaml(os.path.join(EXAMPLES, 'deploy', 'farm.yaml')) or {}
EXAMPLE_PROFILES = ['core'] + (['monitor'] if 'monitoring' in _FARM else []) + \
    (['sandbox'] if any((h or {}).get('kind') == 'sandbox' for h in (_FARM.get('hosts') or {}).values()) else [])
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
