package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestLaneDiagnosticsRetainExactResumeFailureBeforePrompt(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "both")
	first, events := fixture.newManager()
	session := fixture.newSession(t, first)
	fixture.runTurn(t, first, events, session.SessionID, "commit the original thread")
	binding, ok := first.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok {
		t.Fatal("missing mock binding")
	}
	originalTurns := first.TurnDiagnostics("native-tab", "native-chat", 5)["turns"].([]any)
	if len(originalTurns) != 1 {
		t.Fatalf("expected the original turn observation: %#v", originalTurns)
	}
	originalJobID := mapFromAny(originalTurns[0])["jobId"]
	first.Reset()

	restarted, _ := fixture.newManagerTuned(func(opts *Options) {
		opts.Provider.Env["WORKASS_MOCK_ACP_FAIL_RESUME"] = "1"
	})
	t.Cleanup(func() { restarted.Reset() })
	factory := managerLaneFactory{manager: restarted, providerID: "mock"}
	_, err := factory.Resume(context.Background(), providercontract.ResumeLaneRequest{
		Identity: bindingLaneIdentity(binding), Thread: bindingThreadRef(binding),
		Owner: providercontract.AttachmentOwner{TabID: "native-tab"}, CWD: binding.CWD,
	})
	if !providercontract.ErrorIs(err, providercontract.ErrorTransientTransport) {
		t.Fatalf("resume failure = %v", err)
	}
	result := restarted.TurnDiagnostics("native-tab", "native-chat", 5)
	attempts := result["laneAttachments"].([]any)
	if result["available"] != true || len(attempts) != 1 {
		t.Fatalf("pre-prompt failure disappeared: %#v", result)
	}
	turns := result["turns"].([]any)
	if len(turns) != 1 {
		t.Fatalf("resume failure must preserve only the original turn: %#v", turns)
	}
	retained := mapFromAny(turns[0])
	if retained["jobId"] != originalJobID || retained["historical"] != true || retained["active"] != false || retained["outcome"] != "completed" {
		t.Fatalf("resume failure changed the original turn observation: %#v", retained)
	}
	attempt := mapFromAny(attempts[0])
	if attempt["operation"] != "resume" || attempt["rpcCode"] != -32098 || !strings.Contains(asString(attempt["error"]), "Mock exact resume failure") {
		t.Fatalf("underlying failure was discarded: %#v", attempt)
	}
	if persistentMockSessionCount(t, fixture.sessionFile) != 1 {
		t.Fatal("diagnostics created a replacement thread")
	}
	trace := readNativeMockTrace(t, fixture.traceFile)
	if traceContains(trace, "[mock:lifecycle] session/load") {
		t.Fatal("failed resume tried another attachment method")
	}
}

func TestLaneDiagnosticsBoundRedactAndIsolateExactPairs(t *testing.T) {
	t.Parallel()
	var logged []byte
	m := NewManager(Options{RSSSampleInterval: time.Hour, Logf: func(_ string, fields map[string]any) {
		logged, _ = json.Marshal(fields)
	}})
	t.Cleanup(func() { m.Reset() })
	for i := 0; i < maxLaneDiagnostics+5; i++ {
		m.recordLaneDiagnostic("tab", "chat", "mock", "resume", "", time.Now(), nil)
	}
	err := &providercontract.Error{Kind: providercontract.ErrorTransientTransport,
		Message: "resume original-native-id token=private-value",
		Cause:   fmt.Errorf("load failed: %w", context.DeadlineExceeded)}
	m.recordLaneDiagnostic("tab", "chat", "mock", "resume", "", time.Now(), err, "original-native-id")
	if len(m.laneDiagnostics) != maxLaneDiagnostics {
		t.Fatal("attachment history exceeded its retention bound")
	}
	result := m.TurnDiagnostics("tab", "chat", 1)
	attempts := result["laneAttachments"].([]any)
	if len(attempts) != 1 || mapFromAny(attempts[0])["transport"] != "deadline_exceeded" {
		t.Fatalf("lost transport cause or read bound: %#v", result)
	}
	encoded, _ := json.Marshal(result)
	for _, bytes := range [][]byte{logged, encoded} {
		if strings.Contains(string(bytes), "private-value") || strings.Contains(string(bytes), "original-native-id") {
			t.Fatal("diagnostics exposed sensitive data")
		}
	}
	for _, pair := range [][2]string{{"other-tab", "chat"}, {"tab", "other-chat"}} {
		if result := m.TurnDiagnostics(pair[0], pair[1], 5); result["available"] != false {
			t.Fatalf("diagnostics crossed ownership: %#v", result)
		}
	}
	m.recordLaneDiagnostic("tab", "chat", "mock", "create", "", time.Now(), fmt.Errorf("%s", strings.Repeat("界", 3000)))
	last := m.recentLaneDiagnostics("tab", "chat", 1)
	if len(asString(mapFromAny(last[0])["error"])) > 2048 {
		t.Fatal("unbounded provider failure text")
	}
}

