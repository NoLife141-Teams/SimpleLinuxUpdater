#!/usr/bin/env python3
"""Qualify exported production scripts on owned disposable SSH/APT targets only."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import signal
import subprocess
import time

import run

HELPER = '/usr/local/sbin/simplelinuxupdater-root-helper'
MANAGED = '/etc/sudoers.d/simplelinuxupdater'
LEGACY = '/etc/sudoers.d/apt-nopasswd'
PATHS = (HELPER, MANAGED, LEGACY)


def qualify(h, binary, fixtures):
    shutil.copyfile(binary, h.work / 'webserver')
    (h.work / 'webserver').chmod(0o755)
    h.start(h.work / 'webserver')
    # No root SSH is permitted during the onboarding qualification.
    h.target('sh', '-c', "sed -i 's/PermitRootLogin yes/PermitRootLogin no/' /etc/ssh/sshd_config.d/fixture.conf; pkill -HUP -x sshd")
    run.require('permitrootlogin no' in h.target('/usr/sbin/sshd', '-T'), 'root SSH not disabled')
    draft = {'name': 'onboard-operator', 'host': h.name, 'port': 22, 'user': 'operator',
             'pass': h.password, 'auth_method': 'password', 'fingerprint_sha256': h.original_fingerprint,
             'confirmed': True, 'tags': ['fixture']}
    bundle = run.command([str(binary), 'helper', 'export', '--user', 'operator']).encode()

    def snapshot():
        return h.target('python3', '-c', '''import pathlib,json,hashlib,os,stat
paths=['/usr/local/sbin/simplelinuxupdater-root-helper','/etc/sudoers.d/simplelinuxupdater','/etc/sudoers.d/apt-nopasswd','/var/lib/dpkg/status','/run/slu-unknown-executed']
paths += [str(p) for parent in ['/usr/local/sbin','/etc/sudoers.d'] for p in pathlib.Path(parent).glob('.simplelinuxupdater*')]
out={}
for name in paths:
 p=pathlib.Path(name)
 if p.is_symlink(): out[name]={'link':os.readlink(name)}
 elif p.exists():
  s=p.stat();out[name]={'uid':s.st_uid,'mode':stat.S_IMODE(s.st_mode),'sha256':hashlib.sha256(p.read_bytes()).hexdigest()}
 else: out[name]=None
print(json.dumps(out,sort_keys=True))''')

    def check(expected=None):
        before = snapshot()
        result = h.api.request('POST', '/api/servers/onboarding/check', draft)
        run.require(snapshot() == before, 'Check mutated target')
        run.require(h.api.request('GET', '/api/servers') == [], 'Check saved inventory')
        sudo = next(c for c in result['checks'] if c['id'] == 'sudo')
        if expected:
            run.require(not result['ready'] and expected in sudo['message'], str(sudo))
            run.require('helper export --user' in sudo['remediation'] and 'Retry checks' in sudo['remediation'], 'missing remediation')
        else:
            run.require(result['ready'] and sudo['status'] == 'passed', str(result))
        return {'ready': result['ready'], 'permission': sudo}

    def reset():
        h.target('rm', '-f', '--', *PATHS, '/run/slu-unknown-executed')

    def write(path, value, mode='0755'):
        run.require(path in PATHS, 'unexpected fixture path')
        h.target('sh', '-c', 'cat > "$1"; chmod "$2" "$1"', 'fixture', path, mode, data=value.encode())

    def result(*args, data=None):
        run.require(h.owned('container', h.container), 'ownership mismatch')
        run.require(h.docker('exec', h.container, 'cat', '/run/slu-fixture-owner').strip() == h.owner, 'marker mismatch')
        p = subprocess.run([*run.DOCKER, 'exec', '-i', h.container, *args], input=data,
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
        return {'exit_code': p.returncode, 'output': h.redact_text(p.stdout.decode(errors='replace'))}

    def preflight(user, token):
        before = snapshot()
        command = fixtures['required_command'] + ' && touch /run/slu-unknown-executed'
        args = ('sh', '-c', command) if user == 'root' else ('runuser', '-u', 'operator', '--', 'sh', '-c', command)
        observed = result(*args)
        run.require(observed['exit_code'] == 65 and token in observed['output'] and 'protocol 3 required' in observed['output'], str(observed))
        run.require(snapshot() == before, 'failed preflight executed a command or mutated target')
        return observed

    reset()
    with h.scenario('fresh-non-root-check-read-only') as record:
        record.update(check('absent'))
        record['root_ssh'] = 'disabled'
        record['root_deferral_preflight'] = preflight('root', 'SLU_HELPER_MISSING')
        record['operator_deferral_preflight'] = preflight('operator', 'SLU_HELPER_MISSING')
    for version, capabilities in [('2', False), ('3', False)]:
        with h.scenario('old-helper-' + version + '-without-capabilities') as record:
            old = '#!/bin/sh\n# Managed by SimpleLinuxUpdater; do not edit.\n# Protocol version ' + version + '\nexit 64\n'
            write(HELPER, old)
            record.update(check('too old'))
            for user in ('root', 'operator'):
                record[user] = preflight(user, 'SLU_HELPER_OLD')
    with h.scenario('explicit-install-byte-identical') as record:
        installed = result('sh', '-s', data=bundle)
        run.require(installed['exit_code'] == 0, str(installed))
        run.require(h.target('cat', HELPER) == fixtures['helper'], 'installed helper differs from production generator')
        run.require(h.target('cat', MANAGED) == fixtures['sudoers'], 'installed sudoers differs from production generator')
        run.require(h.target('runuser', '-u', 'operator', '--', HELPER, 'capabilities').strip() == fixtures['capabilities'], 'capabilities failed')
        record.update(install=installed, installed_files=json.loads(snapshot()), check=check())
    with h.scenario('sudo-policy-denied-diagnostic') as record:
        h.target('rm', MANAGED)
        record.update(check('sudo policy denies'))
        record['preflight'] = preflight('operator', 'SLU_HELPER_SUDO_DENIED')
        # sudo -l alone can list a PASSWD command when another command has
        # NOPASSWD. The actual read-only dpkg-audit must still refuse access.
        mixed = fixtures['sudoers'].replace('NOPASSWD:', 'PASSWD:') + 'operator ALL=(root) NOPASSWD: /usr/bin/id\n'
        write(MANAGED, mixed, '0440')
        record['mixed_policy_listing'] = result('runuser', '-u', 'operator', '--', 'sudo', '-n', '-l', '--', HELPER, 'dpkg-audit')
        run.require(record['mixed_policy_listing']['exit_code'] == 0, 'fixture did not reproduce permissive sudo listing')
        record['mixed_password_policy'] = check('sudo policy denies')
        run.require(result('sh', '-s', data=bundle)['exit_code'] == 0, 'restore managed policy failed')
    with h.scenario('non-root-onboarding-after-explicit-install') as record:
        record.update(check())
        saved = h.api.request('POST', '/api/servers/onboarding', draft, allowed=(201,))
        inventory = h.api.request('GET', '/api/servers')
        run.require(len(inventory) == 1 and inventory[0]['user'] == 'operator', 'non-root onboarding not saved')
        record.update(saved_user=inventory[0]['user'], root_ssh='disabled')
        h.api.request('DELETE', '/api/servers/' + draft['name'])
    for path in PATHS:
        for kind in ('unmarked', 'symlink'):
            with h.scenario('refuse-' + Path(path).name + '-' + kind) as record:
                reset()
                if kind == 'symlink': h.target('ln', '-s', '/var/lib/dpkg/status', path)
                else: write(path, '#!/bin/sh\ntouch /run/slu-unknown-executed\n', '0755' if path == HELPER else '0440')
                before = snapshot()
                refused = result('sh', '-s', data=bundle)
                run.require(refused['exit_code'] == 65 and before == snapshot(), 'unsafe path replaced: ' + str(refused))
                if path == HELPER: record['check'] = check('not a recognized')
                record.update(refusal=refused, unchanged=True)
    with h.scenario('custom-legacy-refused') as record:
        reset()
        write(LEGACY, fixtures['legacy'] + '# customized policy\n', '0440')
        before = snapshot()
        refused = result('sh', '-s', data=bundle)
        run.require(refused['exit_code'] == 65 and snapshot() == before, 'custom legacy changed')
        record.update(refusal=refused, unchanged=True)
    with h.scenario('legacy-extra-newline-refused') as record:
        reset()
        write(LEGACY, fixtures['legacy'] + '\n', '0440')
        before = snapshot()
        refused = result('sh', '-s', data=bundle)
        run.require(refused['exit_code'] == 65 and snapshot() == before, 'non-exact legacy changed')
        record.update(refusal=refused, unchanged=True)
    with h.scenario('exact-legacy-migrated') as record:
        reset()
        write(LEGACY, fixtures['legacy'], '0440')
        installed = result('sh', '-s', data=bundle)
        run.require(installed['exit_code'] == 0 and json.loads(snapshot())[LEGACY] is None, 'exact migration failed')
        record.update(install=installed, check=check())
    with h.scenario('visudo-failure-cleans-temporaries') as record:
        before = snapshot()
        # Obsolete user in a fixture export is safe syntactically, so inject a
        # failing visudo only inside the owned disposable target.
        h.target('mv', '/usr/sbin/visudo', '/usr/sbin/visudo.fixture-real')
        try:
            h.target('sh', '-c', "printf '#!/bin/sh\\nexit 79\\n' > /usr/sbin/visudo; chmod 755 /usr/sbin/visudo")
            refused = result('sh', '-s', data=bundle)
            run.require(refused['exit_code'] == 79 and snapshot() == before, 'failed visudo changed managed files or leaked temps')
            record.update(refusal=refused, unchanged=True)
        finally:
            h.target('mv', '-f', '/usr/sbin/visudo.fixture-real', '/usr/sbin/visudo')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--distro', required=True, choices=('debian13', 'ubuntu2404'))
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--fixtures', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    h = run.Harness(args.distro, args.output.resolve())
    started = time.monotonic()
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        qualify(h, args.binary.resolve(), json.loads(args.fixtures.read_text()))
    except BaseException as error:
        h.records.append({'scenario': 'harness', 'status': 'failed', 'error': str(error) or type(error).__name__})
    finally:
        h.cleanup()
        args.output.mkdir(parents=True, exist_ok=True)
        report = {'distribution': args.distro, 'base_image': run.IMAGES[args.distro],
                  'binary_sha256': hashlib.sha256(args.binary.read_bytes()).hexdigest(),
                  'fixture_sha256': hashlib.sha256(args.fixtures.read_bytes()).hexdigest(),
                  'source_head': run.command(['git', 'rev-parse', 'HEAD'], cwd=run.REPO).strip(),
                  'scenarios': h.records, 'cleanup_errors': h.cleanup_errors,
                  'seconds': round(time.monotonic()-started, 3),
                  'limits': ['No reboot/systemd qualification; helper distribution scope only.',
                             'Local fixture APT packages; no official provenance claim.']}
        (args.output/'report.json').write_text(json.dumps(h.redact_evidence(report), indent=2)+'\n')
    run.require(not h.cleanup_errors and not any(r['status'] == 'failed' for r in h.records), 'qualification failed; see report.json')


if __name__ == '__main__': main()
