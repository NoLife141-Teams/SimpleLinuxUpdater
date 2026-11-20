#!/usr/bin/env python3
"""Exercise the native Windows distribution on a disposable NTFS installation."""

import argparse
import base64
import concurrent.futures
import contextlib
import json
import os
from pathlib import Path
import platform
import secrets
import socket
import subprocess
import tempfile
import time

from scenario import API, require


USERS_SID = "*S-1-5-32-545"
SYSTEM_SID = "*S-1-5-18"
ADMINISTRATORS_SID = "*S-1-5-32-544"
ARCHIVED = "archived-host"
CURRENT = "pre-restore-host"
PHASES = (
    "native_ntfs", "build", "export", "verify", "private_passphrase",
    "restore_in_isolation", "restart_after_restore", "interruption", "recovery_acl",
    "offline_recovery", "restart_after_recovery",
)
CHECKPOINT = "SLU_WINDOWS_RESTORE_TEST_CHECKPOINT"


def command(args, *, cwd=None, env=None, timeout=90):
    return subprocess.run(
        args, cwd=cwd, env=env, text=True, capture_output=True, timeout=timeout,
    )


def checked(args, label, **options):
    result = command(args, **options)
    require(result.returncode == 0, label + f" failed ({result.returncode})")
    return result.stdout.strip()


def powershell(code, label):
    return checked(["powershell.exe", "-NoProfile", "-NonInteractive", "-Command", code], label)


def configure_acl(path, user):
    checked(["icacls.exe", str(path), "/inheritance:r",
             "/grant:r", user + ":(OI)(CI)F", SYSTEM_SID + ":(OI)(CI)F",
             ADMINISTRATORS_SID + ":(OI)(CI)F"], "protect fixture directory")


def open_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def fixture_key(name):
    blob = b"\0\0\0\x0bssh-ed25519\0\0\0\x20" + secrets.token_bytes(32)
    return f"{name}.example.test ssh-ed25519 {base64.b64encode(blob).decode('ascii')}\n"


def upload(api, archive, phrase, *, restore=False):
    if not restore:
        return api.upload("/api/backup/verify", archive, phrase)
    boundary = "windows-fixture-" + secrets.token_hex(12)
    body = (
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"fixture.slubkp\"\r\n"
        "Content-Type: application/octet-stream\r\n\r\n"
    ).encode() + archive
    body += (f"\r\n--{boundary}\r\nContent-Disposition: form-data; name=\"passphrase\"\r\n\r\n{phrase}"
             f"\r\n--{boundary}\r\nContent-Disposition: form-data; name=\"isolate\"\r\n\r\ntrue"
             f"\r\n--{boundary}--\r\n").encode()
    return api.request("POST", "/api/backup/restore", raw=body, mime=f"multipart/form-data; boundary={boundary}")


