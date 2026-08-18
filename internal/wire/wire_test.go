package wire

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServerFolderChannelsKeepReadsPassiveAndCreationMutating(t *testing.T) {
	if _, ok := mutatingChannels["fs:list-dir"]; ok {
		t.Fatal("listing server folders must remain a passive approved-device read")
	}
	if _, ok := mutatingChannels["fs:create-dir"]; !ok {
		t.Fatal("creating a server folder must take the controller lease")
	}
}

func TestFrameDecoderVectors(t *testing.T) {
	t.Run("masked short frame", func(t *testing.T) {
		dec := &frameDecoder{}
		messages, rest, closeFrame := dec.drain(maskedFrame(0x1, true, []byte("hello")))
		if closeFrame || len(rest) != 0 || len(messages) != 1 || string(messages[0]) != "hello" {
			t.Fatalf("decoded messages=%q rest=%d close=%v", messages, len(rest), closeFrame)
		}
	})

	t.Run("126 length frame", func(t *testing.T) {
		payload := bytes.Repeat([]byte("a"), 200)
		dec := &frameDecoder{}
		messages, rest, closeFrame := dec.drain(maskedFrame(0x1, true, payload))
		if closeFrame || len(rest) != 0 || len(messages) != 1 || !bytes.Equal(messages[0], payload) {
			t.Fatalf("decoded len=%d rest=%d close=%v", len(firstMessage(messages)), len(rest), closeFrame)
		}
	})

	t.Run("127 length frame", func(t *testing.T) {
		payload := bytes.Repeat([]byte("z"), 66000)
		dec := &frameDecoder{}
		messages, rest, closeFrame := dec.drain(maskedFrame(0x1, true, payload))
		if closeFrame || len(rest) != 0 || len(messages) != 1 || !bytes.Equal(messages[0], payload) {
			t.Fatalf("decoded len=%d rest=%d close=%v", len(firstMessage(messages)), len(rest), closeFrame)
		}
	})

	t.Run("fragmented text", func(t *testing.T) {
		dec := &frameDecoder{}
		buf := append(maskedFrame(0x1, false, []byte(`{"a"`)), maskedFrame(0x0, true, []byte(`:1}`))...)
		messages, rest, closeFrame := dec.drain(buf)
		if closeFrame || len(rest) != 0 || len(messages) != 1 || string(messages[0]) != `{"a":1}` {
			t.Fatalf("decoded messages=%q rest=%d close=%v", messages, len(rest), closeFrame)
		}
	})

	t.Run("close mid stream", func(t *testing.T) {
		dec := &frameDecoder{}
		buf := append(maskedFrame(0x1, true, []byte("before")), maskedFrame(0x8, true, nil)...)
		buf = append(buf, maskedFrame(0x1, true, []byte("after"))...)
		messages, rest, closeFrame := dec.drain(buf)
		if !closeFrame || len(messages) != 1 || string(messages[0]) != "before" || len(rest) == 0 {
			t.Fatalf("decoded messages=%q rest=%d close=%v", messages, len(rest), closeFrame)
		}
	})
}

func TestHandshakeAcceptKey(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	client := dialWS(t, server.URL, "dGhlIHNhbXBsZSBub25jZQ==")
	defer client.conn.Close()
	if client.status != "HTTP/1.1 101 Switching Protocols" {
		t.Fatalf("status = %q", client.status)
	}
	if got, want := client.headers["sec-websocket-accept"], "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
}

