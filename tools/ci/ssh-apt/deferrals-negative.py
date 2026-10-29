#!/usr/bin/env python3
"""Require the real gate to detect a missing-action guard regression, then recover."""
import argparse
import hashlib
import json
from pathlib import Path
import sys

import deferrals
import run


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--distro', choices=tuple(run.IMAGES), required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    report = {'status': 'failed', 'exit_code': 1, 'negative_exit_code': None,
              'positive_exit_code': None, 'injection': 'disable approved-action completeness check'}
    # Never mistake an older invocation's evidence for a stage skipped this time.
    for name in ('negative', 'positive'):
        (args.output / (name + '.json')).write_text(json.dumps({
            'status': 'not_executed', 'exit_code': None, 'distribution': args.distro,
            'reason': 'earlier gate stage has not completed',
            'scenarios': [{'scenario': scenario, 'status': 'not_executed'} for scenario in deferrals.SCENARIOS]}) + '\n')
    try:
        source = deferrals.source_identity()
        helper = deferrals.generate_helper()
        before = b'for (change in approved) if (!(change in seen)) deny('
        after = b'for (change in approved) if (0) deny('
        run.require(helper.count(before) == 1, 'negative control injection point changed')
        mutated = helper.replace(before, after)
        report.update(source_sha=source, helper_sha256=hashlib.sha256(helper).hexdigest(),
                      injected_helper_sha256=hashlib.sha256(mutated).hexdigest())
        # Bytes live only in memory; never alter candidate source or a tracked helper.
        negative = args.output / 'negative.json'
        report['negative_exit_code'] = deferrals.qualify(mutated, negative, args.distro, source_sha=source,
                                                       helper_origin='source-generated with temporary regression injection')
        evidence = json.loads(negative.read_text())
        failed = [r for r in evidence['scenarios'] if r['status'] == 'failed']
        run.require(report['negative_exit_code'] == 1 and evidence['exit_code'] == 1
                    and len(failed) == 1 and failed[0]['scenario'] == 'approved-action-missing-after-pin-drift'
                    and failed[0]['outcome'] == 'succeeded' and failed[0]['before'] != failed[0]['after']
                    and evidence['cleanup']['status'] == 'passed',
                    'negative control did not demonstrate the expected guard regression')
        report['positive_exit_code'] = deferrals.qualify(helper, args.output / 'positive.json', args.distro, source_sha=source)
        run.require(report['positive_exit_code'] == 0, 'healthy helper did not recover')
        report.update(status='passed', exit_code=0)
    except Exception as error:
        report['error'] = str(error)[-5000:]
    (args.output / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
    return report['exit_code']


if __name__ == '__main__':
    sys.exit(main())
