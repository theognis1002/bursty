#!/usr/bin/env bash
# Bring up the bursty KVM lab: an emulated aarch64 Ubuntu 24.04 VM (QEMU TCG,
# EL2 on) inside Docker. The guest has /dev/kvm so Firecracker can run in it.
set -euo pipefail

LAB_DIR="$(cd "$(dirname "$0")" && pwd)"
STATE="$LAB_DIR/.state"
SHARE="$LAB_DIR/share"
NAME="${LAB_CONTAINER:-bursty-lab}"
IMAGE="${LAB_IMAGE:-bursty-lab:latest}"
SSH_PORT="${LAB_SSH_PORT:-2222}"
CLOUD_IMG_URL="${LAB_CLOUD_IMG_URL:-https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-arm64.img}"
CLOUD_IMG="$STATE/noble-server-cloudimg-arm64.img"
SSH_WAIT_SECS="${LAB_SSH_WAIT_SECS:-900}"

mkdir -p "$STATE" "$SHARE"

if docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
  echo "lab container $NAME already running"
else
  docker rm -f "$NAME" >/dev/null 2>&1 || true

  echo "==> building $IMAGE"
  docker build -q -t "$IMAGE" "$LAB_DIR" >/dev/null

  if [ ! -f "$CLOUD_IMG" ]; then
    echo "==> downloading Ubuntu 24.04 arm64 cloud image"
    curl -fL --progress-bar -o "$CLOUD_IMG.part" "$CLOUD_IMG_URL"
    mv "$CLOUD_IMG.part" "$CLOUD_IMG"
  fi

  if [ ! -f "$STATE/id_ed25519" ]; then
    ssh-keygen -q -t ed25519 -N '' -C bursty-lab -f "$STATE/id_ed25519"
  fi

  echo "==> starting VM (container $NAME, ssh on 127.0.0.1:$SSH_PORT)"
  docker run -d --name "$NAME" \
    -p "127.0.0.1:$SSH_PORT:2222" \
    -v "$STATE:/state" \
    -v "$SHARE:/share" \
    -e LAB_CPUS="${LAB_CPUS:-4}" \
    -e LAB_MEM="${LAB_MEM:-4G}" \
    ${LAB_CPU_MODEL:+-e LAB_CPU_MODEL="$LAB_CPU_MODEL"} \
    "$IMAGE" >/dev/null
fi

echo "==> waiting for SSH (first boot under emulation takes a few minutes)"
start=$(date +%s)
until "$LAB_DIR/ssh.sh" -o ConnectTimeout=5 true 2>/dev/null; do
  if ! docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
    echo "container exited; see: docker logs $NAME" >&2
    exit 1
  fi
  if [ $(( $(date +%s) - start )) -gt "$SSH_WAIT_SECS" ]; then
    echo "timed out waiting for SSH; console log: $STATE/console.log" >&2
    exit 1
  fi
  sleep 5
done
"$LAB_DIR/ssh.sh" 'cloud-init status --wait >/dev/null 2>&1 || true'
echo "==> lab ready after $(( $(date +%s) - start ))s. Try: $LAB_DIR/ssh.sh ls -l /dev/kvm"
