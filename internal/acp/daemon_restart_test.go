package acp

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type restartCollector struct {
	mu     sync.Mutex
	events []map[string]any
}

func (c *restartCollector) Broadcast(channel string, raw any) {
	if channel != "job:event" {
		return
	}
	payload, _ := raw.(map[string]any)
	c.mu.Lock()
	c.events = append(c.events, payload)
	c.mu.Unlock()
}

func (c *restartCollector) endedJob(id string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, event := range c.events {
		if event["type"] != "end" {
			continue
		}
		job, _ := event["job"].(map[string]any)
		if job != nil && job["id"] == id {
			return job
		}
	}
	return nil
}

func (c *restartCollector) sawData(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, event := range c.events {
		if event["type"] == "data" && event["id"] == id {
			return true
		}
	}
	return false
}

// A daemon restart runs Reset(), which closes every bridge and so fails the
// in-flight prompt. The job is finalized before the process exits, which means
// the next boot's orphan sweep finds nothing running to stamp: the interruption
// has to be recorded here or the turn is indistinguishable from an agent error
// and the sidebar repaints the chat as "Falló" on every restart.
func TestResetMarksInFlightTurnInterrupted(t *testing.T) {
	root := repoRoot(t)
	collector := &restartCollector{}
	manager := NewManager(Options{
		RootDir: root,
		Provider: ProviderConfig{
			ID:      "mock",
			Command: "node",
			Args:    []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD:     root,
			Env:     map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "4000"},
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
	id := jobID(startAppChatJob(t, manager, session.SessionID, "restart-tab", "[mock:slow] hold the line"))

	deadline := time.Now().Add(20 * time.Second)
	for !collector.sawData(id) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the turn to start streaming")
		}
		time.Sleep(5 * time.Millisecond)
	}

	manager.Reset()

	ended := collector.endedJob(id)
	if ended == nil {
		t.Fatal("reset did not finalize the in-flight turn")
	}
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
	collector := &restartCollector{}
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

	deadline := time.Now().Add(20 * time.Second)
	var ended map[string]any
	for {
		if ended = collector.endedJob(id); ended != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the turn to finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ended["interrupted"] != false {
		t.Fatalf("completed turn interrupted = %v, want false", ended["interrupted"])
	}
}
