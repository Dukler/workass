//go:build !windows

package main

import "syscall"

// The daemon leads its own process group, so stopping it stops what it started.
func daemonProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func killDaemonGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
