package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A client that names the connect replay's provider list it already holds
// is not sent that list again while it is unchanged; any other replayed event,
// and a list that changed, still arrive.
func TestConnectReplaySkipsWhatTheClientHolds(t *testing.T) {
	list := []map[string]any{{"id": "mock", "name": "Mock <dev> & co", "enabled": true}}
	raw, _ := json.Marshal(list)
	sum := sha256.Sum256(raw)
	held := hex.EncodeToString(sum[:])
	if etag, ok := ReplayEtag(list); !ok || etag != held {
		t.Fatalf("ReplayEtag = %q, want the hash of the json.Marshal encoding", etag)
	}

	replayed := func(query string) []string {
		t.Helper()
		hub := NewHub()
		hub.SetOnClientReady(func(send func(channel string, payload any) error) {
			_ = send("providers:list", list)
			_ = send("chat:catalog", map[string]any{"groups": []any{}})
			_ = send("test:done", true)
		})
		server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
		defer server.Close()
		peer := dialWSPath(t, server.URL, "/"+query, "cmVwbGF5LWtub3duLTE=")
		defer peer.conn.Close()
		channels := []string{}
		for {
			_, payload, _ := readServerFrame(t, peer)
			var frame struct {
				T       string `json:"t"`
				Channel string `json:"channel"`
			}
			if err := json.Unmarshal(payload, &frame); err != nil || frame.T != "event" {
				continue
			}
			if frame.Channel == "test:done" {
				return channels
			}
			if !strings.HasPrefix(frame.Channel, "lan:") && frame.Channel != "wire:incoming" {
				channels = append(channels, frame.Channel)
			}
		}
	}
	if got := strings.Join(replayed(""), ","); got != "providers:list,chat:catalog" {
		t.Fatalf("plain replay = %s", got)
	}
	if got := strings.Join(replayed("?replayKnown=providers:list:"+held), ","); got != "chat:catalog" {
		t.Fatalf("replay with the held list = %s, want the list skipped", got)
	}
	if got := strings.Join(replayed("?replayKnown=providers:list:"+strings.Repeat("0", 64)), ","); got != "providers:list,chat:catalog" {
		t.Fatalf("replay with a stale list = %s, want the list sent", got)
	}
	if got := strings.Join(replayed("?replayKnown=providers:list:short"), ","); got != "providers:list,chat:catalog" {
		t.Fatalf("replay with a malformed etag = %s, want the list sent", got)
	}
}
