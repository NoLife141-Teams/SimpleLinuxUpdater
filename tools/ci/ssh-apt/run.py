#!/usr/bin/env python3
"""Real API/SSH/APT qualification. Never accepts existing targets or inventories."""
import argparse
from contextlib import contextmanager
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
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
SCENARIOS = ("root-upgrade", "operator-upgrade", "misconfigured-helper", "apt-lock",
             "plan-drift-and-obsolete-approval", "ssh-disconnect-during-mutation-and-repair", "changed-host-key",
             "systemd-active", "controlled-reboot", "official-provenance")


def require(condition, message):
    if not condition:
        raise AssertionError(message)


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


class API:
    def __init__(self, port):
        validate_local_endpoint("127.0.0.1", port)
        self.base = f"http://127.0.0.1:{port}"
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def request(self, method, path, data=None, *, allowed=(200,), raw=None, content_type=None):
        require(path.startswith("/api/") and not path.startswith("//"), "invalid fixture API path")
        body = raw if raw is not None else (json.dumps(data).encode() if data is not None else None)
        request = urllib.request.Request(self.base + path, data=body, method=method,
            headers={"Origin": self.base, "Content-Type": content_type or "application/json"})
        try:
            response = self.client.open(request, timeout=20)
        except urllib.error.HTTPError as error:
            response = error
        result = json.loads(response.read())
        require(response.code in allowed, f"{method} {path}: HTTP {response.code}: {result}")
        return result

    def upload_key(self, name, key):
        boundary = "slu-" + uuid.uuid4().hex
        body = (f'--{boundary}\r\nContent-Disposition: form-data; name="key"; filename="fixture.key"\r\n'
                'Content-Type: application/octet-stream\r\n\r\n').encode() + key + f"\r\n--{boundary}--\r\n".encode()
        return self.request("POST", f"/api/servers/{name}/key", raw=body,
                            content_type=f"multipart/form-data; boundary={boundary}")

    def server(self, name):
        return next(s for s in self.request("GET", "/api/servers") if s["name"] == name)

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
            require(job["status"] not in TERMINAL, f"job terminated before expected state: {job}")
            time.sleep(.15)
        raise AssertionError(f"job timeout: {self.job(job_id)}")


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
        self.redactions = [self.password]
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
        require(operation in ("reset", "publish", "drop-ssh"), "invalid fixture operation")
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
        for key, value in container_env.items():
            env_args.extend(["--env", key + "=" + value])
        self.docker("run", "-d", "--name", self.app_name, "--network", self.network,
            "--label", LABEL + "=" + self.owner, "--user", f"{os.getuid()}:{os.getgid()}",
            "--pids-limit", "256", "--memory", "512m", "-p", "127.0.0.1::8080",
            "--mount", f"type=bind,src={self.work},dst=/work", "--workdir", "/work",
            "--entrypoint", "/work/webserver", *env_args, self.image)
        require(self.owned("container", self.app_name), "application ownership mismatch")
        host, port = self.docker("port", self.app_name, "8080/tcp").strip().rsplit(":", 1)
        validate_local_endpoint(host, int(port))
        self.api = API(int(port))
        end = time.monotonic() + 30
        while time.monotonic() < end:
            require(self.docker("inspect", "--format", "{{.State.Running}}", self.app_name).strip() == "true", "isolated application exited before readiness")
            try:
                self.api.request("GET", "/api/auth/status")
                break
            except OSError: time.sleep(.1)
        self.api.request("POST", "/api/auth/setup", {"username": "fixture-admin", "password": self.password})
        require(self.api.request("GET", "/api/servers") == [], "fresh application unexpectedly contains inventory")
        self.bootstrap_versions = self.target("dpkg-query", "-W", "-f=${binary:Package}=${Version}\n", "apt", "dpkg", "openssh-server", "sudo", "curl").splitlines()
        self.original_fingerprint = self.trust()
        self.api.request("PUT", "/api/postcheck-profiles", {"revision": 0, "profiles": [{
            "id": "fixture", "name": "Real fixture HTTP health", "target_servers": [], "target_tags": ["fixture"],
            "checks": [{"id": "health", "name": "Fixture responds", "kind": "http_status", "url": "http://localhost:18080/version",
                "expected_status": 200, "timeout_seconds": 3, "interval_seconds": 1, "mode": "blocking"}]}]})

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

    def start_update(self, name):
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
                require(event["actor"] == "fixture-admin" and event["status"] == "success", "audit actor/outcome missing")
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
            repair_id = self.api.request("POST", "/api/repair/disconnect", {"confirm": True})["job_id"]
            repaired, _ = self.api.wait_job(repair_id, lambda j: j["status"] in TERMINAL)
            require(repaired["status"] == "succeeded", "controlled repair failed: " + str(repaired))
            require(self.target("dpkg", "--audit").strip() == "" and self.version() == "2.0", "repair did not reconcile dpkg")
            _, facts = self.audit("disconnect")
            require(facts["apt_upgrade_attempts_used"] == 1, "mutation replayed after unknown outcome")
            attempts = self.target("cat", "/var/lib/slu-fixture/configure-attempts").splitlines()
            require(len(attempts) <= 2, "unexpected repeated package configuration")
            record.update(error_class=job["error_class"], mutation_attempts=facts["apt_upgrade_attempts_used"], repair_job_id=repair_id, final_version=self.version())

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

        self.records.extend([
            {"scenario": "systemd-active", "status": "not_executed", "reason": "Container has no systemd PID1; no fake systemctl is installed. Use the VM harness."},
            {"scenario": "controlled-reboot", "status": "not_executed", "reason": "Docker shares host uptime; container restart is not a reboot proof. Use vm-reboot.py."},
            {"scenario": "official-provenance", "status": "not_executed", "reason": "Local fixture repository is deliberately not official; this suite does not qualify distribution provenance."},
        ])

    def cleanup(self):
        if self.output:
            self.output.mkdir(parents=True, exist_ok=True)
            try:
                if self.owned("container", self.app_name):
                    (self.work / "app.log").write_text(self.docker("logs", self.app_name))
            except RuntimeError as error:
                if "no such" not in str(error).lower(): self.cleanup_errors.append(str(error))
            if (self.work / "app.log").exists():
                text = (self.work / "app.log").read_text(errors="replace")[-200000:]
                for secret in self.redactions: text = text.replace(secret, "[REDACTED]")
                (self.output / "app.log").write_text(text)
        # Inspect names even if a create command failed after the daemon created the
        # resource, or SIGTERM arrived before its return value was assigned.
        for kind, name in (("container", self.app_name), ("container", self.name), ("network", self.network), ("image", self.image)):
            try:
                require(self.owned(kind, name), f"refusing cleanup of unowned {kind}")
                self.docker(kind, "rm", *(["-f"] if kind in ("container", "image") else []), name)
            except RuntimeError as error:
                if "no such" not in str(error).lower(): self.cleanup_errors.append(str(error))
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
        text = json.dumps(report, indent=2)
        for secret in self.redactions: text = text.replace(secret, "[REDACTED]")
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
        command(["go", "build", "-o", str(binary), "."], timeout=240, env=build_env, cwd=REPO)
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