func TestInvokeReplyRoundTrip(t *testing.T) {
	hub := NewHub()
	hub.Register("app:meta", func(args []any) (any, error) {
		return map[string]any{"name": "workass", "daemon": true}, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	client := dialWS(t, server.URL, "bXkta2V5LTEyMzQ1Ng==")
	defer client.conn.Close()

	client.sendText(t, `{"t":"invoke","id":1,"channel":"app:meta","args":[]}`)
	reply := readReply(t, client)
	if reply.T != "reply" || reply.ID != json.Number("1") || reply.Error != nil {
		t.Fatalf("reply = %+v", reply)
	}
	result, ok := reply.Result.(map[string]any)
	if !ok || result["name"] != "workass" || result["daemon"] != true {
		t.Fatalf("result = %#v", reply.Result)
	}
}

func TestOutOfBandLivenessReadBypassesSlowOrderedInvoke(t *testing.T) {
	hub := NewHub()
	started := make(chan struct{})
	release := make(chan struct{})
	hub.Register("app-chat:new-session", func(args []any) (any, error) {
		close(started)
		<-release
		return map[string]any{"ok": true}, nil
	})
	hub.RegisterOutOfBandRead("app:meta", func(args []any) (any, error) {
		return map[string]any{"name": "workass"}, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	client := dialWS(t, server.URL, "bGl2ZW5lc3MtYnlwYXNz")
	defer client.conn.Close()

	client.sendText(t, `{"t":"invoke","id":1,"channel":"app-chat:new-session","args":[]}`)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow ordered invoke never started")
	}
	client.sendText(t, `{"t":"invoke","id":2,"channel":"app:meta","args":[]}`)
	if reply := readReply(t, client); reply.ID != json.Number("2") || reply.Error != nil {
		t.Fatalf("liveness reply behind slow invoke = %+v", reply)
	}
	close(release)
	if reply := readReply(t, client); reply.ID != json.Number("1") || reply.Error != nil {
		t.Fatalf("ordered reply after release = %+v", reply)
	}
}

func TestMutatingHandlerCannotRunOutOfBand(t *testing.T) {
	hub := NewHub()
	defer func() {
		if recover() == nil {
			t.Fatal("mutating out-of-band registration did not fail")
		}
	}()
	hub.RegisterOutOfBandRead("job:start", func(args []any) (any, error) {
		return nil, nil
	})
}

func TestOrdinaryInvokesRemainArrivalOrdered(t *testing.T) {
	hub := NewHub()
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	release := make(chan struct{})
	hub.Register("ordered", func(args []any) (any, error) {
		label := fmt.Sprint(args[0])
		if label == "first" {
			close(firstStarted)
			<-release
		} else {
			close(secondStarted)
		}
		return label, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	client := dialWS(t, server.URL, "b3JkZXJlZC1pbnZva2Vz")
	defer client.conn.Close()

	client.sendText(t, `{"t":"invoke","id":1,"channel":"ordered","args":["first"]}`)
	client.sendText(t, `{"t":"invoke","id":2,"channel":"ordered","args":["second"]}`)
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first ordered invoke never started")
	}
	select {
	case <-secondStarted:
		t.Fatal("second ordered invoke overtook the first")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if reply := readReply(t, client); reply.ID != json.Number("1") || reply.Result != "first" {
		t.Fatalf("first ordered reply = %+v", reply)
	}
	if reply := readReply(t, client); reply.ID != json.Number("2") || reply.Result != "second" {
		t.Fatalf("second ordered reply = %+v", reply)
	}
}

func TestRawResultInvokeReplyPreservesFrozenShape(t *testing.T) {
	hub := NewHub()
	raw := make(RawResult, len(`{"answer":42,"items":["a","b"]}`), 4096)
	copy(raw, `{"answer":42,"items":["a","b"]}`)
	hub.Register("session:get", func(args []any) (any, error) {
		return raw, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	client := dialWS(t, server.URL, "cmF3LXJlc3VsdC1rZXk=")
	defer client.conn.Close()

	client.sendText(t, `{"t":"invoke","id":17,"channel":"session:get","args":[]}`)
	payload := client.readText(t)
	want := `{"t":"reply","id":17,"result":{"answer":42,"items":["a","b"]},"error":null}`
	if string(payload) != want {
		t.Fatalf("raw reply = %s, want %s", payload, want)
	}
}

func TestRawResultReplyFrameReusesSpareCapacity(t *testing.T) {
	raw := make(RawResult, len(`{"ok":true}`), 1024)
	copy(raw, `{"ok":true}`)
	before := &raw[0]
	frame, replyLen, err := encodeRawResultReplyFrame(json.Number("9"), raw)
	if err != nil {
		t.Fatal(err)
	}
	if &frame[0] != before {
		t.Fatal("raw reply with spare capacity allocated a second frame buffer")
	}
	messages, rest, closeFrame := (&frameDecoder{}).drain(frame)
	if closeFrame || len(rest) != 0 || len(messages) != 1 {
		t.Fatalf("frame decode messages=%d rest=%d close=%v", len(messages), len(rest), closeFrame)
	}
	want := `{"t":"reply","id":9,"result":{"ok":true},"error":null}`
	if replyLen != len(want) || string(messages[0]) != want {
		t.Fatalf("raw frame payload = %s replyLen=%d", messages[0], replyLen)
	}
}

func TestUnknownChannelError(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	client := dialWS(t, server.URL, "dW5rbm93bi1jaGFubmVs")
	defer client.conn.Close()

	client.sendText(t, `{"t":"invoke","id":2,"channel":"missing:channel","args":[]}`)
	reply := readReply(t, client)
	if reply.Error == nil || *reply.Error != "unknown channel: missing:channel" || reply.Result != nil {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestHandlerErrorSurfacesAsReplyError(t *testing.T) {
	hub := NewHub()
	hub.Register("boom", func(args []any) (any, error) {
		return nil, errors.New("handler blew up")
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	client := dialWS(t, server.URL, "Ym9vbS1oYW5kbGVy")
	defer client.conn.Close()

	client.sendText(t, `{"t":"invoke","id":3,"channel":"boom","args":[]}`)
	reply := readReply(t, client)
	if reply.Error == nil || *reply.Error != "handler blew up" || reply.Result != nil {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestBroadcastReachesTwoClients(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	clientA := dialWS(t, server.URL, "YnJvYWRjYXN0LWtleTE=")
	defer clientA.conn.Close()
	clientB := dialWS(t, server.URL, "YnJvYWRjYXN0LWtleTI=")
	defer clientB.conn.Close()

	hub.Broadcast("job:event", map[string]any{"type": "start"})
	for _, client := range []*testWSClient{clientA, clientB} {
		var event struct {
			T       string         `json:"t"`
			Channel string         `json:"channel"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(client.readText(t), &event); err != nil {
			t.Fatalf("event json: %v", err)
		}
		if event.T != "event" || event.Channel != "job:event" || event.Payload["type"] != "start" {
			t.Fatalf("event = %+v", event)
		}
	}
}

func TestBroadcastDoesNotWaitForSlowClient(t *testing.T) {
	hub := NewHub()
	serverConn, peerConn := net.Pipe()
	client := addDirectClient(t, hub, serverConn)
	defer peerConn.Close()
	defer hub.drop(client)

	returned := make(chan struct{})
	go func() {
		hub.Broadcast("job:event", map[string]any{"type": "chunk", "text": "visible"})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		_ = peerConn.Close()
		<-returned
		t.Fatal("broadcast waited for a client whose socket was not being read")
	}
}

func TestSlowClientDoesNotDelayOrderedFramesToFastClient(t *testing.T) {
	hub := NewHub()
	slowServer, slowPeer := net.Pipe()
	slow := addDirectClient(t, hub, slowServer)
	defer slowPeer.Close()
	defer hub.drop(slow)
	fastServer, fastPeer := net.Pipe()
	fast := addDirectClient(t, hub, fastServer)
	defer fastPeer.Close()
	defer hub.drop(fast)
	reader := &testWSClient{conn: fastPeer, reader: bufio.NewReader(fastPeer)}

	for seq := 0; seq < 20; seq++ {
		hub.Broadcast("job:event", map[string]any{"type": "chunk", "seq": seq})
	}
	for want := 0; want < 20; want++ {
		var event struct {
			T       string         `json:"t"`
			Channel string         `json:"channel"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(reader.readText(t), &event); err != nil {
			t.Fatalf("event json: %v", err)
		}
		if event.Channel != "job:event" || int(event.Payload["seq"].(float64)) != want {
			t.Fatalf("ordered frame %d = %#v", want, event)
		}
	}
}

func TestSlowClientBackpressurePreservesEveryOrderedFrame(t *testing.T) {
	hub := NewHub()
	serverConn, peerConn := net.Pipe()
	client := addDirectClient(t, hub, serverConn)
	defer peerConn.Close()
	defer hub.drop(client)
	reader := &testWSClient{conn: peerConn, reader: bufio.NewReader(peerConn)}

	const count = outboundQueueFrameLimit + 2
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		for i := 0; i < count; i++ {
			hub.Broadcast("job:event", map[string]any{"type": "chunk", "seq": i})
		}
	}()
	select {
	case <-returned:
		t.Fatal("saturated event producer returned before the slow client drained")
	case <-time.After(25 * time.Millisecond):
	}

	for want := 0; want < count; want++ {
		var event struct {
			T       string         `json:"t"`
			Channel string         `json:"channel"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(reader.readText(t), &event); err != nil {
			t.Fatalf("event %d json: %v", want, err)
		}
		if event.Channel != "job:event" || int(event.Payload["seq"].(float64)) != want {
			t.Fatalf("ordered frame %d = %#v", want, event)
		}
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("event producer remained blocked after the client drained")
	}
	hub.mu.RLock()
	_, connected := hub.clients[client]
	hub.mu.RUnlock()
	if !connected {
		t.Fatal("saturated client was disconnected instead of applying lossless backpressure")
	}
}

func TestFastClientReceivesWhileAnotherClientIsSaturated(t *testing.T) {
	hub := NewHub()
	slowServer, slowPeer := net.Pipe()
	slow := addDirectClientWithoutWriter(t, hub, slowServer)
	defer slowPeer.Close()
	defer hub.drop(slow)
	for i := 0; i < outboundQueueFrameLimit; i++ {
		if err := slow.enqueue([]byte("prefill")); err != nil {
			t.Fatalf("prefill slow client frame %d: %v", i, err)
		}
	}

	fastServer, fastPeer := net.Pipe()
	fast := addDirectClient(t, hub, fastServer)
	defer fastPeer.Close()
	defer hub.drop(fast)
	reader := &testWSClient{conn: fastPeer, reader: bufio.NewReader(fastPeer)}

	returned := make(chan struct{})
	go func() {
		hub.Broadcast("job:event", map[string]any{"type": "chunk", "seq": 1})
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("broadcast returned despite a saturated client")
	case <-time.After(25 * time.Millisecond):
	}

	var event decodedWireEvent
	if err := json.Unmarshal(reader.readText(t), &event); err != nil {
		t.Fatalf("fast client event json: %v", err)
	}
	if event.T != "event" || event.Channel != "job:event" || int(event.Payload["seq"].(float64)) != 1 {
		t.Fatalf("fast client event = %+v", event)
	}

	slow.close()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("saturated producer remained blocked after the slow client closed")
	}
}

func TestBackpressuredProducerUnblocksOnDoneAndSocketFailure(t *testing.T) {
	t.Run("done", func(t *testing.T) {
		hub := NewHub()
		serverConn, peerConn := net.Pipe()
		client := addDirectClientWithoutWriter(t, hub, serverConn)
		defer peerConn.Close()
		defer hub.drop(client)

		returned := make(chan struct{})
		go func() {
			defer close(returned)
			for i := 0; i < outboundQueueFrameLimit+2; i++ {
				hub.Broadcast("job:event", map[string]any{"seq": i})
			}
		}()
		waitForOutboundFrames(t, client, outboundQueueFrameLimit)
		select {
		case <-returned:
			t.Fatal("producer returned before done was closed")
		default:
		}

		client.close()
		waitForClosed(t, returned, "done-closed producer")
	})

	t.Run("socket failure", func(t *testing.T) {
		hub := NewHub()
		serverConn, peerConn := net.Pipe()
		client := addDirectClient(t, hub, serverConn)
		defer peerConn.Close()
		defer hub.drop(client)

		returned := make(chan struct{})
		go func() {
			defer close(returned)
			for i := 0; i < outboundQueueFrameLimit+2; i++ {
				hub.Broadcast("job:event", map[string]any{"seq": i})
			}
		}()
		waitForOutboundFrames(t, client, outboundQueueFrameLimit-1)
		select {
		case <-returned:
			t.Fatal("producer returned before the peer failed")
		default:
		}

		if err := peerConn.Close(); err != nil {
			t.Fatalf("close failed peer: %v", err)
		}
		waitForClosed(t, returned, "socket-failed producer")
	})
}

func TestConcurrentBroadcastsPreserveAdmissionOrder(t *testing.T) {
	hub := NewHub()
	serverConn, peerConn := net.Pipe()
	client := addDirectClientWithoutWriter(t, hub, serverConn)
	defer peerConn.Close()
	defer hub.drop(client)
	for i := 0; i < outboundQueueFrameLimit; i++ {
		if err := client.enqueue([]byte("prefill")); err != nil {
			t.Fatalf("prefill frame %d: %v", i, err)
		}
	}

	firstDone := make(chan struct{})
	go func() {
		hub.Broadcast("job:event", map[string]any{"seq": 1})
		close(firstDone)
	}()
	if !waitForBroadcastLock(t, &hub.broadcastMu) {
		client.close()
		waitForClosed(t, firstDone, "first broadcast")
		t.Fatal("first broadcast did not enter the admission critical section")
	}

	secondDone := make(chan struct{})
	go func() {
		hub.Broadcast("job:event", map[string]any{"seq": 2})
		close(secondDone)
	}()
	select {
	case <-secondDone:
		client.close()
		waitForClosed(t, firstDone, "first broadcast after overtaking")
		t.Fatal("second broadcast overtook a blocked first broadcast")
	case <-time.After(25 * time.Millisecond):
	}

	_ = takeQueuedFrame(client)
	waitForClosed(t, firstDone, "first broadcast")
	_ = takeQueuedFrame(client)
	waitForClosed(t, secondDone, "second broadcast")

	frames := drainQueuedFrames(client)
	var seen []int
	for _, frame := range frames {
		messages, rest, closeFrame := (&frameDecoder{}).drain(frame.payload)
		if closeFrame || len(rest) != 0 || len(messages) != 1 {
			continue
		}
		var event decodedWireEvent
		if err := json.Unmarshal(messages[0], &event); err != nil || event.T != "event" {
			continue
		}
		if raw, ok := event.Payload["seq"].(float64); ok {
			seen = append(seen, int(raw))
		}
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Fatalf("concurrent broadcast order = %v, want [1 2]", seen)
	}
}

func TestControllerNotifySocketFailureQueuesBacklog(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	device, _, err := manager.ApproveDevice("controller", "127.0.0.1")
	if err != nil {
		t.Fatalf("approve controller: %v", err)
	}
	manager.EnsureController(device)
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	serverConn, peerConn := net.Pipe()
	client := addApprovedDirectClient(hub, serverConn, device)
	defer peerConn.Close()
	defer hub.drop(client)
	if err := peerConn.Close(); err != nil {
		t.Fatalf("close controller peer: %v", err)
	}

	returned := make(chan struct{})
	go func() {
		hub.Broadcast("notify", map[string]any{"id": "note-1"})
		close(returned)
	}()
	waitForClosed(t, returned, "controller notification")

	hub.mu.RLock()
	defer hub.mu.RUnlock()
	if len(hub.notifyBacklog) != 1 {
		t.Fatalf("notify backlog length = %d, want 1", len(hub.notifyBacklog))
	}
}

func TestOrdinaryInvokeReplyRejectsBoundedQueueWithoutWaiting(t *testing.T) {
	client := &client{
		outbound: make(chan outboundFrame, outboundQueueFrameLimit),
		done:     make(chan struct{}),
	}
	for i := 0; i < outboundQueueFrameLimit; i++ {
		if err := client.enqueue([]byte("queued")); err != nil {
			t.Fatalf("queued frame %d: %v", i, err)
		}
	}

	returned := make(chan error, 1)
	go func() { returned <- client.enqueueAndWait([]byte("reply")) }()
	select {
	case err := <-returned:
		if !errors.Is(err, errOutboundQueueFull) {
			t.Fatalf("ordinary reply error = %v, want queue full", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary invoke reply waited behind a bounded full queue")
	}
}

func TestOutboundQueueRejectsSecondFramePastByteBudget(t *testing.T) {
	client := &client{
		outbound: make(chan outboundFrame, outboundQueueFrameLimit),
		done:     make(chan struct{}),
	}
	frame := make([]byte, outboundQueueByteLimit/2+1)
	if err := client.enqueue(frame); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if err := client.enqueue(frame); !errors.Is(err, errOutboundQueueFull) {
		t.Fatalf("second frame error = %v, want queue full", err)
	}
}

func addDirectClient(t *testing.T, hub *Hub, conn net.Conn) *client {
	t.Helper()
	client := newDirectClient(t, hub, conn)
	go hub.writeLoop(client)
	return client
}

func addDirectClientWithoutWriter(t *testing.T, hub *Hub, conn net.Conn) *client {
	t.Helper()
	return newDirectClient(t, hub, conn)
}

func newDirectClient(t *testing.T, hub *Hub, conn net.Conn) *client {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	client := hub.newClient(conn, req)
	hub.mu.Lock()
	hub.clients[client] = struct{}{}
	hub.mu.Unlock()
	return client
}

type decodedWireEvent struct {
	T       string         `json:"t"`
	Channel string         `json:"channel"`
	Payload map[string]any `json:"payload"`
}

func waitForOutboundFrames(t *testing.T, client *client, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		frames, _ := client.outboundSnapshot()
		if frames >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbound frames = %d, want at least %d", frames, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForClosed(t *testing.T, done <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s remained blocked", name)
	}
}

func waitForBroadcastLock(t *testing.T, mutex *sync.Mutex) bool {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !mutex.TryLock() {
			return true
		}
		// The first broadcast has not run yet if this succeeds. Release the
		// probe lock and let its goroutine make progress before trying again.
		// This avoids making the ordering test depend on a scheduler sleep.
		mutex.Unlock()
		runtime.Gosched()
	}
	return false
}

func takeQueuedFrame(c *client) outboundFrame {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	frame := <-c.outbound
	c.outboundBytes -= len(frame.payload)
	if outboundFrameIsBulk(len(frame.payload)) {
		c.outboundBulkBytes -= len(frame.payload)
	}
	c.signalOutboundSpaceLocked()
	return frame
}

func drainQueuedFrames(c *client) []outboundFrame {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	frames := make([]outboundFrame, 0, len(c.outbound))
	for len(c.outbound) > 0 {
		frame := <-c.outbound
		frames = append(frames, frame)
		c.outboundBytes -= len(frame.payload)
		if outboundFrameIsBulk(len(frame.payload)) {
			c.outboundBulkBytes -= len(frame.payload)
		}
	}
	return frames
}

type decodedReply struct {
	T      string      `json:"t"`
	ID     json.Number `json:"id"`
	Result any         `json:"result"`
	Error  *string     `json:"error"`
}

type testWSClient struct {
	conn    net.Conn
	reader  *bufio.Reader
	status  string
	headers map[string]string
}

func dialWS(t *testing.T, serverURL, key string) *testWSClient {
	t.Helper()
	return dialWSPath(t, serverURL, "/", key)
}

func dialWSPath(t *testing.T, serverURL, path, key string) *testWSClient {
	t.Helper()
	addr := strings.TrimPrefix(serverURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, addr, key)
	if _, err := io.WriteString(conn, req); err != nil {
		_ = conn.Close()
		t.Fatalf("write handshake: %v", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read status: %v", err)
	}
	headers := map[string]string{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			t.Fatalf("read header: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok {
			headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}
	return &testWSClient{conn: conn, reader: reader, status: strings.TrimRight(status, "\r\n"), headers: headers}
}

func (c *testWSClient) sendText(t *testing.T, text string) {
	t.Helper()
	if _, err := c.conn.Write(maskedFrame(0x1, true, []byte(text))); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func (c *testWSClient) readText(t *testing.T) []byte {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.reader, header); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.reader, b[:]); err != nil {
			t.Fatalf("read 126 length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.reader, b[:]); err != nil {
			t.Fatalf("read 127 length: %v", err)
		}
		length = binary.BigEndian.Uint64(b[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
			t.Fatalf("read mask: %v", err)
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return payload
}

func readReply(t *testing.T, client *testWSClient) decodedReply {
	t.Helper()
	var reply decodedReply
	dec := json.NewDecoder(bytes.NewReader(client.readText(t)))
	dec.UseNumber()
	if err := dec.Decode(&reply); err != nil {
		t.Fatalf("reply json: %v", err)
	}
	return reply
}

func maskedFrame(opcode byte, fin bool, payload []byte) []byte {
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	headerLen := 2
	switch {
	case len(payload) < 126:
	case len(payload) <= 0xffff:
		headerLen = 4
	default:
		headerLen = 10
	}
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	out := make([]byte, headerLen+4+len(payload))
	out[0] = b0
	switch {
	case len(payload) < 126:
		out[1] = 0x80 | byte(len(payload))
	case len(payload) <= 0xffff:
		out[1] = 0x80 | 126
		binary.BigEndian.PutUint16(out[2:4], uint16(len(payload)))
	default:
		out[1] = 0x80 | 127
		binary.BigEndian.PutUint64(out[2:10], uint64(len(payload)))
	}
	copy(out[headerLen:], mask[:])
	for i, b := range payload {
		out[headerLen+4+i] = b ^ mask[i&3]
	}
	return out
}

func firstMessage(messages [][]byte) []byte {
	if len(messages) == 0 {
		return nil
	}
	return messages[0]
}

// Session/archive reads retain one frozen reply frame. Even though session:get
// is globally lean, one rich active transcript can cross the regular queue
// budget, so a snapshot-scale reply must still be delivered end to end.
func TestSnapshotScaleInvokeReplyIsDelivered(t *testing.T) {
	payload := strings.Repeat("a", 65<<20)
	hub := NewHub()
	hub.Register("session:get", func(args []any) (any, error) {
		return map[string]any{"blob": payload}, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	client := dialWS(t, server.URL, "bXkta2V5LTEyMzQ1Ng==")
	defer client.conn.Close()

	client.sendText(t, `{"t":"invoke","id":7,"channel":"session:get","args":[]}`)
	reply := readReply(t, client)
	if reply.T != "reply" || reply.Error != nil {
		t.Fatalf("reply = %+v", reply)
	}
	result, ok := reply.Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %T", reply.Result)
	}
	blob, _ := result["blob"].(string)
	if len(blob) != len(payload) {
		t.Fatalf("blob len = %d, want %d", len(blob), len(payload))
	}
}

// The raised ceiling must still reject a runaway frame instead of letting one
// reply exhaust memory, and a snapshot-scale first frame must remain accepted
// past the aggregate queue byte budget.
func TestOutboundFrameCeilingStillRejectsRunaway(t *testing.T) {
	client := &client{
		outbound: make(chan outboundFrame, outboundQueueFrameLimit),
		done:     make(chan struct{}),
	}
	if err := client.enqueue(make([]byte, 65<<20)); err != nil {
		t.Fatalf("snapshot-scale frame: %v", err)
	}
	if err := client.enqueue(make([]byte, outboundFrameByteLimit+1)); !errors.Is(err, errOutboundFrameTooLarge) {
		t.Fatalf("runaway frame error = %v, want too large", err)
	}
}

func TestEventAdmittedWhileBulkReplyDrains(t *testing.T) {
	client := &client{
		outbound: make(chan outboundFrame, outboundQueueFrameLimit),
		done:     make(chan struct{}),
	}
	// The 2026-07-27 iPhone shape: a 19.4MiB session:get reply is draining and
	// a job:event push arrives. Charging the reply to the shared budget made
	// the event enqueue fail, which dropped the client mid-hydration.
	bulk := make([]byte, outboundQueueByteLimit+1)
	if err := client.enqueue(bulk); err != nil {
		t.Fatalf("bulk reply: %v", err)
	}
	if err := client.enqueue(make([]byte, 64)); err != nil {
		t.Fatalf("event during bulk drain must be admitted: %v", err)
	}
}

func TestBulkReplyAdmittedBehindQueuedEvents(t *testing.T) {
	client := &client{
		outbound: make(chan outboundFrame, outboundQueueFrameLimit),
		done:     make(chan struct{}),
	}
	if err := client.enqueue(make([]byte, 64)); err != nil {
		t.Fatalf("event: %v", err)
	}
	if err := client.enqueue(make([]byte, outboundQueueByteLimit+1)); err != nil {
		t.Fatalf("snapshot reply behind queued events must be admitted: %v", err)
	}
}

func TestSecondBulkRejectedWhileOneDrains(t *testing.T) {
	client := &client{
		outbound: make(chan outboundFrame, outboundQueueFrameLimit),
		done:     make(chan struct{}),
	}
	bulk := make([]byte, outboundQueueByteLimit+1)
	if err := client.enqueue(bulk); err != nil {
		t.Fatalf("first bulk: %v", err)
	}
	if err := client.enqueue(bulk); !errors.Is(err, errOutboundQueueFull) {
		t.Fatalf("second bulk error = %v, want queue full", err)
	}
}

func TestWriteTimeoutScalesWithPayload(t *testing.T) {
	if got := writeTimeoutForPayload(0); got != writeDeadlineBase {
		t.Fatalf("empty payload timeout = %v, want %v", got, writeDeadlineBase)
	}
	// 20 MiB at the 512 KiB/s floor is 40s of patience on top of the base.
	if got, want := writeTimeoutForPayload(20<<20), writeDeadlineBase+40*time.Second; got != want {
		t.Fatalf("20MiB payload timeout = %v, want %v", got, want)
	}
}
