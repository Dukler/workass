//go:build windows

package acp

import (
	"os/exec"
	"testing"
)

func TestManagedCommandSuppressesWindowsConsoleAllocation(t *testing.T) {
	cmd := configureManagedCommand(exec.Command("cmd.exe", "/c", "exit", "0"))
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatal("managed command does not hide its Windows window")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatal("managed command may still allocate a Windows console")
	}
}
