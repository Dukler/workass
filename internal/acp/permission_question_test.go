package acp

import (
	"strings"
	"testing"
	"time"
)

// A question from the agent (AskUserQuestion) reaches the client down the
// permission channel, because the SDK has no other one. The payload must carry
// what is being ASKED — dropping it is what rendered a permission card with no
// question in it.
func TestPermissionRequestForwardsTheAgentsQuestion(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast, PermissionTimeout: time.Second})
	t.Cleanup(func() { manager.Reset() })

	go manager.requestPermission(permissionRequest{
		JobID: "visible-job", SessionID: "sess-question",
		ToolCall: map[string]any{
			"title": "Deploy target",
			"kind":  "other",
			"rawInput": map[string]any{
				"question":    "¿A qué máquina deployamos primero?",
				"header":      "Deploy target",
				"multiSelect": false,
				"options": []any{
					map[string]any{"label": "El nodo de build", "description": "Canary; ya tiene el artefacto"},
					map[string]any{"label": "Gold", "description": "Producción, sigue offline"},
				},
			},
		},
		Options: []any{
			map[string]any{"optionId": "answer-0", "name": "El nodo de build", "kind": "answer"},
			map[string]any{"optionId": "answer-1", "name": "Gold", "kind": "answer"},
			map[string]any{"optionId": "question-cancel", "name": "Responder en el chat", "kind": "reject_once"},
		},
	})

	payload := events.waitFor(t, time.Second, func(event collectedEvent) bool {
		return event.channel == "chat:permission-request" &&
			asString(mapFromAny(event.payload)["sessionId"]) == "sess-question"
	}).payload.(map[string]any)

	question := mapFromAny(payload["question"])
	if question == nil {
		t.Fatal("permission payload dropped the question")
	}
	if got := asString(question["question"]); got != "¿A qué máquina deployamos primero?" {
		t.Fatalf("question text = %q", got)
	}
	if got := asString(question["header"]); got != "Deploy target" {
		t.Fatalf("header = %q", got)
	}
	options := anySlice(question["options"])
	if len(options) != 2 {
		t.Fatalf("choices = %d, want 2", len(options))
	}
	if got := asString(mapFromAny(options[0])["description"]); got != "Canary; ya tiene el artefacto" {
		t.Fatalf("first choice description = %q", got)
	}
	// The answers must survive as their own option kind: labelling them
	// allow/reject is what let the renderer overwrite them with "Permitir una vez".
	first := mapFromAny(anySlice(payload["options"])[0])
	if asString(first["kind"]) != "answer" || asString(first["name"]) != "El nodo de build" {
		t.Fatalf("first option = %+v", first)
	}
}

