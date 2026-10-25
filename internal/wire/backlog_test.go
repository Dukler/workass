package wire

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// setOutboundMemoryLimit lowers the per-client event limit for one test. Call
// it before NewHub, which captures it.
func setOutboundMemoryLimit(t *testing.T, n int) {
	t.Helper()
	old := outboundMemoryLimit
	outboundMemoryLimit = n
	t.Cleanup(func() { outboundMemoryLimit = old })
}

// secondHubClient waits for a second socket and returns the one that is not
// known.
func secondHubClient(t *testing.T, hub *Hub, known *client) *client {
	t.Helper()
	waitForClientCount(t, hub, 2)
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	for c := range hub.clients {
		if c != known {
			return c
		}
	}
	t.Fatal("second client not registered")
	return nil
}

// toggleReader throttles like throttledReader until released.
type toggleReader struct {
	r        io.Reader
	chunk    int
	pause    time.Duration
	released atomic.Bool
}

func (t *toggleReader) Read(p []byte) (int, error) {
	if t.released.Load() {
		return t.r.Read(p)
	}
	if len(p) > t.chunk {
		p = p[:t.chunk]
	}
	n, err := t.r.Read(p)
	time.Sleep(t.pause)
	return n, err
}

// eventSeq reads the seq of an event whose payload map starts with it
// (encoding/json sorts map keys, so "seq" precedes "text").
func eventSeq(payload []byte, channel string) (int, bool) {
	prefix := []byte(`{"t":"event","channel":"` + channel + `","payload":{"seq":`)
	if !bytes.HasPrefix(payload, prefix) {
		return 0, false
	}
	rest := payload[len(prefix):]
	end := bytes.IndexAny(rest, ",}")
	if end < 0 {
		return 0, false
	}
	seq, err := strconv.Atoi(string(rest[:end]))
	return seq, err == nil
}

// readBurst reads events of one channel until want have arrived, checking
// their order and size.
func readBurst(src io.Reader, channel string, want, minSize int, progress *atomic.Int32) error {
	for next := 0; next < want; {
		b0, payload, err := readFrameFrom(src)
		if err != nil {
			return fmt.Errorf("after %d events: %w", next, err)
		}
		seq, ok := eventSeq(payload, channel)
		if b0 != 0x81 || !ok {
			return fmt.Errorf("frame %#x %.80s is not a %s event", b0, payload, channel)
		}
		if seq != next || len(payload) < minSize {
			return fmt.Errorf("event %d arrived as seq %d with %d bytes", next, seq, len(payload))
		}
		next++
		if progress != nil {
			progress.Store(int32(next))
		}
	}
	return nil
}

