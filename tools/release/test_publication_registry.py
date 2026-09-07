import hashlib
import io
import json
import os
import unittest
from unittest.mock import patch
from urllib.error import HTTPError

import publication_registry as registry


class Response(io.BytesIO):
    def __init__(self, raw, headers=None):
        super().__init__(raw)
        self.headers = headers or {}


class RegistryTests(unittest.TestCase):
    def setUp(self):
        self.enterContext(patch.dict(os.environ, GITHUB_REPOSITORY='owner/repo', GH_TOKEN=''))
        self.raw = json.dumps(dict(schemaVersion=2, mediaType=registry.MEDIA_TYPES[0], manifests=[])).encode()
        self.digest = 'sha256:' + hashlib.sha256(self.raw).hexdigest()

    def token(self):
        return Response(b'{"token":"read-token"}')

    def resolve(self, responses, ref='v1.0.0', **kwargs):
        with patch.object(registry.OPENER, 'open', side_effect=responses):
            return registry.registry_digest('ghcr.io/owner/repo', ref, **kwargs)

    def test_content_digest_is_verified(self):
        result = self.resolve([self.token(), Response(self.raw, {'Docker-Content-Digest': self.digest})], self.digest)
        self.assertEqual(result, self.digest)
        with self.assertRaises(ValueError):
            self.resolve([self.token(), Response(self.raw, {'Docker-Content-Digest': 'sha256:'+'a'*64})])
        with self.assertRaises(ValueError):
            self.resolve([self.token(), Response(self.raw, {'Docker-Content-Digest': self.digest})], 'sha256:'+'a'*64)

    def test_only_manifest_404_can_mean_missing(self):
        for code in (401, 403, 404, 429, 500, 503):
            with self.subTest(code=code):
                error = HTTPError('https://ghcr.io/', code, 'error', {}, None)
                if code == 404:
                    self.assertIsNone(self.resolve([self.token(), error], allow_missing=True))
                else:
                    with self.assertRaises(HTTPError):
                        self.resolve([self.token(), error], allow_missing=True)
                # Token errors (including 404) are never absence evidence.
                with self.assertRaises(HTTPError):
                    self.resolve([error], allow_missing=True)
        with self.assertRaises(HTTPError):
            self.resolve([self.token(), HTTPError('url', 404, 'missing', {}, None)])

    def test_untrusted_repository_or_reference_never_receives_credentials(self):
        with patch.object(registry.OPENER, 'open') as opened:
            for image, ref in [('ghcr.io/attacker/repo', 'latest'), ('ghcr.io/owner/repo', '../latest')]:
                with self.assertRaises(ValueError):
                    registry.registry_digest(image, ref)
            opened.assert_not_called()

    def test_invalid_manifest_and_token_fail_closed(self):
        for raw in (b'{}', b'not json'):
            digest = 'sha256:' + hashlib.sha256(raw).hexdigest()
            with self.assertRaises(ValueError):
                self.resolve([self.token(), Response(raw, {'Docker-Content-Digest': digest})])
        for token in ('', None, 42):
            with self.assertRaises(ValueError):
                self.resolve([Response(json.dumps(dict(token=token)).encode())])