// An ordinary tool permission has no question, and must not grow one.
func TestPermissionRequestWithoutQuestionStaysAPlainPermission(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast, PermissionTimeout: time.Second})
	t.Cleanup(func() { manager.Reset() })

	go manager.requestPermission(permissionRequest{
		JobID: "visible-job", SessionID: "sess-plain",
		ToolCall: map[string]any{"title": "Run npm test", "kind": "execute", "rawInput": map[string]any{"command": "npm test"}},
		Options:  []any{map[string]any{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"}},
	})

	payload := events.waitFor(t, time.Second, func(event collectedEvent) bool {
		return event.channel == "chat:permission-request" &&
			asString(mapFromAny(event.payload)["sessionId"]) == "sess-plain"
	}).payload.(map[string]any)
	if _, present := payload["question"]; present {
		t.Fatalf("plain permission grew a question: %+v", payload["question"])
	}
}

// The reported symptom behind this: the agent asked, nobody was at the machine,
// and it finished the whole job anyway. A tool permission fails closed on a
// deadline; a question addressed to a human must not answer itself.
func TestQuestionWaitsForTheUserWhileAPermissionStillExpires(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast, PermissionTimeout: 120 * time.Millisecond})
	t.Cleanup(func() { manager.Reset() })

	question := make(chan string, 1)
	go func() {
		question <- manager.requestPermission(permissionRequest{
			JobID: "job-question", SessionID: "sess-waits",
			ToolCall: map[string]any{"title": "Deploy target", "kind": "other", "rawInput": map[string]any{
				"question": "¿Deployamos ahora?",
				"options":  []any{map[string]any{"label": "Sí"}, map[string]any{"label": "No"}},
			}},
			Options: []any{
				map[string]any{"optionId": "answer-0", "name": "Sí", "kind": "answer"},
				map[string]any{"optionId": "question-cancel", "name": "Responder en el chat", "kind": "reject_once"},
			},
			FallbackOptionID: "question-cancel",
		})
	}()
	request := events.waitFor(t, time.Second, func(event collectedEvent) bool {
		return event.channel == "chat:permission-request" &&
			asString(mapFromAny(event.payload)["sessionId"]) == "sess-waits"
	}).payload.(map[string]any)

	select {
	case answer := <-question:
		t.Fatalf("the question answered itself as %q instead of waiting for the user", answer)
	case <-time.After(500 * time.Millisecond): // four times the deadline
	}

	// And it is still answerable afterwards — parking is not hanging.
	manager.PermissionDecide(asString(request["id"]), "answer-0")
	select {
	case answer := <-question:
		if answer != "answer-0" {
			t.Fatalf("answer = %q", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("the parked question never settled after the user answered")
	}

	// The fail-closed deadline still governs an ordinary tool permission.
	permission := make(chan string, 1)
	go func() {
		permission <- manager.requestPermission(permissionRequest{
			JobID: "job-plain", SessionID: "sess-expires",
			ToolCall:         map[string]any{"title": "Run npm test", "kind": "execute"},
			Options:          []any{map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"}},
			FallbackOptionID: "deny",
		})
	}()
	select {
	case answer := <-permission:
		if answer != "deny" {
			t.Fatalf("expired permission = %q, want the fail-closed fallback", answer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an unattended tool permission no longer expires")
	}
}

// A parked question must not survive its turn: cancelling settles it.
func TestCancellingASessionSettlesAParkedQuestion(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast, PermissionTimeout: 120 * time.Millisecond})
	t.Cleanup(func() { manager.Reset() })

	done := make(chan string, 1)
	go func() {
		done <- manager.requestPermission(permissionRequest{
			JobID: "job-parked", SessionID: "sess-cancelled",
			ToolCall: map[string]any{"title": "Deploy target", "kind": "other", "rawInput": map[string]any{
				"question": "¿Seguimos?",
				"options":  []any{map[string]any{"label": "Sí"}, map[string]any{"label": "No"}},
			}},
			Options:          []any{map[string]any{"optionId": "answer-0", "name": "Sí", "kind": "answer"}},
			FallbackOptionID: "",
		})
	}()
	events.waitFor(t, time.Second, func(event collectedEvent) bool {
		return event.channel == "chat:permission-request" &&
			asString(mapFromAny(event.payload)["sessionId"]) == "sess-cancelled"
	})
	manager.cancelPermissionsForSession("sess-cancelled")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a parked question outlived its cancelled session")
	}
}

// A spawned subagent's card attaches to the PARENT chat's turn. That is right
// for a permission the user must grant, and wrong for a question: the parent
// agent owns it. Answer the subagent immediately so no background lane parks on
// a human, and put no card on anyone's screen.
func TestSpawnedSubagentQuestionIsHandedBackWhileItsPermissionsStillAsk(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast, PermissionTimeout: 120 * time.Millisecond})
	t.Cleanup(func() { manager.Reset() })

	answer := manager.requestPermission(permissionRequest{
		JobID: "visible-parent-job", SessionID: "sess-subagent-question", Subagent: true,
		ToolCall: map[string]any{"title": "Push", "kind": "other", "rawInput": map[string]any{
			"question": "¿Pusheo los commits?",
			"options":  []any{map[string]any{"label": "Sí"}, map[string]any{"label": "No"}},
		}},
		Options: []any{map[string]any{"optionId": "answer-0", "name": "Sí", "kind": "answer"}},
	})
	if answer != subagentQuestionOptionID {
		t.Fatalf("subagent question answered %q, want the hand-back", answer)
	}
	for _, event := range events.snapshot() {
		if event.channel == "chat:permission-request" {
			t.Fatalf("a subagent's question reached the screen: %#v", event.payload)
		}
	}

	// Its permissions are untouched — those are the user's call, and the card in
	// the parent chat is how they make it.
	go manager.requestPermission(permissionRequest{
		JobID: "visible-parent-job", SessionID: "sess-subagent-permission", Subagent: true,
		ToolCall:         map[string]any{"title": "Run rm -rf", "kind": "execute"},
		Options:          []any{map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"}},
		FallbackOptionID: "deny",
	})
	events.waitFor(t, time.Second, func(event collectedEvent) bool {
		return event.channel == "chat:permission-request" &&
			asString(mapFromAny(event.payload)["sessionId"]) == "sess-subagent-permission"
	})
}

