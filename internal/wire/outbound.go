package wire

import (
	"errors"
	"fmt"
)

// Per-client outbound queue. A frame enters the socket writer's channel when
// nothing is waiting ahead of it and the channel's limits allow it (128
// frames, the regular byte budget, one bulk frame at a time); otherwise it
// joins the client's backlog. The writer promotes backlog frames, in order,
// each time it finishes a frame. Channel order followed by backlog order is
// admission order, so a client receives every frame in exactly the order it
// was queued for it, replies included.
//
// Admission never waits. A slow client grows its own backlog instead of
// pacing Broadcast, and with it every other client and every publisher. The
// backlog is bounded by memoryLimit: an event that would push a client's
// queued events past it means the client cannot keep up, so the socket is
// closed and its reconnect resyncs from snapshots, the existing recovery
// path. No event is dropped from a live stream.
//
// Replies are not counted against the limit and never wait for room, though
// they still wait for their write. Waiting for room would save nothing (the
// handler already built the reply) and would either let later events overtake
// it or make those events wait, which is the pacing this design removes. The
// lanes bound them instead: the ordered lane, the history lane and the
// reader's inline out-of-band reads each hold one reply until it is written,
// so a client has at most three outstanding, each under
// outboundFrameByteLimit. A snapshot reply larger than the limit therefore
// never trips it for the events queued behind it.

// outboundMemoryLimit bounds one client's queued event bytes, channel and
// backlog together, measured uncompressed. NewHub captures it; tests lower it
// before building a hub.
var outboundMemoryLimit = defaultOutboundMemoryLimit

const defaultOutboundMemoryLimit = 64 << 20

var errClientCannotKeepUp = errors.New("websocket client cannot keep up with its events")

// admitEvent queues an event frame without waiting. A lone event is always
// admitted; one that would take the client's queued events past its memory
// limit closes the client and returns errClientCannotKeepUp.
func (c *client) admitEvent(frame outboundFrame) error {
	frame.reply = false
	c.outMu.Lock()
	if err := c.admissibleLocked(frame); err != nil {
		c.outMu.Unlock()
		return err
	}
	queued, limit := c.eventBytes, c.memoryLimit
	if limit <= 0 {
		limit = defaultOutboundMemoryLimit
	}
	if queued > 0 && queued+len(frame.payload) > limit {
		frames, bytes := len(c.outbound)+len(c.backlog), c.outboundBytes+c.backlogBytes
		c.outMu.Unlock()
		if c.logf != nil {
			c.logf("[wire] client cannot keep up: closing it so it resyncs from a snapshot queuedEventBytes=%d frameBytes=%d limit=%d queuedFrames=%d queuedBytes=%d", queued, len(frame.payload), limit, frames, bytes)
		}
		c.close()
		return fmt.Errorf("%w: %d queued event bytes, limit %d", errClientCannotKeepUp, queued, limit)
	}
	c.admitLocked(frame)
	c.outMu.Unlock()
	return nil
}

// admitReply queues an invoke reply behind everything already queued.
func (c *client) admitReply(frame outboundFrame) error {
	frame.reply = true
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if err := c.admissibleLocked(frame); err != nil {
		return err
	}
	c.admitLocked(frame)
	return nil
}

// tryEnqueueFrame queues a frame only when it can go straight into an idle
// channel: nothing in the backlog and room under the channel's limits. Pings,
// pongs and the revocation notice use it; a busy queue means the frame is
// either unnecessary (the writer is already proving liveness) or too late.
func (c *client) tryEnqueueFrame(frame outboundFrame) error {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if err := c.admissibleLocked(frame); err != nil {
		return err
	}
	if len(c.backlog) > 0 || !c.channelTakesLocked(len(frame.payload)) || !c.pushLocked(frame) {
		return errOutboundQueueFull
	}
	if !frame.reply {
		c.eventBytes += len(frame.payload)
	}
	return nil
}

// enqueue is tryEnqueueFrame for a bare frame.
func (c *client) enqueue(frame []byte) error {
	return c.tryEnqueueFrame(outboundFrame{payload: frame})
}

func (c *client) admissibleLocked(frame outboundFrame) error {
	select {
	case <-c.done:
		return errClientClosed
	default:
	}
	if len(frame.payload) > outboundFrameByteLimit {
		return errOutboundFrameTooLarge
	}
	return nil
}

// admitLocked puts frame at the tail of the client's queue.
func (c *client) admitLocked(frame outboundFrame) {
	n := len(frame.payload)
	if !frame.reply {
		c.eventBytes += n
	}
	if len(c.backlog) == 0 && c.channelTakesLocked(n) && c.pushLocked(frame) {
		return
	}
	c.backlog = append(c.backlog, frame)
	c.backlogBytes += n
}

// channelTakesLocked applies the channel's admission rules. A bulk frame
// (larger than the whole regular byte budget, a session:get reply in real
// profiles) is admitted one at a time alongside that budget.
func (c *client) channelTakesLocked(n int) bool {
	if len(c.outbound) >= outboundQueueFrameLimit {
		return false
	}
	if outboundFrameIsBulk(n) {
		return c.outboundBulkBytes == 0
	}
	return c.outboundBytes-c.outboundBulkBytes+n <= outboundQueueByteLimit
}

func (c *client) pushLocked(frame outboundFrame) bool {
	select {
	case c.outbound <- frame:
	default:
		return false
	}
	n := len(frame.payload)
	c.outboundBytes += n
	if outboundFrameIsBulk(n) {
		c.outboundBulkBytes += n
	}
	return true
}

// finishFrame is the writer's bookkeeping once a frame leaves the channel:
// release its bytes, then move what now fits from the backlog. A frame in the
// backlog always has a frame ahead of it in the channel or in the writer's
// hands, so this runs before the backlog could ever stall.
func (c *client) finishFrame(frame outboundFrame) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	c.releaseLocked(frame)
	c.promoteLocked()
}

func (c *client) releaseLocked(frame outboundFrame) {
	n := len(frame.payload)
	c.outboundBytes -= n
	if outboundFrameIsBulk(n) {
		c.outboundBulkBytes -= n
	}
	if !frame.reply {
		c.eventBytes -= n
	}
}

func (c *client) promoteLocked() {
	for len(c.backlog) > 0 {
		head := c.backlog[0]
		if !c.channelTakesLocked(len(head.payload)) || !c.pushLocked(head) {
			return
		}
		c.backlog[0] = outboundFrame{}
		c.backlog = c.backlog[1:]
		c.backlogBytes -= len(head.payload)
	}
	// Let the drained backing array go.
	c.backlog = nil
}

// releaseQueued frees every queued frame once the client is closed. Waiters
// see done instead of a write result. A frame the writer is still writing is
// released by the writer.
func (c *client) releaseQueued() {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	for i, frame := range c.backlog {
		if !frame.reply {
			c.eventBytes -= len(frame.payload)
		}
		c.backlog[i] = outboundFrame{}
	}
	c.backlog = nil
	c.backlogBytes = 0
	for {
		select {
		case frame := <-c.outbound:
			c.releaseLocked(frame)
		default:
			return
		}
	}
}

// outboundSnapshot counts every queued frame, channel and backlog, and the
// frame the writer is on.
func (c *client) outboundSnapshot() (frames int, bytes int) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	return len(c.outbound) + len(c.backlog), c.outboundBytes + c.backlogBytes
}
