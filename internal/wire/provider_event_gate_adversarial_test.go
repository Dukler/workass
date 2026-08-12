package wire

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestPhaseCWireSaturationPreservesTerminalAndFrozenEventEnvelope(t *testing.T) {
	hub := NewHub()
	serverConn, peerConn := net.Pipe()
	client := addDirectClientWithoutWriter(t, hub, serverConn)
	defer peerConn.Close()
	defer hub.drop(client)

	for index := 0; index < outboundQueueFrameLimit; index++ {
		if err := client.enqueue([]byte("prefill")); err != nil {
			t.Fatalf("prefill frame %d: %v", index, err)
		}
	}

	done := make(chan struct{})
	go func() {
		hub.Broadcast("job:event", map[string]any{"kind": "chunk", "seq": 1})
		hub.Broadcast("job:event", map[string]any{"kind": "terminal", "seq": 2, "status": "done"})
		close(done)
	}()
	if !waitForBroadcastLock(t, &hub.broadcastMu) {
		client.close()
		waitForClosed(t, done, "saturated provider publication")
		t.Fatal("saturated provider publication did not enter the admission boundary")
	}

	for index := 0; index < outboundQueueFrameLimit; index++ {
		_ = takeQueuedFrame(client)
	}
	waitForOutboundFrames(t, client, 1)
	chunk := takeQueuedFrame(client)
	waitForOutboundFrames(t, client, 1)
	terminal := takeQueuedFrame(client)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal provider publication remained blocked after queue drain")
	}

	for _, test := range []struct {
		name  string
		frame outboundFrame
		want  string
	}{
		{name: "chunk", frame: chunk, want: `{"t":"event","channel":"job:event","payload":{"kind":"chunk","seq":1}}`},
		{name: "terminal", frame: terminal, want: `{"t":"event","channel":"job:event","payload":{"kind":"terminal","seq":2,"status":"done"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages, rest, closeFrame := (&frameDecoder{}).drain(test.frame.payload)
			if closeFrame || len(rest) != 0 || len(messages) != 1 {
				t.Fatalf("decoded messages=%d rest=%d close=%v", len(messages), len(rest), closeFrame)
			}
			if got := string(messages[0]); got != test.want {
				t.Fatalf("frozen event bytes=%s, want %s", got, test.want)
			}
		})
	}
}

func TestPhaseCWireDisconnectReconnectRehydratesBeforeLiveEvents(t *testing.T) {
	readySent := make(chan struct{})
	hub := NewHub(Options{
		OnClientReady: func(send func(channel string, payload any) error) {
			if err := send("session:hydrate", map[string]any{"chatId": "phase-c-chat", "lastSequence": 1}); err != nil {
				return
			}
			close(readySent)
		},
	})
	oldServer, oldPeer := net.Pipe()
	old := addDirectClientWithoutWriter(t, hub, oldServer)
	defer oldPeer.Close()
	defer hub.drop(old)
	for index := 0; index < outboundQueueFrameLimit; index++ {
		if err := old.enqueue([]byte("stale-renderer-queue")); err != nil {
			t.Fatalf("old renderer prefill %d: %v", index, err)
		}
	}

	blocked := make(chan struct{})
	go func() {
		hub.Broadcast("job:event", map[string]any{"kind": "chunk", "seq": 1})
		close(blocked)
	}()
	if !waitForBroadcastLock(t, &hub.broadcastMu) {
		old.close()
		waitForClosed(t, blocked, "disconnected renderer publication")
		t.Fatal("disconnected renderer publication did not reach backpressure")
	}
	old.close()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("provider publication did not recover after renderer disconnect")
	}

	newServer, newPeer := net.Pipe()
	reconnected := addDirectClientWithoutWriter(t, hub, newServer)
	defer newPeer.Close()
	defer hub.drop(reconnected)
	hub.announceClientReady(reconnected)
	select {
	case <-readySent:
	case <-time.After(time.Second):
		t.Fatal("reconnected renderer did not receive authoritative hydration")
	}
	hub.Broadcast("job:event", map[string]any{"kind": "chunk", "seq": 2})

	first := takeQueuedFrame(reconnected)
	second := takeQueuedFrame(reconnected)
	for _, frame := range []outboundFrame{first, second} {
		messages, rest, closeFrame := (&frameDecoder{}).drain(frame.payload)
		if closeFrame || len(rest) != 0 || len(messages) != 1 {
			t.Fatalf("reconnected frame decoded messages=%d rest=%d close=%v", len(messages), len(rest), closeFrame)
		}
		var event decodedWireEvent
		if err := json.Unmarshal(messages[0], &event); err != nil {
			t.Fatalf("reconnected event json: %v", err)
		}
		if event.T != "event" {
			t.Fatalf("reconnected frame type=%q, want event", event.T)
		}
	}
	firstMessages, _, _ := (&frameDecoder{}).drain(first.payload)
	secondMessages, _, _ := (&frameDecoder{}).drain(second.payload)
	var firstEvent, secondEvent decodedWireEvent
	if err := json.Unmarshal(firstMessages[0], &firstEvent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(secondMessages[0], &secondEvent); err != nil {
		t.Fatal(err)
	}
	if firstEvent.Channel != "session:hydrate" || secondEvent.Channel != "job:event" {
		t.Fatalf("reconnect order=%q then %q, want hydration then live event", firstEvent.Channel, secondEvent.Channel)
	}
	if got := int(secondEvent.Payload["seq"].(float64)); got != 2 {
		t.Fatalf("reconnected live sequence=%d, want 2 after hydrated sequence 1", got)
	}
}