func TestPermissionQuestionIsBoundedAndRedacted(t *testing.T) {
	long := strings.Repeat("x", 900)
	question := permissionQuestion(map[string]any{
		"question": long,
		"header":   strings.Repeat("h", 120),
		"options": []any{
			map[string]any{"label": strings.Repeat("l", 400), "description": "token=ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
			map[string]any{"label": "b"}, map[string]any{"label": "c"},
			map[string]any{"label": "d"}, map[string]any{"label": "e"},
		},
	})
	if question == nil {
		t.Fatal("question dropped")
	}
	if runes := []rune(asString(question["question"])); len(runes) > 401 {
		t.Fatalf("question not clipped: %d runes", len(runes))
	}
	if runes := []rune(asString(question["header"])); len(runes) > 41 {
		t.Fatalf("header not clipped: %d runes", len(runes))
	}
	options := anySlice(question["options"])
	if len(options) != 4 {
		t.Fatalf("choices = %d, want the 4-option cap", len(options))
	}
	if description := asString(mapFromAny(options[0])["description"]); strings.Contains(description, "ghp_abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("secret survived into a rendered question: %q", description)
	}
}

// Workass arms no clock of its own: with nothing configured, an unattended tool
// permission carries no timer at all and waits for the person. Asserted on the
// resolver rather than by waiting, since the bug it guards against is a deadline
// measured in minutes.
func TestPermissionWithoutAConfiguredDeadlineArmsNoTimer(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast})
	t.Cleanup(func() { manager.Reset() })

	decided := make(chan string, 1)
	go func() {
		decided <- manager.requestPermission(permissionRequest{
			JobID: "job-unattended", SessionID: "sess-no-deadline",
			ToolCall: map[string]any{"title": "Run npm test", "kind": "execute"},
			Options: []any{
				map[string]any{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"},
				map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
			},
			FallbackOptionID: "deny",
		})
	}()
	request := events.waitFor(t, time.Second, func(event collectedEvent) bool {
		return event.channel == "chat:permission-request" &&
			asString(mapFromAny(event.payload)["sessionId"]) == "sess-no-deadline"
	}).payload.(map[string]any)

	id := asString(request["id"])
	manager.mu.Lock()
	rec := manager.permissions[id]
	manager.mu.Unlock()
	if rec == nil {
		t.Fatal("the permission never parked for the user")
	}
	rec.timerMu.Lock()
	armed := rec.timer != nil
	rec.timerMu.Unlock()
	if armed {
		t.Fatal("an unattended permission armed a Workass deadline; the origin harness owns that clock")
	}

	manager.PermissionDecide(id, "allow_once")
	select {
	case answer := <-decided:
		if answer != "allow_once" {
			t.Fatalf("answer = %q", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("the waiting permission never settled after the user answered")
	}
}

// A malformed or empty payload must not produce a question card with no answers.
func TestPermissionQuestionRejectsUnanswerableInput(t *testing.T) {
	for name, raw := range map[string]any{
		"nil":            nil,
		"not a map":      "question?",
		"no question":    map[string]any{"options": []any{map[string]any{"label": "a"}}},
		"no options":     map[string]any{"question": "¿y?"},
		"blank labels":   map[string]any{"question": "¿y?", "options": []any{map[string]any{"label": "   "}}},
		"blank question": map[string]any{"question": "  ", "options": []any{map[string]any{"label": "a"}}},
	} {
		if got := permissionQuestion(raw); got != nil {
			t.Fatalf("%s produced a question card: %+v", name, got)
		}
	}
}
