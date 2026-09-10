package acp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

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
