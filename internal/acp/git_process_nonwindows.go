//go:build !windows

package acp

import (
	"os"
	"os/exec"
	"syscall"
)

func configureGitProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func stopGitProcessTree(process *os.Process, _ processTreeHandle) error {
	if process == nil {
		return nil
	}
	return syscall.Kill(-process.Pid, syscall.SIGKILL)
}
