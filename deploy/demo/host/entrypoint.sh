#!/usr/bin/env bash
# Starts sshd (the service opsagent connects to) and sets up the operator key.
# The fake services (nginx / postgres) are deliberately left in a failed state:
# nothing is started, which is what the agent is supposed to discover.
set -e

mkdir -p /run/sshd
# Authorized keys come from the demo-ssh-pub ConfigMap mounted at /etc/authorized_keys.
if [ -f /etc/authorized_keys/ops ]; then
  cp /etc/authorized_keys/ops /home/ops/.ssh/authorized_keys
  chown ops:ops /home/ops/.ssh/authorized_keys
  chmod 600 /home/ops/.ssh/authorized_keys
fi

# Host keys: generated per pod from an emptyDir, so known_hosts is "insecure".
if [ ! -f /etc/ssh/keys/ssh_host_rsa_key ]; then
  ssh-keygen -q -t rsa -N "" -f /etc/ssh/keys/ssh_host_rsa_key
  ssh-keygen -q -t ed25519 -N "" -f /etc/ssh/keys/ssh_host_ed25519_key
fi
sed -i 's|^#HostKey |HostKey |g' /etc/ssh/sshd_config || true

exec /usr/sbin/sshd -D -e