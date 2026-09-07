import importlib.util
import os
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('policy', Path(__file__).with_name('publication-policy.py'))
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


class PublicationTests(unittest.TestCase):
    def test_numeric_order_and_reserved_drafts(self):
        self.assertFalse(policy.may_promote('v0.4.9', [], ['v0.4.10']))
        self.assertTrue(policy.may_promote('v0.4.10', [], ['v0.4.9']))
        self.assertFalse(policy.may_promote('v0.4.9', [{'tag_name': 'v1.0.0', 'draft': True}], []))
        self.assertFalse(policy.may_promote('v0.4.9', [{'tag_name': 'v0.5.0', 'draft': False}], []))
        self.assertTrue(policy.may_promote('v0.4.9', [], ['v0.4.9', 'v1.0.0-rc1']))

    def test_invalid_identity(self):
        for tag in ['v01.2.3', 'v1.2', 'v1.2.3;echo', '', 'main']:
            with self.assertRaises(ValueError):
                policy.version(tag)

    def run_policy(self, mode, records, tags='', fail=None):
        calls = []
        def command(*args):
            calls.append(args)
            if fail and args[:len(fail)] == fail:
                raise RuntimeError('simulated failure')
            return tags if args[:2] == ('git', 'tag') else ''
        with patch.dict(os.environ, {'RELEASE_TAG':'v0.4.9', 'GITHUB_REPOSITORY':'owner/repo', 'IMAGE':'ghcr.io/owner/repo', 'DIGEST':'sha256:'+'a'*64}), patch.object(policy.sys, 'argv', ['policy', mode]), patch.object(policy, 'releases', return_value=records), patch.object(policy, 'command', side_effect=command):
            policy.main()
        return calls

    def test_public_release_never_replaced(self):
        for mode in ['draft', 'finalize']:
            with self.assertRaises(RuntimeError):
                self.run_policy(mode, [{'tag_name':'v0.4.9','draft':False}])

    def test_no_draft_no_finalization(self):
        with self.assertRaises(RuntimeError):
            self.run_policy('finalize', [])

    def test_older_release_finalizes_without_latest(self):
        calls=self.run_policy('finalize',[{'tag_name':'v0.4.9','draft':True}], 'v0.4.10')
        self.assertFalse(any(c[0]=='docker' for c in calls))
        self.assertEqual(calls[-1][-1], '--latest=false')

    def test_latest_only_after_successful_digest_promotion(self):
        calls=self.run_policy('finalize',[{'tag_name':'v0.4.9','draft':True}], 'v0.4.9')
        self.assertEqual(calls[-2][0], 'docker')
        self.assertEqual(calls[-1][-1], '--latest=true')
        with self.assertRaises(RuntimeError):
            self.run_policy('finalize',[{'tag_name':'v0.4.9','draft':True}], 'v0.4.9', ('docker',))

    def test_api_failure_is_fatal(self):
        with patch.object(policy, 'command', side_effect=RuntimeError('unavailable')):
            with self.assertRaises(RuntimeError):
                policy.releases('owner/repo')
