#!/usr/bin/env python3
"""Bounded offline real-APT qualification of the source-generated protocol 3 helper."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import uuid

import run

SCENARIOS = tuple("independent-" + scope for scope in ("all", "security", "security_kept_back", "full_upgrade")) + (
    "dependency-forces-deferred", "implicit-removal-of-deferred", "confirmed-independent-removal",
    "administrator-hold-enforced", "multiarch-dependency", "expired-rules",
    "unapproved-dependency-drift", "approved-action-missing-after-pin-drift")
REASONS = {"dependency-forces-deferred": "would change deferred",
           "implicit-removal-of-deferred": "would change deferred", "multiarch-dependency": "would change deferred",
           "expired-rules": "expired", "unapproved-dependency-drift": "changed since approval",
           "approved-action-missing-after-pin-drift": "changed since approval"}
SOURCE = Path(os.environ.get("SLU_DEFERRALS_SOURCE", run.REPO)).resolve()


def source_identity():
    return run.command(["git", "rev-parse", "HEAD"], cwd=SOURCE).strip()


def generate_helper():
    # Always compile the production generator in the checked-out candidate.
    result = subprocess.run(["go", "run", "./tools/ci/package-deferral-helper"], cwd=SOURCE,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=180)
    run.require(result.returncode == 0, "helper generation failed: " + result.stderr.decode(errors="replace")[-5000:])
    run.require(result.stdout.startswith(b"#!/bin/sh\n"), "generator did not emit a shell helper")
    return result.stdout


def source_manifest():
    files = run.command(["git", "ls-files", "-z", "--", "go.mod", "go.sum", "internal/updates/",
                         "internal/deferrals/", "internal/helperprotocol/",
                         "tools/ci/package-deferral-helper/"], cwd=SOURCE).split("\0")
    return {name: hashlib.sha256((SOURCE / name).read_bytes()).hexdigest() for name in files if name}


def qualify(helper, output, distro, *, source_sha=None, helper_origin="source-generated"):
    run.require(distro in run.IMAGES, "unsupported deferral fixture distribution")
    owner = uuid.uuid4().hex
    name = "slu-package-deferrals-" + owner
    image = run.IMAGES[distro]
    report = {"schema_version": 2, "status": "failed", "exit_code": 1,
              "source_sha": source_sha or source_identity(), "distribution": distro, "image": image,
              "source_files_sha256": {}, "helper_origin": helper_origin,
              "architecture": "not_observed", "image_id": None,
              "helper_sha256": hashlib.sha256(helper).hexdigest(),
              "fixture_sha256": hashlib.sha256((run.HERE / "deferrals-fixture.sh").read_bytes()).hexdigest(),
              "container": name, "owner": owner, "network": "none", "mounts": [],
              "limits": {"seconds": 600, "memory": "512m", "pids": 128},
              "provenance": "synthetic local fixture; no official origin or CVE inference",
              "administrator_holds_and_pins": "not_verified", "scenarios": [],
              "cleanup": {"status": "not_created", "errors": []}}
    records = report["scenarios"]
    started = time.monotonic()

    def docker(*args, data=None, checked=True):
        remaining = 600 - (time.monotonic() - started)
        if remaining <= 0 and args[0] not in ("inspect", "rm"):
            raise TimeoutError("qualification exceeded 600-second budget")
        # Cleanup has its own budget after the qualification deadline or signal.
        timeout = 30 if args[0] in ("inspect", "rm") else min(120, max(1, remaining))
        result = subprocess.run([*run.DOCKER, *args], input=data, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, timeout=timeout)
        if checked and result.returncode:
            raise RuntimeError(f"docker {args[0]} failed ({result.returncode}): {result.stdout.decode(errors='replace')[-5000:]}")
        return result.stdout.decode(errors="replace") if checked else result

    def owned():
        result = docker("inspect", "--format", '{{index .Config.Labels "' + run.LABEL + '"}}', name, checked=False)
        if result.returncode:
            run.require("no such" in result.stdout.decode(errors="replace").lower(), "could not inspect target ownership")
            return False
        return result.stdout.decode().strip() == owner

    def target(*args, data=None, checked=True):
        run.require(owned(), "refusing unowned target")
        run.require(docker("exec", name, "cat", "/run/slu-fixture-owner").strip() == owner, "marker mismatch")
        return docker("exec", "-i", name, *args, data=data, checked=checked)

    def versions():
        return target("sh", "-c", "dpkg-query -W -f='${db:Status-Abbrev} ${binary:Package}=${Version}\\n' | sed -n 's/^ii  //p'").splitlines()

    def state():
        # Full installed state and dpkg log prove refusals precede dpkg mutation.
        return target("sh", "-c", "sha256sum /var/lib/dpkg/status; if test -f /var/log/dpkg.log; then sha256sum /var/log/dpkg.log; fi").splitlines()

    def guarded(scenario, scope, deferred, transaction, targets, *, removals=False, deadline=None):
        args = ["/usr/local/sbin/simplelinuxupdater-root-helper", "guarded-install", scope,
                str(deadline or int(time.time()) + 3600), str(len(deferred)), *deferred,
                str(len(transaction)), *transaction, "allow-removals" if removals else "no-remove", *targets]
        record = {"scenario": scenario, "status": "failed"}
        records.append(record)
        before, dpkg_before = versions(), state()
        administrator_before = target("sh", "-c", "sha256sum /etc/apt/preferences.d/administrator; apt-mark showhold")
        result = target(*args, checked=False)
        record.update({"outcome": "succeeded" if result.returncode == 0 else "blocked",
                "exit_code": result.returncode, "before": before, "after": versions(),
                "dpkg_before": dpkg_before, "dpkg_after": state(),
                "administrator_before": administrator_before,
                "administrator_after": target("sh", "-c", "sha256sum /etc/apt/preferences.d/administrator; apt-mark showhold"),
                "output": result.stdout.decode(errors="replace")[-12000:]})
        return record

    def verify(record):
        record["status"] = "failed"
        scenario = record["scenario"]
        run.require(record["administrator_before"] == record["administrator_after"], "administrator hold/pin changed: " + scenario)
        report["administrator_holds_and_pins"] = "preserved"
        positive = scenario.startswith("independent-") or scenario == "confirmed-independent-removal"
        if positive:
            before, after = set(record["before"]), set(record["after"])
            removed, added = before - after, after - before
            if scenario.startswith("independent-"):
                expected_removed, expected_added = {"slu-independent=1"}, {"slu-independent=2"}
            else:
                expected_removed, expected_added = {"slu-victim=1"}, {"slu-safe-replacement=2"}
            run.require(record["outcome"] == "succeeded" and removed == expected_removed and added == expected_added,
                        "unexpected fixture mutation: " + scenario)
        else:
            run.require(record["outcome"] == "blocked" and record["before"] == record["after"]
                        and record["dpkg_before"] == record["dpkg_after"], "guard did not prevent dpkg mutation: " + scenario)
            if scenario in REASONS:
                run.require(REASONS[scenario] in record["output"], "expected guard refusal absent: " + scenario)
            else:
                run.require("held packages" in record["output"], "administrator hold refusal absent")
        record["status"] = "passed"
        print(scenario + ": passed", flush=True)

    handlers = {}
    def interrupted(signum, frame):
        raise InterruptedError("qualification interrupted by signal " + str(signum))

    try:
        report["source_files_sha256"] = source_manifest()
        for sig in (signal.SIGTERM, signal.SIGINT):
            handlers[sig] = signal.signal(sig, interrupted)
        docker("run", "-d", "--name", name, "--label", run.LABEL + "=" + owner,
               "--network", "none", "--pids-limit", "128", "--memory", "512m",
               "--entrypoint", "/bin/sh", image, "-c",
               "printf '%s\\n' '" + owner + "' > /run/slu-fixture-owner; exec sleep infinity")
        report["image_id"] = docker("inspect", "--format", "{{.Image}}", name).strip()
        report["architecture"] = target("dpkg", "--print-architecture").strip()
        target("sh", "-c", "cat > /usr/local/sbin/simplelinuxupdater-root-helper; chmod 755 /usr/local/sbin/simplelinuxupdater-root-helper", data=helper)
        target("sh", "-s", data=(run.HERE / "deferrals-fixture.sh").read_bytes())
        native = target("cat", "/run/slu-native").strip()
        report["architecture"] = native
        report["baseline"] = versions()
        for scope in ("all", "security", "security_kept_back", "full_upgrade"):
            # Reset only this fixture package in this disposable target.
            target("dpkg", "--force-downgrade", "-i", f"/tmp/slu-debs/slu-independent-1-{native}.deb")
            record = guarded("independent-" + scope, scope, ["slu-deferred:" + native], [f"slu-independent:{native}=2"], ["slu-independent:" + native])
            verify(record)
        simulation = target("apt-get", "-s", "install", "slu-dependent")
        run.require("Inst slu-deferred" in simulation, "fixture did not force deferred dependency")
        blocked = guarded("dependency-forces-deferred", "full_upgrade", ["slu-deferred:" + native],
                          [f"slu-dependent:{native}=2", f"slu-deferred:{native}=2"], ["slu-dependent"])
        blocked.update(simulation=simulation)
        verify(blocked)
        removal = guarded("implicit-removal-of-deferred", "full_upgrade", ["slu-deferred"], [f"slu-replacement:{native}=2", f"slu-deferred:{native}=-"], ["slu-replacement"], removals=True)
        verify(removal)
        safe_removal = guarded("confirmed-independent-removal", "full_upgrade", ["slu-deferred"], [f"slu-safe-replacement:{native}=2",f"slu-victim:{native}=-"], ["slu-safe-replacement"], removals=True)
        verify(safe_removal)
        held = guarded("administrator-hold-enforced", "full_upgrade", ["slu-deferred"], [f"slu-admin-held:{native}=2"], ["slu-admin-held"])
        verify(held)
        multi = guarded("multiarch-dependency", "full_upgrade", ["slu-multi:i386"], [f"slu-multi:{native}=2", "slu-multi:i386=2"], ["slu-multi:" + native])
        verify(multi)
        expired = guarded("expired-rules", "all", ["slu-deferred"], [f"slu-dependent:{native}=2"], ["slu-dependent"], deadline=int(time.time()) - 1)
        verify(expired)
        drift = guarded("unapproved-dependency-drift", "full_upgrade", ["slu-multi:i386"], [f"slu-dependent:{native}=2"], ["slu-dependent"])
        verify(drift)
        # Model a pin/candidate change after approval: the approved pinned
        # upgrade disappears, but APT can still apply the independent upgrade.
        target("dpkg", "--force-downgrade", "-i", f"/tmp/slu-debs/slu-independent-1-{native}.deb")
        subset = guarded("approved-action-missing-after-pin-drift", "full_upgrade", ["slu-deferred"],
                         [f"slu-independent:{native}=2", f"slu-admin-pinned:{native}=2"],
                         ["slu-independent", "slu-admin-pinned"])
        verify(subset)
        target("sh", "-c", "sha256sum -c /run/slu-pin-before && apt-mark showhold > /run/slu-holds-after && cmp /run/slu-holds-before /run/slu-holds-after")
        report["administrator_holds_and_pins"] = "preserved"
        report["packages"] = target("dpkg-query", "-W", "-f=${Package}=${Version}\n", "apt", "dpkg").splitlines()
        transient = target("sh", "-c", "find /tmp -maxdepth 1 -name 'simplelinuxupdater-guard.*' -print")
        run.require(not transient.strip(), "helper temporary resources leaked")
        report.update(status="passed", exit_code=0)
    except (Exception, KeyboardInterrupt) as error:
        report["error"] = str(error)[-5000:]
    finally:
        # Inspect after partial creation too, never remove another owner's resource.
        try:
            if owned():
                docker("rm", "-f", name)
                report["cleanup"]["status"] = "passed"
            else:
                report["cleanup"]["status"] = "absent_or_unowned"
        except Exception as error:
            report["cleanup"] = {"status": "failed", "errors": [str(error)[-5000:]]}
            report.update(status="failed", exit_code=1)
        for sig, handler in handlers.items():
            signal.signal(sig, handler)
        executed = {record["scenario"] for record in records}
        records.extend({"scenario": scenario, "status": "not_executed"} for scenario in SCENARIOS if scenario not in executed)
        report["duration_seconds"] = round(time.monotonic() - started, 3)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(json.dumps(report, indent=2) + "\n")
    print(f"{distro}: {report['status']}; exit={report['exit_code']}; {output}", flush=True)
    return report["exit_code"]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--distro", choices=tuple(run.IMAGES), required=True)
    parser.add_argument("--helper", type=Path, help="explicit helper override for disposable negative controls")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        source = source_identity()
        helper = args.helper.read_bytes() if args.helper else generate_helper()
    except Exception as error:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps({"schema_version": 2, "status": "failed", "exit_code": 1,
            "source_sha": locals().get("source"), "helper_sha256": None,
            "image": run.IMAGES[args.distro], "architecture": "not_observed",
            "distribution": args.distro, "error": str(error)[-5000:],
            "scenarios": [{"scenario": name, "status": "not_executed"} for name in SCENARIOS]}) + "\n")
        return 1
    return qualify(helper, args.output, args.distro, source_sha=source,
                   helper_origin="explicit-override" if args.helper else "source-generated")


if __name__ == "__main__":
    sys.exit(main())
