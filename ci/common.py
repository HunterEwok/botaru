# SPDX-License-Identifier: AGPL-3.0-only
"""Shared paths and functions of the skeleton checks.

The repository root is the folder above ci/. Another root is set with the
environment variable BOTARU_ROOT (this is how the checks run on a copy).

Every check message starts with a stable code in square brackets:
rep.error(where, '[link_broken] broken link x'). The code is never
translated or changed; fixtures and issues can refer to it.
"""
import copy
import functools
import json
import os
import re
import sys

import yaml

sys.dont_write_bytecode = True

ROOT = os.path.abspath(os.environ.get('BOTARU_ROOT') or os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
REGISTRY = 'conformance/requirements.yaml'
GENERATED = ('conformance/rules.md', 'conformance/retired.md')
# Folders that are not part of the public repository and are not checked (also in .gitignore).
SKIP_DIRS = frozenset({'.git', '_review', 'node_modules', '.venv', '__pycache__'})
CHECKS = {'fixture', 'store', 'blackbox', 'adapter', 'eval', 'lint'}
VERSIONS = ('v0.1', 'v0.2')
PROFILES = ('core', 'messaging', 'campaign', 'insights', 'manager', 'governance', 'monitor', 'knowledge', 'ingress', 'connections', 'debug')
DEC_RE = re.compile(r'\bD-(\d+)\b')  # decision number D-NN
CODE_RE = re.compile(r'^\[([a-z][a-z0-9_]*)\] ')
VERBOSE = '-v' in sys.argv or bool(os.environ.get('BOTARU_VERBOSE'))


def rel(path):
    return os.path.relpath(path, ROOT).replace(os.sep, '/')


@functools.lru_cache(maxsize=None)
def _tree(root, skip):
    out = []
    for dp, dn, fn in os.walk(root):
        dn[:] = sorted(d for d in dn if d not in skip)
        out.extend(os.path.join(dp, f) for f in sorted(fn))
    return tuple(out)


def walk(exts=None, root=None, skip=SKIP_DIRS):
    """Repository files in order, without SKIP_DIRS.

    The root is walked once per process (run_all runs the checks in one
    process); a folder inside the root is taken from the same list.
    """
    root = os.path.abspath(root or ROOT)
    skip = frozenset(skip)
    if root != ROOT and root.startswith(ROOT + os.sep) and skip == SKIP_DIRS \
            and not set(os.path.relpath(root, ROOT).split(os.sep)) & skip:
        files = [f for f in _tree(ROOT, skip) if f.startswith(root + os.sep)]
    else:
        files = _tree(root, skip)
    return [f for f in files if exts is None or f.endswith(exts)]


def read(path):
    with open(path, encoding='utf-8') as fh:
        return fh.read()


# ---------------------------------------------------------------- YAML and JSON
# One loader for all checks: YAML 1.2 (booleans are only true and false, the key
# on is a string), a duplicate key is an error, not a silent win of the last one.
_Base = getattr(yaml, 'CSafeLoader', yaml.SafeLoader)


class Loader(_Base):
    """SafeLoader with YAML 1.2 booleans and no duplicate keys."""

    def construct_mapping(self, node, deep=False):
        if isinstance(node, yaml.MappingNode):
            seen = {}
            for k, _ in node.value:
                if k.tag == 'tag:yaml.org,2002:merge':
                    continue
                key = self.construct_object(k, deep=True)
                try:
                    hash(key)
                except TypeError:
                    continue
                if key in seen:
                    raise yaml.constructor.ConstructorError(
                        None, None, f'duplicate key {key!r}, first on line {seen[key] + 1}', k.start_mark)
                seen[key] = k.start_mark.line
        return super().construct_mapping(node, deep)


Loader.yaml_implicit_resolvers = {k: [r for r in v if r[0] != 'tag:yaml.org,2002:bool']
                                  for k, v in yaml.SafeLoader.yaml_implicit_resolvers.items()}
Loader.add_implicit_resolver('tag:yaml.org,2002:bool', re.compile(r'^(?:true|True|TRUE|false|False|FALSE)$'),
                             list('tTfF'))


def no_dup_json(pairs):
    d = {}
    for k, v in pairs:
        if k in d:
            raise ValueError(f'duplicate key {k!r}')
        d[k] = v
    return d


@functools.lru_cache(maxsize=None)
def _load_all(path, mtime):
    with open(path, encoding='utf-8') as fh:
        return list(yaml.load_all(fh, Loader=Loader))


def load_yaml_all(path):
    """All documents of a YAML file; parsing is cached for the run, the caller gets a copy."""
    return copy.deepcopy(_load_all(os.path.abspath(path), os.path.getmtime(path)))


def load_yaml(path):
    docs = load_yaml_all(path)
    if len(docs) > 1:
        raise yaml.YAMLError(f'{rel(path)}: the file holds several documents, one was expected')
    return docs[0] if docs else None


def load_yaml_text(text):
    return yaml.load(text, Loader=Loader)


@functools.lru_cache(maxsize=None)
def _load_json(path, mtime):
    with open(path, encoding='utf-8') as fh:
        return json.load(fh, object_pairs_hook=no_dup_json)


def load_json(path):
    return copy.deepcopy(_load_json(os.path.abspath(path), os.path.getmtime(path)))


# ---------------------------------------------------------------- rule registry
def registry():
    try:
        return load_yaml(os.path.join(ROOT, REGISTRY)) or []
    except yaml.YAMLError as e:
        # the checks cannot run without the registry; check_registry.py prints the details
        print(f'  error  [registry_unreadable] {REGISTRY}: the registry cannot be read: {" ".join(str(e).split())[:200]}')
        sys.exit(1)


def rules(status=None):
    """Registry rows by id; status='active' keeps only active rules."""
    return {r['id']: r for r in registry()
            if isinstance(r, dict) and isinstance(r.get('id'), str) and (status is None or r.get('status') == status)}


def families(rows):
    return sorted({r['id'].split('-')[0] for r in rows if isinstance(r, dict) and isinstance(r.get('id'), str)})


def id_regex(rows):
    fams = '|'.join(sorted(families(rows), key=len, reverse=True))
    return re.compile(r'\b(' + fams + r')-(\d+)([a-z]?)\b')


def id_key(rid):
    m = re.match(r'^([A-Z]+)-(\d+)([a-z]?)$', rid)
    return (m.group(1), int(m.group(2)), m.group(3)) if m else ('~', 0, rid)


def known_decisions():
    text = read(os.path.join(ROOT, 'docs', 'decisions.md'))
    return {int(n) for n in re.findall(r'^\| D-(\d+) \|', text, re.M)}


# ---------------------------------------------------------------- JSON schemas
@functools.lru_cache(maxsize=None)
def _schemas():
    out = {}
    for f in walk(('.schema.json',)):
        try:
            doc = load_json(f)
        except ValueError:
            continue  # check_schemas.py reports the read error
        if isinstance(doc, dict) and isinstance(doc.get('$id'), str):
            out.setdefault(doc['$id'], (rel(f), doc))
    return out


def schemas():
    """{$id: (path, schema)} of every *.schema.json in the repository; the first schema with an $id wins."""
    return _schemas()


@functools.lru_cache(maxsize=None)
def schema_registry():
    from referencing import Registry, Resource
    return Registry().with_resources((sid, Resource.from_contents(d)) for sid, (_, d) in schemas().items())


# ---------------------------------------------------------------- call bodies of fixtures
# Lead decision 11: the call.request body of a behaviour fixture passes the request
# schema of its row in spec/ports/operations.yaml. Only what conformance/runner.md
# lists is allowed (sections "Call step body", "Decide body", "Short ids and
# references"): a short id <prefix>_<number>, a training handle human:<handle>,
# the references $last and $from:<case> in an id field of the body (ref_fields and
# REF_FIELDS), an omitted nonce and the value $stale for NONCE_OPS. A step that
# expects a schema rejection (expect.stage: schema or reason: input_schema_invalid)
# is not checked.
SHORT_ID = re.compile(r'^[a-z]{1,8}(?:_[a-z0-9]+)*_\d+(?::[1-9]\d*)?$')
HUMAN_HANDLE = re.compile(r'^human:(?!p_[0-9A-HJKMNP-TV-Z]{26}$)[a-z][a-z0-9_-]*$')
_FAKE_ULID = '0' * 26
OPS_FILE = 'spec/ports/operations.yaml'
NONCE_OPS = ('P1.decide', 'P1.card.details', 'P1.card.remind', 'P1.campaign.control', 'P1.script.control')
REF_FIELDS = {'id': f'x_{_FAKE_ULID}', 'file_ref': 'file:' + 'A' * 22, 'handles': 'A' * 16}


def ref_fields(op_id):
    """Body fields where conformance/runner.md allows a $last or $from:<case> reference."""
    if op_id in ('P1.decide', 'P1.campaign.control', 'P2.hold.release') or op_id.startswith('P1.card.'):
        return ('id',)
    return {'P1.file.get': ('file_ref',), 'P1.msg.ack': ('handles',), 'P1.msg.nack': ('handles',)}.get(op_id, ())


def _is_ref(v):
    return isinstance(v, str) and (v == '$last' or v.startswith('$from:'))


def _expand_short(x):
    if isinstance(x, dict):
        return {k: _expand_short(v) for k, v in x.items()}
    if isinstance(x, list):
        return [_expand_short(v) for v in x]
    if isinstance(x, str) and SHORT_ID.match(x):
        head, _, ver = x.partition(':')
        return f'{head.split("_")[0]}_{_FAKE_ULID}' + (f':{ver}' if ver else '')
    if isinstance(x, str) and HUMAN_HANDLE.match(x):
        return f'human:p_{_FAKE_ULID}'
    return x


def runner_body(op_id, request):
    """The body as the runner sends it: with the substitutions runner.md allows."""
    body = _expand_short(request if request is not None else {})
    if isinstance(body, dict):
        for field in ref_fields(op_id):
            v = body.get(field)
            if _is_ref(v):
                body[field] = REF_FIELDS[field]
            elif isinstance(v, list):
                body[field] = [REF_FIELDS[field] if _is_ref(x) else x for x in v]
        if op_id in NONCE_OPS and body.get('nonce') == '$stale':
            body['nonce'] = 'A' * 22
    return body


STAND_LOOP_HINTS = {'concurrent', 'point', 'day', 'guard', 'reason', 'decide_within'}


def _seconds(v):
    m = re.fullmatch(r'([1-9][0-9]{0,5})(s|m|h|d)', str(v))
    return int(m.group(1)) * {'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[m.group(2)] if m else 60


def stand_body(op, step):
    """Request body of the test-stand port that the runner builds from a stand step by the table in conformance/runner.md.

    clock.advance: by (a duration or a deadline name, the runner converts it to seconds) -> by_s;
    loop.tick: the refining fields (concurrent, point, day and others) are for the runner, the body gets loop and copies;
    process.stop: process (a stand worker name or core) -> {kind, worker_id}, signal: 9 -> mode: kill, otherwise graceful;
    during and item name the moment of the stop and do not go into the body. Other operations pass as they are, without op.
    """
    st = dict(step)
    st.pop('op', None)
    if op in ('clock.set', 'clock.advance'):
        body = {'op': op}
        if 'at' in st:
            body['at'] = st['at']
        if 'by' in st or 'by_s' in st:
            body['by_s'] = st.get('by_s', _seconds(st.get('by')))
        return body
    if op == 'loop.tick':
        return {k: v for k, v in st.items() if k not in STAND_LOOP_HINTS}
    if op == 'process.stop':
        proc = st.get('process')
        if isinstance(proc, str):
            proc = {'kind': 'core'} if proc == 'core' else {'kind': 'worker', 'worker_id': 'wk_' + '0' * 26}
        return {'process': proc, 'mode': st.get('mode') or ('kill' if st.get('signal') == 9 else 'graceful')}
    return st


def stand_body_errors(doc):
    """[(step path, operation map row id, message)] for stand steps whose body fails the request schema of T.<op>."""
    out = []
    ops = ops_by_name()
    for path, node in iter_nodes(doc):
        st = node.get('stand')
        if not isinstance(st, dict) or not isinstance(st.get('op'), str):
            continue
        e = ops.get(('T', st['op']))
        if not e or not e.get('request'):
            out.append((f'{path}/stand', f'T.{st["op"]}', 'the operation is not in the operation map'))
            continue
        v = _request_validator(e['request'])
        if v is None:
            continue
        errs = list(v.iter_errors(stand_body(st['op'], st)))
        if errs:
            from jsonschema.exceptions import best_match
            err = best_match(errs)
            at = '/'.join(str(x) for x in err.absolute_path)
            out.append((f'{path}/stand', e['id'], f'{e["request"]}{"#/" + at if at else ""}: {err.message[:160]}'))
    return out


@functools.lru_cache(maxsize=None)
def ops_by_name():
    """{(port, operation): row of spec/ports/operations.yaml}."""
    doc = load_yaml(os.path.join(ROOT, OPS_FILE)) or {}
    out = {}
    for e in doc.get('entries', []) if isinstance(doc, dict) else []:
        if isinstance(e, dict) and e.get('port') and e.get('op'):
            out[(str(e['port']), str(e['op']))] = e
    return out


@functools.lru_cache(maxsize=None)
def _request_validator(name):
    from jsonschema import Draft202012Validator
    sid = f'urn:botaru:schema:{name}:1'
    if sid not in schemas():
        return None
    return Draft202012Validator({'$ref': sid}, registry=schema_registry())


def expects_schema_reject(expect):
    if not isinstance(expect, dict):
        return False
    return expect.get('stage') == 'schema' or str(expect.get('reason') or '').split('(')[0] == 'input_schema_invalid'


def call_body_errors(doc):
    """[(step path, operation map row id, message)] for call.request bodies that fail the request schema."""
    out = []
    ops = ops_by_name()
    for path, node in iter_nodes(doc):
        call = node.get('call')
        if not isinstance(call, dict) or not isinstance(call.get('port'), str):
            continue
        op = call.get('op') or call.get('method')
        e = ops.get((call['port'].upper(), str(op)))
        if not e or not e.get('request') or expects_schema_reject(node.get('expect')):
            continue
        v = _request_validator(e['request'])
        if v is None:
            continue  # no schema: check_crossrefs.py reports it ([ops_schema_missing])
        body = runner_body(e['id'], call.get('request'))
        errs = [x for x in v.iter_errors(body)
                if not (e['id'] in NONCE_OPS and x.validator == 'required' and "'nonce'" in x.message)]
        if errs:
            from jsonschema.exceptions import best_match
            err = best_match(errs)
            at = '/'.join(str(x) for x in err.absolute_path)
            out.append((f'{path}/call', e['id'], f'{e["request"]}{"#/" + at if at else ""}: {err.message[:160]}'))
    return out


def iter_nodes(node, path=''):
    """Every dict of the tree with its JSON Pointer path."""
    if isinstance(node, dict):
        yield path, node
        for k, v in node.items():
            yield from iter_nodes(v, f'{path}/{k}')
    elif isinstance(node, list):
        for i, v in enumerate(node):
            yield from iter_nodes(v, f'{path}/{i}')


# ---------------------------------------------------------------- Markdown tables
def md_cells(line):
    """Cells of a Markdown table row; a | inside `code` does not split a cell."""
    parts, cur, tick = [], '', False
    for ch in line.strip()[1:-1]:
        if ch == '`':
            tick = not tick
        if ch == '|' and not tick:
            parts.append(cur.strip())
            cur = ''
        else:
            cur += ch
    parts.append(cur.strip())
    return parts


# ---------------------------------------------------------------- report
WHERE = re.compile(r'^(?P<file>[^\s:#\[]+)(?::(?P<line>\d+))?')


class Report:
    """Collects the errors and notes of one check and prints the summary.

    A message starts with a code in square brackets. Errors are always printed,
    notes with the -v flag (or BOTARU_VERBOSE=1) and in GitHub Actions.
    """

    def __init__(self, name):
        self.name = name
        self.errors = []
        self.notes = []
        self.infos = []

    @staticmethod
    def _checked(msg):
        if not CODE_RE.match(msg):
            raise ValueError(f'check message without a code in square brackets: {msg[:80]!r}')
        return msg

    def error(self, where, msg):
        self.errors.append((str(where), self._checked(msg)))

    def note(self, where, msg):
        self.notes.append((str(where), self._checked(msg)))

    def info(self, msg):
        """A summary line that is always printed (numbers, totals)."""
        self.infos.append(msg)

    def _gh(self, level, where, msg):
        code, rest = CODE_RE.match(msg).group(1), msg[CODE_RE.match(msg).end():]
        m = WHERE.match(where)
        loc = ''
        if m and os.path.isfile(os.path.join(ROOT, m['file'])):
            loc = f' file={m["file"]}' + (f',line={m["line"]}' if m['line'] else '')
        return f'::{level}{loc}::[{code}] {self.name}: {where}: {rest}'

    def finish(self, limit=200):
        gh = bool(os.environ.get('GITHUB_ACTIONS'))
        for msg in self.infos:
            print(f'  {msg}')
        if gh or VERBOSE:
            for where, msg in self.notes[:limit]:
                print(self._gh('notice', where, msg) if gh else f'  note   {msg[:CODE_RE.match(msg).end()]}{where}: {msg[CODE_RE.match(msg).end():]}')
        for where, msg in self.errors[:limit]:
            m = CODE_RE.match(msg)
            print(self._gh('error', where, msg) if gh else f'  error  {msg[:m.end()]}{where}: {msg[m.end():]}')
        if len(self.errors) > limit:
            print(f'  ... and {len(self.errors) - limit} more errors')
        status = 'ok' if not self.errors else f'{len(self.errors)} errors'
        hint = '' if not self.notes or gh or VERBOSE else ' (show: -v)'
        print(f'{self.name}: {status}, notes {len(self.notes)}{hint}')
        sys.exit(1 if self.errors else 0)
