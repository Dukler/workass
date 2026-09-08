//go:build windows

package acp

import (
	"os"
	"os/exec"
)

func configureGitProcess(_ *exec.Cmd) {}

func stopGitProcessTree(process *os.Process, tree processTreeHandle) error {
	if tree.job == 0 {
		if process == nil {
			return nil
		}
		return process.Kill()
	}
	// gitCommandOutput owns the handle until Wait completes. Do not close it
	// here and then close a potentially reused Windows handle in its defer.
	terminated, _, err := procTerminateJobObject.Call(uintptr(tree.job), 1)
	if terminated == 0 {
		return windowsProcessCallError("TerminateJobObject", err)
	}
	return nil
}
