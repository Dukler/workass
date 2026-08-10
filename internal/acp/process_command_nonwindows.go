//go:build !windows

package acp

import "os/exec"

func configureManagedCommand(cmd *exec.Cmd) *exec.Cmd { return cmd }
