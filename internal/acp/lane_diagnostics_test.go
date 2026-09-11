package acp

import (
	"context"
	"encoding/json"
	"fmt"
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
	if result["available"] != true || len(attempts) != 1 || len(result["turns"].([]any)) != 0 {
		t.Fatalf("pre-prompt failure disappeared: %#v", result)
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
		m.recordLaneDiagnostic("tab", "chat", "mock", "resume", time.Now(), nil)
	}
	err := &providercontract.Error{Kind: providercontract.ErrorTransientTransport,
		Message: "resume original-native-id token=private-value",
		Cause:   fmt.Errorf("load failed: %w", context.DeadlineExceeded)}
	m.recordLaneDiagnostic("tab", "chat", "mock", "resume", time.Now(), err, "original-native-id")
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
	m.recordLaneDiagnostic("tab", "chat", "mock", "create", time.Now(), fmt.Errorf("%s", strings.Repeat("界", 3000)))
	last := m.recentLaneDiagnostics("tab", "chat", 1)
	if len(asString(mapFromAny(last[0])["error"])) > 2048 {
		t.Fatal("unbounded provider failure text")
	}
}
