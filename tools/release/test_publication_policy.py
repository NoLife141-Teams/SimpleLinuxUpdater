import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('policy', Path(__file__).with_name('publication-policy.py'))
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)
D = 'sha256:' + 'a' * 64
OTHER = 'sha256:' + 'b' * 64
ENV = dict(RELEASE_TAG='v0.4.9', RELEASE_SHA='c' * 40, GITHUB_REPOSITORY='owner/repo',
           IMAGE='ghcr.io/owner/repo', DIGEST=D, GITHUB_RUN_ID='12', GITHUB_RUN_ATTEMPT='1')


class Backend:
    """Persistent fake external state across independent runner invocations."""
    def __init__(self):
        self.release = dict(id=1, tag_name='v0.4.9', draft=True)
        self.record = None
        self.refs = {D: D}
        self.calls = []
        self.writes = []
        self.fail = None
        self.after_accept = False
        self.tags = 'v0.4.9'

    def registry(self, image, ref, *, allow_missing=False):
        if ref not in self.refs and not allow_missing:
            raise RuntimeError('missing manifest')
        return self.refs.get(ref)

    def capture_command(self, *args):
        assert args[:2] in [('gh', 'api'), ('git', 'tag')], args
        return self.command(*args)

    def run_command(self, *args, env=None):
        assert args[:2] not in [('gh', 'api'), ('git', 'tag')], args
        self.command(*args)

    def command(self, *args):
        self.calls.append(args)
        operation = None
        if args[0] == 'docker':
            operation = args[-2].split(':')[-1]
        elif args[:3] == ('gh', 'release', 'edit'):
            operation = 'finalize'
        elif args[:3] == ('gh', 'release', 'upload'):
            operation = 'record'
        elif args[0] == 'bash' and args[1].endswith(('docker-smoke.sh', 'scan-image.sh')):
            operation = Path(args[1]).name + ':' + args[-1]
        should_fail = self.fail is not None and self.fail == operation
        if should_fail and not self.after_accept:
            raise RuntimeError('simulated failure before acceptance')
        if operation in ('latest', 'v0.4.9'):
            self.writes.append(operation)
            self.refs[operation] = args[-1].split('@')[1]
        elif operation == 'finalize':
            self.writes.append(operation)
            self.release['draft'] = False
        elif operation == 'record':
            if self.record is not None:
                raise RuntimeError('asset already exists')
            self.writes.append(operation)
            self.record = json.loads(Path(args[4]).read_text())
        if should_fail:
            raise RuntimeError('simulated response lost after acceptance')
        if args[:2] == ('gh', 'api'):
            if args[-1].endswith('/assets?per_page=100'):
                return json.dumps([[dict(id=2, name='publication.json', size=1000)] if self.record else []])
            if args[-1].endswith('/assets/2'):
                return json.dumps(self.record)
        return self.tags if args[:2] == ('git', 'tag') else ''


