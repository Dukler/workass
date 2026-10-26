package wire

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func announceTestHub(big, compressible []byte) *Hub {
	hub := NewHub()
	hub.Register("test:big", func([]any) (any, error) { return EncodedResult(append([]byte(nil), big...)), nil })
	hub.Register("test:compressible", func([]any) (any, error) {
		return EncodedResult(append([]byte(nil), compressible...)), nil
	})
	hub.Register("test:small", func([]any) (any, error) { return "tiny", nil })
	return hub
}

// expectAnnouncedFrame reads one announcement and the frame it announces,
// and returns that frame's first byte and payload.
func expectAnnouncedFrame(t *testing.T, peer *testWSClient) (byte, []byte) {
	t.Helper()
	b0, announcement, _ := readServerFrame(t, peer)
	prefix := `{"t":"event","channel":"wire:incoming","payload":{"bytes":`
	text := string(announcement)
	if b0 != 0x81 || len(text) >= 200 || len(text) <= len(prefix)+2 || text[:len(prefix)] != prefix || text[len(text)-2:] != "}}" {
		t.Fatalf("announcement first byte %#x payload %q", b0, announcement)
	}
	announced, err := strconv.Atoi(text[len(prefix) : len(text)-2])
	if err != nil {
		t.Fatalf("announcement bytes in %q: %v", announcement, err)
	}
	b0, payload, raw := readServerFrame(t, peer)
	if announced != len(raw) {
		t.Fatalf("announced %d bytes, the frame that followed is %d on the wire", announced, len(raw))
	}
	if announced < largeFrameAnnounceBytes {
		t.Fatalf("announced a %d-byte frame, below the %d threshold", announced, largeFrameAnnounceBytes)
	}
	return b0, payload
}

func TestProgressClientIsToldAboutLargeFramesBeforeTheyArrive(t *testing.T) {
	big := syntheticSessionJSON(1 << 20)
	compressible := []byte(`"` + strings.Repeat("a", 256<<10) + `"`)
	hub := announceTestHub(big, compressible)
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	t.Run("uncompressed", func(t *testing.T) {
		peer := dialWSPath(t, server.URL, "/?progress=1", "cHJvZ3Jlc3MtcGxhaW4=")
		defer peer.conn.Close()
		sendInvoke(t, peer, 1, "test:big")
		b0, payload := expectAnnouncedFrame(t, peer)
		if want := `{"t":"reply","id":1,"result":` + string(big) + `,"error":null}`; b0 != 0x81 || string(payload) != want {
			t.Fatalf("announced frame first byte %#x is not the intact reply", b0)
		}
		// A small reply arrives unannounced.
		sendInvoke(t, peer, 2, "test:small")
		if b0, payload, _ := readServerFrame(t, peer); b0 != 0x81 || string(payload) != `{"t":"reply","id":2,"result":"tiny","error":null}` {
			t.Fatalf("small reply first byte %#x payload %.80s", b0, payload)
		}
	})

	t.Run("compressed", func(t *testing.T) {
		peer := dialWSPathWithHeaders(t, server.URL, "/?progress=1", "cHJvZ3Jlc3MtZGVmbGF0ZQ==", "Sec-WebSocket-Extensions: "+browserDeflateOffer+"\r\n")
		defer peer.conn.Close()
		sendInvoke(t, peer, 1, "test:big")
		b0, payload := expectAnnouncedFrame(t, peer)
		if want := `{"t":"reply","id":1,"result":` + string(big) + `,"error":null}`; b0 != 0xC1 || string(inflateForTest(t, payload)) != want {
			t.Fatalf("announced frame first byte %#x is not the intact compressed reply", b0)
		}
		// The threshold is the wire size: 256 KiB that compresses below it is
		// not announced.
		sendInvoke(t, peer, 2, "test:compressible")
		b0, payload, raw := readServerFrame(t, peer)
		if b0 != 0xC1 || len(raw) >= largeFrameAnnounceBytes {
			t.Fatalf("compressible reply first byte %#x, %d bytes on the wire", b0, len(raw))
		}
		if want := `{"t":"reply","id":2,"result":` + string(compressible) + `,"error":null}`; string(inflateForTest(t, payload)) != want {
			t.Fatal("compressible reply differs")
		}
	})

	t.Run("not requested", func(t *testing.T) {
		peer := dialWS(t, server.URL, "cHJvZ3Jlc3Mtb2Zm")
		defer peer.conn.Close()
		sendInvoke(t, peer, 1, "test:big")
		_, _, raw := readServerFrame(t, peer)
		if want := encodeTextFrame([]byte(`{"t":"reply","id":1,"result":` + string(big) + `,"error":null}`)); string(raw) != string(want) {
			t.Fatalf("first frame is not the plain reply (%d bytes, want %d)", len(raw), len(want))
		}
	})
}
