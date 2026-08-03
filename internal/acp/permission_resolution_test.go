package acp

import (
	"testing"
	"time"
)

func TestPermissionResolutionEmitsTerminalReceiptForDecisionAndCancellation(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast, PermissionTimeout: time.Second})
	t.Cleanup(func() { manager.Reset() })

	run := func(sessionID string, resolve func(id string)) {
		t.Helper()
		result := make(chan string, 1)
		go func() {
			result <- manager.requestPermission(permissionRequest{
				JobID: "visible-job", SessionID: sessionID,
				ToolCall: map[string]any{"title": "Ready to code?", "kind": "execute"},
				Options:  []any{map[string]any{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"}},
			})
		}()
		request := events.waitFor(t, time.Second, func(event collectedEvent) bool {
			if event.channel != "chat:permission-request" {
				return false
			}
			return asString(mapFromAny(event.payload)["sessionId"]) == sessionID
		}).payload.(map[string]any)
		resolve(asString(request["id"]))
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Fatal("permission waiter did not settle")
		}
		resolved := events.waitFor(t, time.Second, func(event collectedEvent) bool {
			if event.channel != "chat:permission-resolved" {
				return false
			}
			return asString(mapFromAny(event.payload)["sessionId"]) == sessionID
		}).payload.(map[string]any)
		if resolved["id"] != request["id"] || resolved["jobId"] != "visible-job" || resolved["sessionId"] != sessionID || resolved["resolvedAt"] == "" {
			t.Fatalf("permission resolution = %#v, request = %#v", resolved, request)
		}
	}

	run("session-selected", func(id string) {
		if !manager.PermissionDecide(id, "allow-once") {
			t.Fatal("permission decision was rejected")
		}
	})
	run("session-cancelled", func(string) { manager.cancelPermissionsForSession("session-cancelled") })
	if pending := manager.PendingPermissions(); len(pending) != 0 {
		t.Fatalf("settled permissions remained pending: %#v", pending)
	}
}
