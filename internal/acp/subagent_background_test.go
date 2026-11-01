package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The live bug of 2026-11-01, on the real stdio path against the mock: a tracked
// subagent starts background work and ends its turn.
//
// Before the fix every one of these fails the same way: the run settles "done"
// on the first end_turn and its session closes over the running work.

type subagentBackgroundFixture struct {
	manager  *Manager
	owner    string
	session  subagentParentSession
	root     string
	traceLog string
}

func newSubagentBackgroundFixture(t *testing.T, name string, opts Options) *subagentBackgroundFixture {
	t.Helper()
	root := repoRoot(t)
	trace := filepath.Join(t.TempDir(), "prompts.jsonl")
	opts.RootDir, opts.StateDir = root, filepath.Join(t.TempDir(), "state")
	opts.Provider = ProviderConfig{
		ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP",
		Env: map[string]string{"WORKASS_MOCK_ACP_TRACE_FILE": trace},
	}
	opts.PermissionTimeout, opts.StdoutFlushInterval, opts.ThoughtFlushInterval, opts.RSSSampleInterval = 8*time.Second, 10*time.Millisecond, 10*time.Millisecond, time.Hour
	opts.SpawnedWorkReconcileInterval = time.Hour
	manager := NewManager(opts)
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	chatID, tabID := "chat-"+name, "tab-"+name
	info, err := manager.NewSession(ctx, SessionOptions{ChatID: chatID, TabID: tabID, CWD: root})
	if err != nil {
		t.Fatalf("new parent session: %v", err)
	}
	manager.mu.Lock()
	owner := manager.agentOwnerBySession[info.SessionID]
	manager.mu.Unlock()
	return &subagentBackgroundFixture{manager: manager, owner: owner, root: root, traceLog: trace,
		session: subagentParentSession{Info: info, ChatID: chatID, TabID: tabID}}
}

func (f *subagentBackgroundFixture) spawn(t *testing.T, prompt string) SubagentRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run, err := f.manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: f.owner, ParentChatID: f.session.ChatID, ParentTabID: f.session.TabID,
		Prompt: prompt, Label: "background child", ProviderID: f.session.Info.ProviderID,
		ModelID: "mock-deterministic", ModeID: "ask", CWD: f.root,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return run
}

func (f *subagentBackgroundFixture) snapshot(id string) SubagentRun {
	f.manager.mu.Lock()
	defer f.manager.mu.Unlock()
	return copySubagentRun(f.manager.subagents[id])
}

func (f *subagentBackgroundFixture) waitPhase(t *testing.T, id, phase string) SubagentRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run := f.snapshot(id)
		if run.Phase == phase && run.Status == "running" {
			return run
		}
		if run.Status != "running" && run.Status != "" {
			t.Fatalf("settled %q before phase %q: %#v", run.Status, phase, run)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never reached phase %q: %#v", phase, f.snapshot(id))
	return SubagentRun{}
}

func (f *subagentBackgroundFixture) waitSettled(t *testing.T, id string) SubagentRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.manager.mu.Lock()
		run := f.manager.subagents[id]
		settled := subagentRunSettled(run)
		f.manager.mu.Unlock()
		if settled {
			return f.snapshot(id)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never settled: %#v", f.snapshot(id))
	return SubagentRun{}
}

func (f *subagentBackgroundFixture) promptsTo(t *testing.T, sessionID string) []string {
	t.Helper()
	data, err := os.ReadFile(f.traceLog)
	if err != nil {
		t.Fatal(err)
	}
	prompts := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry struct{ SessionID, Text string }
		if json.Unmarshal([]byte(line), &entry) == nil && entry.SessionID == sessionID && !strings.HasPrefix(entry.Text, "[mock:lifecycle]") {
			prompts = append(prompts, entry.Text)
		}
	}
	return prompts
}

const backgroundWorkRunning = "[mock:spawned-work-running] "

