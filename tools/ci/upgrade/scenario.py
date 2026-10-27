#!/usr/bin/env python3
"""Private scenario, invoked only inside the networkless qualification container.

No production switches or fake services: the two actual executables own all
commands/migrations. Offline SQL creates documented crash-recovery fixtures.
Only fixed assertion labels and safe provenance leave this process.
"""
import base64
import datetime
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import socketserver
import sqlite3
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

STEPS = ['network-isolation', 'old-installation', 'old-backup-export',
         'offline-crash-fixtures', 'candidate-migration', 'candidate-restart',
         'old-archive-restore', 'restored-restart', 'no-maintenance', 'cleanup']


class QualificationInterrupted(BaseException):
    """A signal must not be swallowed by selector EINTR retry handling."""


def require(value, label):
    if not value:
        raise AssertionError(label)


class API:
    def __init__(self):
        self.base = 'http://127.0.0.1:8080'
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def request(self, method, path, data=None, allowed=(200,), raw=None, mime='application/json', binary=False):
        body = raw if raw is not None else (json.dumps(data).encode() if data is not None else None)
        request = urllib.request.Request(self.base + path, method=method, data=body,
            headers={'Origin': self.base, 'Content-Type': mime})
        try:
            response = self.client.open(request, timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        content = response.read(64 * 1024 * 1024)
        require(response.code in allowed, f'{method} {path}: HTTP {response.code}')
        return content if binary else json.loads(content)

    def upload(self, path, archive, phrase):
        boundary = 'qualification-' + secrets.token_hex(12)
        body = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="old.slubkp"\r\n'
                'Content-Type: application/octet-stream\r\n\r\n').encode() + archive
        body += (f'\r\n--{boundary}\r\nContent-Disposition: form-data; name="passphrase"\r\n\r\n'
                 f'{phrase}\r\n--{boundary}--\r\n').encode()
        return self.request('POST', path, raw=body, mime='multipart/form-data; boundary=' + boundary)


