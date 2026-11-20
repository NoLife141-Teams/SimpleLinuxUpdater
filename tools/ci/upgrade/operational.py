#!/usr/bin/env python3
"""Qualify a candidate image on disposable Docker volumes and a real VM."""
import argparse
import base64
import ipaddress
import json
import re
import secrets
import shutil
import signal
import subprocess
import tarfile
import tempfile
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from copy import copy
from dataclasses import dataclass
from pathlib import Path

from scenario import API, require

HERE = Path(__file__).resolve().parent
DOCKER = ("docker", "--host=unix:///var/run/docker.sock")
LABEL = "io.simplelinuxupdater.operational-qualification"
PIN = json.loads((HERE / "operational-release.json").read_text())
PYTHON_IMAGE = json.loads((HERE / "release.json").read_text())["runtime"]
PLATFORM = "linux/amd64"
BATCH_PATH = "/api/status/batches"
BATCH_SERVER = "maintenance-batch-fixture"
PROBE_PORT = 22922
PROBE_CONTACTS = "/tmp/slu-qualification-batch-contacts"
WEBHOOK_PORT = 22923
WEBHOOK_CONTACTS = "/tmp/slu-qualification-webhook-contacts"
WEBHOOK_URL = f"http://127.0.0.1:{WEBHOOK_PORT}/receive"
UPGRADE_HOST_TOKEN = "[127.0.0.1]:9"
LEGACY_APT_SERVER = "legacy-apt-fixture"
LEGACY_APT_JOB = "qualification-legacy-apt-incident"


@dataclass(frozen=True)
class PublishedBackup:
    image: str
    credentials: dict[str, str]
    archive: bytes
    passphrase: str
    known_host: str


@dataclass(frozen=True)
class MaintenanceBatchFixture:
    source: dict
    retry: dict
    terminal_hosts: dict
    other_account: dict[str, str]


def fixture_ssh_key():
    key = base64.b64encode(b"\0\0\0\x0bssh-ed25519\0\0\0\x20" + secrets.token_bytes(32)).decode()
    return f"ssh-ed25519 {key}"


def command(*args, timeout=90, stdin=None, stdout=subprocess.PIPE):
    # Report the failing operation without storing application logs, request
    # bodies, environment values or raw subprocess output in evidence.
    operation = ' '.join(map(str, args[:4]))
    try:
        result = subprocess.run(args, stdin=stdin, stdout=stdout, stderr=subprocess.PIPE, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise RuntimeError(f"{operation} timed out after {timeout} seconds") from None
    if result.returncode:
        raise RuntimeError(f"{operation} exited {result.returncode}")
    return result.stdout.decode().strip() if result.stdout is not None else ""


def docker(*args, **kwargs):
    return command(*DOCKER, *args, **kwargs)


def until(predicate, failure, seconds=10):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.1)
    raise AssertionError(failure)


@contextmanager
def container_archive(source):
    # Docker can run through an elevated wrapper. Extract as the harness user
    # so the resulting tree can always be removed by its owner.
    with tempfile.TemporaryFile() as stream:
        docker("cp", source, "-", stdout=stream)
        stream.seek(0)
        with tarfile.open(fileobj=stream) as archive:
            yield archive


