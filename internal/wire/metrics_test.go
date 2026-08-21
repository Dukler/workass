package wire

import (
	"testing"
	"time"
)

func TestStatsTimeInvokesPerChannel(t *testing.T) {
	hub := NewHub(Options{TrustLocalhost: true})
	hub.Register("session:save", func(args []any) (any, error) {
		// The real handler merges the whole snapshot under a lock; a slow one is
		// exactly what queues streamed text behind it.
		time.Sleep(60 * time.Millisecond)
		return true, nil
	})
	hub.Register("job:cancel", func(args []any) (any, error) { return nil, nil })

	if _, err := hub.Invoke("session:save", nil); err != nil {
		t.Fatalf("invoke session:save: %v", err)
	}
	if _, err := hub.Invoke("job:cancel", nil); err != nil {
		t.Fatalf("invoke job:cancel: %v", err)
	}

	stats := hub.Stats()
	invokes, ok := stats["slowestInvokes"].([]map[string]any)
	if !ok || len(invokes) == 0 {
		t.Fatalf("slowestInvokes missing: %v", stats)
	}
	// Ranked by total time, so the expensive handler is first.
	if invokes[0]["channel"] != "session:save" {
		t.Fatalf("slowest invoke = %v, want session:save", invokes[0]["channel"])
	}
	if avg, _ := invokes[0]["avgMs"].(float64); avg < 50 {
		t.Fatalf("session:save avgMs = %v, want >= 50", invokes[0]["avgMs"])
	}
	if over, _ := invokes[0]["over50ms"].(uint64); over != 1 {
		t.Fatalf("session:save over50ms = %v, want 1", invokes[0]["over50ms"])
	}
}

func TestStatsCountEventBytesPerChannel(t *testing.T) {
	hub := NewHub(Options{TrustLocalhost: true})
	hub.Broadcast("job:event", map[string]any{"type": "data", "chunk": "hola"})

	stats := hub.Stats()
	if broadcasts, _ := stats["broadcasts"].(uint64); broadcasts != 1 {
		t.Fatalf("broadcasts = %v, want 1", stats["broadcasts"])
	}
	if _, ok := stats["enqueueAvgMs"].(float64); !ok {
		t.Fatalf("canonical enqueue average timing metric missing: %v", stats)
	}
	if _, ok := stats["enqueueMaxMs"].(float64); !ok {
		t.Fatalf("canonical enqueue maximum timing metric missing: %v", stats)
	}
	if _, ok := stats["enqueuesOver50ms"].(uint64); !ok {
		t.Fatalf("canonical enqueue timing metrics missing: %v", stats)
	}
	if scope := stats["eventTimingScope"]; scope != "enqueue" {
		t.Fatalf("event timing scope = %v, want enqueue", scope)
	}
	channels, ok := stats["eventChannels"].([]map[string]any)
	if !ok || len(channels) == 0 {
		t.Fatalf("eventChannels missing: %v", stats)
	}
	if channels[0]["channel"] != "job:event" {
		t.Fatalf("event channel = %v, want job:event", channels[0]["channel"])
	}
	if bytes, _ := channels[0]["bytes"].(uint64); bytes == 0 {
		t.Fatalf("event bytes not counted: %v", channels[0])
	}
}
