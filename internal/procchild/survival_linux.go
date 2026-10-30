package procchild

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SurvivalOf measures one pid against this process's own cgroup.
func SurvivalOf(pid int) Survival {
	self, err := cgroupOf("self")
	if err != nil {
		return Survival{Protected: true, How: "this machine has no cgroup to share"}
	}
	other, err := cgroupOf(fmt.Sprint(pid))
	if err != nil {
		return Survival{Why: "its cgroup could not be read, so nothing can say it is safe"}
	}
	return classifyCgroups(self, other, serviceKillMode)
}

// classifyCgroups is the verdict, apart from the files it reads.
//
// Only a unit's own cgroup is a threat. A daemon started from a shell has no
// service to take anything down with it, and a job in any other cgroup is not
// touched when this unit stops, whatever its KillMode says.
func classifyCgroups(self, other string, killMode func(unit string) string) Survival {
	unit := serviceUnitOf(self)
	if unit == "" {
		return Survival{Protected: true, How: "this daemon is not a systemd service, so no unit stop can reach it"}
	}
	if other != self && !strings.HasPrefix(other, self+"/") {
		return Survival{Protected: true, How: "it runs in its own cgroup, " + cgroupLeaf(other)}
	}
	switch mode := killMode(unit); mode {
	case "process", "none":
		return Survival{Protected: true, How: unit + " has KillMode=" + mode + ", so stopping it leaves other processes running"}
	case "":
		return Survival{Why: "it shares " + unit + "'s cgroup and that unit's KillMode could not be read"}
	default:
		return Survival{Why: fmt.Sprintf("it shares %s's cgroup and that unit has KillMode=%s, so systemd kills it when the daemon exits", unit, mode)}
	}
}

func cgroupOf(pid string) (string, error) {
	raw, err := os.ReadFile("/proc/" + pid + "/cgroup")
	if err != nil {
		return "", err
	}
	return parseCgroup(string(raw)), nil
}

// parseCgroup reads the unified hierarchy's path, or on a v1 host the path of the
// named systemd hierarchy. Those are the two the unit's membership is written in.
func parseCgroup(raw string) string {
	fallback := ""
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 {
			continue
		}
		if fields[0] == "0" && fields[1] == "" {
			return fields[2]
		}
		if fields[1] == "name=systemd" {
			fallback = fields[2]
		}
	}
	return fallback
}

func cgroupLeaf(path string) string {
	path = strings.TrimRight(path, "/")
	if cut := strings.LastIndexByte(path, '/'); cut >= 0 {
		return path[cut+1:]
	}
	return path
}

// serviceUnitOf is the unit a cgroup belongs to. The manager's own user@N.service
// is not one: everything a user runs sits beneath it, and that is a logout rather
// than a stop of this daemon.
func serviceUnitOf(path string) string {
	leaf := cgroupLeaf(path)
	if strings.HasSuffix(leaf, ".service") && !strings.HasPrefix(leaf, "user@") {
		return leaf
	}
	return ""
}

var killModeCache = struct {
	sync.Mutex
	unit, mode string
	at         time.Time
}{}

// serviceKillMode asks systemd, at most every half minute. Survival is measured
// per listed job, and the answer is the same for all of them.
func serviceKillMode(unit string) string {
	killModeCache.Lock()
	defer killModeCache.Unlock()
	if killModeCache.unit == unit && time.Since(killModeCache.at) < 30*time.Second {
		return killModeCache.mode
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=KillMode", "--value", unit).Output()
	mode := ""
	if err == nil {
		mode = strings.TrimSpace(string(out))
	}
	killModeCache.unit, killModeCache.mode, killModeCache.at = unit, mode, time.Now()
	return mode
}

// EscapeServiceCgroup moves this supervisor out of a service's cgroup before it
// has done anything, and returns only if it stayed.
//
// A transient user scope, because that is what a user manager offers with no root
// and no change to the unit: `systemd-run --scope` asks the manager to adopt the
// calling process into a new cgroup and then execs the command, so this is the
// SAME PID throughout. The daemon that launched it and the row it wrote never see
// a different process, and it holds under the stock KillMode=control-group.
//
// A machine with no reachable user manager (a system-level unit, a container)
// cannot do this. The supervisor says so and runs anyway, because the work is
// still worth doing, and SurvivalOf tells everyone else what that means.
func EscapeServiceCgroup(childID, executable string, args []string, diagnostics io.Writer) {
	if strings.TrimSpace(os.Getenv(scopedEnvVar)) != "" {
		return
	}
	self, err := cgroupOf("self")
	if err != nil || serviceUnitOf(self) == "" {
		return
	}
	reason := scopeEscapeUnavailable()
	if reason == "" {
		launcher, lookErr := exec.LookPath("systemd-run")
		if lookErr != nil {
			reason = lookErr.Error()
		} else {
			argv := append([]string{
				"systemd-run", "--user", "--scope", "--collect", "--quiet", "--unit", ScopeUnitName(childID), "--", executable,
			}, args...)
			reason = syscall.Exec(launcher, argv, append(os.Environ(), scopedEnvVar+"=1")).Error()
		}
	}
	fmt.Fprintf(diagnostics, "workass run: NOT ISOLATED: this job stays in %s's cgroup (%s), so stopping or updating that service will kill it\n",
		serviceUnitOf(self), reason)
}

// scopeEscapeUnavailable proves the manager answers before the exec, because after
// the exec a refusal would be the end of the job rather than a sentence in a log.
func scopeEscapeUnavailable() string {
	if !linuxScopeAvailable() {
		return "no reachable systemd user manager"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "--user", "show-environment").Run(); err != nil {
		return "the systemd user manager did not answer: " + err.Error()
	}
	return ""
}

// SupervisorEnvironmentNames are the variables a supervisor needs from the daemon
// to reach the user manager. They name a socket, and hold nothing secret.
func SupervisorEnvironmentNames() []string {
	return []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}
}

func supervisorExecutableFrom(_, executable string) string { return executable }
