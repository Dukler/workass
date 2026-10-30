package procchild

import "os/exec"

// Windows has no signals, so a child that ends abnormally has a real exit code
// and nothing has to be reconstructed from a wait status.
func signalledExitCode(err *exec.ExitError) int {
	if code := err.ExitCode(); code >= 0 {
		return code
	}
	return ExitSupervisorFailed
}
