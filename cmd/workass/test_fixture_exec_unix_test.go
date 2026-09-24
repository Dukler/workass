//go:build !windows && !plan9 && !js && !wasip1

package main

import "syscall"

func wireFixtureExecScript(args, env []string) error {
	return syscall.Exec(args[0], args, env)
}
