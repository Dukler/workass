package wire

import "encoding/json"

// Boot reads: the reads a reconnecting client is about to make anyway, named on
// the socket URL (?boot=<JSON array of invoke frames>) so the daemon answers
// them as soon as the token is accepted, in the same flight as the upgrade
// response, instead of one round trip after the client sees `ready`.
//
// Measured 2026-11-12 on the phone lane (1.6 Mbit/s, 300 ms RTT): a reconnect
// was the TCP + TLS + WebSocket handshakes (3 RTT) and then the list reads, one
// more RTT even when pipelined. The bytes are a few KB, so the reconnect is
// bound by round trips; this removes the last one.
//
// Additive in both directions:
//   - an older daemon ignores the parameter; its `lan:access-state` approval
//     carries no `boot` count, and the client then sends the same frames
//     itself the moment it is ready;
//   - an older client never sends it.
//
// Only a token-authenticated connection runs them, through the same lanes and
// gates as frames read from the socket (handleInvoke), after the approval
// event. Only the read-only channels below are accepted: one entry outside
// them, or a malformed list, and the whole list is ignored (no `boot` in the
// approval, so the client falls back to sending them). Replies are ordinary
// reply frames matched by the ids the client chose.
const (
	bootReadLimit = 8
	bootReadBytes = 8 << 10
)

// bootReadChannels are the reads a client may name on the URL. Read-only and
// cheap or already conditional; settings:get is accepted but a client should
// name it last (it may build the model catalog on the ordered lane).
var bootReadChannels = map[string]struct{}{
	"state:digest":             {},
	"session:get":              {},
	"chat:permissions-pending": {},
	"spawned-work:summary":     {},
	"machines:list":            {},
	"settings:get":             {},
	"app:meta":                 {},
}

func bootReadsFromQuery(value string) []invokeMessage {
	if value == "" || len(value) > bootReadBytes {
		return nil
	}
	var frames []json.RawMessage
	if err := json.Unmarshal([]byte(value), &frames); err != nil || len(frames) == 0 || len(frames) > bootReadLimit {
		return nil
	}
	reads := make([]invokeMessage, 0, len(frames))
	seen := map[string]struct{}{}
	for _, raw := range frames {
		msg, ok := decodeInvoke(raw)
		if !ok || msg.ID == nil {
			return nil
		}
		if _, allowed := bootReadChannels[msg.Channel]; !allowed {
			return nil
		}
		if _, mutating := mutatingChannels[msg.Channel]; mutating {
			return nil
		}
		id, err := json.Marshal(msg.ID)
		if err != nil {
			return nil
		}
		if _, dup := seen[string(id)]; dup {
			return nil
		}
		seen[string(id)] = struct{}{}
		reads = append(reads, msg)
	}
	return reads
}
