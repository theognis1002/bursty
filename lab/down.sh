#!/usr/bin/env bash
# Stop the lab VM. The disk in lab/.state is kept; pass --purge to delete it.
set -euo pipefail
LAB_DIR="$(cd "$(dirname "$0")" && pwd)"
NAME="${LAB_CONTAINER:-bursty-lab}"

if docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
  # Ask the guest to power off cleanly, then fall back to killing QEMU.
  "$LAB_DIR/ssh.sh" -o ConnectTimeout=5 'sudo systemctl poweroff' >/dev/null 2>&1 || true
  for _ in $(seq 1 30); do
    docker ps --format '{{.Names}}' | grep -qx "$NAME" || break
    sleep 2
  done
fi
docker rm -f "$NAME" >/dev/null 2>&1 || true

if [ "${1:-}" = "--purge" ]; then
  rm -f "$LAB_DIR/.state/disk.qcow2" "$LAB_DIR/.state/efivars.fd" "$LAB_DIR/.state/seed.iso"
  echo "lab stopped; VM disk deleted (cloud image and SSH key kept)"
else
  echo "lab stopped"
fi
