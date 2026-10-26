package wire

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// What Chromium, Safari and Node's undici offer.
const browserDeflateOffer = "permessage-deflate; client_max_window_bits"

func TestNegotiatePermessageDeflateOffers(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []string
		want   string
	}{
		{name: "browser offer", values: []string{browserDeflateOffer}, want: deflateExtensionResponse},
		{name: "bare offer", values: []string{"permessage-deflate"}, want: deflateExtensionResponse},
		{name: "case and whitespace", values: []string{"  Permessage-Deflate ;  Client_Max_Window_Bits  ;server_no_context_takeover "}, want: deflateExtensionResponse},
		{name: "client window value", values: []string{"permessage-deflate; client_max_window_bits=10"}, want: deflateExtensionResponse},
		{name: "quoted client window value", values: []string{`permessage-deflate; client_max_window_bits="12"`}, want: deflateExtensionResponse},
		{name: "both no context takeover", values: []string{"permessage-deflate; client_no_context_takeover; server_no_context_takeover"}, want: deflateExtensionResponse},
		{name: "server window 15 is echoed", values: []string{"permessage-deflate; server_max_window_bits=15"}, want: deflateExtensionResponse + "; server_max_window_bits=15"},
		{name: "server window below 15", values: []string{"permessage-deflate; server_max_window_bits=10"}},
		{name: "server window without value", values: []string{"permessage-deflate; server_max_window_bits"}},
		{name: "client window too large", values: []string{"permessage-deflate; client_max_window_bits=16"}},
		{name: "client window too small", values: []string{"permessage-deflate; client_max_window_bits=7"}},
		{name: "client window leading zero", values: []string{"permessage-deflate; client_max_window_bits=09"}},
		{name: "client window not a number", values: []string{"permessage-deflate; client_max_window_bits=ten"}},
		{name: "unterminated quote", values: []string{`permessage-deflate; client_max_window_bits="12`}},
		{name: "unknown parameter", values: []string{"permessage-deflate; mystery"}},
		{name: "duplicate parameter", values: []string{"permessage-deflate; server_no_context_takeover; server_no_context_takeover"}},
		{name: "value on a flag", values: []string{"permessage-deflate; client_no_context_takeover=1"}},
		{name: "other extension only", values: []string{"x-webkit-deflate-frame"}},
		{name: "not offered"},
		{name: "second offer after an unacceptable one", values: []string{"permessage-deflate; server_max_window_bits=10, permessage-deflate; client_max_window_bits"}, want: deflateExtensionResponse},
		{name: "second header line", values: []string{"permessage-deflate; mystery", "x-webkit-deflate-frame, permessage-deflate"}, want: deflateExtensionResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{}
			for _, value := range test.values {
				header.Add("Sec-WebSocket-Extensions", value)
			}
			got, ok := negotiatePermessageDeflate(header)
			if ok != (test.want != "") || got != test.want {
				t.Fatalf("negotiate = %q, %v; want %q", got, ok, test.want)
			}
		})
	}
}

func TestDeflateHandshakeResponseHeader(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	for _, test := range []struct {
		name  string
		offer string
		want  string
	}{
		{name: "browser offer", offer: browserDeflateOffer, want: "permessage-deflate; server_no_context_takeover; client_no_context_takeover"},
		{name: "not offered"},
		{name: "server window 10 only", offer: "permessage-deflate; server_max_window_bits=10"},
		{name: "invalid parameter", offer: "permessage-deflate; client_max_window_bits=99"},
		{name: "first offer unacceptable", offer: "permessage-deflate; server_max_window_bits=9, permessage-deflate; client_max_window_bits", want: "permessage-deflate; server_no_context_takeover; client_no_context_takeover"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := dialDeflate(t, server.URL, test.offer)
			defer client.conn.Close()
			if client.status != "HTTP/1.1 101 Switching Protocols" {
				t.Fatalf("status = %q", client.status)
			}
			got, present := client.headers["sec-websocket-extensions"]
			if test.want == "" && present {
				t.Fatalf("declined offer still answered Sec-WebSocket-Extensions: %q", got)
			}
			if test.want != "" && got != test.want {
				t.Fatalf("Sec-WebSocket-Extensions = %q, want %q", got, test.want)
			}
		})
	}
}

