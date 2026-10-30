//go:build linux

package procchild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Linux's transient scope, asserted without needing a systemd on the test
// host. The argv and the probe are the whole of the Linux-specific behaviour;
// the cgroup itself is the kernel's, and a live run on a machine with a user
// bus is the only thing that can prove that part.

func TestLinuxScopeArgvWrapsTheCommandWithoutRewritingIt(t *testing.T) {
	wrapped := linuxWrapForScope("workass-run-xwabc123456", []string{"python", "train.py", "--epochs", "3"})
	want := "systemd-run --user --scope --collect --quiet --unit workass-run-xwabc123456 -- python train.py --epochs 3"
	if got := strings.Join(wrapped, " "); got != want {
		t.Fatalf("scope argv = %q, want %q", got, want)
	}
	// `--` matters: a job flag that looks like a systemd-run flag must reach
	// the job, not the launcher.
	if wrapped[len(wrapped)-4] != "--" && !strings.Contains(strings.Join(wrapped, " "), " -- ") {
		t.Fatal("the scope launcher must separate its own flags from the job's")
	}
	if len(linuxWrapForScope("unit", nil)) != 0 {
		t.Fatal("an empty command must not be wrapped into a launcher with no job")
	}
}

func TestLinuxScopeStopUsesStopForGracefulAndKillForTheKill(t *testing.T) {
	graceful := strings.Join(linuxScopeStopArgs("workass-run-xwabc123456", false), " ")
	if graceful != "--user stop workass-run-xwabc123456" {
		t.Fatalf("graceful stop argv = %q", graceful)
	}
	kill := strings.Join(linuxScopeStopArgs("workass-run-xwabc123456", true), " ")
	if kill != "--user kill --signal=SIGKILL workass-run-xwabc123456" {
		// Getting these the wrong way round would turn every polite stop into
		// a kill with no grace.
		t.Fatalf("kill argv = %q", kill)
	}
}

func TestLinuxScopeIsUsedOnlyWhenAUserBusIsReallyThere(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	if linuxScopeAvailable() {
		// systemd-run --user without a reachable bus fails with a message
		// about D-Bus, and the lane would die for a reason that has nothing to
		// do with the work.
		t.Fatal("no bus and no socket must mean no scope")
	}
	if err := os.WriteFile(filepath.Join(runtime, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Whether it is now available still depends on systemd-run being installed
	// on this host, which is the other half of the precondition.
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	if _, err := os.Stat("/usr/bin/systemd-run"); err == nil {
		if !linuxScopeAvailable() {
			t.Fatal("a bus address and systemd-run present must mean the scope is available")
		}
	} else if linuxScopeAvailable() {
		t.Fatal("without systemd-run there is no scope to start, whatever the bus says")
	}
}

func TestNewGroupFallsBackToAProcessGroupWithoutAScope(t *testing.T) {
	previous := scopeAvailable
	t.Cleanup(func() { scopeAvailable = previous })
	scopeAvailable = func() bool { return false }
	group, command := newGroup("xwabc123456", []string{"sh", "-c", "true"})
	if group.Kind() != GroupProcessGroup {
		t.Fatalf("isolation = %q, want a process group", group.Kind())
	}
	if strings.Join(command, " ") != "sh -c true" {
		t.Fatalf("the command must be untouched without a scope, got %q", strings.Join(command, " "))
	}
	scopeAvailable = func() bool { return true }
	group, command = newGroup("xwabc123456", []string{"sh", "-c", "true"})
	if group.Kind() != GroupSystemdScope || command[0] != "systemd-run" {
		t.Fatalf("with a scope available the command must be wrapped, got %q / %q", group.Kind(), command[0])
	}
}
