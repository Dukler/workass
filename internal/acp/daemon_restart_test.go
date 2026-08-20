package acp

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type restartCollector struct {
	data  chan string
	ended chan map[string]any
}

func (c *restartCollector) Broadcast(channel string, raw any) {
	if channel != "job:event" {
		return
	}
	payload, _ := raw.(map[string]any)
	switch payload["type"] {
	case "data":
		if id, _ := payload["id"].(string); id != "" {
			select {
			case c.data <- id:
			default:
			}
		}
	case "end":
		if job, _ := payload["job"].(map[string]any); job != nil {
			c.ended <- job
		}
	}
}

func newRestartCollector() *restartCollector {
	return &restartCollector{data: make(chan string, 1), ended: make(chan map[string]any, 1)}
}

func waitForRestartData(t *testing.T, collector *restartCollector, id string) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case got := <-collector.data:
			if got == id {
				return
			}
		case <-timer.C:
			t.Fatal("timed out waiting for the turn to start streaming")
		}
	}
}

func waitForRestartEnd(t *testing.T, collector *restartCollector, id string) map[string]any {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case job := <-collector.ended:
			if job["id"] == id {
				return job
			}
		case <-timer.C:
			t.Fatal("timed out waiting for the turn to finish")
		}
	}
}

// A daemon restart runs Reset(), which closes every bridge and so fails the
// in-flight prompt. The job is finalized before the process exits, which means
// the next boot's orphan sweep finds nothing running to stamp: the interruption
// has to be recorded here or the turn is indistinguishable from an agent error
// and the sidebar repaints the chat as "Falló" on every restart.
func TestResetMarksInFlightTurnInterrupted(t *testing.T) {
	root := repoRoot(t)
	collector := newRestartCollector()
	manager := NewManager(Options{
		RootDir: root,
		Provider: ProviderConfig{
			ID:      "mock",
			Command: "node",
			Args:    []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD:     root,
			Env:     map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "0"},
			Enabled: true,
			Label:   "Workass Mock ACP",
		},
		Broadcast:         collector.Broadcast,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "restart-tab", ChatID: "chat-restart-tab", ProviderID: "mock"})
	if err != nil {
		t.Fatalf("new mock session: %v", err)
	}
	id := jobID(startAppChatJob(t, manager, session.SessionID, "restart-tab", "[mock:active-without-terminal] hold the line"))
	waitForRestartData(t, collector, id)

	manager.Reset()

	ended := waitForRestartEnd(t, collector, id)
	if ended["status"] != "failed" {
		t.Fatalf("reset turn status = %v, want failed", ended["status"])
	}
	if ended["interrupted"] != true {
		t.Fatalf("reset turn interrupted = %v, want true", ended["interrupted"])
	}
	if ended["stopReason"] != "daemon-restart" {
		t.Fatalf("reset turn stopReason = %v, want daemon-restart", ended["stopReason"])
	}
}

// A turn that ends on its own must stay clean: the flag means "we stopped it",
// and an agent error still has to reach the sidebar as a failure.
func TestCompletedTurnIsNotMarkedInterrupted(t *testing.T) {
	root := repoRoot(t)
	collector := newRestartCollector()
	manager := NewManager(Options{
		RootDir: root,
		Provider: ProviderConfig{
			ID:      "mock",
			Command: "node",
			Args:    []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD:     root,
			Env:     map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "0"},
			Enabled: true,
			Label:   "Workass Mock ACP",
		},
		Broadcast:         collector.Broadcast,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "clean-tab", ChatID: "chat-clean-tab", ProviderID: "mock"})
	if err != nil {
		t.Fatalf("new mock session: %v", err)
	}
	id := jobID(startAppChatJob(t, manager, session.SessionID, "clean-tab", "finish normally"))

	ended := waitForRestartEnd(t, collector, id)
	if ended["interrupted"] != false {
		t.Fatalf("completed turn interrupted = %v, want false", ended["interrupted"])
	}
}
