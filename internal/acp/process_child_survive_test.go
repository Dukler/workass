package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/procchild"
)

// A job that ended is still readable. A chat's settled row is compacted into its
// receipt, and the read that said "no process child is tracked" came minutes after
// the job was lost, which is exactly when somebody asks.
func TestAnEndedChatBoundJobStaysReadableFromItsReceipt(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, Logf: func(string, map[string]any) {}})
	oldID, err := procchild.IDForOperation("the-old-chat-bound-job")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateDir, "spawned-work-receipts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt, err := json.Marshal(SpawnedWorkReceipt{
		ReceiptID: "spawned-" + oldID, TaskID: oldID, TabID: "host-tab", ChatID: "host-chat", Kind: "external",
		Label: "training", Status: "orphaned", Outcome: procchild.OutcomeLost, FinishedAt: "2026-10-30T14:31:10Z",
		Summary: "Lost: its supervisor is gone", OutputTail: "update 10464",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host-tab.jsonl"), append(receipt, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	answer, err := manager.ReadProcessChild(oldID, 4096, 0)
	if err != nil {
		t.Fatalf("an ended job could not be read: %v", err)
	}
	view := answer["child"].(ProcessChildView)
	if view.Outcome != procchild.OutcomeLost || view.Status != "orphaned" || answer["output_tail"] != "update 10464" {
		t.Fatalf("the read: %#v", answer)
	}
	neverID, _ := procchild.IDForOperation("a-job-that-never-was")
	if _, err := manager.ReadProcessChild(neverID, 4096, 0); err == nil {
		t.Fatal("a job that never existed read as one")
	}
}

// A row from before the wait and the run were split: the supervisor wrote when its
// command started into the outbox, whether or not anybody heard. With that it gets
// a real run time, and with nothing it gets no run time at all, because a number
// that includes the queue is not one.
func TestALostOldRowReadsItsStartFromTheOutboxOrClaimsNoRunTime(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, Logf: func(string, map[string]any) {}})
	outbox, err := procchild.OpenOutbox(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	if _, err := outbox.Append("xwstamped", procchild.KindCreated, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Append("xwstamped", procchild.KindStarted, map[string]any{"pid": 7}); err != nil {
		t.Fatal(err)
	}
	queued := time.Now().UTC().Add(-8 * time.Hour)
	pid := 999999
	rec := &spawnedWorkRecord{Item: SpawnedWorkItem{
		ID: "xwstamped", TaskID: "xwstamped", TabID: "t", ChatID: "c", Kind: "external", Status: "running",
		StartedAt: queued.Format(time.RFC3339Nano), PID: &pid,
	}}
	manager.backfillProcessChildStart(rec)
	if rec.Item.ExecStartedAt == "" {
		t.Fatal("the start in the outbox was not read")
	}
	times := ProcessChildTimes(rec.Item, time.Now().UTC())
	if times.Unsplit || times.QueuedMs < (7*time.Hour).Milliseconds() || times.RunMs > time.Minute.Milliseconds() {
		t.Fatalf("times: %+v", times)
	}

	unstamped := SpawnedWorkItem{
		ID: "xwbare", TaskID: "xwbare", Kind: "external", Status: "orphaned", StartedAt: queued.Format(time.RFC3339Nano),
		FinishedAt: time.Now().UTC().Format(time.RFC3339Nano), PID: &pid,
	}
	supervisor := 4242
	unstamped.SupervisorPID = &supervisor
	receipt := manager.spawnedWorkReceiptFromItem(unstamped)
	if receipt.RunMs != 0 || receipt.ElapsedMs < (7*time.Hour).Milliseconds() {
		t.Fatalf("a job nobody can split must report elapsed and no run time: %+v", receipt)
	}
}

func TestLostSaysWhenTheWorkIsStillRunning(t *testing.T) {
	pid := os.Getpid()
	summary := lostProcessChildSummary(&spawnedWorkRecord{Item: SpawnedWorkItem{PID: &pid}})
	if !strings.Contains(summary, "still running") {
		t.Fatalf("summary: %s", summary)
	}
	if summary := lostProcessChildSummary(&spawnedWorkRecord{}); strings.Contains(summary, "still running") {
		t.Fatalf("summary: %s", summary)
	}
}

// The updater reads this: every running job, and whether it would outlive the daemon.
func TestRunningJobSurvivalMeasuresEachLiveSupervisor(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, Logf: func(string, map[string]any) {}})
	pid := os.Getpid()
	manager.spawnedWorkMu.Lock()
	manager.spawnedWork[spawnedWorkKey("", "", "xwlive")] = &spawnedWorkRecord{Item: SpawnedWorkItem{
		ID: "xwlive", TaskID: "xwlive", Kind: "external", Label: "train", Status: "running", Owner: "wa:m/chat/c",
		StartedAt: isoNow(), ExecStartedAt: isoNow(), SupervisorPID: &pid,
	}}
	manager.spawnedWorkMu.Unlock()
	jobs := manager.RunningJobSurvival()
	if len(jobs) != 1 || jobs[0].WorkID != "xwlive" || jobs[0].Note == "" {
		t.Fatalf("jobs: %#v", jobs)
	}
	// On this machine a pid in this daemon's own process group is exactly what is
	// not safe, and a test process is in its own group by construction.
	if survival := procchild.SurvivalOf(pid); survival.Protected != jobs[0].Protected {
		t.Fatalf("measured %v, reported %v", survival.Protected, jobs[0].Protected)
	}
}

// A subagent never survives its daemon, whichever way it runs here: tracked, hosted
// for another machine, or native to a provider. Work another machine runs is not
// this machine's to report, and a job keeps its own measurement.
func TestRunningJobSurvivalCountsEverySubagentAsDying(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), Logf: func(string, map[string]any) {}})
	manager.mu.Lock()
	manager.subagents["wa-local"] = &SubagentRun{ID: "wa-local", Label: "local", Status: "running"}
	manager.subagents["wa-hosted"] = &SubagentRun{ID: "wa-hosted", Label: "hosted", Status: "running", originMachine: "m-owner"}
	manager.subagents["wa-ended"] = &SubagentRun{ID: "wa-ended", Label: "ended", Status: "completed"}
	manager.mu.Unlock()
	row := func(id, kind, status, host string) {
		manager.spawnedWork[spawnedWorkKey("tab", "chat", id)] = &spawnedWorkRecord{Item: SpawnedWorkItem{
			ID: id, TaskID: id, TabID: "tab", ChatID: "chat", Kind: kind, Label: id, Status: status, HostMachine: host,
		}}
	}
	manager.spawnedWorkMu.Lock()
	row("wa-local", trackedSubagentSpawnedWorkKind, "running", "")
	row("native", "agent", "running", "")
	row("native-ended", "agent", "exited", "")
	row("elsewhere", trackedSubagentSpawnedWorkKind, "running", "m-host")
	row("lane", "external", "running", "")
	manager.spawnedWorkMu.Unlock()

	got := map[string]JobSurvival{}
	for _, job := range manager.RunningJobSurvival() {
		got[job.WorkID] = job
	}
	if len(got) != 3 {
		t.Fatalf("listed: %#v", got)
	}
	for _, id := range []string{"wa-local", "wa-hosted", "native"} {
		if job := got[id]; job.Kind != "subagent" || job.Protected || job.Note == "" {
			t.Fatalf("%s: %#v", id, job)
		}
	}
	if got["wa-hosted"].Owner != "m-owner" {
		t.Fatalf("hosted owner: %#v", got["wa-hosted"])
	}
}
