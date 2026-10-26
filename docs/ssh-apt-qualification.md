# Real SSH and APT qualification

Run the application against disposable Debian/Ubuntu targets, using its real HTTP API, OpenSSH transport, host-key trust and APT commands:

```bash
python3 tools/ci/ssh-apt/run.py --distro debian13 --output .tmp-ssh-apt/debian13
python3 tools/ci/ssh-apt/run.py --distro ubuntu2404 --output .tmp-ssh-apt/ubuntu2404
```

Requirements: Linux, Python 3.11+, Go from `go.mod`, Git, `ssh-keygen`, and a local Docker engine accessible at `/var/run/docker.sock`. The client explicitly selects this UNIX socket; a personal `DOCKER_HOST` or Docker context cannot redirect the harness to a remote engine. Initial image construction needs access to the public distribution package repositories. The run creates its own resources; it never accepts an existing SSH host, database, Docker resource, SSH key or inventory as a target.

Each run builds a static application binary and a target image from the digest in `tools/ci/ssh-apt/images.json`. The application and target run on a uniquely named bridge without outbound masquerading. Only the application's API is published, on an allocated `127.0.0.1` port; SSH is private to that network. The app runs as the invoking user's UID (non-root in CI) with only a generated private working directory mounted. Its environment is constructed from an allowlist. Public assets are copied from Git's tracked file list, excluding local prototypes and ignored runtime files. The app cannot see the host's `/data`, home, legacy inventory, SSH agent or deployment settings.

The disposable SSH target receives `SYS_PTRACE` so older `fuser` versions can inspect the operator's process descriptors during the production APT lock probe. It retains its private PID namespace and has no host mounts; the application container receives no added capability. Without this target permission, Debian 12 and Ubuntu 22.04 report unreadable descriptors and correctly block the operator update before discovery.

Host keys, an SSH client key, account password, database, encryption settings and `known_hosts` are generated for each run. The API-scanned key and fingerprint are compared against the owned target before trust is accepted. Target-changing control commands require both a Docker ownership label and the generated marker inside the target. Cleanup checks labels and only removes the run's named containers, bridge and tagged image, including resources created before a client error. It runs after success, failure, SIGINT and SIGTERM. If the entire harness is killed with SIGKILL, automatic cleanup cannot execute; inspect the ownership-labelled resources instead of pruning unrelated Docker resources.

## Scenarios and evidence

The local repository is deliberately **not an official distribution repository**. It contains only the harmless `slu-ci-fixture` package, versions `1.0`, `2.0` and `3.0`. Package source timestamps, control metadata, archive owner, gzip timestamp and repository metadata are fixed. The target's public APT sources are removed after bootstrap. Scenario assertions reject any pending package other than this fixture; application upgrades cannot inadvertently upgrade system packages from public repositories. Bootstrap tool versions are recorded in the report because public security package updates can change the image's installed tooling between builds.

Every container distribution executes:

- Root update using a generated per-Server SSH private key and operator update using the production typed helper. The helper is installed by the application's sudoers API; the fixture's temporary bootstrap sudo permission is then removed.
- Discovery and pending approval without changing the installed fixture version; obsolete approval identity rejection; accepted approval generation persisted in the job and audit.
- Real fixture upgrade, HTTP application post-check, final installed version, job transitions, actor, approved packages and audit facts.
- A malformed helper permission and an actual frontend package-manager lock, both preventing unauthorized mutation.
- A new repository candidate published during approval, requiring a refreshed plan and generation before mutation.
- An SSH session killed while the fixture's real dpkg `postinst` is running, one mutation attempt, `reconciliation_required`, refusal of another update, and explicit repair through the application after locks settle.
- A changed real SSH host key, refused by the application's existing trust store before mutation.

`report.json` records each scenario as `passed`, `failed` or `not_executed`, with timings and safe evidence. A failure prevents later scenarios from running and records them as `not_executed`, rather than treating them as successful. Evidence collection errors, including log timeouts or output write failures, are recorded without interrupting resource removal. Cleanup errors fail the command. `app.log` and failure details are bounded and scrubbed of the generated password/private key; database files, encryption settings, private keys, cookies and cloud-init seeds are never copied to the output directory. CI retains these reports and logs for one day, including on failure.