func TestLaneDiagnosticsRetainFailureAfterSuccessfulResume(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "resume")
	manager, events := fixture.newManager()
	session := fixture.newSession(t, manager)
	fixture.runTurn(t, manager, events, session.SessionID, "establish")
	binding, ok := manager.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok {
		t.Fatal("missing native binding")
	}
	lane, err := (managerLaneFactory{manager: manager, providerID: "mock"}).Resume(context.Background(), providercontract.ResumeLaneRequest{
		Identity: bindingLaneIdentity(binding), Thread: bindingThreadRef(binding), Owner: providercontract.AttachmentOwner{TabID: "native-tab"}, CWD: binding.CWD,
	})
	if err != nil {
		manager.Reset()
		t.Fatal(err)
	}
	managed := lane.(*managerLane)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range lane.Events() {
			managed.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
		}
	}()
	t.Cleanup(func() { _ = lane.Detach(context.Background()); manager.Reset(); <-drained })
	_, err = lane.Delivery().StartTurn(context.Background(), providercontract.TurnInput{
		OperationID: "invalid-attachment", Text: "do not submit", Attachments: []providercontract.Attachment{{Name: "missing reference"}},
	})
	if err == nil {
		t.Fatal("invalid attachment admitted")
	}
	attempts := manager.recentLaneDiagnostics("native-tab", "native-chat", 1)
	if len(attempts) != 1 {
		t.Fatal("turn-preparation failure was lost")
	}
	attempt := mapFromAny(attempts[0])
	if attempt["operation"] != "start" || attempt["outcome"] != "failed" || !strings.Contains(asString(attempt["error"]), "immutable content reference") {
		t.Fatalf("lost post-resume error: %#v", attempt)
	}
	if persistentMockSessionCount(t, fixture.sessionFile) != 1 {
		t.Fatal("diagnostics replaced native thread")
	}
}

func TestLaneControlRPCRejectionIsNotTransportFailure(t *testing.T) {
	t.Parallel()
	for _, code := range []int{-32002, -32601, -32602} {
		err := fmt.Errorf("wrapped: %w", &acpError{Code: code, Msg: "Resource not found"})
		classified := classifyLaneControlError("apply lane model", err)
		if !providercontract.ErrorIs(classified, providercontract.ErrorAdmissionRejected) {
			t.Fatalf("RPC %d was not classified as rejected admission: %v", code, classified)
		}
		var rpcErr *acpError
		if !errors.As(classified, &rpcErr) || rpcErr.Code != code {
			t.Fatal("classification discarded the original RPC error")
		}
	}
	if got := classifyLaneControlError("apply lane model", io.EOF); !providercontract.ErrorIs(got, providercontract.ErrorTransientTransport) {
		t.Fatalf("closed transport must retain its classification: %v", got)
	}
	typed := &providercontract.Error{Kind: providercontract.ErrorAuthenticationRequired, Message: "login required"}
	if classifyLaneControlError("apply lane mode", typed) != typed {
		t.Fatal("classification replaced a typed provider error")
	}
	if classifyLaneControlError("apply lane mode", nil) != nil {
		t.Fatal("nil error became a failure")
	}
}

