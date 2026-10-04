#!/usr/bin/env bash
# End-to-end check of the CLI + API.
#   scripts/e2e.sh sim        start a throwaway sim daemon and test it
#   scripts/e2e.sh            test the daemon at $BURSTY_API (e.g. a real firecracker host)
# BURSTY / BURSTYD point at the binaries (default: bin/bursty, bin/burstyd).
set -euo pipefail
cd "$(dirname "$0")/.."
BURSTY="${BURSTY:-bin/bursty}"
BURSTYD="${BURSTYD:-bin/burstyd}"
BENCH_N="${BENCH_N:-10}"

suffix="$RANDOM"
web="e2e-web-$suffix"; snap="e2e-snap-$suffix"; clone="e2e-clone-$suffix"
tpl1="e2e-tpl1-$suffix"; tpl2="e2e-tpl2-$suffix"; cold="e2e-cold-$suffix"
daemon=""
cleanup() {
  "$BURSTY" rm "$web" "$clone" "$tpl1" "$tpl2" "$cold" >/dev/null 2>&1 || true
  "$BURSTY" rmsnap "$snap" >/dev/null 2>&1 || true
  if [[ -n "$daemon" ]]; then kill "$daemon" 2>/dev/null; wait "$daemon" 2>/dev/null || true; rm -rf "$data"; fi
}
trap cleanup EXIT

if [[ "${1:-}" == "sim" ]]; then
  data="$(mktemp -d)"
  export BURSTY_API="127.0.0.1:$((17000 + RANDOM % 1000))"
  "$BURSTYD" --driver sim --data-dir "$data" --listen "$BURSTY_API" --sim-port-base $((21000 + RANDOM % 5000)) 2>"$data/burstyd.log" &
  daemon=$!
  for _ in $(seq 50); do "$BURSTY" info >/dev/null 2>&1 && break; sleep 0.1; done
fi

pass() { printf '  \033[32mok\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; exit 1; }
field() { sed -n "s/.*\"$1\": *\"\{0,1\}\([^\",}]*\).*/\1/p" | head -1; }

echo "== daemon"
driver="$("$BURSTY" info | field driver)"
[[ -n "$driver" ]] || fail "burstyd not reachable at ${BURSTY_API:-127.0.0.1:7070}"; pass "info: driver $driver"

echo "== boot"
out="$("$BURSTY" run --name "$web" --cmd 'echo e2e-cmd-ran')"; echo "$out" | sed 's/^/    /'
"$BURSTY" ls | grep >/dev/null "$web.*running" || fail "VM running"; pass "VM running"
reply="$("$BURSTY" curl "$web")"
echo "$reply" | grep >/dev/null '"requests":1' || fail "guest HTTP service answers over the VM network"; pass "guest HTTP service answers over the VM network"
boot_id="$(echo "$reply" | field boot_id)"
for _ in $(seq 50); do "$BURSTY" logs "$web" | grep >/dev/null e2e-cmd-ran && break; sleep 0.2; done
"$BURSTY" logs "$web" | grep >/dev/null e2e-cmd-ran || fail "guest ran --cmd (seen on serial console)"; pass "guest ran --cmd (seen on serial console)"

echo "== create from template"
for _ in $(seq 300); do "$BURSTY" info | grep >/dev/null '"prepared_ready": [1-9]' && break; sleep 1; done
"$BURSTY" info | grep >/dev/null '"1x128"' || fail "no template built (see daemon log)"
ids=()
for name in "$tpl1" "$tpl2"; do
  vm="$("$BURSTY" run --json --name "$name")"
  [[ "$(echo "$vm" | field mode)" == template ]] || fail "$name: mode $(echo "$vm" | field mode), want template"
  total="$(echo "$vm" | field total_ms)"; prepared="$(echo "$vm" | field prepared)"
  echo "    $name: ${total}ms (prepared=${prepared:-false})"
  r="$("$BURSTY" curl "$name")"
  [[ "$(echo "$r" | field vm_id)" == "$(echo "$vm" | field id)" ]] || fail "$name answers with the wrong identity: $r"
  ids+=("$(echo "$r" | field boot_id)")
  sleep 2 # let the prepared pool refill
done
[[ "${ids[0]}" != "${ids[1]}" ]] || fail "template clones share a boot_id"
pass "template clones have their own identity and boot_id"
"$BURSTY" rm "$tpl1" "$tpl2" >/dev/null

echo "== cold boot"
vm="$("$BURSTY" run --json --cold --name "$cold")"
[[ "$(echo "$vm" | field mode)" == cold ]] || fail "--cold gave mode $(echo "$vm" | field mode)"
echo "    $cold: $(echo "$vm" | field total_ms)ms (guest kernel→ready $(echo "$vm" | field guest_uptime_ms)ms)"
"$BURSTY" curl "$cold" | grep >/dev/null '"requests":1' || fail "cold VM answers"; pass "cold boot works"
"$BURSTY" rm "$cold" >/dev/null

echo "== pause / resume"
"$BURSTY" pause "$web" | grep >/dev/null paused || fail "paused"; pass "paused"
"$BURSTY" resume "$web" | grep >/dev/null running || fail "resumed"; pass "resumed"
"$BURSTY" curl "$web" | grep >/dev/null '"requests":2' || fail "guest state intact after pause"; pass "guest state intact after pause"

echo "== snapshot / restore"
"$BURSTY" snapshot "$web" --name "$snap" | sed 's/^/    /'
"$BURSTY" restore "$snap" --name "$clone" | sed 's/^/    /'
r="$("$BURSTY" curl "$clone")"
[[ "$(echo "$r" | field boot_id)" == "$boot_id" ]] || fail "clone boot_id differs: $r"
echo "$r" | grep >/dev/null '"requests":3' || fail "clone did not keep memory state: $r"
pass "clone resumed from snapshot memory (same boot_id, counter continued)"
"$BURSTY" curl "$web" | grep >/dev/null '"requests":3' || fail "source VM still running independently"; pass "source VM still running independently"

echo "== stop / delete"
"$BURSTY" stop "$web" | grep >/dev/null stopped || fail "stopped"; pass "stopped"
"$BURSTY" rm "$web" "$clone" >/dev/null && "$BURSTY" rmsnap "$snap" >/dev/null
"$BURSTY" ls | grep >/dev/null "$web" && fail "VM still listed after rm"
pass "deleted"

echo "== bench (n=$BENCH_N)"
"$BURSTY" bench -n "$BENCH_N" --gap "${BENCH_GAP:-300ms}" | sed 's/^/    /'
echo "e2e passed"
