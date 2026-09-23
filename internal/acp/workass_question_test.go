package acp

import (
	"context"
	"strings"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestWorkassQuestionAnswerReachesTheOwningRequester(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast})
	t.Cleanup(func() { manager.Reset() })
	bindWorkassQuestionOwner(manager, "owner-a", "chat-a", "tab-a", "session-a", "job-a")
	question := testWorkassQuestion()

	type result struct {
		answer *providercontract.QuestionAnswer
		err    error
	}
	done := make(chan result, 1)
	go func() {
		answer, err := manager.AskAgentQuestion(context.Background(), "owner-a", "chat-a", "tab-a", question, 0)
		done <- result{answer: answer, err: err}
	}()
	request := events.waitFor(t, time.Second, func(event collectedEvent) bool {
		payload := mapFromAny(event.payload)
		return event.channel == "chat:permission-request" && asString(payload["chatId"]) == "chat-a"
	}).payload.(map[string]any)
	uiQuestion := mapFromAny(request["question"])
	if uiQuestion["workassTool"] != true || asString(uiQuestion["questionId"]) != question.ID || asString(uiQuestion["operationId"]) != question.OperationID {
		t.Fatalf("question card metadata = %#v", uiQuestion)
	}
	choices := anySlice(uiQuestion["options"])
	if len(choices) != 2 || asString(mapFromAny(choices[0])["id"]) != "build" || asString(mapFromAny(choices[1])["label"]) != "Production" {
		t.Fatalf("structured question choices = %#v", choices)
	}
	// A controller reconnect receives the same pending card snapshot; this is a
	// read of the existing resolver, not a second question request.
	pending := manager.PendingPermissions()
	if len(pending) != 1 {
		t.Fatalf("pending question snapshot count = %d, want 1", len(pending))
	}
	reconnected := mapFromAny(pending[0])
	reconnectedQuestion := mapFromAny(reconnected["question"])
	if asString(reconnected["id"]) != asString(request["id"]) ||
		asString(reconnectedQuestion["operationId"]) != question.OperationID ||
		asString(reconnectedQuestion["questionId"]) != question.ID {
		t.Fatalf("reconnect did not receive the same pending question: %#v", reconnected)
	}
	answer := providercontract.QuestionAnswer{Status: "answered", SelectedOptionIDs: []string{"build", "production"}, FreeText: "Keep this exact text. "}
	token, err := providercontract.EncodeWorkassQuestionAnswer(answer)
	if err != nil {
		t.Fatal(err)
	}
	staleToken, err := providercontract.EncodeWorkassQuestionAnswer(providercontract.QuestionAnswer{
		Status: "answered", SelectedOptionIDs: []string{"stale-option"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if manager.PermissionDecide(asString(request["id"]), staleToken) {
		t.Fatal("stale option reply resolved the pending question")
	}
	if manager.PermissionDecide("stale-permission-id", token) {
		t.Fatal("reply for another controller's stale question id was accepted")
	}
	if !manager.PermissionDecide(asString(request["id"]), token) {
		t.Fatal("owning Workass question rejected its structured answer")
	}
	got := <-done
	if got.err != nil || got.answer == nil {
		t.Fatalf("question result = %#v err=%v", got.answer, got.err)
	}
	if got.answer.Status != "answered" || strings.Join(got.answer.SelectedOptionIDs, ",") != "build,production" || got.answer.FreeText != answer.FreeText {
		t.Fatalf("question answer was not preserved: %#v", got.answer)
	}
	resolved := events.waitFor(t, time.Second, func(event collectedEvent) bool {
		return event.channel == "chat:permission-resolved" && asString(mapFromAny(event.payload)["id"]) == asString(request["id"])
	})
	resolvedPayload := mapFromAny(resolved.payload)
	if asString(resolvedPayload["optionId"]) != providercontract.WorkassQuestionResolvedOptionID ||
		strings.Contains(asString(resolvedPayload["optionId"]), answer.FreeText) {
		t.Fatalf("resolved wire event exposed answer content: %#v", resolvedPayload)
	}
	resolvedQuestion := mapFromAny(resolvedPayload["question"])
	answerSummary := mapFromAny(resolvedPayload["questionAnswer"])
	if resolvedQuestion["workassTool"] != true || asString(answerSummary["status"]) != "answered" {
		t.Fatalf("terminal question status is not recoverable from its permission event: %#v", resolvedPayload)
	}
	if err := manager.ValidateWorkassQuestionCaller("other-owner", "chat-a", "tab-a"); err == nil {
		t.Fatal("another agent owner passed the question caller fence")
	}
}

func TestWorkassQuestionTimeoutAndTurnEndAreExplicit(t *testing.T) {
	t.Run("caller cancellation", func(t *testing.T) {
		events := newEventCollector()
		manager := NewManager(Options{Broadcast: events.Broadcast})
		t.Cleanup(func() { manager.Reset() })
		bindWorkassQuestionOwner(manager, "owner-caller", "chat-caller", "tab-caller", "session-caller", "job-caller")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan *providercontract.QuestionAnswer, 1)
		go func() {
			answer, _ := manager.AskAgentQuestion(ctx, "owner-caller", "chat-caller", "tab-caller", testWorkassQuestion(), 0)
			done <- answer
		}()
		request := events.waitFor(t, time.Second, func(event collectedEvent) bool { return event.channel == "chat:permission-request" })
		cancel()
		select {
		case answer := <-done:
			if answer == nil || answer.Status != "cancelled" || answer.Reason != "caller_cancelled" {
				t.Fatalf("caller cancellation result = %#v", answer)
			}
		case <-time.After(time.Second):
			t.Fatal("caller cancellation left a question waiter behind")
		}
		resolved := events.waitFor(t, time.Second, func(event collectedEvent) bool {
			return event.channel == "chat:permission-resolved" && asString(mapFromAny(event.payload)["id"]) == asString(mapFromAny(request.payload)["id"])
		})
		status := mapFromAny(mapFromAny(resolved.payload)["questionAnswer"])
		if asString(status["status"]) != "cancelled" || asString(status["reason"]) != "caller_cancelled" || len(manager.PendingPermissions()) != 0 {
			t.Fatalf("caller cancellation left an unresolved card: resolved=%#v pending=%#v", resolved.payload, manager.PendingPermissions())
		}
	})

	t.Run("explicit timeout", func(t *testing.T) {
		events := newEventCollector()
		manager := NewManager(Options{Broadcast: events.Broadcast})
		t.Cleanup(func() { manager.Reset() })
		bindWorkassQuestionOwner(manager, "owner-timeout", "chat-timeout", "tab-timeout", "session-timeout", "job-timeout")
		answer, err := manager.AskAgentQuestion(context.Background(), "owner-timeout", "chat-timeout", "tab-timeout", testWorkassQuestion(), 35*time.Millisecond)
		if err != nil || answer == nil || answer.Status != "timed_out" || answer.Reason != "timeout" {
			t.Fatalf("explicit timeout result = %#v err=%v", answer, err)
		}
	})

	t.Run("provider turn end", func(t *testing.T) {
		events := newEventCollector()
		manager := NewManager(Options{Broadcast: events.Broadcast})
		t.Cleanup(func() { manager.Reset() })
		bindWorkassQuestionOwner(manager, "owner-end", "chat-end", "tab-end", "session-end", "job-end")
		done := make(chan *providercontract.QuestionAnswer, 1)
		go func() {
			answer, _ := manager.AskAgentQuestion(context.Background(), "owner-end", "chat-end", "tab-end", testWorkassQuestion(), 0)
			done <- answer
		}()
		events.waitFor(t, time.Second, func(event collectedEvent) bool { return event.channel == "chat:permission-request" })
		manager.cancelWorkassQuestionsForJob("job-end")
		select {
		case answer := <-done:
			if answer == nil || answer.Status != "cancelled" || answer.Reason != "provider_turn_ended" {
				t.Fatalf("turn end result = %#v", answer)
			}
		case <-time.After(time.Second):
			t.Fatal("question waiter survived its provider turn")
		}
	})

	t.Run("daemon reset", func(t *testing.T) {
		events := newEventCollector()
		manager := NewManager(Options{Broadcast: events.Broadcast})
		bindWorkassQuestionOwner(manager, "owner-reset", "chat-reset", "tab-reset", "session-reset", "job-reset")
		done := make(chan *providercontract.QuestionAnswer, 1)
		go func() {
			answer, _ := manager.AskAgentQuestion(context.Background(), "owner-reset", "chat-reset", "tab-reset", testWorkassQuestion(), 0)
			done <- answer
		}()
		events.waitFor(t, time.Second, func(event collectedEvent) bool { return event.channel == "chat:permission-request" })
		manager.Reset()
		select {
		case answer := <-done:
			if answer == nil || answer.Status != "cancelled" || answer.Reason != "daemon_reset" {
				t.Fatalf("daemon reset result = %#v", answer)
			}
		case <-time.After(time.Second):
			t.Fatal("daemon reset left the Workass question waiter orphaned")
		}
	})
}

func TestWorkassQuestionCallerAndAnswerValidationAreIsolated(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	bindWorkassQuestionOwner(manager, "owner-isolated", "chat-isolated", "tab-isolated", "session-isolated", "job-isolated")
	for _, test := range []struct{ owner, chat, tab string }{
		{"owner-isolated", "other-chat", "tab-isolated"},
		{"owner-isolated", "chat-isolated", "other-tab"},
		{"other-owner", "chat-isolated", "tab-isolated"},
	} {
		if err := manager.ValidateWorkassQuestionCaller(test.owner, test.chat, test.tab); err == nil {
			t.Fatalf("cross-owner question caller passed for %#v", test)
		}
	}
	manager.mu.Lock()
	manager.jobs["job-isolated"].SubagentID = "child-isolated"
	manager.mu.Unlock()
	if err := manager.ValidateWorkassQuestionCaller("owner-isolated", "chat-isolated", "tab-isolated"); err == nil {
		t.Fatal("child agent was allowed to park its parent chat on a human question")
	}

	question := testWorkassQuestion()
	uiQuestion := question
	uiQuestion.Options = []providercontract.PermissionQuestionOption{{ID: "production", Label: "Production"}, {ID: "canary", Label: "Canary"}}
	uiToken := "workass-question-v1:eyJzdGF0dXMiOiJhbnN3ZXJlZCIsInNlbGVjdGVkT3B0aW9uSWRzIjpbInByb2R1Y3Rpb24iLCJjYW5hcnkiXSwiZnJlZVRleHQiOiJDb25zZXJ2YXIgZXN0ZSB0ZXh0byB0YWwgY3VhbC4gICJ9"
	if answer, err := providercontract.DecodeWorkassQuestionAnswer(uiToken, uiQuestion); err != nil || answer.FreeText != "Conservar este texto tal cual.  " {
		t.Fatalf("renderer token is incompatible with the daemon decoder: answer=%#v err=%v", answer, err)
	}
	valid, err := providercontract.EncodeWorkassQuestionAnswer(providercontract.QuestionAnswer{
		Status: "answered", SelectedOptionIDs: []string{"build"}, FreeText: "additional detail",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providercontract.DecodeWorkassQuestionAnswer(valid, question); err != nil {
		t.Fatalf("valid answer rejected: %v", err)
	}
	unicodeText := strings.Repeat("界🙂", 500)
	unicodeToken, err := providercontract.EncodeWorkassQuestionAnswer(providercontract.QuestionAnswer{
		Status: "answered", SelectedOptionIDs: []string{"build"}, FreeText: unicodeText,
	})
	if err != nil {
		t.Fatalf("1000-codepoint Unicode answer encoding failed: %v", err)
	}
	unicodeAnswer, err := providercontract.DecodeWorkassQuestionAnswer(unicodeToken, question)
	if err != nil || unicodeAnswer.FreeText != unicodeText {
		t.Fatalf("valid Unicode answer did not round trip: answer=%#v err=%v", unicodeAnswer, err)
	}
	for _, malformed := range []string{
		"workass-question-v1:%%%",
		"workass-question-v1:eyJzdGF0dXMiOiJhbnN3ZXJlZCIsInNlbGVjdGVkT3B0aW9uSWRzIjpbImJvZ3VzIl19",
	} {
		if _, err := providercontract.DecodeWorkassQuestionAnswer(malformed, question); err == nil {
			t.Fatalf("malformed or stale answer accepted: %q", malformed)
		}
	}
	tooMany := providercontract.QuestionAnswer{Status: "answered", SelectedOptionIDs: []string{"build", "production"}}
	question.MultiSelect = false
	token, _ := providercontract.EncodeWorkassQuestionAnswer(tooMany)
	if _, err := providercontract.DecodeWorkassQuestionAnswer(token, question); err == nil {
		t.Fatal("single-select question accepted multiple choices")
	}
	for name, invalidAnswer := range map[string]providercontract.QuestionAnswer{
		"empty answered status": {Status: "answered"},
		"disabled free text": {
			Status: "answered", FreeText: "answer", SelectedOptionIDs: []string{},
		},
		"non-answer with content": {Status: "cancelled", SelectedOptionIDs: []string{"build"}},
	} {
		t.Run(name, func(t *testing.T) {
			ownerQuestion := question
			ownerQuestion.MultiSelect = true
			ownerQuestion.AllowFreeText = name != "disabled free text"
			token, err := providercontract.EncodeWorkassQuestionAnswer(invalidAnswer)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := providercontract.DecodeWorkassQuestionAnswer(token, ownerQuestion); err == nil {
				t.Fatalf("malformed partial answer %q was accepted", name)
			}
		})
	}
}

func TestWorkassQuestionAdmissionRacesJobEndWithoutOrphaning(t *testing.T) {
	for iteration := 0; iteration < 12; iteration++ {
		events := newEventCollector()
		manager := NewManager(Options{Broadcast: events.Broadcast})
		bindWorkassQuestionOwner(manager, "owner-race", "chat-race", "tab-race", "session-race", "job-race")
		start := make(chan struct{})
		askReady := make(chan struct{})
		endReady := make(chan struct{})
		endDone := make(chan struct{})
		type askResult struct {
			answer *providercontract.QuestionAnswer
			err    error
		}
		asked := make(chan askResult, 1)
		go func() {
			close(askReady)
			<-start
			answer, err := manager.AskAgentQuestion(context.Background(), "owner-race", "chat-race", "tab-race", testWorkassQuestion(), 0)
			asked <- askResult{answer: answer, err: err}
		}()
		go func() {
			defer close(endDone)
			close(endReady)
			<-start
			manager.mu.Lock()
			job := manager.jobs["job-race"]
			if job != nil {
				job.Status = "done"
				delete(manager.jobs, job.ID)
			}
			manager.mu.Unlock()
			manager.cancelWorkassQuestionsForJob("job-race")
		}()
		<-askReady
		<-endReady
		close(start)
		select {
		case result := <-asked:
			if result.err == nil && (result.answer == nil || result.answer.Status != "cancelled" || result.answer.Reason != "provider_turn_ended") {
				t.Fatalf("iteration %d: admitted question escaped the ending turn: %#v", iteration, result)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: question waiter survived its owner turn", iteration)
		}
		<-endDone
		if pending := manager.PendingPermissions(); len(pending) != 0 {
			t.Fatalf("iteration %d: dead turn retained an unowned question: %#v", iteration, pending)
		}
		requestCount, resolvedCount := 0, 0
		for _, event := range events.snapshot() {
			switch event.channel {
			case "chat:permission-request":
				requestCount++
			case "chat:permission-resolved":
				resolvedCount++
			}
		}
		if requestCount != resolvedCount {
			t.Fatalf("iteration %d: published question has no terminal resolution: request=%d resolved=%d", iteration, requestCount, resolvedCount)
		}
		manager.Reset()
	}
}

func bindWorkassQuestionOwner(manager *Manager, ownerKey, chatID, tabID, sessionID, jobID string) {
	manager.mu.Lock()
	manager.agentOwners[ownerKey] = agentOwnerBinding{ChatID: chatID, TabID: tabID}
	manager.agentOwnerBySession[sessionID] = ownerKey
	manager.jobs[jobID] = &Job{ID: jobID, Status: "running", ChatID: chatID, TabID: tabID, SessionID: sessionID}
	manager.mu.Unlock()
}

func testWorkassQuestion() providercontract.PermissionQuestion {
	return providercontract.PermissionQuestion{
		WorkassTool: true, ID: "deploy-target", OperationID: "ask-deploy-target",
		Question: "Which target?", Header: "Deploy", MultiSelect: true, AllowFreeText: true,
		Options: []providercontract.PermissionQuestionOption{
			{ID: "build", Label: "Build", Description: "Canary"},
			{ID: "production", Label: "Production"},
		},
	}
}
