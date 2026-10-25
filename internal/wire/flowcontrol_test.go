package wire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// setFlowTimers lowers the flow-control defaults for one test. Call it before
// NewHub: the hub captures them, so nothing running reads the package vars.
// A zero pingEvery disables the liveness monitor.
func setFlowTimers(t *testing.T, stall, pingEvery, pongWait time.Duration) {
	t.Helper()
	oldStall, oldPing, oldPong := writeStallTimeout, pingInterval, pongTimeout
	writeStallTimeout, pingInterval, pongTimeout = stall, pingEvery, pongWait
	t.Cleanup(func() {
		writeStallTimeout, pingInterval, pongTimeout = oldStall, oldPing, oldPong
	})
}

// throttledReader hands out at most chunk bytes per Read and then sleeps,
// which caps the rate a test client drains its socket at.
type throttledReader struct {
	r     io.Reader
	chunk int
	pause time.Duration
}

func (t *throttledReader) Read(p []byte) (int, error) {
	if len(p) > t.chunk {
		p = p[:t.chunk]
	}
	n, err := t.r.Read(p)
	time.Sleep(t.pause)
	return n, err
}

// readFrameFrom reads one unmasked server frame and returns its first byte.
func readFrameFrom(r io.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	if header[1]&0x80 != 0 {
		return 0, nil, errors.New("server frame is masked")
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(b[0])<<8 | uint64(b[1])
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		length = 0
		for _, x := range b {
			length = length<<8 | uint64(x)
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func onlyHubClient(t *testing.T, hub *Hub) *client {
	t.Helper()
	waitForClientCount(t, hub, 1)
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	if len(hub.clients) != 1 {
		t.Fatalf("hub clients = %d, want 1", len(hub.clients))
	}
	for c := range hub.clients {
		return c
	}
	return nil
}

func hubHasClient(hub *Hub, c *client) bool {
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	_, ok := hub.clients[c]
	return ok
}

func waitForClientDropped(t *testing.T, hub *Hub, c *client, within time.Duration) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(within):
		t.Fatalf("client was not dropped within %s", within)
	}
	deadline := time.Now().Add(time.Second)
	for hubHasClient(hub, c) {
		if time.Now().After(deadline) {
			t.Fatal("closed client is still registered with the hub")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertConnected(t *testing.T, hub *Hub, c *client, when string) {
	t.Helper()
	select {
	case <-c.done:
		t.Fatalf("%s: client was closed", when)
	default:
	}
	if !hubHasClient(hub, c) {
		t.Fatalf("%s: client is no longer registered", when)
	}
	if drops := atomic.LoadUint64(&hub.stats.drops); drops != 0 {
		t.Fatalf("%s: hub counted %d drops", when, drops)
	}
}

// shrinkSocketBuffers keeps the kernel from absorbing a large frame, so the
// daemon's writer really waits on the peer.
func shrinkSocketBuffers(t *testing.T, server *client, peer net.Conn) {
	t.Helper()
	if conn, ok := server.conn.(interface{ SetWriteBuffer(int) error }); ok {
		_ = conn.SetWriteBuffer(32 << 10)
	}
	if conn, ok := peer.(interface{ SetReadBuffer(int) error }); ok {
		_ = conn.SetReadBuffer(32 << 10)
	}
}

// exactBody returns a JSON string literal of n bytes with no spare capacity,
// so every reply frames it into a buffer of its own and calls can share it.
func exactBody(n int) EncodedResult {
	body := make([]byte, 0, n)
	body = append(body, '"')
	body = append(body, strings.Repeat("a", n-2)...)
	body = append(body, '"')
	return EncodedResult(body[:n:n])
}

// The measured failure: behind a 400 kbps throttle a 1.5 MB reply needed 30 s
// and the whole-frame deadline gave it 8. A peer that keeps draining, however
// slowly, must receive the whole reply and the events behind it.
func TestSlowButHealthyReaderIsNeverDropped(t *testing.T) {
	const stall = 500 * time.Millisecond
	setFlowTimers(t, stall, 0, 0)
	hub := NewHub()
	body := exactBody(3 << 20)
	hub.Register("session:get", func([]any) (any, error) { return body, nil })
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialWS(t, server.URL, "c2xvdy1oZWFsdGh5LXJlYWRlcg==")
	defer peer.conn.Close()
	c := onlyHubClient(t, hub)
	shrinkSocketBuffers(t, c, peer.conn)

	started := time.Now()
	peer.sendText(t, `{"t":"invoke","id":1,"channel":"session:get","args":[]}`)
	const events = 4
	eventText := strings.Repeat("e", 128<<10)
	go func() {
		for i := 0; i < events; i++ {
			hub.Broadcast("test:big-event", map[string]any{"seq": i, "text": eventText})
		}
	}()

	// About 1 MiB/s at most: the reply alone takes several stall windows.
	src := &throttledReader{r: peer.reader, chunk: 8 << 10, pause: 8 * time.Millisecond}
	_ = peer.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	wantReply := `{"t":"reply","id":1,"result":` + string(body) + `,"error":null}`
	gotReply := false
	gotEvents := 0
	for !gotReply || gotEvents < events {
		b0, payload, err := readFrameFrom(src)
		if err != nil {
			t.Fatalf("read after %s (reply=%v events=%d): %v", time.Since(started), gotReply, gotEvents, err)
		}
		if b0 != 0x81 {
			t.Fatalf("frame first byte = %#x, want a text frame", b0)
		}
		switch {
		case bytes.HasPrefix(payload, []byte(`{"t":"reply"`)):
			if string(payload) != wantReply {
				t.Fatalf("reply differs (len %d, want %d)", len(payload), len(wantReply))
			}
			gotReply = true
		case bytes.HasPrefix(payload, []byte(`{"t":"event","channel":"test:big-event"`)):
			if !bytes.Contains(payload, []byte(eventText)) {
				t.Fatalf("event %d arrived truncated (len %d)", gotEvents, len(payload))
			}
			gotEvents++
		default:
			t.Fatalf("unexpected frame %.80s", payload)
		}
	}
	if elapsed := time.Since(started); elapsed < 4*stall {
		t.Fatalf("transfer took %s; the throttle should have stretched it past %s", elapsed, 4*stall)
	}
	assertConnected(t, hub, c, "after the slow transfer")
}

// A peer that accepts nothing for a whole stall window is dropped, and its
// socket loops let go of it.
func TestStalledReaderIsDropped(t *testing.T) {
	setFlowTimers(t, 300*time.Millisecond, 0, 0)
	hub := NewHub()
	body := exactBody(24 << 20)
	hub.Register("session:get", func([]any) (any, error) { return body, nil })
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialWS(t, server.URL, "c3RhbGxlZC1yZWFkZXI=")
	defer peer.conn.Close()
	c := onlyHubClient(t, hub)

	started := time.Now()
	peer.sendText(t, `{"t":"invoke","id":1,"channel":"session:get","args":[]}`)
	// The peer never reads.
	waitForClientDropped(t, hub, c, 10*time.Second)
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
		t.Fatalf("dropped after %s, before a whole stall window passed", elapsed)
	}
}

// A compressed frame larger than one write chunk still arrives intact.
func TestCompressedFrameSpanningChunksArrivesIntact(t *testing.T) {
	body := syntheticSessionJSON(1 << 20)
	hub := deflateTestHub(body)
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialDeflate(t, server.URL, browserDeflateOffer)
	defer peer.conn.Close()

	sendInvoke(t, peer, 1, "test:big")
	b0, payload, _ := readServerFrame(t, peer)
	if b0 != 0xC1 {
		t.Fatalf("reply first byte = %#x, want 0xc1 (FIN|RSV1|text)", b0)
	}
	if len(payload) <= writeChunkBytes {
		t.Fatalf("compressed reply is %d bytes; the test needs more than one %d-byte chunk", len(payload), writeChunkBytes)
	}
	want := `{"t":"reply","id":1,"result":` + string(body) + `,"error":null}`
	if got := inflateForTest(t, payload); string(got) != want {
		t.Fatalf("inflated reply differs (len %d, want %d)", len(got), len(want))
	}
}

// Each lane holds one reply at a time, so what fills the queue in practice is
// events. Replies that meet that full queue must wait for room, not drop the
// socket, on every lane, including the out-of-band reads the reader runs
// inline.
func TestFullQueueRepliesWaitInsteadOfDropping(t *testing.T) {
	setFlowTimers(t, 5*time.Second, 0, 0)
	hub := NewHub()
	body := exactBody(200 << 10)
	reply := func([]any) (any, error) { return body, nil }
	lanes := []string{"test:out-of-band", "test:ordered", "test:history"}
	hub.RegisterOutOfBandRead(lanes[0], reply)
	hub.Register(lanes[1], reply)
	hub.RegisterHistoryRead(lanes[2], reply)
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialWS(t, server.URL, "ZnVsbC1xdWV1ZS1yZXBsaWVz")
	defer peer.conn.Close()
	c := onlyHubClient(t, hub)
	shrinkSocketBuffers(t, c, peer.conn)

	// Saturate the queue's byte budget with events while the peer is not
	// reading.
	const events = 150
	eventText := strings.Repeat("e", 200<<10)
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		for i := 0; i < events; i++ {
			hub.Broadcast("test:pump", map[string]any{"seq": i, "text": eventText})
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.outMu.Lock()
		regular := c.outboundBytes - c.outboundBulkBytes
		c.outMu.Unlock()
		if regular+len(body) > outboundQueueByteLimit {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("event pump never filled the queue (regular bytes %d)", regular)
		}
		time.Sleep(time.Millisecond)
	}

	const invokes = 300
	go func() {
		for id := 1; id <= invokes; id++ {
			frame := fmt.Sprintf(`{"t":"invoke","id":%d,"channel":%q,"args":[]}`, id, lanes[id%len(lanes)])
			if _, err := peer.conn.Write(maskedFrame(0x1, true, []byte(frame))); err != nil {
				return
			}
		}
	}()
	// Replies now meet a full queue; the old reply path dropped the socket here.
	time.Sleep(300 * time.Millisecond)
	assertConnected(t, hub, c, "while replies waited on a full queue")

	_ = peer.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	lastID := make([]int, len(lanes))
	replies, gotEvents := 0, 0
	replyPrefix := []byte(`{"t":"reply","id":`)
	replySuffix := []byte(`,"result":` + string(body) + `,"error":null}`)
	for replies < invokes || gotEvents < events {
		b0, payload, err := readFrameFrom(peer.reader)
		if err != nil {
			t.Fatalf("read (replies=%d events=%d): %v", replies, gotEvents, err)
		}
		if b0 != 0x81 {
			t.Fatalf("frame first byte = %#x, want a text frame", b0)
		}
		switch {
		case bytes.HasPrefix(payload, replyPrefix):
			rest := payload[len(replyPrefix):]
			digits := rest[:bytes.IndexByte(rest, ',')]
			id, err := strconv.Atoi(string(digits))
			if err != nil || !bytes.Equal(rest[len(digits):], replySuffix) {
				t.Fatalf("malformed reply %.80s", payload)
			}
			lane := id % len(lanes)
			if id <= lastID[lane] {
				t.Fatalf("%s reply %d arrived after %d", lanes[lane], id, lastID[lane])
			}
			lastID[lane] = id
			replies++
		case bytes.HasPrefix(payload, []byte(`{"t":"event","channel":"test:pump"`)):
			gotEvents++
		default:
			t.Fatalf("unexpected frame %.80s", payload)
		}
	}
	waitForClosed(t, pumped, "event pump")
	assertConnected(t, hub, c, "after every reply arrived")
}

// An idle peer is pinged, and one that answers stays connected.
func TestIdlePeerThatAnswersPingsStaysConnected(t *testing.T) {
	setFlowTimers(t, time.Second, 100*time.Millisecond, 300*time.Millisecond)
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialWS(t, server.URL, "cGluZy1hbnN3ZXJlcg==")
	defer peer.conn.Close()
	c := onlyHubClient(t, hub)

	pings := 0
	until := time.Now().Add(1200 * time.Millisecond)
	_ = peer.conn.SetReadDeadline(until)
	for {
		b0, payload, err := readFrameFrom(peer.reader)
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			break
		}
		if err != nil {
			t.Fatalf("read after %d pings: %v", pings, err)
		}
		if b0 != 0x89 || len(payload) > maxControlPayload {
			t.Fatalf("frame first byte %#x payload %d bytes, want a ping", b0, len(payload))
		}
		pings++
		if _, err := peer.conn.Write(maskedFrame(0xA, true, payload)); err != nil {
			t.Fatalf("write pong: %v", err)
		}
	}
	if pings < 2 {
		t.Fatalf("idle peer saw %d pings in 1.2s at a 100ms interval", pings)
	}
	assertConnected(t, hub, c, fmt.Sprintf("after answering %d pings", pings))
}

// A peer that keeps talking is never idle, so it never sees a ping.
func TestBusyPeerSeesNoPings(t *testing.T) {
	setFlowTimers(t, time.Second, 200*time.Millisecond, 300*time.Millisecond)
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialWS(t, server.URL, "YnVzeS1wZWVy")
	defer peer.conn.Close()
	c := onlyHubClient(t, hub)

	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// An unsolicited pong is a valid heartbeat and needs no answer.
				if _, err := peer.conn.Write(maskedFrame(0xA, true, nil)); err != nil {
					return
				}
			}
		}
	}()
	assertNoSocketData(t, peer, time.Second, "a peer that was never idle received a frame")
	close(stop)
	<-stopped
	assertConnected(t, hub, c, "after a second of heartbeats")
}