// A client that does not offer the extension gets the exact frame bytes it
// always got, large or small.
func TestDeflateNotNegotiatedRepliesAreByteIdentical(t *testing.T) {
	body := syntheticSessionJSON(64 << 10)
	hub := deflateTestHub(body)
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	for _, offer := range []string{"", "permessage-deflate; server_max_window_bits=10"} {
		client := dialDeflate(t, server.URL, offer)
		sendInvoke(t, client, 1, "test:big")
		_, _, raw := readServerFrame(t, client)
		want := encodeTextFrame([]byte(`{"t":"reply","id":1,"result":` + string(body) + `,"error":null}`))
		if !bytes.Equal(raw, want) {
			t.Fatalf("offer %q: reply frame differs from the uncompressed encoding (len %d, want %d, first byte %#x)", offer, len(raw), len(want), raw[0])
		}
		client.conn.Close()
	}
}

func TestDeflateLargeReplyIsCompressedAndSmallReplyIsNot(t *testing.T) {
	body := syntheticSessionJSON(256 << 10)
	hub := deflateTestHub(body)
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	client := dialDeflate(t, server.URL, browserDeflateOffer)
	defer client.conn.Close()

	sendInvoke(t, client, 1, "test:big")
	b0, payload, _ := readServerFrame(t, client)
	if b0 != 0xC1 {
		t.Fatalf("large reply first byte = %#x, want 0xc1 (FIN|RSV1|text)", b0)
	}
	want := `{"t":"reply","id":1,"result":` + string(body) + `,"error":null}`
	if got := inflateForTest(t, payload); string(got) != want {
		t.Fatalf("inflated reply differs (len %d, want %d)", len(got), len(want))
	}
	if len(payload)*2 > len(want) {
		t.Fatalf("compressed reply is %d bytes for %d of JSON", len(payload), len(want))
	}

	sendInvoke(t, client, 2, "test:echo", "tiny")
	b0, payload, _ = readServerFrame(t, client)
	if b0 != 0x81 || string(payload) != `{"t":"reply","id":2,"result":"tiny","error":null}` {
		t.Fatalf("small reply first byte %#x payload %s, want an uncompressed text frame", b0, payload)
	}
}

// Broadcast hands one encoded frame to every client; compressing it for one
// must not change what another receives.
func TestDeflateBroadcastLeavesTheSharedFrameIntact(t *testing.T) {
	hub := NewHub()
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	compressed := dialDeflate(t, server.URL, browserDeflateOffer)
	defer compressed.conn.Close()
	plain := dialDeflate(t, server.URL, "")
	defer plain.conn.Close()
	waitForClientCount(t, hub, 2)

	text := strings.Repeat("broadcast payload ", 512)
	hub.Broadcast("test:event", map[string]any{"text": text})
	want, _ := json.Marshal(eventFrame{T: "event", Channel: "test:event", Payload: map[string]any{"text": text}})

	b0, payload, _ := readServerFrame(t, compressed)
	if b0 != 0xC1 || !bytes.Equal(inflateForTest(t, payload), want) {
		t.Fatalf("compressed client got first byte %#x", b0)
	}
	_, _, raw := readServerFrame(t, plain)
	if !bytes.Equal(raw, encodeTextFrame(want)) {
		t.Fatal("uncompressed client received a modified frame")
	}
}

