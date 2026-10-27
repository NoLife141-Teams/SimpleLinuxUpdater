"""Harness control-flow tests; real binaries are exercised separately in CI."""
import contextlib
import importlib.util
import io
import json
import selectors
import signal
from pathlib import Path
import tempfile
import sys
import unittest
from unittest import mock


def module(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


runner = module('run')
scenario = module('scenario')
negative = module('negative_control')


class HarnessTests(unittest.TestCase):
    def test_untrusted_cached_asset_rejected_before_use(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            cache = root / 'cache'
            cache.mkdir()
            (cache / 'checksums.txt').write_text('wrong distribution')
            with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
                runner.checked_file('checksums.txt', root / 'copy', cache)

    def test_failure_keeps_later_steps_not_run_and_always_cleans(self):
        subject = scenario.Scenario()
        private = subject.work
        subject.network = mock.Mock(side_effect=RuntimeError('private-password-cookie-key'))
        with contextlib.redirect_stdout(io.StringIO()) as output:
            code = subject.run()
        self.assertEqual(code, 1)
        report = json.loads(output.getvalue())
        self.assertEqual(report['steps']['network-isolation']['status'], 'failed')
        self.assertEqual(report['steps']['old-installation']['status'], 'not-run')
        self.assertEqual(report['steps']['cleanup']['status'], 'passed')
        self.assertNotIn('private-password-cookie-key', output.getvalue())
        self.assertFalse(private.exists())

    def test_signal_exception_interrupts_selector_without_eintr_retry(self):
        # InterruptedError would be swallowed by selectors as an EINTR retry.
        for exception in (runner.QualificationInterrupted, scenario.QualificationInterrupted):
            previous = signal.getsignal(signal.SIGALRM)
            def interrupted(_signum, _frame):
                raise exception()
            try:
                signal.signal(signal.SIGALRM, interrupted)
                signal.setitimer(signal.ITIMER_REAL, .01)
                with selectors.DefaultSelector() as selector:
                    with self.assertRaises(exception):
                        selector.select(timeout=1)
            finally:
                signal.setitimer(signal.ITIMER_REAL, 0)
                signal.signal(signal.SIGALRM, previous)

    def test_signal_is_failure_and_cleans(self):
        subject = scenario.Scenario()
        subject.network = mock.Mock(side_effect=scenario.QualificationInterrupted())
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(subject.run(), 1)
        self.assertFalse(subject.work.exists())
        self.assertEqual(subject.report['steps']['cleanup']['status'], 'passed')

    def test_shutdown_kills_only_owned_process_after_deadline(self):
        subject = scenario.Scenario()
        process = mock.Mock()
        process.poll.return_value = None
        process.wait.side_effect = [scenario.subprocess.TimeoutExpired('owned', 20), 0]
        subject.process = process
        try:
            with self.assertRaisesRegex(AssertionError, 'shutdown deadline'):
                subject.stop()
            process.terminate.assert_called_once()
            process.kill.assert_called_once()
        finally:
            subject.process = None
            subject.cleanup()

    def test_cleanup_failure_cannot_report_success(self):
        subject = scenario.Scenario()
        for name in ('network', 'old', 'export', 'fixtures', 'migrate', 'restart', 'restore', 'restored_restart', 'no_maintenance'):
            setattr(subject, name, mock.Mock())
        subject.cleanup = mock.Mock(side_effect=OSError('private cleanup detail'))
        try:
            with contextlib.redirect_stdout(io.StringIO()) as output:
                self.assertEqual(subject.run(), 1)
            self.assertEqual(subject.report['steps']['cleanup']['status'], 'failed')
            self.assertNotIn('private cleanup detail', output.getvalue())
        finally:
            scenario.shutil.rmtree(subject.work)



class ContainerOwnershipTests(unittest.TestCase):
    def test_unowned_container_is_never_stopped_or_removed(self):
        calls = []
        def invoke(args, **_kwargs):
            calls.append(args)
            return mock.Mock(stdout=b'owned-name\n' if args[1] == 'container' else b'{"slu.upgrade.owner":"someone-else"}')
        with mock.patch.object(runner, 'command', side_effect=invoke):
            with self.assertRaisesRegex(ValueError, 'label mismatch'):
                runner.cleanup_container('owned-name')
        self.assertFalse(any(call[1] in ('stop', 'rm') for call in calls))

    def test_temporary_files_removed_even_if_container_cleanup_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            private = Path(directory) / 'owned'
            private.mkdir()
            with mock.patch.object(runner, 'cleanup_container', side_effect=OSError('private detail')):
                errors = runner.cleanup_resources('owned-name', private, True)
            self.assertEqual(errors, ['owned-container-cleanup'])
            self.assertFalse(private.exists())

    def test_partial_create_without_container_requires_no_removal(self):
        with mock.patch.object(runner, 'command', return_value=mock.Mock(stdout=b'')) as command:
            runner.cleanup_container('owned-name')
        self.assertEqual(command.call_count, 1)

    def test_remove_still_runs_after_stop_failure(self):
        calls = []
        def invoke(args, **_kwargs):
            calls.append(args)
            if args[1] == 'container':
                return mock.Mock(stdout=b'owned-name\n')
            if args[1] == 'inspect':
                return mock.Mock(stdout=b'{"slu.upgrade.owner":"owned-name"}')
            if args[1] == 'stop':
                raise OSError('shutdown failed')
            return mock.Mock(stdout=b'')
        with mock.patch.object(runner, 'command', side_effect=invoke):
            with self.assertRaises(OSError):
                runner.cleanup_container('owned-name')
        self.assertEqual(calls[-1], ['docker', 'rm', '--force', 'owned-name'])

class NegativeControlCancellationTests(unittest.TestCase):
    def test_timeout_sends_term_and_waits_for_runner_cleanup(self):
        process = mock.Mock(returncode=1)
        process.wait.side_effect = [negative.subprocess.TimeoutExpired('owned-runner', 300), 1]
        process.poll.return_value = None
        with mock.patch.object(negative.subprocess, 'Popen', return_value=process), \
             mock.patch.object(negative.signal, 'signal'):
            code, interrupted = negative.run_harness(['owned-runner'])
        self.assertEqual((code, interrupted), (1, True))
        process.terminate.assert_called_once()
        self.assertEqual(process.wait.call_args_list, [mock.call(timeout=300), mock.call(timeout=150)])
        process.kill.assert_not_called()

    def test_signal_forwards_to_runner_and_cannot_count_as_negative_success(self):
        process = mock.Mock(returncode=1)
        handlers = {}
        def register(kind, handler):
            previous = handlers.get(kind)
            handlers[kind] = handler
            return previous
        def wait(**_kwargs):
            handlers[negative.signal.SIGTERM](negative.signal.SIGTERM, None)
            process.poll.return_value = 1
        process.poll.return_value = None
        process.wait.side_effect = wait
        with mock.patch.object(negative.subprocess, 'Popen', return_value=process), \
             mock.patch.object(negative.signal, 'signal', side_effect=register):
            self.assertEqual(negative.run_harness(['owned-runner']), (1, True))
        process.terminate.assert_called_once()
        process.kill.assert_not_called()


class PreparationFailureTests(unittest.TestCase):
    def test_prepare_failure_reports_not_run_and_removes_temporary_files(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            private = directory / 'owned'
            private.mkdir()
            output = directory / 'report'
            arguments = ['run.py', '--candidate', str(directory / 'missing-private-secret'),
                '--candidate-sha', 'a' * 40, '--output', str(output)]
            with mock.patch.object(sys, 'argv', arguments), \
                 mock.patch.object(runner.tempfile, 'mkdtemp', return_value=str(private)), \
                 mock.patch.object(runner.signal, 'signal'), \
                 contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(runner.main(), 1)
            report = (output / 'report.json').read_text()
            self.assertNotIn('missing-private-secret', report)
            self.assertEqual(json.loads(report)['scenario']['steps']['candidate-migration']['status'], 'not-run')
            self.assertEqual(json.loads(report)['steps']['cleanup']['status'], 'passed')
            self.assertFalse(private.exists())


if __name__ == '__main__':
    unittest.main()