// A peer that reads but never says anything after the handshake is dead once
// a written ping goes unanswered for pongTimeout.
func TestSilentPeerIsDroppedAfterPongTimeout(t *testing.T) {
	const pongWait = 300 * time.Millisecond
	setFlowTimers(t, time.Second, 100*time.Millisecond, pongWait)
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialWS(t, server.URL, "c2lsZW50LXBlZXI=")
	defer peer.conn.Close()
	c := onlyHubClient(t, hub)
	started := time.Now()

	var pings atomic.Int32
	go func() {
		for {
			b0, _, err := readFrameFrom(peer.reader)
			if err != nil {
				return
			}
			if b0 == 0x89 {
				pings.Add(1)
			}
		}
	}()
	waitForClientDropped(t, hub, c, 3*time.Second)
	if elapsed := time.Since(started); elapsed < pongWait {
		t.Fatalf("dropped after %s, before the pong timeout", elapsed)
	}
	if pings.Load() == 0 {
		t.Fatal("peer was dropped without ever being pinged")
	}
}

// A reader blocked on the peer's own request (a slow out-of-band handler, or
// a full lane) cannot see an answer, so it must not read as a dead peer.
func TestBlockedReaderIsNotDeclaredDead(t *testing.T) {
	t.Run("out-of-band handler", func(t *testing.T) {
		setFlowTimers(t, time.Second, 100*time.Millisecond, 300*time.Millisecond)
		hub := NewHub()
		started := make(chan struct{})
		release := make(chan struct{})
		hub.RegisterOutOfBandRead("test:slow-read", func([]any) (any, error) {
			close(started)
			<-release
			return "done", nil
		})
		server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
		defer server.Close()
		peer := dialWS(t, server.URL, "YmxvY2tlZC1yZWFkZXI=")
		defer peer.conn.Close()
		c := onlyHubClient(t, hub)

		peer.sendText(t, `{"t":"invoke","id":1,"channel":"test:slow-read","args":[]}`)
		waitForClosed(t, started, "slow out-of-band handler")
		// Silent for over three pong timeouts while the handler runs.
		time.Sleep(time.Second)
		assertConnected(t, hub, c, "while the out-of-band handler ran")
		close(release)
		if reply := readReply(t, peer); reply.ID != "1" || reply.Error != nil || reply.Result != "done" {
			t.Fatalf("reply after release = %+v", reply)
		}
		assertConnected(t, hub, c, "after the handler returned")
	})

	t.Run("full ordered lane", func(t *testing.T) {
		setFlowTimers(t, time.Second, 100*time.Millisecond, 300*time.Millisecond)
		hub := NewHub()
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		hub.Register("test:slow-write", func([]any) (any, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return "done", nil
		})
		server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
		defer server.Close()
		peer := dialWS(t, server.URL, "ZnVsbC1vcmRlcmVkLWxhbmU=")
		defer peer.conn.Close()
		defer close(release)
		c := onlyHubClient(t, hub)

		// One invoke runs, a lane's worth waits, and the next one blocks the
		// reader handing it over.
		var burst bytes.Buffer
		for id := 1; id <= outboundQueueFrameLimit+2; id++ {
			burst.Write(maskedFrame(0x1, true, []byte(fmt.Sprintf(`{"t":"invoke","id":%d,"channel":"test:slow-write","args":[]}`, id))))
		}
		if _, err := peer.conn.Write(burst.Bytes()); err != nil {
			t.Fatalf("write burst: %v", err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("slow ordered handler never started")
		}
		deadline := time.Now().Add(time.Second)
		for c.live.readerBlocked.Load() == 0 {
			if time.Now().After(deadline) {
				t.Fatal("reader never blocked on the full ordered lane")
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(time.Second)
		assertConnected(t, hub, c, "while the reader waited on the ordered lane")
	})
}

// A client ping gets a pong with the same payload.
func TestClientPingIsAnsweredWithPong(t *testing.T) {
	setFlowTimers(t, time.Second, 0, 0)
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialWS(t, server.URL, "Y2xpZW50LXBpbmc=")
	defer peer.conn.Close()

	if _, err := peer.conn.Write(maskedFrame(0x9, true, []byte("are you there"))); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	b0, payload, _ := readServerFrame(t, peer)
	if b0 != 0x8A || string(payload) != "are you there" {
		t.Fatalf("answer first byte %#x payload %q, want a pong echoing the ping", b0, payload)
	}
}

// The pong wait starts at the later of the ping write and the writer's last
// progress, and any inbound byte or a blocked reader holds the verdict.
func TestPeerDeadWeighsInboundProgressAndBlockedReader(t *testing.T) {
	var l liveness
	if l.peerDead(0) {
		t.Fatal("dead with no ping outstanding")
	}
	l.noteInbound()
	l.notePingWritten(l.inboundSeq.Load())
	time.Sleep(50 * time.Millisecond)
	if !l.peerDead(30 * time.Millisecond) {
		t.Fatal("unanswered ping past the wait is not dead")
	}
	l.noteWriteProgress()
	if l.peerDead(30 * time.Millisecond) {
		t.Fatal("writer progress after the ping did not restart the wait")
	}
	time.Sleep(50 * time.Millisecond)
	l.blockReader()
	if l.peerDead(30 * time.Millisecond) {
		t.Fatal("dead while the reader was blocked")
	}
	l.unblockReader()
	if l.peerDead(30*time.Millisecond) || l.pingWrittenAt.Load() != 0 {
		t.Fatal("inbound after the ping did not answer it")
	}
}
