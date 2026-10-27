#!/usr/bin/env python3
"""Require a real red result caused specifically by diagnostic fixture loss.

This wrapper succeeds only when the harness detects the intentional regression;
run.py itself always returns nonzero for that diagnostic. No product is patched.
"""
import json
from pathlib import Path
import subprocess
import signal
import sys


def run_harness(command, timeout=300):
    """Forward cancellation and allow run.py to clean before an emergency kill."""
    process = None
    interrupted = False
    def terminate(_signum, _frame):
        nonlocal interrupted
        interrupted = True
        if process is not None and process.poll() is None:
            process.terminate()
    previous = {kind: signal.signal(kind, terminate) for kind in (signal.SIGTERM, signal.SIGINT)}
    try:
        process = subprocess.Popen(command)
        if interrupted:
            process.terminate()
        try:
            process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            interrupted = True
        finally:
            # Keep cancellation from interrupting the bounded graceful cleanup.
            for kind in previous:
                signal.signal(kind, signal.SIG_IGN)
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=150)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
                    interrupted = True
        return process.returncode, interrupted
    finally:
        for kind, handler in previous.items():
            signal.signal(kind, handler)


def main():
    args = sys.argv[1:]
    if '--diagnostic' in args or '--output' not in args:
        raise SystemExit('pass an explicit --output; diagnostic is selected here')
    output = Path(args[args.index('--output') + 1])
    code, interrupted = run_harness([sys.executable, str(Path(__file__).with_name('run.py')),
        *args, '--diagnostic', 'drop-inventory'])
    if interrupted or not (output / 'report.json').exists():
        print(json.dumps({'negative_control_detected': False, 'harness_exit': code, 'interrupted': interrupted}))
        return 1
    report = json.loads((output / 'report.json').read_text())
    child = report.get('scenario', {})
    valid = (code == 1 and report['exit_code'] == 1
        and report['diagnostic'] == 'drop-inventory'
        and report['steps']['cleanup']['status'] == 'passed'
        and child.get('assertion') == 'inventory or encrypted SSH secret lost'
        and child['steps']['candidate-migration']['status'] == 'failed'
        and child['steps']['candidate-restart']['status'] == 'not-run'
        and child['steps']['cleanup']['status'] == 'passed')
    print(json.dumps({'negative_control_detected': valid, 'harness_exit': code}))
    return 0 if valid else 1


if __name__ == '__main__':
    raise SystemExit(main())
