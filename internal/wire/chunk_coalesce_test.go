package wire

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type coalescedFrame struct {
	Channel string
	Type    string
	Chunk   string
	Offset  *int64
	At      time.Time
}

// readCoalesced connects with query, broadcasts with drive once the replay is
// in, and returns the job:event frames up to test:done.
func readCoalesced(t *testing.T, query string, drive func(hub *Hub)) []coalescedFrame {
	t.Helper()
	hub := NewHub()
	hub.SetOnClientReady(func(send func(channel string, payload any) error) {
		_ = send("test:replayed", true)
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	peer := dialWSPath(t, server.URL, "/"+query, "Y2h1bmstY29hbGVzY2UtMQ==")
	defer peer.conn.Close()
	got := []coalescedFrame{}
	for {
		_, payload, _ := readServerFrame(t, peer)
		var frame struct {
			T       string          `json:"t"`
			Channel string          `json:"channel"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(payload, &frame); err != nil || frame.T != "event" {
			continue
		}
		switch frame.Channel {
		case "test:replayed":
			go drive(hub)
			continue
		case "test:done":
			return got
		case "job:event":
			var job struct {
				Type   string `json:"type"`
				Chunk  string `json:"chunk"`
				Offset *int64 `json:"offset"`
			}
			_ = json.Unmarshal(frame.Payload, &job)
			got = append(got, coalescedFrame{Channel: frame.Channel, Type: job.Type, Chunk: job.Chunk, Offset: job.Offset, At: time.Now()})
		}
	}
}

func streamChunks(hub *Hub, job string, texts []string, gap time.Duration) {
	offset := 0
	for _, text := range texts {
		hub.Broadcast("job:event", map[string]any{"type": "data", "id": job, "stream": "stdout", "chunk": text, "offset": offset})
		offset += len(text)
		time.Sleep(gap)
	}
}

// A lean client with ?ck gets a burst of stdout chunks as fewer frames
// carrying the same text in the same order, the first one at once; the
// turn's end arrives after all of it. Without the parameter (or without
// lean=1) every chunk is its own frame, as before.
func TestChunkWindowMergesStreamedTextForALeanClient(t *testing.T) {
	texts := []string{}
	for i := 0; i < 40; i++ {
		texts = append(texts, "chunk ñ "+strings.Repeat("x", i%7)+" ")
	}
	whole := strings.Join(texts, "")
	drive := func(hub *Hub) {
		hub.Broadcast("job:event", map[string]any{"type": "start", "job": map[string]any{"id": "j1"}})
		streamChunks(hub, "j1", texts, 5*time.Millisecond)
		hub.Broadcast("job:event", map[string]any{"type": "end", "job": map[string]any{"id": "j1"}})
		hub.Broadcast("test:done", true)
	}
	for _, query := range []string{"?lean=1", "?ck=1", "?lean=1&ck=0", "?lean=1&ck=5"} {
		frames := readCoalesced(t, query, drive)
		if len(frames) != len(texts)+2 {
			t.Fatalf("%s: %d frames, want every chunk as its own (%d)", query, len(frames), len(texts)+2)
		}
	}

	started := time.Now()
	frames := readCoalesced(t, "?lean=1&ck=1", drive)
	if frames[0].Type != "start" || frames[len(frames)-1].Type != "end" {
		t.Fatalf("start/end out of place: %+v", frames)
	}
	data := frames[1 : len(frames)-1]
	if len(data) >= len(texts)/2 {
		t.Fatalf("%d data frames for %d chunks: not merged", len(data), len(texts))
	}
	if data[0].Chunk != texts[0] || data[0].At.Sub(started) > 80*time.Millisecond {
		t.Fatalf("the first chunk did not go out at once: %q after %s", data[0].Chunk, data[0].At.Sub(started))
	}
	joined := ""
	next := int64(0)
	for _, frame := range data {
		if frame.Type != "data" || frame.Offset == nil || *frame.Offset != next {
			t.Fatalf("merged frame does not continue the text: %+v (want offset %d)", frame, next)
		}
		joined += frame.Chunk
		next += int64(len(frame.Chunk))
	}
	if joined != whole {
		t.Fatalf("merged text differs:\n got %q\nwant %q", joined, whole)
	}
}

// Chunks that do not follow on (another job, a gap in the offsets) are not
// merged together, and a held chunk leaves before any other event.
func TestChunkWindowKeepsJobsAndGapsApart(t *testing.T) {
	frames := readCoalesced(t, "?lean=1&ck=200", func(hub *Hub) {
		hub.Broadcast("job:event", map[string]any{"type": "data", "id": "j1", "stream": "stdout", "chunk": "a", "offset": 0})
		hub.Broadcast("job:event", map[string]any{"type": "data", "id": "j1", "stream": "stdout", "chunk": "b", "offset": 1})
		hub.Broadcast("job:event", map[string]any{"type": "data", "id": "j1", "stream": "stdout", "chunk": "c", "offset": 2})
		hub.Broadcast("job:event", map[string]any{"type": "data", "id": "j2", "stream": "stdout", "chunk": "x", "offset": 0})
		hub.Broadcast("job:event", map[string]any{"type": "data", "id": "j2", "stream": "stdout", "chunk": "z", "offset": 5})
		hub.Broadcast("job:event", map[string]any{"type": "data", "id": "j2", "stream": "system", "chunk": "!"})
		hub.Broadcast("job:event", map[string]any{"type": "end", "job": map[string]any{"id": "j1"}})
		hub.Broadcast("test:done", true)
	})
	got := []string{}
	for _, frame := range frames {
		got = append(got, frame.Type+":"+frame.Chunk)
	}
	want := "data:a data:bc data:x data:z data:! end:"
	if strings.Join(got, " ") != want {
		t.Fatalf("frames %q, want %q", strings.Join(got, " "), want)
	}
}

// Which connects ask for a window: lean with ck, or lean with compact boot
// reads (the phone app's b1); an explicit ck wins; never without lean.
func TestChunkWindowFromQuery(t *testing.T) {
	cases := map[string]time.Duration{
		"lean=1":              0,
		"ck=1":                0,
		"b1=0.a.b.d":          0,
		"lean=1&ck=1":         defaultChunkWindow,
		"lean=1&ck=250":       250 * time.Millisecond,
		"lean=1&ck=5000":      time.Second,
		"lean=1&ck=0":         0,
		"lean=1&ck=x":         0,
		"lean=1&b1=0.a.b.d":   defaultChunkWindow,
		"lean=1&b1=":          0,
		"lean=1&b1=0.a&ck=0":  0,
		"lean=1&b1=0.a&ck=40": 40 * time.Millisecond,
	}
	for query, want := range cases {
		values, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		if got := chunkWindowFromQuery(values); got != want {
			t.Errorf("%s: window %s, want %s", query, got, want)
		}
	}
}