// A slow but healthy client used to pace Broadcast, and with it every other
// client and every publisher, to its own drain rate. Now it keeps its events
// in a backlog: the broadcaster and the fast client never wait for it, and it
// still receives every event, in order.
func TestSlowClientNeverPacesFastClientOrBroadcaster(t *testing.T) {
	setFlowTimers(t, 5*time.Second, 0, 0)
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	slowPeer := dialWS(t, server.URL, "c2xvdy1wZWVyLWJhY2tsb2c=")
	defer slowPeer.conn.Close()
	slow := onlyHubClient(t, hub)
	shrinkSocketBuffers(t, slow, slowPeer.conn)
	fastPeer := dialWS(t, server.URL, "ZmFzdC1wZWVyLWJhY2tsb2c=")
	defer fastPeer.conn.Close()
	fast := secondHubClient(t, hub, slow)

	const events = 40 // 40 MiB: well past the 16 MiB channel budget
	text := strings.Repeat("x", 1<<20)
	_ = slowPeer.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	_ = fastPeer.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	// About 400 KB/s until released: the slow client needs ~100 s for the
	// burst at that rate.
	slowSrc := &toggleReader{r: slowPeer.reader, chunk: 8 << 10, pause: 20 * time.Millisecond}
	var slowGot atomic.Int32
	slowDone := make(chan error, 1)
	go func() { slowDone <- readBurst(slowSrc, "test:burst", events, 1<<20, &slowGot) }()
	fastDone := make(chan error, 1)
	go func() { fastDone <- readBurst(fastPeer.reader, "test:burst", events, 1<<20, nil) }()

	started := time.Now()
	var slowest time.Duration
	for i := 0; i < events; i++ {
		callStarted := time.Now()
		hub.Broadcast("test:burst", map[string]any{"seq": i, "text": text})
		slowest = max(slowest, time.Since(callStarted))
	}
	if slowest > 500*time.Millisecond {
		t.Fatalf("one Broadcast took %s; the slow client paced the broadcaster", slowest)
	}

	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("fast client: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("fast client did not receive the burst within 15s (slow client had %d events)", slowGot.Load())
	}
	fastElapsed := time.Since(started)
	if got := slowGot.Load(); got >= events {
		t.Fatalf("slow client already had all %d events when the fast one finished; the test did not hold it back", got)
	}
	if frames, _ := slow.outboundSnapshot(); frames == 0 {
		t.Fatal("slow client's queue was empty while it was still draining")
	}
	t.Logf("fast client done in %s; slow client had %d of %d events", fastElapsed, slowGot.Load(), events)

	slowSrc.released.Store(true)
	select {
	case err := <-slowDone:
		if err != nil {
			t.Fatalf("slow client: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("slow client never finished draining (had %d events)", slowGot.Load())
	}
	assertConnected(t, hub, slow, "after the slow client drained")
	assertConnected(t, hub, fast, "after the burst")
}

// Strict per-client FIFO: a reply never overtakes events queued before it,
// on the ordered lane or the reader's inline out-of-band lane.
func TestBackloggedClientGetsEventsAndRepliesInSendOrder(t *testing.T) {
	setFlowTimers(t, 5*time.Second, 0, 0)
	hub := NewHub()
	hub.Register("test:ordered", func([]any) (any, error) { return "ordered", nil })
	hub.RegisterOutOfBandRead("test:oob", func([]any) (any, error) { return "oob", nil })
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialWS(t, server.URL, "c2VuZC1vcmRlci1iYWNrbG9n")
	defer peer.conn.Close()
	c := onlyHubClient(t, hub)
	shrinkSocketBuffers(t, c, peer.conn)

	text := strings.Repeat("y", 1<<20)
	seq := 0
	broadcast := func(n int) {
		for ; n > 0; n-- {
			hub.Broadcast("test:order", map[string]any{"seq": seq, "text": text})
			seq++
		}
	}
	queuedReplies := func() int {
		c.outMu.Lock()
		defer c.outMu.Unlock()
		replies := 0
		for _, frame := range c.backlog {
			if frame.reply {
				replies++
			}
		}
		return replies
	}
	waitForQueuedReplies := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for queuedReplies() < want {
			if time.Now().After(deadline) {
				t.Fatalf("reply %d never joined the backlog", want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// 20 MiB of events: the channel's 16 MiB budget is full, the rest waits.
	broadcast(20)
	if frames, _ := c.outboundSnapshot(); frames <= outboundQueueByteLimit>>20 {
		t.Fatalf("queued frames = %d; the backlog is not in use", frames)
	}
	peer.sendText(t, `{"t":"invoke","id":1,"channel":"test:ordered","args":[]}`)
	waitForQueuedReplies(1)
	broadcast(5)
	peer.sendText(t, `{"t":"invoke","id":2,"channel":"test:oob","args":[]}`)
	waitForQueuedReplies(2)
	broadcast(5)

	var want []string
	for i := 0; i < 20; i++ {
		want = append(want, fmt.Sprintf("event %d", i))
	}
	want = append(want, "reply 1")
	for i := 20; i < 25; i++ {
		want = append(want, fmt.Sprintf("event %d", i))
	}
	want = append(want, "reply 2")
	for i := 25; i < 30; i++ {
		want = append(want, fmt.Sprintf("event %d", i))
	}

	_ = peer.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for i, expected := range want {
		_, payload, err := readFrameFrom(peer.reader)
		if err != nil {
			t.Fatalf("frame %d (%s): %v", i, expected, err)
		}
		got := fmt.Sprintf("%.60s", payload)
		if n, ok := eventSeq(payload, "test:order"); ok {
			got = fmt.Sprintf("event %d", n)
		} else if bytes.HasPrefix(payload, []byte(`{"t":"reply","id":1,`)) {
			got = "reply 1"
		} else if bytes.HasPrefix(payload, []byte(`{"t":"reply","id":2,`)) {
			got = "reply 2"
		}
		if got != expected {
			t.Fatalf("frame %d = %s, want %s", i, got, expected)
		}
	}
	assertConnected(t, hub, c, "after the ordered drain")
}

// A client whose queued events pass the memory limit cannot keep up: it alone
// is closed, with a log line, and its queue is released. Every other client
// keeps receiving.
func TestClientThatCannotKeepUpIsDroppedAlone(t *testing.T) {
	setFlowTimers(t, 5*time.Second, 0, 0)
	setOutboundMemoryLimit(t, 4<<20)
	var logMu sync.Mutex
	var logs strings.Builder
	hub := NewHub(Options{Logf: func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(&logs, format+"\n", args...)
	}})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	stuckPeer := dialWS(t, server.URL, "c3R1Y2stcGVlci1saW1pdA==")
	defer stuckPeer.conn.Close()
	stuck := onlyHubClient(t, hub)
	shrinkSocketBuffers(t, stuck, stuckPeer.conn)
	fastPeer := dialWS(t, server.URL, "ZmFzdC1wZWVyLWxpbWl0")
	defer fastPeer.conn.Close()
	fast := secondHubClient(t, hub, stuck)

	const events = 20 // 10 MiB against a 4 MiB limit
	text := strings.Repeat("z", 512<<10)
	_ = fastPeer.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	fastDone := make(chan error, 1)
	go func() { fastDone <- readBurst(fastPeer.reader, "test:limit", events, 512<<10, nil) }()

	for i := 0; i < events; i++ {
		hub.Broadcast("test:limit", map[string]any{"seq": i, "text": text})
	}
	waitForClientDropped(t, hub, stuck, 5*time.Second)
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("fast client: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("fast client did not receive every event")
	}
	if !hubHasClient(hub, fast) {
		t.Fatal("the fast client was dropped too")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		frames, bytes := stuck.outboundSnapshot()
		stuck.outMu.Lock()
		events, backlog := stuck.eventBytes, stuck.backlog
		stuck.outMu.Unlock()
		if frames == 0 && bytes == 0 && events == 0 && backlog == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dropped client still holds frames=%d bytes=%d eventBytes=%d", frames, bytes, events)
		}
		time.Sleep(time.Millisecond)
	}
	logMu.Lock()
	logged := logs.String()
	logMu.Unlock()
	if !strings.Contains(logged, "client cannot keep up") || !strings.Contains(logged, "limit=4194304") {
		t.Fatalf("no cannot-keep-up log line:\n%s", logged)
	}
}

// Replies are bounded by the lanes, not by the event limit: a snapshot reply
// larger than the limit, with events behind it, drops nobody.
func TestRepliesDoNotCountAgainstTheEventLimit(t *testing.T) {
	setFlowTimers(t, 5*time.Second, 0, 0)
	setOutboundMemoryLimit(t, 4<<20)
	hub := NewHub()
	body := exactBody(8 << 20)
	hub.Register("session:get", func([]any) (any, error) { return body, nil })
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialWS(t, server.URL, "cmVwbGllcy1ub3QtY291bnRlZA==")
	defer peer.conn.Close()
	c := onlyHubClient(t, hub)
	shrinkSocketBuffers(t, c, peer.conn)

	peer.sendText(t, `{"t":"invoke","id":1,"channel":"session:get","args":[]}`)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, bytes := c.outboundSnapshot(); bytes > 4<<20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot reply was never queued")
		}
		time.Sleep(time.Millisecond)
	}
	text := strings.Repeat("r", 1<<20)
	for i := 0; i < 3; i++ {
		hub.Broadcast("test:after", map[string]any{"seq": i, "text": text})
	}
	assertConnected(t, hub, c, "with a reply over the limit queued")

	_ = peer.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, payload, err := readFrameFrom(peer.reader)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if want := `{"t":"reply","id":1,"result":` + string(body) + `,"error":null}`; string(payload) != want {
		t.Fatalf("reply differs (len %d, want %d)", len(payload), len(want))
	}
	if err := readBurst(peer.reader, "test:after", 3, 1<<20, nil); err != nil {
		t.Fatalf("events after the reply: %v", err)
	}
	assertConnected(t, hub, c, "after the drain")
}

// A controller-only event waits for its controller's write, but no longer
// while holding the admission lock: other publishers keep going.
func TestControllerWriteWaitDoesNotHoldOtherPublishers(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	device, _, err := manager.ApproveDevice("controller", "127.0.0.1")
	if err != nil {
		t.Fatalf("approve controller: %v", err)
	}
	manager.EnsureController(device)
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	serverConn, peerConn := net.Pipe()
	c := addApprovedDirectClient(hub, serverConn, device)
	defer peerConn.Close()
	defer hub.drop(c)
	reader := &testWSClient{conn: peerConn, reader: bufio.NewReader(peerConn)}

	// The peer is not reading: the writer sits on this frame.
	if err := c.sendEvent("ordinary", map[string]any{"seq": 1}); err != nil {
		t.Fatalf("queue blocker: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if frames, bytes := c.outboundSnapshot(); frames == 0 && bytes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer did not take the blocker")
		}
		time.Sleep(time.Millisecond)
	}
	notified := make(chan struct{})
	go func() {
		hub.Broadcast("notify", map[string]any{"id": "note-1"})
		close(notified)
	}()
	waitForOutboundFrames(t, c, 1)
	select {
	case <-notified:
		t.Fatal("controller-only broadcast returned before its write")
	case <-time.After(50 * time.Millisecond):
	}

	published := make(chan struct{})
	go func() {
		hub.Broadcast("job:event", map[string]any{"seq": 2})
		close(published)
	}()
	waitForClosed(t, published, "publisher behind a waiting controller-only broadcast")

	for _, want := range []string{"ordinary", "notify", "job:event"} {
		if event := readEvent(t, reader); event.Channel != want {
			t.Fatalf("event = %+v, want %s", event, want)
		}
	}
	waitForClosed(t, notified, "controller-only broadcast after its write")
}
