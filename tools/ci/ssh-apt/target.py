#!/usr/bin/env python3
"""Commands run exclusively inside the owned disposable target container."""
import fcntl
import gzip
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

PACKAGE = "slu-ci-fixture"
STATE = Path("/var/lib/slu-fixture")


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def build():
    for version in ("1.0", "2.0", "3.0"):
        tree = Path("/tmp/slu-package")
        shutil.rmtree(tree, ignore_errors=True)
        (tree / "DEBIAN").mkdir(parents=True)
        (tree / "usr/share/slu-ci-fixture").mkdir(parents=True)
        (tree / "usr/share/slu-ci-fixture/version").write_text(version + "\n")
        (tree / "DEBIAN/control").write_text(
            f"Package: {PACKAGE}\nVersion: {version}\nArchitecture: all\n"
            "Maintainer: SLU CI <ci@example.invalid>\nDescription: Harmless isolated APT fixture\n")
        script = tree / "DEBIAN/postinst"
        script.write_text("""#!/bin/sh
set -eu
mkdir -p /var/lib/slu-fixture
printf '%s\\n' "$1" >> /var/lib/slu-fixture/configure-attempts
if [ -e /run/slu-hold-mutation ]; then
    touch /run/slu-mutation-started
    tries=0
    while [ -e /run/slu-hold-mutation ] && [ "$tries" -lt 60 ]; do
        sleep 1; tries=$((tries+1))
    done
fi
cp /usr/share/slu-ci-fixture/version /var/lib/slu-fixture/version
""")
        script.chmod(0o755)
        for path in tree.rglob("*"):
            os.utime(path, (1700000000, 1700000000))
        run("dpkg-deb", "--root-owner-group", "--build", str(tree), f"/srv/slu-packages/{version}.deb",
            env={**os.environ, "SOURCE_DATE_EPOCH": "1700000000"})
    shutil.rmtree(tree)


def publish(version):
    assert version in ("1.0", "2.0", "3.0")
    shutil.copyfile(f"/srv/slu-packages/{version}.deb", "/srv/slu-repo/fixture.deb")
    result = run("dpkg-scanpackages", ".", "/dev/null", cwd="/srv/slu-repo", stdout=subprocess.PIPE)
    Path("/srv/slu-repo/Packages").write_bytes(result.stdout)
    Path("/srv/slu-repo/Packages.gz").write_bytes(gzip.compress(result.stdout, mtime=0))
    release = "Origin: SLU-TEST-ONLY\nLabel: Local fixture (not official)\nSuite: fixture\nCodename: fixture\nDate: Tue, 14 Nov 2023 22:13:20 UTC\nArchitectures: all\nDescription: Disposable fixture repository\nSHA256:\n"
    for name in ("Packages", "Packages.gz"):
        content = Path("/srv/slu-repo", name).read_bytes()
        release += f" {hashlib.sha256(content).hexdigest()} {len(content)} {name}\n"
    Path("/srv/slu-repo/Release").write_text(release)


def reset():
    for path in ("/run/slu-hold-mutation", "/run/slu-mutation-started"):
        Path(path).unlink(missing_ok=True)
    run("dpkg", "--force-downgrade", "-i", "/srv/slu-packages/1.0.deb", stdout=subprocess.DEVNULL)
    (STATE / "configure-attempts").write_text("")
    publish("2.0")
    run("apt-get", "update", stdout=subprocess.DEVNULL)


def start():
    token = os.environ["SLU_FIXTURE_OWNER"]
    assert len(token) == 32 and all(c in "0123456789abcdef" for c in token)
    Path("/run/slu-fixture-owner").write_text(token)
    run("ssh-keygen", "-A", stdout=subprocess.DEVNULL)
    reset()
    subprocess.Popen(["python3", "-m", "http.server", "18080", "--bind", "127.0.0.1", "--directory", str(STATE)],
                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    os.execv("/usr/sbin/sshd", ["/usr/sbin/sshd", "-D", "-e"])


if __name__ == "__main__":
    action = sys.argv[1]
    if action == "build": build()
    elif action == "start": start()
    elif action == "reset": reset()
    elif action == "publish": publish(sys.argv[2])
    elif action == "lock":
        # Open and lock the actual APT frontend lock, rather than emulating its output.
        with open("/var/lib/dpkg/lock-frontend", "a") as lock:
            fcntl.lockf(lock, fcntl.LOCK_EX)
            Path("/run/slu-lock-held").touch()
            time.sleep(90)
    elif action == "drop-ssh":
        # Kill established sshd session workers only; PID1 remains available for repair.
        import re
        for p in Path("/proc").iterdir():
            if p.name.isdigit():
                try:
                    command = (p / "cmdline").read_bytes().replace(b"\0", b" ").decode()
                    if re.match(r"^sshd(?:-session)?: (?:root|operator)@notty", command):
                        os.kill(int(p.name), 9)
                except (FileNotFoundError, ProcessLookupError, PermissionError):
                    pass
    else: raise SystemExit("unknown fixture operation")
