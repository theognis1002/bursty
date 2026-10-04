#!/usr/bin/env bash
# Runs burstyd with the real firecracker driver inside the KVM lab (lab/) and the
# end-to-end suite against it. Brings the lab up and provisions it if needed.
# Afterwards burstyd keeps running in the lab:  lab/ssh.sh /mnt/share/bin/bursty ls
set -euo pipefail
cd "$(dirname "$0")/.."
BENCH_N="${BENCH_N:-5}"

lab/up.sh
if ! lab/ssh.sh 'command -v firecracker >/dev/null && test -s /var/lib/bursty/images/vmlinux'; then
  lab/ssh.sh 'sudo bash -s' < lab/provision.sh
fi

make linux ARCH=arm64 >/dev/null
mkdir -p lab/share/bin
cp bin/linux-arm64/burstyd bin/linux-arm64/bursty bin/linux-arm64/bursty-init lab/share/bin/
cp scripts/e2e.sh lab/share/

# Emulated KVM is ~30x slower than hardware: one warm sandbox, generous timeouts.
lab/ssh.sh 'set -e
  sudo pkill -INT -f /mnt/share/bin/burstyd || true
  for i in $(seq 50); do pgrep -f /mnt/share/bin/burstyd >/dev/null || break; sleep 0.2; done
  sudo sh -c "nohup /mnt/share/bin/burstyd --data-dir /var/lib/bursty/data \
    --init /mnt/share/bin/bursty-init --pool 1 --prepared 2 --ready-timeout 180s > /var/log/burstyd.log 2>&1 &"
  for i in $(seq 300); do /mnt/share/bin/bursty info >/dev/null 2>&1 && break; sleep 1; done
  /mnt/share/bin/bursty info >/dev/null || { tail -20 /var/log/burstyd.log; exit 1; }'

lab/ssh.sh "cd /mnt/share && BURSTY=/mnt/share/bin/bursty BENCH_N=$BENCH_N BENCH_GAP=3s bash e2e.sh"
echo "burstyd log: lab/ssh.sh sudo tail -f /var/log/burstyd.log"