class Installation:
    def __init__(self, source, directory):
        self.source = source
        self.directory = directory
        self.directory.mkdir()
        self.db = directory / "servers.db"
        self.trust = directory / "known_hosts"
        self.env = {k: v for k, v in os.environ.items()
                    if not k.startswith("DEBIAN_UPDATER_") and k != CHECKPOINT}
        self.env.update(
            DEBIAN_UPDATER_DB_PATH=str(self.db),
            DEBIAN_UPDATER_KNOWN_HOSTS=str(self.trust),
            DEBIAN_UPDATER_HOST_FACTS_AUTO_REFRESH_ENABLED="false",
            DEBIAN_UPDATER_SESSION_COOKIE_SECURE="false",
            TMPDIR=str(directory),
            TEMP=str(directory),
            TMP=str(directory),
            GIN_MODE="release",
        )
        self.credentials = {"username": "windows-fixture", "password": secrets.token_urlsafe(32) + "Aa1!"}
        self.process = None

    @contextlib.contextmanager
    def running(self, binary, *, recovery=False, checkpoint=None):
        port = open_port()
        env = {**self.env, "DEBIAN_UPDATER_LISTEN_ADDR": f"127.0.0.1:{port}"}
        if checkpoint:
            env[CHECKPOINT] = str(checkpoint)
        self.process = subprocess.Popen(
            [str(binary)], cwd=self.source, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        try:
            api = API(follow_redirects=False)
            api.base = f"http://127.0.0.1:{port}"
            deadline = time.monotonic() + 30
            expected = "recovery_required" if recovery else "ready"
            while time.monotonic() < deadline:
                require(self.process.poll() is None, "Windows executable exited before readiness")
                try:
                    ready = api.request("GET", "/readyz", allowed=(200, 503), timeout=2)
                    if ready.get("status") == expected:
                        yield api
                        return
                except OSError:
                    pass
                time.sleep(0.2)
            raise AssertionError("Windows executable did not reach " + expected)
        finally:
            self.stop()

    def stop(self):
        if self.process is not None and self.process.poll() is None:
            self.process.kill()
            self.process.wait(timeout=10)

    def init(self, api, name, trust):
        api.request("POST", "/api/auth/setup", self.credentials)
        api.request("POST", "/api/auth/login", self.credentials)
        self.trust.write_text(trust, encoding="ascii", newline="\n")
        api.request("POST", "/api/servers", {
            "name": name, "host": name + ".example.test", "port": 22,
            "user": "fixture", "pass": secrets.token_urlsafe(32), "disabled": True,
        }, allowed=(201,))

    def check(self, api, name, trust, credentials=None):
        api.request("POST", "/api/auth/login", credentials or self.credentials)
        hosts = api.request("GET", "/api/servers")
        require(len(hosts) == 1 and hosts[0]["name"] == name and hosts[0]["has_password"],
                "restored inventory or credentials did not match the fixture")
        require(hosts[0]["host_key_status"] == "trusted" and self.trust.read_text() == trust,
                "installed SSH trust did not match the fixture")

    def cli(self, binary, *args, success=True):
        result = command([str(binary), "backup", *args], cwd=self.source, env=self.env)
        require((result.returncode == 0) == success, f"backup {args[0]} returned {result.returncode}")
        try:
            return json.loads(result.stdout)
        except json.JSONDecodeError:
            raise AssertionError(f"backup {args[0]} did not produce a structured report") from None


def build(source, work):
    binary = work / "webserver.exe"
    checkpoint_binary = work / "checkpoint.exe"
    env = {**os.environ, "CGO_ENABLED": "0", "GOOS": "windows", "GOARCH": "amd64"}
    checked(["go", "build", "-trimpath", "-ldflags=-s -w", "-o", str(binary), "."],
            "build Windows distribution", cwd=source, env=env, timeout=480)
    original = source / "internal" / "backup" / "file_restore.go"
    text = original.read_text(encoding="utf-8")
    change = "\tif replacesKnownHosts {\n\t\tif err := fs.writeAtomicFileFromPath(knownHostsTarget"
    require(text.count(change) == 1, "could not identify SQLite replacement checkpoint")
    hook = """\tif checkpoint := os.Getenv("SLU_WINDOWS_RESTORE_TEST_CHECKPOINT"); checkpoint != "" {
\t\tif err := os.WriteFile(checkpoint, []byte("sqlite_replaced"), 0600); err != nil {
\t\t\treturn rollback(err)
\t\t}
\t\tselect {}
\t}
"""
    overlay_source = work / "file_restore.go"
    overlay_source.write_text(text.replace(change, hook + change), encoding="utf-8")
    overlay = work / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(original): str(overlay_source)}}), encoding="utf-8")
    checked(["go", "build", "-overlay", str(overlay), "-o", str(checkpoint_binary), "."],
            "build interruption fixture", cwd=source, env=env, timeout=240)
    return binary, checkpoint_binary


def terminate_at_replacement(api, archive, phrase, target, checkpoint):
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
        request = executor.submit(upload, api, archive, phrase, restore=True)
        try:
            deadline = time.monotonic() + 25
            while time.monotonic() < deadline:
                if checkpoint.exists() and checkpoint.read_text() == "sqlite_replaced":
                    break
                require(not request.done(), "restore stopped before its interruption checkpoint")
                time.sleep(0.1)
            else:
                raise AssertionError("SQLite replacement checkpoint was not reached")
        finally:
            target.stop()
        try:
            request.result(timeout=5)
        except OSError:
            pass


