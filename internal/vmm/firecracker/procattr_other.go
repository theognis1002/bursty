//go:build !linux

package firecracker

import "syscall"

func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
