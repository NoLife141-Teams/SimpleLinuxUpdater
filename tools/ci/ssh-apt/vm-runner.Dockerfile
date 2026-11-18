FROM debian:13-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad

RUN apt-get update \
    && apt-get install -y --no-install-recommends cloud-image-utils openssh-client python3 qemu-system-x86 qemu-utils \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 1000 --create-home qualification

COPY vm-reboot.py run.py images.json /qualification/tools/ci/ssh-apt/
USER qualification
ENTRYPOINT ["python3", "/qualification/tools/ci/ssh-apt/vm-reboot.py"]
