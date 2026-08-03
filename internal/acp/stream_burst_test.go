package acp

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type timedBurstEvent struct {
	at      time.Time
	payload map[string]any
}

type timedBurstCollector struct {
	mu     sync.Mutex
	events []timedBurstEvent
}

func (c *timedBurstCollector) Broadcast(channel string, raw any) {
	if channel != "job:event" {
		return
	}
	payload, _ := raw.(map[string]any)
	c.mu.Lock()
	c.events = append(c.events, timedBurstEvent{at: time.Now(), payload: payload})
	c.mu.Unlock()
}

func (c *timedBurstCollector) snapshot() []timedBurstEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]timedBurstEvent, len(c.events))
	copy(out, c.events)
	return out
}

func TestMockBurstStreamsAtDisplayCadenceWithoutDroppingText(t *testing.T) {
	root := repoRoot(t)
	collector := &timedBurstCollector{}
	const (
		chunkCount = 2048
		chunkBytes = 128
	)
	manager := NewManager(Options{
		RootDir: root,
		Provider: ProviderConfig{
			ID:      "mock",
			Command: "node",
			Args:    []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD:     root,
			Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS":          "0",
				"WORKASS_MOCK_ACP_BURST_CHUNKS":      "2048",
				"WORKASS_MOCK_ACP_BURST_CHUNK_BYTES": "128",
			},
			Enabled: true,
			Label:   "Workass Mock ACP",
		},
		Broadcast:           collector.Broadcast,
		StdoutFlushInterval: 16 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "burst-tab", ChatID: "chat-burst-tab", ProviderID: "mock"})
	if err != nil {
		t.Fatalf("new mock session: %v", err)
	}
	started := time.Now()
	job := startAppChatJob(t, manager, session.SessionID, "burst-tab", "[mock:burst]")
	id := jobID(job)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, event := range collector.snapshot() {
			if event.payload["type"] != "end" {
				continue
			}
			endedJob, _ := event.payload["job"].(map[string]any)
			if endedJob["id"] == id {
				goto complete
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for burst job end")

complete:
	var data []timedBurstEvent
	var output strings.Builder
	for _, event := range collector.snapshot() {
		if event.payload["type"] == "data" && event.payload["id"] == id && event.payload["stream"] == "stdout" {
			data = append(data, event)
			output.WriteString(asString(event.payload["chunk"]))
		}
	}
	wantBytes := chunkCount * chunkBytes
	if output.Len() != wantBytes {
		t.Fatalf("stream bytes = %d, want %d", output.Len(), wantBytes)
	}
	if len(data) < 20 {
		t.Fatalf("coalesced updates = %d, want a sustained multi-frame stream", len(data))
	}
	if len(data) >= chunkCount/2 {
		t.Fatalf("coalesced updates = %d for %d ACP chunks; renderer would be updated close to once per token", len(data), chunkCount)
	}

	var intervals time.Duration
	var maxGap time.Duration
	for i := 1; i < len(data); i++ {
		gap := data[i].at.Sub(data[i-1].at)
		intervals += gap
		if gap > maxGap {
			maxGap = gap
		}
	}
	meanGap := intervals / time.Duration(len(data)-1)
	elapsed := time.Since(started)
	throughput := float64(output.Len()) / elapsed.Seconds() / 1024
	t.Logf("burst bytes=%d acpChunks=%d websocketUpdates=%d elapsed=%s throughput=%.1fKiB/s meanUpdateGap=%s maxUpdateGap=%s", output.Len(), chunkCount, len(data), elapsed.Round(time.Millisecond), throughput, meanGap.Round(time.Microsecond), maxGap.Round(time.Microsecond))
	if meanGap > 35*time.Millisecond {
		t.Fatalf("mean renderer update gap = %s, want <= 35ms", meanGap)
	}
	if maxGap > 150*time.Millisecond {
		t.Fatalf("max renderer update gap = %s, want <= 150ms", maxGap)
	}
}
