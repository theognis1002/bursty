# Contributing to Bursty

Thanks for your interest. Bug reports, fixes and small focused features are welcome.

## Questions and bugs

Open an [issue](https://github.com/theognis1002/bursty/issues). For a bug, include the
host OS and arch, the driver (`bursty info`), the command you ran, and the `burstyd` log.
For security issues, follow [SECURITY.md](SECURITY.md) instead.

## Development setup

You need Go 1.26 or newer. Real microVMs also need Linux with `/dev/kvm`
(see `scripts/setup-linux.sh`); everything else runs on macOS with the `sim` driver.

```sh
git clone https://github.com/theognis1002/bursty.git
cd bursty
make hooks        # git pre-commit hook: gofmt on staged files + go vet
make build        # bin/burstyd, bin/bursty
```

## Tests

```sh
make test         # go vet + unit and API tests (sim driver), with -race
make lint         # gofmt check + go vet (host and GOOS=linux)
make e2e-sim      # CLI end-to-end against a throwaway sim daemon
```

If you change the Firecracker driver, networking, guest init or snapshots, also run the
end-to-end suite against real Firecracker: on a Linux KVM host with
`BURSTY=bin/linux-<arch>/bursty scripts/e2e.sh`, or on Apple Silicon with
`make lab-e2e` (see [lab/README.md](lab/README.md)).

## Code style

- Format with `gofmt`. `make lint` must pass.
- Keep dependencies minimal; the module only depends on `golang.org/x/sys`.
- Linux-only code goes behind build tags (`_linux.go` / `_other.go`) so the project
  still builds and tests on macOS.
- Add or update tests for behavior changes. API changes need a test in
  `internal/server`.

## Pull requests

1. Fork the repo and create a branch from `main` (for example `fix/restore-timeout`).
2. Keep each PR to one change. Update `README.md` when you change flags, CLI or API.
3. Make sure `make lint test e2e-sim` passes.
4. Open the PR with a short description of what changed and why, and how you tested it.

A maintainer reviews each PR. Expect comments; small PRs get merged faster.

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE).
