package fleetlog

import (
	"os"
	"path/filepath"
	"testing"
)

func openTestMonitors(t *testing.T, dir, name string) *Monitors {
	t.Helper()
	monitors, err := OpenMonitors(dir, name)
	if err != nil {
		t.Fatalf("open the %s registry: %v", name, err)
	}
	return monitors
}

// §13 P3: "a monitor survives both daemons restarting". The registry is the
// durable half at each end, and a restart reads back the same ref, the same
// cursor and the same acknowledgement.
func TestAMonitorSurvivesTheDaemonRestarting(t *testing.T) {
	dir := t.TempDir()
	first := openTestMonitors(t, dir, MonitorsWatched)
	row, err := first.Add(Registration{
		WatcherID: testWatcher, TargetID: testSubagent, Label: "reviewer",
		WatcherChatID: "app-chat-1792187130561", WatcherTabID: "tab-1",
		OriginLaneID: "lane-1", OriginOperationID: "op-1",
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if row.Ref != MonitorRef(testWatcher, testSubagent) {
		t.Fatalf("the ref is not derived from the pair: %q", row.Ref)
	}
	// Registering the same monitor again is one monitor, not two: it is
	// re-established automatically on link up, so idempotence is the ordinary
	// path (§7).
	again, err := first.Add(Registration{WatcherID: testWatcher, TargetID: testSubagent, Label: "ignored"})
	if err != nil || again.Ref != row.Ref || again.Label != "reviewer" {
		t.Fatalf("a repeated registration = %#v (%v)", again, err)
	}
	if all := first.All(); len(all) != 1 {
		t.Fatalf("the registry holds %#v", all)
	}
	if _, ok := first.Update(row.Ref, func(entry *Registration) { entry.AppliedSeq, entry.AckedSeq = 4, 3 }); !ok {
		t.Fatal("the cursor would not advance")
	}

	restarted := openTestMonitors(t, dir, MonitorsWatched)
	restored, ok := restarted.Get(row.Ref)
	if !ok {
		t.Fatal("the monitor did not survive the restart")
	}
	if restored.AppliedSeq != 4 || restored.AckedSeq != 3 {
		t.Fatalf("the restored cursor = applied %d acked %d", restored.AppliedSeq, restored.AckedSeq)
	}
	if restored.WatcherChatID != "app-chat-1792187130561" || restored.OriginLaneID != "lane-1" {
		t.Fatalf("the restored watcher coordinates = %#v", restored)
	}
	if restored.WatcherHome() != "m-away" || restored.TargetHome() != "m-home" {
		t.Fatalf("homes = watcher %q target %q", restored.WatcherHome(), restored.TargetHome())
	}
	if homes := restarted.Homes(); len(homes) != 1 || homes[0] != "m-home" {
		t.Fatalf("Homes() = %#v", homes)
	}
}

// The two directions a home and a watcher each ask about, and the one answer
// the retention sweep reads.
func TestTheRegistryAnswersBothEndsAndTheRetentionSweep(t *testing.T) {
	monitors := openTestMonitors(t, t.TempDir(), MonitorsHeld)
	for index, watcher := range []string{"wa:m-away/chat/a", "wa:m-other/chat/b"} {
		row, err := monitors.Add(Registration{WatcherID: watcher, TargetID: testSubagent})
		if err != nil {
			t.Fatalf("add %s: %v", watcher, err)
		}
		if _, ok := monitors.Update(row.Ref, func(entry *Registration) { entry.AckedSeq = uint64(5 + index) }); !ok {
			t.Fatalf("ack %s", watcher)
		}
	}
	if owed := monitors.ByTarget(testSubagent); len(owed) != 2 {
		t.Fatalf("the home owes %#v", owed)
	}
	if mine := monitors.ByWatcherHome("m-away"); len(mine) != 1 || mine[0].WatcherID != "wa:m-away/chat/a" {
		t.Fatalf("ByWatcherHome = %#v", mine)
	}
	if theirs := monitors.ByTargetHome("m-home"); len(theirs) != 2 {
		t.Fatalf("ByTargetHome = %#v", theirs)
	}
	// The sweep must use the LOWEST acknowledgement, or a terminal event would
	// be evicted while one of two watchers still owed an ack for it.
	through, monitored := monitors.MinAcked(testSubagent)
	if !monitored || through != 5 {
		t.Fatalf("MinAcked = %d, %v", through, monitored)
	}
	if _, monitored := monitors.MinAcked("wa:m-home/subagent/nobody"); monitored {
		t.Fatal("an unmonitored entity reported a monitor")
	}
}

// §13 P3: "a dropped watcher leaks no registration".
func TestRemovingAMonitorLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	monitors := openTestMonitors(t, dir, MonitorsHeld)
	row, err := monitors.Add(Registration{WatcherID: testWatcher, TargetID: testSubagent})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	gone, removed, err := monitors.Remove(row.Ref)
	if err != nil || !removed || gone.TargetID != testSubagent {
		t.Fatalf("remove = %#v, %v, %v", gone, removed, err)
	}
	if _, repeated, err := monitors.Remove(row.Ref); err != nil || repeated {
		t.Fatalf("a repeated remove = %v, %v", repeated, err)
	}
	if all := openTestMonitors(t, dir, MonitorsHeld).All(); len(all) != 0 {
		t.Fatalf("a removed monitor came back: %#v", all)
	}
}

func TestAMonitorNeedsBothEndsNamed(t *testing.T) {
	monitors := openTestMonitors(t, t.TempDir(), MonitorsWatched)
	for _, bad := range [][2]string{{"", testSubagent}, {testWatcher, ""}} {
		if _, err := monitors.Add(Registration{WatcherID: bad[0], TargetID: bad[1]}); err == nil {
			t.Fatalf("Add(%q, %q) was accepted", bad[0], bad[1])
		}
	}
	var absent *Monitors
	if _, err := absent.Add(Registration{WatcherID: testWatcher, TargetID: testSubagent}); err == nil {
		t.Fatal("a nil registry accepted a monitor")
	}
	if _, ok := absent.Get("mon-0"); ok {
		t.Fatal("a nil registry answered Get")
	}
}

// An unreadable registry is kept, not overwritten: a monitor nobody can read is
// still evidence that one existed.
func TestAnUnreadableRegistryIsSetAsideRatherThanDestroyed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet-monitors", MonitorsHeld+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	monitors := openTestMonitors(t, dir, MonitorsHeld)
	if len(monitors.All()) != 0 {
		t.Fatal("an unreadable registry produced monitors")
	}
	if _, err := os.Stat(path + ".unreadable"); err != nil {
		t.Fatalf("the unreadable registry was destroyed: %v", err)
	}
}

func TestHomeOfReadsOnlyAWellFormedGlobalID(t *testing.T) {
	for id, want := range map[string]string{
		testSubagent:            "m-home",
		"wa:m-away/chat/x":      "m-away",
		"app-chat-123":          "",
		"M~m-home~app-chat-123": "",
		"wa:m-home":             "",
		"":                      "",
	} {
		if got := homeOf(id); got != want {
			t.Fatalf("homeOf(%q) = %q, want %q", id, got, want)
		}
	}
}
