package wire

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

type bootCompactFixture struct {
	Template int `json:"template"`
	Valid    []struct {
		Label  string `json:"label"`
		Value  string `json:"value"`
		Frames []any  `json:"frames"`
	} `json:"valid"`
	Invalid []string `json:"invalid"`
}

func loadBootCompactFixture(t *testing.T) bootCompactFixture {
	t.Helper()
	// A copy of workass-mobile scripts/fixtures/boot-compact.json, which the
	// phone's check (scripts/check-boot-compact.mjs) generates and verifies.
	raw, err := os.ReadFile("testdata/boot-compact.json")
	if err != nil {
		t.Fatal(err)
	}
	var f bootCompactFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Template != 1 || len(f.Valid) == 0 || len(f.Invalid) == 0 {
		t.Fatalf("fixture = template %d, %d valid, %d invalid", f.Template, len(f.Valid), len(f.Invalid))
	}
	return f
}

// The daemon's expansion of each b1 value is exactly the frames the phone would
// have sent as ?boot= JSON (same fixture as the phone's own mirror).
func TestBootCompactExpandsToThePhonesFrames(t *testing.T) {
	f := loadBootCompactFixture(t)
	for _, c := range f.Valid {
		t.Run(c.Label, func(t *testing.T) {
			raw, ok := expandBootCompact(c.Value)
			if !ok {
				t.Fatalf("%q did not expand", c.Value)
			}
			var got []any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.Frames) {
				want, _ := json.Marshal(c.Frames)
				t.Fatalf("expansion\n got  %s\n want %s", raw, want)
			}
			reads := bootReadsFromCompact(c.Value)
			if len(reads) != len(c.Frames) {
				t.Fatalf("bootReadsFromCompact = %d reads, want %d", len(reads), len(c.Frames))
			}
		})
	}
}

func TestBootCompactRefusesMalformedValues(t *testing.T) {
	f := loadBootCompactFixture(t)
	for _, v := range append(f.Invalid, "1.1..dAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "1.1..p.chat") {
		if raw, ok := expandBootCompact(v); ok {
			t.Fatalf("%q expanded to %s, want nothing", v, raw)
		}
		if reads := bootReadsFromCompact(v); reads != nil {
			t.Fatalf("%q gave %d reads, want none", v, len(reads))
		}
	}
}

// End to end: a b1 connect is answered before any frame, like ?boot=, and the
// approval counts the expanded reads.
func TestBootCompactIsAnsweredBeforeAnyFrame(t *testing.T) {
	_, token, server, calls := bootHub(t)
	defer server.Close()
	client := dialWSPath(t, server.URL, "/?deviceToken="+token+"&lean=1&b1=7.mw70ajk0..d.s", "Ym9vdC1jb21wYWN0LTAx")
	defer client.conn.Close()
	approved := readEvent(t, client)
	if approved.Channel != "lan:access-state" || approved.Payload["state"] != "approved" {
		t.Fatalf("first frame = %+v, want the approval", approved)
	}
	if n, _ := approved.Payload["boot"].(float64); n != 2 {
		t.Fatalf("approval boot = %v, want 2", approved.Payload["boot"])
	}
	first := readReply(t, client)
	second := readReply(t, client)
	if first.ID.String() != "7" || second.ID.String() != "8" {
		t.Fatalf("boot replies ids = %v, %v; want 7 then 8", first.ID, second.ID)
	}
	result, _ := first.Result.(map[string]any)
	args, _ := result["args"].([]any)
	arg, _ := args[0].(map[string]any)
	page, _ := arg["page"].(map[string]any)
	if fmt.Sprint(page["settledBefore"]) != "1794092400000" || arg["omit"] == nil {
		t.Fatalf("digest ran with %+v, want the template's page and omit", arg)
	}
	if *calls != 2 {
		t.Fatalf("handlers ran %d times, want 2", *calls)
	}
}

// The JSON form wins when a client names both; a bad b1 runs nothing.
func TestBootCompactYieldsToJSONAndRefusesGarbage(t *testing.T) {
	_, token, server, calls := bootHub(t)
	defer server.Close()
	q := bootQuery(t, bootFrame(3, "state:digest"))
	client := dialWSPath(t, server.URL, "/?deviceToken="+token+"&boot="+q+"&b1=7.1..d.s", "Ym9vdC1jb21wYWN0LTAy")
	approved := readEvent(t, client)
	if n, _ := approved.Payload["boot"].(float64); n != 1 {
		t.Fatalf("approval boot = %v, want 1 (the JSON form)", approved.Payload["boot"])
	}
	if reply := readReply(t, client); reply.ID.String() != "3" {
		t.Fatalf("reply id = %v, want 3", reply.ID)
	}
	client.conn.Close()

	bad := dialWSPath(t, server.URL, "/?deviceToken="+token+"&b1=7.1..x", "Ym9vdC1jb21wYWN0LTAz")
	defer bad.conn.Close()
	approved = readEvent(t, bad)
	if _, has := approved.Payload["boot"]; has {
		t.Fatalf("a malformed b1 was announced: %+v", approved.Payload)
	}
	if *calls != 1 {
		t.Fatalf("handlers ran %d times, want 1", *calls)
	}
}
