#!/bin/bash
# Runs inside the lab container. Prepares disk, firmware vars, and the
# cloud-init seed under /state, then execs QEMU (TCG) with EL2 enabled.
set -euo pipefail

STATE=/state
BASE_IMG="$STATE/${LAB_BASE_IMAGE:-noble-server-cloudimg-arm64.img}"
DISK="$STATE/disk.qcow2"
VARS="$STATE/efivars.fd"
SEED="$STATE/seed.iso"
CPUS="${LAB_CPUS:-4}"
MEM="${LAB_MEM:-4G}"
DISK_SIZE="${LAB_DISK_SIZE:-30G}"
FW_CODE=/usr/share/AAVMF/AAVMF_CODE.fd
FW_VARS=/usr/share/AAVMF/AAVMF_VARS.fd

[ -f "$BASE_IMG" ] || { echo "missing base image $BASE_IMG" >&2; exit 1; }
[ -f "$STATE/id_ed25519.pub" ] || { echo "missing $STATE/id_ed25519.pub" >&2; exit 1; }

if [ ! -f "$DISK" ]; then
  qemu-img create -f qcow2 -F qcow2 -b "$(basename "$BASE_IMG")" "$DISK" "$DISK_SIZE"
fi
[ -f "$VARS" ] || cp "$FW_VARS" "$VARS"

# cloud-init NoCloud seed (rebuilt every start; instance-id is stable so
# first-boot modules only run once per disk).
SEED_DIR=$(mktemp -d)
PUBKEY=$(cat "$STATE/id_ed25519.pub")
cat > "$SEED_DIR/meta-data" <<META
instance-id: bursty-lab-1
local-hostname: bursty-lab
META
cat > "$SEED_DIR/user-data" <<USER
#cloud-config
users:
  - name: bursty
    gecos: bursty lab
    groups: [sudo, kvm]
    shell: /bin/bash
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    lock_passwd: true
    ssh_authorized_keys:
      - $PUBKEY
disable_root: false
ssh_pwauth: false
package_update: false
package_upgrade: false
mounts:
  - [share, /mnt/share, 9p, "trans=virtio,version=9p2000.L,msize=524288,nofail,x-systemd.automount", "0", "0"]
runcmd:
  - [mkdir, -p, /root/.ssh]
  - [cp, /home/bursty/.ssh/authorized_keys, /root/.ssh/authorized_keys]
  - [systemctl, disable, --now, unattended-upgrades.service, apt-daily.timer, apt-daily-upgrade.timer, snapd.service, snapd.socket, snapd.seeded.service]
  - [mkdir, -p, /mnt/share]
  - [systemctl, daemon-reload]
  - [sh, -c, "mount /mnt/share || true"]
USER
genisoimage -quiet -output "$SEED" -volid cidata -joliet -rock "$SEED_DIR/user-data" "$SEED_DIR/meta-data"
rm -rf "$SEED_DIR"

VIRTFS=()
if [ -d /share ]; then
  VIRTFS=(-virtfs "local,path=/share,mount_tag=share,security_model=none,id=share")
fi

exec qemu-system-aarch64 \
  -machine virt,virtualization=on,gic-version=3 \
  -cpu "${LAB_CPU_MODEL:-max,pauth-impdef=on}" \
  -accel tcg,thread=multi,tb-size=1024 \
  -smp "$CPUS" -m "$MEM" \
  -drive if=pflash,format=raw,readonly=on,file="$FW_CODE" \
  -drive if=pflash,format=raw,file="$VARS" \
  -drive if=virtio,format=qcow2,file="$DISK",cache=unsafe,discard=unmap \
  -drive if=virtio,format=raw,file="$SEED",readonly=on \
  -netdev user,id=n0,hostfwd=tcp::2222-:22 \
  -device virtio-net-pci,netdev=n0,romfile= \
  -device virtio-rng-pci \
  "${VIRTFS[@]}" \
  -display none \
  -chardev stdio,id=con0,logfile="$STATE/console.log",signal=off \
  -serial chardev:con0 \
  -monitor unix:"$STATE/qemu-monitor.sock",server,nowait
