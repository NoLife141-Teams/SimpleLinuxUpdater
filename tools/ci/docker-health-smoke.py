#!/usr/bin/env python3
"""Exercise Docker HEALTHCHECK using only isolated, disposable installations."""
import json
import subprocess
import sys
import time
import uuid


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def run_case(image, label, address, nonroot=False, platform=None):
    name = "slu-health-smoke-" + uuid.uuid4().hex[:12]
    args = ["run", "--detach", "--rm", "--name", name, "--network", "none",
            "--health-interval=1s", "--health-start-period=0s", "--health-retries=2",
            "--env", "DEBIAN_UPDATER_HOST_FACTS_AUTO_REFRESH_ENABLED=false",
            "--env", "DEBIAN_UPDATER_VULNERABILITY_SCAN_ENABLED=false"]
    if platform:
        args += ["--platform", platform]
    if address:
        args += ["--env", "DEBIAN_UPDATER_LISTEN_ADDR=" + address]
    if nonroot:
        args += ["--user", "app"]
    docker(*args, image)
    try:
        deadline = time.monotonic() + 15
        state = None
        while time.monotonic() < deadline:
            state = json.loads(docker("inspect", name))[0]
            if state["State"].get("Health", {}).get("Status") == "healthy":
                break
            if not state["State"]["Running"]:
                raise AssertionError(docker("logs", name))
            time.sleep(0.25)
        assert state["State"]["Health"]["Status"] == "healthy", state["State"]
        assert state["HostConfig"]["RestartPolicy"]["Name"] == "no"
        assert docker("exec", "--user", "app", name, "id", "-u") != "0"
        # Invalid installation/deployment settings must be irrelevant to the
        # HTTP-only command; even the live installation lock cannot obstruct it.
        docker("exec", "--user", "app",
               "--env", "DEBIAN_UPDATER_DB_PATH=/proc/forbidden/no.db",
               "--env", "DEBIAN_UPDATER_BACKUP_DIR=invalid",
               "--env", "DEBIAN_UPDATER_VULNERABILITY_SCAN_ENABLED=invalid",
               "--env", "HTTP_PROXY=http://127.0.0.1:1",
               "--env", "HTTPS_PROXY=http://127.0.0.1:1",
               name, "/app/webserver", "healthcheck")
        print(json.dumps({"case": label, "status": "passed", "container": name,
                          "listener": address or "image default", "nonroot": True,
                          "docker_health": state["State"]["Health"]["Status"]}), flush=True)
    finally:
        subprocess.run(["docker", "rm", "--force", "--volumes", name],
                       check=False, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    if len(sys.argv) not in (2, 3):
        raise SystemExit("usage: docker-health-smoke.py IMAGE [PLATFORM]")
    platform = sys.argv[2] if len(sys.argv) == 3 else None
    for case in [("default", "", False), ("custom-ipv4", "0.0.0.0:18094", False),
                 ("custom-ipv6", "[::]:18095", False),
                 ("nonroot-container", "127.0.0.1:18096", True)]:
        run_case(sys.argv[1], *case, platform=platform)