func TestLaneDiagnosticsModelRejectionDoesNotSubmitOrReplaceThread(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "resume")
	manager, events := fixture.newManagerTuned(func(opts *Options) {
		opts.Provider.Env["WORKASS_MOCK_ACP_REJECT_MODEL"] = "1"
	})
	session := fixture.newSession(t, manager)
	fixture.runTurn(t, manager, events, session.SessionID, "establish")
	binding, ok := manager.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok {
		t.Fatal("missing native binding")
	}
	lane, err := (managerLaneFactory{manager: manager, providerID: "mock"}).Resume(context.Background(), providercontract.ResumeLaneRequest{
		Identity: bindingLaneIdentity(binding), Thread: bindingThreadRef(binding), Owner: providercontract.AttachmentOwner{TabID: "native-tab"}, CWD: binding.CWD,
	})
	if err != nil {
		manager.Reset()
		t.Fatal(err)
	}
	managed := lane.(*managerLane)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range lane.Events() {
			managed.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
		}
	}()
	t.Cleanup(func() { _ = lane.Detach(context.Background()); manager.Reset(); <-drained })
	_, err = lane.Delivery().StartTurn(context.Background(), providercontract.TurnInput{
		OperationID: "rejected-model", Text: "MUST_NOT_REACH_PROVIDER", ModelID: "mock-other",
	})
	if !providercontract.ErrorIs(err, providercontract.ErrorAdmissionRejected) {
		t.Fatalf("model rejection = %v", err)
	}
	attempts := manager.recentLaneDiagnostics("native-tab", "native-chat", 1)
	if len(attempts) != 1 {
		t.Fatal("model rejection was not diagnosed")
	}
	attempt := mapFromAny(attempts[0])
	if attempt["errorKind"] != providercontract.ErrorAdmissionRejected || attempt["rpcCode"] != -32002 {
		t.Fatalf("lost explicit model RPC rejection: %#v", attempt)
	}
	if traceContains(readNativeMockTrace(t, fixture.traceFile), "MUST_NOT_REACH_PROVIDER") {
		t.Fatal("a rejected model must prevent prompt dispatch")
	}
	if persistentMockSessionCount(t, fixture.sessionFile) != 1 {
		t.Fatal("model rejection created a replacement native thread")
	}
}

func TestLaneDiagnosticsRetainModelFailureWithoutRawRPCData(t *testing.T) {
	for _, test := range []struct {
		name       string
		code       int
		message    string
		data       any
		wantReason string
	}{
		{"model resource", -32002, "Resource not found", map[string]any{"uri": "Model not found: do-not-log-model. Available models: do-not-log-list", "extra": "do-not-log-extra"}, "model_not_found"},
		{"real uri", -32002, "Resource not found", map[string]any{"uri": "https://do-not-log.example/private"}, ""},
		{"different code", -32016, "Resource not found", map[string]any{"uri": "Model not found: do-not-log-model"}, ""},
		{"different message", -32002, "Other rejection", map[string]any{"uri": "Model not found: do-not-log-model"}, ""},
		{"unstructured data", -32002, "Resource not found", "do-not-log-data", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := &Manager{opts: Options{Logf: func(string, map[string]any) {}}}
			err := classifyLaneControlError("apply lane model", &acpError{Code: test.code, Msg: test.message, Data: test.data})
			m.recordLaneDiagnostic("tab", "chat", "devin", "start", "unavailable-model", time.Now(), err)
			fields := mapFromAny(m.recentLaneDiagnostics("tab", "chat", 1)[0])
			if asString(fields["rpcReason"]) != test.wantReason || fields["requestedModelId"] != "unavailable-model" {
				t.Fatalf("missing bounded model failure metadata: %#v", fields)
			}
			if strings.Contains(fmt.Sprint(fields), "do-not-log") {
				t.Fatal("arbitrary provider error data escaped into diagnostics")
			}
		})
	}
	m := &Manager{opts: Options{Logf: func(string, map[string]any) {}}}
	for _, model := range []string{"secret-model-value", "original-native-id"} {
		m.recordLaneDiagnostic("tab", "chat", "devin", "start", model, time.Now(), errors.New("rejected"), "original-native-id")
		fields := mapFromAny(m.recentLaneDiagnostics("tab", "chat", 1)[0])
		if strings.Contains(fmt.Sprint(fields), model) {
			t.Fatal("sensitive model metadata escaped into diagnostics")
		}
	}
}
