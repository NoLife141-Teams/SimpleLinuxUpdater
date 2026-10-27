#!/usr/bin/env python3
"""Require a real red result caused specifically by diagnostic fixture loss.

This wrapper succeeds only when the harness detects the intentional regression;
run.py itself always returns nonzero for that diagnostic. No product is patched.
"""
import json
from pathlib import Path
import subprocess
import sys


def main():
    args = sys.argv[1:]
    if '--diagnostic' in args or '--output' not in args:
        raise SystemExit('pass an explicit --output; diagnostic is selected here')
    output = Path(args[args.index('--output') + 1])
    result = subprocess.run([sys.executable, str(Path(__file__).with_name('run.py')),
        *args, '--diagnostic', 'drop-inventory'], timeout=480, check=False)
    report = json.loads((output / 'report.json').read_text())
    child = report.get('scenario', {})
    valid = (result.returncode == 1 and report['exit_code'] == 1
        and report['diagnostic'] == 'drop-inventory'
        and report['steps']['cleanup']['status'] == 'passed'
        and child.get('assertion') == 'inventory or encrypted SSH secret lost'
        and child['steps']['candidate-migration']['status'] == 'failed'
        and child['steps']['candidate-restart']['status'] == 'not-run'
        and child['steps']['cleanup']['status'] == 'passed')
    print(json.dumps({'negative_control_detected': valid, 'harness_exit': result.returncode}))
    return 0 if valid else 1


if __name__ == '__main__':
    raise SystemExit(main())
