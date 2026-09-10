package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestTurnDiagnosticsLiveStopAndCompletedRetention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &Manager{}
		job := &Job{ID: "exact-job", TabID: "exact-tab", ChatID: "exact-chat", ProviderID: "mock", startupTiming: &turnStartupTiming{started: time.Now()}}
		m.retainTurnDiagnostic(job)
		read := func() map[string]any {
			return m.TurnDiagnostics("exact-tab", "exact-chat", 1)["turns"].([]any)[0].(map[string]any)
		}
		if read()["phase"] != "host_preparation" {
			t.Fatal(read())
		}
		time.Sleep(2 * time.Second)
		job.startupTiming.mark(startupWritten)
		time.Sleep(3 * time.Second)
		if read()["phase"] != "waiting_for_provider_activity" || read()["managerElapsedMs"] != int64(5000) {
			t.Fatal(read())
		}
		job.startupTiming.mark(startupThought)
		if read()["phase"] != "provider_thinking_observed" {
			t.Fatal(read())
		}
		job.startupTiming.mark(startupCancelRequested)
		if read()["phase"] != "stopping_before_cancel_delivery" {
			t.Fatal(read())
		}
		time.Sleep(time.Second)
		job.startupTiming.mark(startupCancelWritten)
		if read()["phase"] != "waiting_for_provider_stop" {
			t.Fatal(read())
		}
		time.Sleep(2 * time.Second)
		job.startupTiming.mark(startupTerminalReply)
		job.startupTiming.mark(startupFinished)
		time.Sleep(10 * time.Second)
		if read()["managerElapsedMs"] != int64(8000) || read()["active"] != false || read()["terminalReplyMs"] != int64(8000) {
			t.Fatal(read())
		}
		if m.TurnDiagnostics("other-tab", "exact-chat", 1)["available"] != false {
			t.Fatal("cross-tab diagnostic leaked")
		}
		for i := 0; i < maxTurnDiagnostics+10; i++ {
			job := &Job{ID: fmt.Sprint(i), TabID: "other", ChatID: "other", startupTiming: &turnStartupTiming{started: time.Now()}}
			m.retainTurnDiagnostic(job)
		}
		if len(m.turnDiagnostics) != maxTurnDiagnostics || m.TurnDiagnostics("exact-tab", "exact-chat", 1)["available"] != false {
			t.Fatal("diagnostics retention was not bounded")
		}
		if len(m.TurnDiagnostics("other", "other", 20)["turns"].([]any)) != 20 {
			t.Fatal("limit not honored")
		}
	})
}

func TestTurnDiagnosticsConcurrentReadsDoNotRaceStreaming(t *testing.T) {
	m := &Manager{}
	job := &Job{ID: "race", TabID: "tab", ChatID: "chat", startupTiming: &turnStartupTiming{started: time.Now()}}
	m.retainTurnDiagnostic(job)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				job.startupTiming.mark(startupContent)
				job.startupTiming.lastUpdate.Store(int64(time.Since(job.startupTiming.started)) + 1)
				m.TurnDiagnostics("tab", "chat", 5)
			}
		}()
	}
	wg.Wait()
}

func TestTurnDiagnosticsMockCancellationRecordsWireBoundaries(t *testing.T) {
	root := repoRoot(t)
	done := make(chan struct{}, 1)
	m := NewManager(Options{RootDir: root, Provider: ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root}, Logf: func(message string, _ map[string]any) {
		if message == "acp turn startup timing" {
			done <- struct{}{}
		}
	}})
	t.Cleanup(func() { m.Reset() })
	session := newMockSession(t, m, "stop-timing")
	job := startAppChatJob(t, m, session.SessionID, "stop-timing", "[mock:slow] bounded cancellation measurement")
	deadline := time.Now().Add(3 * time.Second)
	for {
		turns := m.TurnDiagnostics("stop-timing", "chat-stop-timing", 1)["turns"].([]any)
		if len(turns) > 0 && turns[0].(map[string]any)["firstThinkingMs"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mock thinking was not observed live")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !m.CancelJobResult(jobID(job)).Cancelled {
		t.Fatal("cancel rejected")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not finish")
	}
	diagnostic := m.TurnDiagnostics("stop-timing", "chat-stop-timing", 1)["turns"].([]any)[0].(map[string]any)
	if diagnostic["active"] != false || diagnostic["outcome"] != "cancelled" {
		t.Fatal(diagnostic)
	}
	for _, key := range []string{"promptWrittenMs", "firstThinkingMs", "firstContentPublishedMs", "stopRequestedMs", "cancelWrittenMs", "terminalReplyMs", "finishedMs"} {
		if diagnostic[key] == nil {
			t.Fatalf("missing %s: %v", key, diagnostic)
		}
	}
	if diagnostic["intervalsMs"].(map[string]any)["providerStopReply"] == nil {
		t.Fatal("missing stop interval")
	}
}

func TestStartupTimingRetainsFirstObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timing := &turnStartupTiming{started: time.Now()}
		timing.mark(startupWorker)
		time.Sleep(2 * time.Second)
		timing.mark(startupWritten)
		time.Sleep(3 * time.Second)
		timing.mark(startupUpdate)
		time.Sleep(time.Second)
		timing.mark(startupContent)
		time.Sleep(time.Second)
		timing.mark(startupContent)
		fields := timing.fields()
		for key, want := range map[string]int64{"workerStartedMs": 0, "promptWrittenMs": 2000, "firstUpdateMs": 5000, "firstContentMs": 6000, "managerElapsedMs": 7000} {
			if fields[key] != want {
				t.Fatalf("%s = %v, want %d", key, fields[key], want)
			}
		}
		if _, invented := fields["firstToolMs"]; invented {
			t.Fatal("a turn without tools must not report a tool timestamp")
		}
	})
}

func TestStartupTimingMeasuresMockWireAndExcludesContent(t *testing.T) {
	root := repoRoot(t)
	receipts := make(chan map[string]any, 2)
	manager := NewManager(Options{
		RootDir:  root,
		Provider: ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root},
		Logf: func(message string, fields map[string]any) {
			if message == "acp turn startup timing" {
				receipts <- fields
			}
		},
	})
	t.Cleanup(func() { manager.Reset() })
	session := newMockSession(t, manager, "startup-timing-tab")
	_, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID,
		TabID: "startup-timing-tab", ChatID: "chat-startup-timing-tab",
		Prompt: "PRIVATE_PROMPT_MARKER password=DO_NOT_LOG_THIS",
		BeforeStart: func(*JobStartOptions) error {
			// A known host-side delay must precede the physical prompt write,
			// and must not be attributed to provider inference.
			time.Sleep(80 * time.Millisecond)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case fields := <-receipts:
		previous := int64(75)
		for _, key := range []string{"workerStartedMs", "controlsFinishedMs", "promptPreparedMs", "promptWrittenMs", "firstUpdateMs", "firstContentMs", "firstToolMs", "managerElapsedMs"} {
			value, ok := fields[key].(int64)
			if !ok || value < previous {
				t.Fatalf("%s = %v, expected >= %d; receipt=%v", key, fields[key], previous, fields)
			}
			previous = value
		}
		encoded, _ := json.Marshal(fields)
		for _, forbidden := range []string{"PRIVATE_PROMPT_MARKER", "DO_NOT_LOG_THIS", "password", session.SessionID} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatal("startup receipt exposed content or native session identity")
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing startup timing receipt from completed mock turn")
	}
}
