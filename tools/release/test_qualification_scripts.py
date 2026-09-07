"""Exercise real shell control flow with isolated failing Docker/HTTP adapters."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import tarfile
import zipfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class QualificationScriptsTests(unittest.TestCase):
    def setUp(self):
        self.folder = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.env = dict(os.environ, PATH=str(self.folder)+os.pathsep+os.environ['PATH'],
                        DIAGNOSTICS_DIR=str(self.folder / 'diagnostics'), FAKE_ROOT=str(self.folder))

    def executable(self, name, code):
        path = self.folder / name
        path.write_text('#!' + sys.executable + '\n' + code)
        path.chmod(0o755)

    def run_script(self, name, *args):
        return subprocess.run(['bash', str(ROOT / name), *args], env=self.env,
                              capture_output=True, text=True, timeout=15)

    def smoke_adapters(self):
        self.executable('docker', '''import json, os, pathlib, sys
root = pathlib.Path(os.environ['FAKE_ROOT'])
a = sys.argv[1:]
with (root/'calls').open('a') as f: f.write(json.dumps(a)+'\\n')
if a == ['volume', 'create']: print('isolated-volume')
elif a[0] == 'run': print('restart' if '--user' in a else 'initial')
elif a[0] == 'port': print('127.0.0.1:43210')
elif a[0] == 'logs': print('diagnostic '+a[1])
elif a[0] == 'inspect': print('{"Running":false}' if 'json' in a[2] else 'false')
elif a[0] == 'rm': sys.exit(19 if os.environ.get('FAIL_PHASE') == 'initial' or a[-1] == 'restart' else 0)
elif a[:2] == ['volume', 'rm']: sys.exit(19)
''')
        self.executable('curl', '''import os, sys
assert '--connect-timeout' in sys.argv and '--max-time' in sys.argv
if os.environ.get('FAIL_PHASE') == 'initial' or sys.argv[-1].endswith('/api/auth/login'): sys.exit(22)
if sys.argv[-1].endswith('/api/auth/status'): print('{"setup_required":false}')
''')

    def test_docker_failure_retains_logs_before_cleanup_and_original_status(self):
        self.smoke_adapters()
        self.env['FAIL_PHASE'] = 'initial'
        result = self.run_script('tools/ci/docker-smoke.sh', 'local:test', 'linux/amd64')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotEqual(result.returncode, 19)
        folder = self.folder / 'diagnostics/linux-amd64'
        self.assertIn('diagnostic initial', (folder/'initial.log').read_text())
        self.assertEqual(json.loads((folder/'initial-state.json').read_text()), {'Running': False})
        calls = [json.loads(line) for line in (self.folder/'calls').read_text().splitlines()]
        self.assertLess(next(i for i,c in enumerate(calls) if c[0]=='logs'), next(i for i,c in enumerate(calls) if c[0]=='rm'))
        self.assertEqual(sorted(p.name for p in folder.iterdir()), ['initial-state.json', 'initial.log'])

    def test_restart_failure_keeps_both_lifecycles_and_original_http_status(self):
        self.smoke_adapters()
        result = self.run_script('tools/ci/docker-smoke.sh', 'local:test', 'linux/arm64')
        self.assertEqual(result.returncode, 22, result.stderr)
        folder = self.folder / 'diagnostics/linux-arm64'
        self.assertIn('diagnostic initial', (folder/'initial.log').read_text())
        self.assertIn('diagnostic restart', (folder/'restarted.log').read_text())
        self.assertEqual(len(list(folder.iterdir())), 4)

    def test_scan_failure_retains_json_and_requires_os_and_go_detection(self):
        self.executable('docker', '''import json, os, pathlib, sys
a = sys.argv[1:]
if a[0] in ('pull', 'save'):
    assert a[a.index('--platform')+1] == 'linux/arm64'
if a[0] == 'save': pathlib.Path(a[a.index('-o')+1]).write_text('fixture')
if a[0] == 'run':
    assert a[a.index('--pkg-types')+1] == 'os,library'
    assert a[a.index('--exit-code')+1] == '1'
    assert '--ignore-unfixed' not in a
    results = [{'Class':'os-pkgs'}, {'Type':'gobinary','Target':'app/webserver'}]
    if os.environ.get('MISSING_GO'): results.pop()
    print(json.dumps({'Results':results}))
    print('scanner diagnostic', file=sys.stderr)
    sys.exit(int(os.environ.get('SCAN_STATUS', '0')))
''')
        self.env['SCAN_STATUS'] = '42'
        result = self.run_script('tools/ci/scan-image.sh', 'ghcr.io/owner/repo@sha256:'+'a'*64, 'linux/arm64')
        self.assertEqual(result.returncode, 42, result.stderr)
        report = self.folder/'diagnostics/trivy-linux-arm64.json'
        self.assertEqual(len(json.loads(report.read_text())['Results']), 2)
        self.assertIn('scanner diagnostic', (report.with_suffix('.log')).read_text())
        self.env['SCAN_STATUS'], self.env['MISSING_GO'] = '0', '1'
        result = self.run_script('tools/ci/scan-image.sh', 'local:test', 'linux/arm64')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Go application binary missing', result.stderr)

    def test_archive_server_failure_preserves_output_after_extraction_cleanup(self):
        app = 'SimpleLinuxUpdater_1.2.3'
        package = self.folder / app
        package.mkdir()
        server = package / 'webserver'
        server.write_text('#!/bin/sh\necho archive-start-failure\nexit 42\n')
        server.chmod(0o755)
        dist = self.folder / 'dist'
        dist.mkdir()
        archives = []
        for target in ('linux_amd64', 'linux_arm64', 'darwin_amd64', 'darwin_arm64'):
            archive = dist / (app + '_' + target + '.tar.gz')
            with tarfile.open(archive, 'w:gz') as stream:
                stream.add(package, arcname=app)
            archives.append(archive)
        archive = dist / (app + '_windows_amd64.zip')
        with zipfile.ZipFile(archive, 'w') as stream:
            stream.write(server, app + '/webserver.exe')
        archives.append(archive)
        (dist/'checksums.txt').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.name+'\n' for p in archives))
        self.executable('curl', "import sys\nassert '--connect-timeout' in sys.argv and '--max-time' in sys.argv\nsys.exit(22)\n")
        self.env['RELEASE_TAG'] = 'v1.2.3'
        result = subprocess.run(['bash', str(ROOT/'tools/release/verify-archives.sh')],
                                cwd=self.folder, env=self.env, capture_output=True, text=True, timeout=15)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('archive-start-failure', (self.folder/'diagnostics/server.log').read_text())
        self.assertEqual([p.name for p in (self.folder/'diagnostics').iterdir()], ['server.log'])
