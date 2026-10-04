# bursty KVM lab

Run real Firecracker on an Apple Silicon Mac. The Mac has no nested
virtualization, so we emulate an aarch64 machine with EL2 in QEMU (TCG) inside
Docker. The emulated Ubuntu 24.04 guest boots at EL2, gets `/dev/kvm`, and runs
Firecracker microVMs on it. It is slow, but it is real KVM.

```
macOS -> Docker Desktop (linuxkit VM) -> container: qemu-system-aarch64 (TCG,
  virt,virtualization=on,gic-version=3, -cpu max) -> Ubuntu 24.04 (KVM, VHE)
  -> Firecracker v1.17.0 -> microVM
```

## Use

```sh
lab/up.sh                                   # build image, boot VM, wait for SSH (~4 min first boot)
lab/ssh.sh 'sudo bash -s' < lab/provision.sh  # install Firecracker + guest kernel/rootfs (idempotent)
lab/ssh.sh 'sudo SNAPSHOT=1 bash -s' < lab/fc-smoke.sh  # boot a microVM, ping it, snapshot round trip
lab/ssh.sh                                  # shell as user "bursty" (passwordless sudo)
lab/ssh.sh uname -a                         # run a remote command
lab/down.sh                                 # stop VM (keeps disk); --purge deletes the VM disk
```

- SSH: `ssh -i lab/.state/id_ed25519 -p 2222 bursty@127.0.0.1` (root also accepts the key).
- Shared dir: `lab/share` on the host is `/mnt/share` in the guest (9p). Put
  cross-compiled binaries there (`GOOS=linux GOARCH=arm64`).
- In the guest: `/usr/local/bin/{firecracker,jailer,snapshot-editor,...}`,
  `/var/lib/bursty/images/{vmlinux,rootfs.ext4}`.
- Serial console of the lab VM: `lab/.state/console.log`. QEMU monitor:
  `lab/.state/qemu-monitor.sock`.
- Tunables (env for `up.sh`): `LAB_CPUS` (4), `LAB_MEM` (4G), `LAB_SSH_PORT`
  (2222), `LAB_CPU_MODEL` (`max,pauth-impdef=on`).

## Notes

- UEFI firmware (AAVMF) comes from Debian's `qemu-efi-aarch64` package in the
  image. The writable vars copy, VM disk (qcow2 overlay), seed ISO, cloud image
  and SSH key live in `lab/.state/` (gitignored).
- Guest kernel runs KVM in VHE mode (`kvm [1]: VHE mode initialized
  successfully`). No `kvm-arm.mode=` flag is needed.
- Expect a microVM to reach its login prompt in roughly 25-40 s (versus ~1 s
  on real hardware). Snapshot create of a 256 MiB VM takes ~3 s; load ~0.2 s.
- Do not use the timings for performance work. Use the lab for correctness.
