//go:build !linux && !windows

package procchild

import (
	"io"
	"syscall"
)

// SurvivalOf measures one pid on a unix with no cgroups. What launchd and the app
// updater take down with the daemon is its process group, so a pid leading a group
// of its own is out of reach and one sharing the daemon's is not.
func SurvivalOf(pid int) Survival {
	group, err := syscall.Getpgid(pid)
	if err != nil {
		return Survival{Why: "its process group could not be read, so nothing can say it is safe"}
	}
	if group == syscall.Getpgrp() {
		return Survival{Why: "it shares this daemon's process group, so anything that stops the group kills it"}
	}
	return Survival{Protected: true, How: "it leads a process group apart from this daemon's"}
}

// EscapeServiceCgroup has nothing to do without cgroups: leaving the caller's
// process group is the whole of it (LeaveCallerProcessGroup).
func EscapeServiceCgroup(string, string, []string, io.Writer) {}

// SupervisorEnvironmentNames has nothing to add on this platform.
func SupervisorEnvironmentNames() []string { return nil }

func supervisorExecutableFrom(_, executable string) string { return executable }
