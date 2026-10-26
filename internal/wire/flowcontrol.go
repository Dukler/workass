package wire

import (
	"strconv"
	"sync/atomic"
	"time"
)

// Flow control for slow links. Behind a 400 kbps / 200 ms throttle a browser
// was dropped every 9-40 s (2026-10-25): one deadline covered a whole frame,
// so a healthy peer that needed 30 s for a 1.5 MB reply got 8. No timer below
// is a budget for a frame; each one measures the absence of progress.
//
//   - writeStallTimeout: the writer re-arms its deadline before every chunk.
//     A peer that accepts nothing for the whole window is stalled and dropped;
//     a peer that keeps accepting bytes at any rate keeps its connection,
//     however long a snapshot takes.
//   - pingInterval: when nothing has arrived from the peer for this long, a
//     WebSocket ping asks it to answer. A client that never goes that long
//     without sending sees no new frames.
//   - pongTimeout: a written ping that got no inbound bytes back for this long,
//     while the writer made no progress either and the reader was free to see
//     an answer, marks a dead peer (a half-open socket after sleep or a network
//     change, which TCP alone takes many minutes to notice).
//
// NewHub copies these defaults into the hub, and every socket copies them from
// its hub, so a test lowers them before building a hub and nothing running
// reads them afterwards.
var (
	writeStallTimeout = defaultWriteStallTimeout
	pingInterval      = 20 * time.Second
	pongTimeout       = 45 * time.Second
)

const defaultWriteStallTimeout = 45 * time.Second

// writeChunkBytes bounds one socket write, so progress is observed at least
// once per chunk. The slowest peer that survives accepts one chunk per stall
// window: 32 KiB per 45 s, about 730 B/s.
const writeChunkBytes = 32 << 10

// maxControlPayload is RFC 6455 §5.5: control frames carry at most 125 bytes.
const maxControlPayload = 125

// serverPingFrame is FIN | ping with an empty payload. It is never compressed
// (deflateTextFrame only takes text frames) and is shared read-only.
var serverPingFrame = []byte{0x89, 0x00}

// monoEpoch anchors monoNow to the monotonic clock, so wall-clock jumps never
// look like silence or progress.
var monoEpoch = time.Now()

func monoNow() int64 { return int64(time.Since(monoEpoch)) }

// liveness is what the socket's three loops tell each other about the peer.
// Times are monoNow values. "Since the ping" is decided by sequence numbers,
// not timestamps: on Windows the monotonic clock ticks coarsely enough for a
// loopback pong to share the ping's timestamp.
type liveness struct {
	// Set by the reader whenever a read returns bytes.
	inboundAt  atomic.Int64
	inboundSeq atomic.Uint64
	// Set by the writer whenever the socket accepts a chunk.
	writeProgressAt atomic.Int64
	// Non-zero while the reader is blocked handing a message to a busy lane or
	// running an inline out-of-band handler. It cannot see an answer then.
	readerBlocked atomic.Int32
	// The ping sitting in the outbound queue, if any.
	pingQueued atomic.Bool
	// When the outstanding ping was written (zero: none) and inboundSeq at
	// that moment.
	pingWrittenAt  atomic.Int64
	pingInboundSeq atomic.Uint64
}

func (l *liveness) noteInbound() {
	l.inboundAt.Store(monoNow())
	l.inboundSeq.Add(1)
}

func (l *liveness) blockReader() {
	l.readerBlocked.Add(1)
}

// unblockReader credits the peer with the time the reader could not watch it:
// an answer may be waiting in the socket buffer, unread. Credit lands before
// the counter drops, so the monitor never sees a free reader with stale input.
func (l *liveness) unblockReader() {
	l.noteInbound()
	l.readerBlocked.Add(-1)
}

func (l *liveness) noteWriteProgress() {
	l.writeProgressAt.Store(monoNow())
}

// notePingWritten starts the pong wait. inboundSeq is the count from before
// the write: on loopback the pong can arrive before the write call returns.
func (l *liveness) notePingWritten(inboundSeq uint64) {
	l.pingInboundSeq.Store(inboundSeq)
	l.pingWrittenAt.Store(max(monoNow(), 1))
	l.pingQueued.Store(false)
}

// peerDead reports a peer that let a written ping go unanswered for longer
// than pongWait. Any inbound byte after the ping answers it. Writer progress
// after the ping restarts the wait: the socket buffer draining means the
// peer is still acknowledging data, and the ping may sit behind that data.
func (l *liveness) peerDead(pongWait time.Duration) bool {
	pingAt := l.pingWrittenAt.Load()
	if pingAt == 0 {
		return false
	}
	if l.inboundSeq.Load() != l.pingInboundSeq.Load() {
		l.pingWrittenAt.CompareAndSwap(pingAt, 0)
		return false
	}
	if l.readerBlocked.Load() > 0 {
		return false
	}
	since := max(pingAt, l.writeProgressAt.Load())
	return monoNow()-since > int64(pongWait)
}

