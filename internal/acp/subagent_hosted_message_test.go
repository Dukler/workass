package acp

import (
	"testing"
)

// A hosted child takes its coordinator's message, from the machine it runs for only,
// and an operation that came twice is one message.
func TestMessageHostedSubagentKeepsToItsMachineAndItsOperation(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), RuntimeProfile: "dev"})
	t.Cleanup(func() { manager.Reset() })
	run := &SubagentRun{ID: "wa-subagent-1-1", Status: "running", originMachine: "m-away", acceptingMessages: true}
	manager.mu.Lock()
	manager.subagents[run.ID] = run
	manager.subagents["wa-subagent-1-2"] = &SubagentRun{ID: "wa-subagent-1-2", Status: "running", acceptingMessages: true}
	manager.mu.Unlock()
	followups := func() int {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(run.followups)
	}

	if _, err := manager.MessageHostedSubagent(run.ID, "m-other", "op-1", "hi"); err == nil || followups() != 0 {
		t.Fatalf("another machine got a message through: err=%v followups=%d", err, followups())
	}
	if _, err := manager.MessageHostedSubagent(run.ID, "", "op-1", "hi"); err == nil || followups() != 0 {
		t.Fatalf("no machine at all got a message through: err=%v followups=%d", err, followups())
	}
	if _, err := manager.MessageHostedSubagent("wa-subagent-1-2", "m-away", "op-1", "hi"); err == nil {
		t.Fatal("a child that runs for a chat here took a message from another machine")
	}
	if _, err := manager.MessageHostedSubagent(run.ID, "m-away", "op-1", "   "); err == nil || followups() != 0 {
		t.Fatalf("an empty message: err=%v followups=%d", err, followups())
	}

	reply, err := manager.MessageHostedSubagent(run.ID, "m-away", "op-1", "look at the logs too")
	if err != nil || reply["ok"] != true || reply["delivery"] != "queued" || followups() != 1 {
		t.Fatalf("message: reply=%#v err=%v followups=%d", reply, err, followups())
	}
	reply, err = manager.MessageHostedSubagent(run.ID, "m-away", "op-1", "look at the logs too")
	if err != nil || reply["ok"] != true || reply["replayed"] != true || followups() != 1 {
		t.Fatalf("the same operation again: reply=%#v err=%v followups=%d", reply, err, followups())
	}
	if _, err := manager.MessageHostedSubagent(run.ID, "m-away", "op-2", "and the config"); err != nil || followups() != 2 {
		t.Fatalf("a new operation: err=%v followups=%d", err, followups())
	}

	manager.mu.Lock()
	run.Status = "done"
	manager.mu.Unlock()
	if _, err := manager.MessageHostedSubagent(run.ID, "m-away", "op-3", "too late"); err == nil || followups() != 2 {
		t.Fatalf("a finished child: err=%v followups=%d", err, followups())
	}
}
