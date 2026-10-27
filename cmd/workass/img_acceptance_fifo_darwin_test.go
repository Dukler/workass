//go:build darwin

package main

import "golang.org/x/sys/unix"

const unixONonblock = unix.O_NONBLOCK

func unixMkfifo(path string) error { return unix.Mkfifo(path, 0o600) }
