#!/usr/bin/env bash
# Prepares a Linux host with KVM to run burstyd with the firecracker driver:
# installs host packages, the Firecracker binary, and a guest kernel + rootfs.
# Usage: sudo scripts/setup-linux.sh
set -euo pipefail

FC_VERSION="${FC_VERSION:-v1.17.0}"
CI_VERSION="${CI_VERSION:-v1.15}"
KERNEL_VERSION="${KERNEL_VERSION:-6.1.155}"
IMAGES="${IMAGES:-/var/lib/bursty/images}"
ARCH="$(uname -m)"
CI="http://spec.ccfc.min.s3.amazonaws.com/firecracker-ci/${CI_VERSION}/${ARCH}"

[[ $EUID -eq 0 ]] || { echo "run as root (sudo $0)" >&2; exit 1; }
[[ -e /dev/kvm ]] || { echo "no /dev/kvm: enable virtualization (or nested virtualization) for this host" >&2; exit 1; }

if command -v apt-get >/dev/null; then
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl iproute2 iptables squashfs-tools e2fsprogs >/dev/null
fi
for bin in curl ip iptables unsquashfs mkfs.ext4 debugfs; do
  command -v "$bin" >/dev/null || { echo "missing $bin" >&2; exit 1; }
done

if ! command -v firecracker >/dev/null || ! firecracker --version | grep -q "${FC_VERSION#v}"; then
  echo "installing firecracker ${FC_VERSION}"
  tmp="$(mktemp -d)"
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz" | tar -xz -C "$tmp"
  install -m 0755 "$tmp/release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH}" /usr/local/bin/firecracker
  rm -rf "$tmp"
fi
firecracker --version | head -1

mkdir -p "$IMAGES"
if [[ ! -s "$IMAGES/vmlinux" ]]; then
  echo "downloading guest kernel ${KERNEL_VERSION}"
  curl -fsSL -o "$IMAGES/vmlinux" "${CI}/vmlinux-${KERNEL_VERSION}"
fi
if [[ ! -s "$IMAGES/rootfs.ext4" ]]; then
  echo "building rootfs.ext4 from the Firecracker CI Ubuntu 24.04 image"
  tmp="$(mktemp -d)"
  curl -fsSL -o "$tmp/rootfs.squashfs" "${CI}/ubuntu-24.04.squashfs"
  unsquashfs -q -d "$tmp/root" "$tmp/rootfs.squashfs"
  size_mb=$(( $(du -sm "$tmp/root" | cut -f1) + 128 ))
  truncate -s "${size_mb}M" "$IMAGES/rootfs.ext4"
  mkfs.ext4 -q -d "$tmp/root" -F "$IMAGES/rootfs.ext4"
  rm -rf "$tmp"
fi
ls -lh "$IMAGES"
case "$ARCH" in x86_64) GOARCH=amd64 ;; aarch64) GOARCH=arm64 ;; *) GOARCH="$ARCH" ;; esac
echo "done. build with 'make linux' and run: sudo ./bin/linux-$GOARCH/burstyd"
