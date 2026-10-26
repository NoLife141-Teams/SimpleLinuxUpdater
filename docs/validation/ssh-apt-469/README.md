# Issue #469 local qualification evidence

Executed on a Linux host with Docker Engine 29.8.2, using the isolated harness and exact base-image digests recorded in the reports:

| Target | Executed scenarios | Outcome | Duration including build/setup/cleanup |
| --- | --- | --- | --- |
| [Debian 13](debian13.json) | 7 | All passed | 21.4 seconds |
| [Ubuntu 24.04](ubuntu2404.json) | 7 | All passed | 22.1 seconds |

Each includes a real root upgrade, non-root upgrade through the production-installed typed helper, malformed-helper rejection, real APT lock, approval drift/obsolete generation, SSH disconnection during dpkg mutation with reconciliation/repair, and changed-host-key rejection. Cleanup removed both containers, the run's bridge and tagged target image. Shared public base images/build cache remain available to Docker.

These are local execution timings, not a performance budget or a claim about other runners. The expanded Debian 12 / Ubuntu 22.04 / Ubuntu 26.04 workflow is configured but was not executed locally.

[VM prerequisite report](vm-prerequisites.json): controlled reboot and real systemd observations were **not executed** because QEMU, `qemu-img` and `cloud-localds` are unavailable. The container suite explicitly reports those scenarios and official-distribution provenance qualification as `not_executed`. No container restart is counted as a reboot. See the [VM command and qualification procedure](../../ssh-apt-qualification.md#controlled-reboot-in-a-fresh-vm).

Additional validation: eleven fast harness admission/cleanup/report/VM checks, complete Go tests, `go vet`, Go build, staticcheck v0.7.0 and actionlint v1.7.12. Runtime Go code and UI were not changed; race/browser suites remain part of CI rather than this local harness proof.
