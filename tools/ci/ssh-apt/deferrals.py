#!/usr/bin/env python3
"""Offline real-APT proof; shares #469's image allowlist, ownership and commands.

No SSH, host package changes, bind mounts, volumes or outbound network are used.
"""
import argparse
import hashlib
import json
from pathlib import Path
import time
import uuid

import run


def qualify(helper, output, distro):
    owner = uuid.uuid4().hex
    name = "slu-package-deferrals-473-" + owner
    run.require(distro in ("debian13", "ubuntu2404"), "unsupported deferral fixture distribution")
    image = run.IMAGES[distro]
    records = []
    administrator_state_verified = False

    def docker(*args, data=None):
        return run.command([*run.DOCKER, *args], data=data, timeout=120)

    def owned():
        return docker("inspect", "--format", '{{index .Config.Labels "' + run.LABEL + '"}}', name).strip() == owner

    def target(*args, data=None):
        run.require(owned(), "refusing unowned target")
        run.require(docker("exec", name, "cat", "/run/slu-fixture-owner").strip() == owner, "marker mismatch")
        return docker("exec", "-i", name, *args, data=data)

    def versions():
        return target("dpkg-query", "-W", "-f=${binary:Package}=${Version}\n", "slu-*").splitlines()

    def guarded(scope, deferred, transaction, targets, *, removals=False, deadline=None):
        args = ["/usr/local/sbin/simplelinuxupdater-root-helper", "guarded-install", scope,
                str(deadline or int(time.time()) + 3600), str(len(deferred)), *deferred,
                str(len(transaction)), *transaction, "allow-removals" if removals else "no-remove", *targets]
        before = versions()
        try:
            text = target(*args)
            return {"status": "succeeded", "before": before, "after": versions(), "output": text}
        except RuntimeError as error:
            return {"status": "blocked", "before": before, "after": versions(), "output": str(error)}

    created = False
    cleanup = "not-created"
    try:
        # No user mounts, no Docker socket mount and no external networking.
        docker("run", "-d", "--name", name, "--label", run.LABEL + "=" + owner,
               "--network", "none", "--pids-limit", "128", "--memory", "512m",
               "--entrypoint", "/bin/sh", image, "-c",
               "printf '%s\\n' '" + owner + "' > /run/slu-fixture-owner; exec sleep infinity")
        created = True
        target("sh", "-c", "cat > /usr/local/sbin/simplelinuxupdater-root-helper; chmod 755 /usr/local/sbin/simplelinuxupdater-root-helper", data=helper)
        target("sh", "-s", data=(run.HERE / "deferrals-fixture.sh").read_bytes())
        native = target("cat", "/run/slu-native").strip()
        baseline = versions()
        for scope in ("all", "security", "security_kept_back", "full_upgrade"):
            # Reset only this fixture package in this disposable target.
            target("dpkg", "--force-downgrade", "-i", f"/tmp/slu-debs/slu-independent-1-{native}.deb")
            record = guarded(scope, ["slu-deferred:" + native], [f"slu-independent:{native}=2"], ["slu-independent:" + native])
            record["scenario"] = "independent-" + scope
            run.require(record["status"] == "succeeded" and f"slu-deferred=1" in record["after"] and "slu-independent=2" in record["after"], str(record))
            records.append(record)
        simulation = target("apt-get", "-s", "install", "slu-dependent")
        run.require("Inst slu-deferred" in simulation, "fixture did not force deferred dependency")
        blocked = guarded("full_upgrade", ["slu-deferred:" + native],
                          [f"slu-dependent:{native}=2", f"slu-deferred:{native}=2"], ["slu-dependent"])
        blocked.update(scenario="dependency-forces-deferred", simulation=simulation)
        run.require(blocked["status"] == "blocked" and blocked["before"] == blocked["after"] and "would change deferred" in blocked["output"], str(blocked))
        records.append(blocked)
        removal = guarded("full_upgrade", ["slu-deferred"], [f"slu-replacement:{native}=2", f"slu-deferred:{native}=-"], ["slu-replacement"], removals=True)
        removal["scenario"] = "implicit-removal-of-deferred"
        run.require(removal["status"] == "blocked" and removal["before"] == removal["after"] and "would change deferred" in removal["output"], str(removal))
        records.append(removal)
        safe_removal = guarded("full_upgrade", ["slu-deferred"], [f"slu-safe-replacement:{native}=2",f"slu-victim:{native}=-"], ["slu-safe-replacement"], removals=True)
        safe_removal["scenario"] = "confirmed-independent-removal"
        run.require(safe_removal["status"] == "succeeded" and "slu-deferred=1" in safe_removal["after"] and "slu-safe-replacement=2" in safe_removal["after"] and "slu-victim=1" not in safe_removal["after"], str(safe_removal))
        records.append(safe_removal)
        held = guarded("full_upgrade", ["slu-deferred"], [f"slu-admin-held:{native}=2"], ["slu-admin-held"])
        held["scenario"] = "administrator-hold-enforced"
        run.require(held["status"] == "blocked" and held["before"] == held["after"], str(held))
        records.append(held)
        multi = guarded("full_upgrade", ["slu-multi:i386"], [f"slu-multi:{native}=2", "slu-multi:i386=2"], ["slu-multi:" + native])
        multi["scenario"] = "multiarch-dependency"
        run.require(multi["status"] == "blocked" and multi["before"] == multi["after"] and "would change deferred" in multi["output"], str(multi))
        records.append(multi)
        expired = guarded("all", ["slu-deferred"], [f"slu-dependent:{native}=2"], ["slu-dependent"], deadline=int(time.time()) - 1)
        expired["scenario"] = "expired-rules"
        run.require(expired["status"] == "blocked" and expired["before"] == expired["after"] and "expired" in expired["output"], str(expired))
        records.append(expired)
        drift = guarded("full_upgrade", ["slu-multi:i386"], [f"slu-dependent:{native}=2"], ["slu-dependent"])
        drift["scenario"] = "unapproved-dependency-drift"
        run.require(drift["status"] == "blocked" and drift["before"] == drift["after"] and "changed since approval" in drift["output"], str(drift))
        records.append(drift)
        # Model a pin/candidate change after approval: the approved pinned
        # upgrade disappears, but APT can still apply the independent upgrade.
        target("dpkg", "--force-downgrade", "-i", f"/tmp/slu-debs/slu-independent-1-{native}.deb")
        subset = guarded("full_upgrade", ["slu-deferred"],
                         [f"slu-independent:{native}=2", f"slu-admin-pinned:{native}=2"],
                         ["slu-independent", "slu-admin-pinned"])
        subset["scenario"] = "approved-action-missing-after-pin-drift"
        records.append(subset)
        run.require(subset["status"] == "blocked" and subset["before"] == subset["after"] and "changed since approval" in subset["output"], str(subset))
        target("sh", "-c", "sha256sum -c /run/slu-pin-before && apt-mark showhold > /run/slu-holds-after && cmp /run/slu-holds-before /run/slu-holds-after")
        administrator_state_verified = True
        packages = target("dpkg-query", "-W", "-f=${Package}=${Version}\n", "apt", "dpkg").splitlines()
        transient = target("sh", "-c", "find /tmp -maxdepth 1 -name 'simplelinuxupdater-guard.*' -print")
        run.require(not transient.strip(), "helper temporary resources leaked")
    finally:
        # Recover by unique name after a partial Docker creation failure, too.
        try:
            if created or owned():
                run.require(owned(), "refusing cleanup of unowned container")
                docker("rm", "-f", name)
                cleanup = "removed-owned-container"
        except RuntimeError as error:
            if "no such" not in str(error).lower():
                raise
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(json.dumps({"schema_version": 1, "distribution": distro, "image": image,
            "helper_sha256": hashlib.sha256(helper).hexdigest(), "fixture_sha256": hashlib.sha256((run.HERE / "deferrals-fixture.sh").read_bytes()).hexdigest(), "container": name,
            "network": "none", "mounts": [], "cleanup": cleanup,
            "packages": locals().get("packages", []), "baseline": locals().get("baseline", []),
            "administrator_holds_and_pins": "preserved" if administrator_state_verified else "not-verified",
            "scenarios": records}, indent=2) + "\n")
    print(f"{len(records)} real APT scenarios passed; {cleanup}; {output}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--distro", choices=("debian13","ubuntu2404"), required=True)
    parser.add_argument("--helper", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    qualify(args.helper.read_bytes(), args.output, args.distro)
