#!/usr/bin/env python3
"""Qualify a candidate image on disposable Docker volumes and a real VM."""
import argparse
import ipaddress
import json
import re
import secrets
import shutil
import signal
import subprocess
import tempfile
import time
import urllib.error
import uuid
from pathlib import Path

from scenario import API, require

HERE = Path(__file__).resolve().parent
VM_TOOLS = HERE.parent / "ssh-apt"
DOCKER = ("docker", "--host=unix:///var/run/docker.sock")
LABEL = "io.simplelinuxupdater.operational-qualification"
PIN = json.loads((HERE / "operational-release.json").read_text())
PLATFORM = "linux/amd64"


def command(*args, timeout=90, stdin=None):
    result = subprocess.run(args, stdin=stdin, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if result.returncode:
        # Report the failing operation without storing application logs, request
        # bodies, environment values or raw subprocess output in evidence.
        raise RuntimeError(f"{' '.join(map(str, args[:4]))} exited {result.returncode}")
    return result.stdout.decode().strip()


def docker(*args, **kwargs):
    return command(*DOCKER, *args, **kwargs)


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
            api = API()
        api.base = f"http://{address}:8080"
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            try:
                api.request("GET", "/api/auth/status")
                return api
            except (OSError, AssertionError):
                time.sleep(0.4)
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
        docker("cp", f"{asset_container}:/app/.", app)
        name = self.prefix + "-reboot"
        docker("run", "--pull", "never", "--detach", "--label", self.label, "--name", name,
               "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
               "--mount", f"type=bind,src={app},dst=/candidate,readonly",
               "--mount", f"type=bind,src={guest},dst=/guest.img,readonly",
               self.vm_runner, "--isolated-app-directory", "/candidate", "--image", "/guest.img",
               "--sha256", PIN["vm_image_sha256"], "--output", "/tmp/report")
        exit_code = docker("wait", name, timeout=600)
        evidence = self.output / "vm-report.json"
        docker("cp", f"{name}:/tmp/report/report.json", evidence)
        proof = json.loads(evidence.read_text())
        if exit_code != "0" or proof.get("status") != "passed":
            self.report["controlled_reboot_vm"]["status"] = proof.get("status", "failed")
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
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    with tempfile.TemporaryDirectory(prefix="slu-operational-qualification-") as work:
        qualification = Qualification(Path(work), output, args.candidate_source.resolve(), args.candidate_sha)
        report = qualification.report
        try:
            missing = [name for name in ("git", "docker", "curl") if not shutil.which(name)]
            if missing:
                report["reason"] = "Missing qualification tools: " + ", ".join(missing)
                return 2
            try:
                platform = docker("version", "--format", "{{.Server.Os}}/{{.Server.Arch}}", timeout=15)
            except (OSError, RuntimeError, subprocess.TimeoutExpired):
                report["reason"] = "Local Docker daemon is unavailable"
                return 2
            if platform != PLATFORM:
                report["reason"] = f"Qualification requires a {PLATFORM} Docker runner"
                return 2
            report["status"] = "failed"
            try:
                qualification.step("candidate_build", qualification.build_candidate)
                qualification.step("docker_upgrade", qualification.docker_upgrade)
                qualification.step("controlled_reboot_vm", qualification.controlled_reboot)
                report["status"] = "passed"
            except (Exception, KeyboardInterrupt) as error:
                report["reason"] = str(error) or type(error).__name__
            finally:
                if not qualification.cleanup():
                    report["status"] = "failed"
            return 0 if report["status"] == "passed" else 1
        finally:
            (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
            print(report["status"] + (": " + report["reason"] if "reason" in report else ""), flush=True)


if __name__ == "__main__":
    raise SystemExit(main())
