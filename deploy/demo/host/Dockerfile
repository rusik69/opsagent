FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        openssh-server \
        nginx \
        procps \
        curl \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Operator account opsagent connects as.
RUN useradd -m -s /bin/bash ops && \
    mkdir -p /run/sshd /home/ops/.ssh /etc/authorized_keys && \
    chmod 700 /home/ops/.ssh

# web-01 fixtures: nginx is installed but misconfigured (discoverable fault).
COPY web-01/broken.conf /etc/nginx/conf.d/broken.conf
COPY web-01/error.log /var/log/nginx/error.log
RUN chmod 666 /var/log/nginx/error.log

# db-01 fixtures: a postgres log showing a failed start.
RUN mkdir -p /var/log/postgresql
COPY db-01/postgresql.log /var/log/postgresql/postgresql.log

# Minimal systemctl/journalctl shims so the demo services report "failed".
COPY bin/systemctl /usr/local/bin/systemctl
COPY bin/journalctl /usr/local/bin/journalctl
RUN chmod +x /usr/local/bin/systemctl /usr/local/bin/journalctl

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 22
ENTRYPOINT ["/entrypoint.sh"]