Systemd service health and controlled reboot are explicitly `not_executed` by the container harness. PID1 is OpenSSH, and Docker shares the host's kernel and uptime. The real systemd client remains present, its unavailable observation is a visible warning, and no fake `systemctl` is installed. Official-provenance/CVE qualification is a separate test concern; the local test repository cannot establish it.

## CI matrices and updates

Normal PR/push CI qualifies Debian 13 and Ubuntu 24.04. Relevant Go, template/static, harness and workflow paths select the matrix; `ci-required` rejects failed, cancelled or unexpectedly skipped qualification. The jobs use no deployment secrets and run for external PRs with read-only repository permissions. The independent `Extended SSH and APT qualification` workflow runs weekly or manually across Debian 12/13 and Ubuntu 22.04/24.04/26.04, matching the distribution coverage advertised by the application.

Image tags identify the intended OS release; immutable manifest digests select the actual bytes. Update `images.json` in a reviewed PR after resolving the official Docker Library digest, then run the two PR distributions and the expanded manual workflow. Changes to bootstrap SSH/APT tooling are visible in the recorded package versions. Do not silently replace a digest with a floating tag.

Harness admission, partial-create cleanup, environment isolation, local daemon selection, report states and VM prerequisite/hash rejection have fast tests:

```bash
python3 -m unittest discover -s tools/ci/ssh-apt -p 'test_*.py' -v
go test -run 'TestCIRequired|TestCIPathFilters|TestSSHAPTHarnessSafety' .
```

## Controlled reboot in a fresh VM

`vm-reboot.py` creates a private qcow2 overlay, generated cloud-init seed/SSH key, QEMU SLIRP network with loopback SSH forwarding, and a separate isolated app container. It accepts only an explicit local cloud image with a verified SHA256, never an existing VM or network target. The input image is read as a backing file and is not modified. It checks the generated VM marker, real `ssh.service` status through the application, controlled reboot job/audit, a changed kernel boot ID and reduced `/proc/uptime`. A container restart cannot satisfy these assertions.

Install QEMU, `qemu-utils`, `cloud-image-utils`, OpenSSH client, Docker and Go on a disposable Linux runner. KVM is preferred; QEMU can fall back to TCG. Check prerequisites without booting anything:

```bash
python3 tools/ci/ssh-apt/vm-reboot.py --check-prerequisites --output .tmp-ssh-apt/vm-prerequisites
```

Exit code `2` and `status: not_executed` mean the VM was not run. A real failed qualification returns `1`; only a completed VM proof returns `0`. This implementation's local runner has no QEMU, `qemu-img`, `cloud-localds` or `/dev/kvm`, so reboot/systemd qualification was **not executed**. The committed prerequisite report records that limit. Hosted PR container jobs do not claim VM coverage.

For a reproducible Ubuntu example, use the [official Noble release dated 20261017](https://cloud-images.ubuntu.com/releases/noble/release-20261017/). Its [published SHA256SUMS](https://cloud-images.ubuntu.com/releases/noble/release-20261017/SHA256SUMS) identifies the amd64 qcow2 image:

```bash
mkdir -p .tmp-ssh-apt/cloud
curl --fail --location --output .tmp-ssh-apt/cloud/ubuntu-24.04.img \
  https://cloud-images.ubuntu.com/releases/noble/release-20261017/ubuntu-24.04-server-cloudimg-amd64.img
python3 tools/ci/ssh-apt/vm-reboot.py \
  --image .tmp-ssh-apt/cloud/ubuntu-24.04.img \
  --sha256 04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94 \
  --output .tmp-ssh-apt/vm
```

Retain `report.json` from the VM run. The VM is terminated and its overlay, seed, private keys and temporary app data are deleted on exit; the downloaded backing image remains available for another fresh run. New cloud images require an explicitly reviewed URL/checksum update. No VM execution is implied by the presence of this command or its prerequisite tests.
