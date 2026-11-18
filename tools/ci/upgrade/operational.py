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
from pathlib import Path

from scenario import API, require

HERE = Path(__file__).resolve().parent
VM_TOOLS = HERE.parent / "ssh-apt"
DOCKER = ("docker", "--host=unix:///var/run/docker.sock")
LABEL = "io.simplelinuxupdater.operational-qualification"
PIN = json.loads((HERE / "operational-release.json").read_text())
PLATFORM = "linux/amd64"
BATCH_PATH = "/api/status/batches"
BATCH_SERVER = "maintenance-batch-fixture"
PROBE_PORT = 22922
PROBE_CONTACTS = "/tmp/slu-qualification-batch-contacts"


def command(*args, timeout=90, stdin=None, stdout=subprocess.PIPE):
    result = subprocess.run(args, stdin=stdin, stdout=stdout, stderr=subprocess.PIPE, timeout=timeout)
    if result.returncode:
        # Report the failing operation without storing application logs, request
        # bodies, environment values or raw subprocess output in evidence.
        raise RuntimeError(f"{' '.join(map(str, args[:4]))} exited {result.returncode}")
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
        self.report = {
            "schema_version": 1,
            "candidate_commit": sha,
            "published_version": PIN["published_version"],
            "published_image": PIN["published_image"],
            "status": "not_executed",
            "candidate_build": {"status": "not_executed"},
            "docker_upgrade": {"status": "not_executed"},
            "maintenance_batches": {
                "status": "not_executed",
                "history_after_restart": "not_executed",
                "account_isolation": "not_executed",
                "single_retry_admission": "not_executed",
                "interrupted_job_recovery": "not_executed",
            },
            "controlled_reboot_vm": {"status": "not_executed"},
            "cleanup": {"status": "not_executed"},
        }

    def step(self, name, action):
        started = time.monotonic()
        self.report[name]["status"] = "failed"
        try:
            action()
        finally:
            self.report[name]["seconds"] = round(time.monotonic() - started, 3)
        self.report[name]["status"] = "passed"
        print(f"{name} passed", flush=True)

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

    def verify_inventory(self, api):
        servers = api.request("GET", "/api/servers")
        require(len(servers) == 1 and servers[0]["name"] == "upgrade-fixture"
                and servers[0]["disabled"] and servers[0]["has_password"]
                and servers[0]["tags"] == ["qualification"], "fixture inventory was not preserved")
        policies = api.request("GET", "/api/update-policies")["items"]
        require(len(policies) == 1 and policies[0]["name"] == "Upgrade fixture"
                and not policies[0]["enabled"] and policies[0]["target_servers"] == ["upgrade-fixture"]
                and policies[0]["time_local"] == "03:00", "fixture policy was not preserved")

    def docker_upgrade(self):
        published = PIN["published_image"]
        require(re.fullmatch(r"ghcr\.io/nolife141-teams/simplelinuxupdater@sha256:[0-9a-f]{64}", published),
                "published image must be pinned by digest")
        docker("pull", "--platform", PLATFORM, published, timeout=150)
        docker("network", "create", "--internal", "--label", self.label, self.network)
        volumes = [self.prefix + "-" + suffix for suffix in ("published-data", "candidate-data", "restored-data")]
        for volume in volumes:
            docker("volume", "create", "--label", self.label, volume)
            owner = docker("volume", "inspect", "--format", '{{index .Labels "' + LABEL + '"}}', volume)
            require(owner == self.prefix, "refusing to mount a volume owned by another run")

        credentials = {"username": "qualification-admin", "password": secrets.token_urlsafe(28) + "Aa1!"}
        phrase = secrets.token_urlsafe(32)
        ssh_password = secrets.token_urlsafe(32)
        old_name = self.prefix + "-published"
        old = self.start_app(old_name, published, volumes[0])
        old.request("POST", "/api/auth/setup", credentials)
        old.request("POST", "/api/servers", {"name": "upgrade-fixture", "host": "127.0.0.1", "port": 9,
            "user": "fixture", "pass": ssh_password, "disabled": True, "tags": ["qualification"]}, allowed=(201,))
        old.request("POST", "/api/update-policies", {"name": "Upgrade fixture", "enabled": False,
            "target_servers": ["upgrade-fixture"], "execution_mode": "scan_only", "package_scope": "full",
            "cadence_kind": "daily", "time_local": "03:00", "rollout_mode": "immediate"}, allowed=(201,))
        self.verify_inventory(old)
        archive = old.request("POST", "/api/backup/export", {"passphrase": phrase, "include_known_hosts": True}, binary=True)
        require(len(archive) > 100 and ssh_password.encode() not in archive, "missing or unencrypted backup archive")
        docker("stop", "--timeout", "25", old_name)
        docker("run", "--pull", "never", "--rm", "--label", self.label, "--name", self.prefix + "-copy",
               "--network", "none", "--entrypoint", "/bin/sh",
               "--mount", f"type=volume,src={volumes[0]},dst=/source,readonly",
               "--mount", f"type=volume,src={volumes[1]},dst=/target",
               self.candidate, "-ec", "cp -a /source/. /target/")

        new_name = self.prefix + "-upgraded"
        new = self.start_app(new_name, self.candidate, volumes[1])
        new.request("POST", "/api/auth/login", credentials)
        self.verify_inventory(new)
        docker("restart", "--timeout", "25", new_name)
        self.connect(new_name, new)
        new.request("POST", "/api/auth/login", credentials)
        self.verify_inventory(new)
        self.report["docker_upgrade"]["copied_volume_restart"] = "passed"

        recovered_name = self.prefix + "-restored"
        recovered = self.start_app(recovered_name, self.candidate, volumes[2])
        recovered.request("POST", "/api/auth/setup", {"username": "recovery-admin", "password": secrets.token_urlsafe(28) + "Aa1!"})
        require(recovered.upload("/api/backup/verify", archive, phrase)["restore_ready"],
                "published archive was not restore-ready on candidate")
        recovered.upload("/api/backup/restore", archive, phrase)
        require(not recovered.request("GET", "/api/auth/status")["authenticated"], "restore retained the previous session")
        recovered.request("POST", "/api/auth/login", credentials)
        self.verify_inventory(recovered)
        docker("restart", "--timeout", "25", recovered_name)
        self.connect(recovered_name, recovered)
        recovered.request("POST", "/api/auth/login", credentials)
        self.verify_inventory(recovered)
        self.report["docker_upgrade"]["separate_volume_restore"] = "passed"
        self.step("maintenance_batches", lambda: self.qualify_maintenance_batches(new_name, new, credentials))

    def start_batch_probe(self, name):
        # The isolated peer only accepts and holds TCP connections. It does
        # not proceed with SSH or execute commands supplied over the connection.
        key = base64.b64encode(b"\0\0\0\x0bssh-ed25519\0\0\0\x20" + secrets.token_bytes(32)).decode()
        docker("exec", "--user", "app", name, "/bin/sh", "-ec",
               'umask 077; printf "%s\n" "$1" >> /data/known_hosts; printf "%s\n" "$2" > /tmp/slu-qualification-batch-probe',
               "probe", f"[127.0.0.1]:{PROBE_PORT} ssh-ed25519 {key}",
               f'printf "connected\\n" >> {PROBE_CONTACTS}\nexec /bin/sleep 60')
        docker("exec", "--detach", "--user", "app", name, "/bin/busybox", "nc", "-lk",
               "-s", "127.0.0.1", "-p", str(PROBE_PORT), "-e", "/bin/sh", "/tmp/slu-qualification-batch-probe")
        until(lambda: f"127.0.0.1:{PROBE_PORT} " in docker("exec", name, "/bin/busybox", "netstat", "-ltn"),
              "qualification TCP probe did not start")

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
        def contacts():
            return int(docker("exec", name, "/bin/sh", "-c",
                              f"if test -f {PROBE_CONTACTS}; then wc -l < {PROBE_CONTACTS}; else printf '0'; fi"))

        until(contacts, "retry job did not reach the isolated peer")
        audit_path = f"/api/audit-events?action=update.start&status=started&target_type=server&target_name={BATCH_SERVER}"
        admissions = api.request("GET", audit_path)
        require(admissions["total"] == 1 and json.loads(admissions["items"][0]["meta_json"])["job_id"] == job_id and contacts() == 1,
                "a concurrent or replayed retry produced more than one job admission")
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

    def controlled_reboot(self):
        build = self.work / "vm-runner"
        build.mkdir()
        for name in ("vm-runner.Dockerfile", "vm-reboot.py", "run.py", "images.json"):
            shutil.copyfile(VM_TOOLS / name, build / name)
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
        name = self.prefix + "-reboot"
        docker("run", "--pull", "never", "--detach", "--label", self.label, "--name", name,
               "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
               "--mount", f"type=bind,src={app},dst=/candidate,readonly",
               "--mount", f"type=bind,src={guest},dst=/guest.img,readonly",
               self.vm_runner, "--isolated-app-directory", "/candidate", "--image", "/guest.img",
               "--sha256", PIN["vm_image_sha256"], "--output", "/tmp/report")
        self.collect_vm_evidence(name, docker("wait", name, timeout=600))

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
              and proof.get("image_sha256") == PIN["vm_image_sha256"]):
            status = "passed"
        else:
            status = "failed"
        proof["status"] = status
        (self.output / "vm-report.json").write_text(json.dumps(proof, indent=2) + "\n")
        if status != "passed":
            self.report["controlled_reboot_vm"]["status"] = status
            raise RuntimeError(f"VM qualification exited {exit_code}; see vm-report.json")
        self.report["controlled_reboot_vm"]["guest_sha256"] = proof["image_sha256"]

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
            images = docker("image", "ls", "--filter", "label=" + self.label, "--format", "{{.Repository}}:{{.Tag}}").splitlines()
            for image in images:
                if image in (self.candidate, self.vm_runner):
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
        except (OSError, RuntimeError, subprocess.TimeoutExpired):
            self.report["reason"] = "Local Docker daemon is unavailable"
            return 2
        if platform != PLATFORM:
            self.report["reason"] = f"Qualification requires a {PLATFORM} Docker runner"
            return 2
        self.report["status"] = "failed"
        try:
            self.step("candidate_build", self.build_candidate)
            self.step("docker_upgrade", self.docker_upgrade)
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
