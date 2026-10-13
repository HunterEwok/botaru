#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Public-safety: the public repository holds no secrets and no data of real installations.

All files that will enter the repository are checked (with git: tracked ones
and new ones not in .gitignore; without git: all files except SKIP_DIRS), and their paths:
- Python bytecode (__pycache__, *.pyc) and any non-text files;
- strings that look like keys and tokens, including POSTGRES_PASSWORD= and BOT_TOKEN:;
- IPv4 and IPv6 addresses outside the documentation ranges (192.0.2.0/24,
  198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32, loopback);
- domains outside example.org and the standards list: generic zones (com, org, net,
  edu, gov, dev, app and others) and all two-letter country zones, also
  in `backticks`, and any address after http(s):// and www.;
- mail addresses outside example.org;
- time zones other than UTC and Europe/Berlin (IANA names, timezone/tz values,
  zone abbreviations, time offsets), EPSG codes other than 4326 and 3857, UTM
  zones, coordinate pairs;
- forbidden words of the organization: the list is not stored in the repository, it comes
  from the file BOTARU_FORBIDDEN_FILE or from the text BOTARU_FORBIDDEN (a GitHub secret);
  lines `p <stem>` (start of a word) and `w <word>` (whole word), # comment;
- tenants only demo, demo2 and farm in all folders;
- no Cyrillic character in any public file or path ([cyrillic]): the project is English only.

  python3 -I ci/public_safety.py             the files that will enter the repository
  python3 -I ci/public_safety.py --history   forbidden words in every file of every commit (all refs)
