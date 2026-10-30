package procchild

import "testing"

func TestAJobInTheServicesCgroupIsExposedUnderTheStockKillMode(t *testing.T) {
	service := "/user.slice/user-1000.slice/user@1000.service/app.slice/workass.service"
	controlGroup := func(string) string { return "control-group" }
	for _, testCase := range []struct {
		name, other string
		killMode    func(string) string
		protected   bool
	}{
		{"the exact failure: same cgroup, stock unit", service, controlGroup, false},
		{"a child cgroup of the unit goes with it", service + "/sub", controlGroup, false},
		{"mixed kills the rest too", service, func(string) string { return "mixed" }, false},
		{"a transient scope is out of reach", "/user.slice/user-1000.slice/user@1000.service/app.slice/workass-job-x.scope", controlGroup, true},
		{"an operator's KillMode=process leaves it alone", service, func(string) string { return "process" }, true},
		{"a KillMode nobody could read is not called safe", service, func(string) string { return "" }, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := classifyCgroups(service, testCase.other, testCase.killMode)
			if got.Protected != testCase.protected {
				t.Fatalf("protected = %v, want %v: %#v", got.Protected, testCase.protected, got)
			}
			if !got.Protected && got.Why == "" {
				t.Fatalf("an exposed job must say why: %#v", got)
			}
		})
	}
}

func TestADaemonThatIsNotAServiceThreatensNothing(t *testing.T) {
	session := "/user.slice/user-1000.slice/session-3.scope"
	if got := classifyCgroups(session, session, func(string) string { return "control-group" }); !got.Protected {
		t.Fatalf("a daemon in a login session has no unit to stop: %#v", got)
	}
	if unit := serviceUnitOf("/user.slice/user-1000.slice/user@1000.service"); unit != "" {
		t.Fatalf("the user manager itself was read as the daemon's unit: %s", unit)
	}
}

func TestCgroupParsing(t *testing.T) {
	if got := parseCgroup("0::/a/b.service\n"); got != "/a/b.service" {
		t.Fatalf("unified: %q", got)
	}
	if got := parseCgroup("12:cpu:/x\n1:name=systemd:/a/b.service\n"); got != "/a/b.service" {
		t.Fatalf("legacy: %q", got)
	}
}
