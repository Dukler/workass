package procchild

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Whether a job outlives this daemon going away.
//
// docs/specs/fleet-runtime.md §14g. A process group does not escape a cgroup, so
// "it is in its own process group" was never the answer: on Linux under the stock
// KillMode=control-group, systemd kills everything left in the unit's cgroup when
// the daemon exits, and a 3-hour training run went with it at update 10,464 of
// 12,000. So this is measured for the actual pid, every time it is asked, and not
// remembered from what somebody intended. That also makes the answer true for a
// supervisor started by a build that knew none of this.

// Survival is one pid's prospects when this daemon stops.
type Survival struct {
	// Protected means stopping or updating this daemon will not kill it.
	Protected bool
	// How says why it is protected, or Why says why it is not.
	How string
	Why string
}

// scopedEnvVar marks a supervisor that has already been moved. Without it, a
// machine where the move succeeded and changed nothing would re-exec forever.
const scopedEnvVar = "WORKASS_RUN_SCOPED"

// ScopeUnitName is the transient unit one supervisor runs in, separate from the
// child's own `workass-run-` scope so a stop aimed at the child cannot reach it.
func ScopeUnitName(childID string) string {
	return "workass-job-" + strings.TrimSpace(childID)
}

// AwaitSurvival gives a supervisor that has just been launched a moment to move
// itself, then measures. The move happens in its first milliseconds, before it
// has done anything, so a job still exposed after this is really exposed.
func AwaitSurvival(pid int, wait time.Duration) Survival {
	deadline := time.Now().Add(wait)
	for {
		survival := SurvivalOf(pid)
		if survival.Protected || !PIDAlive(pid) || time.Now().After(deadline) {
			return survival
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// SupervisorExecutable is the file to launch a supervisor from. It is the running
// binary everywhere except Windows (supervisor_executable_windows.go).
func SupervisorExecutable(stateDir string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return supervisorExecutableFrom(strings.TrimSpace(stateDir), filepath.Clean(executable)), nil
}
