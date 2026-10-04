// Package vmm defines the driver boundary: a Driver spawns sandboxes, and a Machine is
// one VMM process that is booted (cold) or restored (from a snapshot) exactly once.
//
// Splitting Spawn from Boot/Restore is what makes the warm pool possible: the expensive,
// request-independent part (process start, API socket, network namespace) happens ahead
// of time, and a create request only pays for configuration plus guest boot.
package vmm

import (
	"context"

	"github.com/theognis1002/bursty/internal/guest"
	"github.com/theognis1002/bursty/internal/netx"
)

type SpawnSpec struct {
	ID string
	// Dir is the VM's private directory; the VMM runs with it as its working directory,
	// so relative paths inside (the rootfs) survive snapshot/restore into another dir.
	Dir   string
	Lease netx.Lease
}

type BootConfig struct {
	VCPUs    int
	MemoryMB int
	// Kernel is an absolute path. The rootfs is always "rootfs.ext4" inside the VM dir.
	Kernel   string
	BootArgs string
	ReadOnly bool
	// Cmd is passed to the guest init.
	Cmd string
}

// RootfsName is the rootfs file (or symlink) inside every VM dir.
const RootfsName = "rootfs.ext4"

// VsockName is the host side of the guest vsock (a unix socket) inside every VM dir.
const VsockName = "v.sock"

type Driver interface {
	Name() string
	Spawn(ctx context.Context, spec SpawnSpec) (Machine, error)
}

type Machine interface {
	PID() int
	Boot(ctx context.Context, cfg BootConfig) error
	// Restore loads a snapshot from dir and resumes it.
	Restore(ctx context.Context, snapDir string) error
	// Init sends a message to the guest agent over a host-only channel (vsock) and waits
	// for it to be applied. Used to turn a template clone into a new VM.
	Init(ctx context.Context, msg guest.InitMsg) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	// Snapshot writes the paused VM's state into dir.
	Snapshot(ctx context.Context, dir string) error
	// Ready closes when the guest reports ready after a cold boot.
	Ready() <-chan struct{}
	// GuestUptime is the guest's uptime in seconds at ready, if it reported it.
	GuestUptime() float64
	// Exited closes when the VMM process is gone.
	Exited() <-chan struct{}
	Kill() error
}
