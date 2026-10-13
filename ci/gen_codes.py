#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Builds the manifest lint code table in ci/README.md from ci/lint_codes.py.

The table sits between the marks <!-- generated: lint-codes --> and
<!-- /generated: lint-codes -->. It also checks that every code written by
check_manifests.py is described in lint_codes.py and that lint_codes.py has
no codes the lint never writes.

  python3 ci/gen_codes.py          rebuild the table
  python3 ci/gen_codes.py --check  check that the table matches lint_codes.py
"""
import os
import re
import sys

sys.dont_write_bytecode = True
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from common import Report, read
from lint_codes import CODES

rep = Report('lint codes')
README = os.path.join(HERE, 'README.md')
LINT = os.path.join(HERE, 'check_manifests.py')
HEADS = {'': ('| Code | Meaning | Rules |', 0)}

src = read(LINT)
used = set(re.findall(r"""\b(?:f|got|findings)\.(?:error|note)\)?\(\s*'([a-z_]+)'""", src))
used |= set(re.findall(r"""\(f\.note if examples_mode else f\.error\)\('([a-z_]+)'""", src))
for code in sorted(used - set(CODES)):
    rep.error('ci/check_manifests.py', f'[lint_code_undocumented] code {code} is not described in ci/lint_codes.py')
for code in sorted(set(CODES) - used):
    rep.error('ci/lint_codes.py', f'[lint_code_unused] code {code} is described, but the lint never writes it')


def table(lang):
    head, i = HEADS[lang]
    rows = [head, '|---|---|---|']
    for code in sorted(CODES):
        rows.append(f'| `{code}` | {CODES[code][0]} | {CODES[code][1]} |')
    return '\n'.join(rows)


text = read(README)
out = text
for lang in HEADS:
    start, end = '<!-- generated: lint-codes -->', '<!-- /generated: lint-codes -->'
    if start not in out or end not in out:
        rep.error('ci/README.md', f'[lint_codes_marks] the marks {start} and {end} are missing')
        continue
    a, b = out.index(start) + len(start), out.index(end)
    out = out[:a] + '\n' + table(lang) + '\n' + out[b:]
if out != text:
    if '--check' in sys.argv:
        rep.error('ci/README.md', '[generated_stale] the lint code table does not match ci/lint_codes.py: '
                  'run python3 ci/gen_codes.py')
    else:
        with open(README, 'w', encoding='utf-8') as fh:
            fh.write(out)
        rep.note('ci/README.md', '[generated_written] the code table was rebuilt')
rep.info(f'codes {len(CODES)}')
rep.finish()
