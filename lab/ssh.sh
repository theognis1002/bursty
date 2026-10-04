#!/usr/bin/env bash
# SSH into the lab guest as user "bursty" (passwordless sudo).
# Usage: lab/ssh.sh                 # interactive shell
#        lab/ssh.sh uname -a        # run a remote command
#        lab/ssh.sh -o Foo=bar cmd  # leading -o options go to ssh
set -euo pipefail
LAB_DIR="$(cd "$(dirname "$0")" && pwd)"
opts=()
while [ "${1:-}" = "-o" ]; do opts+=(-o "$2"); shift 2; done
exec ssh -i "$LAB_DIR/.state/id_ed25519" -p "${LAB_SSH_PORT:-2222}" \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
  -o ServerAliveInterval=15 \
  ${opts[@]+"${opts[@]}"} bursty@127.0.0.1 "$@"
