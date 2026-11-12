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
// error and streamed text (job:event start/end/error/data), thoughts, the
// turn heartbeat, steer receipts and each tool step without its input,
// output and images, permissions, lease, pairing, fleet, session refresh,
// spawned work and machines.

// leanDroppedChannels are whole channels a lean client is not sent.
var leanDroppedChannels = map[string]struct{}{
	"chat:catalog":      {},
	"chat:plan-usage":   {},
	"providers:list":    {},
	"providers:updates": {},
	"proc:changed":      {},
}

// leanForm is what a lean client is sent of an event: send false when it is
// not sent at all; otherwise form, which is the payload itself unless
// changed is true (a tool step without its input, output and images).
func leanForm(channel string, payload any) (form any, send bool, changed bool) {
	if _, drop := leanDroppedChannels[channel]; drop {
		return nil, false, false
	}
	if channel != "job:event" {
		return payload, true, false
	}
	event, ok := payload.(map[string]any)
	if !ok {
		return payload, true, false
	}
	switch event["type"] {
	case "usage", "assistant-media":
		return nil, false, false
	case "acp":
		inner, _ := event["event"].(map[string]any)
		if inner == nil {
			return nil, false, false
		}
		switch inner["kind"] {
		case "plan":
			// The plan is redrawn whole on every step; a lean client has
			// no plan view.
			return nil, false, false
		case "tool":
			// The step itself is what a client follows a turn by (its
			// Working line: what runs now, how many steps); the input,
			// output and images are read on demand when a step is opened.
			// Images were the heavy part: one screenshot was 623 KB.
			slim := make(map[string]any, len(inner))
			for key, value := range inner {
				switch key {
				case "input", "output", "images":
					continue
				}
				slim[key] = value
			}
			outer := make(map[string]any, len(event))
			for key, value := range event {
				outer[key] = value
			}
			outer["event"] = slim
			return outer, true, true
		}
	}
	return payload, true, false
}
