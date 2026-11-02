//go:build windows

package main

import "syscall"

func daemonProcAttr() *syscall.SysProcAttr { return nil }

// Windows has no process group to signal. The daemon's context already killed it.
func killDaemonGroup(int) {}