class Qualification:
    def __init__(self, work, output, source, sha):
        self.work = work
        self.output = output
        self.source = source
        self.sha = sha
        self.prefix = "slu-operational-" + uuid.uuid4().hex
        self.label = LABEL + "=" + self.prefix
        self.network = self.prefix + "-bridge"
        self.candidate = self.prefix + ":candidate"
        self.vm_runner = self.prefix + ":vm"
        self.apt_fixture_image = self.prefix + ":apt-fixture"
        self.report = {
            "schema_version": 1,
            "candidate_commit": sha,
            "published_version": PIN["published_version"],
            "published_image": PIN["published_image"],
            "status": "not_executed",
            "candidate_build": {"status": "not_executed"},
            "docker_upgrade": {"status": "not_executed"},
            "docker_rollback": {"status": "not_executed"},
            "maintenance_batches": {
                "status": "not_executed",
                "history_after_restart": "not_executed",
                "account_isolation": "not_executed",
                "single_retry_admission": "not_executed",
                "interrupted_job_recovery": "not_executed",
            },
            "candidate_backup_restore": {
                "status": "not_executed",
                "archive_export": "not_executed",
                "isolated_batch_history": "not_executed",
                "isolated_retry_admission": "not_executed",
                "live_terminal_delivery": "not_executed",
                "obsolete_approval": "not_executed",
            },
            "controlled_reboot_vm": {"status": "not_executed"},
            "cleanup": {"status": "not_executed"},
        }

    def step(self, name, action):
        started = time.monotonic()
        self.report[name]["status"] = "failed"
        try:
            result = action()
        finally:
            self.report[name]["seconds"] = round(time.monotonic() - started, 3)
        self.report[name]["status"] = "passed"
        print(f"{name} passed", flush=True)
        return result

    def build_candidate(self):
        require(command("git", "-C", self.source, "rev-parse", "HEAD") == self.sha,
                "candidate SHA does not match the checked-out commit")
        context = self.work / "source.tar"
        command("git", "-C", self.source, "archive", "--format=tar", "-o", context, self.sha)
        with context.open("rb") as source:
            docker("build", "--platform", PLATFORM, "--label", self.label, "--label",
                   "org.opencontainers.image.revision=" + self.sha, "--tag", self.candidate, "-", stdin=source, timeout=480)
        self.report["candidate_build"]["image_id"] = docker("image", "inspect", "--format", "{{.Id}}", self.candidate)

    def start_app(self, name, image, volume):
        docker("run", "--pull", "never", "--detach", "--label", self.label, "--name", name,
               "--network", self.network, "--env", "DEBIAN_UPDATER_HOST_FACTS_AUTO_REFRESH_ENABLED=false",
               "--mount", f"type=volume,src={volume},dst=/data", image)
        return self.connect(name)

    def create_volume(self, suffix):
        volume = self.prefix + "-" + suffix
        docker("volume", "create", "--label", self.label, volume)
        owner = docker("volume", "inspect", "--format", '{{index .Labels "' + LABEL + '"}}', volume)
        require(owner == self.prefix, "refusing to mount a volume owned by another run")
        return volume

    def connect(self, name, api=None):
        address = ipaddress.IPv4Address(docker("container", "inspect", "--format",
            "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name))
        require(address.is_private and not address.is_loopback and not address.is_unspecified,
                "expected an app address on the private qualification bridge")
        if api is None:
            api = API(follow_redirects=False)
        api.base = f"http://{address}:8080"
        deadline = time.monotonic() + 40
        while (remaining := deadline - time.monotonic()) > 0:
            try:
                api.request("GET", "/api/auth/status", timeout=min(30, remaining))
                return api
            except OSError:
                pass
            except AssertionError as error:
                if str(error) != "GET /api/auth/status: HTTP 503":
                    raise
            time.sleep(min(0.4, max(0, deadline - time.monotonic())))
        raise RuntimeError("application did not become ready on the private qualification bridge")

    def verify_inventory(self, api, container, known_host):
        servers = api.request("GET", "/api/servers")
        require(len(servers) == 1 and servers[0]["name"] == "upgrade-fixture"
                and servers[0]["disabled"] and servers[0]["has_password"]
                and servers[0]["tags"] == ["qualification"], "fixture inventory was not preserved")
        require(servers[0]["host_key_status"] == "trusted" and
                known_host in docker("exec", "--user", "app", container, "cat", "/data/known_hosts").splitlines(),
                "fixture SSH host-key trust was not preserved")
        policies = api.request("GET", "/api/update-policies")["items"]
        require(len(policies) == 1 and policies[0]["name"] == "Upgrade fixture"
                and not policies[0]["enabled"] and policies[0]["target_servers"] == ["upgrade-fixture"]
                and policies[0]["time_local"] == "03:00", "fixture policy was not preserved")

    def apply_archive(self, api, archive, passphrase, credentials):
        require(api.upload("/api/backup/verify", archive, passphrase)["restore_ready"],
                "qualification archive was not restore-ready")
        # Check server-side invalidation independently of the response expiring cookies.
        initial_session = API(follow_redirects=False)
        initial_session.base = api.base
        for cookie in api.cookies:
            initial_session.cookies.set_cookie(copy(cookie))
        require(initial_session.request("GET", "/api/auth/status")["authenticated"],
                "initialization session was not established before restore")
        api.upload("/api/backup/restore", archive, passphrase)
        require(not api.request("GET", "/api/auth/status")["authenticated"] and
                not initial_session.request("GET", "/api/auth/status")["authenticated"],
                "restore retained the previous session")
        api.request("POST", "/api/auth/login", credentials)

    def initialize_restore_target(self, name, image, volume, username):
        api = self.start_app(name, image, volume)
        require(api.request("GET", "/api/auth/status")["setup_required"],
                "restore target was already initialized")
        api.request("POST", "/api/auth/setup", {"username": username, "password": secrets.token_urlsafe(28) + "Aa1!"})
        return api

    def restore_archive(self, name, image, volume, backup, setup_username):
        restored = self.initialize_restore_target(name, image, volume, setup_username)
        self.apply_archive(restored, backup.archive, backup.passphrase, backup.credentials)
        self.verify_inventory(restored, name, backup.known_host)
        docker("restart", "--timeout", "25", name)
        self.connect(name, restored)
        restored.request("POST", "/api/auth/login", backup.credentials)
        self.verify_inventory(restored, name, backup.known_host)

    def docker_rollback(self, backup):
        self.restore_archive(self.prefix + "-rollback", backup.image, self.create_volume("rollback-data"), backup, "rollback-admin")

    def add_legacy_apt_target(self, api, app_name):
        fixture = self.source / "tools/ci/ssh-apt"
        base = json.loads((fixture / "images.json").read_text())["debian13"]
        docker("build", "--platform", PLATFORM, "--build-arg", "BASE_IMAGE=" + base,
               "--label", self.label, "--file", fixture / "target.Dockerfile",
               "-t", self.apt_fixture_image, fixture, timeout=180)
        name = self.prefix + "-apt-target"
        docker("run", "--pull", "never", "--detach", "--label", self.label, "--name", name,
               "--network", self.network, "--env", "SLU_FIXTURE_OWNER=" + uuid.uuid4().hex,
               "--cap-add=SYS_PTRACE", "--pids-limit", "256", "--memory", "768m", self.apt_fixture_image)
        address = str(ipaddress.IPv4Address(docker("inspect", "--format",
            "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)))
        public_keys = until(lambda: self.legacy_host_keys(name), "legacy APT target did not generate its SSH keys")
        password = secrets.token_urlsafe(32)
        with tempfile.TemporaryFile() as stream:
            stream.write(f"root:{password}\n".encode())
            stream.seek(0)
            docker("exec", "--interactive", name, "chpasswd", stdin=stream)
        docker("exec", "--user", "app", app_name, "/bin/sh", "-ec",
               'umask 077; printf "%s\\n" "$1" >> /data/known_hosts',
               "seed-legacy-host-keys", "\n".join(f"{address} {key}" for key in public_keys))
        api.request("POST", "/api/servers", {"name": LEGACY_APT_SERVER, "host": address, "port": 22,
            "user": "root", "pass": password}, allowed=(201,))

    def legacy_host_keys(self, name):
        try:
            keys = docker("exec", name, "cat",
                          *(f"/etc/ssh/ssh_host_{algorithm}_key.pub" for algorithm in ("ecdsa", "ed25519", "rsa")))
        except RuntimeError:
            return None
        lines = keys.splitlines()
        if len(lines) == 3:
            return [" ".join(key.split()[:2]) for key in lines]
        return None

    def copy_upgrade_volume(self, source, destination, suffix):
        docker("run", "--pull", "never", "--rm", "--label", self.label, "--name", self.prefix + "-" + suffix,
               "--network", "none", "--entrypoint", "/bin/sh",
               "--mount", f"type=volume,src={source},dst=/source,readonly",
               "--mount", f"type=volume,src={destination},dst=/target",
               self.candidate, "-ec", "cp -a /source/. /target/")

    def check_legacy_apt_admission(self, api):
        health = api.request("GET", "/api/installation/health")
        host = next((host for host in health["restore_apt_hosts"] if host["name"] == LEGACY_APT_SERVER), None)
        require(host and host["state"] == "legacy_target_unverified" and host["legacy_job_id"] == LEGACY_APT_JOB,
                "candidate did not present the legacy APT incident")
        result = api.request("POST", f"/api/autoremove/{LEGACY_APT_SERVER}", {}, allowed=(409,))
        require(result["reason_code"] == "restore_apt_validation_required", "legacy APT incident did not block admission")
        return host

    def resolve_legacy_apt_incident(self, api, credentials, host):
        api.request("POST", "/api/auth/reauthenticate", {"password": credentials["password"]})
        result = until(lambda: self.probe_legacy_apt_incident(api, host),
                       "read-only qualification could not resolve the legacy APT incident", seconds=30)
        require(result["state"] == "approved" and "legacy_job_id" not in result, "legacy resolution did not authorize the verified target")

    def probe_legacy_apt_incident(self, api, host):
        result = api.request("POST", "/api/backup/restore-isolation/apt-check", {
            "name": host["name"], "identity": host["identity"],
            "target_revision": host["target_revision"], "confirm_legacy_job_id": LEGACY_APT_JOB,
        }, timeout=38)
        return result if result["state"] != "unreachable" else None

    def qualify_legacy_apt_upgrade(self, published_name, published_api, published_volume, credentials):
        # The resolved probe adds a protected endpoint. Keep it out of the
        # baseline volume used for the separate in-place restore scenario.
        legacy_volume = self.create_volume("legacy-apt-data")
        docker("start", published_name)
        self.connect(published_name, published_api)
        published_api.request("POST", "/api/auth/login", credentials)
        self.add_legacy_apt_target(published_api, published_name)
        docker("stop", "--timeout", "25", published_name)
        self.edit_fixture_database(published_name, "published-data", "seed-legacy-apt-incident", """
import sqlite3
import sys

with sqlite3.connect('/data/servers.db') as db:
    if 'apt_target' in {column[1] for column in db.execute('PRAGMA table_info(jobs)')}:
        raise ValueError('fixture requires a published pre-target database')
    db.execute('INSERT INTO jobs(id,kind,server_name,actor,status,phase,error_class,created_at,updated_at) '
               'VALUES(?,?,?,?,?,?,?,?,?)',
               (sys.argv[1], 'update', sys.argv[2], 'qualification-admin', 'failed', 'complete', 'reconciliation_required',
                '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z'))
""", LEGACY_APT_JOB, LEGACY_APT_SERVER)
        self.copy_upgrade_volume(published_volume, legacy_volume, "copy-legacy-apt")
        name = self.prefix + "-legacy-apt-upgraded"
        api = self.start_app(name, self.candidate, legacy_volume)
        api.request("POST", "/api/auth/login", credentials)
        self.check_legacy_apt_admission(api)
        docker("restart", "--timeout", "25", name)
        self.connect(name, api)
        api.request("POST", "/api/auth/login", credentials)
        host = self.check_legacy_apt_admission(api)
        self.resolve_legacy_apt_incident(api, credentials, host)
        health = api.request("GET", "/api/installation/health")
        validated = next((host for host in health["restore_apt_hosts"] if host["name"] == LEGACY_APT_SERVER), None)
        require(validated and validated["state"] == "approved" and "legacy_job_id" not in validated,
                "candidate did not retain the legacy APT resolution")
        self.report["docker_upgrade"]["legacy_apt_incident_migration"] = "passed"

    def docker_upgrade(self):
        published = PIN["published_image"]
        require(re.fullmatch(r"ghcr\.io/nolife141-teams/simplelinuxupdater@sha256:[0-9a-f]{64}", published),
                "published image must be pinned by digest")
        docker("pull", "--platform", PLATFORM, published, timeout=150)
        docker("network", "create", "--internal", "--label", self.label, self.network)
        old_volume, new_volume, restored_volume = (
            self.create_volume(suffix) for suffix in ("published-data", "candidate-data", "restored-data")
        )

        credentials = {"username": "qualification-admin", "password": secrets.token_urlsafe(28) + "Aa1!"}
        phrase = secrets.token_urlsafe(32)
        ssh_password = secrets.token_urlsafe(32)
        old_name = self.prefix + "-published"
        old = self.start_app(old_name, published, old_volume)
        old.request("POST", "/api/auth/setup", credentials)
        old.request("POST", "/api/servers", {"name": "upgrade-fixture", "host": "127.0.0.1", "port": 9,
            "user": "fixture", "pass": ssh_password, "disabled": True, "tags": ["qualification"]}, allowed=(201,))
        old.request("POST", "/api/update-policies", {"name": "Upgrade fixture", "enabled": False,
            "target_servers": ["upgrade-fixture"], "execution_mode": "scan_only", "package_scope": "full",
            "cadence_kind": "daily", "time_local": "03:00", "rollout_mode": "immediate"}, allowed=(201,))
        known_host = f"{UPGRADE_HOST_TOKEN} {fixture_ssh_key()}"
        docker("exec", "--user", "app", old_name, "/bin/sh", "-ec",
               'umask 077; printf "%s\\n" "$1" >> /data/known_hosts', "seed-known-host", known_host)
        self.verify_inventory(old, old_name, known_host)
        archive = old.request("POST", "/api/backup/export", {"passphrase": phrase, "include_known_hosts": True}, binary=True)
        require(len(archive) > 100 and ssh_password.encode() not in archive, "missing or unencrypted backup archive")
        backup = PublishedBackup(published, credentials, archive, phrase, known_host)
        docker("stop", "--timeout", "25", old_name)
        self.copy_upgrade_volume(old_volume, new_volume, "copy")

        new_name = self.prefix + "-upgraded"
        new = self.start_app(new_name, self.candidate, new_volume)
        new.request("POST", "/api/auth/login", credentials)
        self.verify_inventory(new, new_name, known_host)
        docker("restart", "--timeout", "25", new_name)
        self.connect(new_name, new)
        new.request("POST", "/api/auth/login", credentials)
        self.verify_inventory(new, new_name, known_host)
        self.report["docker_upgrade"]["copied_volume_restart"] = "passed"

        self.restore_archive(self.prefix + "-restored", self.candidate, restored_volume, backup, "recovery-admin")
        self.report["docker_upgrade"]["separate_volume_restore"] = "passed"
        docker("pull", "--platform", PLATFORM, PYTHON_IMAGE, timeout=150)
        self.qualify_legacy_apt_upgrade(old_name, old, old_volume, credentials)
        return new_name, new, backup

    def start_batch_probe(self, name):
        # The isolated peer only accepts and holds TCP connections. It does
        # not proceed with SSH or execute commands supplied over the connection.
        docker("exec", "--user", "app", name, "/bin/sh", "-ec",
               'umask 077; printf "%s\n" "$1" >> /data/known_hosts; printf "%s\n" "$2" > /tmp/slu-qualification-batch-probe',
               "probe", f"[127.0.0.1]:{PROBE_PORT} {fixture_ssh_key()}",
               f'printf "connected\\n" >> {PROBE_CONTACTS}\nexec /bin/sleep 60')
        docker("exec", "--detach", "--user", "app", name, "/bin/busybox", "nc", "-lk",
               "-s", "127.0.0.1", "-p", str(PROBE_PORT), "-e", "/bin/sh", "/tmp/slu-qualification-batch-probe")
        until(lambda: f"127.0.0.1:{PROBE_PORT} " in docker("exec", name, "/bin/busybox", "netstat", "-ltn"),
              "qualification TCP probe did not start")

    def probe_contacts(self, name, path):
        return int(docker("exec", name, "/bin/sh", "-ec",
                          'if test -f "$1"; then wc -l < "$1"; else printf "0"; fi', "probe-count", path))

    def batch_update_admissions(self, api):
        return api.request("GET", f"/api/audit-events?action=update.start&status=started&target_type=server&target_name={BATCH_SERVER}",
                           timeout=4)

    def qualify_maintenance_batches(self, name, api, credentials):
        evidence = self.report["maintenance_batches"]
        api.request("POST", "/api/servers", {
            "name": BATCH_SERVER, "host": "127.0.0.1", "port": PROBE_PORT,
            "user": "fixture", "pass": secrets.token_urlsafe(24), "disabled": True,
        }, allowed=(201,))
        server = next(item for item in api.request("GET", "/api/servers") if item["name"] == BATCH_SERVER)
        identity = server["server_identity"]
        require(bool(identity), "candidate did not assign the batch fixture a stable identity")
        batch_id = uuid.uuid4().hex
        source = {"id": batch_id, "action": "update", "targets": [{"name": BATCH_SERVER, "server_identity": identity}]}
        require(api.request("POST", BATCH_PATH, source, allowed=(202,)) == {"id": batch_id},
                "candidate did not accept the source batch")

        def read_host(batch):
            hosts = api.request("GET", f"{BATCH_PATH}/{batch}", timeout=4)["batch"]["hosts"]
            require(len(hosts) == 1 and hosts[0]["server_identity"] == identity,
                    "maintenance batch host identity changed")
            return hosts[0]

        def refused_host():
            host = read_host(batch_id)
            return host if host["state"] == "refused" and host.get("retryable") else None

        expected = until(refused_host, "disabled fixture did not produce a retryable refusal")
        docker("restart", "--timeout", "25", name)
        self.connect(name, api)
        api.request("POST", "/api/auth/login", credentials)
        require(read_host(batch_id) == expected, "source batch result changed after restart")
        history = api.request("GET", BATCH_PATH)["batches"]
        require(any(row["id"] == batch_id and row["terminal"] == 1 and row["needs_verification"] == 0 for row in history),
                "owner history lost the source batch after restart")
        evidence["history_after_restart"] = "passed"

        operator = {"username": "qualification-operator", "password": secrets.token_urlsafe(28) + "Aa1!"}
        api.request("POST", "/api/auth/accounts", {**operator, "role": "operator"}, allowed=(201,))
        stranger = self.connect(name)
        stranger.request("POST", "/api/auth/login", operator)
        require(stranger.request("GET", BATCH_PATH)["batches"] == [], "owner batch appeared in another operator's history")
        require(stranger.request("GET", f"{BATCH_PATH}/{batch_id}", allowed=(404,)).get("batch") is None,
                "another operator could access the source batch")
        evidence["account_isolation"] = "passed"

        self.start_batch_probe(name)
        api.request("PUT", f"/api/servers/{BATCH_SERVER}/availability", {"disabled": False})
        second_session = self.connect(name)
        second_session.request("POST", "/api/auth/login", credentials)
        retries = [{**source, "id": uuid.uuid4().hex, "retry_of": batch_id} for _ in range(2)]
        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(client.request, "POST", BATCH_PATH, retry, allowed=(202, 409))
                       for client, retry in zip((api, second_session), retries)]
            responses = [future.result() for future in futures]
        winners = [retry for retry, response in zip(retries, responses) if response == {"id": retry["id"]}]
        require(len(winners) == 1, "concurrent retry did not admit exactly one batch")
        winner = winners[0]
        winner_id = winner["id"]
        loser = next(response for retry, response in zip(retries, responses) if retry != winner)
        require(loser.get("retry_batch_id") == winner_id, "conflicting retry did not identify the admitted attempt")
        require(api.request("POST", BATCH_PATH, winner, allowed=(202,)) == {"id": winner_id},
                "replaying the admitted request did not return the same batch")
        require(read_host(batch_id).get("retry_batch_id") == winner_id, "source lost its admitted retry link")

        def running_job():
            host = read_host(winner_id)
            return host.get("job_id") if host.get("job_status") == "running" else None

        job_id = until(running_job, "retry did not start a real job")

        def recorded_admissions():
            result = self.batch_update_admissions(api)
            return result if result["total"] else None

        # The runner may reach the probe before the admission audit is written.
        admissions = until(recorded_admissions, "retry job admission did not appear in the audit")
        require(admissions["total"] == 1 and json.loads(admissions["items"][0]["meta_json"])["job_id"] == job_id,
                "concurrent and replayed retries must record exactly one admission for the running job")
        until(lambda: self.probe_contacts(name, PROBE_CONTACTS), "retry job did not reach the isolated peer")
        require(self.probe_contacts(name, PROBE_CONTACTS) == 1,
                "the isolated peer received more than one connection before interruption")
        evidence["single_retry_admission"] = "passed"

        require(read_host(winner_id).get("job_status") == "running", "retry completed before the interruption checkpoint")
        docker("kill", name)
        docker("start", name)
        self.connect(name, api)
        api.request("POST", "/api/auth/login", credentials)
        interrupted = read_host(winner_id)
        require(interrupted.get("job_id") == job_id and interrupted.get("job_status") == "interrupted" and
                interrupted["state"] == "completed" and not interrupted.get("retryable"),
                "restart lost the admitted job or its interrupted result")
        history = api.request("GET", BATCH_PATH)["batches"]
        require(any(row["id"] == winner_id and row["terminal"] == 1 and row["needs_verification"] == 0 for row in history),
                "owner history lost the interrupted retry")
        evidence["interrupted_job_recovery"] = "passed"
        return MaintenanceBatchFixture(source, winner, {batch_id: read_host(batch_id), winner_id: interrupted}, operator)

    def edit_fixture_database(self, name, volume_suffix, task, script, *args):
        volume = self.prefix + "-" + volume_suffix
        mount = docker("container", "inspect", "--format",
                       '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}', name)
        owner = docker("volume", "inspect", "--format", '{{index .Labels "' + LABEL + '"}}', volume)
        require(mount == volume and owner == self.prefix, "refusing to edit a volume owned by another run")
        docker("run", "--pull", "never", "--rm", "--label", self.label, "--name", self.prefix + "-" + task,
               "--network", "none", "--mount", f"type=volume,src={volume},dst=/data", PYTHON_IMAGE,
               "python3", "-c", script, *args)

    def seed_pending_deliveries(self, name, job_id):
        # The ordinary event and the superseded plan are fixture inputs. The
        # candidate still performs every export, restore and delivery itself.
        ordinary_id, stale_id = (secrets.token_hex(32) for _ in range(2))
        docker("stop", "--timeout", "25", name)
        script = """
import hashlib
import json
import sqlite3
import sys
from datetime import UTC, datetime

job_id, expected_server, url, ordinary_id, stale_id = sys.argv[1:]
fingerprint = hashlib.sha256(("webhook\\0" + url).encode()).hexdigest()
with sqlite3.connect("file:/data/servers.db?mode=rw", uri=True) as db:
    job = db.execute("SELECT j.status, j.server_name, s.identity FROM jobs j "
                     "JOIN servers s ON s.name=j.server_name WHERE j.id=?", (job_id,)).fetchone()
    if job is None or job[:2] != ("interrupted", expected_server) or not job[2]:
        raise ValueError("pending deliveries require the terminal batch fixture")
    server_identity = job[2]
    created = datetime.now(UTC).isoformat().replace("+00:00", "Z")
    condition_id = "approval/" + job_id
    plan_meta = {"job_id": job_id, "approval_generation": 1}
    for delivery_id, event, metadata in (
        (ordinary_id, "update.complete", {"job_id": job_id}),
        (stale_id, "approval.published", {**plan_meta, "condition_id": condition_id, "condition_episode": 1}),
    ):
        db.execute(
            "INSERT INTO notification_outbox (id, destination, event_type, action, target_type, target_name, "
            "event_created_at, meta_json, destination_fingerprint, state, next_attempt_at, created_at, updated_at) "
            "VALUES (?, 'webhook', ?, ?, 'server', ?, ?, ?, ?, 'pending', '9999-01-01T00:00:00Z', ?, ?)",
            (delivery_id, event, event, expected_server, created, json.dumps(metadata), fingerprint, created, created),
        )
    # Model a previously admitted approval whose job terminated before
    # the condition monitor observed the change.
    prior = db.execute("SELECT state_json FROM notification_conditions WHERE id=1").fetchone()
    state = json.loads(prior[0]) if prior else {"Conditions": {}, "Admissions": {}, "Reminders": {}}
    state["Conditions"][condition_id] = {
        "Fact": {
            "ID": condition_id, "EventType": "approval.published", "TargetType": "server",
            "TargetName": expected_server, "ServerIdentity": server_identity, "Generation": "1",
            "Active": True, "Message": "Previous fixture approval", "MetaJSON": json.dumps(plan_meta),
            "CreatedAt": created,
        },
        "Episode": 1, "UpdatedAt": created,
    }
    state.setdefault("ApprovalAdmissions", {})[condition_id] = [{
        "ServerIdentity": server_identity, "Generation": "1", "Fingerprint": fingerprint, "OutboxID": stale_id,
    }]
    db.execute("INSERT INTO notification_conditions(id,state_json) VALUES(1,?) "
               "ON CONFLICT(id) DO UPDATE SET state_json=excluded.state_json", (json.dumps(state),))
db.close()
"""
        self.edit_fixture_database(name, "candidate-data", "outbox-fixture", script,
                                   job_id, BATCH_SERVER, WEBHOOK_URL, ordinary_id, stale_id)
        docker("start", name)
        return ordinary_id, stale_id

    def release_fixture_delivery(self, name, volume_suffix, delivery_id):
        # Only release the fixture's due time. The application's periodic
        # condition monitor wakes delivery and still records its own outcome.
        script = """
import sqlite3
import sys

with sqlite3.connect("file:/data/servers.db?mode=rw", uri=True, timeout=15) as db:
    db.execute("UPDATE notification_outbox SET next_attempt_at='' "
               "WHERE id=? AND state='pending' AND next_attempt_at='9999-01-01T00:00:00Z'", (sys.argv[1],))
db.close()
"""
        self.edit_fixture_database(name, volume_suffix, "outbox-release", script, delivery_id)

    def start_webhook_receiver(self, app_name, suffix):
        # Each receiver shares only its app's network namespace and keeps
        # request counts without persisting or logging body contents.
        script = """
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

port, count_path = sys.argv[1:]

class Receiver(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/receive":
            self.send_error(404)
            return
        self.rfile.read(int(self.headers["Content-Length"]))
        with open(count_path, "a") as count:
            count.write("received\\n")
        self.send_response(204)
        self.end_headers()

    def log_message(self, *_):
        pass

HTTPServer(("127.0.0.1", int(port)), Receiver).serve_forever()
"""
        receiver = self.prefix + "-webhook-" + suffix
        docker("run", "--pull", "never", "--detach", "--label", self.label, "--name", receiver,
               "--network", "container:" + app_name, PYTHON_IMAGE, "python3", "-c", script, str(WEBHOOK_PORT), WEBHOOK_CONTACTS)
        until(lambda: f"127.0.0.1:{WEBHOOK_PORT} " in docker("exec", app_name, "/bin/busybox", "netstat", "-ltn"),
              "qualification webhook receiver did not start")
        return receiver

    def verify_restored_batches(self, name, api, fixture):
        for request in (fixture.source, fixture.retry):
            batch = api.request("GET", f"{BATCH_PATH}/{request['id']}")["batch"]
            require(batch["id"] == request["id"] and batch.get("retry_of") == request.get("retry_of")
                    and batch["hosts"] == [fixture.terminal_hosts[request["id"]]],
                    "restore changed the batch's terminal result or retry lineage")
        history = api.request("GET", BATCH_PATH)["batches"]
        require({item["id"] for item in history if item["terminal"] == 1 and item["needs_verification"] == 0}
                == {fixture.source["id"], fixture.retry["id"]}, "restore lost terminal batches from owner history")
        job_id = fixture.terminal_hosts[fixture.retry["id"]]["job_id"]
        job = api.request("GET", f"/api/jobs/{job_id}")["job"]
        require(job["id"] == job_id and job["status"] == "interrupted" and job["server_name"] == BATCH_SERVER,
                "restore lost the linked job")
        other = self.connect(name)
        other.request("POST", "/api/auth/login", fixture.other_account)
        require(other.request("GET", BATCH_PATH)["batches"] == [], "restored batches appeared in another account's history")
        for request in (fixture.source, fixture.retry):
            require(other.request("GET", f"{BATCH_PATH}/{request['id']}", allowed=(404,)).get("batch") is None,
                    "another account could access a restored batch")

    def notification_delivery(self, api, job_id, event):
        items = api.request("GET", f"/api/notifications/delivery-history?destination=webhook&job_id={job_id}")["items"]
        matching = [item for item in items if item["event_type"] == event]
        require(len(matching) == 1, "expected exactly one seeded delivery in candidate history")
        return matching[0]

    def qualify_candidate_backup_restore(self, name, api, credentials, fixture):
        evidence = self.report["candidate_backup_restore"]
        isolated_name = self.prefix + "-candidate-restore"
        isolated_suffix = "candidate-restore-data"
        isolated = self.initialize_restore_target(isolated_name, self.candidate, self.create_volume(isolated_suffix),
                                                  "candidate-recovery-admin")
        require(re.fullmatch(r"python:3\.12\.\d+-slim-bookworm@sha256:[0-9a-f]{64}", PYTHON_IMAGE),
                "qualification helpers must use the pinned Python runtime")
        docker("pull", "--platform", PLATFORM, PYTHON_IMAGE, timeout=150)

        settings = api.request("PUT", "/api/notifications/settings", {
            "enabled": True, "webhook_url_intent": "replace", "webhook_url": WEBHOOK_URL,
            "event_types": ["update.complete", "approval.published"],
        })
        require(settings["enabled"] and settings["webhook_configured"], "candidate did not configure the isolated destination")
        job_id = fixture.terminal_hosts[fixture.retry["id"]]["job_id"]
        ordinary_id, stale_id = self.seed_pending_deliveries(name, job_id)
        self.connect(name, api)
        api.request("POST", "/api/auth/login", credentials)
        source_receiver = self.start_webhook_receiver(name, "source")
        ordinary = self.notification_delivery(api, job_id, "update.complete")
        stale = self.notification_delivery(api, job_id, "approval.published")
        require(ordinary["state"] == stale["state"] == "pending" and stale["approval_generation"] == 1,
                "candidate did not have both pending deliveries before export")
        phrase = secrets.token_urlsafe(32)
        evidence["archive_export"] = "failed"
        archive = api.request("POST", "/api/backup/export", {"passphrase": phrase, "include_known_hosts": True}, binary=True)
        require(len(archive) > 100 and self.probe_contacts(source_receiver, WEBHOOK_CONTACTS) == 0,
                "candidate failed to export before delivering the pending notification")
        evidence["archive_export"] = "passed"

        evidence["isolated_batch_history"] = "failed"
        self.apply_archive(isolated, archive, phrase, credentials)
        require(self.notification_delivery(isolated, job_id, "update.complete") == ordinary
                and self.notification_delivery(isolated, job_id, "approval.published") == stale,
                "isolated restore lost the original pending delivery evidence")
        self.verify_restored_batches(isolated_name, isolated, fixture)
        docker("restart", "--timeout", "25", isolated_name)
        self.connect(isolated_name, isolated)
        isolated.request("POST", "/api/auth/login", credentials)
        isolated_receiver = self.start_webhook_receiver(isolated_name, "restored")
        self.verify_restored_batches(isolated_name, isolated, fixture)
        evidence["isolated_batch_history"] = "passed"

        self.start_batch_probe(isolated_name)
        evidence["isolated_retry_admission"] = "failed"
        blocked = isolated.request("POST", BATCH_PATH, fixture.retry, allowed=(409,))
        require(blocked.get("code") == "restore_isolated", "restore isolation did not block the batch route")
        isolated.request("POST", "/api/auth/reauthenticate", {"password": credentials["password"]})
        preview = isolated.request("GET", "/api/backup/restore-isolation/resume-preview")
        require(isolated.request("POST", "/api/backup/restore-isolation/resume",
                                 {"confirmation": "RESUME", "preview_token": preview["token"]})["restore_isolation"] == "inactive",
                "recovery did not resume external work")
        # This peer only counts connections. It cannot produce an APT proof.
        # Verify that replaying history works while fresh package work remains gated.
        blocked = isolated.request("POST", f"/api/update/{BATCH_SERVER}", allowed=(409,))
        require(blocked.get("reason_code") == "restore_apt_validation_required",
                "resuming external work released the unchecked host")
        winner_id = fixture.retry["id"]
        require(isolated.request("POST", BATCH_PATH, fixture.retry, allowed=(202,)) == {"id": winner_id},
                "restored retry replay did not return the original attempt")
        another_retry = {**fixture.retry, "id": uuid.uuid4().hex}
        conflict = isolated.request("POST", BATCH_PATH, another_retry, allowed=(409,))
        require(conflict.get("retry_batch_id") == winner_id, "a distinct request did not identify the existing retry")
        require(isolated.request("GET", f"{BATCH_PATH}/{another_retry['id']}", allowed=(404,)).get("batch") is None,
                "the rejected retry produced an unexpected batch")
        self.verify_restored_batches(isolated_name, isolated, fixture)
        audit = self.batch_update_admissions(isolated)
        require(audit["total"] == 1 and json.loads(audit["items"][0]["meta_json"])["job_id"] == job_id
                and self.probe_contacts(isolated_name, PROBE_CONTACTS) == 0,
                "restored retry submitted another job or contacted the SSH probe")
        evidence["isolated_retry_admission"] = "passed"

        def terminal_delivery(client, event, state):
            result = self.notification_delivery(client, job_id, event)
            return result if result["state"] == state else None

        evidence["live_terminal_delivery"] = "failed"
        self.release_fixture_delivery(name, "candidate-data", ordinary_id)
        delivered = until(lambda: terminal_delivery(api, "update.complete", "succeeded"),
                          "source failed to deliver the archived pending notification")
        require(delivered["attempts"] == 1 and self.probe_contacts(source_receiver, WEBHOOK_CONTACTS) == 1
                and self.notification_delivery(api, job_id, "approval.published") == stale,
                "source did not deliver exactly once with the obsolete plan still pending")
        self.apply_archive(api, archive, phrase, credentials)
        require(self.notification_delivery(api, job_id, "update.complete") == delivered,
                "in-place restore lost the live terminal delivery outcome")
        self.verify_restored_batches(name, api, fixture)

        self.release_fixture_delivery(isolated_name, isolated_suffix, ordinary_id)
        until(lambda: terminal_delivery(isolated, "update.complete", "succeeded"),
              "isolated restore did not deliver the archived pending notification")
        require(self.notification_delivery(api, job_id, "update.complete") == delivered
                and self.probe_contacts(source_receiver, WEBHOOK_CONTACTS) == 1,
                "in-place restore replayed the delivered notification")
        require(self.probe_contacts(isolated_receiver, WEBHOOK_CONTACTS) == 1,
                "isolated restore did not deliver the archived notification exactly once")
        evidence["live_terminal_delivery"] = "passed"

        evidence["obsolete_approval"] = "failed"
        self.release_fixture_delivery(name, "candidate-data", stale_id)
        self.release_fixture_delivery(isolated_name, isolated_suffix, stale_id)
        for client in (api, isolated):
            obsoleted = until(lambda: terminal_delivery(client, "approval.published", "skipped"),
                              "restored obsolete approval did not become ineligible")
            require(obsoleted.get("reason_code") == "condition_ineligible"
                    and obsoleted["approval_generation"] == stale["approval_generation"] and obsoleted["attempts"] == 0,
                    "restored obsolete approval was not skipped before delivery")
        require(self.probe_contacts(source_receiver, WEBHOOK_CONTACTS) == 1
                and self.probe_contacts(isolated_receiver, WEBHOOK_CONTACTS) == 1,
                "restored obsolete approval contacted a receiver")
        evidence["obsolete_approval"] = "passed"

    def controlled_reboot(self):
        source = self.work / "vm-source"
        source.mkdir()
        with tarfile.open(self.work / "source.tar") as archive:
            archive.extractall(source, filter="data")
        build = self.work / "vm-runner"
        build.mkdir()
        # The release gate runs from its preserved trusted tools directory.
        runner_sources = HERE.parent / "ssh-apt"
        for name in ("vm-runner.Dockerfile", "vm-reboot.py", "run.py", "images.json"):
            shutil.copyfile(runner_sources / name, build / name)
        docker("build", "--platform", PLATFORM, "--label", self.label, "--tag", self.vm_runner,
               "--file", build / "vm-runner.Dockerfile", build, timeout=240)

        guest = self.work / "guest.img"
        command("curl", "--fail", "--silent", "--show-error", "--location", "--retry", "2", "--max-time", "180",
                "--max-filesize", str(1024**3), "--output", guest, PIN["vm_image_url"], timeout=240)
        guest.chmod(0o644)
        asset_container = self.prefix + "-assets"
        docker("create", "--pull", "never", "--label", self.label, "--name", asset_container, "--network", "none", self.candidate)
        app = self.work / "app"
        app.mkdir(mode=0o755)
        with container_archive(f"{asset_container}:/app/.") as archive:
            archive.extractall(app, filter="data")
        command("env", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "go", "-C", source,
                "test", "-c", "-o", app / "restore-power-loss.test", "./internal/backup", timeout=240)
        name = self.prefix + "-reboot"
        docker("run", "--pull", "never", "--detach", "--label", self.label, "--name", name,
               "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
               "--mount", f"type=bind,src={app},dst=/candidate,readonly",
               "--mount", f"type=bind,src={guest},dst=/guest.img,readonly",
               self.vm_runner, "--isolated-app-directory", "/candidate", "--image", "/guest.img",
               "--sha256", PIN["vm_image_sha256"], "--output", "/tmp/report", "--qualify-restore-power-loss")
        self.collect_vm_evidence(name, docker("wait", name, timeout=660))

    def collect_vm_evidence(self, name, exit_code):
        with container_archive(f"{name}:/tmp/report/report.json") as archive:
            member = archive.getmember("report.json")
            require(member.isfile() and member.size <= 64 * 1024, "invalid VM evidence file")
            with archive.extractfile(member) as data:
                proof = json.load(data)
        if exit_code == "2" and proof.get("status") == "not_executed":
            status = "not_executed"
        elif (exit_code == "0" and proof.get("status") == "passed"
              and proof.get("cleanup", {}).get("status") == "passed"
              and proof.get("image_sha256") == PIN["vm_image_sha256"]
              and proof.get("restore_power_loss") == {
                  phase: {"status": "passed", "checkpoint_observed": True,
                          "disk_verified": True, "recovery_required": phase != "before"}
                  for phase in ("before", "forward", "rollback")
              }):
            status = "passed"
        else:
            status = "failed"
        proof["status"] = status
        (self.output / "vm-report.json").write_text(json.dumps(proof, indent=2) + "\n")
        if status != "passed":
            self.report["controlled_reboot_vm"]["status"] = status
            raise RuntimeError(f"VM qualification exited {exit_code}; see vm-report.json")
        self.report["controlled_reboot_vm"]["guest_sha256"] = proof["image_sha256"]
        self.report["controlled_reboot_vm"]["restore_power_loss"] = "passed"

    def cleanup(self):
        errors = []
        for kind, flags in (("container", ("-a",)), ("volume", ()), ("network", ())):
            try:
                resources = docker(kind, "ls", *flags, "--quiet", "--filter", "label=" + self.label).splitlines()
            except Exception as error:
                errors.append(str(error))
                continue
            for resource in resources:
                try:
                    options = ("--force", "--volumes") if kind == "container" else ()
                    docker(kind, "rm", *options, resource)
                except Exception as error:
                    errors.append(str(error))
            try:
                if docker(kind, "ls", *flags, "--quiet", "--filter", "label=" + self.label):
                    errors.append(f"Owned qualification {kind} resources remain")
            except Exception as error:
                errors.append(str(error))
        try:
            images = docker("image", "ls", "--filter", "label=" + self.label, "--format", "{{.Repository}}:{{.Tag}}").splitlines()
            for image in images:
                if image in (self.candidate, self.vm_runner, self.apt_fixture_image):
                    docker("image", "rm", image)
        except Exception as error:
            errors.append(str(error))
        self.report["cleanup"] = {"status": "failed" if errors else "passed", "errors": errors}
        return not errors

    def run(self):
        missing = [name for name in ("git", "docker", "curl") if not shutil.which(name)]
        if missing:
            self.report["reason"] = "Missing qualification tools: " + ", ".join(missing)
            return 2
        try:
            platform = docker("version", "--format", "{{.Server.Os}}/{{.Server.Arch}}", timeout=15)
        except (OSError, RuntimeError):
            self.report["reason"] = "Local Docker daemon is unavailable"
            return 2
        if platform != PLATFORM:
            self.report["reason"] = f"Qualification requires a {PLATFORM} Docker runner"
            return 2
        self.report["status"] = "failed"
        try:
            self.step("candidate_build", self.build_candidate)
            name, api, backup = self.step("docker_upgrade", self.docker_upgrade)
            self.step("docker_rollback", lambda: self.docker_rollback(backup))
            fixture = self.step("maintenance_batches", lambda: self.qualify_maintenance_batches(name, api, backup.credentials))
            self.step("candidate_backup_restore", lambda: self.qualify_candidate_backup_restore(name, api, backup.credentials, fixture))
            self.step("controlled_reboot_vm", self.controlled_reboot)
            self.report["status"] = "passed"
        except (Exception, KeyboardInterrupt) as error:
            self.report["reason"] = str(error) or type(error).__name__
        finally:
            if not self.cleanup():
                self.report["status"] = "failed"
        return 0 if self.report["status"] == "passed" else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--candidate-source", required=True, type=Path)
    parser.add_argument("--candidate-sha", required=True)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9a-f]{40}", args.candidate_sha):
        parser.error("--candidate-sha requires a full commit SHA")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    (output / "vm-report.json").unlink(missing_ok=True)
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    work = tempfile.TemporaryDirectory(prefix="slu-operational-qualification-")
    qualification = Qualification(Path(work.name), output, args.candidate_source.resolve(), args.candidate_sha)
    report = qualification.report
    try:
        code = qualification.run()
    finally:
        try:
            work.cleanup()
        except OSError:
            code = 1
            report["status"] = "failed"
            report["cleanup"]["status"] = "failed"
            report["cleanup"].setdefault("errors", []).append("Could not remove the private qualification directory")
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        print(report["status"] + (": " + report["reason"] if "reason" in report else ""), flush=True)
    return code


if __name__ == "__main__":
    raise SystemExit(main())