// largeFrameAnnounceBytes is the wire size from which a text frame is
// announced to a client that connected with progress=1. NewHub captures it;
// tests lower it before building a hub.
var largeFrameAnnounceBytes = defaultLargeFrameAnnounceBytes

const defaultLargeFrameAnnounceBytes = 64 << 10

// writeAnnounced writes one frame. For a client that asked for it
// (progress=1), a text frame of at least the announce size on the wire (after
// compression) is preceded by a small uncompressed wire:incoming event naming
// that size, so the client can tell a slow link from a dead daemon while the
// frame arrives. The announcement bypasses the queue and its limits; written
// here, it is always immediately followed by its frame. Control frames are
// never announced.
func (c *client) writeAnnounced(frame []byte) error {
	threshold := c.announceBytes
	if threshold <= 0 {
		threshold = defaultLargeFrameAnnounceBytes
	}
	if c.announceLarge && len(frame) >= threshold && frame[0]&0x0f == 0x1 {
		if err := c.writeWithProgress(incomingAnnouncement(len(frame))); err != nil {
			return err
		}
	}
	return c.writeWithProgress(frame)
}

// incomingAnnouncement is the frame
// {"t":"event","channel":"wire:incoming","payload":{"bytes":N}}.
func incomingAnnouncement(wireBytes int) []byte {
	payload := strconv.AppendInt([]byte(`{"t":"event","channel":"wire:incoming","payload":{"bytes":`), int64(wireBytes), 10)
	return encodeTextFrame(append(payload, "}}"...))
}

// writeWithProgress writes p in chunks, re-arming the write deadline before
// each one. A timeout therefore means the peer accepted no chunk for a whole
// stall window, never that a large frame took long. On TLS a timed-out write
// leaves the connection unusable, which is fine: the caller drops the socket.
func (c *client) writeWithProgress(p []byte) error {
	stall := c.writeStall
	if stall <= 0 {
		stall = defaultWriteStallTimeout
	}
	for len(p) > 0 {
		chunk := p[:min(len(p), writeChunkBytes)]
		_ = c.conn.SetWriteDeadline(time.Now().Add(stall))
		n, err := c.conn.Write(chunk)
		if n > 0 {
			c.live.noteWriteProgress()
		}
		if err != nil {
			return err
		}
		if n != len(chunk) {
			return errShortWrite
		}
		p = p[n:]
	}
	return nil
}

// writePing writes the liveness probe; the pong wait starts once the socket
// has taken it, not when it was queued behind other frames.
func (c *client) writePing(frame []byte) error {
	inbound := c.live.inboundSeq.Load()
	err := c.writeFrame(frame)
	if err == nil {
		c.live.notePingWritten(inbound)
	}
	return err
}

// livenessLoop pings an idle peer and drops one that stopped answering. A
// ping goes only into an idle queue (tryEnqueueFrame): a busy queue means the
// writer is working, and its stall deadline already watches the peer.
func (h *Hub) livenessLoop(c *client) {
	ticker := time.NewTicker(c.pingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
		if c.live.peerDead(c.pongWait) {
			frames, bytes := c.outboundSnapshot()
			h.logf("[wire] closing client that did not answer a ping within %s queuedFrames=%d queuedBytes=%d", c.pongWait, frames, bytes)
			h.drop(c)
			return
		}
		c.maybePing()
	}
}

func (c *client) maybePing() {
	l := &c.live
	if l.pingQueued.Load() || l.pingWrittenAt.Load() != 0 || l.readerBlocked.Load() > 0 {
		return
	}
	if monoNow()-l.inboundAt.Load() < int64(c.pingEvery) {
		return
	}
	l.pingQueued.Store(true)
	if err := c.tryEnqueueFrame(outboundFrame{payload: serverPingFrame, ping: true}); err != nil {
		l.pingQueued.Store(false)
	}
}

// answerPing queues a pong carrying the ping's payload (RFC 6455 §5.5.3).
// Like a ping it never waits: when the queue is full the writer is already
// sending, which shows the peer this end is alive.
func (c *client) answerPing(payload []byte) {
	payload = payload[:min(len(payload), maxControlPayload)]
	frame := make([]byte, 2+len(payload))
	frame[0] = 0x8A
	frame[1] = byte(len(payload))
	copy(frame[2:], payload)
	_ = c.tryEnqueueFrame(outboundFrame{payload: frame})
}