class PublicationTests(unittest.TestCase):
    def setUp(self):
        self.backend = Backend()
        self.enterContext(patch.dict(os.environ, ENV))
        self.enterContext(patch.object(policy, 'capture_command', side_effect=self.backend.capture_command))
        self.enterContext(patch.object(policy, 'run_command', side_effect=self.backend.run_command))
        # Test runs must never append simulated release results to the runner summary.
        self.enterContext(patch.dict(os.environ, GITHUB_STEP_SUMMARY=''))
        self.enterContext(patch.object(policy, 'registry_digest', side_effect=self.backend.registry))
        self.enterContext(patch.object(policy, 'releases', side_effect=lambda repo: [self.backend.release]))
        self.identity = policy.identity()

    def publish(self):
        policy.publish(self.identity)
        policy.finalize(self.identity)

    def test_numeric_order_and_reserved_drafts(self):
        self.assertFalse(policy.may_promote('v0.4.9', [], ['v0.4.10']))
        self.assertTrue(policy.may_promote('v0.4.10', [], ['v0.4.9']))
        for draft in (True, False):
            self.assertFalse(policy.may_promote('v0.4.9', [dict(tag_name='v1.0.0', draft=draft)], []))
        self.assertTrue(policy.may_promote('v0.4.9', [], ['v0.4.9', 'v1.0.0-rc1']))

    def test_invalid_identity(self):
        for tag in ['v01.2.3', 'v1.2', 'v1.2.3;echo', '', 'main']:
            with self.assertRaises(ValueError):
                policy.version(tag)
        with patch.dict(os.environ, RELEASE_SHA='main'):
            with self.assertRaises(ValueError):
                policy.identity()

    def test_qualification_failures_never_write_official_tags_or_record(self):
        for script in ('docker-smoke.sh', 'scan-image.sh'):
            for arch in policy.PLATFORMS:
                with self.subTest(script=script, arch=arch):
                    self.backend.fail = script + ':' + arch
                    with self.assertRaises(RuntimeError):
                        self.publish()
                    self.assertEqual(self.backend.writes, [])
                    self.assertTrue(self.backend.release['draft'])

    def test_success_qualifies_same_digest_before_durable_record_and_tags(self):
        self.publish()
        self.assertEqual(self.backend.writes, ['record', 'v0.4.9', 'latest', 'finalize'])
        checks = [c for c in self.backend.calls if c[0] == 'bash' and c[1].endswith(('docker-smoke.sh', 'scan-image.sh'))]
        self.assertEqual(len(checks), 4)
        record_index = next(i for i, c in enumerate(self.backend.calls) if c[:3] == ('gh', 'release', 'upload'))
        self.assertTrue(all(self.backend.calls.index(c) < record_index for c in checks))
        self.assertTrue(all(c[2] == ENV['IMAGE'] + '@' + D for c in checks))
        self.assertEqual(self.backend.refs['v0.4.9'], D)
        self.assertEqual(self.backend.refs['latest'], D)

    def test_image_publication_leaves_github_draft_for_separate_finalization(self):
        policy.publish(self.identity)
        self.assertTrue(self.backend.release['draft'])
        self.assertEqual(self.backend.writes, ['record', 'v0.4.9', 'latest'])

    def test_finalization_token_is_only_passed_to_github_release_edit(self):
        policy.publish(self.identity)
        self.backend.calls.clear()
        with patch.dict(os.environ, GH_TOKEN='automatic-token', RELEASE_TOKEN='release-only-token'), \
                patch.object(policy.sys, 'argv', ['policy', 'finalize']), \
                patch.object(policy, 'run_command', wraps=self.backend.run_command) as commands:
            policy.main()
            self.assertEqual(os.environ['GH_TOKEN'], 'automatic-token')
            self.assertNotIn('RELEASE_TOKEN', os.environ)
        authenticated = [c for c in commands.call_args_list if c.kwargs.get('env') is not None]
        self.assertEqual(len(authenticated), 1)
        self.assertEqual(authenticated[0].args[:3], ('gh', 'release', 'edit'))
        self.assertEqual(authenticated[0].kwargs['env']['GH_TOKEN'], 'release-only-token')
        self.assertNotIn('RELEASE_TOKEN', authenticated[0].kwargs['env'])
        self.assertFalse(any(c[0] == 'docker' or c[0] == 'bash' and c[1].endswith(('docker-smoke.sh', 'scan-image.sh'))
                             for c in self.backend.calls))

    def test_finalization_rejects_missing_record_official_digest_or_latest_promotion(self):
        with self.assertRaises(RuntimeError):
            policy.finalize(self.identity)
        policy.publish(self.identity)
        for reference in ('v0.4.9', 'latest'):
            for replacement in (None, OTHER):
                with self.subTest(reference=reference, replacement=replacement):
                    self.backend.writes.clear()
                    if replacement is None:
                        self.backend.refs.pop(reference, None)
                    else:
                        self.backend.refs[reference] = replacement
                    with self.assertRaises(RuntimeError):
                        policy.finalize(self.identity)
                    self.assertEqual(self.backend.writes, [])
                    self.backend.refs[reference] = D

    def test_finalization_rechecks_newer_reservations_without_registry_writes(self):
        policy.publish(self.identity)
        self.backend.tags = 'v0.4.10'
        self.backend.refs['latest'] = OTHER
        self.backend.writes.clear()
        policy.finalize(self.identity)
        self.assertEqual(self.backend.writes, ['finalize'])
        self.assertEqual(self.backend.refs['latest'], OTHER)
        self.assertEqual(self.backend.calls[-1][-1], '--latest=false')

    def test_failed_finalization_can_resume_without_rebuilding_or_requalifying(self):
        policy.publish(self.identity)
        record = self.backend.record.copy()
        self.backend.fail = 'finalize'
        with self.assertRaises(RuntimeError):
            policy.finalize(self.identity)
        self.backend.fail = None
        self.backend.calls.clear()
        self.backend.writes.clear()
        policy.finalize(self.identity)
        self.assertEqual(self.backend.writes, ['finalize'])
        self.assertEqual(self.backend.record, record)
        self.assertFalse(any(c[0] == 'docker' or c[0] == 'bash' and c[1].endswith(('docker-smoke.sh', 'scan-image.sh'))
                             for c in self.backend.calls))

    def test_partial_rerun_after_publication_is_noop_before_any_build_or_write(self):
        self.publish()
        self.backend.calls.clear()
        self.backend.writes.clear()
        self.assertEqual(policy.plan(self.identity)[0], 'complete')
        self.publish()
        self.assertEqual(self.backend.writes, [])
        self.assertFalse(any(c[0] in ('docker', 'bash') for c in self.backend.calls))

    def test_retry_after_each_write_failure_preserves_recorded_digest(self):
        for operation in ('record', 'v0.4.9', 'latest', 'finalize'):
            for accepted in (False, True):
                with self.subTest(operation=operation, accepted=accepted):
                    # Fresh service, then two independent policy invocations.
                    self.backend.__init__()
                    self.backend.fail, self.backend.after_accept = operation, accepted
                    with self.assertRaises(RuntimeError):
                        self.publish()
                    first_writes = list(self.backend.writes)
                    self.backend.fail = None
                    mode, record = policy.plan(self.identity)
                    self.assertEqual(mode, 'complete' if operation == 'finalize' and accepted else
                                     'build' if operation == 'record' and not accepted else 'resume')
                    if record:
                        self.assertEqual(record['digest'], D)
                    self.publish()
                    self.assertFalse(self.backend.release['draft'])
                    for tag in ('v0.4.9', 'latest', 'record'):
                        self.assertEqual(self.backend.writes.count(tag), 1, (first_writes, self.backend.writes))

    def test_recorded_retry_rejects_new_digest_without_writes(self):
        self.backend.fail = 'v0.4.9'
        with self.assertRaises(RuntimeError):
            self.publish()
        self.backend.writes.clear()
        with patch.dict(os.environ, DIGEST=OTHER):
            with self.assertRaises(RuntimeError):
                self.publish()
        self.assertEqual(self.backend.writes, [])

    def test_missing_draft_legacy_public_and_unrecorded_tag_fail_closed(self):
        with patch.object(policy, 'releases', return_value=[]):
            with self.assertRaises(RuntimeError):
                policy.plan(self.identity)
        self.backend.release['draft'] = False
        with self.assertRaises(RuntimeError):
            policy.plan(self.identity)
        self.backend.release['draft'] = True
        self.backend.refs['v0.4.9'] = D
        with self.assertRaises(RuntimeError):
            policy.plan(self.identity)
        self.assertEqual(self.backend.writes, [])

    def test_record_mismatch_missing_official_and_malformed_record_fail_closed(self):
        self.publish()
        record = self.backend.record.copy()
        for key, value in [('commit', 'd'*40), ('digest', 'bad'), ('platforms', ['linux/amd64']), ('checks', []), ('run_id', 'bad')]:
            self.backend.record = dict(record, **{key: value})
            with self.assertRaises(ValueError):
                policy.plan(self.identity)
        self.backend.record = record
        self.backend.refs['v0.4.9'] = OTHER
        with self.assertRaises(RuntimeError):
            policy.plan(self.identity)
        del self.backend.refs['v0.4.9']
        with self.assertRaises(RuntimeError):
            policy.plan(self.identity)

    def test_older_release_publishes_version_without_regressing_latest(self):
        self.backend.tags = 'v0.4.10'
        self.backend.refs['latest'] = OTHER
        self.publish()
        self.assertEqual(self.backend.writes, ['record', 'v0.4.9', 'finalize'])
        self.assertEqual(self.backend.refs['latest'], OTHER)
        self.assertEqual(self.backend.calls[-1][-1], '--latest=false')

    def test_plan_outputs_candidate_scoped_to_attempt(self):
        with tempfile.TemporaryDirectory() as folder:
            output = Path(folder) / 'outputs'
            with patch.dict(os.environ, GITHUB_OUTPUT=str(output)), patch.object(policy.sys, 'argv', ['policy', 'plan']):
                policy.main()
            self.assertIn('mode=build\n', output.read_text())
            self.assertIn('candidate=ghcr.io/owner/repo:candidate-v0.4.9-12-1\n', output.read_text())
        self.assertEqual(self.backend.writes, [])

    def test_api_failure_is_fatal_before_writes(self):
        with patch.object(policy, 'releases', side_effect=RuntimeError('unavailable')):
            with self.assertRaises(RuntimeError):
                self.publish()
        with patch.object(policy, 'capture_command', side_effect=RuntimeError('unavailable')):
            with self.assertRaises(RuntimeError):
                policy.plan(self.identity)
        self.assertEqual(self.backend.writes, [])

    def test_full_rerun_cannot_replace_archives_after_record_is_created(self):
        self.backend.fail = 'v0.4.9'
        with self.assertRaises(RuntimeError):
            self.publish()
        self.backend.calls.clear()
        with patch.object(policy.sys, 'argv', ['policy', 'draft']):
            with self.assertRaisesRegex(RuntimeError, 'assets are frozen'):
                policy.main()
        self.assertFalse(any(c[:2] == ('gh', 'release') for c in self.backend.calls))

    def test_progress_identifies_each_validation_and_flushes_immediately(self):
        with patch('builtins.print') as printed:
            self.publish()
        messages = [call for call in printed.call_args_list if call.args[0].startswith(('START:', 'SUCCESS:'))]
        self.assertEqual(len(messages), 8)
        for call in messages:
            self.assertIn(ENV['RELEASE_TAG'], call.args[0])
            self.assertIn(D, call.args[0])
            self.assertTrue(call.kwargs.get('flush'))
        for platform in policy.PLATFORMS:
            for check in ('startup/persistence', 'OS/Go scan'):
                label = f"{check} | {platform} | {ENV['RELEASE_TAG']} | {D}"
                self.assertIn('START: ' + label, [c.args[0] for c in messages])
                self.assertIn('SUCCESS: ' + label, [c.args[0] for c in messages])

    def test_summary_records_success_failure_and_unexecuted_checks(self):
        with tempfile.TemporaryDirectory() as folder:
            summary = Path(folder) / 'summary'
            self.backend.fail = 'scan-image.sh:linux/amd64'
            with patch.dict(os.environ, GITHUB_STEP_SUMMARY=str(summary)), patch('builtins.print') as printed:
                with self.assertRaises(RuntimeError):
                    self.publish()
            text = summary.read_text()
            self.assertIn(ENV['RELEASE_TAG'], text)
            self.assertIn(D, text)
            self.assertIn('| linux/amd64 | startup/persistence | success |', text)
            self.assertIn('| linux/amd64 | OS/Go scan | failure |', text)
            self.assertIn('| linux/arm64 | startup/persistence | not executed |', text)
            self.assertIn('| linux/arm64 | OS/Go scan | not executed |', text)
            failure = [c for c in printed.call_args_list if c.args[0].startswith('FAILURE:')]
            self.assertEqual(len(failure), 1)
            self.assertTrue(failure[0].kwargs.get('flush'))
            self.assertEqual(self.backend.writes, [])

    def test_successful_checks_do_not_announce_publication_if_finalization_fails(self):
        with tempfile.TemporaryDirectory() as folder:
            summary = Path(folder) / 'summary'
            self.backend.fail = 'finalize'
            with patch.dict(os.environ, GITHUB_STEP_SUMMARY=str(summary)):
                with self.assertRaises(RuntimeError):
                    self.publish()
            text = summary.read_text()
            self.assertEqual(text.count('| success |'), 4)
            self.assertIn('validation results only', text)
            self.assertNotIn('Published', text)
            self.assertTrue(self.backend.release['draft'])

    def test_complete_summary_marks_checks_unexecuted_without_side_effects(self):
        self.publish()
        self.backend.calls.clear()
        self.backend.writes.clear()
        with tempfile.TemporaryDirectory() as folder:
            summary = Path(folder) / 'summary'
            with patch.dict(os.environ, GITHUB_STEP_SUMMARY=str(summary)):
                self.publish()
            self.assertEqual(summary.read_text().count('| not executed |'), 4)
            self.assertIn('Already complete', summary.read_text())
        self.assertEqual(self.backend.writes, [])
        self.assertFalse(any(c[0] in ('docker', 'bash') for c in self.backend.calls))

    def test_absent_summary_is_optional_for_local_publication(self):
        with patch.dict(os.environ):
            os.environ.pop('GITHUB_STEP_SUMMARY', None)
            self.publish()
        self.assertFalse(self.backend.release['draft'])

    def test_summary_io_error_cannot_mask_validation_failure_or_block_success(self):
        with tempfile.TemporaryDirectory() as folder, patch.dict(os.environ, GITHUB_STEP_SUMMARY=folder):
            self.backend.fail = 'docker-smoke.sh:linux/amd64'
            with self.assertRaisesRegex(RuntimeError, 'simulated failure before acceptance'):
                self.publish()
            self.assertEqual(self.backend.writes, [])
            self.backend.fail = None
            self.publish()
            self.assertFalse(self.backend.release['draft'])
