#!/usr/bin/env bash
# Boot one Firecracker microVM by hand via the API socket, wait for the guest
# login prompt on its serial console, then (optionally) do a snapshot
# create/load round trip. Run INSIDE the lab guest as root, or from the host:
#   lab/ssh.sh 'sudo bash -s' < lab/fc-smoke.sh
#   lab/ssh.sh 'sudo SNAPSHOT=1 bash -s' < lab/fc-smoke.sh   # + snapshot round trip
# Env: NET=0 skips the tap NIC + ping check; BOOT_TIMEOUT (default 600s).
set -euo pipefail

IMAGES=/var/lib/bursty/images
WORK="${WORK:-/tmp/fc-smoke}"
BOOT_TIMEOUT="${BOOT_TIMEOUT:-600}"
SNAPSHOT="${SNAPSHOT:-0}"
NET="${NET:-1}"          # 1 = attach a tap NIC and ping the guest
TAP=fc-smoke-tap0
HOST_IP=172.16.0.1
GUEST_IP=172.16.0.2

rm -rf "$WORK"; mkdir -p "$WORK"
cp --sparse=always "$IMAGES/rootfs.ext4" "$WORK/rootfs.ext4"

api() { # api METHOD PATH JSON [socket]
  local sock="${4:-$WORK/fc.sock}"
  local code
  code=$(curl -sS -o "$WORK/resp" -w '%{http_code}' --unix-socket "$sock" \
    -X "$1" "http://localhost$2" -H 'Content-Type: application/json' -d "$3")
  if [ "${code:0:1}" != 2 ]; then
    echo "API $1 $2 -> HTTP $code: $(cat "$WORK/resp")" >&2; return 1
  fi
}

wait_sock() { for _ in $(seq 1 100); do [ -S "$1" ] && return 0; sleep 0.1; done; return 1; }

wait_for() { # wait_for PATTERN LOG TIMEOUT
  local t0=$(date +%s)
  until grep -qE "$1" "$2" 2>/dev/null; do
    [ $(( $(date +%s) - t0 )) -lt "$3" ] || return 1
    sleep 0.5
  done
}

cleanup() { ip link del "$TAP" 2>/dev/null || true; [ -n "${FC_PID:-}" ] && kill "$FC_PID" 2>/dev/null || true; [ -n "${FC2_PID:-}" ] && kill "$FC2_PID" 2>/dev/null || true; }
trap cleanup EXIT

firecracker --api-sock "$WORK/fc.sock" --log-path "$WORK/fc.log" --level Info \
  >"$WORK/console.log" 2>&1 </dev/null &
FC_PID=$!
touch "$WORK/fc.log"
wait_sock "$WORK/fc.sock"

BOOT_ARGS="console=ttyS0 reboot=k panic=1 pci=off"
if [ "$NET" = 1 ]; then
  ip link del "$TAP" 2>/dev/null || true
  ip tuntap add dev "$TAP" mode tap
  ip addr add "$HOST_IP/30" dev "$TAP"
  ip link set "$TAP" up
  BOOT_ARGS="$BOOT_ARGS ip=$GUEST_IP::$HOST_IP:255.255.255.252::eth0:off"
fi
api PUT /boot-source "{\"kernel_image_path\":\"$IMAGES/vmlinux\",\"boot_args\":\"$BOOT_ARGS\"}"
api PUT /drives/rootfs "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$WORK/rootfs.ext4\",\"is_root_device\":true,\"is_read_only\":false}"
api PUT /machine-config '{"vcpu_count":1,"mem_size_mib":256}'
if [ "$NET" = 1 ]; then
  api PUT /network-interfaces/eth0 "{\"iface_id\":\"eth0\",\"guest_mac\":\"06:00:AC:10:00:02\",\"host_dev_name\":\"$TAP\"}"
fi

t0=$(date +%s.%N)
api PUT /actions '{"action_type":"InstanceStart"}'
echo "InstanceStart accepted; waiting for login prompt (timeout ${BOOT_TIMEOUT}s)"
if wait_for 'login:' "$WORK/console.log" "$BOOT_TIMEOUT"; then
  echo "BOOT OK: login prompt after $(echo "$(date +%s.%N) - $t0" | bc)s"
else
  echo "BOOT FAILED/TIMEOUT. console tail:"; tail -40 "$WORK/console.log"; echo "fc.log tail:"; tail -40 "$WORK/fc.log"; exit 1
fi
grep -m1 'Linux version' "$WORK/console.log" || true
if [ "$NET" = 1 ]; then
  ping -c 3 -W 5 "$GUEST_IP" >/dev/null && echo "NET OK: guest $GUEST_IP answers ping" || { echo "NET FAILED: no ping reply"; exit 1; }
fi

if [ "$SNAPSHOT" = 1 ]; then
  echo "==> snapshot create"
  api PATCH /vm '{"state":"Paused"}'
  s0=$(date +%s.%N)
  api PUT /snapshot/create "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$WORK/vm.snap\",\"mem_file_path\":\"$WORK/vm.mem\"}"
  echo "snapshot created in $(echo "$(date +%s.%N) - $s0" | bc)s"; ls -lh "$WORK/vm.snap" "$WORK/vm.mem"
  kill "$FC_PID"; wait "$FC_PID" 2>/dev/null || true; FC_PID=

  echo "==> snapshot load into a new firecracker process"
  firecracker --api-sock "$WORK/fc2.sock" --log-path "$WORK/fc2.log" --level Info \
    >"$WORK/console2.log" 2>&1 </dev/null &
  FC2_PID=$!
  touch "$WORK/fc2.log"
  wait_sock "$WORK/fc2.sock"
  l0=$(date +%s.%N)
  api PUT /snapshot/load "{\"snapshot_path\":\"$WORK/vm.snap\",\"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"$WORK/vm.mem\"},\"resume_vm\":true}" "$WORK/fc2.sock"
  echo "snapshot loaded + resumed in $(echo "$(date +%s.%N) - $l0" | bc)s"
  sleep 3
  curl -sS --unix-socket "$WORK/fc2.sock" http://localhost/vm/config >/dev/null && echo "restored VM API responsive"
  kill -0 "$FC2_PID"
  if [ "$NET" = 1 ]; then
    ping -c 3 -W 5 "$GUEST_IP" >/dev/null && echo "SNAPSHOT ROUND TRIP OK: restored guest answers ping" || { echo "restored guest does not answer ping"; exit 1; }
  else
    echo "SNAPSHOT ROUND TRIP OK (restored VM process alive)"
  fi
fi
