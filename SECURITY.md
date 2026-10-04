# Security Policy

## Supported versions

Only the latest commit on `main` gets security fixes.

## Reporting a vulnerability

Do not open a public issue for a security problem.

Report it privately through GitHub:
[Report a vulnerability](https://github.com/theognis1002/bursty/security/advisories/new).
Include the affected component (`burstyd`, `bursty`, `bursty-init`, networking), steps to
reproduce, and the impact you expect.

You can expect an acknowledgement within 7 days and a status update within 30 days.

## Scope

Bursty runs Firecracker with its default settings and does not use the jailer or extra
seccomp hardening. It is meant for trusted hosts and workloads. Guest-to-host escapes
through Firecracker or KVM itself belong to the
[Firecracker project](https://github.com/firecracker-microvm/firecracker/security).
