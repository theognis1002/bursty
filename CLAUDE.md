# CLAUDE.md

Bursty: a Firecracker microVM orchestrator. `burstyd` (daemon, HTTP API on 127.0.0.1:7070) runs VMs on one Linux KVM host; `bursty` is the CLI; `bursty-init` is the guest PID 1. A `sim` driver (child processes) stands in for Firecracker on macOS / hosts without KVM.

## Stack

- Go 1.26, module `github.com/theognis1002/bursty`, only dep `golang.org/x/sys`
- Stdlib `net/http` (Go 1.22+ method patterns), `log/slog`, `flag`
- Linux host tools: Firecracker v1.17, `ip`, `iptables`, `debugfs`
- Lab: Docker + QEMU TCG (emulated arm64 host with `/dev/kvm`) for Apple Silicon

## Commands

```sh
make build                 # bin/burstyd, bin/bursty (native; sim driver on macOS)
make linux [ARCH=arm64]    # bin/linux-<arch>/{burstyd,bursty,bursty-init}, static
./bin/burstyd -v           # run daemon (auto driver); `--driver sim` to force sim
make test                  # go vet + go test -race -count=1 ./...
go test ./internal/server -run TestName -v   # single test
make lint                  # gofmt -l check + go vet (host and GOOS=linux)
make e2e-sim               # CLI end-to-end against a throwaway sim daemon (~2 s)
make lab-e2e               # end-to-end against real Firecracker in lab/ (slow)
make hooks                 # git config core.hooksPath .githooks
```

Always run `GOOS=linux go vet ./...` after touching Linux-only code; macOS builds skip it.

## Layout

- `cmd/burstyd` daemon flags and driver selection; `cmd/bursty` CLI (incl. `bench`); `cmd/bursty-init` guest init (`main_linux.go`)
- `internal/orchestrator` Manager: VM lifecycle, warm pool, templates + prepared pool, snapshots, rootfs prep (`image.go`), port proxy, JSON state store
- `internal/vmm` driver interface; `vmm/firecracker` (REST over unix socket, vsock); `vmm/sim`
- `internal/netx` per-VM netns/veth/tap + iptables (`linux.go`), sim ports (`sim.go`)
- `internal/server` HTTP API; `internal/client` Go client; `internal/api` wire types
- `internal/guest` HTTP service in every guest (:8080); `internal/console` serial log + ready marker
- `scripts/` setup-linux.sh, e2e.sh, lab-e2e.sh; `lab/` QEMU KVM lab (state in `lab/.state`, gitignored, holds an SSH private key)

## Patterns

- Linux-only code uses `_linux.go` / `_other.go` file pairs; the module must build and test on macOS.
- Every VM sees the same in-guest network (172.16.0.2/30, fixed MAC); uniqueness lives in the host netns (`bsN`, veth `bsvN`, 10.201.0.0/16). This makes snapshots cloneable.
- Guest readiness = `BURSTY_READY uptime=<s>` on the serial console. Kernel args carry `bursty.id` and base64 `bursty.cmd`.
- Template creates = restore of a pre-booted snapshot + vsock message for identity/clock/entropy/cmd. `--cold` / `--writable` boot a kernel.
- Prepared pool (`--prepared`, default 2): default-shape VMs kept restored from the template, initialized and paused; a create resumes one and sends only `--cmd` over vsock (`timings.prepared`).
- Firecracker runs with the VM dir as CWD; drive paths are relative so snapshots restore anywhere.
- State is JSON files under `--data-dir`. VMs die with the daemon by design.
- Update README.md (CLI, API table, flags) when you change them.

## Configuration

No `.env`. `burstyd` uses flags (`burstyd -h`). CLI reads `BURSTY_API`. Scripts read `BURSTY`, `BURSTYD`, `BENCH_N`, `BENCH_GAP`, `FC_VERSION`, `KERNEL_VERSION`, `IMAGES`, `LAB_*` (see README Configuration).
