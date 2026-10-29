#!/usr/bin/env python3
"""Controlled-reboot proof in a freshly created QEMU VM, never an existing host."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import subprocess
import tempfile
import time
import uuid

from run import API, DOCKER, IMAGES, LABEL, REPO, app_environment, command, copy_assets, loopback_port, require, wait_for_app


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", type=Path, help="local official Debian/Ubuntu amd64 cloud qcow2 image")
    parser.add_argument("--sha256", help="verified immutable SHA256 of the input cloud image")
    parser.add_argument("--output", type=Path, default=REPO / ".tmp-ssh-apt/vm")
    parser.add_argument("--check-prerequisites", action="store_true")
    args = parser.parse_args()
    tools = ("qemu-system-x86_64", "qemu-img", "cloud-localds", "ssh", "ssh-keygen", "go", "docker")
    missing = [name for name in tools if not shutil.which(name)]
    args.output.mkdir(parents=True, exist_ok=True)
    report = {"schema_version": 1, "scenario": "controlled-reboot-vm", "status": "not_executed"}
    if missing:
        report["reason"] = "Missing local VM prerequisites: " + ", ".join(missing)
        (args.output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        print(report["reason"])
        return 2
    if args.check_prerequisites:
        report["reason"] = "Prerequisites available, but no VM was booted in prerequisite-only mode"
        (args.output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        return 2
    try:
        require(args.image and args.sha256 and len(args.sha256) == 64, "provide a verified cloud image and SHA256")
        image = args.image.resolve(strict=True)
        with image.open("rb") as source:
            digest = hashlib.file_digest(source, "sha256").hexdigest()
        require(digest == args.sha256.lower(), "cloud image SHA256 mismatch")
    except (OSError, AssertionError) as error:
        report.update(status="failed", error=str(error))
        (args.output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        return 1
    owner = uuid.uuid4().hex
    work = Path(tempfile.mkdtemp(prefix="slu-reboot-vm-"))
    vm = None
    app_name = "slu-reboot-app-" + owner
    vm_log = app_log = None
    password = secrets.token_urlsafe(24) + "Aa7!"
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        command(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(work / "client")])
        public = (work / "client.pub").read_text().strip()
        # JSON is valid YAML. All values are generated locally; no deployment env is consumed.
        cloud_config = {"disable_root": False, "ssh_pwauth": False,
            "users": [{"name": "root", "lock_passwd": True, "ssh_authorized_keys": [public]}],
            "write_files": [{"path": "/var/lib/slu-fixture-owner", "permissions": "0600", "content": owner}]}
        (work / "user-data").write_text("#cloud-config\n" + json.dumps(cloud_config))
        (work / "meta-data").write_text(json.dumps({"instance-id": owner, "local-hostname": "slu-disposable"}))
        command(["cloud-localds", str(work / "seed.img"), str(work / "user-data"), str(work / "meta-data")])
        command(["qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", str(image), str(work / "overlay.qcow2")])
        ssh_port = loopback_port()
        vm_log = (work / "vm.log").open("wb")
        launch = ["qemu-system-x86_64", "-machine", "q35,accel=kvm:tcg", "-m", "1024", "-smp", "2",
            "-drive", f"file={work / 'overlay.qcow2'},if=virtio,format=qcow2",
            "-drive", f"file={work / 'seed.img'},if=virtio,format=raw,readonly=on",
            "-netdev", f"user,id=net0,hostfwd=tcp:127.0.0.1:{ssh_port}-:22", "-device", "virtio-net-pci,netdev=net0", "-nographic"]
        vm = subprocess.Popen(launch, stdout=vm_log, stderr=subprocess.STDOUT)

        def ssh(text):
            require(vm.poll() is None, "owned QEMU process exited")
            return command(["ssh", "-F", "/dev/null", "-i", str(work / "client"), "-p", str(ssh_port), "-o", "BatchMode=yes",
                "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "GlobalKnownHostsFile=/dev/null",
                "-o", "ProxyCommand=none", "-o", "ProxyJump=none", "-o", "ControlMaster=no", "-o", "ControlPath=none",
                "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + str(work / "control_known_hosts"),
                "-o", "ConnectTimeout=3", "root@127.0.0.1", text], timeout=10,
                env={"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(work)})

        end = time.monotonic() + 300
        while time.monotonic() < end:
            try:
                require(ssh("cat /var/lib/slu-fixture-owner").strip() == owner, "VM ownership marker mismatch")
                break
            except (RuntimeError, subprocess.TimeoutExpired): time.sleep(2)
        require(ssh("cat /var/lib/slu-fixture-owner").strip() == owner, "VM not ready")
        require(ssh("systemctl is-active ssh.service").strip() == "active", "VM SSH unit is not active")
        # Give the initial uptime a useful margin over the post-reboot SSH startup time.
        end = time.monotonic() + 90
        while float(ssh("cut -d' ' -f1 /proc/uptime").strip()) < 60:
            require(time.monotonic() < end, "VM uptime did not progress")
            time.sleep(2)
        binary = work / "webserver"
        architecture = command([*DOCKER, "version", "--format", "{{.Server.Arch}}"]).strip()
        require(architecture in ("amd64", "arm64"), "unsupported local Docker architecture")
        command(["go", "build", "-o", str(binary), "."], timeout=240,
                env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": architecture}, cwd=REPO)
        port = loopback_port(); api = API(port)
        (work / "known_hosts").touch(mode=0o600)
        copy_assets(work)
        env_args = []
        for key, value in app_environment(Path("/work"), port).items():
            env_args.extend(["--env", key + "=" + value])
        # Only the fresh VM's forwarded loopback socket is targeted. Host networking
        # lets the isolated app reach QEMU SLIRP without mounting host credentials.
        command([*DOCKER, "run", "-d", "--name", app_name, "--network", "host", "--label", LABEL + "=" + owner,
            "--user", f"{os.getuid()}:{os.getgid()}", "--mount", f"type=bind,src={work},dst=/work",
            "--workdir", "/work", "--entrypoint", "/work/webserver", *env_args, IMAGES["debian13"]])
        wait_for_app(api)
        api.request("POST", "/api/auth/setup", {"username": "fixture-admin", "password": password})
        endpoint = {"host": "127.0.0.1", "port": ssh_port}
        scan = api.request("POST", "/api/hostkeys/scan", endpoint)
        api.request("POST", "/api/hostkeys/trust", {**endpoint, "fingerprint_sha256": scan["fingerprint_sha256"]})
        api.request("POST", "/api/servers", {**endpoint, "name": "reboot-vm", "user": "root", "pass": password}, allowed=(201,))
        api.upload_key("reboot-vm", (work / "client").read_bytes())
        api.request("PUT", "/api/postcheck-profiles", {"revision": 0, "profiles": [{"id": "vm", "name": "Real systemd unit",
            "target_servers": ["reboot-vm"], "target_tags": [], "checks": [{"id": "ssh", "name": "SSH unit active",
                "kind": "systemd_active", "unit": "ssh.service", "mode": "blocking", "timeout_seconds": 10, "interval_seconds": 1}]}]})
        probe = api.request("POST", "/api/servers/reboot-vm/postchecks/test")
        require(probe["results"][0]["passed"], "real systemd postcheck failed")
        before = ssh("cat /proc/sys/kernel/random/boot_id; cut -d' ' -f1 /proc/uptime").splitlines()
        job_id = api.request("POST", "/api/reboot/reboot-vm", {"confirm": True})["job_id"]
        job, states = api.wait_job(job_id, lambda j: j["status"] in {"succeeded", "failed", "interrupted", "cancelled"}, seconds=240)
        require(job["status"] == "succeeded", "controlled reboot job failed")
        after = ssh("cat /proc/sys/kernel/random/boot_id; cut -d' ' -f1 /proc/uptime").splitlines()
        require(before[0] != after[0] and float(after[1]) < float(before[1]), "reboot did not change boot identity and reset uptime")
        end = time.monotonic() + 5
        while True:
            audits = api.request("GET", "/api/audit-events?action=reboot.complete&target_name=reboot-vm")["items"]
            if audits or time.monotonic() >= end: break
            time.sleep(.1)
        require(audits and audits[0]["status"] == "success", "reboot completion audit missing")
        report.update(status="passed", image_sha256=digest, boot_id_before=before[0], boot_id_after=after[0],
            uptime_before=float(before[1]), uptime_after=float(after[1]), job_id=job_id, job_states=states, audit_id=audits[0]["id"], systemd_check="passed")
    except BaseException as error:
        report.update(status="failed", error=str(error).replace(password, "[REDACTED]"))
    finally:
        cleanup_errors = []
        for process in (vm,):
            if process is not None and process.poll() is None:
                try:
                    process.terminate()
                    try: process.wait(timeout=10)
                    except subprocess.TimeoutExpired: process.kill(); process.wait(timeout=5)
                except Exception as error: cleanup_errors.append(str(error))
        try:
            label = command([*DOCKER, "container", "inspect", "--format", '{{index .Config.Labels "' + LABEL + '"}}', app_name]).strip()
            require(label == owner, "refusing cleanup of unowned application container")
            command([*DOCKER, "rm", "-f", app_name])
        except RuntimeError as error:
            if "no such" not in str(error).lower(): cleanup_errors.append(str(error))
        except Exception as error: cleanup_errors.append(str(error))
        for handle in (app_log, vm_log):
            try:
                if handle: handle.close()
            except Exception as error: cleanup_errors.append(str(error))
        try:
            if report["status"] != "passed" and (work / "vm.log").exists():
                text = (work / "vm.log").read_text(errors="replace")[-200000:]
                for sensitive in (password, (work / "client").read_text() if (work / "client").exists() else ""):
                    if sensitive: text = text.replace(sensitive, "[REDACTED]")
                (args.output / "vm.log").write_text(text)
        except Exception as error: cleanup_errors.append(str(error))
        try: shutil.rmtree(work)
        except OSError as error: cleanup_errors.append(str(error))
        report["cleanup"] = {"status": "failed" if cleanup_errors else "passed", "errors": cleanup_errors}
        (args.output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    return 0 if report["status"] == "passed" and not cleanup_errors else 1


if __name__ == "__main__": raise SystemExit(main())
