package acp

import (
	"strings"
	"testing"
	"time"
)

// The option reading is the part that decides what actually happens to the
// user's machine, so it is pinned separately from the authority rule. A bare
// "no" inside an allow label must never read as a rejection.
func TestPermissionOptionForDecisionReadsAllowAndReject(t *testing.T) {
	options := []any{
		map[string]any{"optionId": "allow-1", "name": "Allow, no confirmation", "kind": "allow_once"},
		map[string]any{"optionId": "reject-1", "name": "Reject", "kind": "reject_once"},
	}
	if got, ok := permissionOptionForDecision(options, subagentPermissionAllow); !ok || got != "allow-1" {
		t.Fatalf("allow selected %q ok=%v, want allow-1", got, ok)
	}
	if got, ok := permissionOptionForDecision(options, subagentPermissionDeny); !ok || got != "reject-1" {
		t.Fatalf("deny selected %q ok=%v, want reject-1", got, ok)
	}
	if _, ok := permissionOptionForDecision(nil, subagentPermissionAllow); ok {
		t.Fatal("an empty option list produced a decision")
	}
}

// A parent whose own mode still prompts holds no authority to hand down, so it
// may not grant one on a child's behalf. This is the boundary the whole feature
// rests on: without it, a chat could widen its own reach by spawning.
func TestParentMayGrantOnlyFromItsOwnFullAccessMode(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })

	if _, allowed := manager.parentMayGrantSubagentPermission(nil); allowed {
		t.Fatal("a nil parent was allowed to grant")
	}
	// No effective mode at all is not authority either.
	if _, allowed := manager.parentMayGrantSubagentPermission(&Job{ProviderID: "claude"}); allowed {
		t.Fatal("a parent with no mode was allowed to grant")
	}
}

// The deadlock this exists to end: a child parks on a card, and the parent
// answers it without a human. Deny is the half every parent may always use.
func TestParentDenyReleasesAParkedSubagentPermission(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })

	done := make(chan string, 1)
	go func() {
		done <- manager.requestPermission(permissionRequest{
			JobID: "parent-job", SessionID: "sess-child", Subagent: true,
			ToolCall: map[string]any{"title": "Write /tmp/out", "kind": "edit"},
			Options: []any{
				map[string]any{"optionId": "allow-1", "name": "Allow", "kind": "allow_once"},
				map[string]any{"optionId": "reject-1", "name": "Reject", "kind": "reject_once"},
			},
			FallbackOptionID: "reject-1",
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	permissionID, optionID := "", ""
	for time.Now().Before(deadline) {
		var ok bool
		if permissionID, optionID, ok = manager.pendingSubagentPermission("sess-child", subagentPermissionDeny); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if permissionID == "" || optionID != "reject-1" {
		t.Fatalf("pending lookup = %q/%q, want a request resolving to reject-1", permissionID, optionID)
	}
	manager.PermissionDecide(permissionID, optionID)

	select {
	case got := <-done:
		if got != "reject-1" {
			t.Fatalf("subagent received %q, want reject-1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the subagent stayed parked after the parent answered")
	}
}

// A session with nothing waiting must say so rather than resolve something
// else's card: the lookup is keyed by the child's session precisely so one
// subagent cannot answer another's request.
func TestPendingSubagentPermissionIgnoresOtherSessions(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })

	go manager.requestPermission(permissionRequest{
		JobID: "parent-job", SessionID: "sess-other", Subagent: true,
		ToolCall: map[string]any{"title": "Write", "kind": "edit"},
		Options:  []any{map[string]any{"optionId": "reject-1", "name": "Reject", "kind": "reject_once"}},
	})
	time.Sleep(50 * time.Millisecond)

	if _, _, ok := manager.pendingSubagentPermission("sess-child", subagentPermissionDeny); ok {
		t.Fatal("a request from another session was offered as this one's")
	}
	if _, _, ok := manager.pendingSubagentPermission("", subagentPermissionDeny); ok {
		t.Fatal("an empty session id matched a pending request")
	}
}

// A run the registry no longer holds — the shape a daemon restart leaves behind,
// since the subagent registry is in memory only — must fail with a reason the
// parent can act on instead of a bare false.
func TestDecideSubagentPermissionRejectsUnknownRunWithAReason(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })

	if _, err := manager.DecideSubagentPermission("", "chat-1", "tab-1", "", "allow"); err == nil {
		t.Fatal("an empty subagent id was accepted")
	}
	if _, err := manager.DecideSubagentPermission("", "chat-1", "tab-1", "wa-subagent-gone", "maybe"); err == nil ||
		!strings.Contains(err.Error(), "allow") {
		t.Fatalf("an invalid decision was accepted or misreported: %v", err)
	}
}

