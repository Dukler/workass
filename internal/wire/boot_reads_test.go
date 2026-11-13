package wire

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func bootQuery(t *testing.T, frames ...map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(frames)
	if err != nil {
		t.Fatal(err)
	}
	return url.QueryEscape(string(raw))
}

func bootFrame(id int, channel string, args ...any) map[string]any {
	if args == nil {
		args = []any{}
	}
	return map[string]any{"t": "invoke", "id": id, "channel": channel, "args": args}
}

func bootHub(t *testing.T) (*Hub, string, *httptest.Server, *int) {
	t.Helper()
	manager, _, _ := testLeaseManager(t)
	token := approveTestDevice(t, manager, "phone", "192.168.50.20")
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	calls := 0
	hub.Register("state:digest", func(args []any) (any, error) {
		calls++
		return map[string]any{"digest": true, "args": args}, nil
	})
	hub.Register("session:get", func(args []any) (any, error) {
		calls++
		return map[string]any{"shell": true}, nil
	})
	hub.Register("chat:delete", func(args []any) (any, error) {
		calls++
		return true, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	return hub, token, server, &calls
}

// Reads named on the URL are answered right after the approval, before the
// client has sent a frame, and the approval says how many there are.
func TestBootReadsAreAnsweredBeforeAnyFrame(t *testing.T) {
	_, token, server, calls := bootHub(t)
	defer server.Close()
	q := bootQuery(t, bootFrame(7, "state:digest", map[string]any{"page": map[string]any{"settledBefore": 1}}), bootFrame(8, "session:get"))
	client := dialWSPath(t, server.URL, "/?deviceToken="+token+"&lean=1&boot="+q, "Ym9vdC1yZWFkcy0wMDAx")
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
		t.Fatalf("boot replies ids = %v, %v; want 7 then 8 (arrival order on the ordered lane)", first.ID, second.ID)
	}
	result, _ := first.Result.(map[string]any)
	if result["digest"] != true {
		t.Fatalf("digest boot reply = %+v", first.Result)
	}
	if *calls != 2 {
		t.Fatalf("handlers ran %d times, want 2", *calls)
	}
	// The socket carries on as usual afterwards; ids continue from the client.
	sendInvoke(t, client, 9, "session:get")
	if reply := readReply(t, client); reply.ID.String() != "9" {
		t.Fatalf("later reply id = %v", reply.ID)
	}
}

// One channel outside the read-only list (or a malformed list) and nothing runs:
// the approval carries no count and the client sends the frames itself.
func TestBootReadsRefuseAnythingButTheReadList(t *testing.T) {
	for name, query := range map[string]string{
		"mutating":  bootQuery(t, bootFrame(1, "state:digest"), bootFrame(2, "chat:delete")),
		"unknown":   bootQuery(t, bootFrame(1, "fs:read")),
		"pre-ready": bootQuery(t, bootFrame(1, "fleet:enroll")),
		"no id":     url.QueryEscape(`[{"t":"invoke","channel":"state:digest","args":[]}]`),
		"dup id":    bootQuery(t, bootFrame(1, "state:digest"), bootFrame(1, "session:get")),
		"not json":  "%5Bnope",
	} {
		t.Run(name, func(t *testing.T) {
			_, token, server, calls := bootHub(t)
			defer server.Close()
			client := dialWSPath(t, server.URL, "/?deviceToken="+token+"&boot="+query, "Ym9vdC1yZWFkcy0wMDAy")
			defer client.conn.Close()
			approved := readEvent(t, client)
			if _, has := approved.Payload["boot"]; has || approved.Payload["state"] != "approved" {
				t.Fatalf("approval = %+v, want approved with no boot count", approved.Payload)
			}
			sendInvoke(t, client, 1, "session:get")
			if reply := readReply(t, client); reply.ID.String() != "1" {
				t.Fatalf("first reply id = %v, want the client's own frame", reply.ID)
			}
			if *calls != 1 {
				t.Fatalf("handlers ran %d times, want only the sent frame", *calls)
			}
		})
	}
}

// A connection that is not token-approved runs nothing from the URL.
func TestBootReadsNeedAnApprovedToken(t *testing.T) {
	_, _, server, calls := bootHub(t)
	defer server.Close()
	q := bootQuery(t, bootFrame(1, "state:digest"))
	client := dialWSPath(t, server.URL, "/?deviceToken=not-a-token&boot="+q, "Ym9vdC1yZWFkcy0wMDAz")
	defer client.conn.Close()
	refused := readEvent(t, client)
	if refused.Payload["state"] != "rejected" {
		t.Fatalf("state = %+v, want rejected", refused.Payload)
	}
	if _, has := refused.Payload["boot"]; has {
		t.Fatal("a refused connection announced boot reads")
	}
	expectServerClosed(t, client)
	if *calls != 0 {
		t.Fatalf("a refused connection ran %d boot reads", *calls)
	}
}

func TestBootReadsFromQueryLimits(t *testing.T) {
	many := make([]map[string]any, 0, bootReadLimit+1)
	for i := 0; i <= bootReadLimit; i++ {
		many = append(many, bootFrame(i+1, "app:meta"))
	}
	raw, _ := json.Marshal(many)
	if got := bootReadsFromQuery(string(raw)); got != nil {
		t.Fatalf("%d reads accepted past the limit", len(got))
	}
	raw, _ = json.Marshal(many[:bootReadLimit])
	if got := bootReadsFromQuery(string(raw)); len(got) != bootReadLimit {
		t.Fatalf("accepted %d of %d reads at the limit", len(got), bootReadLimit)
	}
	if got := bootReadsFromQuery(""); got != nil {
		t.Fatal("empty parameter produced reads")
	}
}
