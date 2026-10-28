import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import threading
import unittest
from unittest import mock

import deferrals
import run


class DeferralHarnessTests(unittest.TestCase):
    def test_installed_inventory_includes_administrator_held_versions(self):
        self.assertEqual(deferrals.installed_versions(
            'ii  independent=1\nhi  administrator-held=1\nrc  removed=1\nun  absent=\n'),
            ['independent=1', 'administrator-held=1'])

    def test_all_existing_distributions_are_admitted(self):
        self.assertEqual(set(run.IMAGES), {'debian12', 'debian13', 'ubuntu2204', 'ubuntu2404', 'ubuntu2604'})
        with mock.patch.object(deferrals.subprocess, 'run') as execute:
            with self.assertRaises(AssertionError):
                deferrals.qualify(b'helper', Path('unused.json'), 'production')
            execute.assert_not_called()

    def test_generator_uses_candidate_source_even_for_preserved_trusted_harness(self):
        with mock.patch.object(deferrals.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'#!/bin/sh\nsource helper', b'')) as execute:
            self.assertEqual(deferrals.generate_helper(), b'#!/bin/sh\nsource helper')
        self.assertEqual(execute.call_args.args[0], ['go', 'run', './tools/ci/package-deferral-helper'])
        self.assertEqual(execute.call_args.kwargs['cwd'], deferrals.SOURCE)

    def fake_run(self, mode):
        owner = 'a' * 32
        state = {'created': False, 'removed': False, 'marker_reads': 0, 'fixture_started': False}
        real_run = subprocess.run

        def execute(args, **kwargs):
            self.assertEqual(args[:2], run.DOCKER)
            op = args[2]
            if op == 'run':
                self.assertIn('--network', args)
                self.assertEqual(args[args.index('--network') + 1], 'none')
                self.assertNotIn('--mount', args)
                self.assertNotIn('-v', args)
                self.assertIn('--memory', args)
                self.assertIn('--pids-limit', args)
                state['created'] = True
                if mode == 'partial-create':
                    return subprocess.CompletedProcess(args, 1, b'injected client failure')
            elif op == 'inspect':
                if not state['created']:
                    return subprocess.CompletedProcess(args, 1, b'Error: No such object')
                if '.State.Running' in args[4]:
                    return subprocess.CompletedProcess(args, 0, json.dumps({
                        'owner': 'other-owner' if mode == 'unowned' else owner,
                        'running': mode != 'startup-exit'}).encode())
                return subprocess.CompletedProcess(args, 0, (('other-owner' if mode == 'unowned' else owner)
                    if '.Config.Labels' in args[4] else 'sha256:fixture-image').encode())
            elif op == 'exec':
                if args[-1] == deferrals.MARKER_PROBE:
                    state['marker_reads'] += 1
                    if mode == 'startup-signal':
                        import os
                        import signal
                        os.kill(os.getpid(), signal.SIGTERM)
                    if mode == 'startup-timeout' or (mode == 'slow-start' and state['marker_reads'] < 3):
                        return subprocess.CompletedProcess(args, 75, b'')
                    if mode == 'startup-read-error':
                        return subprocess.CompletedProcess(args, 1, b'Permission denied')
                    return subprocess.CompletedProcess(args, 0, ('wrong-marker' if mode == 'marker' else owner).encode())
                if args[-1] == '/run/slu-fixture-owner':
                    return subprocess.CompletedProcess(args, 0, ('wrong-marker' if mode == 'marker' else owner).encode())
                if args[-1] == '-s':
                    state['fixture_started'] = True
                    if mode == 'real-signal':
                        import os
                        import signal
                        timer = threading.Timer(.1, lambda: os.kill(os.getpid(), signal.SIGTERM))
                        timer.start()
                        try:
                            # Exercise the actual selector/poll loop that retries EINTR.
                            real_run([sys.executable, '-c', 'import time; time.sleep(3)'],
                                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=5)
                        finally:
                            timer.cancel()
                    if mode == 'interrupt':
                        raise InterruptedError('injected interruption')
                    if mode == 'signal':
                        import os
                        import signal
                        os.kill(os.getpid(), signal.SIGTERM)
                    if mode == 'timeout':
                        raise subprocess.TimeoutExpired(args, 1)
                    return subprocess.CompletedProcess(args, 1, b'injected fixture setup failure')
            elif op == 'rm':
                self.assertNotEqual(mode, 'unowned')
                state['removed'] = True
                state['created'] = False
            return subprocess.CompletedProcess(args, 0, b'')
        return owner, state, execute

    def test_generator_keeps_compiler_stderr_out_of_helper(self):
        result = subprocess.CompletedProcess([], 0, b'#!/bin/sh\nhelper', b'go: downloading dependency\n')
        with mock.patch.object(deferrals.subprocess, 'run', return_value=result):
            self.assertEqual(deferrals.generate_helper(), b'#!/bin/sh\nhelper')
        for result in (subprocess.CompletedProcess([], 1, b'', b'build failed'),
                       subprocess.CompletedProcess([], 0, b'not a helper', b'')):
            with mock.patch.object(deferrals.subprocess, 'run', return_value=result), self.assertRaises(AssertionError):
                deferrals.generate_helper()

    def test_cleanup_after_setup_failure_partial_creation_timeout_and_signal(self):
        for mode in ('failure', 'partial-create', 'interrupt', 'signal', 'real-signal', 'timeout', 'marker', 'unowned'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as output:
                owner, state, execute = self.fake_run(mode)
                with mock.patch.object(deferrals, 'source_manifest', return_value={}), \
                     mock.patch.object(deferrals.uuid, 'uuid4', return_value=mock.Mock(hex=owner)), \
                     mock.patch.object(deferrals.subprocess, 'run', side_effect=execute):
                    path = Path(output, 'report.json')
                    started = time.monotonic()
                    self.assertEqual(deferrals.qualify(b'helper', path, 'debian13', source_sha='f' * 40), 1)
                    if mode == 'real-signal':
                        self.assertLess(time.monotonic() - started, 2, 'SIGTERM was swallowed by subprocess EINTR retry')
                report = json.loads(path.read_text())
                self.assertEqual(report['status'], 'failed')
                self.assertEqual(report['exit_code'], 1)
                if mode == 'real-signal':
                    self.assertIn('interrupted by signal', report['error'])
                self.assertTrue(all(r['status'] == 'not_executed' for r in report['scenarios']))
                self.assertEqual(state['removed'], mode != 'unowned')
                self.assertEqual(report['cleanup']['status'], 'absent_or_unowned' if mode == 'unowned' else 'passed')

    def test_startup_waits_for_marker_and_keeps_cleanup_on_startup_failures(self):
        for mode in ('slow-start', 'marker', 'unowned', 'startup-exit', 'startup-read-error',
                     'startup-timeout', 'startup-signal'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as output:
                owner, state, execute = self.fake_run(mode)
                clock = [0.0]
                def sleep(seconds):
                    clock[0] += seconds
                with mock.patch.object(deferrals, 'source_manifest', return_value={}), \
                     mock.patch.object(deferrals.uuid, 'uuid4', return_value=mock.Mock(hex=owner)), \
                     mock.patch.object(deferrals.subprocess, 'run', side_effect=execute), \
                     mock.patch.object(deferrals.time, 'monotonic', side_effect=lambda: clock[0]), \
                     mock.patch.object(deferrals.time, 'sleep', side_effect=sleep):
                    path = Path(output, 'report.json')
                    # The fake fixture deliberately fails after startup; no APT result
                    # can be manufactured by this admission/cleanup test.
                    self.assertEqual(deferrals.qualify(b'helper', path, 'debian13', source_sha='f' * 40), 1)
                report = json.loads(path.read_text())
                self.assertEqual(report['startup']['status'], 'passed' if mode == 'slow-start' else 'failed')
                self.assertEqual(state['fixture_started'], mode == 'slow-start')
                self.assertEqual(state['removed'], mode != 'unowned')
                self.assertEqual(report['cleanup']['status'], 'absent_or_unowned' if mode == 'unowned' else 'passed')
                self.assertTrue(all(r['status'] == 'not_executed' for r in report['scenarios']))
                if mode == 'slow-start':
                    self.assertEqual(state['marker_reads'], 3)
                elif mode == 'startup-timeout':
                    self.assertEqual(clock[0], 30)
                    self.assertIn('not ready within 30 seconds', report['error'])
                elif mode == 'startup-signal':
                    self.assertIn('interrupted by signal', report['error'])

    def test_startup_rechecks_owner_and_running_state_before_each_marker_read(self):
        for change in ({'owner': 'foreign', 'running': True}, {'owner': 'owned', 'running': False}):
            with self.subTest(change=change):
                responses = iter([json.dumps({'owner': 'owned', 'running': True}), json.dumps(change)])
                calls = []
                def docker(*args, **kwargs):
                    calls.append(args[0])
                    if args[0] == 'inspect': return next(responses)
                    return subprocess.CompletedProcess([], 75, b'')
                with mock.patch.object(deferrals.time, 'sleep'), self.assertRaises(AssertionError):
                    deferrals.wait_for_target(docker, 'owned-target', 'owned')
                self.assertEqual(calls, ['inspect', 'exec', 'inspect'])

    def test_startup_command_timeouts_share_the_remaining_startup_budget(self):
        clock = [0.0]
        calls = []
        def docker(*args, **kwargs):
            remaining = 30 - clock[0]
            self.assertEqual(kwargs['timeout_seconds'], remaining)
            calls.append((args[0], kwargs['timeout_seconds']))
            # A slow inspect consumes most of the budget; the marker read must
            # receive only what is left, and a late successful read cannot pass.
            clock[0] += 29 if args[0] == 'inspect' else 1
            if args[0] == 'inspect':
                return json.dumps({'owner': 'owned', 'running': True})
            return subprocess.CompletedProcess([], 0, b'owned')
        with mock.patch.object(deferrals.time, 'monotonic', side_effect=lambda: clock[0]), self.assertRaises(TimeoutError):
            deferrals.wait_for_target(docker, 'owned-target', 'owned')
        self.assertEqual(calls, [('inspect', 30), ('exec', 1)])

    def test_generator_failure_writes_not_executed_evidence(self):
        with tempfile.TemporaryDirectory() as output, mock.patch.object(deferrals, 'source_identity', return_value='f' * 40), \
             mock.patch.object(deferrals, 'generate_helper', side_effect=RuntimeError('generation failed')), \
             mock.patch('sys.argv', ['deferrals.py', '--distro', 'debian13', '--output', output + '/report.json']):
            self.assertEqual(deferrals.main(), 1)
            report = json.loads(Path(output, 'report.json').read_text())
            self.assertEqual(report['exit_code'], 1)
            self.assertEqual({r['status'] for r in report['scenarios']}, {'not_executed'})

    def test_negative_control_rejects_infrastructure_failure(self):
        spec = importlib.util.spec_from_file_location('negative', run.HERE / 'deferrals-negative.py')
        negative = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(negative)
        def infrastructure_failure(helper, path, distro, **kwargs):
            path.write_text(json.dumps({'status': 'failed', 'exit_code': 1,
                'scenarios': [{'scenario': name, 'status': 'not_executed'} for name in deferrals.SCENARIOS]}))
            return 1
        with tempfile.TemporaryDirectory() as output, \
             mock.patch('sys.argv', ['negative', '--distro', 'debian13', '--output', output]), \
             mock.patch.object(deferrals, 'source_identity', return_value='f' * 40), \
             mock.patch.object(deferrals, 'generate_helper', return_value=b'for (change in approved) if (!(change in seen)) deny('), \
             mock.patch.object(deferrals, 'qualify', side_effect=infrastructure_failure):
            self.assertEqual(negative.main(), 1)
            report = json.loads(Path(output, 'report.json').read_text())
            self.assertEqual(report['negative_exit_code'], 1)
            self.assertIsNone(report['positive_exit_code'])

    def test_workflow_gates_and_explicit_evidence(self):
        for filename in ('ci.yml', 'ssh-apt-extended.yml', 'release.yml'):
            workflow = (run.REPO / '.github/workflows' / filename).read_text()
            self.assertIn('deferrals-negative.py', workflow)
            for name in ('report', 'negative', 'positive'):
                self.assertIn('deferrals-evidence/' + name + '.json', workflow)
            self.assertIn('timeout-minutes: 8', workflow)
        release = (run.REPO / '.github/workflows/release.yml').read_text()
        self.assertIn('needs: [release-gate, deferrals-gate]', release)
        self.assertIn('needs: [release-gate, deferrals-gate, publish-release]', release)
        self.assertIn('SLU_DEFERRALS_SOURCE: ${{ github.workspace }}', release)


if __name__ == '__main__':
    unittest.main()
