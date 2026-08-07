//go:build !windows

package acp

import "os"

func stopProcessTree(process *os.Process) error {
	if process == nil {
		return nil
	}
	return process.Kill()
}
