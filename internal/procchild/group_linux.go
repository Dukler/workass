package procchild

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Linux adds one thing the rest of unix cannot offer: a transient
// `systemd --user` scope, which is a CGROUP rather than a process group.
//
// It matters for exactly one failure: a child that changes its own process
// group — a daemonizing server, a container runtime, a script that calls
// setsid — leaves the supervisor's stop reaching nothing, and the work keeps
// running after the row says it stopped. A cgroup cannot be left by its
// members, so `systemctl --user stop` reaches the whole tree whatever the
// child did to itself.
//
// It is used only when it is really there. `systemd-run --user` without a
// reachable user bus fails with a message about D-Bus and the lane dies for a
// reason that has nothing to do with the work, so the probe below is a
// precondition, not an optimisation. duklerver runs the daemon under
// `systemd --user` with linger enabled, so the bus is present there; a
// container or a bare `su` session has none and falls back to the process
// group, which is what every other unix uses anyway.
func init() {
	scopeAvailable = linuxScopeAvailable
	wrapForScope = linuxWrapForScope
	scopeStop = linuxScopeStop
}

func linuxScopeAvailable() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		// Without systemctl the scope could be started and never stopped,
		// which is strictly worse than a process group.
		return false
	}
	if strings.TrimSpace(os.Getenv("DBUS_SESSION_BUS_ADDRESS")) != "" {
		return true
	}
	runtime := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if runtime == "" {
		return false
	}
	for _, candidate := range []string{"bus", filepath.Join("systemd", "private")} {
		if _, err := os.Stat(filepath.Join(runtime, candidate)); err == nil {
			return true
		}
	}
	return false
}

// linuxWrapForScope prefixes the command with the transient-scope launcher.
//
// --scope keeps the supervisor as the waiting parent, so the exit status is
// still read by waitpid and no journal parsing is involved; --collect removes
// the unit when it ends, so a failed run leaves nothing to clean up by hand.
func linuxWrapForScope(unit string, command []string) []string {
	if len(command) == 0 {
		return command
	}
	wrapped := []string{"systemd-run", "--user", "--scope", "--collect", "--quiet", "--unit", unit, "--"}
	return append(wrapped, command...)
}

// linuxScopeStopArgs is split from the call so the argv can be asserted
// without a systemd on the test host: a graceful stop and a kill are different
// subcommands, and getting them the wrong way round would turn every polite
// stop into a kill with no grace.
func linuxScopeStopArgs(unit string, kill bool) []string {
	if kill {
		return []string{"--user", "kill", "--signal=SIGKILL", unit}
	}
	return []string{"--user", "stop", unit}
}

func linuxScopeStop(unit string, kill bool) error {
	unit = strings.TrimSpace(unit)
	if unit == "" {
		return errors.New("no scope unit to stop")
	}
	return exec.Command("systemctl", linuxScopeStopArgs(unit, kill)...).Run()
}