func TestSubagentWaitsForItsBackgroundWorkAndTheHarnessWakesIt(t *testing.T) {
	t.Parallel()
	f := newSubagentBackgroundFixture(t, "bg-self-wake", Options{SubagentBackgroundSettle: 20 * time.Millisecond})
	release := filepath.Join(t.TempDir(), "release")
	run := f.spawn(t, backgroundWorkRunning+"[mock:background-ends-when="+release+"] [mock:self-wake]")
	waiting := f.waitPhase(t, run.ID, subagentPhaseWaitingBackground)
	if !strings.Contains(waiting.LatestActivity, "Mock background helper") {
		t.Fatalf("a parent reading the list cannot see what it waits for: %#v", waiting)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	settled := f.waitSettled(t, run.ID)
	if settled.Status != "done" || settled.Error != "" || len(settled.BackgroundStopped) != 0 {
		t.Fatalf("a child whose work finished and was read = %#v", settled)
	}
	if !strings.Contains(settled.Result, "Woken by the harness") {
		t.Fatalf("the woken turn's output is not in the result: %q", settled.Result)
	}
	if prompts := f.promptsTo(t, settled.SessionID); len(prompts) != 1 {
		t.Fatalf("a harness that woke the child was also prompted: %q", prompts)
	}
}

// Real Codex never starts a turn by itself, so the daemon has to say so, once.
func TestSubagentThatNothingWakesGetsOnePromptWhenItsWorkEnds(t *testing.T) {
	t.Parallel()
	f := newSubagentBackgroundFixture(t, "bg-no-wake", Options{SubagentBackgroundWakeGrace: 20 * time.Millisecond})
	release := filepath.Join(t.TempDir(), "release")
	run := f.spawn(t, backgroundWorkRunning+"[mock:background-ends-when="+release+"]")
	f.waitPhase(t, run.ID, subagentPhaseWaitingBackground)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	settled := f.waitSettled(t, run.ID)
	if settled.Status != "done" || len(settled.BackgroundStopped) != 0 {
		t.Fatalf("a child woken by prompt = %#v", settled)
	}
	prompts := f.promptsTo(t, settled.SessionID)
	if len(prompts) != 2 {
		t.Fatalf("want the task and one wake, got %q", prompts)
	}
	for _, want := range []string{"Background notification", "Mock background helper", "Mock background helper completed"} {
		if !strings.Contains(prompts[1], want) {
			t.Fatalf("wake prompt has no %q: %q", want, prompts[1])
		}
	}
}

func TestSubagentPastItsWaitCeilingFailsAndNamesTheWorkItLost(t *testing.T) {
	t.Parallel()
	f := newSubagentBackgroundFixture(t, "bg-ceiling", Options{SubagentBackgroundCeiling: 100 * time.Millisecond})
	delivered := make(chan SubagentReceipt, 1)
	if err := f.manager.InstallSubagentCompletionObserver(func(_, _ string, receipt SubagentReceipt) error {
		delivered <- receipt
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	run := f.spawn(t, backgroundWorkRunning+"go")
	settled := f.waitSettled(t, run.ID)
	if settled.Status != "failed" || !strings.Contains(settled.Error, "Mock background helper") || !strings.Contains(settled.Error, "stopped") {
		t.Fatalf("a child that lost its work must not read as anything but failed, with the work named: %#v", settled)
	}
	if len(settled.BackgroundStopped) != 1 || settled.BackgroundStopped[0].Label != "Mock background helper" {
		t.Fatalf("receipt list = %#v", settled.BackgroundStopped)
	}
	if !strings.Contains(settled.Result, "Mock ACP turn") {
		t.Fatalf("the child's own report was dropped: %q", settled.Result)
	}
	select {
	case receipt := <-delivered:
		if receipt.Status != "failed" || len(receipt.BackgroundStopped) != 1 {
			t.Fatalf("the parent's notice = %#v", receipt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parent was never told")
	}
}

func TestSubagentCancelledWhileWaitingStopsAtOnceAndNamesTheWork(t *testing.T) {
	t.Parallel()
	f := newSubagentBackgroundFixture(t, "bg-cancel", Options{})
	run := f.spawn(t, backgroundWorkRunning+"go")
	f.waitPhase(t, run.ID, subagentPhaseWaitingBackground)
	if !f.manager.CancelSubagent(f.owner, f.session.ChatID, f.session.TabID, run.ID) {
		t.Fatal("cancel refused")
	}
	settled := f.waitSettled(t, run.ID)
	if settled.Status != "cancelled" || len(settled.BackgroundStopped) != 1 {
		t.Fatalf("cancelled while waiting = %#v", settled)
	}
}

func TestSubagentMessagedWhileWaitingTakesTheFollowUpAndKeepsWaiting(t *testing.T) {
	t.Parallel()
	f := newSubagentBackgroundFixture(t, "bg-message", Options{})
	run := f.spawn(t, backgroundWorkRunning+"go")
	f.waitPhase(t, run.ID, subagentPhaseWaitingBackground)
	delivery, err := f.manager.MessageSubagent(f.owner, f.session.ChatID, f.session.TabID, run.ID, "the coordinator asks for a status")
	if err != nil || delivery["delivery"] == "uncertain" {
		t.Fatalf("message while waiting = %#v err=%v", delivery, err)
	}
	sessionID := f.snapshot(run.ID).SessionID
	deadline := time.Now().Add(10 * time.Second)
	for {
		prompts := f.promptsTo(t, sessionID)
		if len(prompts) >= 2 && strings.Contains(prompts[len(prompts)-1], "the coordinator asks for a status") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("follow-up was not delivered: %q", prompts)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// It went on waiting, and did not settle over the work, after that turn too.
	f.waitPhase(t, run.ID, subagentPhaseWaitingBackground)
	if !f.manager.CancelSubagent(f.owner, f.session.ChatID, f.session.TabID, run.ID) {
		t.Fatal("cancel refused")
	}
	f.waitSettled(t, run.ID)
}

func TestSubagentWhoseEngineDiesWhileWaitingFails(t *testing.T) {
	t.Parallel()
	f := newSubagentBackgroundFixture(t, "bg-engine-death", Options{})
	run := f.spawn(t, backgroundWorkRunning+"go")
	waiting := f.waitPhase(t, run.ID, subagentPhaseWaitingBackground)
	bridge := f.manager.bridgeForSession(waiting.SessionID, SessionOptions{SessionID: waiting.SessionID})
	if bridge == nil {
		t.Fatal("no bridge")
	}
	bridge.Close(true, errors.New("the engine died"))
	settled := f.waitSettled(t, run.ID)
	if settled.Status != "failed" || !strings.Contains(settled.Error, "engine exited") || !strings.Contains(settled.Error, "Mock background helper") {
		t.Fatalf("engine death while waiting = %#v", settled)
	}
	if len(settled.BackgroundStopped) != 1 || !strings.Contains(settled.Result, "Mock ACP turn") {
		t.Fatalf("the work lost and the child's own report must both survive an engine death: %#v", settled)
	}
	receipts := f.manager.ListSubagentReceipts(f.owner, f.session.ChatID, f.session.TabID, 10)
	if len(receipts) != 1 || !strings.Contains(receipts[0].Result, "Mock ACP turn") || len(receipts[0].BackgroundStopped) != 1 {
		t.Fatalf("durable receipt = %#v", receipts)
	}
}

func TestSubagentWithNoBackgroundWorkSettlesWithoutWaiting(t *testing.T) {
	t.Parallel()
	f := newSubagentBackgroundFixture(t, "bg-none", Options{SubagentBackgroundCeiling: time.Hour})
	run := f.spawn(t, "plain task")
	settled := f.waitSettled(t, run.ID)
	if settled.Status != "done" || len(settled.BackgroundStopped) != 0 {
		t.Fatalf("plain child = %#v", settled)
	}
}

// finishSubagent itself is the last line of defence: whatever the wait did or did
// not see. These cover the paths the wait never reaches.
func TestSubagentCloseReportsEveryKindOfLiveWorkHonestly(t *testing.T) {
	t.Parallel()
	const sessionID = "bg-close-session"
	cases := []struct {
		name       string
		item       SpawnedWorkItem
		background bool
		evidence   *harnessTurnEvidence
		status     string
		stopped    int
	}{
		{name: "owed", item: SpawnedWorkItem{Kind: "bash", Label: "sweep"}, background: true, status: "failed", stopped: 1},
		{name: "service", item: SpawnedWorkItem{Kind: "bash", Label: "dev server", Role: spawnedWorkRoleService}, background: true, status: "done", stopped: 1},
		{name: "foreground", item: SpawnedWorkItem{Kind: "bash", Label: "a call in progress"}, status: "done"},
		{name: "external", item: SpawnedWorkItem{Kind: "external", Label: "workass run"}, background: true, status: "done"},
		{name: "only-the-harness-saw-it", evidence: &harnessTurnEvidence{Tasks: []harnessBackgroundTask{{ID: "unseen-task", Type: "local_bash", Status: "running"}}, TasksKnown: true}, status: "failed", stopped: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			manager := NewManager(Options{StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour})
			t.Cleanup(func() { manager.Reset() })
			id := "close-" + tc.name
			chatID := subagentChatIDPrefix + id
			if tc.item.Label != "" {
				item := tc.item
				item.ID, item.TaskID, item.TabID, item.ChatID, item.Status = "task-1", "task-1", chatID, chatID, "running"
				manager.spawnedWorkMu.Lock()
				manager.spawnedWork[spawnedWorkKey(chatID, chatID, "task-1")] = &spawnedWorkRecord{Item: item, SessionID: sessionID, Backgrounded: tc.background}
				manager.spawnedWorkMu.Unlock()
			}
			if tc.evidence != nil {
				manager.harnessTurns.record(sessionID, tc.evidence)
			}
			run := &SubagentRun{ID: id, Status: "running", SessionID: sessionID, parentChatID: "p", parentTabID: "p", done: make(chan struct{})}
			manager.mu.Lock()
			manager.subagents[id] = run
			manager.mu.Unlock()
			manager.finishSubagent(run, nil, nil)
			if run.Status != tc.status || len(run.BackgroundStopped) != tc.stopped {
				t.Fatalf("status %q stopped %#v, want %q and %d", run.Status, run.BackgroundStopped, tc.status, tc.stopped)
			}
			if tc.status == "failed" && !strings.Contains(run.Error, "stopped") {
				t.Fatalf("error does not say what happened: %q", run.Error)
			}
		})
	}
}

func TestCodexBackgroundCommandIsTrackedOnlyForASubagent(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	event := providerSpawnedWorkUpdate{Kind: "started", Task: providerSpawnedWorkTask{
		TaskID: "codex-exec-call_1", ToolCallID: "call_1", TaskType: "local_bash", Status: "running", Backgrounded: true, Description: "npm run dev",
	}}
	manager.observeProviderSpawnedWork("chat-tab", "chat-1", "thread-1", "codex", event)
	if items := manager.ListSpawnedWork("chat-tab", "chat-1"); len(items) != 0 {
		t.Fatalf("a chat grew a rail row for a command: %#v", items)
	}
	child := subagentChatIDPrefix + "child-1"
	manager.observeProviderSpawnedWork(child, child, "thread-2", "codex", event)
	if owed := manager.subagentOwedBackgroundWork(child, child, "thread-2"); len(owed) != 1 || owed[0].Label != "npm run dev" {
		t.Fatalf("owed = %#v", owed)
	}
	ended := event
	ended.Kind, ended.Task.Status = "progress", "completed"
	manager.observeProviderSpawnedWork(child, child, "thread-2", "codex", ended)
	if owed := manager.subagentOwedBackgroundWork(child, child, "thread-2"); len(owed) != 0 {
		t.Fatalf("a finished command is still owed: %#v", owed)
	}
}

func TestCodexBackgroundCommandDecodesAsBackgroundWork(t *testing.T) {
	t.Parallel()
	strategy := codexProviderSpawnedWorkStrategy{}
	update, ok := strategy.DecodeLifecycle(map[string]any{
		"type": "started", "taskId": "codex-exec-call_1", "toolCallId": "call_1", "description": "sh sweep.sh token=hunter2", "status": "running",
	})
	if !ok || !update.Task.Backgrounded || update.Task.TaskID != "codex-exec-call_1" || update.Task.TaskType != "local_bash" {
		t.Fatalf("decoded = %#v ok=%v", update, ok)
	}
	if strings.Contains(update.Task.Description, "hunter2") {
		t.Fatalf("a secret crossed the decoder: %q", update.Task.Description)
	}
	for _, bad := range []map[string]any{
		{"type": "started", "taskId": "codex-exec-call_1", "toolCallId": "call_2", "status": "running"},
		{"type": "started", "taskId": "codex-exec-call_1", "toolCallId": "call_1", "status": "whatever"},
		{"type": "started", "taskId": "codex-exec-a/b", "toolCallId": "a/b", "status": "running"},
	} {
		if _, ok := strategy.DecodeLifecycle(bad); ok {
			t.Fatalf("accepted %#v", bad)
		}
	}
}
