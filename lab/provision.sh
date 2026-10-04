#!/usr/bin/env bash
# Provision the lab guest for Firecracker. Run INSIDE the guest as root, or
# from the host with:  lab/ssh.sh 'sudo bash -s' < lab/provision.sh
# Idempotent: skips downloads that are already in place.
set -euo pipefail

FC_VERSION="${FC_VERSION:-v1.17.0}"
FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-aarch64.tgz"
CI_BASE="${CI_BASE:-http://spec.ccfc.min.s3.amazonaws.com/firecracker-ci/v1.15/aarch64}"
KERNEL_URL="$CI_BASE/vmlinux-6.1.155"
ROOTFS_URL="$CI_BASE/ubuntu-24.04.squashfs"
IMAGES=/var/lib/bursty/images
ROOTFS_SIZE="${ROOTFS_SIZE:-1G}"

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
export DEBIAN_FRONTEND=noninteractive

echo "==> apt packages"
need=()
for p in iproute2 iptables squashfs-tools e2fsprogs curl; do
  dpkg -s "$p" >/dev/null 2>&1 || need+=("$p")
done
if [ ${#need[@]} -gt 0 ]; then
  apt-get update -q
  apt-get install -y -q --no-install-recommends "${need[@]}"
fi

echo "==> firecracker $FC_VERSION"
if ! /usr/local/bin/firecracker --version 2>/dev/null | grep -q "${FC_VERSION#v}"; then
  tmp=$(mktemp -d)
  curl -fsSL "$FC_URL" | tar -xz -C "$tmp"
  rel="$tmp/release-${FC_VERSION}-aarch64"
  install -m 0755 "$rel/firecracker-${FC_VERSION}-aarch64" /usr/local/bin/firecracker
  install -m 0755 "$rel/jailer-${FC_VERSION}-aarch64" /usr/local/bin/jailer
  for b in snapshot-editor rebase-snap cpu-template-helper seccompiler-bin; do
    [ -f "$rel/$b-${FC_VERSION}-aarch64" ] && install -m 0755 "$rel/$b-${FC_VERSION}-aarch64" "/usr/local/bin/$b"
  done
  rm -rf "$tmp"
fi
/usr/local/bin/firecracker --version | head -1

echo "==> guest kernel + rootfs in $IMAGES"
mkdir -p "$IMAGES"
if [ ! -s "$IMAGES/vmlinux" ]; then
  curl -fsSL -o "$IMAGES/vmlinux.part" "$KERNEL_URL"
  mv "$IMAGES/vmlinux.part" "$IMAGES/vmlinux"
fi
if [ ! -s "$IMAGES/rootfs.ext4" ]; then
  tmp=$(mktemp -d)
  curl -fsSL -o "$tmp/rootfs.squashfs" "$ROOTFS_URL"
  unsquashfs -q -no-progress -d "$tmp/root" "$tmp/rootfs.squashfs"
  truncate -s "$ROOTFS_SIZE" "$IMAGES/rootfs.ext4.part"
  mkfs.ext4 -q -F -d "$tmp/root" "$IMAGES/rootfs.ext4.part"
  mv "$IMAGES/rootfs.ext4.part" "$IMAGES/rootfs.ext4"
  rm -rf "$tmp"
fi
ls -lh "$IMAGES"

echo "==> kvm"
ls -l /dev/kvm
dmesg | grep -i kvm | head -20 || true
echo "provision done"