"""
import bisect
import hashlib
import json
import ipaddress
import os
import re
import subprocess
import sys

import yaml

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import ROOT, SKIP_DIRS, Report, iter_nodes, load_json, load_yaml_all, read, rel, walk

rep = Report('public-safety')
DOC_NETS = [ipaddress.ip_network(n) for n in (
    '192.0.2.0/24', '198.51.100.0/24', '203.0.113.0/24', '2001:db8::/32', '127.0.0.0/8', '0.0.0.0/32')]
# Domains of standards and specifications the skeleton refers to.
OK_DOMAINS = {
    'json-schema.org', 'www.gnu.org', 'gnu.org', 'fsf.org', 'spdx.org', 'github.com', 'docs.github.com',
    'modelcontextprotocol.io', 'cloudevents.io', 'opentelemetry.io', 'semver.org', 'adr.github.io',
    'www.rfc-editor.org', 'yaml.org', 'datatracker.ietf.org', 'core.telegram.org', 'www.zabbix.com', 'spec.openapis.org',
    'docs.anthropic.com', 'api.anthropic.com', 'pypi.org',
    # Go module hosts of impl/go.mod and impl/go.sum (D-92): a new module host is added here on purpose
    'go.dev', 'pkg.go.dev', 'golang.org', 'proxy.golang.org', 'honnef.co',
}
# Zones without a country: all generic zones that do not collide with field and
# method names, and all two-letter country zones (ISO 3166). Zones that collide with
# file extensions (md, py, sh) and with fields (id, info, name, group, services,
# store, today) are caught only in http(s):// addresses and with www.
GENERIC_TLDS = ('com', 'org', 'net', 'edu', 'gov', 'mil', 'int', 'biz', 'pro', 'aero', 'coop', 'museum', 'mobi',
                'asia', 'tel', 'travel', 'jobs', 'xxx', 'post', 'dev', 'app', 'cloud', 'online', 'site', 'tech',
                'xyz', 'shop', 'top', 'club', 'blog', 'website', 'digital', 'email', 'systems', 'solutions',
                'company', 'agency', 'network', 'software', 'science', 'research', 'institute', 'foundation')
CC_TLDS = tuple('''ac ad ae af ag ai al am ao aq ar as at au aw ax az ba bb bd be bf bg bh bi bj bm bn bo br bs bt bw
by bz ca cc cd cf cg ch ci ck cl cm cn co cr cu cv cw cx cy cz de dj dk dm do dz ec ee eg er es et eu fi fj fk fm fo fr
ga gd ge gf gg gh gi gl gm gn gp gq gr gs gt gu gw gy hk hm hn hr ht hu ie il im in io iq ir is it je jm jo jp ke kg
kh ki km kn kp kr kw ky kz la lb lc li lk lr ls lt lu lv ly ma mc me mg mh mk ml mm mn mo mp mq mr ms mt mu mv mw mx
my mz na nc ne nf ng ni nl no np nr nu nz om pa pe pf pg ph pk pl pm pn pr ps pt pw qa re ro rs ru rw sa sb sc sd se
sg si sk sl sm sn so sr ss st su sv sx sy sz tc td tf tg th tj tk tl tm tn to tr tt tv tw tz ua ug uk us uy uz va vc
ve vg vi vn vu wf ws ye yt za zm zw'''.split())
# Names from the skeleton text that look like a domain but are not one.
NOT_DOMAINS = {'notice.ai', 'polkit.result.no', 'main.nf', 'requirements.in'}
DOMAIN = re.compile(r'(?i)(?<![\w.@-])((?:[a-z0-9-]+\.)+(?:' + '|'.join(GENERIC_TLDS + CC_TLDS) + r'))\b(?![\w-])'
                    r'|(?<![\w.@-])(www\.(?:[a-z0-9-]+\.)+[a-z]{2,24})\b(?![\w-])')
URL_HOST = re.compile(r'(?i)\bhttps?://([a-z0-9.-]+)')
KEYLIKE = [(re.compile(p), why) for p, why in (
    (r'sk-ant-[A-Za-z0-9_-]{6,}', 'Anthropic key'),
    (r'\bsk-[A-Za-z0-9]{20,}', 'model provider key'),
    (r'\bghp_[A-Za-z0-9]{10,}|\bgithub_pat_[A-Za-z0-9_]{10,}|\bgh[ousr]_[A-Za-z0-9]{20,}', 'GitHub token'),
    (r'\bxox[abposr]-[A-Za-z0-9-]{6,}', 'Slack token'),
    (r'\bAKIA[0-9A-Z]{12,}', 'AWS key'),
    (r'-----BEGIN [A-Z ]*PRIVATE KEY', 'private key'),
    (r'\b\d{8,10}:[A-Za-z0-9_-]{30,}', 'Telegram bot token'),
    (r'(?i)(?<![a-z0-9])(?:[a-z0-9]+_)*(password|passwd|secret|token|api_?key)["\']?[ \t]*[:=][ \t]*["\']?'
     r'(?!CANARY-NOT-A-SECRET)[A-Za-z0-9/+_-]{16,}', 'secret value'),
)]
IPV4 = re.compile(r'(?<![\w.])(\d{1,3}(?:\.\d{1,3}){3})(?:/\d+)?(?![\w.])')
IPV6 = re.compile(r'(?<![\w:.])([0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7})(?![\w:])')
EMAIL = re.compile(r'(?<![\w.+-])[\w.+-]+@[A-Za-z][\w-]*(?:\.[\w-]+)+')
OK_TZ = {'UTC', 'Etc/UTC', 'Europe/Berlin'}
OK_ABBR = {'GMT', 'CET', 'CEST', 'UTC'}  # zones of UTC and Europe/Berlin
IANA_TZ = re.compile(r'\b(?:Africa|America|Antarctica|Arctic|Asia|Atlantic|Australia|Europe|Indian|Pacific|Etc)'
                     r'/[A-Z][A-Za-z_+-]+(?:/[A-Z][A-Za-z_]+)?')
TZ_KEY = re.compile(r'(?i)\b(?:timezone|time_zone|tz)["\']?[ \t]*[:=][ \t]*["\']?([A-Za-z][A-Za-z_/+-]*)')
TZ_ABBR = re.compile(r'\b(EET|EEST|MSK|WET|WEST|BST|PST|PDT|EST|EDT|CDT|MST|MDT|JST|KST|AEST|AEDT|HKT|SGT)\b')  # ps: pattern
TZ_OFFSET = re.compile(r'T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?([+-]\d{2}:?\d{2})\b')
OK_OFFSETS = {'+00:00', '-00:00', '+0000', '+01:00', '+0100', '+02:00', '+0200'}
EPSG = re.compile(r'\bEPSG:(\d+)')
OK_EPSG = {'4326', '3857'}
UTM = re.compile(r'(?i)\bUTM[ \t]*(?:zone[ \t]*)?\d{1,2}[ \t]*[NS]?\b')
COORD_PAIR = re.compile(r'(?<![\w.])(-?\d{1,3}\.\d{3,})[ \t]*[,;][ \t]*(-?\d{1,3}\.\d{3,})(?![\w.])')
COORD_KEY = re.compile(r'(?i)\b(?:lat|latitude|lon|lng|longitude)["\']?[ \t]*[:=][ \t]*-?\d{1,3}\.\d{2,}')
# The tenant key counts only in key position (line start, list item, after { , quote or backtick) and with a lone
# value before the line end, a comma, a brace or a comment, so English prose after the word is not read as a tenant
# name; the forms with an equals sign and the command line option count anywhere.
TENANT = re.compile(r'(?m)(?:^[ \t]*(?:-[ \t]+)?|[{,"\'`])tenant(?:_id)?["\'`]?[ \t]*:[ \t]*["\']?(<[a-z]+>|\{[a-z]+\}|[A-Za-z0-9_-]+)(?=["\'`]?[ \t]*(?:$|[#,}\]]))'
                    r'|(?<![\w:.])tenant(?:_id)?=["\']?(<[a-z]+>|\{[a-z]+\}|[A-Za-z0-9_-]+)'
                    r'|--tenant[= ]["\']?([A-Za-z0-9_-]+)')
OK_TENANTS = {'demo', 'demo2', 'farm', '<t>', '<tenant>', '<name>', '{tenant}', 'null', 'none', 'None', 'string', 'object'}
TENANT_KEYS = ('tenant', 'tenant_id', 'tenants')
BINARY_OK = set()  # non-text files allowed for publication (none so far)
SKIP = {'LICENSE'}
WORD = re.compile(r'[^\W\d_]{2,}')
# A narrow exception for the owner copyright line (lead decision, 2026-10-12): in README.md and
# LICENSE the words at the start of the line "Copyright (C) 2026 <owner>" are not checked for forbidden
# words. The start of the line is stored as a SHA-256 hash and a length, so the name does not enter this
# file. The rest of such a line and all other lines of these files are checked as usual.
COPYRIGHT_FILES = {'README.md', 'LICENSE'}
COPYRIGHT_PREFIX = (29, '7b6135318c817003c34fafc6caf70de35da4711d53bb6b84c82e9a522172379c')


def copyright_spans(rp, text):
    """Spans [start, end) of the text with the allowed start of a copyright line."""
    if rp not in COPYRIGHT_FILES:
        return []
    n, digest = COPYRIGHT_PREFIX
    spans, pos = [], 0
    for line in text.split('\n'):
        head = line[:n]
        if head.startswith('Copyright (C) 2026 ') and hashlib.sha256(head.encode('utf-8')).hexdigest() == digest:
            spans.append((pos, pos + n))
        pos += len(line) + 1
    return spans


# A narrow exception for the owner account (D-89, lead order 2026-10-12): the project owner name
# as a separate word is allowed only in .github/CODEOWNERS (only as @<name>), in CLA.md
# (the parties of the agreement) and in the Go module path (OWNER_MODULE below). The name is stored as a SHA-256 hash and the length of the lowercase
# word; any other word of these files and this word in any other file are checked as usual.
OWNER_WORD = (10, '288927853ffd3e6c2933cf5706e1999d5109178f5ebac01b1c29476d1dbe2348')
OWNER_FILES = {'.github/CODEOWNERS': re.compile(r'(?<![\w@])@([^\W\d_]+)'), 'CLA.md': re.compile(r'([^\W\d_]+)')}


# The Go module path of impl/ holds the owner account (D-92): in impl/go.mod and impl/**/*.go the name is
# allowed only as github.com/<name>/botaru, followed by / or the end of the path.
OWNER_MODULE = re.compile(r'(?<![\w.])github\.com/([^\W\d_]+)/botaru(?![\w.-])')


def owner_rx(rp):
    if rp == 'impl/go.mod' or (rp.startswith('impl/') and rp.endswith('.go')):
        return OWNER_MODULE
    return OWNER_FILES.get(rp)


def owner_spans(rp, text):
    """Spans [start, end) of the text where the owner name stands in the allowed form."""
    rx = owner_rx(rp)
    if rx is None:
        return []
    n, digest = OWNER_WORD
    return [(m.start(1), m.end(1)) for m in rx.finditer(text)
            if len(m.group(1)) == n and hashlib.sha256(m.group(1).lower().encode('utf-8')).hexdigest() == digest]


# ---------------------------------------------------------------- forbidden words
def forbidden_missing():
    """No list: a note locally and in a pull request (the workflow does not give the secret to PR code), otherwise
    an error in CI."""
    msg = ('[forbidden_missing] no forbidden word list: set BOTARU_FORBIDDEN_FILE (path to a file) '
           'or BOTARU_FORBIDDEN (text, a GitHub secret); words were not checked')
    if not os.environ.get('GITHUB_ACTIONS'):
        rep.note('BOTARU_FORBIDDEN', msg)
        return
    if os.environ.get('GITHUB_EVENT_NAME', '').startswith('pull_request'):
        rep.note('BOTARU_FORBIDDEN', msg + ' (pull request: the owner checks the words locally before merging)')
    else:
        rep.error('BOTARU_FORBIDDEN', msg)


def load_forbidden():
    """(stems, words) in lowercase, or None if there is no list."""
    text, src = None, 'BOTARU_FORBIDDEN'
    path = os.environ.get('BOTARU_FORBIDDEN_FILE')
    if path:
        src = 'BOTARU_FORBIDDEN_FILE'
        try:
            text = read(path)
        except (OSError, UnicodeDecodeError):
            rep.error(src, '[forbidden_unreadable] the file from BOTARU_FORBIDDEN_FILE cannot be read')
            return None
    elif os.environ.get('BOTARU_FORBIDDEN'):
        text = os.environ['BOTARU_FORBIDDEN']
    if not text or not text.strip():
        forbidden_missing()
        return None
    stems, words = set(), set()
    for n, line in enumerate(text.splitlines(), 1):
        # a pasted secret can start with a byte order mark or an invisible space
        parts = line.lstrip('\ufeff\u200b\u2060\u00a0').split()
        if not parts or parts[0].startswith('#'):
            continue
        if len(parts) == 2 and parts[0] == 'p':
            stems.add(parts[1].lower())
        elif len(parts) == 2 and parts[0] == 'w':
            words.add(parts[1].lower())
        else:
            # the line content is not printed: the list is secret
            rep.error(f'{src}:{n}', '[forbidden_form] a list line is not of the form "p <stem>", "w <word>" or "# comment"')
    return stems, words


def is_forbidden(token, stems, words):
    return token in words or any(token.startswith(st) for st in stems)


# ---------------------------------------------------------------- file list
def files():
    top = subprocess.run(['git', '-C', ROOT, 'rev-parse', '--show-toplevel'], capture_output=True, text=True)
    if top.returncode == 0:
        out = subprocess.run(['git', '-C', ROOT, 'ls-files', '-z', '--cached', '--others', '--exclude-standard', '--', '.'],
                             capture_output=True, text=True)
        if out.returncode == 0:
            paths = [os.path.join(ROOT, p) for p in out.stdout.split('\0') if p]
            return sorted(p for p in paths if os.path.isfile(p))
    return walk(skip=SKIP_DIRS)


def ok_domain(d):
    d = d.lower().rstrip('.')
    return d in OK_DOMAINS or d in NOT_DOMAINS or d == 'localhost' or re.search(r'(^|\.)example\.(org|com|net)$', d) is not None \
        or re.fullmatch(r'[0-9.]+', d) is not None


def tenant_values(node):
    for _, n in iter_nodes(node):
        for k in TENANT_KEYS:
            v = n.get(k)
            for x in (v if isinstance(v, list) else [v]):
                if isinstance(x, str):
                    yield k, x


# Any Cyrillic letter (U+0400..U+052F, U+1C80..U+1C8F, U+2DE0..U+2DFF, U+A640..U+A69F): the project is
# English only. The forbidden word list is outside the repository and is not scanned.
CYRILLIC = re.compile('[\u0400-\u052f\u1c80-\u1c8f\u2de0-\u2dff\ua640-\ua69f\U0001e030-\U0001e08f]')


def cyrillic(rp, text):
    """One error per file: the first line with Cyrillic and the number of such lines."""
    hit = [n for n, ln in enumerate(text.split('\n'), 1) if CYRILLIC.search(ln)]
    if hit:
        rep.error(f'{rp}:{hit[0]}', f'[cyrillic] {len(hit)} lines with Cyrillic characters, first here: '
                  'the project is English only (vocabulary of parsed strings in ci/README.md)')
    return bool(hit)


PATTERN_MARK = '# ps: pattern'  # a line with a pattern of the check itself: places and zones are not searched in it


def scan(rp, text, lines=True):
    """All patterns over the file text at once; the error place is file:line."""
    starts = [0] + [m.end() for m in re.finditer('\n', text)] if lines else [0]
    marked = {n for n, ln in enumerate(text.split('\n'), 1) if ln.rstrip().endswith(PATTERN_MARK)} if lines else set()
    seen = set()

    def at(pos):
        return f'{rp}:{bisect.bisect_right(starts, pos)}' if lines else rp

    def err(pos, code, msg, value=None):
        where = at(pos)
        if value is not None:
            if (where, code, value) in seen:
                return
            seen.add((where, code, value))
        rep.error(where, f'[{code}] {msg}')

    def place_ok(pos):
        return lines and bisect.bisect_right(starts, pos) in marked

    for pat, why in KEYLIKE:
        for m in pat.finditer(text):
            err(m.start(), 'secret_like', why, why)
    for m in IPV4.finditer(text):
        try:
            ip = ipaddress.ip_address(m.group(1))
        except ValueError:
            continue
        if not any(ip in net for net in DOC_NETS):
            err(m.start(), 'ip_not_doc', f'address {m.group(0)} is outside the documentation ranges')
    if '::' in text or ':' in text:
        for m in IPV6.finditer(text):
            try:
                ip = ipaddress.ip_address(m.group(1))
            except ValueError:
                continue
            if ip.version == 6 and not (ip.is_loopback or ip.is_unspecified or any(ip in n for n in DOC_NETS if n.version == 6)):
                err(m.start(), 'ip_not_doc', f'address {m.group(1)} is outside the documentation ranges')
    for m in URL_HOST.finditer(text):
        if not ok_domain(m.group(1)):
            err(m.start(), 'domain_not_example', f'address {m.group(1)} in a link is outside example.org and the standards list')
    wo_urls = URL_HOST.sub(lambda m: ' ' * len(m.group(0)), text)
    for m in DOMAIN.finditer(wo_urls):
        d = m.group(1) or m.group(2)
        if not ok_domain(d):
            err(m.start(), 'domain_not_example', f'domain {d.lower()} is outside example.org and the standards list')
    if '@' in text:
        for m in EMAIL.finditer(text):
            a = m.group(0)
            if re.search(r'@(?:[\w-]+\.)*example\.(?:org|com|net)$', a) or a.endswith(('.service', '.timer')):
                continue
            err(m.start(), 'email_not_example', f'mail address {a} is outside example.org')
    for m in IANA_TZ.finditer(text):
        if m.group(0) not in OK_TZ and not place_ok(m.start()):
            err(m.start(), 'timezone_place', f'zone {m.group(0)}: public files use UTC, examples use Europe/Berlin', m.group(0))
    for m in TZ_KEY.finditer(text):
        v = m.group(1)
        # IANA_TZ catches IANA names; here abbreviations outside TZ_ABBR (IST, AST and others)
        if re.fullmatch(r'[A-Z]{3,5}', v) and v not in OK_TZ | OK_ABBR and not TZ_ABBR.fullmatch(v) and not place_ok(m.start()):
            err(m.start(), 'timezone_place', f'zone {v}: public files use UTC, examples use Europe/Berlin', v)
    for m in TZ_ABBR.finditer(text):
        if not place_ok(m.start()):
            err(m.start(), 'timezone_place', f'zone abbreviation {m.group(1)}: public files use UTC, examples use Europe/Berlin',
                m.group(1))
    for m in TZ_OFFSET.finditer(text):
        if m.group(1) not in OK_OFFSETS:
            err(m.start(), 'timezone_place', f'time offset {m.group(1)}: examples use Z, +00:00 or the zone Europe/Berlin')
    for m in EPSG.finditer(text):
        if m.group(1) not in OK_EPSG and not place_ok(m.start()):
            err(m.start(), 'coordinates_place', f'EPSG:{m.group(1)}: examples use only EPSG:4326 and EPSG:3857')
    for m in UTM.finditer(text):
        if not place_ok(m.start()):
            err(m.start(), 'coordinates_place', 'a UTM zone points to a place: examples use EPSG:3857')
    for m in COORD_PAIR.finditer(text):
        a, b = abs(float(m.group(1))), abs(float(m.group(2)))
        # latitude first or longitude first (GeoJSON order)
        if (a <= 90 and b <= 180) or (a <= 180 and b <= 90):
            err(m.start(), 'coordinates_place', f'coordinate pair {m.group(0)}: points to a place')
    for m in COORD_KEY.finditer(text):
        if not place_ok(m.start()):
            err(m.start(), 'coordinates_place', 'latitude or longitude with a fraction: points to a place')
    for m in TENANT.finditer(text):
        v = m.group(1) or m.group(2) or m.group(3)
        if v not in OK_TENANTS:
            err(m.start(), 'tenant_not_example', f'tenant {v}: public files use only demo, demo2 and farm')
    return starts


def history(stems, words):
    """--history: forbidden words in every file of every commit reachable from any ref. Every commit of a
    public repository is public; a word found there cannot be removed by a new commit, so the owner decides."""
    def git(*args):
        return subprocess.run(['git', '-C', ROOT, *args], capture_output=True, check=True).stdout
    seen, commits = set(), git('rev-list', '--all').decode().split()
    for c in commits:
        for line in git('ls-tree', '-r', '-z', c).decode('utf-8', 'replace').split('\0'):
            if not line:
                continue
            meta, rp = line.split('\t', 1)
            blob = meta.split()[2]
            if (blob, rp) in seen:
                continue
            seen.add((blob, rp))
            # paths of deleted or renamed files stay public in the history too
            if any(is_forbidden(w.group(0).lower(), stems, words) for w in WORD.finditer(rp)):
                rep.error(f'{c[:12]}:{rp}', '[forbidden_word] a word from the forbidden list (BOTARU_FORBIDDEN) in the path')
            # unlike the normal scan, LICENSE is read too: only its copyright line start is allowed
            text = git('cat-file', '-p', blob).decode('utf-8', 'replace')
            allowed = copyright_spans(rp, text) + owner_spans(rp, text)
            starts = [0] + [i + 1 for i, ch in enumerate(text) if ch == '\n']
            hit = sorted({bisect.bisect_right(starts, w.start()) for w in WORD.finditer(text)
                          if not any(lo <= w.start() < hi for lo, hi in allowed)
                          and is_forbidden(w.group(0).lower(), stems, words)})
            if hit:
                rep.error(f'{c[:12]}:{rp}', f'[forbidden_word] a word from the forbidden list (BOTARU_FORBIDDEN) '
                          f'on lines {hit[:20]}')
    rep.info(f'history: commits {len(commits)}, files {len(seen)}, forbidden list stems {len(stems)} '
             f'and words {len(words)}')
    rep.finish(limit=4000)


forb = load_forbidden()
if '--history' in sys.argv[1:]:
    if not forb:
        rep.error('BOTARU_FORBIDDEN', '[forbidden_missing] --history needs the forbidden word list')
        rep.finish()
    else:
        history(*forb)
tokens = {}  # word -> places (at most 20)
n_files = 0
n_cyrillic = 0
for f in files():
    rp = rel(f)
    n_files += 1
    parts = rp.split('/')
    if '__pycache__' in parts or rp.endswith(('.pyc', '.pyo')):
        rep.error(rp, '[bytecode_file] Python bytecode in the repository: delete the file, it is in .gitignore')
        continue
    scan(rp, ' '.join(parts), lines=False)  # names of files and folders
    if CYRILLIC.search(rp):
        rep.error(rp, '[cyrillic] Cyrillic characters in the path: the project is English only')
    for w in WORD.finditer(rp):
        tokens.setdefault(w.group(0).lower(), []).append(rp)
    try:
        with open(f, 'rb') as fh:
            raw = fh.read()
        if b'\0' in raw:
            raise UnicodeDecodeError('utf-8', raw, 0, 1, 'NUL')
        text = raw.decode('utf-8')
    except UnicodeDecodeError:
        if rp not in BINARY_OK:
            rep.error(rp, '[binary_file] non-text file: the check cannot see into it; remove the file or add it to BINARY_OK')
        continue
    n_cyrillic += cyrillic(rp, text)
    if rp in SKIP:
        continue
    starts = scan(rp, text)
    allowed = copyright_spans(rp, text) + owner_spans(rp, text)
    for w in WORD.finditer(text):
        if any(lo <= w.start() < hi for lo, hi in allowed):
            continue
        places = tokens.setdefault(w.group(0).lower(), [])
        if len(places) < 20:
            places.append(f'{rp}:{bisect.bisect_right(starts, w.start())}')
    if rp.endswith(('.yaml', '.yml', '.json')):
        try:
            docs = [load_json(f)] if rp.endswith('.json') else load_yaml_all(f)
        except (ValueError, yaml.YAMLError):
            docs = []  # check_text.py reports the read error
        for d in docs:
            for k, v in tenant_values(d):
                if v not in OK_TENANTS and not v.startswith(('<', '{', '$')):
                    rep.error(rp, f'[tenant_not_example] {k}: {v}: public files use only demo, demo2 and farm')

if forb:
    stems, words = forb
    for t in sorted(t for t in tokens if is_forbidden(t, stems, words)):
        for where in tokens[t]:
            rep.error(where, '[forbidden_word] a word from the forbidden list (BOTARU_FORBIDDEN)')
    rep.info(f'files {n_files}, words {len(tokens)}, forbidden list stems {len(stems)} and words {len(words)}, '
             f'files with Cyrillic {n_cyrillic}')
else:
    rep.info(f'files {n_files}, files with Cyrillic {n_cyrillic}; forbidden words not checked: no list')
rep.finish(limit=4000)
