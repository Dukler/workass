package wire

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A lean client (the phone, ?lean=1) is not sent the catalog, plan usage,
// provider lists or a turn's tool and plan events, in the connect replay or
// in a broadcast; it still gets a turn's start, streamed text, thoughts,
// heartbeat and end. A client that does not ask gets everything, byte for byte as before.
func TestLeanClientIsNotSentEventsItNeverReads(t *testing.T) {
	toolSeen := map[string]map[string]any{}
	received := func(query string) []string {
		t.Helper()
		hub := NewHub()
		hub.SetOnClientReady(func(send func(channel string, payload any) error) {
			_ = send("providers:list", []any{})
			_ = send("chat:catalog", map[string]any{"groups": []any{}})
			_ = send("agent:apply", map[string]any{"action": "session-refresh"})
			_ = send("test:replayed", true)
		})
		server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
		defer server.Close()
		peer := dialWSPath(t, server.URL, "/"+query, "bGVhbi1ldmVudHMtMQ==")
		defer peer.conn.Close()
		broadcast := func() {
			hub.Broadcast("job:event", map[string]any{"type": "start", "job": map[string]any{"id": "j1"}})
			hub.Broadcast("job:event", map[string]any{"type": "acp", "id": "j1", "event": map[string]any{"kind": "tool", "id": "t1", "title": "Run tests", "status": "in_progress", "input": "{}", "output": strings.Repeat("x", 4096), "images": []any{map[string]any{"data": strings.Repeat("A", 8192)}}}})
			hub.Broadcast("job:event", map[string]any{"type": "acp", "id": "j1", "event": map[string]any{"kind": "turn-heartbeat", "elapsedMs": 10}})
			hub.Broadcast("job:event", map[string]any{"type": "acp", "id": "j1", "event": map[string]any{"kind": "thinking", "text": "checking the lane"}})
			hub.Broadcast("job:event", map[string]any{"type": "acp", "id": "j1", "event": map[string]any{"kind": "plan", "entries": []any{}}})
			hub.Broadcast("job:event", map[string]any{"type": "data", "id": "j1", "stream": "stdout", "chunk": "hi", "offset": 0})
			hub.Broadcast("chat:plan-usage", map[string]any{})
			hub.Broadcast("job:event", map[string]any{"type": "usage", "used": 1})
			hub.Broadcast("job:event", map[string]any{"type": "end", "job": map[string]any{"id": "j1"}})
			hub.Broadcast("test:done", true)
		}
		got := []string{}
		for {
			_, payload, _ := readServerFrame(t, peer)
			var frame struct {
				T       string          `json:"t"`
				Channel string          `json:"channel"`
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(payload, &frame); err != nil || frame.T != "event" {
				continue
			}
			var job struct {
				Type  string `json:"type"`
				Event struct {
					Kind string `json:"kind"`
				} `json:"event"`
			}
			if frame.Channel == "job:event" {
				_ = json.Unmarshal(frame.Payload, &job)
				if job.Event.Kind == "tool" {
					var raw struct {
						Event map[string]any `json:"event"`
					}
					_ = json.Unmarshal(frame.Payload, &raw)
					toolSeen[query] = raw.Event
				}
			}
			if frame.Channel == "test:done" {
				return got
			}
			if frame.Channel == "test:replayed" {
				broadcast()
				continue
			}
			if strings.HasPrefix(frame.Channel, "lan:") || frame.Channel == "wire:incoming" {
				continue
			}
			name := frame.Channel
			if frame.Channel == "job:event" {
				name += "/" + job.Type
				if job.Event.Kind != "" {
					name += "/" + job.Event.Kind
				}
			}
			got = append(got, name)
		}
	}
	full := "providers:list,chat:catalog,agent:apply,job:event/start,job:event/acp/tool,job:event/acp/turn-heartbeat,job:event/acp/thinking,job:event/acp/plan,job:event/data,chat:plan-usage,job:event/usage,job:event/end"
	if got := strings.Join(received(""), ","); got != full {
		t.Fatalf("plain client got %s\nwant %s", got, full)
	}
	lean := "agent:apply,job:event/start,job:event/acp/tool,job:event/acp/turn-heartbeat,job:event/acp/thinking,job:event/data,job:event/end"
	if got := strings.Join(received("?lean=1"), ","); got != lean {
		t.Fatalf("lean client got %s\nwant %s", got, lean)
	}
	// The plain client got the step whole; the lean one without its input,
	// output and images, but with what a Working line shows.
	if plain := toolSeen[""]; plain["output"] == nil || plain["images"] == nil || plain["input"] == nil {
		t.Fatalf("plain tool step = %#v, want it whole", plain)
	}
	slim := toolSeen["?lean=1"]
	for _, key := range []string{"input", "output", "images"} {
		if _, present := slim[key]; present {
			t.Fatalf("lean tool step kept %q: %#v", key, slim)
		}
	}
	if slim["title"] != "Run tests" || slim["status"] != "in_progress" || slim["id"] != "t1" {
		t.Fatalf("lean tool step = %#v, want its id, title and status", slim)
	}
}
