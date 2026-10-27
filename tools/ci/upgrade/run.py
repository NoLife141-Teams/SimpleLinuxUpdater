#!/usr/bin/env python3
"""Qualify a Linux amd64 candidate against the pinned official v0.4.16 archive.

Only report.json is public. All binaries, assets, process output, cookies,
archives and databases live in an owned temporary directory or container tmpfs.
"""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import signal
import subprocess
import tarfile
import tempfile
import time
import urllib.request
import uuid

HERE = Path(__file__).resolve().parent
PIN = json.loads((HERE / 'release.json').read_text())


class QualificationInterrupted(BaseException):
    """A signal must not be swallowed by selector EINTR retry handling."""


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def checked_file(name, destination, cache):
    if cache and (cache / name).is_file():
        shutil.copyfile(cache / name, destination)
    else:
        with urllib.request.urlopen(PIN['base_url'] + name, timeout=60) as response, destination.open('wb') as output:
            started = time.monotonic()
            total = 0
            while block := response.read(1024 * 1024):
                total += len(block)
                if total > 128 * 1024 * 1024 or time.monotonic() - started > 120:
                    raise ValueError('release download budget exceeded')
                output.write(block)
    if digest(destination) != PIN['files'][name]:
        raise ValueError('release file checksum mismatch')


def command(args, timeout=60, **kwargs):
    return subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, check=True, **kwargs)


def cleanup_container(owner, recover_report=None):
    """Mutate only an exact named container whose random ownership label agrees."""
    names = command(['docker', 'container', 'ls', '--all', '--filter', 'name=^/' + owner + '$', '--format', '{{.Names}}'], timeout=15).stdout.decode().splitlines()
    if not names:
        return
    if names != [owner]:
        raise ValueError('container ownership name mismatch')
    labels = json.loads(command(['docker', 'inspect', '--format', '{{json .Config.Labels}}', owner], timeout=15).stdout)
    if labels.get('slu.upgrade.owner') != owner:
        raise ValueError('container ownership label mismatch')
    failure = None
    try:
        command(['docker', 'stop', '--time', '25', owner], timeout=35)
    except Exception as error:
        failure = error
    if recover_report is not None:
        try:
            recover_report(command(['docker', 'logs', owner], timeout=15).stdout,
                int(command(['docker', 'inspect', '--format', '{{.State.ExitCode}}', owner], timeout=15).stdout))
        except Exception as error:
            failure = failure or error
    # Removal is independent of graceful shutdown; the owner was verified above.
    try:
        command(['docker', 'rm', '--force', owner], timeout=15)
    except Exception as error:
        failure = error
    if failure:
        raise failure


