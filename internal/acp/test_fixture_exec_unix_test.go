//go:build !windows && !plan9 && !js && !wasip1

package acp

import "syscall"

func fixtureExecScript(args, env []string) error {
	return syscall.Exec(args[0], args, env)
}