func TestDeflateCompressedInvokeIsAnswered(t *testing.T) {
	hub := deflateTestHub(nil)
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	client := dialDeflate(t, server.URL, browserDeflateOffer)
	defer client.conn.Close()

	invoke := []byte(`{"t":"invoke","id":7,"channel":"test:echo","args":["hello"]}`)
	if _, err := client.conn.Write(maskedFrameBits(0xC1, deflateForTest(t, invoke))); err != nil {
		t.Fatal(err)
	}
	if reply := readReply(t, client); reply.ID != "7" || reply.Result != "hello" || reply.Error != nil {
		t.Fatalf("reply = %+v", reply)
	}

	// RSV1 sits on the first frame only; continuations carry the rest.
	invoke = []byte(`{"t":"invoke","id":8,"channel":"test:echo","args":["` + strings.Repeat("fragmented ", 400) + `"]}`)
	compressed := deflateForTest(t, invoke)
	third := len(compressed) / 3
	var frames []byte
	frames = append(frames, maskedFrameBits(0x41, compressed[:third])...)
	frames = append(frames, maskedFrameBits(0x00, compressed[third:2*third])...)
	frames = append(frames, maskedFrameBits(0x80, compressed[2*third:])...)
	if _, err := client.conn.Write(frames); err != nil {
		t.Fatal(err)
	}
	b0, payload, _ := readServerFrame(t, client)
	if b0 == 0xC1 {
		payload = inflateForTest(t, payload)
	}
	want := `{"t":"reply","id":8,"result":"` + strings.Repeat("fragmented ", 400) + `","error":null}`
	if string(payload) != want {
		t.Fatalf("fragmented compressed invoke reply = %.80s...", payload)
	}

	// An uncompressed message is still fine on a compressing socket.
	sendInvoke(t, client, 9, "test:echo", "plain")
	if reply := readReply(t, client); reply.ID != "9" || reply.Result != "plain" {
		t.Fatalf("plain reply = %+v", reply)
	}
}

func TestDeflateReservedBitViolationsCloseTheConnection(t *testing.T) {
	invoke := []byte(`{"t":"invoke","id":1,"channel":"test:echo","args":["x"]}`)
	for _, test := range []struct {
		name   string
		offer  string
		frames func(t *testing.T) []byte
	}{
		{name: "RSV1 without negotiation", frames: func(t *testing.T) []byte {
			return maskedFrameBits(0xC1, deflateForTest(t, invoke))
		}},
		{name: "RSV1 on a continuation", offer: browserDeflateOffer, frames: func(t *testing.T) []byte {
			compressed := deflateForTest(t, invoke)
			return append(maskedFrameBits(0x41, compressed[:4]), maskedFrameBits(0xC0, compressed[4:])...)
		}},
		{name: "RSV1 on a ping", offer: browserDeflateOffer, frames: func(t *testing.T) []byte {
			return append(maskedFrameBits(0xC9, nil), maskedFrame(0x1, true, invoke)...)
		}},
		{name: "RSV2", offer: browserDeflateOffer, frames: func(t *testing.T) []byte {
			return maskedFrameBits(0xA1, invoke)
		}},
		{name: "RSV3", frames: func(t *testing.T) []byte {
			return maskedFrameBits(0x91, invoke)
		}},
		{name: "RSV1 on a message that does not inflate", offer: browserDeflateOffer, frames: func(t *testing.T) []byte {
			return maskedFrameBits(0xC1, []byte{0xff, 0xff, 0xff, 0xff})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			hub := NewHub()
			hub.Register("test:echo", func(args []any) (any, error) {
				calls.Add(1)
				return nil, nil
			})
			server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
			defer server.Close()
			client := dialDeflate(t, server.URL, test.offer)
			defer client.conn.Close()
			if _, err := client.conn.Write(test.frames(t)); err != nil {
				t.Fatal(err)
			}
			expectServerClosed(t, client)
			if n := calls.Load(); n != 0 {
				t.Fatalf("handler ran %d times for a rejected message", n)
			}
		})
	}
}

