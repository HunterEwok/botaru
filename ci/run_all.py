#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""All skeleton checks in a row; exit code 1 if any of them fails.

  python3 -I ci/run_all.py [base]       base for the additive-only check, origin/main by default
  python3 -I ci/run_all.py [base] -v    with notes

The checks run in one process: YAML, JSON and schemas are parsed once and
reused (common.load_yaml_all, common.schemas). A missing check script is an
error: renaming a script does not silently drop its check.
"""
import os
import runpy
import sys
import time
import traceback

sys.dont_write_bytecode = True
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from common import VERSIONS  # noqa: E402

args = [a for a in sys.argv[1:] if not a.startswith('-')]
flags = [a for a in sys.argv[1:] if a.startswith('-')]
STEPS = [
    ['check_registry.py'],
    ['gen_rules.py', '--check'],
    ['gen_lifecycles.py', '--check'],
    ['gen_suites.py', '--check'],
    ['gen_codes.py', '--check'],
    ['gen_openapi.py', '--check'],
    ['check_text.py'],
    ['check_schemas.py'],
    ['check_fixtures.py'],
    ['check_crossrefs.py'],
    ['check_manifests.py'],
    ['check_evals.py'],
    ['check_additive.py', *args[:1]],
    ['public_safety.py'],
    *(['readiness.py', v, '--strict'] for v in VERSIONS),
]
failed = []
gh = bool(os.environ.get('GITHUB_ACTIONS'))
started = time.time()
for step in STEPS:
    path = os.path.join(HERE, step[0])
    print(f'\n== {step[0]}', flush=True)
    if not os.path.exists(path):
        print(f'::error::[step_missing] check script ci/{step[0]} is missing' if gh
              else f'  error  [step_missing] check script ci/{step[0]} is missing', flush=True)
        failed.append(step[0])
        continue
    t0 = time.time()
    sys.argv = [path, *step[1:], *flags]
    code = 0
    try:
        runpy.run_path(path, run_name='__main__')
    except SystemExit as e:
        code = e.code if isinstance(e.code, int) else (0 if e.code is None else 1)
    except Exception:  # noqa: BLE001 - a crashing script fails its step, not the whole run
        traceback.print_exc()
        print(f'  error  [step_crashed] ci/{step[0]} raised an exception: the check is broken, not the data', flush=True)
        code = 1
    sys.stdout.flush()
    if code != 0:
        failed.append(step[0])
    if '-v' in flags:
        print(f'  ({time.time() - t0:.1f} s)')
print(f'\ntime {time.time() - started:.0f} s')
print('failed: ' + ', '.join(failed) if failed else 'all checks passed')
sys.exit(1 if failed else 0)