// Plan mode ends by asking a human to approve ExitPlanMode, so the most
// conservative-looking permission_intent was the only one that could deadlock.
// A subagent's plan handshake now answers itself with "keep planning": it grants
// nothing, and analysis in prose is what the narrowing was chosen for.
func TestSubagentExitPlanModeAnswersItselfWithoutGranting(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })

	got := manager.requestPermission(permissionRequest{
		JobID: "parent-job", SessionID: "sess-plan", Subagent: true,
		ToolCall: map[string]any{"title": "ExitPlanMode", "kind": "other"},
		Options: []any{
			map[string]any{"optionId": "allow-1", "name": "Yes, proceed", "kind": "allow_once"},
			map[string]any{"optionId": "reject-1", "name": "Keep planning", "kind": "reject_once"},
		},
	})
	if got != "reject-1" {
		t.Fatalf("subagent plan handshake resolved to %q, want the keep-planning option", got)
	}
}

// The same handshake raised by the USER's own chat still belongs to the user:
// only a subagent's is answered for it.
func TestUserExitPlanModeIsStillTheUsersToAnswer(t *testing.T) {
	manager := NewManager(Options{PermissionTimeout: 80 * time.Millisecond})
	t.Cleanup(func() { manager.Reset() })

	got := manager.requestPermission(permissionRequest{
		JobID: "user-job", SessionID: "sess-user",
		ToolCall: map[string]any{"title": "ExitPlanMode", "kind": "other"},
		Options: []any{
			map[string]any{"optionId": "allow-1", "name": "Yes, proceed", "kind": "allow_once"},
			map[string]any{"optionId": "reject-1", "name": "Keep planning", "kind": "reject_once"},
		},
		FallbackOptionID: "fallback-used",
	})
	if got != "fallback-used" {
		t.Fatalf("a user's plan handshake resolved to %q; it must wait for the card, not be answered for them", got)
	}
}

func TestExitPlanModeRecognitionSurvivesNamingShapes(t *testing.T) {
	for _, shape := range []map[string]any{
		{"title": "ExitPlanMode"},
		{"kind": "exit_plan_mode"},
		{"toolName": "exitPlanMode"},
		{"title": "Exit Plan Mode"},
	} {
		if !isExitPlanModeToolCall(shape) {
			t.Fatalf("%v was not recognised as the plan handshake", shape)
		}
	}
	for _, shape := range []map[string]any{
		{"title": "Write"},
		{"title": "Plan the migration"},
		{"kind": "edit"},
	} {
		if isExitPlanModeToolCall(shape) {
			t.Fatalf("%v was wrongly read as the plan handshake", shape)
		}
	}
}

// An attention the parent cannot grant must say so, or the parent's only
// rational move is to keep waiting on someone who is not at the machine.
func TestPermissionAttentionNamesWhoCanGrantIt(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })

	run := &SubagentRun{ID: "child", Status: "running", Phase: "working", ParentJobID: "parent-job"}
	manager.mu.Lock()
	manager.subagents[run.ID] = run
	manager.mu.Unlock()

	manager.notifySubagentPermissionForJob(&Job{SubagentID: run.ID}, "Write /tmp/out")

	manager.mu.Lock()
	attention := run.Attention
	manager.mu.Unlock()
	if attention == nil || attention.GrantableBy != "human" {
		t.Fatalf("attention = %#v, want grantableBy human for a parent with no full-access mode", attention)
	}
	if !strings.Contains(attention.Message, "only the user can grant") {
		t.Fatalf("attention message %q does not tell the parent to stop waiting", attention.Message)
	}
}

// The mode lookup behind grantableBy reaches bridgeForSession, which takes the
// manager mutex. Resolving it inside the notify lock deadlocks the daemon the
// first time a subagent asks for anything while its parent job is still live —
// and every earlier test missed it by leaving m.jobs empty, so the parent
// resolved to nil and never reached the lookup. This one keeps a real parent.
func TestPermissionAttentionDoesNotDeadlockWithALiveParentJob(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })

	parent := &Job{ID: "parent-job", ChatID: "chat-1", TabID: "tab-1", Status: "running", SessionID: "sess-parent", ProviderID: "claude"}
	run := &SubagentRun{ID: "child", Status: "running", Phase: "working", ParentJobID: "parent-job"}
	manager.mu.Lock()
	manager.jobs[parent.ID] = parent
	manager.subagents[run.ID] = run
	manager.mu.Unlock()

	settled := make(chan struct{})
	go func() {
		manager.notifySubagentPermissionForJob(&Job{SubagentID: run.ID}, "Write /tmp/out")
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("notifySubagentPermissionForJob deadlocked with a live parent job")
	}

	manager.mu.Lock()
	attention := run.Attention
	manager.mu.Unlock()
	if attention == nil || attention.GrantableBy == "" {
		t.Fatalf("attention = %#v, want a grantableBy verdict", attention)
	}
}
