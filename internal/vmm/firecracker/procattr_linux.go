package firecracker

import "syscall"

// VMs die with the daemon: a VMM without its orchestrator is unmanaged state.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
