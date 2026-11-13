package chat

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// The ordered, padded head must be the same JSON object encoding/json
// writes, member for member, and decode to the same State.
func TestSQLHeadIsTheStateJSONReorderedAndPadded(t *testing.T) {
	state := longChatState(t)
	state.Parent = nil
	head, err := encodeSQLHead(state)
	if err != nil {
		t.Fatal(err)
	}
	plain := state
	plain.Ledger, plain.Tools = nil, nil
	want, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.TrimRight(head, " ")
	if len(head) != sqlHeadPaddedSize(len(content)) || len(head)%512 != 0 {
		t.Fatalf("head is %d bytes for %d of JSON", len(head), len(content))
	}
	var gotMembers, wantMembers map[string]json.RawMessage
	if err := json.Unmarshal(head, &gotMembers); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantMembers); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMembers, wantMembers) {
		t.Fatal("the head's members differ from encoding/json's")
	}
	if !bytes.HasPrefix(content, []byte(`{"Background":`)) {
		t.Fatalf("head starts %q", content[:min(40, len(content))])
	}
	var fromHead, fromPlain State
	if err := json.Unmarshal(head, &fromHead); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &fromPlain); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromHead, fromPlain) {
		t.Fatal("the head decodes to another State")
	}
	// omitempty members are left out exactly as encoding/json leaves them.
	withParent := state
	withParent.Parent = &ParentLink{ChatID: "parent"}
	withParent.NativeForkThrough = 3
	parentHead, err := encodeSQLHead(withParent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(parentHead, []byte(`"Parent":{`)) || !bytes.Contains(parentHead, []byte(`"NativeForkThrough":3`)) ||
		bytes.Contains(head, []byte(`"Parent"`)) || bytes.Contains(head, []byte(`"NativeForkThrough"`)) {
		t.Fatal("omitempty members are not encoded as encoding/json encodes them")
	}
}

// A head that grows or shrinks a little keeps its stored size, which is what
// lets SQLite rewrite only the pages that changed.
func TestSQLHeadKeepsItsSizeAcrossSmallChanges(t *testing.T) {
	state := longChatState(t)
	head, err := encodeSQLHead(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, draft := range []string{"x", "a longer draft than before", ""} {
		next := state
		next.Presentation.Draft = draft
		next.Revision++
		changed, err := encodeSQLHead(next)
		if err != nil {
			t.Fatal(err)
		}
		if len(changed) != len(head) {
			t.Fatalf("draft %q: head went from %d to %d bytes", draft, len(head), len(changed))
		}
	}
	for _, size := range []int{1, 511, 512, 513, 16_000, 5_000_000} {
		padded := sqlHeadPaddedSize(size)
		if padded < size || padded-size >= max(512, size/16) {
			t.Fatalf("size %d pads to %d", size, padded)
		}
	}
}

func TestSQLNowHasAFixedWidthAndParses(t *testing.T) {
	first := sqlNow()
	for range 50 {
		now := sqlNow()
		if len(now) != len(first) {
			t.Fatalf("%q and %q differ in width", first, now)
		}
		if _, err := time.Parse(time.RFC3339Nano, now); err != nil {
			t.Fatal(err)
		}
	}
}
