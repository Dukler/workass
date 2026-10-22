package wire

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// An EncodedResult is framed exactly like a RawResult, in its own buffer when
// it carries room for the envelope, and without a second validating scan.
func TestEncodedResultReplyKeepsTheFrozenShape(t *testing.T) {
	hub := NewHub()
	body := `[{"id":"a","text":"bold"},{"id":"b"}]`
	encoded := make(EncodedResult, len(body), len(body)+128)
	copy(encoded, body)
	hub.Register("chat:archive-load", func(args []any) (any, error) {
		return encoded, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	client := dialWS(t, server.URL, "ZW5jb2RlZC1yZXN1bHQta2V5")
	defer client.conn.Close()

	client.sendText(t, `{"t":"invoke","id":5,"channel":"chat:archive-load","args":["tab"]}`)
	payload := client.readText(t)
	want := `{"t":"reply","id":5,"result":` + body + `,"error":null}`
	if string(payload) != want {
		t.Fatalf("encoded reply = %s, want %s", payload, want)
	}

	// In-process callers that marshal the result get the same bytes. (Framing
	// consumed the first buffer, as it does a RawResult's.)
	again, err := json.Marshal(map[string]any{"result": EncodedResult(body)})
	if err != nil || string(again) != `{"result":`+body+`}` {
		t.Fatalf("marshal encoded result = %s, %v", again, err)
	}
}
