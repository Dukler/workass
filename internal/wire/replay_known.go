package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// The connect replay (the provider list, the catalog and the like, sent to
// every client that becomes ready) is the first thing a reload parses, and it
// is the same as last time unless something changed meanwhile: the provider
// list alone was 3 KB of the 4-5 KB a cached reload parsed before its chat
// was on screen. A client that kept an event it was replayed can name it on
// the socket URL, ?replayKnown=<channel>:<etag> (additive; an older daemon
// ignores the parameter, an older client never sends it), and the replay skips
// that event while its payload still hashes to that etag. The client keeps
// showing what it held; any later change is broadcast as always.
//
// The etag is the SHA-256 of the payload's json.Marshal encoding, which the
// client reproduces from the value it parsed exactly as it does for
// state:digest's hashes (desktop/renderer2/src/store/digest-hash.ts). A hash
// that does not match only means the event is sent.
const replayKnownLimit = 8

func replayKnownFromQuery(values []string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	known := map[string]string{}
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			cut := strings.LastIndex(item, ":")
			if cut <= 0 || cut == len(item)-1 || len(known) >= replayKnownLimit {
				continue
			}
			channel, etag := strings.TrimSpace(item[:cut]), strings.TrimSpace(item[cut+1:])
			if channel != "" && len(etag) == sha256.Size*2 {
				known[channel] = strings.ToLower(etag)
			}
		}
	}
	if len(known) == 0 {
		return nil
	}
	return known
}

// ReplayEtag is the etag a client computes for a replayed payload.
func ReplayEtag(payload any) (string, bool) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), true
}

// replaySend is sendEvent for the connect replay: an event the client named
// as known, with exactly that content, is skipped.
func (c *client) replaySend(channel string, payload any) error {
	if etag, named := c.replayKnown[channel]; named {
		if current, ok := ReplayEtag(payload); ok && current == etag {
			return nil
		}
	}
	return c.sendEvent(channel, payload)
}