class Scenario:
    def __init__(self):
        self.report = {'steps': {name: {'status': 'not-run'} for name in STEPS}, 'exit_code': 1}
        self.work = Path(tempfile.mkdtemp(prefix='slu-upgrade-'))
        self.process = None
        self.log = None
        self.listener = None
        self.contacts = 0
        self.api = API()
        self.password = secrets.token_urlsafe(30) + 'Aa1!'
        self.phrase = secrets.token_urlsafe(32)
        self.ssh_secret = secrets.token_urlsafe(32)
        self.known = ''
        self.before = None
        self.old_schema = None
        self.archive = None

    def step(self, name, action):
        self.report['steps'][name] = {'status': 'failed'}
        started = time.monotonic()
        action()
        self.report['steps'][name] = {'status': 'passed', 'seconds': round(time.monotonic() - started, 3)}

    def db(self):
        return sqlite3.connect(self.work / 'servers.db', timeout=10)

    def start(self, binary, assets):
        for name in ('templates', 'static'):
            shutil.rmtree(self.work / name, ignore_errors=True)
            shutil.copytree(assets / name, self.work / name)
        env = {'PATH': '/usr/bin:/bin', 'HOME': str(self.work), 'TMPDIR': str(self.work),
               'GIN_MODE': 'release', 'TZ': 'UTC',
               'DEBIAN_UPDATER_LISTEN_ADDR': '127.0.0.1:8080',
               'DEBIAN_UPDATER_DB_PATH': str(self.work / 'servers.db'),
               'DEBIAN_UPDATER_KNOWN_HOSTS': str(self.work / 'known_hosts'),
               'DEBIAN_UPDATER_HOST_FACTS_AUTO_REFRESH_ENABLED': 'false',
               'DEBIAN_UPDATER_VULNERABILITY_SCAN_ENABLED': 'false',
               'DEBIAN_UPDATER_VULNERABILITY_SCAN_INTERVAL_HOURS': '0'}
        self.log = (self.work / 'private.log').open('ab')
        self.process = subprocess.Popen([str(binary)], cwd=self.work, env=env,
            stdout=self.log, stderr=subprocess.STDOUT)
        end = time.monotonic() + 30
        while time.monotonic() < end:
            require(self.process.poll() is None, 'application exited during startup')
            try:
                self.api.request('GET', '/api/auth/status')
                return
            except (OSError, AssertionError):
                time.sleep(.1)
        raise AssertionError('startup deadline')

    def stop(self):
        if self.process is not None:
            if self.process.poll() is None:
                self.process.terminate()
                try:
                    code = self.process.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    self.process.kill()
                    self.process.wait(timeout=5)
                    raise AssertionError('shutdown deadline') from None
                require(code == 0, 'application shutdown exit')
            else:
                require(self.process.returncode == 0, 'application unexpected exit')
            self.process = None
        if self.log:
            self.log.close()
            self.log = None

    def network(self):
        # Docker provides only lo, no external interfaces or default routes.
        require(set(os.listdir('/sys/class/net')) == {'lo'}, 'external network interface present')
        require(len(Path('/proc/net/route').read_text().splitlines()) == 1, 'external IPv4 route present')
        owner = self
        class Sentinel(socketserver.BaseRequestHandler):
            def handle(self):
                owner.contacts += 1
        self.listener = socketserver.ThreadingTCPServer(('127.0.0.1', 0), Sentinel)
        threading.Thread(target=self.listener.serve_forever, daemon=True).start()
        public = base64.b64encode(b'\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20' + secrets.token_bytes(32)).decode()
        self.known = f'[127.0.0.1]:{self.listener.server_address[1]} ssh-ed25519 {public}\n'
        (self.work / 'known_hosts').write_text(self.known)
        (self.work / 'known_hosts').chmod(0o600)

    def old(self):
        self.start(Path('/input/old/webserver'), Path('/input/old'))
        self.api.request('POST', '/api/auth/setup', {'username': 'qualification-admin', 'password': self.password})
        self.api.request('PUT', '/api/app-settings/timezone', {'timezone': 'Europe/Paris'})
        self.api.request('POST', '/api/servers', {'name': 'qualification-host', 'host': '127.0.0.1',
            'port': self.listener.server_address[1], 'user': 'synthetic', 'pass': self.ssh_secret,
            'disabled': True, 'tags': ['qualification', 'legacy']}, allowed=(201,))
        for cadence in ('daily', 'weekly'):
            self.api.request('POST', '/api/update-policies', {'name': 'legacy-' + cadence, 'enabled': False,
                'target_servers': ['qualification-host'], 'execution_mode': 'scan_only', 'package_scope': 'full',
                'cadence_kind': cadence, 'weekdays': ['mon'] if cadence == 'weekly' else [],
                'time_local': '03:00', 'rollout_mode': 'immediate'}, allowed=(201,))
        with self.db() as db:
            self.before = db.execute('SELECT id,username,password_hash FROM auth_users').fetchone()
            self.encrypted = db.execute('SELECT pass_enc FROM servers').fetchone()[0]
            self.encryption_config = (self.work / 'config.json').read_bytes()
            require(self.encrypted and self.ssh_secret not in self.encrypted, 'old credential not encrypted')
            require(db.execute('SELECT count(*) FROM sessions').fetchone()[0] > 0, 'old session missing')
            require(db.execute("SELECT count(*) FROM jobs WHERE kind NOT IN ('backup_export','backup_restore')").fetchone()[0] == 0, 'old worker created maintenance job')
            require(self.audit_count(db) == 0, 'old worker created maintenance audit')
            self.old_schema = self.schema(db)

    @staticmethod
    def schema(db):
        tables = [row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")]
        return {table: {row[1] for row in db.execute(f'PRAGMA table_info("{table}")')} for table in tables}

    def export(self):
        self.archive = self.api.request('POST', '/api/backup/export',
            {'passphrase': self.phrase, 'include_known_hosts': True}, binary=True)
        require(len(self.archive) > 100, 'old archive empty')
        require(self.ssh_secret.encode() not in self.archive, 'old archive plaintext')
        self.stop()

    def fixtures(self):
        # Persisted pre-crash states, no dispatch or SSH request is made.
        with self.db() as db:
            for name, status, phase in [('queued', 'queued', 'dial'),
                ('running', 'running', 'prechecks'), ('approval', 'waiting_approval', 'approval_wait'),
                ('mutation', 'running', 'apt_upgrade'), ('terminal', 'succeeded', 'complete')]:
                db.execute('INSERT INTO jobs(id,kind,server_name,actor,status,phase,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)',
                    ('qualification-' + name, 'update', 'qualification-host', 'qualification-fixture', status, phase,
                     '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z'))
            policy = db.execute("SELECT id FROM update_policies WHERE name='legacy-daily'").fetchone()[0]
            db.execute('INSERT INTO update_policy_runs(policy_id,policy_name,server_name,scheduled_for_utc,execution_mode,package_scope,status,job_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)',
                (policy, 'legacy-daily', 'qualification-host', '2026-01-01T03:00:00Z', 'scan_only', 'full',
                 'waiting_approval', 'qualification-approval', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'))
            self.job_ids = {row[0] for row in db.execute('SELECT id FROM jobs')}
            self.maintenance_audits = self.audit_count(db)
            # Deliberate diagnostic fixture corruption, never a production flag.
            if os.environ.get('QUALIFICATION_DIAGNOSTIC') == 'drop-inventory':
                db.execute("DELETE FROM servers WHERE name='qualification-host'")

    @staticmethod
    def audit_count(db):
        return db.execute("SELECT count(*) FROM audit_events WHERE action LIKE 'update.%' OR action LIKE 'server.update%' OR action LIKE 'autoremove%' OR action LIKE 'ssh.%' OR action LIKE 'apt_repair.%' OR action LIKE 'reboot.%' OR action LIKE 'sudoers.%' OR action LIKE 'scan.%'").fetchone()[0]

    def conserved(self):
        require((self.work / 'config.json').read_bytes() == self.encryption_config, 'local encryption config changed')
        with self.db() as db:
            require(db.execute('SELECT id,username,password_hash FROM auth_users').fetchone() == self.before,
                    'initial account or password hash changed')
            require(db.execute('SELECT role,disabled FROM auth_users').fetchone() == ('admin', 0), 'initial admin lost')
            row = db.execute("SELECT pass_enc FROM servers WHERE name='qualification-host'").fetchone()
            require(row is not None and row[0] == self.encrypted, 'inventory or encrypted SSH secret lost')
        inventory = self.api.request('GET', '/api/servers')
        require(len(inventory) == 1 and inventory[0]['has_password'] and inventory[0]['disabled']
                and sorted(inventory[0]['tags']) == ['legacy', 'qualification'], 'inventory API changed')
        require(self.api.request('GET', '/api/app-settings/timezone')['timezone'] == 'Europe/Paris', 'timezone changed')
        policies = self.api.request('GET', '/api/update-policies')['items']
        require(len(policies) == 2, 'policy count changed')
        for policy in policies:
            require(not policy['enabled'] and policy['execution_mode'] == 'scan_only'
                and policy['package_scope'] == 'full' and policy['time_local'] == '03:00'
                and policy['cadence_kind'] in ('daily', 'weekly'), 'legacy policy interpretation changed')
            require(policy['canary_mode'] == 'automatic' and policy['target_servers'] == ['qualification-host'], 'legacy policy targeting or canary changed')
            if policy['cadence_kind'] == 'weekly':
                require(policy['weekdays'] == ['mon'], 'weekly weekday changed')
        require((self.work / 'known_hosts').read_text() == self.known, 'SSH trust changed')
        self.api.request('POST', '/api/auth/setup', {'username': 'reopened', 'password': self.password}, allowed=(409,))

    def login(self):
        self.api.request('POST', '/api/auth/login', {'username': 'qualification-admin', 'password': self.password})
        account = self.api.request('GET', '/api/auth/identity')['account']
        require(account['id'] == self.before[0] and account['role'] == 'admin', 'identity not preserved')

    def migrate(self):
        self.start(Path('/input/candidate/webserver'), Path('/input/candidate'))
        require(not self.api.request('GET', '/api/auth/status')['authenticated'], 'old session survived migration')
        self.login()
        self.conserved()
        with self.db() as db:
            schema = self.schema(db)
            # auth_users deliberately rebuilt by the transactional account migration.
            require(all(columns <= schema.get(table, set()) for table, columns in self.old_schema.items()), 'legacy schema removed')
            require({'package_deferrals', 'package_deferral_snapshots', 'update_policy_occurrences',
                     'auth_personal_tokens'} <= set(schema), 'additive migrations missing')
            for name in ('queued', 'running', 'approval'):
                require(db.execute('SELECT status,error_class FROM jobs WHERE id=?', ('qualification-' + name,)).fetchone()
                        == ('interrupted', 'restart'), 'unfinished job not interrupted')
            require(db.execute("SELECT status,error_class FROM jobs WHERE id='qualification-mutation'").fetchone()
                    == ('failed', 'reconciliation_required'), 'interrupted mutation not reconciled')
            require(db.execute("SELECT status FROM jobs WHERE id='qualification-terminal'").fetchone()[0] == 'succeeded', 'terminal job changed')
            require(db.execute("SELECT status FROM update_policy_runs WHERE job_id='qualification-approval'").fetchone()[0]
                    == 'interrupted', 'legacy scheduled approval resumed')
            self.migrated_schema = schema
            self.epoch = db.execute('SELECT epoch FROM auth_installation_epoch').fetchone()[0]
            self.generation = db.execute('SELECT generation FROM auth_login_generation').fetchone()[0]
        self.stop()

    def restart(self):
        self.start(Path('/input/candidate/webserver'), Path('/input/candidate'))
        require(self.api.request('GET', '/api/auth/status')['authenticated'], 'new session lost on second start')
        self.conserved()
        with self.db() as db:
            require(self.schema(db) == self.migrated_schema, 'second migration changed schema')
            require(db.execute('SELECT generation FROM auth_login_generation').fetchone()[0] == self.generation, 'session migration repeated')
            require(db.execute('SELECT epoch FROM auth_installation_epoch').fetchone()[0] == self.epoch, 'epoch changed on restart')
        self.no_maintenance()

    def token_request(self, token, allowed=(200,)):
        client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        request = urllib.request.Request(self.api.base + '/api/servers', headers={'Authorization': 'Bearer ' + token})
        try:
            response = client.open(request, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        response.read()
        require(response.code in allowed, 'personal token restore invalidation failed')

    def restore(self):
        # Existing candidate credentials must be invalidated by the actual old archive.
        token = self.api.request('POST', '/api/auth/tokens', {'name': 'qualification-token', 'scopes': ['operational:read'], 'expires_at': (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1)).isoformat()}, allowed=(201,))
        require(token.get('secret'), 'candidate token creation failed')
        self.token_request(token['secret'])
        verified = self.api.upload('/api/backup/verify', self.archive, self.phrase)
        require(verified['valid'] and verified['restore_ready'], 'old archive not restore ready')
        self.api.upload('/api/backup/restore', self.archive, self.phrase)
        require(not self.api.request('GET', '/api/auth/status')['authenticated'], 'restore retained session')
        self.token_request(token['secret'], allowed=(401,))
        self.login()
        self.conserved()
        with self.db() as db:
            require(db.execute('SELECT epoch FROM auth_installation_epoch').fetchone()[0] != self.epoch, 'restore did not advance epoch')
            require(db.execute('SELECT count(*) FROM auth_personal_tokens WHERE revoked=0').fetchone()[0] == 0, 'restore retained personal tokens')
            require(db.execute('SELECT count(*) FROM auth_recovery_codes').fetchone()[0] == 0, 'restore retained recovery codes')
        automation = self.api.request('GET', '/api/backup/automation')
        require(not automation['configured'] and not automation['config']['enabled'], 'legacy restore enabled unvalidated automation')
        self.stop()

    def restored_restart(self):
        self.start(Path('/input/candidate/webserver'), Path('/input/candidate'))
        require(self.api.request('GET', '/api/auth/status')['authenticated'], 'restored new session lost at restart')
        self.conserved()
        self.stop()

    def no_maintenance(self):
        require(self.contacts == 0, 'unexpected SSH contact')
        with self.db() as db:
            ids = {row[0] for row in db.execute("SELECT id FROM jobs WHERE kind NOT IN ('backup_export','backup_restore')")}
            require(ids <= self.job_ids, 'new maintenance job created')
            self.report['no_maintenance'] = {'ssh_contacts': self.contacts, 'new_maintenance_jobs': 0, 'new_maintenance_audits': 0}
            require(self.audit_count(db) == self.maintenance_audits, 'maintenance audit created')
            require(db.execute("SELECT count(*) FROM update_policy_runs WHERE status IN ('queued','running','waiting_approval')").fetchone()[0] == 0, 'active scheduled maintenance remains')

    def cleanup(self):
        try:
            self.stop()
        finally:
            if self.listener:
                self.listener.shutdown()
                self.listener.server_close()
            shutil.rmtree(self.work)

    def run(self):
        actions = [self.network, self.old, self.export, self.fixtures, self.migrate,
                   self.restart, self.restore, self.restored_restart, self.no_maintenance]
        try:
            for name, action in zip(STEPS, actions):
                self.step(name, action)
            self.report['exit_code'] = 0
        except BaseException as error:
            # Never print exception bodies: SQLite/HTTP/runtime diagnostics can contain secrets.
            self.report['failure_type'] = type(error).__name__
            if isinstance(error, AssertionError):
                self.report['assertion'] = str(error)  # only our fixed labels / method, path, status
        finally:
            try:
                self.step('cleanup', self.cleanup)
            except BaseException:
                self.report['exit_code'] = 1
            print(json.dumps(self.report), flush=True)
        return self.report['exit_code']


if __name__ == '__main__':
    def interrupted(signum, _frame):
        raise QualificationInterrupted()
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raise SystemExit(Scenario().run())
