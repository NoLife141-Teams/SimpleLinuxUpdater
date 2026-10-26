#!/usr/bin/env python3
"""Qualify local account recovery on a fresh Docker volume only."""
import argparse
import http.cookiejar
import json
import secrets
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

LABEL = "io.simplelinuxupdater.recovery-smoke-owner"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image")
    parser.add_argument("platform", choices=("linux/amd64", "linux/arm64"))
    args = parser.parse_args()
    owner = uuid.uuid4().hex
    volume = "slu-recovery-" + owner
    container = volume + "-app"
    reset = volume + "-reset"
    password = secrets.token_urlsafe(24) + "Aa7!"
    recovered = secrets.token_urlsafe(24) + "Bb8!"
    cookiejar = http.cookiejar.CookieJar()
    api = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                    urllib.request.HTTPCookieProcessor(cookiejar))
    base = ""
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))

    def docker(*words, data=None, success=True):
        result = subprocess.run(["docker", "--host=unix:///var/run/docker.sock", *words], input=data,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=120)
        text = result.stdout.decode(errors="replace")
        assert password not in text and recovered not in text, "smoke output disclosed password"
        if success:
            assert result.returncode == 0, "Docker smoke command failed: " + text
        return result

    def request(method, path, data=None, client=None, status=200):
        body = json.dumps(data).encode() if data is not None else None
        req = urllib.request.Request(base + path, body, method=method,
                                     headers={"Origin": base, "Content-Type": "application/json"})
        try:
            response = (client or api).open(req, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        assert response.code == status, "unexpected HTTP status for " + path
        return json.load(response)

    def start():
        nonlocal base
        docker("run", "-d", "--name", container, "--platform", args.platform,
               "--label", LABEL + "=" + owner, "-v", volume + ":/data",
               "-p", "127.0.0.1::8080", args.image)
        port = docker("port", container, "8080/tcp").stdout.decode().strip().split(":")[-1]
        base = "http://127.0.0.1:" + port
        end = time.monotonic() + 45
        while True:
            try:
                request("GET", "/api/auth/status")
                break
            except (OSError, AssertionError):
                assert time.monotonic() < end, "app readiness timed out"
                time.sleep(.1)

    def snapshot():
        return docker("exec", container, "sh", "-ec",
                      "sha256sum /data/config.json /data/known_hosts").stdout

    def reset_command():
        return docker("run", "--rm", "-i", "--name", reset, "--platform", args.platform,
                      "--label", LABEL + "=" + owner, "-v", volume + ":/data", args.image,
                      "./webserver", "admin", "reset-password", "--username", "recovery-admin",
                      "--password-stdin", data=(recovered + "\n" + recovered + "\n").encode(), success=False)

    try:
        docker("volume", "create", "--label", LABEL + "=" + owner, volume)
        start()
        request("POST", "/api/auth/setup", {"username": "recovery-admin", "password": password})
        # Keep a client authenticated before reset, including its old cookie.
        request("POST", "/api/auth/login", {"username": "recovery-admin", "password": password})
        assert cookiejar, "old session not captured"
        request("POST", "/api/servers", {"name": "preserved-fixture", "host": "fixture.invalid",
                "port": 22, "user": "operator", "pass": "Fixture-SSH-Secret123!", "disabled": True}, status=201)
        docker("exec", "--user", "app", container, "sh", "-ec", "printf fixture > /data/known_hosts")
        inventory = request("GET", "/api/servers")
        before = snapshot()
        refused = reset_command()
        assert refused.returncode != 0 and b"already in use" in refused.stdout, "active server did not refuse reset"
        # This also proves that entrypoint ownership repair preserves the lock inode.
        request("POST", "/api/auth/login", {"username": "recovery-admin", "password": password})
        docker("stop", "-t", "40", container)
        docker("rm", container)
        result = reset_command()
        assert result.returncode == 0 and b"recovery committed" in result.stdout, "offline recovery failed"
        start()
        state = request("GET", "/api/auth/status")
        assert state["setup_required"] is False and state["authenticated"] is False, "old session/setup state invalid"
        assert before == snapshot(), "encryption config or known_hosts changed"
        fresh = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                    urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        request("POST", "/api/auth/login", {"username": "recovery-admin", "password": password}, client=fresh, status=401)
        request("POST", "/api/auth/login", {"username": "recovery-admin", "password": recovered}, client=fresh)
        assert request("GET", "/api/servers", client=fresh) == inventory, "inventory changed"
        events = request("GET", "/api/audit-events?action=auth.password.recover", client=fresh)["items"]
        assert len(events) == 1 and events[0]["actor"] == "local-recovery", "local recovery audit missing"
        assert password not in json.dumps(events) and recovered not in json.dumps(events), "audit leaked secret"
        print("Local recovery Docker smoke passed: lock, reset, old-session revocation, login and data preservation")
    finally:
        failures = []
        for kind, name in (("container", container), ("container", reset), ("volume", volume)):
            try:
                result = docker(kind, "inspect", "--format",
                                '{{index .Config.Labels "' + LABEL + '"}}' if kind == "container" else '{{index .Labels "' + LABEL + '"}}', name, success=False)
                if result.returncode == 0:
                    if result.stdout.decode().strip() == owner:
                        deleted = docker(kind, "rm", "-f", name, success=False)
                        if deleted.returncode: failures.append(name)
                    else: failures.append(name)
                elif b"no such" not in result.stdout.lower() and b"not found" not in result.stdout.lower(): failures.append(name)
            except Exception as error:
                failures.append(name + ": " + type(error).__name__)
        if failures:
            # Preserve the original qualification failure while also making a
            # cleanup-only failure fail the job. Never stop at the first resource.
            message = "smoke resource cleanup failed: " + ", ".join(failures)
            if sys.exc_info()[0] is not None:
                print(message, file=sys.stderr)
            else:
                raise AssertionError(message)


if __name__ == "__main__": main()
