ARG BASE_IMAGE
FROM ${BASE_IMAGE}
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get -o Acquire::Retries=3 update && apt-get -y --no-install-recommends install openssh-server sudo curl python3 psmisc procps ca-certificates dpkg-dev systemd && rm -rf /var/lib/apt/lists/* && rm -f /etc/ssh/ssh_host_*_key*
COPY target.py /usr/local/lib/slu-target.py
RUN groupadd slu-fixture && useradd -m -g slu-fixture -s /bin/sh operator && mkdir -p /run/sshd /srv/slu-repo /srv/slu-packages /var/lib/slu-fixture && \
    printf 'operator ALL=(root) /bin/sh\n' > /etc/sudoers.d/fixture-bootstrap && chmod 0440 /etc/sudoers.d/fixture-bootstrap && \
    printf 'PermitRootLogin yes\nPasswordAuthentication yes\nUsePAM no\nPrintMotd no\nLogLevel ERROR\n' > /etc/ssh/sshd_config.d/fixture.conf && \
    rm -f /etc/apt/sources.list /etc/apt/sources.list.d/* && \
    printf 'deb [trusted=yes] file:/srv/slu-repo ./\n' > /etc/apt/sources.list.d/slu-fixture.list && \
    python3 /usr/local/lib/slu-target.py build && rm -f /etc/ssh/ssh_host_*_key*
ENTRYPOINT ["python3", "/usr/local/lib/slu-target.py", "start"]
