package wire

// A lean client is one that asked, with ?lean=1 on its socket URL, not to be
// sent the events it never reads. The phone app asks: measured on an iPhone on
// 2026-11-12, it was being sent every tool step of every running chat in full
// (one screenshot a tool read was a single 623 KB event), the model catalog
// (44 KB) every few seconds, plan usage and provider lists, and it read none
// of them. Parsing them on the phone's one JavaScript thread is what a
// streaming reply then waited behind.
//
// Additive both ways: an older daemon ignores the parameter, and a client that
// does not send it gets every event exactly as before. What a lean client
// still gets is everything a client needs to follow chats: turn start, end,
// error and streamed text (job:event start/end/error/data), thoughts and the
// turn heartbeat, permissions, lease, pairing, fleet, session refresh,
// spawned work and machines.

// leanDroppedChannels are whole channels a lean client is not sent.
var leanDroppedChannels = map[string]struct{}{
	"chat:catalog":      {},
	"chat:plan-usage":   {},
	"providers:list":    {},
	"providers:updates": {},
	"proc:changed":      {},
}

// leanDrops reports whether a lean client is not sent this event.
func leanDrops(channel string, payload any) bool {
	if _, drop := leanDroppedChannels[channel]; drop {
		return true
	}
	if channel != "job:event" {
		return false
	}
	event, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	switch event["type"] {
	case "usage", "assistant-media":
		return true
	case "acp":
		// Tool calls (inputs, outputs, images) and plans are the bulk of a
		// turn's events. Kept: the heartbeat (a few bytes, what the turn is
		// doing) and thoughts, which a client shows as the reply's live last
		// line; an agent often says what it is doing only there.
		inner, _ := event["event"].(map[string]any)
		if inner == nil {
			return true
		}
		kind := inner["kind"]
		return kind != "turn-heartbeat" && kind != "thinking"
	}
	return false
}
