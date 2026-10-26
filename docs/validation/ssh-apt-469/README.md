# Issue #469 local qualification evidence

Executed on a Linux host with Docker Engine 29.8.2, using the isolated harness and exact base-image digests recorded in the reports:

| Target | Executed scenarios | Outcome | Duration including build/setup/cleanup |
| --- | --- | --- | --- |
| [Debian 12](debian12.json) | 7 | All passed | 16.4 seconds |
| [Debian 13](debian13.json) | 7 | All passed | 48.9 seconds |
| [Ubuntu 22.04](ubuntu2204.json) | 7 | All passed | 48.0 seconds |
| [Ubuntu 24.04](ubuntu2404.json) | 7 | All passed | 47.0 seconds |
| [Ubuntu 26.04](ubuntu2604.json) | 7 | All passed | 47.3 seconds |

Each includes a real root upgrade, non-root upgrade through the production-installed typed helper, malformed-helper rejection, real APT lock, approval drift/obsolete generation, SSH disconnection during dpkg mutation with reconciliation/repair, and changed-host-key rejection. Cleanup removed both containers, the run's bridge and tagged target image. Shared public base images/build cache remain available to Docker.

These are local execution timings from 2026-10-26, not a performance budget or a claim about other runners. Four reports come from a concurrent five-target run alongside Go validation; Debian 12 comes from an isolated rerun. The [concurrent Debian 12 failure](debian12-parallel-failure.json) timed out waiting for the disconnect scenario's mutation marker, with cleanup passing. The isolated rerun completed all seven scenarios without changing deadlines; the concurrent timeout remains a validation limit.

Review validation first reproduced deterministic operator-precheck failures on Debian 12 and Ubuntu 22.04: older `fuser` versions reported unreadable process descriptors under Docker's default capabilities. The fixture now gives only the target container `SYS_PTRACE`, preserving its private PID namespace and no host mounts. Both older distributions then passed all seven scenarios, as did the other supported distributions. The regression rejects removal of this permission or use of a privileged/shared-PID target.

[VM prerequisite report](vm-prerequisites.json): controlled reboot and real systemd observations were **not executed** because QEMU, `qemu-img` and `cloud-localds` are unavailable. The container suite explicitly reports those scenarios and official-distribution provenance qualification as `not_executed`. No container restart is counted as a reboot. See the [VM command and qualification procedure](../../ssh-apt-qualification.md#controlled-reboot-in-a-fresh-vm).

Additional validation: twelve fast harness admission/cleanup/report/VM checks (the new capability regression fails against the original branch head), complete Go tests, `go vet`, Go build, staticcheck v0.7.0 and actionlint v1.7.12. Runtime Go code and UI were not changed; race/browser suites remain part of CI rather than this local harness proof.