def qualify(source, work, report):
    require(platform.system() == "Windows" and platform.machine().upper() in ("AMD64", "X86_64"),
            "native Windows amd64 required")
    drive = work.drive.removesuffix(":")
    require(len(drive) == 1 and drive.isalpha(), "disposable filesystem needs a local volume")
    filesystem = powershell(f"(Get-Volume -DriveLetter '{drive}').FileSystem", "identify fixture filesystem")
    require(filesystem == "NTFS", "NTFS required")
    user = "*" + powershell("[Security.Principal.WindowsIdentity]::GetCurrent().User.Value", "identify fixture user")
    configure_acl(work, user)
    report["steps"]["native_ntfs"] = "passed"

    binary, checkpoint_binary = build(source, work)
    report["steps"]["build"] = "passed"
    origin = Installation(source, work / "origin")
    target = Installation(source, work / "target")
    interrupted = Installation(source, work / "interrupted")
    phrase = secrets.token_urlsafe(32)
    archived_trust, current_trust = fixture_key(ARCHIVED), fixture_key(CURRENT)
    with origin.running(binary) as api:
        origin.init(api, ARCHIVED, archived_trust)
        archive = api.request("POST", "/api/backup/export", {
            "passphrase": phrase, "include_known_hosts": True,
        }, binary=True)
    require(len(archive) > 100 and ARCHIVED.encode() not in archive, "encrypted archive was not generated")
    report["steps"]["export"] = "passed"
    archive_path = work / "fixture.slubkp"
    archive_path.write_bytes(archive)
    secret_path = work / "passphrase.txt"
    secret_path.write_text(phrase, encoding="utf-8")
    verify = ("verify", "--archive", str(archive_path), "--work-dir", str(work), "--passphrase-file", str(secret_path), "--json")
    require(origin.cli(binary, *verify).get("archive_verified"), "native offline verification failed")
    report["steps"]["verify"] = "passed"
    checked(["icacls.exe", str(secret_path), "/grant", USERS_SID + ":(R)"], "grant fixture read access")
    require(not origin.cli(binary, *verify, success=False).get("archive_verified"), "overbroad passphrase ACL was accepted")
    report["steps"]["private_passphrase"] = "passed"

    with target.running(binary) as api:
        target.init(api, CURRENT, current_trust)
        require(upload(api, archive, phrase).get("restore_ready"), "native archive preview was not ready")
        result = upload(api, archive, phrase, restore=True)
        require(result.get("restore_isolated") and result.get("known_hosts_restored"), "native isolated restore was incomplete")
        target.check(api, ARCHIVED, archived_trust, origin.credentials)
    report["steps"]["restore_in_isolation"] = "passed"
    with target.running(binary) as api:
        target.check(api, ARCHIVED, archived_trust, origin.credentials)
        require(api.request("GET", "/api/installation/health")["restore_isolation"] == "active",
                "isolated restore lost its isolation on restart")
    report["steps"]["restart_after_restore"] = "passed"

    checkpoint = work / "checkpoint.txt"
    with interrupted.running(checkpoint_binary, checkpoint=checkpoint) as api:
        interrupted.init(api, CURRENT, current_trust)
        terminate_at_replacement(api, archive, phrase, interrupted, checkpoint)
    with interrupted.running(binary, recovery=True):
        pass
    inspect = interrupted.cli(binary, "recover", "--inspect")
    require(inspect.get("recovery_required") and len(inspect["sets"]) == 1 and inspect["sets"][0]["ready"],
            "native interruption did not retain an applicable sealed recovery set")
    report["steps"]["interruption"] = "passed"

    set_id = inspect["sets"][0]["id"]
    rollback_path = interrupted.directory / set_id
    checked(["icacls.exe", str(rollback_path), "/grant", USERS_SID + ":(RX)"], "grant fixture rollback access")
    unsafe = interrupted.cli(binary, "recover", "--inspect")
    require(unsafe["sets"][0].get("blocker") == "invalid_manifest" and not unsafe["sets"][0]["ready"],
            "overbroad rollback ACL was accepted")
    checked(["icacls.exe", str(rollback_path), "/remove:g", USERS_SID], "restore private rollback ACL")
    report["steps"]["recovery_acl"] = "passed"
    applied = interrupted.cli(binary, "recover", "--apply", set_id)
    require(applied.get("applied") == set_id and not applied.get("recovery_required"), "native offline recovery was incomplete")
    report["steps"]["offline_recovery"] = "passed"
    with interrupted.running(binary) as api:
        interrupted.check(api, CURRENT, current_trust)
    report["steps"]["restart_after_recovery"] = "passed"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, default=Path.cwd())
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    report = {"platform": "windows/amd64", "filesystem": "NTFS", "status": "failed",
              "steps": dict.fromkeys(PHASES, "not_executed")}
    source = args.source.resolve()
    try:
        report["candidate_commit"] = checked(["git", "rev-parse", "HEAD"], "identify candidate commit", cwd=source)
        with tempfile.TemporaryDirectory(prefix="slu-native-restore-") as disposable:
            qualify(source, Path(disposable), report)
        report["status"] = "passed"
        return 0
    except (AssertionError, OSError, subprocess.TimeoutExpired) as error:
        report["failure"] = str(error) if isinstance(error, AssertionError) else type(error).__name__
        return 1
    finally:
        args.output.mkdir(parents=True, exist_ok=True)
        (args.output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(report, indent=2))


if __name__ == "__main__":
    raise SystemExit(main())