def cleanup_resources(owner, work, creation_attempted, recover_report=None):
    errors = []
    try:
        if creation_attempted:
            cleanup_container(owner, recover_report)
    except BaseException:
        errors.append('owned-container-cleanup')
    try:
        shutil.rmtree(work)
    except BaseException:
        errors.append('owned-temporary-files-cleanup')
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-root', type=Path, default=Path.cwd(), help='candidate checkout containing tracked web assets')
    parser.add_argument('--candidate', required=True, type=Path, help='fresh Linux amd64 executable; no Go needed to run the harness')
    parser.add_argument('--candidate-sha', required=True, help='source Git commit identifying the candidate')
    parser.add_argument('--output', type=Path, default=Path('upgrade-evidence'))
    parser.add_argument('--release-cache', type=Path, help='optional untrusted cache, every file still verified')
    parser.add_argument('--diagnostic', choices=['drop-inventory'], help='negative control: corrupt only the disposable legacy fixture before migration')
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    report = {'schema': 1, 'exit_code': 1,
        'old': {'version': PIN['version'], 'commit': PIN['commit'], 'release_url': PIN['base_url'], 'files': PIN['files']},
        'candidate': {'commit': args.candidate_sha}, 'runtime': PIN['runtime'],
        'diagnostic': args.diagnostic,
        'scenario': {'exit_code': None, 'steps': {name: {'status': 'not-run'} for name in
            ['network-isolation', 'old-installation', 'old-backup-export', 'offline-crash-fixtures',
             'candidate-migration', 'candidate-restart', 'old-archive-restore', 'restored-restart', 'no-maintenance', 'cleanup']}},
        'steps': {name: {'status': 'not-run'} for name in
            ['prepare', 'provenance', 'container', 'cleanup']}}
    owner = 'slu-upgrade-' + uuid.uuid4().hex
    work = Path(tempfile.mkdtemp(prefix=owner + '-'))
    creation_attempted = False
    def interrupt(_signum, _frame):
        raise QualificationInterrupted()
    signal.signal(signal.SIGTERM, interrupt)
    signal.signal(signal.SIGINT, interrupt)
    try:
        report['steps']['prepare'] = {'status': 'failed'}
        if not re.fullmatch(r'[0-9a-f]{40}', args.candidate_sha):
            raise ValueError('candidate source SHA must be a full Git commit')
        report['harness_sha256'] = {name: digest(HERE / name) for name in ('run.py', 'scenario.py', 'release.json', 'negative_control.py')}
        payload = work / 'input'
        payload.mkdir()
        candidate = payload / 'candidate'
        candidate.mkdir()
        shutil.copyfile(args.candidate.resolve(), candidate / 'webserver')
        (candidate / 'webserver').chmod(0o755)
        report['candidate']['binary_sha256'] = digest(candidate / 'webserver')
        assets = command(['git', '-C', str(args.source_root.resolve()), 'ls-files', '-z', '--', 'templates/', 'static/']).stdout.decode().split('\0')
        asset_hashes = {}
        for relative in filter(None, assets):
            target = candidate / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(args.source_root / relative, target)
            asset_hashes[relative] = digest(target)
        report['candidate']['assets_sha256'] = hashlib.sha256(json.dumps(asset_hashes, sort_keys=True).encode()).hexdigest()
        shutil.copyfile(HERE / 'scenario.py', payload / 'scenario.py')
        report['scenario_sha256'] = digest(HERE / 'scenario.py')
        report['steps']['prepare'] = {'status': 'passed'}
        report['steps']['provenance'] = {'status': 'failed'}
        for name in PIN['files']:
            checked_file(name, work / name, args.release_cache)
        publication = json.loads((work / 'publication.json').read_text())
        if publication['commit'] != PIN['commit'] or publication['tag'] != PIN['version'] or publication['repository'] != 'NoLife141-Teams/SimpleLinuxUpdater':
            raise ValueError('release publication provenance mismatch')
        archive_name = 'SimpleLinuxUpdater_0.4.16_linux_amd64.tar.gz'
        if f"{PIN['files'][archive_name]}  ./{archive_name}" not in (work / 'checksums.txt').read_text().splitlines():
            raise ValueError('archive absent from published checksum list')
        with tarfile.open(work / archive_name) as archive:
            archive.extractall(work / 'release', filter='data')
        shutil.move(str(work / 'release/SimpleLinuxUpdater_0.4.16'), payload / 'old')
        report['old']['binary_sha256'] = digest(payload / 'old/webserver')
        report['old']['publication'] = publication
        report['steps']['provenance'] = {'status': 'passed'}
        report['steps']['container'] = {'status': 'failed'}
        command(['docker', 'pull', '--platform', 'linux/amd64', PIN['runtime']], timeout=180)
        creation_attempted = True
        command(['docker', 'create', '--name', owner, '--label', 'slu.upgrade.owner=' + owner,
            '--platform', 'linux/amd64', '--network', 'none', '--read-only', '--cap-drop', 'ALL',
            '--security-opt', 'no-new-privileges', '--pids-limit', '128', '--memory', '1g', '--cpus', '2',
            '--user', f'{os.getuid()}:{os.getgid()}', '--tmpfs', '/tmp:rw,exec,nosuid,size=512m,mode=1777',
            '--mount', f'type=bind,src={payload},dst=/input,readonly',
            '-e', 'QUALIFICATION_DIAGNOSTIC=' + (args.diagnostic or ''),
            PIN['runtime'], 'python3', '-B', '/input/scenario.py'])
        report['scenario']['status'] = 'started-unobserved'
        for step in report['scenario']['steps'].values():
            step['status'] = 'not-observed'
        completed = subprocess.run(['docker', 'start', '--attach', owner], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=240)
        report['container_exit_code'] = completed.returncode
        child = json.loads(completed.stdout)
        report['scenario'] = child
        if completed.returncode != 0 or child['exit_code'] != 0:
            raise ValueError('scenario failed')
        report['steps']['container'] = {'status': 'passed'}
        report['exit_code'] = 0
    except BaseException as error:
        report['failure_type'] = type(error).__name__
        # Captured stderr, HTTP bodies, paths and exceptions are intentionally not published.
    finally:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        def recover_report(raw, exit_code):
            report['container_exit_code'] = exit_code
            if not raw:
                return
            recovered = json.loads(raw)
            if isinstance(recovered.get('steps'), dict) and 'exit_code' in recovered:
                report['scenario'] = recovered
        errors = cleanup_resources(owner, work, creation_attempted, recover_report)
        report['steps']['cleanup'] = {'status': 'failed' if errors else 'passed'}
        if errors:
            report['steps']['cleanup']['failures'] = errors
            report['exit_code'] = 1
        (args.output / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
        print(json.dumps({'exit_code': report['exit_code'], 'report': str(args.output / 'report.json')}))
    return report['exit_code']


if __name__ == '__main__':
    raise SystemExit(main())
