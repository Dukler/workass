package wire

import (
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

// Streamed text, merged for a lean client that asks (?ck=1 for the default
// window, ?ck=N for N milliseconds, with lean=1; the name is short because it
// rides every connect's upgrade request): a model streams a reply in ~25 ms chunks, one job:event data frame
// each (~36 a second from the mock, more from a fast model), and a phone
// draws them at most ten times a second anyway (workass-mobile
// state/liveText.ts). Every frame wakes its radio, crosses into its
// JavaScript thread and is parsed there; merged, the phone gets one frame per
// window instead of three or four.
//
// What it merges: consecutive stdout data frames of the same job and phase
// whose offsets follow on (the chunk's place in the turn's output, in UTF-8
// bytes; a frame without one merges only with frames without one). The
// merged frame is the first frame with the chunks joined, so its offset is
// the first chunk's and a client that tracks offsets sees exactly the text it
// would have seen in pieces.
//
// When: the first chunk after a quiet window goes out at once (a reply never
// starts late); chunks inside the window are held and leave together when it
// ends. Anything else queued for the client (another event, a reply) sends
// what is held first, so frames never pass each other.
//
// Additive both ways: an older daemon ignores the parameter, and a client
// that does not send it, or is not lean, gets every frame as before.

// defaultChunkWindow is ?ck=1's window: what the phone draws at most anyway.
const defaultChunkWindow = 100 * time.Millisecond

// chunkWindowFromQuery is the window a lean client asked for: 0 (off) unless
// lean=1 and ck is 1 (the default window) or a number of milliseconds from 10
// up, capped at a second.
func chunkWindowFromQuery(query url.Values) time.Duration {
	if query.Get("lean") != "1" {
		return 0
	}
	ms, err := strconv.Atoi(query.Get("ck"))
	switch {
	case err != nil || ms <= 0:
		return 0
	case ms == 1:
		return defaultChunkWindow
	case ms < 10:
		return 0
	}
	return time.Duration(min(ms, 1000)) * time.Millisecond
}

// heldChunk is the stdout text a client's window holds.
type heldChunk struct {
	payload map[string]any // the first frame's fields
	job     any
	phase   any
	offset  int64 // -1 when the frames carry none
	text    []byte
}

// streamedChunk reports whether a job:event payload is a stdout data frame
// that may be merged, and its parts.
func streamedChunk(payload any) (event map[string]any, chunk string, offset int64, ok bool) {
	event, ok = payload.(map[string]any)
	if !ok || event["type"] != "data" || event["stream"] != "stdout" {
		return nil, "", 0, false
	}
	chunk, ok = event["chunk"].(string)
	if !ok {
		return nil, "", 0, false
	}
	offset = -1
	switch v := event["offset"].(type) {
	case nil:
	case int:
		offset = int64(v)
	case int64:
		offset = v
	case float64:
		offset = int64(v)
	default:
		return nil, "", 0, false
	}
	return event, chunk, offset, true
}

// admitChunk queues a stdout data frame for a client with a chunk window:
// at once after a quiet window, otherwise held until the window ends.
func (c *client) admitChunk(event map[string]any, chunk string, offset int64) error {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if err := c.admissibleLocked(outboundFrame{}); err != nil {
		return err
	}
	if held := c.heldChunk; held != nil {
		follows := held.job == event["id"] && held.phase == event["phase"] &&
			((held.offset < 0 && offset < 0) || (held.offset >= 0 && offset == held.offset+int64(len(held.text))))
		if follows && len(held.text)+len(chunk) < outboundFrameByteLimit/2 {
			held.text = append(held.text, chunk...)
			return nil
		}
		c.flushHeldLocked()
	}
	now := time.Now()
	if wait := c.chunkSentAt.Add(c.chunkWindow).Sub(now); wait > 0 {
		c.heldChunk = &heldChunk{payload: event, job: event["id"], phase: event["phase"], offset: offset, text: append([]byte(nil), chunk...)}
		if c.chunkTimer == nil {
			c.chunkTimer = time.AfterFunc(wait, c.flushHeldOnTimer)
		} else {
			c.chunkTimer.Reset(wait)
		}
		return nil
	}
	frame, err := json.Marshal(eventFrame{T: "event", Channel: "job:event", Payload: event})
	if err != nil {
		return err
	}
	c.chunkSentAt = now
	c.admitLocked(outboundFrame{payload: encodeTextFrame(frame)})
	return nil
}

// flushHeldLocked queues what the window holds. outMu is held.
func (c *client) flushHeldLocked() {
	held := c.heldChunk
	if held == nil {
		return
	}
	c.heldChunk = nil
	if c.chunkTimer != nil {
		c.chunkTimer.Stop()
	}
	merged := make(map[string]any, len(held.payload))
	for key, value := range held.payload {
		merged[key] = value
	}
	merged["chunk"] = string(held.text)
	frame, err := json.Marshal(eventFrame{T: "event", Channel: "job:event", Payload: merged})
	if err != nil {
		return
	}
	c.chunkSentAt = time.Now()
	select {
	case <-c.done:
		return
	default:
	}
	c.admitLocked(outboundFrame{payload: encodeTextFrame(frame)})
}

func (c *client) flushHeldOnTimer() {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	c.flushHeldLocked()
}
