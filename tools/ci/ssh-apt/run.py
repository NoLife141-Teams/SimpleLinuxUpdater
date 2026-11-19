#!/usr/bin/env python3
"""Real API/SSH/APT qualification. Never accepts existing targets or inventories."""
import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing, contextmanager
import http.client
import http.cookiejar
import ipaddress
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
IMAGES = json.loads((HERE / "images.json").read_text())
LABEL = "io.simplelinuxupdater.ssh-apt-owner"
DOCKER = ["docker", "--host=unix:///var/run/docker.sock"]
PACKAGE = "slu-ci-fixture"
TERMINAL = {"succeeded", "failed", "cancelled", "interrupted"}
APT_EVENT = "apt.reconciliation.required"
RESTORE_CRASH = "restore-crash-after-sqlite-replacement"
SCENARIOS = ("root-upgrade", "operator-upgrade", "misconfigured-helper", "apt-lock",
             "plan-drift-and-obsolete-approval", "ssh-disconnect-during-mutation-and-repair",
             "batch-app-interruption-during-mutation-and-repair", "changed-host-key",
             RESTORE_CRASH, "systemd-active", "controlled-reboot", "official-provenance")


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def until(predicate, message, seconds=30):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = predicate()
        if result:
            return result
        time.sleep(.1)
    raise AssertionError(message)


def qualification_overlay(work):
    """Instrument only the disposable binary built by this qualification."""
    replacements = {}

    def instrument(relative, edits):
        original = REPO / relative
        source = original.read_text()
        for anchor, replacement in edits:
            require(source.count(anchor) == 1, "qualification checkpoint moved in " + relative)
            source = source.replace(anchor, replacement)
        copy = work / original.name
        copy.write_text(source)
        replacements[str(original)] = str(copy)

    # This marker can only be reached after applying the staged database and
    # removing SQLite sidecars, before host keys, handoff or runtime reload.
    anchor = '\tif knownHosts, ok := files["known_hosts"]; ok {\n\t\tif err := writeAtomicFileFromPath'
    instrument("internal/backup/file_restore.go", [
        ('\n\t"strings"\n', '\n\t"strings"\n\t"time"\n'),
        (anchor, """\
\tif os.Remove("/work/arm-restore-crash") == nil {
\t\tif err := os.WriteFile("/work/restore-db-replaced", nil, 0600); err != nil {
\t\t\treturn rollback(err)
\t\t}
\t\tfor {
\t\t\ttime.Sleep(time.Second)
\t\t}
\t}
""" + anchor),
    ])

    # Refresh the proof after condition reconciliation and outbox draining.
    # Record which destination was eligible, since an idle worker alone cannot
    # establish that the restored configuration still allows this delivery.
    anchor = '\t\tif s.conditionMonitoringStarted() {\n\t\t\tif conditionTicker == nil {'
    failure = '\t\t\t\ts.deps.Logf("notification condition reconciliation failed: %v", err)\n\t\t\t}'
    finish = '\t\ts.health.Finish(s.deps.Now(), nil)\n\t\tif !scheduled {'
    instrument("internal/notifications/service.go", [
        ('\n\t"net/url"\n', '\n\t"net/url"\n\t"os"\n'),
        (anchor, '\t\tqualificationReconciled := false\n' + anchor),
        (failure, failure + ' else {\n\t\t\t\tqualificationReconciled = true\n\t\t\t}'),
        (finish, """\
\t\tif qualificationReconciled {
\t\t\tif _, err := os.Stat("/work/observe-notifications"); err == nil {
\t\t\t\tfingerprint := ""
\t\t\t\tif settings, err := s.currentSettings(); err == nil && destinationEnabled(settings, DestinationWebhook) &&
\t\t\t\t\teventEnabled(settings.EventTypes, EventAPTReconciliationRequired) {
\t\t\t\t\tfingerprint = destinationConfigFingerprint(settings, DestinationWebhook)
\t\t\t\t}
\t\t\t\tconst marker = "/work/notification-poll-complete"
\t\t\t\tif err := os.WriteFile(marker+".tmp", []byte(fingerprint), 0600); err == nil {
\t\t\t\t\t_ = os.Rename(marker+".tmp", marker)
\t\t\t\t}
\t\t\t}
\t\t}
""" + finish),
    ])
    overlay = work / "qualification-overlay.json"
    overlay.write_text(json.dumps({"Replace": replacements}))
    return overlay


def command(args, *, timeout=60, data=None, env=None, cwd=None):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            timeout=timeout, env=env, cwd=cwd)
    if result.returncode:
        raise RuntimeError(f"{args[0]} failed ({result.returncode}): {result.stdout.decode(errors='replace')[-5000:]}")
    return result.stdout.decode()


def loopback_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def validate_local_endpoint(host, port):
    require(host == "127.0.0.1" and isinstance(port, int) and 1024 <= port <= 65535,
            "fixture endpoints must be allocated loopback ports")


def app_environment(work, port):
    # Construct from an allowlist: never inherit deployment paths, master keys,
    # global SSH credentials, notification settings, proxy settings or .env data.
    validate_local_endpoint("127.0.0.1", port)
    return {
        "PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
        "HOME": str(work), "TMPDIR": str(work), "GIN_MODE": "release", "TZ": "UTC",
        "DEBIAN_UPDATER_LISTEN_ADDR": f"127.0.0.1:{port}",
        "DEBIAN_UPDATER_DB_PATH": str(work / "servers.db"),
        "DEBIAN_UPDATER_KNOWN_HOSTS": str(work / "known_hosts"),
        "DEBIAN_UPDATER_RETRY_MAX_ATTEMPTS": "3",
        "DEBIAN_UPDATER_RETRY_BASE_DELAY_MS": "100",
        "DEBIAN_UPDATER_RETRY_MAX_DELAY_MS": "200",
        "DEBIAN_UPDATER_RETRY_JITTER_PCT": "0",
        "DEBIAN_UPDATER_SSH_COMMAND_TIMEOUT_SECONDS": "30",
        "DEBIAN_UPDATER_POSTCHECK_BLOCK_ON_FAILED_UNITS": "false",
        "DEBIAN_UPDATER_VULNERABILITY_SCAN_ENABLED": "false",
    }


def copy_assets(work):
    """Copy tracked public assets only, never local prototypes or runtime files."""
    files = command(["git", "-C", str(REPO), "ls-files", "-z", "--", "templates/", "static/"]).split("\0")
    for relative in filter(None, files):
        require(relative.startswith(("templates/", "static/")) and ".." not in Path(relative).parts, "invalid asset path")
        destination = work / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(REPO / relative, destination)


def wait_for_app(api, *, check_running=None, path="/api/auth/status"):
    """Wait within the startup budget, including transient startup maintenance."""
    deadline = time.monotonic() + 30
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("application startup was not ready within 30 seconds")
        if check_running is not None:
            check_running(remaining)
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("application startup was not ready within 30 seconds")
        try:
            api.request("GET", path, timeout=min(20, remaining))
        except OSError:
            pass
        except AssertionError as error:
            # API.request emits this prefix from the actual HTTP status, not
            # response-body text. Permanent HTTP errors and unrelated assertions
            # remain fatal; only startup maintenance is retried.
            if not str(error).startswith(f"GET {path}: HTTP 503:"):
                raise
        else:
            if time.monotonic() >= deadline:
                raise TimeoutError("application startup exceeded 30 seconds")
            return
        time.sleep(min(.1, max(0, deadline - time.monotonic())))



