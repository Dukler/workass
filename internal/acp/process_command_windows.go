//go:build windows

package acp

import (
	"os/exec"
	"syscall"
)

// CREATE_NO_WINDOW prevents a console subsystem child from allocating its own
// window when Workass itself was launched from the GUI Electron shell.
const createNoWindow = 0x08000000

func configureManagedCommand(cmd *exec.Cmd) *exec.Cmd {
	if cmd == nil {
		return nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	return cmd
}
