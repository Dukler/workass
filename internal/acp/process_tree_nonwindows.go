//go:build !windows

package acp

import (
	"os"
	"os/exec"
)

type processTreeHandle struct{}

func startProcessTree(cmd *exec.Cmd) (processTreeHandle, error) {
	if err := cmd.Start(); err != nil {
		return processTreeHandle{}, err
	}
	return processTreeHandle{}, nil
}

func stopProcessTree(process *os.Process, _ processTreeHandle) error {
	if process == nil {
		return nil
	}
	return process.Kill()
}

func releaseProcessTree(_ processTreeHandle) error { return nil }
