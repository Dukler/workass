package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestReadOmitDropsNamedKeysAndKeepsIdentity(t *testing.T) {
	raw := []byte(`{"chats":[{"id":"tab-1","chatId":"c1","title":"a","contextUsageByProvider":{"x":1},"planLatest":[]},{"id":"tab-2","chatId":"c2","title":"b"}],"chatOrder":["tab-1"],"deferredChats":{"count":3},"workspaces":[]}`)
	omit := readOmitFrom(map[string]any{"omit": map[string]any{
		"root": []any{"deferredChats", "chats", "etag"},
		"chat": []any{"contextUsageByProvider", "planLatest", "id", "chatId", 5},
	}})
	if omit == nil {
		t.Fatal("omit not parsed")
	}
	var got map[string]any
	if err := json.Unmarshal(omit.apply(raw), &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got["deferredChats"]; has {
		t.Fatal("root key kept")
	}
	if _, has := got["chatOrder"]; !has {
		t.Fatal("unnamed root key dropped")
	}
	chats, _ := got["chats"].([]any)
	if len(chats) != 2 {
		t.Fatalf("chats = %v (protected key must survive)", got["chats"])
	}
	first := chats[0].(map[string]any)
	if first["id"] != "tab-1" || first["chatId"] != "c1" || first["title"] != "a" {
		t.Fatalf("row identity or unnamed field lost: %v", first)
	}
	if _, has := first["contextUsageByProvider"]; has {
		t.Fatal("chat field kept")
	}
	if _, has := first["planLatest"]; has {
		t.Fatal("chat field kept")
	}
}

func TestReadOmitAbsentIsByteIdentical(t *testing.T) {
	raw := []byte(`{"b":1,"a":2,"chats":[]}`)
	if got := readOmitFrom(map[string]any{}).apply(raw); string(got) != string(raw) {
		t.Fatalf("no omit changed the reply: %s", got)
	}
	if got := readOmitFrom(map[string]any{"omit": map[string]any{"root": []any{}}}).apply(raw); string(got) != string(raw) {
		t.Fatalf("empty omit changed the reply: %s", got)
	}
}

// The delta memory and the etag cover the projected shell: a client that asks
// with the same omit both times gets a delta over the projection that rebuilds
// exactly the projected shell.
func TestReadOmitShellDeltaCoversTheProjection(t *testing.T) {
	omit := readOmitFrom(map[string]any{"omit": map[string]any{"chat": []any{"contextUsageByProvider"}}})
	shell := func(title string, used int) []byte {
		rows := []any{}
		order := []any{}
		for i := 0; i < 20; i++ {
			id := fmt.Sprintf("tab-%02d", i)
			row := map[string]any{"id": id, "chatId": "c" + id, "title": fmt.Sprintf("chat %d with a long enough title", i), "cwd": "/Users/someone/Workspace/project", "contextUsageByProvider": map[string]any{"claude": map[string]any{"used": used + i}}}
			if i == 3 {
				row["title"] = title
			}
			rows = append(rows, row)
			order = append(order, id)
		}
		raw, _ := json.Marshal(map[string]any{"chats": rows, "chatOrder": order})
		return raw
	}
	one := omit.apply(shell("a", 1))
	two := omit.apply(shell("b", 2))
	first := sessionShellDeltaReply(one, "")
	var whole map[string]any
	if err := json.Unmarshal(first, &whole); err != nil {
		t.Fatal(err)
	}
	etag, _ := whole["etag"].(string)
	if etag == "" {
		t.Fatalf("no etag in %s", first)
	}
	again := sessionShellDeltaReply(one, etag)
	if string(again) != `{"etag":"`+etag+`","unchanged":true}` {
		t.Fatalf("same projection is not unchanged: %s", again)
	}
	next := sessionShellDeltaReply(two, etag)
	var reply map[string]any
	if err := json.Unmarshal(next, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["delta"] == nil {
		t.Fatalf("expected a delta, got %s", next)
	}
	if string(next) == "" || json.Valid(next) == false {
		t.Fatal("invalid reply")
	}
	raw, _ := json.Marshal(reply["delta"])
	if json.Valid(raw) && containsKey(reply["delta"], "contextUsageByProvider") {
		t.Fatalf("delta carries an omitted field: %s", raw)
	}
}

func containsKey(value any, key string) bool {
	switch v := value.(type) {
	case map[string]any:
		for k, inner := range v {
			if k == key || containsKey(inner, key) {
				return true
			}
		}
	case []any:
		for _, inner := range v {
			if containsKey(inner, key) {
				return true
			}
		}
	}
	return false
}