func TestInboundLimitBoundsDecompressedAndDeclaredSizes(t *testing.T) {
	const limit = 4096
	newServer := func(t *testing.T) (*atomic.Int32, *httptest.Server) {
		calls := &atomic.Int32{}
		hub := NewHub()
		hub.inboundLimit = limit
		hub.Register("test:echo", func(args []any) (any, error) {
			calls.Add(1)
			return len(fmt.Sprint(args...)), nil
		})
		return calls, httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	}
	invokeOf := func(textLen int) []byte {
		return []byte(`{"t":"invoke","id":1,"channel":"test:echo","args":["` + strings.Repeat("a", textLen) + `"]}`)
	}

	t.Run("inflation past the limit", func(t *testing.T) {
		calls, server := newServer(t)
		defer server.Close()
		client := dialDeflate(t, server.URL, browserDeflateOffer)
		defer client.conn.Close()
		// Under the limit once inflated: answered.
		if _, err := client.conn.Write(maskedFrameBits(0xC1, deflateForTest(t, invokeOf(limit/2)))); err != nil {
			t.Fatal(err)
		}
		if reply := readReply(t, client); reply.Error != nil || fmt.Sprint(reply.Result) != strconv.Itoa(limit/2) {
			t.Fatalf("reply = %+v", reply)
		}
		// A hundred-odd bytes that inflate to 64 KiB: refused unread.
		bomb := deflateForTest(t, invokeOf(64<<10))
		if len(bomb) > limit/8 {
			t.Fatalf("test bomb is %d bytes; it should be tiny", len(bomb))
		}
		if _, err := client.conn.Write(maskedFrameBits(0xC1, bomb)); err != nil {
			t.Fatal(err)
		}
		expectServerClosed(t, client)
		if n := calls.Load(); n != 1 {
			t.Fatalf("handler ran %d times, want only the message under the limit", n)
		}
	})

	t.Run("declared frame length past the limit", func(t *testing.T) {
		calls, server := newServer(t)
		defer server.Close()
		client := dialDeflate(t, server.URL, "")
		defer client.conn.Close()
		// A header promising 1 TiB and no payload: refused on the header.
		header := []byte{0x81, 0x80 | 127, 0, 0, 0, 0, 0, 0, 0, 0, 0x11, 0x22, 0x33, 0x44}
		binary.BigEndian.PutUint64(header[2:10], 1<<40)
		if _, err := client.conn.Write(header); err != nil {
			t.Fatal(err)
		}
		expectServerClosed(t, client)
		if n := calls.Load(); n != 0 {
			t.Fatalf("handler ran %d times", n)
		}
	})

	t.Run("uncompressed message past the limit", func(t *testing.T) {
		calls, server := newServer(t)
		defer server.Close()
		client := dialDeflate(t, server.URL, "")
		defer client.conn.Close()
		if _, err := client.conn.Write(maskedFrame(0x1, true, invokeOf(limit))); err != nil {
			t.Fatal(err)
		}
		expectServerClosed(t, client)
		if n := calls.Load(); n != 0 {
			t.Fatalf("handler ran %d times", n)
		}
	})

	t.Run("fragments past the limit", func(t *testing.T) {
		calls, server := newServer(t)
		defer server.Close()
		client := dialDeflate(t, server.URL, "")
		defer client.conn.Close()
		message := invokeOf(limit)
		half := len(message) / 2
		frames := append(maskedFrame(0x1, false, message[:half]), maskedFrame(0x0, true, message[half:])...)
		if _, err := client.conn.Write(frames); err != nil {
			t.Fatal(err)
		}
		expectServerClosed(t, client)
		if n := calls.Load(); n != 0 {
			t.Fatalf("handler ran %d times", n)
		}
	})
}

func TestInflateMessageLimitIsInclusive(t *testing.T) {
	message := []byte(strings.Repeat("x", 3000))
	compressed := deflateForTest(t, message)
	got, err := inflateMessage(compressed, len(message))
	if err != nil || !bytes.Equal(got, message) {
		t.Fatalf("inflate at the limit: %d bytes, %v", len(got), err)
	}
	if _, err := inflateMessage(compressed, len(message)-1); !errors.Is(err, errInboundMessageTooLarge) {
		t.Fatalf("inflate past the limit: %v", err)
	}
}

// The decoder that tests use on server frames still reports a protocol error
// as the end of the stream.
func TestFrameDecoderDrainEndsOnProtocolError(t *testing.T) {
	frames := append(maskedFrame(0x1, true, []byte("before")), maskedFrameBits(0xC1, []byte("x"))...)
	messages, _, closed := (&frameDecoder{}).drain(frames)
	if !closed || len(messages) != 1 || string(messages[0]) != "before" {
		t.Fatalf("messages=%q closed=%v", messages, closed)
	}
}