class API:
    def __init__(self, port, *, include_error_body=True):
        validate_local_endpoint("127.0.0.1", port)
        self.base = f"http://127.0.0.1:{port}"
        self.include_error_body = include_error_body
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def request(self, method, path, data=None, *, allowed=(200,), raw=None, content_type=None, timeout=20, binary=False):
        require(path.startswith("/api/") and not path.startswith("//"), "invalid fixture API path")
        body = raw if raw is not None else (json.dumps(data).encode() if data is not None else None)
        request = urllib.request.Request(self.base + path, data=body, method=method,
            headers={"Origin": self.base, "Content-Type": content_type or "application/json"})
        try:
            response = self.client.open(request, timeout=timeout)
        except urllib.error.HTTPError as error:
            response = error
        if response.code not in allowed and not self.include_error_body:
            response.close()
            raise AssertionError(f"{method} {path}: HTTP {response.code}: response body omitted")
        if binary and response.code in allowed:
            with response:
                return response.read(16 * 1024 * 1024 + 1)
        try:
            result = json.loads(response.read())
            require(response.code in allowed, f"{method} {path}: HTTP {response.code}: {result}")
        finally:
            response.close()
        return result

    def upload_key(self, name, key):
        boundary = "slu-" + uuid.uuid4().hex
        body = (f'--{boundary}\r\nContent-Disposition: form-data; name="key"; filename="fixture.key"\r\n'
                'Content-Type: application/octet-stream\r\n\r\n').encode() + key + f"\r\n--{boundary}--\r\n".encode()
        return self.request("POST", f"/api/servers/{name}/key", raw=body,
                            content_type=f"multipart/form-data; boundary={boundary}")

    def server(self, name):
        return next(s for s in self.request("GET", "/api/servers") if s["name"] == name)

    def backup_archive(self, operation, archive, passphrase, *, allowed=(200,), timeout=20):
        require(operation in ("verify", "restore"), "invalid fixture archive operation")
        boundary = "slu-" + uuid.uuid4().hex
        body = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="fixture.slubkp"\r\n'
                'Content-Type: application/octet-stream\r\n\r\n').encode() + archive
        body += (f'\r\n--{boundary}\r\nContent-Disposition: form-data; name="passphrase"\r\n\r\n'
                 f'{passphrase}\r\n--{boundary}--\r\n').encode()
        return self.request("POST", "/api/backup/" + operation, raw=body,
                            content_type=f"multipart/form-data; boundary={boundary}", allowed=allowed, timeout=timeout)

    def job(self, job_id):
        return self.request("GET", f"/api/jobs/{job_id}")["job"]

    def wait_job(self, job_id, predicate, *, seconds=90):
        end = time.monotonic() + seconds
        observations = []
        while time.monotonic() < end:
            job = self.job(job_id)
            state = (job["status"], job["phase"])
            if not observations or observations[-1] != state:
                observations.append(state)
            if predicate(job):
                return job, observations
            if job["status"] in TERMINAL:
                detail = f": {job}" if self.include_error_body else ""
                raise AssertionError("job terminated before expected state" + detail)
            time.sleep(.15)
        detail = f": {self.job(job_id)}" if self.include_error_body else ""
        raise AssertionError("job timeout" + detail)


