# Bursty

On-demand Firecracker microVMs: an orchestrator with an HTTP API, per-VM networking,
memory snapshots and a CLI.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/theognis1002/bursty.svg)](https://pkg.go.dev/github.com/theognis1002/bursty)

## Overview

Bursty is the core loop of platforms like Fly.io: take a request, hand back a running,
network-reachable VM in well under 200 ms. A small daemon (`burstyd`) manages
[Firecracker](https://firecracker-microvm.github.io/) microVMs on one Linux host with KVM.
A CLI (`bursty`) and a JSON HTTP API create, pause, snapshot, restore and delete them.
On a Mac (or any host without KVM), a process-based `sim` driver runs the same API for
development.

## Features

- **Fast creates** from a warm pool of pre-spawned Firecracker processes and network slots
- **Templates**: new VMs restore from a pre-booted snapshot of their shape, then get their
  own identity, clock, entropy and command over vsock (no kernel boot on the request path)
- **Snapshots**: pause, snapshot memory and state, then restore any number of clones
- **Per-VM networking**: one network namespace per VM, NAT egress, published host ports
- **Minimal guest init** (`bursty-init`, PID 1) injected into a stock ext4 rootfs
- **Serial console** logs, with follow mode
- **Built-in benchmark** (`bursty bench`) for cold boot, template create and restore latency
- **`sim` driver** for API and CLI work on macOS or hosts without `/dev/kvm`

## Prerequisites

- Go 1.26 or newer (to build)
- For real microVMs: Linux with `/dev/kvm`, root, `iproute2`, `iptables` and `e2fsprogs`
  (`debugfs`). `scripts/setup-linux.sh` installs these on Debian/Ubuntu, plus Firecracker
  v1.17, a guest kernel and an Ubuntu 24.04 rootfs.
- For the KVM lab on Apple Silicon: Docker Desktop

## Quick Start

### On a Mac (or anything without KVM): sim driver

```sh
git clone https://github.com/theognis1002/bursty.git
cd bursty
make build                 # bin/burstyd, bin/bursty
./bin/burstyd              # picks the sim driver when there is no KVM
```

In a second terminal:

```sh
./bin/bursty run --name web
./bin/bursty curl web
./bin/bursty ls
```

### On Linux with KVM (the real thing)

```sh
git clone https://github.com/theognis1002/bursty.git
cd bursty
sudo scripts/setup-linux.sh          # firecracker v1.17, guest kernel 6.1, Ubuntu 24.04 rootfs
make linux                           # bin/linux-<arch>/{burstyd,bursty,bursty-init}
sudo bin/linux-amd64/burstyd         # driver auto-detects firecracker
bin/linux-amd64/bursty run --name web
bin/linux-amd64/bursty curl web
BURSTY=bin/linux-amd64/bursty scripts/e2e.sh   # full end-to-end check + benchmark
```

Replace `amd64` with `arm64` on an arm64 host.

`--writable` VMs get a private rootfs copy at create time; put `--data-dir` on XFS or
btrfs so `cp --reflink` makes that instant (on ext4 it is a full sparse copy).

## Usage

```sh
bursty run --name web --cpus 1 --mem 128 --cmd 'echo hello' -p 8081:8080
bursty logs -f web                   # follow the serial console
bursty snapshot web --name web-snap  # pause, snapshot, resume
bursty restore web-snap --name clone # new VM from the snapshot
bursty curl clone                    # same boot_id, request counter continued
bursty rm web clone && bursty rmsnap web-snap
bursty bench -n 10                   # latency percentiles
```

### CLI

```text
bursty run [--name N] [--cpus N] [--mem MB] [--cmd CMD] [--writable] [--cold] [-p HOST:GUEST]
bursty ls | get <vm> | logs [-f] <vm> | curl <vm> [path]
bursty pause|resume|stop|rm <vm>...
bursty snapshot <vm> [--name N] [--stop] | snapshots | restore <snap> [--name N] | rmsnap <snap>
bursty info | bench [-n N]
```

### API

| Method | Path | |
|---|---|---|
| `GET` | `/healthz` | liveness |
| `GET` | `/v1/info` | driver, network, pool status |
| `POST` | `/v1/vms` | `{name, vcpus, memory_mb, cmd, writable, cold, ports:[{host,guest}]}` → VM (returns once the guest is ready) |
| `GET` | `/v1/vms`, `/v1/vms/{id}` | list / get (by ID or name) |
| `POST` | `/v1/vms/{id}/pause`, `/resume`, `/stop` | lifecycle |
| `DELETE` | `/v1/vms/{id}` | kill and remove |
| `GET` | `/v1/vms/{id}/logs?follow=1&tail=N` | serial console |
| `POST` | `/v1/vms/{id}/snapshots` | `{name, stop}` → Snapshot |
| `GET` | `/v1/snapshots`, `/v1/snapshots/{id}` | list / get |
| `POST` | `/v1/snapshots/{id}/restore` | `{name, ports}` → new VM |
| `DELETE` | `/v1/snapshots/{id}` | remove |

## Architecture

```text
 bursty (CLI) ──HTTP──▶ burstyd ──┬─ warm pool: pre-spawned firecracker + netns slots
                                  ├─ orchestrator: boot / restore / pause / snapshot / stop
                                  ├─ templates: pre-booted snapshot per VM shape
                                  ├─ netns provider: veth + tap + NAT per VM
                                  └─ proxy: published host ports ─▶ VM ports
                                         │
                          firecracker (one process per VM, API on a unix socket)
                                         │
                             guest kernel ─▶ /bursty-init (PID 1) ─▶ :8080 + your --cmd
```

### Why it boots fast

* **Warm pool.** `burstyd` keeps N sandboxes ready: a Firecracker process already
  listening on its API socket, inside a network namespace whose TAP device already
  exists. A create request only sends 5 small API calls (`machine-config`,
  `boot-source`, `drives`, `network-interfaces`, `InstanceStart`).
* **Templates.** A template is a snapshot of a freshly booted, idle VM of one shape
  (vCPUs, memory). Creating a VM from it is a restore plus a vsock message that gives
  the clone its own identity, clock, entropy and command. `--cold` (or `--writable`)
  still boots a kernel; `burstyd --templates=false` turns templates off.
* **Prepared VMs.** For the default shape, the pool goes one step further: it restores
  the template, sends the identity/clock/entropy message, and pauses the VM. A create
  then only resumes it (one API call) and, if you passed `--cmd`, sends the command. All
  guest-side work is off the request path. A paused VM uses no CPU, and its clock stays
  right because the guest's virtual counter keeps running on the host.
* **Minimal guest userspace.** The rootfs gets `/bursty-init` injected (with `debugfs`,
  no mounts or root needed). It is PID 1: it mounts `/proc`, `/sys`, `/dev`, `/run`,
  `/tmp`, prints `BURSTY_READY uptime=…` on the serial console, serves HTTP on :8080
  and runs your `--cmd`. No systemd.
* **Quiet kernel.** `quiet loglevel=1` keeps the slow emulated UART out of the boot path;
  the network is configured by the kernel (`ip=` autoconfig).
* **Snapshots restore instead of boot.** `snapshot/load` maps guest memory from the
  snapshot file (lazily, `MAP_PRIVATE`), so a restore skips the kernel entirely.

Create latency in the API is measured end to end: request received → VM running and
answering (cold boot: guest init reports ready on the serial console; template: the
guest agent acknowledged its identity). Each VM's `timings` shows where the time went:
`mode` (`template`, `cold`, `restore`), `prepared`, `acquire` from the pool, `start`
(configure + InstanceStart, snapshot load, or resume), `init_ms` (guest agent), and
`guest_uptime_ms` (the guest kernel's own clock at ready, cold boot only).

### Networking

Every VM gets its own network namespace with the **same** inside view:

```text
 host                               netns bsN                      guest
 bsvN 10.201.x.(4N+1)/30 ═veth═ veth0 10.201.x.(4N+2)/30
                                 DNAT 10.201.x.(4N+2) → 172.16.0.2
                                 SNAT 172.16.0.2 → 10.201.x.(4N+2)
                                 tap0 172.16.0.1/30 ══════════ eth0 172.16.0.2
```

The guest always sees `172.16.0.2` behind `172.16.0.1` and MAC `06:00:ac:10:00:02`.
That is what makes snapshots cloneable: N restores of one snapshot all wake up with the
same in-guest network config, yet each is reachable at its own unique host IP. The
host masquerades `10.201.0.0/16` for egress and forwards between VMs; guests cannot
open connections to services on the host (an `INPUT` drop for `bsv+`). `-p HOST:GUEST`
publishes a port through the daemon's TCP proxy.

### Snapshots

`bursty snapshot <vm>` pauses the VM, writes a Full snapshot (`vmstate` + `mem`, plus a
copy of the disk for `--writable` VMs), and resumes it (or stops it with `--stop`).
`bursty restore <snap>` loads it into a warm sandbox — any number of times, concurrently.
Drive paths are relative to each VM's directory (Firecracker runs with it as CWD), so a
snapshot restores into any new directory. The guest's HTTP service reports a `boot_id`
and a request counter, which makes it easy to see that a clone really resumed the
source's memory.

### Layout

```text
cmd/burstyd               daemon (flags: --driver, --pool, --kernel, --rootfs, --listen, …)
cmd/bursty                CLI
cmd/bursty-init           guest PID 1 (static, injected into the rootfs)
internal/orchestrator     lifecycle, warm pool, templates, snapshots, rootfs prep, port proxy
internal/vmm/firecracker  Firecracker driver (REST over the API socket, vsock)
internal/vmm/sim          process-based stand-in for hosts without KVM
internal/netx             netns/veth/tap networking (Linux) and sim ports
internal/server           HTTP API
internal/client           Go client
lab/                      QEMU-emulated KVM host for Macs without nested virtualization
scripts/                  setup-linux.sh, e2e.sh, lab-e2e.sh
```

## Configuration

Bursty does not read a `.env` file. `burstyd` takes flags; run `burstyd -h` for the full
list. The main ones:

| Flag | Default | |
|---|---|---|
| `--listen` | `127.0.0.1:7070` | API listen address |
| `--data-dir` | `/var/lib/bursty` (root on Linux), else `~/.bursty` | VMs, snapshots, prepared images |
| `--driver` | `auto` | `auto`, `firecracker` or `sim` |
| `--kernel` | `/var/lib/bursty/images/vmlinux` | guest kernel (uncompressed) |
| `--rootfs` | `/var/lib/bursty/images/rootfs.ext4` | base guest rootfs (ext4) |
| `--init` | `bursty-init` next to `burstyd` | guest init binary |
| `--pool` | `3` | warm sandboxes kept ready |
| `--templates` | `true` | create VMs from pre-booted templates |
| `--prepared` | `2` | default-shape VMs kept restored, initialized and paused |
| `--ready-timeout` | `10s` | max time for a guest to report ready |
| `--publish-addr` | `127.0.0.1` | host address for published VM ports |
| `--max-vms` | `1024` | max concurrent network slots |

Environment variables:

| Variable | Used by | Default |
|---|---|---|
| `BURSTY_API` | `bursty` CLI | `127.0.0.1:7070` (or `--api ADDR`) |
| `BURSTY`, `BURSTYD`, `BENCH_N`, `BENCH_GAP` | `scripts/e2e.sh` | `bin/bursty`, `bin/burstyd`, `10`, `300ms` |
| `FC_VERSION`, `KERNEL_VERSION`, `IMAGES` | `scripts/setup-linux.sh` | `v1.17.0`, `6.1.155`, `/var/lib/bursty/images` |
| `LAB_CPUS`, `LAB_MEM`, `LAB_SSH_PORT`, … | `lab/up.sh` | see [lab/README.md](lab/README.md) |

## Development

```sh
make build        # native binaries in bin/
make test         # go vet + unit and API tests (sim driver), with -race
make lint         # gofmt check + go vet
make e2e-sim      # CLI end-to-end against a throwaway sim daemon (~2 s)
make lab-e2e      # CLI end-to-end against real Firecracker in the lab (~2-5 min)
make hooks        # install the git pre-commit hook (gofmt + go vet)
```

### KVM lab on Apple Silicon

Apple M1/M2 have no nested virtualization, so `lab/` runs an emulated (QEMU TCG) arm64
Ubuntu host with EL2, which gives it a working `/dev/kvm`. Real Firecracker runs inside
it; boot times there are emulation-bound and not representative. `make lab-e2e` brings
the lab up (first run ~5 min), deploys `burstyd`, and runs the end-to-end suite against
real Firecracker. See [lab/README.md](lab/README.md).

```sh
lab/ssh.sh /mnt/share/bin/bursty ls   # poke the lab daemon afterwards
lab/down.sh                           # stop the lab (keeps its disk)
```

### Measured

Real Firecracker v1.17.0 in the lab (arm64 guest, kernel 6.1.155, 1 vCPU / 128 MiB),
i.e. nested KVM on top of **software CPU emulation** that runs roughly 30x slower than
hardware:

| | p50 | range | under 200 ms |
|---|---|---|---|
| **create (`POST /v1/vms`), prepared template VM → running** | **6.3 ms** | 6.2–26.5 ms | 5/5, 2/2 in e2e |
| create from template, not prepared (restore + vsock init) | 440–650 ms | 430–1140 ms | |
| snapshot restore, API request → running | 70–95 ms | 56–113 ms | 5/5 |
| cold boot (`--cold`), API request → guest init ready | 2.6–3.7 s | 2.4–13 s | |
| ↳ guest kernel start → init ready (guest clock) | 1.8–2.4 s | | |
| orchestration overhead (sim driver: pool acquire + API + bookkeeping) | ~1 ms | | |

So a create returns a running, network-reachable VM with its own identity in single
digit milliseconds, even on emulated hardware: no guest code runs on the request path.
A paused prepared VM kept the host's wall clock to the second after 60 s in the pool.
Paths where the guest must execute (cold boot, the vsock init of an unprepared
template clone, delivering `--cmd`) are emulation-bound here (~30x); for scale, the
same microVM with the image's stock systemd took 27–39 s to boot in the lab, and
`bursty-init` cuts that by ~10x. Run `bursty bench` on a Linux host with `/dev/kvm` for
hardware numbers on those paths.

## Limits

* One host, no scheduler across hosts; state is JSON files under `--data-dir`.
* No jailer/seccomp hardening beyond Firecracker's defaults; run on trusted hosts.
* VMs die with the daemon (by design: an unmanaged VMM is a leak). Snapshot first if
  you want them back.
* Restored clones share the source's in-guest identity (`bursty.id`, hostname, `boot_id`),
  and wake with the snapshot's wall clock and RNG state; re-seed/re-sync in the guest if
  that matters to your workload. Template creates get a fresh identity, clock and entropy.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). To report a security issue, see
[SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