// Node's built-in WebSocket (undici) is an independent client implementation
// that offers permessage-deflate like a browser.
func TestDeflateInteropWithNodeWebSocket(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	body := syntheticSessionJSON(200 << 10)
	hub := deflateTestHub(body)
	server := httptest.NewUnstartedServer(http.HandlerFunc(hub.HandleUpgrade))
	var written, read atomic.Int64
	server.Listener = countingListener{Listener: server.Listener, written: &written, read: &read}
	server.Start()
	defer server.Close()

	const argLen = 100 << 10
	script := filepath.Join(t.TempDir(), "interop.mjs")
	if err := os.WriteFile(script, []byte(nodeInteropScript), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, script, "ws"+strings.TrimPrefix(server.URL, "http"), strconv.Itoa(argLen))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node client: %v: %s", err, stderr.String())
	}
	var result struct {
		Extensions string `json:"extensions"`
		Big        string `json:"big"`
		Len        string `json:"len"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("node output: %v", err)
	}
	if !strings.Contains(result.Extensions, "permessage-deflate") {
		t.Fatalf("node negotiated extensions %q", result.Extensions)
	}
	wantBig := `{"t":"reply","id":1,"result":` + string(body) + `,"error":null}`
	if result.Big != wantBig {
		t.Fatalf("node received a different large reply (len %d, want %d)", len(result.Big), len(wantBig))
	}
	if want := `{"t":"reply","id":2,"result":` + strconv.Itoa(argLen) + `,"error":null}`; result.Len != want {
		t.Fatalf("node large invoke reply = %s, want %s", result.Len, want)
	}
	if n := written.Load(); n*2 > int64(len(wantBig)) {
		t.Fatalf("server wrote %d bytes for a %d byte reply; it was not compressed", n, len(wantBig))
	}
	t.Logf("node extensions=%q; server wrote %d bytes for a %d byte reply; read %d bytes for a ~%d byte invoke", result.Extensions, written.Load(), len(wantBig), read.Load(), argLen)
}

const nodeInteropScript = `
const [url, argLen] = process.argv.slice(2);
const fail = (message) => { process.stderr.write(String(message) + "\n"); process.exit(1); };
const timer = setTimeout(() => fail("timeout"), 20000);
const replies = {};
const ws = new WebSocket(url);
ws.onerror = (event) => fail("websocket error: " + (event.message || event.type));
ws.onopen = () => {
  ws.send(JSON.stringify({ t: "invoke", id: 1, channel: "test:big", args: [] }));
  ws.send(JSON.stringify({ t: "invoke", id: 2, channel: "test:len", args: ["x".repeat(Number(argLen))] }));
};
ws.onmessage = (event) => {
  const message = JSON.parse(event.data);
  replies[message.id] = event.data;
  if (replies[1] && replies[2]) {
    clearTimeout(timer);
    process.stdout.write(JSON.stringify({ extensions: ws.extensions, big: replies[1], len: replies[2] }), () => process.exit(0));
  }
};
`

// BenchmarkDeflateTextFrame measures the outbound path at deflateLevel. Set
// WORKASS_DEFLATE_SAMPLES to a comma-separated list of JSON files to measure
// real payloads as well; their contents are never printed.
func BenchmarkDeflateTextFrame(b *testing.B) {
	type sample struct {
		name string
		body []byte
	}
	samples := []sample{
		{name: "synthetic-4KiB", body: syntheticSessionJSON(4 << 10)},
		{name: "synthetic-300KiB", body: syntheticSessionJSON(300 << 10)},
		{name: "synthetic-25MB", body: syntheticSessionJSON(25 << 20)},
	}
	for _, path := range strings.Split(os.Getenv("WORKASS_DEFLATE_SAMPLES"), ",") {
		if path = strings.TrimSpace(path); path == "" {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			b.Fatalf("sample %s: %v", filepath.Base(path), err)
		}
		samples = append(samples, sample{name: filepath.Base(path), body: body})
		var big bytes.Buffer
		big.WriteByte('[')
		for big.Len() < 25<<20 {
			if big.Len() > 1 {
				big.WriteByte(',')
			}
			big.Write(body)
		}
		big.WriteByte(']')
		samples = append(samples, sample{name: filepath.Base(path) + "-x25MB", body: big.Bytes()})
	}
	for _, s := range samples {
		frame := encodeTextFrame(s.body)
		b.Run(s.name, func(b *testing.B) {
			b.SetBytes(int64(len(s.body)))
			b.ReportAllocs()
			var size int
			for b.Loop() {
				buf, compressed, ok := deflateTextFrame(frame)
				if !ok {
					b.Fatal("frame was not compressed")
				}
				size = len(compressed)
				releaseDeflateBuffer(buf)
			}
			b.ReportMetric(float64(size)/float64(len(frame)), "ratio")
		})
	}
}

func BenchmarkInflateMessage(b *testing.B) {
	body := syntheticSessionJSON(64 << 10)
	var compressed bytes.Buffer
	w, _ := flate.NewWriter(&compressed, flate.DefaultCompression)
	w.Write(body)
	w.Flush()
	message := compressed.Bytes()[:compressed.Len()-4]
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := inflateMessage(message, inboundMessageByteLimit); err != nil {
			b.Fatal(err)
		}
	}
}

func deflateTestHub(big []byte) *Hub {
	hub := NewHub()
	hub.Register("test:big", func(args []any) (any, error) {
		return EncodedResult(append([]byte(nil), big...)), nil
	})
	hub.Register("test:echo", func(args []any) (any, error) {
		if len(args) == 0 {
			return nil, nil
		}
		return args[0], nil
	})
	hub.Register("test:len", func(args []any) (any, error) {
		text, _ := args[0].(string)
		return len(text), nil
	})
	return hub
}

func dialDeflate(t *testing.T, serverURL, offer string) *testWSClient {
	t.Helper()
	extra := ""
	if offer != "" {
		extra = "Sec-WebSocket-Extensions: " + offer + "\r\n"
	}
	return dialWSPathWithHeaders(t, serverURL, "/", "ZGVmbGF0ZS10ZXN0LWtleQ==", extra)
}

func waitForClientCount(t *testing.T, hub *Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		hub.mu.RLock()
		n := len(hub.clients)
		hub.mu.RUnlock()
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub clients = %d, want %d", n, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// readServerFrame returns the first byte, the payload, and the raw frame.
func readServerFrame(t *testing.T, c *testWSClient) (byte, []byte, []byte) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, 2, 10)
	if _, err := io.ReadFull(c.reader, header); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	if header[1]&0x80 != 0 {
		t.Fatal("server frame is masked")
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		header = header[:4]
		if _, err := io.ReadFull(c.reader, header[2:]); err != nil {
			t.Fatalf("read length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(header[2:]))
	case 127:
		header = header[:10]
		if _, err := io.ReadFull(c.reader, header[2:]); err != nil {
			t.Fatalf("read length: %v", err)
		}
		length = binary.BigEndian.Uint64(header[2:])
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return header[0], payload, append(append([]byte(nil), header...), payload...)
}

func expectServerClosed(t *testing.T, c *testWSClient) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var scratch [4096]byte
	for {
		_, err := c.reader.Read(scratch[:])
		if err == nil {
			continue
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatal("server kept the connection open")
		}
		return
	}
}

// maskedFrameBits builds a masked client frame with an exact first byte, so a
// test can set RSV bits that maskedFrame never would.
func maskedFrameBits(b0 byte, payload []byte) []byte {
	frame := maskedFrame(b0&0x0f, b0&0x80 != 0, payload)
	frame[0] = b0
	return frame
}

// deflateForTest compresses like a browser does: its own context and level,
// a sync flush, and the trailing empty stored block stripped.
func deflateForTest(t *testing.T, message []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(message); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	if !bytes.HasSuffix(out, []byte{0, 0, 0xff, 0xff}) {
		t.Fatal("sync flush did not end in an empty stored block")
	}
	return out[:len(out)-4]
}

func inflateForTest(t *testing.T, payload []byte) []byte {
	t.Helper()
	reader := flate.NewReader(io.MultiReader(bytes.NewReader(payload), strings.NewReader("\x00\x00\xff\xff\x01\x00\x00\xff\xff")))
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("inflate server payload: %v", err)
	}
	return out
}

// syntheticSessionJSON builds deterministic chat-shaped JSON with enough
// entropy (ids, hashes, varied words) that its ratio resembles a real session.
func syntheticSessionJSON(size int) []byte {
	rng := rand.New(rand.NewPCG(7692, 6455))
	words := strings.Fields("the agent read file wire frame deflate daemon renderer provider turn tool result error queue message snapshot event chat session update status running done because when that this with from into json value string number array object handler socket client server write read limit window buffer pool level ratio")
	roles := []string{"user", "assistant", "tool"}
	tools := []string{"Read", "Edit", "Bash", "Grep", "Write"}
	var b bytes.Buffer
	b.WriteByte('[')
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		var text strings.Builder
		for n := 20 + rng.IntN(80); n > 0; n-- {
			text.WriteString(words[rng.IntN(len(words))])
			if rng.IntN(9) == 0 {
				text.WriteString(".\n")
			} else {
				text.WriteByte(' ')
			}
		}
		fmt.Fprintf(&b, `{"id":"msg-%06d-%016x","role":%q,"createdAt":"2026-10-25T17:%02d:%02d.%03dZ","text":%q,"tool":{"name":%q,"callId":"toolu_%024x","input":{"file_path":"/Users/dev/project/internal/pkg%d/file%d.go","limit":%d}},"tokens":%d}`,
			i, rng.Uint64(), roles[rng.IntN(len(roles))], rng.IntN(60), rng.IntN(60), rng.IntN(1000), text.String(),
			tools[rng.IntN(len(tools))], rng.Uint64(), rng.IntN(40), rng.IntN(400), rng.IntN(2000), rng.IntN(100000))
	}
	b.WriteByte(']')
	return b.Bytes()
}

type countingListener struct {
	net.Listener
	written *atomic.Int64
	read    *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, written: l.written, read: l.read}, nil
}

type countingConn struct {
	net.Conn
	written *atomic.Int64
	read    *atomic.Int64
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// The reads a connect waits on are compressed harder than small events and
// giant snapshots; every tier inflates to the exact payload.
func TestDeflateLevelTiersInflateExactly(t *testing.T) {
	cases := []struct {
		size  int
		level int
	}{
		{deflateMinPayload, deflateLevel},
		{deflateRatioMinPayload - 1, deflateLevel},
		{deflateRatioMinPayload, deflateRatioLevel},
		{1 << 20, deflateRatioLevel},
		{deflateRatioMaxPayload, deflateRatioLevel},
		{deflateRatioMaxPayload + 1, deflateLevel},
	}
	for _, tc := range cases {
		if got := deflateLevelFor(tc.size); got != tc.level {
			t.Fatalf("deflateLevelFor(%d) = %d, want %d", tc.size, got, tc.level)
		}
	}
	for _, size := range []int{4 << 10, 64 << 10, 2 << 20} {
		var payload bytes.Buffer
		for payload.Len() < size {
			fmt.Fprintf(&payload, `{"id":"m-%d","role":"assistant","content":"row %d of a realistic page"},`, payload.Len(), payload.Len()%977)
		}
		frame := encodeTextFrame(payload.Bytes()[:size])
		buf, compressed, ok := deflateTextFrame(frame)
		if !ok || compressed[0] != 0xC1 {
			t.Fatalf("size %d was not compressed", size)
		}
		header := 2
		switch compressed[1] & 0x7f {
		case 126:
			header = 4
		case 127:
			header = 10
		}
		inflated, err := inflateMessage(append([]byte(nil), compressed[header:]...), inboundMessageByteLimit)
		releaseDeflateBuffer(buf)
		if err != nil || !bytes.Equal(inflated, payload.Bytes()[:size]) {
			t.Fatalf("size %d did not inflate to its payload: %v", size, err)
		}
	}
}