class Harness:
    def __init__(self, distro, output):
        require(distro in IMAGES, "unsupported fixture distribution")
        self.distro = distro
        self.started = time.monotonic()
        self.output = output
        self.owner = uuid.uuid4().hex
        self.name = "slu-ssh-apt-" + self.owner
        self.app_name = self.name + "-app"
        self.network = self.name + "-net"
        self.image = self.name + ":target"
        self.container = None
        self.work = Path(tempfile.mkdtemp(prefix=self.name + "-"))
        self.password = secrets.token_urlsafe(24) + "!Aa7"
        # The primary credential is always masked; this list holds extra secrets.
        self.redactions = []
        self.records = []
        self.transitions = {}
        self.cleanup_errors = []

    def docker(self, *args, **kwargs):
        return command([*DOCKER, *args], **kwargs)

    def owned(self, kind, name):
        template = '{{index .Config.Labels "' + LABEL + '"}}' if kind in ("container", "image") else '{{index .Labels "' + LABEL + '"}}'
        return self.docker(kind, "inspect", "--format", template, name).strip() == self.owner

    def target(self, *args, data=None, timeout=60):
        require(self.container is not None and self.owned("container", self.container), "target ownership mismatch")
        token = self.docker("exec", self.container, "cat", "/run/slu-fixture-owner").strip()
        require(token == self.owner, "target marker mismatch")
        return self.docker("exec", "-i", self.container, *args, data=data, timeout=timeout)

    def fixture(self, operation, *args):
        require(operation in ("reset", "publish", "drop-ssh", "track-apt"), "invalid fixture operation")
        return self.target("python3", "/usr/local/lib/slu-target.py", operation, *args)

    def version(self):
        return self.target("dpkg-query", "-W", "-f=${Version}", PACKAGE).strip()

    def wait_file(self, path, seconds=20):
        require(path in ("/run/slu-mutation-started", "/run/slu-lock-held"), "invalid fixture marker")
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            found = self.target("sh", "-c", f"test ! -f {path} || printf ready").strip()
            if found == "ready": return
            time.sleep(.1)
        raise AssertionError("fixture marker timed out: " + path)

    def start(self, binary):
        self.docker("build", "--build-arg", "BASE_IMAGE=" + IMAGES[self.distro],
            "--label", LABEL + "=" + self.owner, "-f", str(HERE / "target.Dockerfile"),
            "-t", self.image, str(HERE), timeout=600)
        # Docker internal networks do not publish ports on recent engines. A dedicated
        # bridge without outbound masquerading preserves loopback publication.
        self.docker("network", "create", "--opt", "com.docker.network.bridge.enable_ip_masquerade=false", "--label", LABEL + "=" + self.owner, self.network)
        self.container = self.docker("run", "-d", "--name", self.name, "--network", self.network,
            "--label", LABEL + "=" + self.owner, "--env", "SLU_FIXTURE_OWNER=" + self.owner,
            # Older fuser versions report unreadable operator process descriptors
            # unless root can inspect them. Keep this inside the target's PID namespace.
            "--cap-add=SYS_PTRACE",
            "--pids-limit", "256", "--memory", "768m", self.image).strip()
        self.ssh_host, self.ssh_port = self.name, 22
        end = time.monotonic() + 30
        while time.monotonic() < end:
            try:
                self.target("true")
                break
            except (RuntimeError, AssertionError): time.sleep(.2)
        self.target("chpasswd", data=f"root:{self.password}\noperator:{self.password}\n".encode())
        self.target("python3", "-c", "import socket,time; end=time.monotonic()+30\nwhile time.monotonic()<end:\n try:\n  s=socket.create_connection(('127.0.0.1',22),1); b=s.recv(128); s.close()\n  if b.startswith(b'SSH-'): break\n except OSError: pass\n time.sleep(.1)\nelse: raise SystemExit('SSH not ready')")
        command(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(self.work / "client")])
        key = (self.work / "client").read_bytes()
        self.redactions.append(key.decode())
        self.target("mkdir", "-p", "/root/.ssh")
        self.target("sh", "-c", "cat > /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys",
                    data=(self.work / "client.pub").read_bytes())
        (self.work / "known_hosts").touch(mode=0o600)
        # Copy only source-controlled public assets, excluding local prototypes or
        # data. The app has no host /data, legacy inventory, SSH agent or home mount.
        copy_assets(self.work)
        env_args = []
        container_env = app_environment(Path("/work"), 8080)
        container_env["DEBIAN_UPDATER_LISTEN_ADDR"] = ":8080"
        container_env["DEBIAN_UPDATER_HOST_FACTS_AUTO_REFRESH_ENABLED"] = "true"
        for key, value in container_env.items():
            env_args.extend(["--env", key + "=" + value])
        self.docker("run", "-d", "--name", self.app_name, "--network", self.network,
            "--label", LABEL + "=" + self.owner, "--user", f"{os.getuid()}:{os.getgid()}",
            "--pids-limit", "256", "--memory", "512m", "-p", "127.0.0.1::8080",
            "--mount", f"type=bind,src={self.work},dst=/work", "--workdir", "/work",
            "--entrypoint", "/work/webserver", *env_args, self.image)
        self.connect_app()
        self.api.request("POST", "/api/auth/setup", {"username": "fixture-admin", "password": self.password})
        self.identity = self.api.request("GET", "/api/auth/identity")["account"]
        require(self.identity["id"] == 1 and self.identity["username"] == "fixture-admin"
                and self.identity["role"] == "admin" and not self.identity["disabled"],
                "fixture setup did not establish the expected stable administrator")
        require(self.api.request("GET", "/api/servers") == [], "fresh application unexpectedly contains inventory")
        self.bootstrap_versions = self.target("dpkg-query", "-W", "-f=${binary:Package}=${Version}\n", "apt", "dpkg", "openssh-server", "sudo", "curl").splitlines()
        self.original_fingerprint = self.trust()
        self.api.request("PUT", "/api/postcheck-profiles", {"revision": 0, "profiles": [{
            "id": "fixture", "name": "Real fixture HTTP health", "target_servers": [], "target_tags": ["fixture"],
            "checks": [{"id": "health", "name": "Fixture responds", "kind": "http_status", "url": "http://localhost:18080/version",
                "expected_status": 200, "timeout_seconds": 3, "interval_seconds": 1, "mode": "blocking"}]}]})

    def connect_app(self, *, recovering=False, include_error_body=True):
        require(self.owned("container", self.app_name), "application ownership mismatch")
        host, port = self.docker("port", self.app_name, "8080/tcp").strip().rsplit(":", 1)
        validate_local_endpoint(host, int(port))
        self.api = API(int(port), include_error_body=include_error_body)
        self.wait_for_app(path="/api/maintenance" if recovering else "/api/auth/status")

    def wait_for_app(self, *, path="/api/auth/status"):
        def check_running(remaining):
            require(self.docker("inspect", "--format", "{{.State.Running}}", self.app_name,
                                timeout=min(20, remaining)).strip() == "true",
                    "isolated application exited before readiness")
        wait_for_app(self.api, check_running=check_running, path=path)

    def trust(self):
        require(self.ssh_host == self.name and self.ssh_port == 22 and self.owned("container", self.container), "invalid owned SSH target")
        target = {"host": self.ssh_host, "port": self.ssh_port}
        scan = self.api.request("POST", "/api/hostkeys/scan", target)
        key_files = {"ssh-ed25519": "ed25519", "ssh-rsa": "rsa", "ecdsa-sha2-nistp256": "ecdsa"}
        require(scan["algorithm"] in key_files, "unexpected fixture host key algorithm")
        public_path = "/etc/ssh/ssh_host_" + key_files[scan["algorithm"]] + "_key.pub"
        public = self.target("cat", public_path).split()[:2]
        require(scan["known_hosts_line"].split()[1:3] == public, "API scanned a key other than the owned fixture")
        fingerprint = self.target("ssh-keygen", "-E", "sha256", "-lf", public_path).split()[1]
        require(scan["fingerprint_sha256"] == fingerprint, "host key fingerprint mismatch")
        self.api.request("POST", "/api/hostkeys/trust", {**target, "fingerprint_sha256": scan["fingerprint_sha256"]})
        return scan["fingerprint_sha256"]

    def add_server(self, name, user="root"):
        require(self.ssh_host == self.name and self.ssh_port == 22 and self.owned("container", self.container), "invalid owned SSH target")
        # Inventory deliberately forbids duplicate endpoints, even for distinct
        # accounts. Retire each completed fixture entry before the next scenario.
        for previous in self.api.request("GET", "/api/servers"):
            require(previous["host"] == self.ssh_host and previous["port"] == 22, "unexpected inventory in isolated app")
            end = time.monotonic() + 5
            while True:
                try:
                    self.api.request("DELETE", "/api/servers/" + previous["name"])
                    break
                except AssertionError as error:
                    if "HTTP 409" not in str(error) or time.monotonic() >= end: raise
                    time.sleep(.1)
        self.api.request("POST", "/api/servers", {"name": name, "host": self.ssh_host, "port": self.ssh_port,
            "user": user, "pass": self.password, "tags": ["fixture"]}, allowed=(201,))
        if user == "root": self.api.upload_key(name, (self.work / "client").read_bytes())

    def start_update(self, name, job_id=None):
        if job_id is None:
            job_id = self.api.request("POST", "/api/update/" + name)["job_id"]
        job, transitions = self.api.wait_job(job_id, lambda j: j["status"] == "waiting_approval")
        self.transitions[job_id] = transitions
        status = self.api.server(name)
        require(status["status"] == "pending_approval" and status["job_id"] == job_id, "approval projection mismatch")
        require(self.version() == "1.0", "discovery changed installed package version")
        require([p["package"] for p in status["pending_updates"]] == [PACKAGE], "public/system packages entered fixture plan")
        return {"job_id": job_id, "approval_generation": status["approval_generation"]}

    def finish(self, name, identity, expected="succeeded"):
        self.api.request("POST", "/api/approve/" + name, identity)
        job, states = self.api.wait_job(identity["job_id"], lambda j: j["status"] in TERMINAL)
        self.transitions[identity["job_id"]].extend(states)
        require(job["status"] == expected, f"unexpected terminal result: {job}")
        return job

    def audit(self, name, action="update.complete"):
        path = "/api/audit-events?" + urllib.parse.urlencode({"target_name": name, "action": action})
        end = time.monotonic() + 5
        while time.monotonic() < end:
            items = self.api.request("GET", path)["items"]
            if items: return items[0], json.loads(items[0]["meta_json"])
            time.sleep(.05)
        raise AssertionError("missing completion audit")

    @contextmanager
    def scenario(self, name):
        started = time.monotonic()
        record = {"scenario": name, "status": "failed"}
        try:
            yield record
            record["status"] = "passed"
        except Exception as error:
            record["error"] = str(error)
            raise
        finally:
            record["duration_seconds"] = round(time.monotonic() - started, 3)
            self.records.append(record)
            print(name + ": " + record["status"], flush=True)

    def qualify_batch_interruption(self, record):
        name = "batch-interruption"
        self.fixture("reset")
        self.add_server(name)
        archive_passphrase = secrets.token_urlsafe(24)

        def wait_for_facts():
            deadline = time.monotonic() + 20
            while self.api.server(name)["status"] == "facts_refresh":
                require(time.monotonic() < deadline, "startup facts refresh did not finish")
                time.sleep(.1)

        def check_latch(expected_job):
            status = self.api.server(name)
            require(status.get("apt_reconciliation_required") and status.get("apt_reconciliation_job_id") == expected_job,
                    "APT latch or exact job disappeared while checking the host")

        def export():
            wait_for_facts()
            archive = self.api.request("POST", "/api/backup/export", {"passphrase": archive_passphrase}, binary=True)
            require(100 < len(archive) <= 16 * 1024 * 1024, "expected a bounded encrypted fixture archive")
            return archive

        def restore(archive):
            wait_for_facts()
            self.api.backup_archive("restore", archive, archive_passphrase)
            require(not self.api.request("GET", "/api/auth/status")["authenticated"], "restore retained the former session")
            self.api.request("POST", "/api/auth/login", {"username": "fixture-admin", "password": self.password})

        replaced_identity = self.api.server(name)["server_identity"]
        conflicting_archive = export()
        self.add_server(name)
        pre_incident_archive = export()
        server_identity = self.api.server(name)["server_identity"]
        require(server_identity != replaced_identity, "recreated fixture reused its inventory identity")
        initial_review = self.api.backup_archive("verify", conflicting_archive, archive_passphrase)
        require(initial_review["restore_ready"] and initial_review["installation_review"]["state"] == "ready",
                "archive did not verify before any live APT incident existed")
        batch_id = uuid.uuid4().hex
        batch_path = "/api/status/batches/" + batch_id
        self.api.request("POST", "/api/status/batches", {"id": batch_id, "action": "update",
            "targets": [{"name": name, "server_identity": server_identity}]}, allowed=(202,))

        def batch_host():
            batch = self.api.request("GET", batch_path)["batch"]
            require(batch["id"] == batch_id and len(batch["hosts"]) == 1 and
                    batch["hosts"][0]["server_identity"] == server_identity, "batch host identity changed")
            return batch["hosts"][0]

        job_id = None
        end = time.monotonic() + 15
        while time.monotonic() < end:
            host = batch_host()
            job_id = host.get("job_id")
            if job_id or host["state"] not in ("queued", "admitting"):
                break
            time.sleep(.1)
        require(job_id, "batch never admitted a job")
        identity = self.start_update(name, job_id)

        def upgrades():
            return int(self.target("awk", '$3 == "upgrade" && $4 ~ /^slu-ci-fixture(:all)?$/ && $5 == "1.0" && $6 == "2.0" {n++} END {print n+0}',
                                   "/var/log/dpkg.log"))

        baseline_upgrades = upgrades()
        self.target("touch", "/run/slu-hold-mutation")
        try:
            self.api.request("POST", "/api/approve/" + name, identity)
            self.wait_file("/run/slu-mutation-started")
            self.target("fuser", "/var/lib/dpkg/lock")
            require(self.api.job(job_id)["phase"] == "apt_upgrade", "job did not persist its mutation phase")
            require(self.owned("container", self.app_name), "application ownership mismatch")
            self.docker("kill", self.app_name)
            self.docker("start", self.app_name)
            self.connect_app()
            self.api.request("POST", "/api/auth/login", {"username": "fixture-admin", "password": self.password})
            self.target("fuser", "/var/lib/dpkg/lock")
            host = batch_host()
            job = self.api.job(job_id)
            require(job["status"] == "failed" and job["error_class"] == "reconciliation_required" and
                    host.get("job_id") == job_id and host.get("reconciliation_required") and
                    host.get("diagnostic_available") and not host.get("retryable"),
                    "restart did not preserve the batch job and its reconciliation requirement")
            check_latch(job_id)
            logs_path = f"/api/jobs/{job_id}/logs"
            log_head = self.api.request("GET", logs_path + "?limit=1")
            require(log_head["has_more"], "interrupted job must have multiple retained log fragments")
            log_tail_path = f"{logs_path}?after_seq={log_head['next_sequence']}&limit=500"
            log_tail = self.api.request("GET", log_tail_path)
            wait_for_facts()
            review = self.api.backup_archive("verify", conflicting_archive, archive_passphrase)
            require(review["restore_ready"] and review["installation_review"]["state"] == "blocked",
                    "review failed to distinguish a valid archive from an APT conflict on this installation")
            conflicts = review["installation_review"]["conflicts"]
            require(len(conflicts) == 1 and conflicts[0]["code"] == "apt_target_changed"
                    and conflicts[0]["server"] == name and conflicts[0]["report_url"] == f"/api/reports/jobs/{job_id}",
                    "review did not identify the replaced server and the exact authorized report")
            conflict = self.api.backup_archive("restore", conflicting_archive, archive_passphrase, allowed=(409,))
            require("conflicts with APT reconciliation evidence" in conflict["error"]
                    and self.api.server(name)["server_identity"] == server_identity
                    and self.api.server(name).get("job_id") == job_id
                    and self.api.job(job_id)["error_class"] == "reconciliation_required",
                    "restore with a reused name did not preserve the live installation and incident")
            restore(pre_incident_archive)
            check_latch(job_id)
            require(self.api.job(job_id)["error_class"] == "reconciliation_required",
                    "restore of a pre-incident archive lost the live APT reconciliation job")
            self.docker("restart", "--timeout", "25", self.app_name)
            self.connect_app()
            self.api.request("POST", "/api/auth/login", {"username": "fixture-admin", "password": self.password})
            check_latch(job_id)
            require(self.api.job(job_id)["status"] == "failed",
                    "restart after restore lost the live APT reconciliation job")
            require(self.api.request("GET", log_tail_path) == log_tail,
                    "restart after restore changed the interrupted job's log fragments or cursor")
            self.api.request("PUT", "/api/servers/" + name, {
                "name": name, "host": self.ssh_host, "port": self.ssh_port + 1, "user": "root",
            }, allowed=(409,))
            for action in ("update", "autoremove"):
                rejection = self.api.request("POST", f"/api/{action}/{name}", allowed=(409,))
                require("APT reconciliation is required" in rejection["error"],
                        f"{action} was not blocked by the recovered APT reconciliation requirement")

            wait_for_facts()
            locked_repair_id = self.api.request("POST", "/api/repair/" + name,
                                                {"confirm": True, "job_id": job_id})["job_id"]
            locked_repair, _ = self.api.wait_job(locked_repair_id, lambda j: j["status"] in TERMINAL, seconds=15)
            require(locked_repair["status"] == "failed" and locked_repair["error_class"] == "reconciliation_required" and
                    "APT/DPKG repair blocked: package-manager lock is active." in locked_repair["logs_text"],
                    "repair did not respect the active lock and preserve the reconciliation requirement")
            check_latch(locked_repair_id)
            restore(pre_incident_archive)
            require(self.api.server(name).get("job_id") == locked_repair_id
                    and self.api.job(job_id)["error_class"] == "reconciliation_required"
                    and self.api.request("GET", log_tail_path) == log_tail,
                    "restore after failed repair lost the original interrupted mutation report or the active repair job")
            self.api.request("POST", "/api/repair/" + name,
                             {"confirm": True, "job_id": job_id}, allowed=(409,))
            require(upgrades() == baseline_upgrades + 1, "APT mutation was not attempted exactly once")
        finally:
            self.target("rm", "/run/slu-hold-mutation")

        self.target("sh", "-c", "for i in $(seq 1 100); do fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock /var/lib/apt/lists/lock >/dev/null 2>&1 || exit 0; sleep .1; done; exit 1")
        self.target("sh", "-c", "rm /etc/ssh/ssh_host_*; ssh-keygen -A >/dev/null")
        require(self.owned("container", self.container), "target ownership mismatch")
        self.docker("kill", "--signal", "HUP", self.container)
        time.sleep(.3)
        self.api.request("POST", "/api/hostkeys/clear", {"host": self.ssh_host, "port": self.ssh_port})
        verified_key = self.trust()
        require(verified_key != self.original_fingerprint, "fixture did not negotiate a new host key")
        rejected_id = self.api.request("POST", "/api/repair/" + name,
                                       {"confirm": True, "job_id": locked_repair_id})["job_id"]
        rejected, _ = self.api.wait_job(rejected_id, lambda j: j["status"] in TERMINAL)
        require(rejected["status"] == "failed" and "SSH host key differs" in rejected["logs_text"],
                "newly trusted host key did not block repair before executing a command")
        check_latch(rejected_id)
        self.api.request("POST", "/api/repair/" + name,
                         {"confirm": True, "job_id": locked_repair_id, "verified_host_key": verified_key}, allowed=(409,))
        pending_repair_archive = export()
        repair_id = self.api.request("POST", "/api/repair/" + name,
                                    {"confirm": True, "job_id": rejected_id, "verified_host_key": verified_key})["job_id"]
        event, facts = self.audit(name, "apt_repair.start")
        require(event["actor"] == "account:" + str(self.identity["id"]) and event["status"] == "started" and
                facts.get("account_id") == self.identity["id"] and
                facts.get("job_id") == repair_id and facts.get("reconciliation_job_id") == rejected_id and
                facts.get("previous_host_fingerprint") == self.original_fingerprint and
                facts.get("independently_verified_host_fingerprint") == verified_key,
                "rotated host key confirmation did not retain the operator, job and fingerprint audit evidence")
        repaired, _ = self.api.wait_job(repair_id, lambda j: j["status"] in TERMINAL)
        require(repaired["status"] == "succeeded" and not self.api.server(name).get("apt_reconciliation_required"),
                "explicit repair did not reconcile the restarted host")
        require(self.target("dpkg", "--audit").strip() == "" and self.version() == "2.0" and
                upgrades() == baseline_upgrades + 1, "repair did not verify APT state without replaying the upgrade")
        # An intervening restore with no archived incident must still preserve
        # the repair needed to settle the next, newer archive.
        restore(pre_incident_archive)
        repaired_review = self.api.backup_archive("verify", pending_repair_archive, archive_passphrase)
        require(repaired_review["restore_ready"] and repaired_review["installation_review"]["state"] == "ready",
                "review did not accept the later verified repair for this archive")
        restore(pending_repair_archive)
        require(not self.api.server(name).get("apt_reconciliation_required") and self.api.job(repair_id)["status"] == "succeeded"
                and upgrades() == baseline_upgrades + 1,
                "restoring an older pending-repair archive reopened a resolved incident")
        record.update(batch_id=batch_id, job_id=job_id, error_class=job["error_class"],
                      mutation_attempts=1, locked_repair_job_id=locked_repair_id,
                      repair_job_id=repair_id, changed_key_rejection_job_id=rejected_id,
                      live_reconciliation_after_restore="passed", repaired_reconciliation_after_restore="passed",
                      conflicting_inventory_restore="passed",
                      final_version=self.version())

    def qualify_restore_crash(self, record):
        # The VM runner imports the shared harness without the Docker target.
        from target import WEBHOOK_PORT

        name = "restore-crash"
        credentials = {"username": "fixture-admin", "password": self.password}
        passphrase = secrets.token_urlsafe(24)
        self.redactions.append(passphrase)
        self.api.include_error_body = False
        record["checkpoint"] = "export"
        self.fixture("reset")
        self.fixture("track-apt")
        self.api.request("POST", "/api/hostkeys/clear", {"host": self.ssh_host, "port": self.ssh_port})
        self.trust()
        known_hosts = self.work / "known_hosts"
        trusted_hosts = known_hosts.read_bytes()
        require(trusted_hosts, "trusted fixture host key was not saved")
        self.add_server(name)
        server_identity = self.api.server(name)["server_identity"]
        template = '{{ (index .NetworkSettings.Networks "' + self.network + '").IPAddress }}'
        address = self.docker("inspect", "--format", template, self.container).strip()
        require(ipaddress.IPv4Address(address).is_private, "webhook receiver must use the owned private bridge")
        url = f"http://{address}:{WEBHOOK_PORT}/receive"
        self.redactions.extend([url, address])
        settings = self.api.request("PUT", "/api/notifications/settings", {
            "enabled": True, "webhook_url_intent": "replace", "webhook_url": url, "event_types": [APT_EVENT],
        })
        require(settings["enabled"] and settings["webhook_configured"], "fixture webhook was not enabled")
        archive = self.api.request("POST", "/api/backup/export", {"passphrase": passphrase}, binary=True)
        require(100 < len(archive) <= 16 * 1024 * 1024, "expected a bounded encrypted archive")

        record["checkpoint"] = "apt-interruption"
        identity = self.start_update(name)
        job_id = identity["job_id"]
        self.target("touch", "/run/slu-hold-mutation")
        try:
            self.api.request("POST", "/api/approve/" + name, identity)
            self.wait_file("/run/slu-mutation-started")
            require(self.api.job(job_id)["phase"] == "apt_upgrade", "APT mutation phase did not persist")
            self.kill_app()
            self.docker("start", self.app_name)
            self.connect_app(include_error_body=False)
            self.api.request("POST", "/api/auth/login", credentials)
        finally:
            self.target("rm", "-f", "/run/slu-hold-mutation")
        report_path = f"/api/reports/jobs/{job_id}"
        job = self.api.job(job_id)
        status = self.api.server(name)
        require(job["status"] == "failed" and job["error_class"] == "reconciliation_required"
                and status.get("apt_reconciliation_required") and status.get("apt_reconciliation_job_id") == job_id,
                "APT interruption did not create the original incident")
        report = self.api.request("GET", report_path, binary=True)
        require(report.startswith(f"# Update Job Report {job_id}\n".encode()), "original job report did not match")
        until(lambda: self.version() == "2.0" and not self.target("dpkg", "--audit").strip(),
              "original package mutation did not settle")

        record["checkpoint"] = "notification-delivery"
        receipts_path = "/var/lib/slu-fixture/webhook-receipts"

        def receipts():
            return [json.loads(line) for line in self.target("cat", receipts_path).splitlines()]

        def execution_counts():
            calls = self.target("cat", "/var/lib/slu-fixture/apt-invocations").splitlines()
            return {action: calls.count(action) for action in ("update", "upgrade", "full-upgrade", "autoremove")}

        def successful_delivery():
            history = self.api.request("GET", f"/api/notifications/delivery-history?job_id={job_id}")["items"]
            return len(history) == 1 and history[0]["state"] == "succeeded" and history[0]["attempts"] == 1

        until(successful_delivery, "worker did not record the fixture delivery as successful")
        expected_receipts = [[APT_EVENT, name, job_id]]
        require(receipts() == expected_receipts, "receiver did not observe exactly one delivery for the original incident")
        commands = execution_counts()
        require(commands["update"] >= 1 and commands["upgrade"] + commands["full-upgrade"] >= 1,
                "command tracker did not observe the real APT run")
        db_uri = (self.work / "servers.db").as_uri()

        def persistence():
            with closing(sqlite3.connect(db_uri + "?mode=ro", uri=True, timeout=5)) as db:
                db.execute("BEGIN")
                incident = db.execute("SELECT status, error_class, apt_target FROM jobs WHERE id=?", (job_id,)).fetchone()
                chunks = db.execute("SELECT sequence, stream, data FROM job_log_chunks WHERE job_id=? ORDER BY sequence",
                                    (job_id,)).fetchall()
                delivered = db.execute(
                    "SELECT id, destination_fingerprint, state, attempts, json_extract(meta_json, '$.condition_id') "
                    "FROM notification_outbox WHERE event_type=? AND json_extract(meta_json, '$.job_id')=?",
                    (APT_EVENT, job_id)).fetchall()
                row = db.execute("SELECT state_json FROM notification_conditions WHERE id=1").fetchone()
                state = json.loads(row[0]) if row else {}
                admitted = False
                if len(delivered) == 1:
                    fingerprint, condition_id = delivered[0][1], delivered[0][4]
                    admitted = db.execute("SELECT 1 FROM notification_apt_admissions WHERE identity=?",
                                          (f"condition/{condition_id}/{fingerprint}",)).fetchone() == (1,)
                marker = db.execute("SELECT value FROM settings WHERE key='maintenance_state'").fetchone()
                return incident, chunks, delivered, state, admitted, json.loads(marker[0]) if marker else {}

        incident, chunks, delivered, _, admitted, _ = persistence()
        require(incident and incident[:2] == ("failed", "reconciliation_required") and incident[2] and chunks
                and len(delivered) == 1 and delivered[0][2:4] == ("succeeded", 1) and admitted,
                "original job, logs or successful delivery were missing")
        fingerprint, condition_id = delivered[0][1], delivered[0][4]
        require(fingerprint and condition_id, "delivery was not tied to the incident and configured destination")
        self.redactions.append(fingerprint)

        record["checkpoint"] = "restore-process-kill"
        until(lambda: self.api.server(name)["status"] != "facts_refresh", "startup facts refresh did not finish")
        review = self.api.backup_archive("verify", archive, passphrase)
        require(review["restore_ready"] and review["installation_review"]["state"] == "ready",
                "pre-incident archive did not verify for this installation")
        self.crash_during_restore(archive, passphrase)
        record["crash_confirmed_after_sqlite_replacement"] = True
        record["checkpoint"] = "staged-restore-evidence"
        current_incident, current_chunks, current_delivery, state, admitted, marker = persistence()
        require(marker.get("recovery_required") and marker.get("kind") == "backup_restore",
                "replacement database did not persist the recovery latch before runtime reload")
        require((current_incident, current_chunks, current_delivery) == (incident, chunks, delivered),
                "replacement database lost the exact job, logs or delivery before runtime reload")
        require(admitted and condition_id not in (state.get("Conditions") or {}),
                "replacement database did not stage the admission independently of runtime reconciliation")

        record["checkpoint"] = "recovery-restarts"
        observe = self.work / "observe-notifications"
        poll = self.work / "notification-poll-complete"
        observe.touch(exist_ok=False)
        for restart in range(2):
            if restart:
                self.stop_app()
            poll.unlink(missing_ok=True)
            self.docker("start", self.app_name)
            self.connect_app(recovering=True, include_error_body=False)
            maintenance = self.api.request("GET", "/api/maintenance")
            require(maintenance["active"] and maintenance["recovery_required"] and maintenance["kind"] == "backup_restore",
                    "startup did not preserve the interrupted restore recovery latch")
            for method, path in (("GET", report_path), ("POST", f"/api/update/{name}"), ("POST", f"/api/autoremove/{name}")):
                blocked = self.api.request(method, path, allowed=(503,))
                require(blocked.get("recovery_required"), "runtime bypassed the recovery latch")
            until(poll.is_file, "notification worker did not finish condition reconciliation and drain")
            require(poll.read_text() == fingerprint, "restored worker did not observe the original enabled webhook")
            current_incident, current_chunks, current_delivery, state, admitted, _ = persistence()
            fact = (state.get("Conditions") or {}).get(condition_id, {}).get("Fact", {})
            require(fact.get("Active") and fact.get("TargetName") == name
                    and json.loads(fact["MetaJSON"]).get("job_id") == job_id
                    and admitted,
                    "notification worker did not observe the original active incident and admission")
            require((current_incident, current_chunks, current_delivery) == (incident, chunks, delivered)
                    and receipts() == expected_receipts and execution_counts() == commands,
                    "restart changed the original evidence, repeated a delivery or executed another APT command")
            record["verified_recovery_restarts"] = restart + 1

        record["checkpoint"] = "authorized-report"
        self.stop_app()
        # The HTTP API is unavailable while latched. Verify the disposable
        # replacement files offline before completing the documented release.
        with closing(sqlite3.connect(db_uri + "?mode=rw", uri=True, timeout=5)) as db, db:
            require(db.execute("PRAGMA quick_check").fetchall() == [("ok",)]
                    and db.execute("SELECT name, host, port, identity FROM servers").fetchall()
                    == [(name, self.ssh_host, self.ssh_port, server_identity)]
                    and known_hosts.read_bytes() == trusted_hosts,
                    "replacement persistence, inventory or host trust failed verification")
            require(db.execute("DELETE FROM settings WHERE key='maintenance_state'").rowcount == 1,
                    "offline recovery marker was missing")
        poll.unlink()
        self.docker("start", self.app_name)
        self.connect_app(include_error_body=False)
        self.api.request("GET", report_path, allowed=(401,))
        self.api.request("POST", "/api/auth/login", credentials)
        require(self.api.request("GET", report_path, binary=True) == report,
                "authorized report differed from the original incident")
        for action in ("update", "autoremove"):
            blocked = self.api.request("POST", f"/api/{action}/{name}", allowed=(409,))
            require("APT reconciliation is required" in blocked["error"], "recovered APT latch did not block " + action)
        until(poll.is_file, "notification worker did not finish after offline recovery")
        require(poll.read_text() == fingerprint, "recovered worker did not observe the original enabled webhook")
        require(receipts() == expected_receipts and execution_counts() == commands,
                "offline recovery repeated a webhook delivery or executed another APT command")
        record.update(job_id=job_id, authorized_report_preserved=True, webhook_deliveries=1,
                      extra_update_attempts=0, extra_autoremove_attempts=0)
        del record["checkpoint"]

    def crash_during_restore(self, archive, passphrase):
        (self.work / "arm-restore-crash").touch(exist_ok=False)
        with ThreadPoolExecutor(max_workers=1) as pool:
            request = pool.submit(self.api.backup_archive, "restore", archive, passphrase, timeout=45)
            killed = False
            try:
                until(lambda: request.done() or (self.work / "restore-db-replaced").is_file(),
                      "restore did not reach the SQLite replacement checkpoint", seconds=35)
                require(not request.done(), "restore completed before the crash checkpoint")
            finally:
                # Release an active request before the executor joins, even if
                # the controlling thread timed out or received SIGINT/SIGTERM.
                if not request.cancel() and not request.done():
                    self.kill_app()
                    killed = True
            require(killed, "restore connection closed before the fixture sent SIGKILL")
            try:
                request.result(timeout=5)
            except (http.client.RemoteDisconnected, ConnectionResetError):
                return
            raise AssertionError("restore request did not lose its connection to the killed process")

    def kill_app(self):
        require(self.owned("container", self.app_name), "application ownership mismatch")
        self.docker("kill", self.app_name)
        require(self.docker("inspect", "--format", "{{.State.ExitCode}}", self.app_name).strip() == "137",
                "fixture process was not killed with SIGKILL")

    def stop_app(self):
        require(self.owned("container", self.app_name), "application ownership mismatch")
        self.docker("stop", "--timeout", "25", self.app_name)
        require(self.docker("inspect", "--format", "{{.State.Running}}", self.app_name).strip() == "false",
                "fixture process did not stop")

    def qualify(self):
        for user in ("root", "operator"):
            name = user + "-upgrade"
            with self.scenario(name) as record:
                self.fixture("reset")
                self.add_server(name, user)
                if user == "operator":
                    job_id = self.api.request("POST", "/api/sudoers/" + name, {"password": self.password})["job_id"]
                    job, _ = self.api.wait_job(job_id, lambda j: j["status"] in TERMINAL)
                    require(job["status"] == "succeeded", "production helper bootstrap failed")
                    self.target("rm", "/etc/sudoers.d/fixture-bootstrap")
                    self.target("visudo", "-cf", "/etc/sudoers.d/simplelinuxupdater")
                    self.target("sh", "-c", "test -x /usr/local/sbin/simplelinuxupdater-root-helper && test $(stat -c %U /usr/local/sbin/simplelinuxupdater-root-helper) = root")
                identity = self.start_update(name)
                self.api.request("POST", "/api/approve/" + name, {**identity, "approval_generation": identity["approval_generation"] + 100}, allowed=(409,))
                require(self.version() == "1.0", "stale approval mutated fixture")
                job = self.finish(name, identity)
                require(self.version() == "2.0", "fixture upgrade did not install version2")
                meta = json.loads(job["meta_json"])
                require(meta["approval_generation"] == identity["approval_generation"], "job lost the accepted approval generation")
                require(meta["application_postcheck_profile"]["id"] == "fixture", "job lost profile snapshot")
                require(meta["application_postcheck_results"][0]["passed"], "real HTTP postcheck failed")
                event, facts = self.audit(name)
                expected_actor = "account:" + str(self.identity["id"])
                require(job["actor"] == expected_actor and event["actor"] == expected_actor
                        and event["status"] == "success" and facts.get("account_id") == self.identity["id"]
                        and "personal_token_id" not in facts,
                        "job/audit lost the authenticated fixture cookie account identity or outcome")
                require(facts["upgrade_completed"] and facts["postchecks_passed"] and facts["approved_packages"] == [PACKAGE], "completion audit lacks operation facts")
                published, published_meta = self.audit(name, "approval.published")
                require(published_meta["job_id"] == identity["job_id"] and published_meta["approval_generation"] == identity["approval_generation"], "approval identity not audited")
                record.update(final_version="2.0", job_id=job["id"], approval=identity,
                    job_states=self.transitions[job["id"]], profile_revision=meta["application_postcheck_profile"]["revision"], audit_id=event["id"])

        with self.scenario("misconfigured-helper") as record:
            self.fixture("reset")
            self.add_server("bad-helper", "operator")
            self.target("chmod", "000", "/usr/local/sbin/simplelinuxupdater-root-helper")
            try:
                job_id = self.api.request("POST", "/api/update/bad-helper")["job_id"]
                job, _ = self.api.wait_job(job_id, lambda j: j["status"] in TERMINAL)
                require(job["status"] == "failed" and self.version() == "1.0", "bad helper allowed package mutation")
                record.update(job_id=job_id, error_class=job["error_class"], final_version=self.version())
            finally: self.target("chmod", "755", "/usr/local/sbin/simplelinuxupdater-root-helper")

        with self.scenario("apt-lock") as record:
            self.fixture("reset")
            self.add_server("locked")
            self.docker("exec", "-d", self.container, "python3", "/usr/local/lib/slu-target.py", "lock")
            self.wait_file("/run/slu-lock-held")
            try:
                job_id = self.api.request("POST", "/api/update/locked")["job_id"]
                job, _ = self.api.wait_job(job_id, lambda j: j["status"] in TERMINAL)
                require(job["status"] == "failed" and self.version() == "1.0", "APT lock did not block mutation")
                _, facts = self.audit("locked")
                require(facts["precheck_failed"] == "apt_locks", "wrong precheck failure")
                record.update(job_id=job_id, failed_check=facts["precheck_failed"], final_version=self.version())
            finally:
                self.target("pkill", "-f", "^python3 /usr/local/lib/slu-target.py lock$")

        with self.scenario("plan-drift-and-obsolete-approval") as record:
            self.fixture("reset")
            self.add_server("drift")
            old = self.start_update("drift")
            self.fixture("publish", "3.0")
            self.target("apt-get", "update")
            self.api.request("POST", "/api/approve/drift", old)
            end = time.monotonic() + 30
            while time.monotonic() < end:
                status = self.api.server("drift")
                if status["status"] == "pending_approval" and status["approval_generation"] > old["approval_generation"]: break
                time.sleep(.1)
            require(status["approval_generation"] > old["approval_generation"], "changed plan did not require fresh approval")
            self.api.request("POST", "/api/approve/drift", old, allowed=(409,))
            require(self.version() == "1.0", "obsolete approval changed package")
            fresh = {"job_id": old["job_id"], "approval_generation": status["approval_generation"]}
            self.finish("drift", fresh)
            require(self.version() == "3.0", "refreshed approved candidate not installed")
            record.update(previous_generation=old["approval_generation"], accepted_generation=fresh["approval_generation"], final_version="3.0")

        with self.scenario("ssh-disconnect-during-mutation-and-repair") as record:
            self.fixture("reset")
            self.add_server("disconnect")
            identity = self.start_update("disconnect")
            self.target("touch", "/run/slu-hold-mutation")
            self.api.request("POST", "/api/approve/disconnect", identity)
            self.wait_file("/run/slu-mutation-started")
            self.fixture("drop-ssh")
            job, _ = self.api.wait_job(identity["job_id"], lambda j: j["status"] in TERMINAL)
            require(job["status"] == "failed" and job["error_class"] == "reconciliation_required", "transport loss did not require reconciliation")
            self.api.request("POST", "/api/update/disconnect", allowed=(409,))
            self.target("rm", "/run/slu-hold-mutation")
            self.target("sh", "-c", "for i in $(seq 1 100); do fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock /var/lib/apt/lists/lock >/dev/null 2>&1 || exit 0; sleep .1; done; exit 1")
            repair_id = self.api.request("POST", "/api/repair/disconnect",
                                         {"confirm": True, "job_id": identity["job_id"]})["job_id"]
            repaired, _ = self.api.wait_job(repair_id, lambda j: j["status"] in TERMINAL)
            require(repaired["status"] == "succeeded", "controlled repair failed: " + str(repaired))
            require(self.target("dpkg", "--audit").strip() == "" and self.version() == "2.0", "repair did not reconcile dpkg")
            _, facts = self.audit("disconnect")
            require(facts["apt_upgrade_attempts_used"] == 1, "mutation replayed after unknown outcome")
            attempts = self.target("cat", "/var/lib/slu-fixture/configure-attempts").splitlines()
            require(len(attempts) <= 2, "unexpected repeated package configuration")
            record.update(error_class=job["error_class"], mutation_attempts=facts["apt_upgrade_attempts_used"], repair_job_id=repair_id, final_version=self.version())

        with self.scenario("batch-app-interruption-during-mutation-and-repair") as record:
            self.qualify_batch_interruption(record)

        with self.scenario("changed-host-key") as record:
            self.fixture("reset")
            self.add_server("changed-key")
            self.target("sh", "-c", "rm /etc/ssh/ssh_host_*; ssh-keygen -A >/dev/null")
            require(self.owned("container", self.container), "target ownership mismatch")
            self.docker("kill", "--signal", "HUP", self.container)
            time.sleep(.3)
            scan = self.api.request("POST", "/api/hostkeys/scan", {"host": self.ssh_host, "port": 22})
            require(scan["fingerprint_sha256"] != self.original_fingerprint and not scan["already_trusted"], "host key rotation was not observed")
            job_id = self.api.request("POST", "/api/update/changed-key")["job_id"]
            job, _ = self.api.wait_job(job_id, lambda j: j["status"] in TERMINAL)
            require(job["status"] == "failed" and self.version() == "1.0", "changed host key allowed mutation")
            logs = (job["logs_text"] + self.api.server("changed-key")["logs"]).lower()
            require(any(term in logs for term in ("host key", "hostkey", "knownhosts", "fingerprint mismatch")), "failure did not identify host trust: " + logs)
            record.update(previous_fingerprint=self.original_fingerprint, rejected_fingerprint=scan["fingerprint_sha256"], final_version=self.version(), error_class=job["error_class"])

        with self.scenario(RESTORE_CRASH) as record:
            try:
                self.qualify_restore_crash(record)
            except (RuntimeError, OSError, ValueError, sqlite3.Error, subprocess.SubprocessError) as error:
                # Subprocess and HTTP errors can embed fixture output. Publish only
                # the failing checkpoint and exception type for this scenario.
                raise AssertionError(f"fixture operation failed ({type(error).__name__})") from None

        self.records.extend([
            {"scenario": "systemd-active", "status": "not_executed", "reason": "Container has no systemd PID1; no fake systemctl is installed. Use the VM harness."},
            {"scenario": "controlled-reboot", "status": "not_executed", "reason": "Docker shares host uptime; container restart is not a reboot proof. Use vm-reboot.py."},
            {"scenario": "official-provenance", "status": "not_executed", "reason": "Local fixture repository is deliberately not official; this suite does not qualify distribution provenance."},
        ])

    def redact_text(self, text):
        """Mask the primary credential before processing optional extra secrets."""
        redacted = text.replace(self.password, "[REDACTED]")
        redacted = redacted.replace(json.dumps(self.password)[1:-1], "[REDACTED]")
        for secret in sorted(set(self.redactions), key=len, reverse=True):
            if secret:
                # Apply the same primary masking to overlapping extra secrets.
                masked_secret = secret.replace(self.password, "[REDACTED]")
                masked_secret = masked_secret.replace(json.dumps(self.password)[1:-1], "[REDACTED]")
                # Errors can contain an already JSON-encoded diagnostic.
                for text_form in (masked_secret, json.dumps(masked_secret)[1:-1]):
                    redacted = redacted.replace(text_form, "[REDACTED]")
        return redacted

    def redact_evidence(self, value):
        """Redact structured report values before JSON encoding."""
        if isinstance(value, str):
            return self.redact_text(value)
        if isinstance(value, dict):
            return {key: self.redact_evidence(item) for key, item in value.items()}
        if isinstance(value, (list, tuple)):
            return [self.redact_evidence(item) for item in value]
        return value

    def cleanup(self):
        try:
            if self.output:
                self.output.mkdir(parents=True, exist_ok=True)
                if self.owned("container", self.app_name):
                    text = self.redact_text(self.docker("logs", self.app_name))[-200000:]
                    (self.output / "app.log").write_text(text)
        except RuntimeError as error:
            if "no such" not in str(error).lower(): self.cleanup_errors.append(str(error))
        except Exception as error:
            self.cleanup_errors.append(str(error))
        # Inspect names even if a create command failed after the daemon created the
        # resource, or SIGTERM arrived before its return value was assigned.
        for kind, name in (("container", self.app_name), ("container", self.name), ("network", self.network), ("image", self.image)):
            try:
                require(self.owned(kind, name), f"refusing cleanup of unowned {kind}")
                self.docker(kind, "rm", *(["-f"] if kind in ("container", "image") else []), name)
                self.owned(kind, name)
                raise AssertionError(f"{kind} remained after cleanup")
            except RuntimeError as error:
                message = str(error).lower()
                if "no such" not in message and f"network {name} not found" not in message:
                    self.cleanup_errors.append(str(error))
            except Exception as error: self.cleanup_errors.append(str(error))
        try: shutil.rmtree(self.work)
        except OSError as error: self.cleanup_errors.append(str(error))

    def report(self):
        recorded = {record["scenario"] for record in self.records}
        for name in SCENARIOS:
            if name not in recorded:
                self.records.append({"scenario": name, "status": "not_executed", "reason": "Earlier harness/scenario failure prevented execution"})
        report = {"schema_version": 1, "distribution": self.distro, "base_image": IMAGES[self.distro],
            "bootstrap_packages": getattr(self, "bootstrap_versions", []),
            "duration_seconds": round(time.monotonic() - self.started, 3),
            "scenarios": self.records, "cleanup": {"status": "failed" if self.cleanup_errors else "passed", "errors": self.cleanup_errors}}
        text = json.dumps(self.redact_evidence(report), indent=2)
        self.output.mkdir(parents=True, exist_ok=True)
        (self.output / "report.json").write_text(text + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--distro", required=True, choices=sorted(IMAGES))
    parser.add_argument("--output", type=Path, default=REPO / ".tmp-ssh-apt")
    args = parser.parse_args()
    # Output never becomes a mount or a target. Private fixtures are always freshly allocated.
    harness = Harness(args.distro, args.output.resolve())
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        binary = harness.work / "webserver"
        architecture = harness.docker("version", "--format", "{{.Server.Arch}}").strip()
        require(architecture in ("amd64", "arm64"), "unsupported local Docker architecture")
        build_env = {**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": architecture}
        overlay = qualification_overlay(harness.work)
        command(["go", "build", "-overlay", str(overlay), "-o", str(binary), "."], timeout=240, env=build_env, cwd=REPO)
        harness.start(binary)
        harness.qualify()
    except BaseException as error:
        harness.records.append({"scenario": "harness", "status": "failed", "error": str(error) or type(error).__name__})
    finally:
        harness.cleanup()
        harness.report()
    require(not harness.cleanup_errors and not any(r["status"] == "failed" for r in harness.records),
            "qualification failed; see redacted report.json")


if __name__ == "__main__": main()
